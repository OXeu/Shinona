package main

import (
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

type benchmarkResources struct {
	start        goruntime.MemStats
	cpu          time.Duration
	cpuAvailable bool
	rssReset     bool
	stopSampling chan struct{}
	sampled      chan benchmarkResourceSamples
}

type benchmarkResourceSamples struct {
	peakHeap uint64
	disk     benchmarkDiskSamples
	err      error
}

func startBenchmarkResources(databasePath string) (*benchmarkResources, error) {
	goruntime.GC()
	initial, err := benchmarkDiskUsage(databasePath)
	if err != nil {
		return nil, err
	}
	monitor := &benchmarkResources{
		stopSampling: make(chan struct{}),
		sampled:      make(chan benchmarkResourceSamples, 1),
	}
	goruntime.ReadMemStats(&monitor.start)
	monitor.rssReset = resetBenchmarkPeakRSS()
	monitor.cpu, monitor.cpuAvailable = benchmarkProcessCPU()
	go func() {
		var mem goruntime.MemStats
		samples := benchmarkResourceSamples{peakHeap: monitor.start.HeapInuse}
		samples.disk.record(initial)
		heapTicker := time.NewTicker(25 * time.Millisecond)
		defer heapTicker.Stop()
		diskTicker := time.NewTicker(250 * time.Millisecond)
		defer diskTicker.Stop()
		sampleDisk := func() {
			if samples.err != nil {
				return
			}
			disk, err := benchmarkDiskUsage(databasePath)
			if err != nil {
				samples.err = err
				return
			}
			samples.disk.record(disk)
		}
		for {
			select {
			case <-heapTicker.C:
				goruntime.ReadMemStats(&mem)
				samples.peakHeap = max(samples.peakHeap, mem.HeapInuse)
			case <-diskTicker.C:
				sampleDisk()
			case <-monitor.stopSampling:
				sampleDisk()
				monitor.sampled <- samples
				return
			}
		}
	}()
	return monitor, nil
}

func (m *benchmarkResources) stop() {
	if m.stopSampling != nil {
		close(m.stopSampling)
		<-m.sampled
		m.stopSampling = nil
	}
}

func (m *benchmarkResources) finish(requests int) (map[string]readLoadMetric, error) {
	close(m.stopSampling)
	samples := <-m.sampled
	m.stopSampling = nil
	if samples.err != nil {
		return nil, samples.err
	}
	cpu, cpuAvailable := benchmarkProcessCPU()
	var after goruntime.MemStats
	goruntime.ReadMemStats(&after)
	rss, rssAvailable := benchmarkPeakRSS()
	goruntime.GC()
	goruntime.GC()
	var retained goruntime.MemStats
	goruntime.ReadMemStats(&retained)
	metrics := map[string]readLoadMetric{
		"alloc-bytes-per-req": {Value: float64(after.TotalAlloc-m.start.TotalAlloc) / float64(requests)},
		"peak-heap":           {Value: float64(max(samples.peakHeap, after.HeapInuse))},
		"heap-inuse":          {Value: float64(retained.HeapInuse)},
		"gc-pause":            {Value: float64(after.PauseTotalNs-m.start.PauseTotalNs) / float64(time.Microsecond) / float64(requests)},
	}
	if m.cpuAvailable && cpuAvailable {
		metrics["cpu-time"] = readLoadMetric{Value: float64(cpu-m.cpu) / float64(time.Microsecond) / float64(requests)}
	}
	if m.rssReset && rssAvailable {
		metrics["peak-rss"] = readLoadMetric{Value: float64(rss)}
	}
	samples.disk.addMetrics(metrics)
	return metrics, nil
}

// Benchmarks aggregate only server timing phases that were actually present.
type benchmarkTiming map[string]float64

func (response benchmarkResponse) serverTiming() benchmarkTiming {
	phases := make(benchmarkTiming)
	response.recordTiming(func(name string, ms float64) { phases[name] += ms })
	return phases
}

func (p benchmarkTiming) has(phase string) bool {
	_, ok := p[phase]
	return ok
}

func (response benchmarkResponse) recordTiming(record func(name string, ms float64)) {
	for _, header := range []map[string][]string{response.header, response.trailer} {
		for _, field := range header["Server-Timing"] {
			for phase := range strings.SplitSeq(field, ",") {
				name, duration, ok := strings.Cut(strings.TrimSpace(phase), ";dur=")
				if !ok {
					continue
				}
				if ms, err := strconv.ParseFloat(duration, 64); err == nil {
					record(name, ms)
				}
			}
		}
	}
}
