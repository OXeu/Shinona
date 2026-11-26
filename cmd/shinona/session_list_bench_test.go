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
	poll := func(b *testing.B, etag string, status int) (benchmarkSessionList, string, benchmarkResponse) {
		b.Helper()
		ctx, cancel := context.WithTimeout(b.Context(), 15*time.Second)
		defer cancel()
		var requestHeaders http.Header
		if etag != "" {
			requestHeaders = http.Header{"If-None-Match": {etag}}
		}
		response, err := a.measuredRequest(ctx, a.Client(), http.MethodGet, path, nil, status, requestHeaders)
		if err != nil {
			b.Fatal(err)
		}
		tag := response.header.Get("ETag")
		if tag == "" || (status == http.StatusNotModified && (tag != etag || len(response.body) != 0)) {
			b.Fatalf("unexpected response: ETag %q, body %s", tag, response.body)
		}
		var result benchmarkSessionList
		if status == http.StatusOK {
			if err := json.Unmarshal(response.body, &result); err != nil {
				b.Fatal(err)
			}
		}
		return result, tag, response
	}
	full := func(b *testing.B) (benchmarkSessionList, string, benchmarkResponse) {
		b.Helper()
		page, etag, size := poll(b, "", http.StatusOK)
		if len(page.Projects) != projectCount || len(page.Sessions) != 30 {
			b.Fatalf("unexpected full response: %d projects, %d sessions", len(page.Projects), len(page.Sessions))
		}
		return page, etag, size
	}
	b.Run("Full", func(b *testing.B) {
		var size benchmarkPayloadSize
		for b.Loop() {
			_, _, response := full(b)
			size.add(response)
		}
		size.report(b)
	})
	b.Run("NotModified", func(b *testing.B) {
		_, etag, _ := full(b)
		for b.Loop() {
			poll(b, etag, http.StatusNotModified)
		}
		(benchmarkPayloadSize{}).report(b)
	})
	b.Run("Delta", func(b *testing.B) {
		page, etag, _ := full(b)
		session := page.Sessions[0].ID
		titles := [2]string{"Reviewed workspace A", "Reviewed workspace B"}
		var size benchmarkPayloadSize
		var iteration int
		if page.Sessions[0].Title == titles[0] {
			iteration = 1
		}
		for b.Loop() {
			title := titles[iteration%len(titles)]
			b.StopTimer()
			if _, err := a.repo.Conversations.UpdateSession(b.Context(), a.owner, session, run.SessionChanges{Title: &title}); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			patch, current, response := poll(b, etag, http.StatusOK)
			if patch.Delta == nil || current == etag {
				b.Fatal("expected a new incremental response")
			}
			upsert := patch.Delta.Sessions.Upsert
			if len(upsert) != 1 || upsert[0].ID != session || upsert[0].Title != title {
				b.Fatalf("unexpected delta: %+v", upsert)
			}
			etag = current
			size.add(response)
			iteration++
		}
		size.report(b)
	})
}
