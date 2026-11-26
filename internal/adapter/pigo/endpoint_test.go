package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
)

func TestEndpointUsesPiMetadataOnlyForCatalogOrigin(t *testing.T) {
	const modelID = "gpt-4o"
	known := ai.GetModel(completionProvider, modelID)
	if known == nil || known.ContextWindow <= 0 {
		t.Fatal("Pi's built-in OpenAI model metadata is missing")
	}
	for _, fixture := range []struct {
		endpoint string
		usesPi   bool
	}{
		{known.BaseURL, true},
		{"https://API.OPENAI.COM:443/v1", true},
		{"https://api.openai.com:8443/v1", false},
		{"http://api.openai.com/v1", false},
		{"https://api.openai.com.proxy.example/v1", false},
	} {
		t.Run(fixture.endpoint, func(t *testing.T) {
			endpoint, err := NewEndpoint(fixture.endpoint, "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			model := endpoint.Model(modelID)
			if model.BaseURL != fixture.endpoint || model.Api != ai.APIOpenAICompletions {
				t.Fatalf("the selected model changed the request routing: %+v", model)
			}
			if fixture.usesPi {
				if model.ContextWindow != known.ContextWindow || model.Name != known.Name {
					t.Fatalf("Pi model metadata was lost: %+v", model)
				}
			} else {
				var compat map[string]json.RawMessage
				if err := json.Unmarshal(model.Compat, &compat); err != nil || string(compat["supportsDeveloperRole"]) != "false" || model.ContextWindow != 0 {
					t.Fatalf("custom endpoint inherited hosted model metadata: %+v, %v", model, err)
				}
			}
		})
	}
}

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

func TestAgentReportsModelInactivityWithPartialOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Partial answer.\"}}]}\n\n")
		controller := http.NewResponseController(w)
		if err := controller.Flush(); err != nil {
			return
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = fmt.Fprint(w, ": keepalive\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"}}]}\n\n")
				if err := controller.Flush(); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	endpoint, err := NewEndpoint(server.URL, "fixture", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	endpoint.completionIdle = 150 * time.Millisecond
	result, err := New(endpoint, "fixture", nil).Run(ctx, run.Input{Prompt: "Explain."}, &recordingWorkspace{})
	if !errors.Is(err, run.ErrModelIdle) || len(result.Parts) != 1 || result.Parts[0].Text != "Partial answer." {
		t.Fatalf("model idle timeout should preserve the partial response: %+v, %v", result.Parts, err)
	}
}

func TestCompatibleEndpointReplaysAcrossModelChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Messages json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if messages := string(request.Messages); !strings.Contains(messages, "Previous visible reply.") || strings.Contains(messages, "Previous private reasoning.") {
			t.Errorf("expected Pi to preserve the previous reply and drop the other model's reasoning: %s", messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Current reply.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	endpoint, err := NewEndpoint(server.URL, "fixture", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := New(endpoint, "current-model", nil).Run(t.Context(), run.Input{
		Prompt: "Continue.",
		History: []run.Exchange{{Prompt: "Start.", AgentMessages: []byte(`[
			{"role":"assistant","api":"openai-completions","provider":"openai","model":"previous-model","stopReason":"stop","content":[
				{"type":"thinking","thinking":"Previous private reasoning.","thinkingSignature":"reasoning_content"},
				{"type":"text","text":"Previous visible reply."}
			]}
		]`)}},
	}, &recordingWorkspace{})
	if err != nil || result.Output != "Current reply." {
		t.Fatalf("continue after model change: %q, %v", result.Output, err)
	}
}

func TestAgentRestoresPiCompactedContext(t *testing.T) {
	const oldDetail = "Old workspace details. "
	const summary = "Pi summarized the old workspace details."
	const hostInstruction = "Do not pass host instructions into a workspace."
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte(hostInstruction), 0600); err != nil {
		t.Fatal(err)
	}
	var summaries atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"fixture","context_length":50000}]}`)
	})
	mux.HandleFunc("POST /chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages json.RawMessage
			Tools    []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("Pi's model and summary requests must both use the account credential")
		}
		response := "Current answer."
		messages := string(request.Messages)
		if strings.Contains(messages, hostInstruction) {
			t.Error("Pi included host instructions in a model request")
		}
		if len(request.Tools) == 0 {
			summaries.Add(1)
			response = summary
		} else if !strings.Contains(messages, summary) || !strings.Contains(messages, "Keep this recent message.") || strings.Contains(messages, oldDetail) {
			t.Errorf("regular request should use the Pi summary and recent messages without the summarized prefix (message bytes=%d)", len(messages))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", response)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	endpoint, err := NewEndpoint(server.URL, "fixture", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := endpoint.Catalog(ctx, false); err != nil {
		t.Fatal(err)
	}
	history := []run.Exchange{
		{Prompt: "Inspect the workspace.", Output: strings.Repeat(oldDetail, 5000)},
		{Prompt: "Keep the test context.", Output: "Keep this recent message. " + strings.Repeat("recent ", 12000)},
	}
	result, err := New(endpoint, "fixture", nil).Run(ctx, run.Input{Prompt: "Continue.", History: history}, &recordingWorkspace{})
	if err != nil || result.Output != "Current answer." || summaries.Load() == 0 || len(result.CompactedContext) == 0 {
		t.Fatalf("Pi did not compact the original context: output=%q, summaries=%d, error=%v", result.Output, summaries.Load(), err)
	}
	summaryCount := summaries.Load()
	history = append(history, run.Exchange{Prompt: "Continue.", Output: result.Output, AgentMessages: result.AgentMessages, CompactedContext: result.CompactedContext})
	resumed, err := New(endpoint, "fixture", nil).Run(ctx, run.Input{Prompt: "Follow up.", History: history}, &recordingWorkspace{})
	if err != nil || resumed.Output != "Current answer." || summaries.Load() != summaryCount {
		t.Fatalf("restoring the saved Pi context summarized the same messages again or failed: output=%q, summaries=%d, error=%v", resumed.Output, summaries.Load(), err)
	}
}
