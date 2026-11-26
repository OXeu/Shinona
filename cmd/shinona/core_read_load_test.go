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
}

type readLoadMetric struct {
	Value float64 `json:"value"`
	P50   float64 `json:"p50,omitempty"`
	P99   float64 `json:"p99,omitempty"`
}

var readLoadConcurrency = [...]int{1, 4, 16, 64}

// TestCoreReadLoad runs only when asked to produce a BMF v1 load report.
func TestCoreReadLoad(t *testing.T) {
	output := os.Getenv("SHINONA_BENCH_LOAD_OUTPUT")
	if output == "" {
		t.Skip("set SHINONA_BENCH_LOAD_OUTPUT to run core read load")
	}
	duration := 5 * time.Second
	if value := os.Getenv("SHINONA_BENCH_LOAD_DURATION"); value != "" {
		var err error
		duration, err = time.ParseDuration(value)
		if err != nil || duration <= 0 {
			t.Fatalf("invalid read load duration %q", value)
		}
	}

	report := make(map[string][]readLoadVariant)
	run := func(t *testing.T, benchmark string, params readLoadParameters, newClient func(testing.TB, int) benchmarkRead) {
		t.Helper()
		t.Run(fmt.Sprintf("c=%d", params.Concurrency), func(t *testing.T) {
			clients := make([]benchmarkRead, params.Concurrency)
			for i := range clients {
				clients[i] = newClient(t, i)
			}
			metrics, count, err := measureReadLoad(t.Context(), clients, duration)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d requests, %.1f req/s, p50 %.2f ms, p99 %.2f ms", count,
				metrics["throughput"].Value, metrics["latency"].P50/1e6, metrics["latency"].P99/1e6)
			report[benchmark] = append(report[benchmark], readLoadVariant{Parameters: params, Measures: metrics})
		})
	}

	t.Run("SessionsList", func(t *testing.T) {
		for _, size := range []int{256, 2_000} {
			t.Run(fmt.Sprintf("sessions=%d", size), func(t *testing.T) {
				api := newCoreBenchmarkAPI(t, demo.Agent{})
				projects := api.seedBenchmarkSessions(t, size)
				for _, concurrency := range readLoadConcurrency {
					run(t, "CoreAPI/SessionsList", readLoadParameters{Concurrency: concurrency, Sessions: size},
						func(t testing.TB, _ int) benchmarkRead { return api.recentBenchmarkClient(t, projects) })
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
		transport := api.Client().Transport.(*http.Transport).Clone()
		transport.MaxIdleConns = readLoadConcurrency[len(readLoadConcurrency)-1]
		transport.MaxIdleConnsPerHost = transport.MaxIdleConns
		t.Cleanup(transport.CloseIdleConnections)
		client := &http.Client{Transport: transport, Jar: api.Client().Jar}
		for _, page := range []string{"latest", "full"} {
			t.Run("page="+page, func(t *testing.T) {
				for _, concurrency := range readLoadConcurrency {
					run(t, "CoreAPI/Transcript", readLoadParameters{Concurrency: concurrency, Turns: turns, Page: page},
						func(t testing.TB, i int) benchmarkRead {
							return api.transcriptBenchmarkClient(t, client, sessions[i%len(sessions)], turns, page == "latest")
						})
				}
			})
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

func measureReadLoad(ctx context.Context, clients []benchmarkRead, duration time.Duration) (map[string]readLoadMetric, int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ready, workers sync.WaitGroup
	var firstError sync.Once
	var requestErr error
	var deadline time.Time
	start := make(chan struct{})
	results := make([][]time.Duration, len(clients))
	fail := func(err error) {
		firstError.Do(func() {
			requestErr = err
			cancel()
		})
	}

	ready.Add(len(clients))
	for i, read := range clients {
		workers.Go(func() {
			if err := read(ctx); err != nil {
				fail(fmt.Errorf("warm up client %d: %w", i, err))
			}
			ready.Done()
			<-start

			for ctx.Err() == nil {
				sent := time.Now()
				if !sent.Before(deadline) {
					return
				}
				if err := read(ctx); err != nil {
					fail(fmt.Errorf("request from client %d: %w", i, err))
					return
				}
				results[i] = append(results[i], time.Since(sent))
			}
		})
	}
	ready.Wait()
	started := time.Now()
	deadline = started.Add(duration)
	close(start)
	workers.Wait()
	elapsed := time.Since(started)
	if requestErr != nil {
		return nil, 0, requestErr
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	var latencies []time.Duration
	var total time.Duration
	for i, samples := range results {
		if len(samples) == 0 {
			return nil, 0, fmt.Errorf("client %d completed no measured requests", i)
		}
		latencies = append(latencies, samples...)
		for _, latency := range samples {
			total += latency
		}
	}
	slices.Sort(latencies)
	count := len(latencies)
	return map[string]readLoadMetric{
		"latency": {
			Value: float64(total) / float64(count),
			P50:   float64(latencies[(count*50+99)/100-1]),
			P99:   float64(latencies[(count*99+99)/100-1]),
		},
		"throughput": {Value: float64(count) / elapsed.Seconds()},
	}, count, nil
}
