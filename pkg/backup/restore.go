package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/AlexAkulov/clickhouse-backup/pkg/status"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/AlexAkulov/clickhouse-backup/pkg/common"

	"github.com/mattn/go-shellwords"

	"github.com/AlexAkulov/clickhouse-backup/pkg/clickhouse"
	"github.com/AlexAkulov/clickhouse-backup/pkg/filesystemhelper"
	"github.com/AlexAkulov/clickhouse-backup/pkg/metadata"
	"github.com/AlexAkulov/clickhouse-backup/pkg/utils"
	apexLog "github.com/apex/log"
	recursiveCopy "github.com/otiai10/copy"
	"github.com/yargevad/filepathx"
)

var CreateDatabaseRE = regexp.MustCompile(`(?m)^CREATE DATABASE (\s*)(\S+)(\s*)`)

// Restore - restore tables matched by tablePattern from backupName
func (b *Backuper) Restore(backupName, tablePattern string, databaseMapping, partitions []string, schemaOnly, dataOnly, dropTable, ignoreDependencies, rbacOnly, configsOnly bool, commandId int) error {
	ctx, cancel, err := status.Current.GetContextWithCancel(commandId)
	if err != nil {
		return err
	}
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	backupName = utils.CleanBackupNameRE.ReplaceAllString(backupName, "")
	if err := b.prepareRestoreDatabaseMapping(databaseMapping); err != nil {
		return err
	}

	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "restore",
	})
	doRestoreData := !schemaOnly || dataOnly

	if err := b.ch.Connect(); err != nil {
		return fmt.Errorf("can't connect to clickhouse: %v", err)
	}
	defer b.ch.Close()

	if backupName == "" {
		_ = b.PrintLocalBackups(ctx, "all")
		return fmt.Errorf("select backup for restore")
	}
	disks, err := b.ch.GetDisks(ctx)
	if err != nil {
		return err
	}
	defaultDataPath, err := b.ch.GetDefaultPath(disks)
	if err != nil {
		log.Warnf("%v", err)
		return ErrUnknownClickhouseDataPath
	}
	backupMetafileLocalPaths := []string{path.Join(defaultDataPath, "backup", backupName, "metadata.json")}
	var backupMetadataBody []byte
	isEmbedded := false
	embeddedBackupPath, err := b.ch.GetEmbeddedBackupPath(disks)
	if err == nil && embeddedBackupPath != "" {
		backupMetafileLocalPaths = append(backupMetafileLocalPaths, path.Join(embeddedBackupPath, backupName, "metadata.json"))
	} else if b.cfg.ClickHouse.UseEmbeddedBackupRestore && b.cfg.ClickHouse.EmbeddedBackupDisk == "" {
		log.Warnf("%v", err)
	} else if err != nil {
		return err
	}
	for _, metadataPath := range backupMetafileLocalPaths {
		backupMetadataBody, err = os.ReadFile(metadataPath)
		if err == nil && embeddedBackupPath != "" {
			isEmbedded = strings.HasPrefix(metadataPath, embeddedBackupPath)
			break
		}
	}
	if b.cfg.General.RestoreSchemaOnCluster != "" {
		b.cfg.General.RestoreSchemaOnCluster, err = b.ch.ApplyMacros(ctx, b.cfg.General.RestoreSchemaOnCluster)
	}
	if err == nil {
		backupMetadata := metadata.BackupMetadata{}
		if err := json.Unmarshal(backupMetadataBody, &backupMetadata); err != nil {
			return err
		}

		if schemaOnly || doRestoreData {
			for _, database := range backupMetadata.Databases {
				targetDB := database.Name
				if !IsInformationSchema(targetDB) {
					if err = b.restoreEmptyDatabase(ctx, targetDB, tablePattern, database, dropTable, schemaOnly); err != nil {
						return err
					}
				}
			}
			for _, function := range backupMetadata.Functions {
				if err = b.ch.CreateUserDefinedFunction(function.Name, function.CreateQuery, b.cfg.General.RestoreSchemaOnCluster); err != nil {
					return err
				}
			}
		}
		if len(backupMetadata.Tables) == 0 {
			log.Warnf("'%s' doesn't contains tables for restore", backupName)
			if (!rbacOnly) && (!configsOnly) {
				return nil
			}
		}
	} else if !os.IsNotExist(err) { // Legacy backups don't contain metadata.json
		return err
	}
	needRestart := false
	if rbacOnly && !isEmbedded {
		if err := b.restoreRBAC(ctx, backupName, disks); err != nil {
			return err
		}
		needRestart = true
	}
	if configsOnly && !isEmbedded {
		if err := b.restoreConfigs(backupName, disks); err != nil {
			return err
		}
		needRestart = true
	}

	if needRestart {
		log.Warnf("%s contains `access` or `configs` directory, so we need exec %s", backupName, b.ch.Config.RestartCommand)
		cmd, err := shellwords.Parse(b.ch.Config.RestartCommand)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
		log.Infof("run %s", b.ch.Config.RestartCommand)
		var out []byte
		if len(cmd) > 1 {
			out, err = exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		} else {
			out, err = exec.CommandContext(ctx, cmd[0]).CombinedOutput()
		}
		cancel()
		log.Debug(string(out))
		return err
	}

	if schemaOnly || (schemaOnly == dataOnly) {
		if err := b.RestoreSchema(ctx, backupName, tablePattern, dropTable, ignoreDependencies, disks, isEmbedded); err != nil {
			return err
		}
	}
	if dataOnly || (schemaOnly == dataOnly) {
		if err := b.RestoreData(ctx, backupName, tablePattern, partitions, disks, isEmbedded); err != nil {
			return err
		}
	}
	log.Info("done")
	return nil
}

