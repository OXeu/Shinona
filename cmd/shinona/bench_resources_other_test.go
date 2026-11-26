//go:build !linux

package main

import "time"

func benchmarkProcessCPU() (time.Duration, bool) { return 0, false }
func resetBenchmarkPeakRSS() bool                { return false }
func benchmarkPeakRSS() (uint64, bool)           { return 0, false }
func benchmarkContextSwitches() (uint64, bool)   { return 0, false }
