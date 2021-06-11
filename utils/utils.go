package utils

import (
	"fmt"
	"strings"
	"time"
)

const (
	day  = time.Minute * 60 * 24
	year = 365 * day
)

// FormatBytes - Convert bytes to human readable string
func FormatBytes(i int64) string {
	const (
		KiB = 1024
		MiB = 1048576
		GiB = 1073741824
		TiB = 1099511627776
	)
	switch {
	case i >= TiB:
		return fmt.Sprintf("%.02fTiB", float64(i)/TiB)
	case i >= GiB:
		return fmt.Sprintf("%.02fGiB", float64(i)/GiB)
	case i >= MiB:
		return fmt.Sprintf("%.02fMiB", float64(i)/MiB)
	case i >= KiB:
		return fmt.Sprintf("%.02fKiB", float64(i)/KiB)
	default:
		return fmt.Sprintf("%dB", i)
	}
}

func HumanizeDuration(d time.Duration) string {
	if d < day {
		return d.Round(time.Millisecond).String()
	}
	var b strings.Builder
	if d >= year {
		years := d / year
		fmt.Fprintf(&b, "%dy", years)
		d -= years * year
	}
	days := d / day
	d -= days * day
	fmt.Fprintf(&b, "%dd%s", days, d)
	return b.String()
}