func (b *Backuper) restoreEmptyDatabase(ctx context.Context, targetDB, tablePattern string, database metadata.DatabasesMeta, dropTable, schemaOnly bool) error {
	isMapped := false
	if targetDB, isMapped = b.cfg.General.RestoreDatabaseMapping[database.Name]; !isMapped {
		targetDB = database.Name
	}
	// https://github.com/AlexAkulov/clickhouse-backup/issues/583
	if ShallSkipDatabase(b.cfg, targetDB, tablePattern) {
		return nil
	}
	//https://github.com/AlexAkulov/clickhouse-backup/issues/514
	if schemaOnly && dropTable {
		onCluster := ""
		if b.cfg.General.RestoreSchemaOnCluster != "" {
			onCluster = fmt.Sprintf(" ON CLUSTER '%s'", b.cfg.General.RestoreSchemaOnCluster)
		}
		if _, err := b.ch.QueryContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s` %s SYNC", targetDB, onCluster)); err != nil {
			return err
		}

	}
	substitution := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS ${1}`%s`${3}", targetDB)
	if err := b.ch.CreateDatabaseFromQuery(ctx, CreateDatabaseRE.ReplaceAllString(database.Query, substitution), b.cfg.General.RestoreSchemaOnCluster); err != nil {
		return err
	}
	return nil
}

func (b *Backuper) prepareRestoreDatabaseMapping(databaseMapping []string) error {
	for i := 0; i < len(databaseMapping); i++ {
		splitByCommas := strings.Split(databaseMapping[i], ",")
		for _, m := range splitByCommas {
			splitByColon := strings.Split(m, ":")
			if len(splitByColon) != 2 {
				return fmt.Errorf("restore-database-mapping %s should only have srcDatabase:destinationDatabase format for each map rule", m)
			}
			b.cfg.General.RestoreDatabaseMapping[splitByColon[0]] = splitByColon[1]
		}
	}
	return nil
}

// restoreRBAC - copy backup_name>/rbac folder to access_data_path
func (b *Backuper) restoreRBAC(ctx context.Context, backupName string, disks []clickhouse.Disk) error {
	log := b.log.WithField("logger", "restoreRBAC")
	accessPath, err := b.ch.GetAccessManagementPath(ctx, nil)
	if err != nil {
		return err
	}
	if err = b.restoreBackupRelatedDir(backupName, "access", accessPath, disks); err == nil {
		markFile := path.Join(accessPath, "need_rebuild_lists.mark")
		log.Infof("create %s for properly rebuild RBAC after restart clickhouse-server", markFile)
		file, err := os.Create(markFile)
		if err != nil {
			return err
		}
		_ = file.Close()
		_ = filesystemhelper.Chown(markFile, b.ch, disks, false)
		listFilesPattern := path.Join(accessPath, "*.list")
		log.Infof("remove %s for properly rebuild RBAC after restart clickhouse-server", listFilesPattern)
		if listFiles, err := filepathx.Glob(listFilesPattern); err != nil {
			return err
		} else {
			for _, f := range listFiles {
				if err := os.Remove(f); err != nil {
					return err
				}
			}
		}
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// restoreConfigs - copy backup_name/configs folder to /etc/clickhouse-server/
func (b *Backuper) restoreConfigs(backupName string, disks []clickhouse.Disk) error {
	if err := b.restoreBackupRelatedDir(backupName, "configs", b.ch.Config.ConfigDir, disks); err != nil && os.IsNotExist(err) {
		return nil
	} else {
		return err
	}
}

func (b *Backuper) restoreBackupRelatedDir(backupName, backupPrefixDir, destinationDir string, disks []clickhouse.Disk) error {
	log := b.log.WithField("logger", "restoreBackupRelatedDir")
	defaultDataPath, err := b.ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	srcBackupDir := path.Join(defaultDataPath, "backup", backupName, backupPrefixDir)
	info, err := os.Stat(srcBackupDir)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a dir", srcBackupDir)
	}
	log.Debugf("copy %s -> %s", srcBackupDir, destinationDir)
	copyOptions := recursiveCopy.Options{OnDirExists: func(src, dest string) recursiveCopy.DirExistsAction {
		return recursiveCopy.Merge
	}}
	if err := recursiveCopy.Copy(srcBackupDir, destinationDir, copyOptions); err != nil {
		return err
	}

	files, err := filepathx.Glob(path.Join(destinationDir, "**"))
	if err != nil {
		return err
	}
	files = append(files, destinationDir)
	for _, localFile := range files {
		if err := filesystemhelper.Chown(localFile, b.ch, disks, false); err != nil {
			return err
		}
	}
	return nil
}

// RestoreSchema - restore schemas matched by tablePattern from backupName
func (b *Backuper) RestoreSchema(ctx context.Context, backupName, tablePattern string, dropTable, ignoreDependencies bool, disks []clickhouse.Disk, isEmbedded bool) error {
	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "restore",
	})

	defaultDataPath, err := b.ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	version, err := b.ch.GetVersion(ctx)
	if err != nil {
		return err
	}
	metadataPath := path.Join(defaultDataPath, "backup", backupName, "metadata")
	if isEmbedded {
		defaultDataPath, err = b.ch.GetEmbeddedBackupPath(disks)
		if err != nil {
			return err
		}
		metadataPath = path.Join(defaultDataPath, backupName, "metadata")
	}
	info, err := os.Stat(metadataPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a dir", metadataPath)
	}
	if tablePattern == "" {
		tablePattern = "*"
	}
	tablesForRestore, err := getTableListByPatternLocal(b.cfg, b.ch, metadataPath, tablePattern, dropTable, nil)
	if err != nil {
		return err
	}
	// if restore-database-mapping specified, create database in mapping rules instead of in backup files.
	if len(b.cfg.General.RestoreDatabaseMapping) > 0 {
		err = changeTableQueryToAdjustDatabaseMapping(&tablesForRestore, b.cfg.General.RestoreDatabaseMapping)
		if err != nil {
			return err
		}
	}
	if len(tablesForRestore) == 0 {
		return fmt.Errorf("no have found schemas by %s in %s", tablePattern, backupName)
	}
	if dropErr := b.dropExistsTables(tablesForRestore, ignoreDependencies, version, log); dropErr != nil {
		return dropErr
	}
	var restoreErr error
	if isEmbedded {
		restoreErr = b.restoreSchemaEmbedded(backupName, tablesForRestore)
	} else {
		restoreErr = b.restoreSchemaRegular(tablesForRestore, version, log)
	}
	if restoreErr != nil {
		return restoreErr
	}
	return nil
}

var UUIDWithReplicatedMergeTreeRE = regexp.MustCompile(`^(.+)(UUID)(\s+)'([^']+)'(.+)({uuid})(.*)`)

