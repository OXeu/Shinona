package pigo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
)

func TestJSONCompletionContinuesToolsThroughPi(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Stream   bool            `json:"stream"`
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !input.Stream {
			t.Errorf("expected Pi's streaming request: %v", err)
		}
		switch requests.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = fmt.Fprint(w, `{
				"id":"chatcmpl-tool","model":"served-model",
				"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"I should read the file.",
					"tool_calls":[{"id":"call_read","type":"function","function":{"name":"read","arguments":"{\"path\":\"answer.txt\"}"}}]}
					,"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":31,"completion_tokens":9,"total_tokens":40,
					"prompt_tokens_details":{"cached_tokens":18,"cache_write_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}
			}`)
		case 2:
			for _, want := range []string{`"tool_call_id":"call_read"`, `"role":"tool"`, "Text from the sandbox."} {
				if !strings.Contains(string(input.Messages), want) {
					t.Errorf("Pi tool continuation omitted %q", want)
				}
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Read complete.\"},\"finish_reason\":\"stop\"}]}\n\n")
		default:
			t.Error("the adapter sent a duplicate model request")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	endpoint, err := NewEndpoint(server.URL, "model-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	workspace := &recordingWorkspace{path: "answer.txt", text: "Text from the sandbox."}
	var usage []run.ContextUsage
	result, err := New(endpoint, "fixture", nil).Run(t.Context(), run.Input{
		Prompt: "Read answer.txt.",
		ReportContext: func(update run.ContextUsage) {
			usage = append(usage, update)
		},
	}, workspace)
	if err != nil || result.Output != "Read complete." || !workspace.read || requests.Load() != 2 {
		t.Fatalf("Pi did not continue the JSON tool call: %q, %v", result.Output, err)
	}
	if len(result.Parts) != 2 || result.Parts[0].Text != "I should read the file." ||
		result.Parts[1].Tool != "read" || result.Parts[1].Status != string(run.Succeeded) {
		t.Fatalf("JSON completion lost its transcript parts: %+v", result.Parts)
	}
	if result.TokenUsage == nil || *result.TokenUsage != (run.TokenUsage{Input: 13, Cached: 18, Output: 9}) ||
		result.ContextTokens != 40 || !slices.Equal(usage, []run.ContextUsage{{Tokens: 40, Window: int32(result.ContextWindow)}}) {
		t.Fatalf("JSON completion lost its token usage: %+v, %v", result.TokenUsage, usage)
	}
	for _, want := range []string{`"responseId":"chatcmpl-tool"`, `"responseModel":"served-model"`,
		`"thinkingSignature":"reasoning_content"`, `"reasoning":2`} {
		if !strings.Contains(string(result.AgentMessages), want) {
			t.Errorf("Pi did not persist JSON completion metadata %q", want)
		}
	}
}

func TestJSONCompletionRejectsTrailingOrOversizedResponses(t *testing.T) {
	const before = `{"choices":[{"message":{"content":"`
	const after = `"},"finish_reason":"stop"}]}`
	for _, test := range []struct {
		name, body, expected string
	}{
		{"trailing value", before + "answer" + after + " null", "more than one JSON value"},
		{"truncated value", before + "partial", "unexpected EOF"},
		{"too large", before + strings.Repeat("x", maxJSONCompletionBytes) + after, "response size limit"},
		{"null tool call", `{"choices":[{"message":{"tool_calls":[null]},"finish_reason":"tool_calls"}]}`, "null tool call"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if completion, err := decodeJSONCompletion(strings.NewReader(test.body), &ai.Model{}); completion != nil ||
				err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("invalid JSON completion was accepted: %v", err)
			}
		})
	}
}

func TestJSONCompletionKeepsValidReasoningAndProviderBilling(t *testing.T) {
	const detail = `{"type":"reasoning.encrypted","id":"proof","data":"opaque"}`
	body := `{"choices":[{"message":{"content":"Answer","reasoning":{"trace":[]},"reasoning_text":"Thinking",
		"reasoning_details":[null,{"type":"reasoning.summary","summary":false},` + detail + `]},"finish_reason":"stop",
		"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.125,"is_byok":true,
			"cost_details":{"upstream_inference_cost":null,"upstream_inference_prompt_cost":0.25,"upstream_inference_completions_cost":0.125}}}]}`
	reply, err := decodeJSONCompletion(strings.NewReader(body), &ai.Model{BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil || len(reply.Content) != 2 {
		t.Fatalf("optional provider metadata prevented completion: %+v, %v", reply, err)
	}
	thinking, ok := reply.Content[0].(ai.ThinkingContent)
	if !ok || thinking.Thinking != "Thinking" || thinking.ThinkingSignature != "["+detail+"]" {
		t.Fatalf("completion lost its valid reasoning signature: %+v", reply.Content[0])
	}
	if cost := reply.Usage.Cost; cost.Total != 0.5 || cost.TotalFromProvider != ai.OptBoolTrue {
		t.Fatalf("provider-reported cost was lost: %+v", cost)
	}
}

func TestJSONCompletionStillReportsInactivity(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[`)
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	endpoint, err := NewEndpoint(server.URL, "fixture", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	endpoint.completionIdle = 150 * time.Millisecond
	result, err := New(endpoint, "fixture", nil).Run(ctx, run.Input{Prompt: "Explain."}, &recordingWorkspace{})
	if !errors.Is(err, run.ErrModelIdle) || result.Output != "" {
		t.Fatalf("incomplete JSON should time out cleanly: %q, %v", result.Output, err)
	}
}
