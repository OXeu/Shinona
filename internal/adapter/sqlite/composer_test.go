package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/pigo"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type controlledAgent struct {
	inputs chan run.Input
	drain  chan struct{}
}

func TestWorkerSendsConversationHistoryToModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	type modelMessage struct {
		Role             string
		Content          json.RawMessage
		ReasoningContent string `json:"reasoning_content"`
		ToolCallID       string `json:"tool_call_id"`
		ToolCalls        []struct {
			ID       string
			Function struct{ Name, Arguments string }
		} `json:"tool_calls"`
	}
	requests := make(chan []modelMessage, 8)
	const modelKey = "fake-model-api-key"
	var rejectedOnce atomic.Bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Messages []modelMessage
		}
		if r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) == 0 {
			t.Error("unexpected model request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- body.Messages
		last := body.Messages[len(body.Messages)-1]
		rejected := strings.Contains(string(last.Content), "fixture rejected")
		if rejected && !rejectedOnce.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Rejected turn using `+modelKey+`","Authorization":"Bearer fake-response-token"}}`)
			return
		}
		if rejected && body.Model != "fixture-alternative" {
			t.Errorf("retry requested model %q", body.Model)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if last.Role == "user" && strings.Contains(string(last.Content), "fixture initial prompt") {
			_, _ = io.WriteString(w, `data: {"id":"tool","object":"chat.completion.chunk","model":"fixture","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"fixture reasoning","content":"I'll read the fixture.","tool_calls":[{"index":0,"id":"call_fixture","type":"function","function":{"name":"read","arguments":"{\"path\":\"fixture.txt\"}"}}]},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"tool","object":"chat.completion.chunk","model":"fixture","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n"+
				"data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"id":"one","object":"chat.completion.chunk","model":"fixture","choices":[{"index":0,"delta":{"role":"assistant","content":"fixture reply"},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"one","object":"chat.completion.chunk","model":"fixture","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	defer model.Close()
	endpoint, err := pigo.NewEndpoint(model.URL+"/v1", modelKey, model.Client())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	agent := pigo.New(endpoint, "fixture", nil)
	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "test", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) { return agent, nil }}, nil
	}
	controls := &run.Controls{}
	service := run.NewService(repository, resolve, uuid.NewString, controls, nil)
	session, err := service.CreateSession(ctx, "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repository, resolve, slog.New(slog.DiscardHandler), controls).Serve(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	type expectedMessage struct{ role, text string }
	first := expectedMessage{"user", "fixture initial prompt"}
	reply := expectedMessage{"assistant", "fixture reply"}
	rejected := expectedMessage{"user", "fixture rejected prompt"}
	followUp := expectedMessage{"user", "fixture follow-up"}
	firstSteps := []expectedMessage{first, {"assistant", "I'll read the fixture."}, {"tool", "sandbox operations are disabled in demo mode"}}
	firstHistory := append(slices.Clone(firstSteps), reply)
	assertRequest := func(expected []expectedMessage, sameModel bool) {
		t.Helper()
		select {
		case messages := <-requests:
			conversation := slices.DeleteFunc(messages, func(message modelMessage) bool { return message.Role == "system" || message.Role == "developer" })
			if len(conversation) != len(expected) {
				t.Fatalf("model request has %d conversation messages, want %d: %+v", len(conversation), len(expected), conversation)
			}
			for i, want := range expected {
				got := conversation[i]
				if got.Role != want.role || !strings.Contains(string(got.Content), want.text) {
					t.Fatalf("wrong model message at index %d: %s %s", i, got.Role, got.Content)
				}
				switch want {
				case firstSteps[1]:
					if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call_fixture" ||
						got.ToolCalls[0].Function.Name != "read" || got.ToolCalls[0].Function.Arguments != `{"path":"fixture.txt"}` {
						t.Fatalf("wrong tool call: %+v", got)
					}
					if sameModel && got.ReasoningContent != "fixture reasoning" {
						t.Fatalf("same-model request lost reasoning content: %+v", got)
					}
					if !sameModel && (got.ReasoningContent != "" || strings.Contains(string(got.Content), "fixture reasoning")) {
						t.Fatalf("model switch replayed reasoning from a different model: %+v", got)
					}
				case firstSteps[2]:
					if got.ToolCallID != "call_fixture" {
						t.Fatalf("wrong tool result ID: %s", got.ToolCallID)
					}
				}
			}
		case <-ctx.Done():
			t.Fatal("missing model request")
		}
	}
	payload := run.Payload{
		Images:  []run.Image{{Name: "attached.jpg", MediaType: "image/jpeg", Data: "/9j/4A=="}},
		Options: run.Options{Model: "fixture", Permission: run.ReadOnly, Thinking: "low"},
	}
	var failedID string
	for _, turn := range []struct {
		prompt   string
		status   run.Status
		expected []expectedMessage
		retry    bool
	}{
		{first.text, run.Succeeded, []expectedMessage{first}, false},
		{rejected.text, run.Failed, append(slices.Clone(firstHistory), rejected), false},
		{rejected.text, run.Succeeded, append(slices.Clone(firstHistory), rejected), true},
		{followUp.text, run.Succeeded, append(slices.Clone(firstHistory), rejected, reply, followUp), false},
	} {
		var item run.Run
		var err error
		switch {
		case turn.retry:
			if _, err := service.RetryTurn(ctx, "other", session.ID, failedID, run.RetryOptions{}); !errors.Is(err, run.ErrSession) {
				t.Fatalf("another account could retry a turn: %v", err)
			}
			selection := run.Options{Model: "fixture-alternative", Permission: run.Workspace, Thinking: "high", MCPServers: []string{}}
			item, err = service.RetryTurn(ctx, "owner", session.ID, failedID, run.RetryOptions{Selection: &selection})
			if err == nil && (item.Prompt != rejected.text || !slices.Equal(item.Payload.Images, payload.Images) || item.Payload.Options.Thinking != selection.Thinking ||
				item.Payload.Options.Permission != selection.Permission || item.Payload.Options.Model != selection.Model) {
				t.Fatal("retry lost the original prompt, attachments, or options")
			}
		case turn.status == run.Failed:
			item, err = service.Submit(ctx, "owner", session.ID, turn.prompt, payload, false)
		default:
			item, err = service.Submit(ctx, "owner", session.ID, turn.prompt, run.Payload{}, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		transcript := awaitTurn(t, ctx, service, session.ID, item.ID, turn.status)
		if turn.status == run.Failed {
			failedID = item.ID
			parts := transcript.Turns[len(transcript.Turns)-1].Parts
			if len(parts) == 0 {
				t.Fatal("model failure details were not saved")
			}
			detail := parts[len(parts)-1]
			if detail.Kind != "failure" || detail.Text != "model" || !strings.Contains(detail.Output, "400") ||
				!strings.Contains(detail.Output, "Rejected turn") || strings.Contains(detail.Output, modelKey) || strings.Contains(detail.Output, "fake-response-token") {
				t.Fatalf("incorrect failure detail: %+v", detail)
			}
		}
		assertRequest(turn.expected, !turn.retry)
		if turn.prompt == first.text {
			assertRequest(firstSteps, true)
		}
	}
	if _, err := service.RetryTurn(ctx, "owner", session.ID, failedID, run.RetryOptions{}); !errors.Is(err, run.ErrRetryTurn) {
		t.Fatalf("retry of an older failure affected the current history: %v", err)
	}
}

func (a controlledAgent) Run(ctx context.Context, input run.Input, _ sandbox.Workspace) (run.Result, error) {
	a.inputs <- input
	if input.Prompt == "second" {
		return run.Result{Output: "completed second"}, nil
	}
	select {
	case <-ctx.Done():
		return run.Result{}, ctx.Err()
	case <-a.drain:
		return run.Result{Output: input.Steering.Drain()[0], ContextTokens: 36, ContextWindow: 1024,
			TokenUsage: &run.TokenUsage{Input: 16, Cached: 20, Output: 3}}, nil
	}
}

func TestWorkerQueueSteeringCancellationAndAttachmentMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	repository, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	}()
	agent := controlledAgent{inputs: make(chan run.Input, 8), drain: make(chan struct{})}
	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) { return agent, nil }}, nil
	}
	controls := &run.Controls{}
	service := run.NewService(repository, resolve, uuid.NewString, controls, nil)
	session, err := service.CreateSession(ctx, "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	images := []run.Image{
		{Name: "shot.png", MediaType: "image/png", Data: "iVBORw0KGgo="},
		{Name: `second "截图".png`, MediaType: "image/png", Data: "iVBORw0KGgo="},
	}
	first, err := service.Submit(ctx, "owner", session.ID, "first", run.Payload{
		Images: images, Options: run.Options{Model: "fixture", ArrowJS: true},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repository, resolve, slog.New(slog.DiscardHandler), controls).Serve(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	awaitInput(t, ctx, agent.inputs, "first")
	second, err := service.Submit(ctx, "owner", session.ID, "second", run.Payload{}, true)
	if err != nil {
		t.Fatal(err)
	}
	third, err := service.Submit(ctx, "owner", session.ID, "third", run.Payload{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Stop(ctx, "other", session.ID, third.ID); !errors.Is(err, run.ErrNotRunning) {
		t.Fatalf("cross-account stop: %v", err)
	}
	if err := service.Stop(ctx, "owner", session.ID, third.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Steer("owner", session.ID, first.ID, "steered instruction"); err != nil {
		t.Fatal(err)
	}
	pending, err := service.Transcript(ctx, "owner", session.ID)
	if err != nil || pending == nil || len(pending.Turns) != 3 || !pending.Session.Active || pending.Turns[0].Steering[0].Delivered {
		t.Fatalf("pending transcript: %+v, %v", pending, err)
	}
	wantImages := slices.Clone(images)
	for i := range wantImages {
		wantImages[i].Data = ""
	}
	payload := pending.Turns[0].Payload
	if !slices.Equal(payload.Images, wantImages) || payload.Options.Model != "fixture" || !payload.Options.ArrowJS {
		t.Fatal("the transcript must preserve attachment order and options without image data")
	}
	attachment, err := service.Attachment(ctx, "owner", session.ID, first.ID, 1)
	if err != nil || attachment == nil || *attachment != images[1] {
		t.Fatalf("stored attachment: %+v, %v", attachment, err)
	}
	close(agent.drain)
	awaitInput(t, ctx, agent.inputs, "second")
	final := awaitTurn(t, ctx, service, session.ID, second.ID, run.Succeeded)
	if len(final.Turns) != 3 || final.Turns[0].Output != "steered instruction" ||
		!final.Turns[0].Steering[0].Delivered || final.Turns[0].ContextTokens != 36 || final.Turns[2].Status != run.Stopped {
		t.Fatalf("unexpected final transcript: %+v", final)
	}
	if usage := final.Turns[0].TokenUsage; usage == nil || *usage != (run.TokenUsage{Input: 16, Cached: 20, Output: 3}) || final.Turns[1].TokenUsage != nil {
		t.Fatalf("expected stored usage only for the measured turn: %+v", final.Turns)
	}
}

func TestWorkerStopsRunningTurnAndRetainsQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	repository, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	}()
	agent := controlledAgent{inputs: make(chan run.Input, 8), drain: make(chan struct{})}
	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: func(context.Context, string) (run.Agent, error) { return agent, nil }}, nil
	}
	controls := &run.Controls{}
	service := run.NewService(repository, resolve, uuid.NewString, controls, nil)
	session, err := service.CreateSession(ctx, "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Submit(ctx, "owner", session.ID, "first", run.Payload{}, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Submit(ctx, "owner", session.ID, "second", run.Payload{}, true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repository, resolve, slog.New(slog.DiscardHandler), controls).Serve(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	awaitInput(t, ctx, agent.inputs, "first")
	if err := service.Stop(ctx, "owner", session.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	awaitInput(t, ctx, agent.inputs, "second")
	final := awaitTurn(t, ctx, service, session.ID, second.ID, run.Succeeded)
	if final.Turns[0].Status != run.Stopped || final.Turns[1].Output != "completed second" {
		t.Fatalf("stop did not preserve the next turn: %+v", final)
	}
}

func awaitInput(t *testing.T, ctx context.Context, inputs <-chan run.Input, prompt string) {
	t.Helper()
	select {
	case input := <-inputs:
		if input.Prompt != prompt {
			t.Fatalf("started %q, expected %q", input.Prompt, prompt)
		}
	case <-ctx.Done():
		t.Fatalf("worker did not start %q", prompt)
	}
}

func awaitTurn(t *testing.T, ctx context.Context, service *run.Service, session, id string, status run.Status) *run.Transcript {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		transcript, err := service.Transcript(ctx, "owner", session)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(transcript.Turns, func(turn run.Run) bool { return turn.ID == id && turn.Status == status }) {
			return transcript
		}
		select {
		case <-ctx.Done():
			t.Fatalf("turn %s did not reach %s", id, status)
		case <-ticker.C:
		}
	}
}