func (b *Backuper) restoreSchemaEmbedded(backupName string, tablesForRestore ListOfTables) error {
	return b.restoreEmbedded(backupName, true, tablesForRestore, nil)
}

func (b *Backuper) restoreSchemaRegular(tablesForRestore ListOfTables, version int, log *apexLog.Entry) error {
	totalRetries := len(tablesForRestore)
	restoreRetries := 0
	isDatabaseCreated := common.EmptyMap{}
	var restoreErr error
	for restoreRetries < totalRetries {
		var notRestoredTables ListOfTables
		for _, schema := range tablesForRestore {
			// if metadata.json doesn't contain "databases", we will re-create tables with default engine
			if _, isCreated := isDatabaseCreated[schema.Database]; !isCreated {
				if err := b.ch.CreateDatabase(schema.Database, b.cfg.General.RestoreSchemaOnCluster); err != nil {
					return fmt.Errorf("can't create database '%s': %v", schema.Database, err)
				} else {
					isDatabaseCreated[schema.Database] = struct{}{}
				}
			}
			//materialized and window views should restore via ATTACH
			schema.Query = strings.Replace(
				schema.Query, "CREATE MATERIALIZED VIEW", "ATTACH MATERIALIZED VIEW", 1,
			)
			schema.Query = strings.Replace(
				schema.Query, "CREATE WINDOW VIEW", "ATTACH WINDOW VIEW", 1,
			)
			schema.Query = strings.Replace(
				schema.Query, "CREATE LIVE VIEW", "ATTACH LIVE VIEW", 1,
			)
			// https://github.com/AlexAkulov/clickhouse-backup/issues/466
			if b.cfg.General.RestoreSchemaOnCluster == "" && strings.Contains(schema.Query, "{uuid}") && strings.Contains(schema.Query, "Replicated") {
				if !strings.Contains(schema.Query, "UUID") {
					log.Warnf("table query doesn't contains UUID, can't guarantee properly restore for ReplicatedMergeTree")
				} else {
					schema.Query = UUIDWithReplicatedMergeTreeRE.ReplaceAllString(schema.Query, "$1$2$3'$4'$5$4$7")
				}
			}
			restoreErr = b.ch.CreateTable(clickhouse.Table{
				Database: schema.Database,
				Name:     schema.Table,
			}, schema.Query, false, false, b.cfg.General.RestoreSchemaOnCluster, version)

			if restoreErr != nil {
				restoreRetries++
				if restoreRetries >= totalRetries {
					return fmt.Errorf(
						"can't create table `%s`.`%s`: %v after %d times, please check your schema dependencies",
						schema.Database, schema.Table, restoreErr, restoreRetries,
					)
				} else {
					log.Warnf(
						"can't create table '%s.%s': %v, will try again", schema.Database, schema.Table, restoreErr,
					)
				}
				notRestoredTables = append(notRestoredTables, schema)
			}
		}
		tablesForRestore = notRestoredTables
		if len(tablesForRestore) == 0 {
			break
		}
	}
	return nil
}

