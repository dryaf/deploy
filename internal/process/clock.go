package process

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func HostClock() (string, float64, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", 0, err
	}
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "", 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("host monotonic clock unavailable")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	return strings.TrimSpace(string(boot)), seconds, err
}
