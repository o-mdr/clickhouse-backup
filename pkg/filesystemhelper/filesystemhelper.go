package filesystemhelper

import (
	"fmt"
	"github.com/AlexAkulov/clickhouse-backup/pkg/partition"
	"github.com/AlexAkulov/clickhouse-backup/pkg/utils"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AlexAkulov/clickhouse-backup/pkg/clickhouse"
	"github.com/AlexAkulov/clickhouse-backup/pkg/common"
	"github.com/AlexAkulov/clickhouse-backup/pkg/metadata"
	apexLog "github.com/apex/log"
)

var (
	uid       *int
	gid       *int
	chownLock sync.Mutex
)

// Chown - set permission on path to clickhouse user
// This is necessary that the ClickHouse will be able to read parts files on restore
func Chown(path string, ch *clickhouse.ClickHouse, disks []clickhouse.Disk, recursive bool) error {
	var (
		dataPath string
		err      error
	)
	if os.Getuid() != 0 {
		return nil
	}
	chownLock.Lock()
	if uid == nil {
		if dataPath, err = ch.GetDefaultPath(disks); err != nil {
			return err
		}
		info, err := os.Stat(dataPath)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		intUid := int(stat.Uid)
		intGid := int(stat.Gid)
		uid = &intUid
		gid = &intGid
	}
	chownLock.Unlock()
	if !recursive {
		return os.Chown(path, *uid, *gid)
	}
	return filepath.Walk(path, func(fName string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(fName, *uid, *gid)
	})
}

func Mkdir(name string, ch *clickhouse.ClickHouse, disks []clickhouse.Disk) error {
	if err := os.MkdirAll(name, 0750); err != nil && !os.IsExist(err) {
		return err
	}
	if err := Chown(name, ch, disks, false); err != nil {
		return err
	}
	return nil
}