func (b *Backuper) dropExistsTables(tablesForDrop ListOfTables, ignoreDependencies bool, version int, log *apexLog.Entry) error {
	var dropErr error
	dropRetries := 0
	totalRetries := len(tablesForDrop)
	for dropRetries < totalRetries {
		var notDroppedTables ListOfTables
		for i, schema := range tablesForDrop {
			if schema.Query == "" {
				possibleQueries := []string{
					fmt.Sprintf("CREATE DICTIONARY `%s`.`%s`", schema.Database, schema.Table),
					fmt.Sprintf("CREATE MATERIALIZED VIEW `%s`.`%s`", schema.Database, schema.Table),
				}
				if len(schema.Parts) > 0 {
					possibleQueries = append([]string{
						fmt.Sprintf("CREATE TABLE `%s`.`%s`", schema.Database, schema.Table),
					}, possibleQueries...)
				}
				for _, query := range possibleQueries {
					dropErr = b.ch.DropTable(clickhouse.Table{
						Database: schema.Database,
						Name:     schema.Table,
					}, query, b.cfg.General.RestoreSchemaOnCluster, ignoreDependencies, version)
					if dropErr == nil {
						tablesForDrop[i].Query = query
					}
				}
			} else {
				dropErr = b.ch.DropTable(clickhouse.Table{
					Database: schema.Database,
					Name:     schema.Table,
				}, schema.Query, b.cfg.General.RestoreSchemaOnCluster, ignoreDependencies, version)
			}

			if dropErr != nil {
				dropRetries++
				if dropRetries >= totalRetries {
					return fmt.Errorf(
						"can't drop table `%s`.`%s`: %v after %d times, please check your schema dependencies",
						schema.Database, schema.Table, dropErr, dropRetries,
					)
				} else {
					log.Warnf(
						"can't drop table '%s.%s': %v, will try again", schema.Database, schema.Table, dropErr,
					)
				}
				notDroppedTables = append(notDroppedTables, schema)
			}
		}
		tablesForDrop = notDroppedTables
		if len(tablesForDrop) == 0 {
			break
		}
	}
	return nil
}

