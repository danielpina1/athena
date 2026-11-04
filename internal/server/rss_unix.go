//go:build unix

package server

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// processRSS returns the resident set size of this process in bytes, without running another program: the current
// size on Linux (/proc/self/statm), and elsewhere (macOS, BSDs) the peak size from getrusage's ru_maxrss, which is an
// upper bound of the current one and, for a server that has been idle since it started, close to it.
func processRSS() uint64 {
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/self/statm"); err == nil {
			if f := strings.Fields(string(b)); len(f) >= 2 {
				if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					return pages * uint64(os.Getpagesize())
				}
			}
		}
	}
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return uint64(ru.Maxrss) // bytes
	}
	return uint64(ru.Maxrss) * 1024 // kilobytes
}
