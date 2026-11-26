package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/run"
)

type readLoadVariant struct {
	Parameters readLoadParameters        `json:"parameters"`
	Measures   map[string]readLoadMetric `json:"measures"`
}

type readLoadParameters struct {
	Concurrency int    `json:"concurrency"`
	Sessions    int    `json:"sessions,omitempty"`
	Turns       int    `json:"turns,omitempty"`
	Page        string `json:"page,omitempty"`
	Writers     int    `json:"writers,omitempty"`
}

type readLoadMetric struct {
	Value float64 `json:"value"`
	P50   float64 `json:"p50,omitempty"`
	P99   float64 `json:"p99,omitempty"`
}

type benchmarkLoadClient struct {
	request    benchmarkRead
	transcript bool
	interval   time.Duration // Nonzero for background writers.
}

var readLoadConcurrency = [...]int{1, 4, 16, 64}

// TestCoreReadLoad writes the pure read, mixed and streaming load variants to BMF v1.
func TestCoreReadLoad(t *testing.T) {
	output := os.Getenv("SHINONA_BENCH_LOAD_OUTPUT")
	if output == "" {
		t.Skip("set SHINONA_BENCH_LOAD_OUTPUT to run core load benchmarks")
	}
	duration := 5 * time.Second
	if value := os.Getenv("SHINONA_BENCH_LOAD_DURATION"); value != "" {
		var err error
		duration, err = time.ParseDuration(value)
		if err != nil || duration <= 0 {
			t.Fatalf("invalid load duration %q", value)
		}
	}

	report := make(map[string][]readLoadVariant)
	run := func(t *testing.T, benchmark string, params readLoadParameters, clients func(testing.TB, int) benchmarkLoadClient,
		writers []benchmarkLoadClient,
	) {
		t.Helper()
		t.Run(fmt.Sprintf("c=%d", params.Concurrency), func(t *testing.T) {
			work := make([]benchmarkLoadClient, params.Concurrency, params.Concurrency+len(writers))
			for i := range work {
				work[i] = clients(t, i)
			}
			work = append(work, writers...)
			metrics, count, phases, err := measureReadLoad(t.Context(), work, duration)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d requests, %.1f read req/s, read p50 %.2f ms, read p99 %.2f ms, metrics %+v",
				count, metrics["throughput"].Value, metrics["latency"].P50/1e6, metrics["latency"].P99/1e6, metrics)
			t.Logf("Server-Timing in ms averaged over requests reporting each phase: %v", phases)
			report[benchmark] = append(report[benchmark], readLoadVariant{Parameters: params, Measures: metrics})
		})
	}

	t.Run("SessionsList", func(t *testing.T) {
		for _, size := range []int{256, 2_000} {
			t.Run(fmt.Sprintf("sessions=%d", size), func(t *testing.T) {
				api := newCoreBenchmarkAPI(t, demo.Agent{})
				projects := api.seedBenchmarkSessions(t, size)
				benchmarkLoadTransport(t, api)
				for _, concurrency := range readLoadConcurrency {
					run(t, "CoreAPI/SessionsList", readLoadParameters{Concurrency: concurrency, Sessions: size},
						func(t testing.TB, _ int) benchmarkLoadClient {
							return benchmarkLoadClient{request: api.recentBenchmarkClient(t, projects)}
						}, nil)
				}
			})
		}
	})
	t.Run("Transcript", func(t *testing.T) {
		api := newCoreBenchmarkAPI(t, demo.Agent{})
		const turns, logLines = 200, 80
		sessions := make([]string, 4)
		for i := range sessions {
			sessions[i] = api.seedBenchmarkTranscript(t, turns, logLines)
		}
		benchmarkLoadTransport(t, api)
		for _, page := range []string{"latest", "full"} {
			t.Run("page="+page, func(t *testing.T) {
				for _, concurrency := range readLoadConcurrency {
					run(t, "CoreAPI/Transcript", readLoadParameters{Concurrency: concurrency, Turns: turns, Page: page},
						func(t testing.TB, i int) benchmarkLoadClient {
							return benchmarkLoadClient{
								request:    api.transcriptBenchmarkClient(t, api.Client(), sessions[i%len(sessions)], turns, page == "latest"),
								transcript: true,
							}
						}, nil)
				}
			})
		}
	})
	t.Run("Mixed", func(t *testing.T) {
		api := newCoreBenchmarkAPI(t, demo.Agent{})
		benchmarkLoadTransport(t, api)
		const sessions, turns = 256, 120
		projects := api.seedBenchmarkSessions(t, sessions)
		transcripts := make([]string, 4)
		for i := range transcripts {
			transcripts[i] = api.seedBenchmarkTranscript(t, turns, 48)
		}
		writers := api.mixedLoadWriters(t, duration, len(readLoadConcurrency)-1)
		for _, concurrency := range readLoadConcurrency[1:] {
			run(t, "CoreAPI/Mixed", readLoadParameters{Concurrency: concurrency, Sessions: sessions + len(transcripts) + len(writers), Turns: turns, Writers: len(writers)},
				func(t testing.TB, i int) benchmarkLoadClient {
					if i%2 == 0 {
						return benchmarkLoadClient{request: api.recentBenchmarkClient(t, projects)}
					}
					return benchmarkLoadClient{
						request:    api.transcriptBenchmarkClient(t, api.Client(), transcripts[(i/2)%len(transcripts)], turns, true),
						transcript: true,
					}
				}, writers)
		}
	})
	t.Run("TranscriptFullStream", func(t *testing.T) {
		api := newCoreBenchmarkAPI(t, demo.Agent{})
		benchmarkLoadTransport(t, api)
		session := api.seedBenchmarkStream(t)
		stream := api.fullStreamBenchmarkClient(t, api.Client(), session)
		for _, concurrency := range []int{1, 64} {
			run(t, "CoreAPI/TranscriptFullStream", readLoadParameters{Concurrency: concurrency, Turns: benchmarkStreamTurns},
				func(testing.TB, int) benchmarkLoadClient {
					return benchmarkLoadClient{request: stream, transcript: true}
				}, nil)
		}
	})
	if t.Failed() {
		return
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func benchmarkLoadTransport(t testing.TB, api *benchmarkAPI) {
	t.Helper()
	transport := api.Client().Transport.(*http.Transport).Clone()
	transport.MaxIdleConns = readLoadConcurrency[len(readLoadConcurrency)-1] + 2
	transport.MaxIdleConnsPerHost = transport.MaxIdleConns
	api.Client().Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
}

func (a *benchmarkAPI) mixedLoadWriters(t *testing.T, duration time.Duration, variants int) []benchmarkLoadClient {
	t.Helper()
	titlePath := benchmarkSessionPath(a.session(t, ""))
	markerSession := a.session(t, "")
	markerPath := benchmarkSessionPath(markerSession) + "/read-marker"
	const markInterval = 35 * time.Millisecond
	markers := make([][]byte, variants*(int(duration/markInterval)+1))
	for i := range markers {
		turn := a.completedTurn(t, markerSession, benchmarkPrompt, run.Result{Output: "Reviewed."})
		markers[i] = a.requestBody(t, map[string]any{"through": turn.Sequence, "throughId": turn.ID})
	}
	titles := [][]byte{
		a.requestBody(t, map[string]string{"title": "Writer title A"}),
		a.requestBody(t, map[string]string{"title": "Writer title B"}),
	}
	var titleIndex, markerIndex int
	return []benchmarkLoadClient{
		{interval: 20 * time.Millisecond, request: func(ctx context.Context) (benchmarkResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			request := titles[titleIndex]
			titleIndex = 1 - titleIndex
			return a.measuredRequest(ctx, a.Client(), http.MethodPatch, titlePath, request, http.StatusOK, nil)
		}},
		{interval: markInterval, request: func(ctx context.Context) (benchmarkResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			request := markers[markerIndex]
			markerIndex++
			return a.measuredRequest(ctx, a.Client(), http.MethodPut, markerPath, request, http.StatusNoContent, nil)
		}},
	}
}

type benchmarkLoadSamples struct {
	latencies []time.Duration
	bytes     benchmarkPayloadSize
	hits      int
	phases    map[string]phaseTotal
}

type phaseTotal struct {
	milliseconds float64
	count        int
}

func (p *phaseTotal) add(ms float64, count int) {
	p.milliseconds += ms
	p.count += count
}

func measureReadLoad(ctx context.Context, clients []benchmarkLoadClient, duration time.Duration) (
	map[string]readLoadMetric, int, benchmarkTiming, error,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ready, workers sync.WaitGroup
	var firstError sync.Once
	var requestErr error
	var deadline time.Time
	start := make(chan struct{})
	results := make([]benchmarkLoadSamples, len(clients))
	fail := func(err error) {
		firstError.Do(func() {
			requestErr = err
			cancel()
		})
	}

	ready.Add(len(clients))
	for i, client := range clients {
		workers.Go(func() {
			result := &results[i]
			result.phases = make(map[string]phaseTotal)
			if client.interval == 0 {
				if _, err := client.request(ctx); err != nil {
					fail(fmt.Errorf("warm up reader %d: %w", i, err))
				}
			}
			ready.Done()
			<-start

			for ctx.Err() == nil && time.Now().Before(deadline) {
				sent := time.Now()
				response, err := client.request(ctx)
				if err != nil {
					fail(fmt.Errorf("request from client %d: %w", i, err))
					return
				}
				result.latencies = append(result.latencies, time.Since(sent))
				result.bytes.add(response)
				response.recordTiming(func(name string, ms float64) {
					phase := result.phases[name]
					phase.add(ms, 1)
					result.phases[name] = phase
					if name == "transcript_response_cache_hit" {
						result.hits++
					}
				})
				if client.interval > 0 && !waitBenchmarkWriter(ctx, sent.Add(client.interval), deadline) {
					return
				}
			}
		})
	}
	ready.Wait()
	monitor := startBenchmarkResources()
	defer monitor.stop()
	started := time.Now()
	deadline = started.Add(duration)
	close(start)
	workers.Wait()
	elapsed := time.Since(started)
	if requestErr != nil {
		return nil, 0, nil, requestErr
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, nil, err
	}
	count := 0
	for _, result := range results {
		count += len(result.latencies)
	}
	if count == 0 {
		return nil, 0, nil, fmt.Errorf("load completed no measured requests")
	}
	metrics := monitor.finish(count)
	var reads, writes, lists, transcripts []time.Duration
	var bytes benchmarkPayloadSize
	var cacheHits, transcriptCount int
	phases := make(map[string]phaseTotal)
	for i, result := range results {
		if clients[i].interval > 0 {
			writes = append(writes, result.latencies...)
			continue
		}
		reads = append(reads, result.latencies...)
		if clients[i].transcript {
			transcripts = append(transcripts, result.latencies...)
			cacheHits += result.hits
			transcriptCount += len(result.latencies)
		} else {
			lists = append(lists, result.latencies...)
		}
		bytes.wire += result.bytes.wire
		bytes.body += result.bytes.body
		for name, sample := range result.phases {
			phase := phases[name]
			phase.add(sample.milliseconds, sample.count)
			phases[name] = phase
		}
	}
	if len(reads) == 0 || (len(clients) > 0 && clients[len(clients)-1].interval > 0 && len(writes) == 0) {
		return nil, 0, nil, fmt.Errorf("load did not complete both required request types")
	}
	metrics["latency"] = benchmarkLatency(reads)
	metrics["throughput"] = readLoadMetric{Value: float64(len(reads)) / elapsed.Seconds()}
	metrics["wire-bytes-per-read"] = readLoadMetric{Value: float64(bytes.wire) / float64(len(reads))}
	if bytes.body != 0 {
		metrics["body-bytes-per-read"] = readLoadMetric{Value: float64(bytes.body) / float64(len(reads))}
	}
	if transcriptCount != 0 {
		metrics["cache-hit-rate"] = readLoadMetric{Value: 100 * float64(cacheHits) / float64(transcriptCount)}
	}
	if len(writes) != 0 {
		if len(lists) == 0 || len(transcripts) == 0 {
			return nil, 0, nil, fmt.Errorf("mixed load did not complete both read paths")
		}
		metrics["write-latency"] = benchmarkLatency(writes)
		metrics["write-throughput"] = readLoadMetric{Value: float64(len(writes)) / elapsed.Seconds()}
		metrics["sessions-list-latency"] = benchmarkLatency(lists)
		metrics["transcript-latency"] = benchmarkLatency(transcripts)
	}
	diagnostics := make(benchmarkTiming, len(phases))
	for name, phase := range phases {
		diagnostics[name] = phase.milliseconds / float64(phase.count)
	}
	return metrics, count, diagnostics, nil
}

func waitBenchmarkWriter(ctx context.Context, next, deadline time.Time) bool {
	if !next.Before(deadline) {
		return false
	}
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func benchmarkLatency(samples []time.Duration) readLoadMetric {
	var total time.Duration
	for _, sample := range samples {
		total += sample
	}
	slices.Sort(samples)
	count := len(samples)
	return readLoadMetric{
		Value: float64(total) / float64(count),
		P50:   float64(samples[(count*50+99)/100-1]),
		P99:   float64(samples[(count*99+99)/100-1]),
	}
}

func TestBenchmarkIdleWAL(t *testing.T) {
	if os.Getenv("SHINONA_BENCH_IDLE_WAL") == "" {
		t.Skip("set SHINONA_BENCH_IDLE_WAL=1 to measure idle SQLite checkpoint overhead")
	}
	benchmarkRepository(t)
	beforeCPU, cpuOK := benchmarkProcessCPU()
	beforeSwitches, switchesOK := benchmarkContextSwitches()
	if !cpuOK || !switchesOK {
		t.Skip("process CPU and context switch counters are unavailable")
	}
	time.Sleep(10 * time.Second)
	afterCPU, cpuOK := benchmarkProcessCPU()
	afterSwitches, switchesOK := benchmarkContextSwitches()
	if !cpuOK || !switchesOK {
		t.Fatal("process CPU or context switch counters disappeared")
	}
	t.Logf("idle WAL checkpoint over 10s: cpu-us %.1f, context-switches %d",
		float64(afterCPU-beforeCPU)/float64(time.Microsecond), afterSwitches-beforeSwitches)
}
