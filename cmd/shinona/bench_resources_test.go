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
	peak         chan uint64
}

func startBenchmarkResources() *benchmarkResources {
	goruntime.GC()
	monitor := &benchmarkResources{stopSampling: make(chan struct{}), peak: make(chan uint64, 1)}
	goruntime.ReadMemStats(&monitor.start)
	monitor.rssReset = resetBenchmarkPeakRSS()
	monitor.cpu, monitor.cpuAvailable = benchmarkProcessCPU()
	go func() {
		var sample goruntime.MemStats
		peak := monitor.start.HeapInuse
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				goruntime.ReadMemStats(&sample)
				peak = max(peak, sample.HeapInuse)
			case <-monitor.stopSampling:
				monitor.peak <- peak
				return
			}
		}
	}()
	return monitor
}

func (m *benchmarkResources) stop() {
	if m.stopSampling != nil {
		close(m.stopSampling)
		<-m.peak
		m.stopSampling = nil
	}
}

func (m *benchmarkResources) finish(requests int) map[string]readLoadMetric {
	close(m.stopSampling)
	peak := <-m.peak
	m.stopSampling = nil
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
		"peak-heap":           {Value: float64(max(peak, after.HeapInuse))},
		"heap-inuse":          {Value: float64(retained.HeapInuse)},
		"gc-pause":            {Value: float64(after.PauseTotalNs-m.start.PauseTotalNs) / float64(time.Microsecond) / float64(requests)},
	}
	if m.cpuAvailable && cpuAvailable {
		metrics["cpu-time"] = readLoadMetric{Value: float64(cpu-m.cpu) / float64(time.Microsecond) / float64(requests)}
	}
	if m.rssReset && rssAvailable {
		metrics["peak-rss"] = readLoadMetric{Value: float64(rss)}
	}
	return metrics
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
