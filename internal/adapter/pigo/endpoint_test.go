package pigo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestCachedCatalogDoesNotWaitForModelDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-release:
			_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture-model","contextWindow":1000000}]}`)
		case <-ctx.Done():
		}
	}))
	defer server.Close()
	defer cancel()
	endpoint, err := NewEndpoint(server.URL, "fixture", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		models, loading := endpoint.CachedCatalog()
		if !loading || len(models) != 0 {
			t.Fatalf("cold catalog must return immediately while refreshing: %v, loading %t", models, loading)
		}
	}
	close(release)
	models, err := endpoint.Catalog(ctx, false)
	if err != nil || len(models) != 1 || models[0].ID != "fixture-model" {
		t.Fatalf("completed refresh: %v, %v", models, err)
	}
	cached, loading := endpoint.CachedCatalog()
	if loading || len(cached) != 1 || cached[0].ContextWindow != 1000000 || requests.Load() != 1 {
		t.Fatalf("cached catalog: %v, loading %t, requests %d", cached, loading, requests.Load())
	}
}

func TestCompatibleEndpointCatalogAndComposerPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	reportedUsage := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("model list request is missing its credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[{"id":"default","context_length":1000000,"contextWindow":{}},{"id":"vision","contextWindow":32000,"context_length":"32000"},{"id":"vision"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("completion request is missing its credential")
		}
		var request struct {
			Model               string
			MaxTokens           *int   `json:"max_tokens"`
			MaxCompletionTokens *int   `json:"max_completion_tokens"`
			ReasoningEffort     string `json:"reasoning_effort"`
			Messages            json.RawMessage
			Tools               []struct {
				Function struct{ Name string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.MaxTokens != nil || request.MaxCompletionTokens != nil {
			t.Error("completion request must leave the token limit to the model endpoint")
		}
		time.Sleep(30 * time.Millisecond)
		if request.Model != "vision" || request.ReasoningEffort != "high" ||
			!strings.Contains(string(request.Messages), `"type":"image_url"`) ||
			len(request.Tools) != 1 || request.Tools[0].Function.Name != "read" {
			t.Errorf("composer options were not forwarded: %+v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"one\",\"object\":\"chat.completion.chunk\",\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Image received.\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"id\":\"one\",\"object\":\"chat.completion.chunk\",\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"id\":\"one\",\"object\":\"chat.completion.chunk\",\"model\":\"vision\",\"choices\":[],\"usage\":{\"prompt_tokens\":125,\"completion_tokens\":6,\"total_tokens\":131}}\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-reportedUsage:
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		case <-ctx.Done():
			t.Error("usage-only chunk was not reported before the stream closed")
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	httpClient := *server.Client()
	endpoint, err := NewEndpoint(server.URL+"/v1", "fixture", &httpClient)
	if err != nil {
		t.Fatal(err)
	}
	models, err := endpoint.Catalog(t.Context(), true)
	if err != nil || len(models) != 2 || models[0].ContextWindow != 1000000 || models[1].ID != "vision" || models[1].ContextWindow != 32000 {
		t.Fatalf("model discovery: %v, %v", models, err)
	}
	httpClient.Timeout = 10 * time.Millisecond
	var readings []run.ContextUsage
	result, err := New(endpoint, "default", nil).Run(ctx, run.Input{
		Payload: run.Payload{
			Options: run.Options{Model: "vision", Permission: run.ReadOnly, Thinking: "high"},
			Images:  []run.Image{{MediaType: "image/png", Data: "iVBORw0KGgo="}},
		},
		ReportContext: func(usage run.ContextUsage) {
			readings = append(readings, usage)
			if usage == (run.ContextUsage{Tokens: 131, Window: 32000}) {
				close(reportedUsage)
			}
		},
	}, &recordingWorkspace{})
	if err != nil || result.Output != "Image received." || result.ContextTokens != 131 || result.ContextWindow != 32000 {
		t.Fatalf("completion: %+v, %v", result, err)
	}
	if len(readings) != 1 || readings[0] != (run.ContextUsage{Tokens: 131, Window: 32000}) {
		t.Fatalf("expected one provider-reported reading: %+v", readings)
	}
}
