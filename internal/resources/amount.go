package resources

import (
	"fmt"
	"math"
)

// FormatAmount renders a resource metric in dashboard and notification units.
func FormatAmount(key string, n float64) string {
	if key == "cpu" {
		return fmt.Sprintf("%.1f%%", n)
	}
	if key == "pids" {
		return fmt.Sprintf("%.0f", n)
	}
	unit := 0
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	for math.Abs(n) >= 1024 && unit < len(units)-1 {
		n /= 1024
		unit++
	}
	precision := 1
	if unit == 0 {
		precision = 0
	}
	value := fmt.Sprintf("%.*f %s", precision, n, units[unit])
	switch key {
	case "rx", "tx", "read", "write":
		return value + "/s"
	case "day", "week":
		if n > 0 {
			return "+" + value
		}
	}
	return value
}
