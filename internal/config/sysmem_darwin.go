package config

import (
	"os/exec"
	"strconv"
	"strings"
)

// systemMemory returns the total physical RAM in bytes on macOS.
func systemMemory() int64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 16 << 30 // fallback: 16 GB
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 16 << 30
	}
	return n
}
