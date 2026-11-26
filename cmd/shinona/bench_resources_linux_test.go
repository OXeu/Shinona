package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
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

// Linux stat reports allocated disk blocks in units of 512 bytes.
func benchmarkAllocatedFileBytes(info os.FileInfo) (bytes int64, shared bool) {
	stats := info.Sys().(*syscall.Stat_t)
	return stats.Blocks * 512, stats.Nlink > 1
}

func TestBenchmarkDiskUsageCountsNestedFiles(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "benchmark.db")
	nested := filepath.Join(root, "attachments")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, count int) int64 {
		t.Helper()
		if err := os.WriteFile(path, bytes.Repeat([]byte{0x77}, count), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		size, _ := benchmarkAllocatedFileBytes(info)
		if size == 0 {
			t.Fatalf("fixture %s uses no disk blocks", path)
		}
		return size
	}
	dbBytes := write(database, 8192)
	walBytes := write(database+"-wal", 16384)
	shmBytes := write(database+"-shm", 4096)
	file := filepath.Join(nested, "image.png")
	fileBytes := write(file, 32768)
	if err := os.Link(file, filepath.Join(nested, "image-link.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "external", "ignored.bin"), 65536)

	var samples benchmarkDiskSamples
	first, err := benchmarkDiskUsage(database)
	if err != nil {
		t.Fatal(err)
	}
	want := benchmarkDiskSnapshot{total: dbBytes + walBytes + shmBytes + fileBytes, database: dbBytes, wal: walBytes, files: fileBytes}
	if first != want {
		t.Fatalf("disk usage = %+v, want %+v", first, want)
	}
	samples.record(first)

	for _, path := range []string{database + "-wal", file, filepath.Join(nested, "image-link.png")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	last, err := benchmarkDiskUsage(database)
	if err != nil {
		t.Fatal(err)
	}
	samples.record(last)
	if samples.final.total != dbBytes+shmBytes || samples.final.files != 0 ||
		samples.peakTotal != first.total || samples.peakWAL != walBytes || samples.peakFiles != fileBytes {
		t.Fatalf("unexpected disk usage after removal: %+v", samples)
	}
}
