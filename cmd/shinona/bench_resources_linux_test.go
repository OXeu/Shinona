package main

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func benchmarkProcessCPU() (time.Duration, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), true
}

// Reset the process high-water mark to current RSS after fixture setup.
func resetBenchmarkPeakRSS() bool {
	file, err := os.OpenFile("/proc/self/clear_refs", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_, writeErr := file.WriteString("5")
	return file.Close() == nil && writeErr == nil
}

func benchmarkPeakRSS() (uint64, bool) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	return benchmarkProcStatusValue(string(status), "VmHWM:", 1024)
}

func benchmarkProcStatusValue(status, key string, scale uint64) (uint64, bool) {
	for line := range strings.SplitSeq(status, "\n") {
		if value, ok := strings.CutPrefix(line, key); ok {
			fields := strings.Fields(value)
			if len(fields) != 0 {
				n, err := strconv.ParseUint(fields[0], 10, 64)
				return n * scale, err == nil
			}
			break
		}
	}
	return 0, false
}

func benchmarkContextSwitches() (uint64, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	return uint64(usage.Nvcsw + usage.Nivcsw), true
}
