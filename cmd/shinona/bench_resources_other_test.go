//go:build !linux

package main

import (
	"os"
	"time"
)

func benchmarkProcessCPU() (time.Duration, bool) { return 0, false }
func resetBenchmarkPeakRSS() bool                { return false }
func benchmarkPeakRSS() (uint64, bool)           { return 0, false }
func benchmarkContextSwitches() (uint64, bool)   { return 0, false }

func benchmarkAllocatedFileBytes(info os.FileInfo) (bytes int64, shared bool) {
	return info.Size(), false
}
