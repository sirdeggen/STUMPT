package config

import "syscall"

// systemMemory returns the total physical RAM in bytes on Linux.
func systemMemory() int64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err == nil {
		return int64(info.Totalram) * int64(info.Unit)
	}
	return 16 << 30 // fallback: 16 GB
}