// RestoreData - restore data for tables matched by tablePattern from backupName
func (b *Backuper) RestoreData(ctx context.Context, backupName string, tablePattern string, partitions []string, disks []clickhouse.Disk, isEmbedded bool) error {
	startRestore := time.Now()
	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "restore",
	})
	defaultDataPath, err := b.ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	if b.ch.IsClickhouseShadow(path.Join(defaultDataPath, "backup", backupName, "shadow")) {
		return fmt.Errorf("backups created in v0.0.1 is not supported now")
	}
	backup, _, err := b.getLocalBackup(ctx, backupName, disks)
	if err != nil {
		return fmt.Errorf("can't restore: %v", err)
	}

	diskMap := map[string]string{}
	for _, disk := range disks {
		diskMap[disk.Name] = disk.Path
	}
	var tablesForRestore ListOfTables
	if backup.Legacy {
		tablesForRestore, err = b.ch.GetBackupTablesLegacy(backupName, disks)
	} else {
		metadataPath := path.Join(defaultDataPath, "backup", backupName, "metadata")
		if isEmbedded {
			metadataPath = path.Join(diskMap[b.cfg.ClickHouse.EmbeddedBackupDisk], backupName, "metadata")
		}
		tablesForRestore, err = getTableListByPatternLocal(b.cfg, b.ch, metadataPath, tablePattern, false, partitions)
	}
	if err != nil {
		return err
	}
	if len(tablesForRestore) == 0 {
		return fmt.Errorf("no have found schemas by %s in %s", tablePattern, backupName)
	}
	log.Debugf("found %d tables with data in backup", len(tablesForRestore))
	if isEmbedded {
		err = b.restoreDataEmbedded(backupName, tablesForRestore, partitions)
	} else {
		err = b.restoreDataRegular(ctx, backupName, tablePattern, tablesForRestore, diskMap, disks, log)
	}
	if err != nil {
		return err
	}
	log.WithField("duration", utils.HumanizeDuration(time.Since(startRestore))).Info("done")
	return nil
}

func (b *Backuper) restoreDataEmbedded(backupName string, tablesForRestore ListOfTables, partitions []string) error {
	return b.restoreEmbedded(backupName, false, tablesForRestore, partitions)
}

