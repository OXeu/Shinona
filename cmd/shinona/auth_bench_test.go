package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/auth"
)

func benchmarkAuthSession(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	read := func(ctx context.Context, client *http.Client, headers http.Header) (benchmarkResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		result, err := a.measuredRequest(ctx, client, http.MethodGet, "/auth/session", nil, http.StatusOK, headers)
		if err != nil {
			return result, err
		}
		var session struct {
			User *struct{ ID string }
		}
		if err := json.Unmarshal(result.body, &session); err != nil {
			return result, err
		}
		if session.User == nil || session.User.ID != a.owner {
			return result, fmt.Errorf("unexpected authentication response: %s", result.body)
		}
		return result, nil
	}
	b.Run("Hit", func(b *testing.B) {
		if _, err := read(b.Context(), a.Client(), nil); err != nil {
			b.Fatal(err)
		}
		var size benchmarkPayloadSize
		var latencies benchmarkLatencies
		for b.Loop() {
			start := time.Now()
			response, err := read(b.Context(), a.Client(), nil)
			latencies.add(time.Since(start))
			if err != nil {
				b.Fatal(err)
			}
			size.add(response)
		}
		size.report(b)
		reportBenchmarkLatency(b, latencies.retained())
	})
	b.Run("Miss", func(b *testing.B) {
		var size benchmarkPayloadSize
		var latencies benchmarkLatencies
		for i := 0; b.Loop(); i++ {
			b.StopTimer()
			// SetAdmin invalidates all cached sessions for this owner.
			if err := a.repo.Accounts.SetAdmin(b.Context(), a.owner, i%2 == 0); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			start := time.Now()
			response, err := read(b.Context(), a.Client(), nil)
			latencies.add(time.Since(start))
			if err != nil {
				b.Fatal(err)
			}
			size.add(response)
		}
		size.report(b)
		reportBenchmarkLatency(b, latencies.retained())
	})
	b.Run("InvalidateOwner", func(b *testing.B) {
		const sessions = 32
		headers := make([]http.Header, sessions)
		transport := a.Client().Transport.(*http.Transport).Clone()
		transport.MaxIdleConns, transport.MaxIdleConnsPerHost = sessions, sessions
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		for i := range headers {
			token, err := a.accounts.StartSession(b.Context(), auth.User{ID: a.owner})
			if err != nil {
				b.Fatal(err)
			}
			cookie := http.Cookie{Name: "shinona_session", Value: token}
			headers[i] = http.Header{"Cookie": {cookie.String()}}
			if _, err := read(b.Context(), client, headers[i]); err != nil {
				b.Fatal(err)
			}
		}
		stop := make(chan struct{})
		stopped := make(chan error, 1)
		// Guarantee an owner invalidation before the readers start. Keep
		// invalidating their populated entries throughout the timed window.
		if err := a.repo.Accounts.SetAdmin(b.Context(), a.owner, true); err != nil {
			b.Fatal(err)
		}
		go func() {
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			admin := false
			for {
				select {
				case <-ticker.C:
					if err := a.repo.Accounts.SetAdmin(b.Context(), a.owner, admin); err != nil {
						stopped <- err
						return
					}
					admin = !admin
				case <-stop:
					stopped <- nil
					return
				}
			}
		}()
		var mu sync.Mutex
		latencies := make([][]time.Duration, 0, sessions)
		var size benchmarkPayloadSize
		var readers atomic.Uint64
		b.SetParallelism(16) // 32 concurrent readers with the CI's -cpu 2.
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			header := headers[(readers.Add(1)-1)%sessions]
			var samples benchmarkLatencies
			var payload benchmarkPayloadSize
			for pb.Next() {
				start := time.Now()
				response, err := read(b.Context(), client, header)
				samples.add(time.Since(start))
				if err != nil {
					b.Error(err)
					return
				}
				payload.add(response)
			}
			mu.Lock()
			if samples.count > 0 {
				latencies = append(latencies, samples.retained())
			}
			size.wire += payload.wire
			size.body += payload.body
			mu.Unlock()
		})
		b.StopTimer()
		close(stop)
		if err := <-stopped; err != nil {
			b.Fatal(err)
		}
		if len(latencies) != 0 {
			reportBenchmarkLatency(b, slices.Concat(latencies...))
			size.report(b)
		}
	})
}

// Keep percentile bookkeeping bounded, including when running long locally.
type benchmarkLatencies struct {
	samples [8192]time.Duration
	count   int
}

func (l *benchmarkLatencies) add(latency time.Duration) {
	l.samples[l.count%len(l.samples)] = latency
	l.count++
}

func (l *benchmarkLatencies) retained() []time.Duration {
	return l.samples[:min(l.count, len(l.samples))]
}

func reportBenchmarkLatency(b *testing.B, samples []time.Duration) readLoadMetric {
	latency := benchmarkLatency(samples)
	b.ReportMetric(latency.P50/float64(time.Microsecond), "p50-us/op")
	b.ReportMetric(latency.P99/float64(time.Microsecond), "p99-us/op")
	return latency
}