func MkdirAll(path string, ch *clickhouse.ClickHouse, disks []clickhouse.Disk) error {
	// Fast path: if we can tell whether path is a directory or file, stop with success or error.
	dir, err := os.Stat(path)
	if err == nil {
		if dir.IsDir() {
			return nil
		}
		return &os.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}

	// Slow path: make sure parent exists and then call Mkdir for path.
	i := len(path)
	for i > 0 && os.IsPathSeparator(path[i-1]) { // Skip trailing path separator.
		i--
	}

	j := i
	for j > 0 && !os.IsPathSeparator(path[j-1]) { // Scan backward over element.
		j--
	}

	if j > 1 {
		// Create parent.
		err = MkdirAll(path[:j-1], ch, disks)
		if err != nil {
			return err
		}
	}

	// Parent now exists; invoke Mkdir and use its result.
	err = Mkdir(path, ch, disks)
	if err != nil {
		// Handle arguments like "foo/." by
		// double-checking that directory doesn't exist.
		dir, err1 := os.Lstat(path)
		if err1 == nil && dir.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// CopyDataToDetached - copy partitions for specific table to detached folder
// TODO: check when disk exists in backup, but miss in ClickHouse
func CopyDataToDetached(backupName string, backupTable metadata.TableMetadata, disks []clickhouse.Disk, tableDataPaths []string, ch *clickhouse.ClickHouse) error {
	dstDataPaths := clickhouse.GetDisksByPaths(disks, tableDataPaths)
	log := apexLog.WithFields(apexLog.Fields{"operation": "CopyDataToDetached"})
	start := time.Now()
	for _, backupDisk := range disks {
		backupDiskName := backupDisk.Name
		if len(backupTable.Parts[backupDiskName]) == 0 {
			log.Debugf("%s disk have no parts", backupDisk.Name)
			continue
		}
		detachedParentDir := filepath.Join(dstDataPaths[backupDisk.Name], "detached")
		for _, part := range backupTable.Parts[backupDiskName] {
			detachedPath := filepath.Join(detachedParentDir, part.Name)
			info, err := os.Stat(detachedPath)
			if err != nil {
				if os.IsNotExist(err) {
					log.Debugf("MkDirAll %s", detachedPath)
					if mkdirErr := MkdirAll(detachedPath, ch, disks); mkdirErr != nil {
						log.Warnf("error during Mkdir %+v", mkdirErr)
					}
				} else {
					return err
				}
			} else if !info.IsDir() {
				return fmt.Errorf("'%s' should be directory or absent", detachedPath)
			}
			dbAndTableDir := path.Join(common.TablePathEncode(backupTable.Database), common.TablePathEncode(backupTable.Table))
			partPath := path.Join(backupDisk.Path, "backup", backupName, "shadow", dbAndTableDir, backupDisk.Name, part.Name)
			// Legacy backup support
			if _, err := os.Stat(partPath); os.IsNotExist(err) {
				partPath = path.Join(backupDisk.Path, "backup", backupName, "shadow", dbAndTableDir, part.Name)
			}
			if err := filepath.Walk(partPath, func(filePath string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				filename := strings.Trim(strings.TrimPrefix(filePath, partPath), "/")
				dstFilePath := filepath.Join(detachedPath, filename)
				if info.IsDir() {
					log.Debugf("MkDir %s", dstFilePath)
					return Mkdir(dstFilePath, ch, disks)
				}
				if !info.Mode().IsRegular() {
					log.Debugf("'%s' is not a regular file, skipping.", filePath)
					return nil
				}
				log.Debugf("Link %s -> %s", filePath, dstFilePath)
				if err := os.Link(filePath, dstFilePath); err != nil {
					if !os.IsExist(err) {
						return fmt.Errorf("failed to create hard link '%s' -> '%s': %w", filePath, dstFilePath, err)
					}
				}
				return Chown(dstFilePath, ch, disks, false)
			}); err != nil {
				return fmt.Errorf("error during filepath.Walk for part '%s': %w", part.Name, err)
			}
		}
	}
	log.WithField("duration", utils.HumanizeDuration(time.Since(start))).Debugf("done")
	return nil
}

func IsPartInPartition(partName string, partitionsBackupMap common.EmptyMap) bool {
	_, ok := partitionsBackupMap[strings.Split(partName, "_")[0]]
	return ok
}

func IsFileInPartition(disk, fileName string, partitionsBackupMap common.EmptyMap) bool {
	fileName = strings.TrimPrefix(fileName, disk+"_")
	_, ok := partitionsBackupMap[strings.Split(fileName, "_")[0]]
	return ok
}

func MoveShadow(shadowPath, backupPartsPath string, partitionsBackupMap common.EmptyMap) ([]metadata.Part, int64, error) {
	log := apexLog.WithField("logger", "MoveShadow")
	size := int64(0)
	parts := make([]metadata.Part, 0)
	err := filepath.Walk(shadowPath, func(filePath string, info os.FileInfo, err error) error {
		// possible relative path
		// store / 1f9 / 1f9dc899-0de9-41f8-b95c-26c1f0d67d93 / 20181023_2_2_0 / checksums.txt
		// store / 1f9 / 1f9dc899-0de9-41f8-b95c-26c1f0d67d93 / 20181023_2_2_0 / x.proj / checksums.txt
		// data / database / table / 20181023_2_2_0 / checksums.txt
		// data / database / table / 20181023_2_2_0 / x.proj / checksums.txt
		relativePath := strings.Trim(strings.TrimPrefix(filePath, shadowPath), "/")
		pathParts := strings.SplitN(relativePath, "/", 4)
		if len(pathParts) != 4 {
			return nil
		}
		if len(partitionsBackupMap) != 0 && !IsPartInPartition(pathParts[3], partitionsBackupMap) {
			return nil
		}
		dstFilePath := filepath.Join(backupPartsPath, pathParts[3])
		if info.IsDir() {
			if !strings.HasSuffix(pathParts[3], ".proj") {
				parts = append(parts, metadata.Part{
					Name: pathParts[3],
				})
			}
			return os.MkdirAll(dstFilePath, 0750)
		}
		if !info.Mode().IsRegular() {
			log.Debugf("'%s' is not a regular file, skipping", filePath)
			return nil
		}
		size += info.Size()
		return os.Rename(filePath, dstFilePath)
	})
	return parts, size, err
}

func IsDuplicatedParts(part1, part2 string) error {
	log := apexLog.WithField("logger", "IsDuplicatedParts")
	p1, err := os.Open(part1)
	if err != nil {
		return err
	}
	defer func() {
		if err = p1.Close(); err != nil {
			log.Warnf("Can't close %s", part1)
		}
	}()
	p2, err := os.Open(part2)
	if err != nil {
		return err
	}
	defer func() {
		if err = p2.Close(); err != nil {
			log.Warnf("Can't close %s", part2)
		}
	}()
	pf1, err := p1.Readdirnames(-1)
	if err != nil {
		return err
	}
	pf2, err := p2.Readdirnames(-1)
	if err != nil {
		return err
	}
	if len(pf1) != len(pf2) {
		return fmt.Errorf("files count in parts is different")
	}
	for _, f := range pf1 {
		part1File, err := os.Stat(path.Join(part1, f))
		if err != nil {
			return err
		}
		part2File, err := os.Stat(path.Join(part2, f))
		if err != nil {
			return err
		}
		if !os.SameFile(part1File, part2File) {
			return fmt.Errorf("file '%s' is different", f)
		}
	}
	return nil
}

var partitionTupleRE = regexp.MustCompile(`\)\s*,\s*\(`)

func CreatePartitionsToBackupMap(ch *clickhouse.ClickHouse, tablesFromClickHouse []clickhouse.Table, tablesFromMetadata []metadata.TableMetadata, partitions []string) (common.EmptyMap, []string) {
	if len(partitions) == 0 {
		return make(common.EmptyMap, 0), partitions
	}

	partitionsMap := common.EmptyMap{}

	// to allow use --partitions val1 --partitions val2, https://github.com/AlexAkulov/clickhouse-backup/issues/425#issuecomment-1149855063
	for _, partitionArg := range partitions {
		partitionArg = strings.Trim(partitionArg, " \t")
		// when PARTITION BY clause return partition_id field as hash, https://github.com/AlexAkulov/clickhouse-backup/issues/602
		if strings.HasPrefix(partitionArg, "(") {
			partitionArg = strings.TrimSuffix(strings.TrimPrefix(partitionArg, "("), ")")
			for _, partitionTuple := range partitionTupleRE.Split(partitionArg, -1) {
				for _, item := range tablesFromClickHouse {
					if err, partitionId := partition.GetPartitionId(ch, item.Database, item.Name, item.CreateTableQuery, partitionTuple); err != nil {
						apexLog.Errorf("partition.GetPartitionId error: %v", err)
						return make(common.EmptyMap, 0), partitions
					} else if partitionId != "" {
						partitionsMap[partitionId] = struct{}{}
					}
				}
				for _, item := range tablesFromMetadata {
					if err, partitionId := partition.GetPartitionId(ch, item.Database, item.Table, item.Query, partitionTuple); err != nil {
						apexLog.Errorf("partition.GetPartitionId error: %v", err)
						return make(common.EmptyMap, 0), partitions
					} else if partitionId != "" {
						partitionsMap[partitionId] = struct{}{}
					}
				}
			}
		} else {
			for _, item := range strings.Split(partitionArg, ",") {
				partitionsMap[strings.Trim(item, " \t")] = struct{}{}
			}
		}
	}
	newPartitions := make([]string, len(partitionsMap))
	i := 0
	for partitionName := range partitionsMap {
		newPartitions[i] = partitionName
		i += 1
	}
	return partitionsMap, newPartitions
}