func (b *Backuper) restoreDataRegular(ctx context.Context, backupName string, tablePattern string, tablesForRestore ListOfTables, diskMap map[string]string, disks []clickhouse.Disk, log *apexLog.Entry) error {
	if len(b.cfg.General.RestoreDatabaseMapping) > 0 {
		for sourceDb, targetDb := range b.cfg.General.RestoreDatabaseMapping {
			if tablePattern != "" {
				sourceDbRE := regexp.MustCompile(fmt.Sprintf("(^%s.*)|(,%s.*)", sourceDb, sourceDb))
				if sourceDbRE.MatchString(tablePattern) {
					matches := sourceDbRE.FindAllStringSubmatch(tablePattern, -1)
					substitution := targetDb + ".*"
					if strings.HasPrefix(matches[0][1], ",") {
						substitution = "," + substitution
					}
					tablePattern = sourceDbRE.ReplaceAllString(tablePattern, substitution)
				} else {
					tablePattern += "," + targetDb + ".*"
				}
			} else {
				tablePattern += targetDb + ".*"
			}
		}
	}
	chTables, err := b.ch.GetTables(ctx, tablePattern)
	if err != nil {
		return err
	}
	for _, t := range tablesForRestore {
		for disk := range t.Parts {
			if _, diskExists := diskMap[disk]; !diskExists {
				log.Warnf("table '%s.%s' require disk '%s' that not found in clickhouse table system.disks, you can add nonexistent disks to `disk_mapping` in  `clickhouse` config section, data will restored to %s", t.Database, t.Table, disk, diskMap["default"])
				found := false
				for _, d := range disks {
					if d.Name == disk {
						found = true
						break
					}
				}
				if !found {
					newDisk := clickhouse.Disk{
						Name: disk,
						Path: diskMap["default"],
						Type: "local",
					}
					disks = append(disks, newDisk)
				}
			}
		}
	}
	dstTablesMap := map[metadata.TableTitle]clickhouse.Table{}
	for i, chTable := range chTables {
		dstTablesMap[metadata.TableTitle{
			Database: chTables[i].Database,
			Table:    chTables[i].Name,
		}] = chTable
	}

	var missingTables []string
	for _, table := range tablesForRestore {
		dstDatabase := table.Database
		if len(b.cfg.General.RestoreDatabaseMapping) > 0 {
			if targetDB, isMapped := b.cfg.General.RestoreDatabaseMapping[table.Database]; isMapped {
				dstDatabase = targetDB
			}
		}
		found := false
		for _, chTable := range chTables {
			if (dstDatabase == chTable.Database) && (table.Table == chTable.Name) {
				found = true
				break
			}
		}
		if !found {
			missingTables = append(missingTables, fmt.Sprintf("'%s.%s'", dstDatabase, table.Table))
		}
	}
	if len(missingTables) > 0 {
		return fmt.Errorf("%s is not created. Restore schema first or create missing tables manually", strings.Join(missingTables, ", "))
	}

	for i, table := range tablesForRestore {
		// need mapped database path and original table.Database for CopyDataToDetached
		dstDatabase := table.Database
		if len(b.cfg.General.RestoreDatabaseMapping) > 0 {
			if targetDB, isMapped := b.cfg.General.RestoreDatabaseMapping[table.Database]; isMapped {
				dstDatabase = targetDB
				tablesForRestore[i].Database = targetDB
			}
		}
		log := log.WithField("table", fmt.Sprintf("%s.%s", dstDatabase, table.Table))
		dstTable, ok := dstTablesMap[metadata.TableTitle{
			Database: dstDatabase,
			Table:    table.Table}]
		if !ok {
			return fmt.Errorf("can't find '%s.%s' in current system.tables", dstDatabase, table.Table)
		}
		if err := filesystemhelper.CopyDataToDetached(backupName, table, disks, dstTable.DataPaths, b.ch); err != nil {
			return fmt.Errorf("can't restore '%s.%s': %v", table.Database, table.Table, err)
		}
		log.Debugf("copied data to 'detached'")
		if err := b.ch.AttachPartitions(tablesForRestore[i], disks); err != nil {
			return fmt.Errorf("can't attach partitions for table '%s.%s': %v", tablesForRestore[i].Database, tablesForRestore[i].Table, err)
		}
		log.Info("done")
	}
	return nil
}

func (b *Backuper) restoreEmbedded(backupName string, restoreOnlySchema bool, tablesForRestore ListOfTables, partitions []string) error {
	restoreSQL := "Disk(?,?)"
	tablesSQL := ""
	l := len(tablesForRestore)
	for i, t := range tablesForRestore {
		if t.Query != "" {
			kind := "TABLE"
			if strings.Contains(t.Query, " DICTIONARY ") {
				kind = "DICTIONARY"
			}
			if newDb, isMapped := b.cfg.General.RestoreDatabaseMapping[t.Database]; isMapped {
				tablesSQL += fmt.Sprintf("%s `%s`.`%s` AS `%s`.`%s`", kind, t.Database, t.Table, newDb, t.Table)
			} else {
				tablesSQL += fmt.Sprintf("%s `%s`.`%s`", kind, t.Database, t.Table)
			}

			if strings.Contains(t.Query, " VIEW ") {
				kind = "VIEW"
			}

			if kind == "TABLE" && len(partitions) > 0 {
				tablesSQL += fmt.Sprintf(" PARTITIONS '%s'", strings.Join(partitions, "','"))
			}
			if i < l-1 {
				tablesSQL += ", "
			}
		}
	}
	settings := ""
	if restoreOnlySchema {
		settings = "SETTINGS structure_only=true"
	}
	restoreSQL = fmt.Sprintf("RESTORE %s FROM %s %s", tablesSQL, restoreSQL, settings)
	restoreResults := make([]clickhouse.SystemBackups, 0)
	if err := b.ch.Select(&restoreResults, restoreSQL, b.cfg.ClickHouse.EmbeddedBackupDisk, backupName); err != nil {
		return fmt.Errorf("restore error: %v", err)
	}
	if len(restoreResults) == 0 || restoreResults[0].Status != "RESTORED" {
		return fmt.Errorf("restore wrong result: %v", restoreResults)
	}
	return nil
}
