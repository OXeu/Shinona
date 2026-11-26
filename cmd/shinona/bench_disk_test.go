package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type benchmarkDiskSnapshot struct {
	total, database, wal, files int64
}

func benchmarkDiskUsage(databasePath string) (benchmarkDiskSnapshot, error) {
	root := filepath.Dir(databasePath)
	var usage benchmarkDiskSnapshot
	var hardLinks []fs.FileInfo
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		// Sidecars and application files can disappear between directory reads
		// and stat while the server is active.
		if path != root && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		size, shared := benchmarkAllocatedFileBytes(info)
		if shared {
			for _, previous := range hardLinks {
				if os.SameFile(info, previous) {
					return nil
				}
			}
			hardLinks = append(hardLinks, info)
		}
		usage.total += size
		switch path {
		case databasePath:
			usage.database = size
		case databasePath + "-wal":
			usage.wal = size
		case databasePath + "-shm":
			// Shared memory stays visible in the total disk usage.
		default:
			usage.files += size
		}
		return nil
	})
	if err != nil {
		return benchmarkDiskSnapshot{}, fmt.Errorf("measure benchmark data directory %s: %w", root, err)
	}
	return usage, nil
}

type benchmarkDiskSamples struct {
	final                         benchmarkDiskSnapshot
	peakTotal, peakWAL, peakFiles int64
}

func (s *benchmarkDiskSamples) record(usage benchmarkDiskSnapshot) {
	s.final = usage
	s.peakTotal = max(s.peakTotal, usage.total)
	s.peakWAL = max(s.peakWAL, usage.wal)
	s.peakFiles = max(s.peakFiles, usage.files)
}

func (s benchmarkDiskSamples) addMetrics(metrics map[string]readLoadMetric) {
	metrics["disk-inuse"] = readLoadMetric{Value: float64(s.final.total)}
	metrics["peak-disk"] = readLoadMetric{Value: float64(s.peakTotal)}
	metrics["sqlite-db-disk"] = readLoadMetric{Value: float64(s.final.database)}
	metrics["peak-sqlite-wal-disk"] = readLoadMetric{Value: float64(s.peakWAL)}
	metrics["data-files-disk"] = readLoadMetric{Value: float64(s.final.files)}
	metrics["peak-data-files-disk"] = readLoadMetric{Value: float64(s.peakFiles)}
}
