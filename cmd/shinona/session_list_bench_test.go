package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/run"
)

type benchmarkSessionList struct {
	Projects []run.Project
	Sessions []struct{ ID, Title string }
	Delta    *struct {
		Sessions struct {
			Upsert []struct{ ID, Title string }
		}
	}
}

func benchmarkSessionListPoll(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	projectCount := a.seedBenchmarkSessions(b, 256)
	path := rest.Prefix + "/sessions?delta=true"
	poll := func(b *testing.B, etag string, status int) (benchmarkSessionList, string, int) {
		b.Helper()
		ctx, cancel := context.WithTimeout(b.Context(), 15*time.Second)
		defer cancel()
		var requestHeaders http.Header
		if etag != "" {
			requestHeaders = http.Header{"If-None-Match": {etag}}
		}
		body, headers, err := a.requestWithHeaders(ctx, a.Client(), http.MethodGet, path, nil, status, requestHeaders)
		if err != nil {
			b.Fatal(err)
		}
		tag := headers.Get("ETag")
		if tag == "" || (status == http.StatusNotModified && (tag != etag || len(body) != 0)) {
			b.Fatalf("unexpected response: ETag %q, body %s", tag, body)
		}
		var result benchmarkSessionList
		if status == http.StatusOK {
			if err := json.Unmarshal(body, &result); err != nil {
				b.Fatal(err)
			}
		}
		return result, tag, len(body)
	}
	full := func(b *testing.B) (benchmarkSessionList, string, int) {
		b.Helper()
		page, etag, size := poll(b, "", http.StatusOK)
		if len(page.Projects) != projectCount || len(page.Sessions) != 30 {
			b.Fatalf("unexpected full response: %d projects, %d sessions", len(page.Projects), len(page.Sessions))
		}
		return page, etag, size
	}
	b.Run("Full", func(b *testing.B) {
		var size int
		for b.Loop() {
			_, _, size = full(b)
		}
		b.ReportMetric(float64(size), "body-B/op")
	})
	b.Run("NotModified", func(b *testing.B) {
		_, etag, _ := full(b)
		for b.Loop() {
			poll(b, etag, http.StatusNotModified)
		}
		b.ReportMetric(0, "body-B/op")
	})
	b.Run("Delta", func(b *testing.B) {
		page, etag, _ := full(b)
		session := page.Sessions[0].ID
		titles := [2]string{"Reviewed workspace A", "Reviewed workspace B"}
		var bytes, iteration int
		if page.Sessions[0].Title == titles[0] {
			iteration = 1
		}
		for b.Loop() {
			title := titles[iteration%len(titles)]
			b.StopTimer()
			if _, err := a.repo.UpdateSession(b.Context(), a.owner, session, run.SessionChanges{Title: &title}); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			patch, current, size := poll(b, etag, http.StatusOK)
			if patch.Delta == nil || current == etag {
				b.Fatal("expected a new incremental response")
			}
			upsert := patch.Delta.Sessions.Upsert
			if len(upsert) != 1 || upsert[0].ID != session || upsert[0].Title != title {
				b.Fatalf("unexpected delta: %+v", upsert)
			}
			etag = current
			bytes += size
			iteration++
		}
		b.ReportMetric(float64(bytes)/float64(b.N), "body-B/op")
	})
}
