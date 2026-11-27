package pigo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestWorkerRecordsPiTurnInterruptedDuringShutdown(t *testing.T) {
	ctx, timeout := context.WithTimeout(t.Context(), 5*time.Second)
	defer timeout()
	lifetime, shutdown := context.WithCancel(ctx)
	defer shutdown()
	started := make(chan struct{}, 1)
	client := &http.Client{Transport: completionTransport(func(request *http.Request) (*http.Response, error) {
		defer request.Body.Close()
		started <- struct{}{}
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	endpoint, err := NewEndpoint("https://models.example.test/v1", "fixture", client)
	if err != nil {
		t.Fatal(err)
	}
	repo := &interruptedTurnRepository{completed: make(chan run.Completion, 1)}
	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{
			Provider: interruptedTurnProvider{},
			ResolveAgent: func(context.Context, string) (run.Agent, error) {
				return New(endpoint, "fixture", nil), nil
			},
		}, nil
	}
	worker := run.NewWorker(repo, repo, resolve, slog.New(slog.DiscardHandler), &run.Controls{}, 1, run.DefaultWorkspaceTimeouts())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Serve(lifetime)
	}()
	select {
	case <-started:
		shutdown()
	case <-ctx.Done():
		t.Fatal("Pi never started the model request")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("worker did not stop after canceling Pi")
	}
	select {
	case completion := <-repo.completed:
		if completion.Status != run.Failed || len(completion.Parts) != 1 || completion.Parts[0].Text != run.FailureInterrupted {
			t.Fatalf("Pi turn completed with %+v", completion)
		}
	default:
		t.Fatal("worker did not persist the interrupted turn")
	}
}

type interruptedTurnRepository struct {
	run.WorkerRepository
	run.ProjectSandboxRepository
	claimed   bool
	completed chan run.Completion
}

func (r *interruptedTurnRepository) ClaimNext(context.Context, time.Time) (*run.Run, error) {
	if r.claimed {
		return nil, nil
	}
	r.claimed = true
	return &run.Run{ID: "turn", OwnerID: "owner", SessionID: "session", Policy: sandbox.Shared, Prompt: "Start."}, nil
}

func (*interruptedTurnRepository) SessionProject(context.Context, string, string) (string, error) {
	return "", nil
}

func (*interruptedTurnRepository) ProjectSandboxConfig(context.Context, string, string) (*run.ProjectSandboxConfig, error) {
	return nil, nil
}

func (*interruptedTurnRepository) History(context.Context, run.Run, int) ([]run.Exchange, error) {
	return nil, nil
}

func (r *interruptedTurnRepository) Complete(_ context.Context, _ string, completion run.Completion) error {
	r.completed <- completion
	return nil
}

type interruptedTurnProvider struct{}

func (interruptedTurnProvider) Policies(string) []sandbox.Policy {
	return []sandbox.Policy{sandbox.Shared}
}

func (interruptedTurnProvider) Acquire(context.Context, sandbox.Spec) (sandbox.Workspace, error) {
	return &recordingWorkspace{}, nil
}

func TestAgentContinuesAfterToolTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := scriptedAgent(t,
			ai.AssistantMessage{StopReason: ai.StopToolUse, Content: ai.ContentList{
				ai.ToolCall{ID: "tool-1", Name: "bash", Arguments: map[string]any{"command": "inspect", "timeout": float64(1)}},
			}},
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "I can answer from the partial output."}}},
		)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		workspace := &timedWorkspace{}
		started := time.Now()
		result, err := fixture.Run(ctx, run.Input{Prompt: "Inspect the workspace."}, workspace)
		if err != nil || result.Output != "I can answer from the partial output." {
			t.Fatalf("expected a final answer after the tool timeout: %q, %v", result.Output, err)
		}
		if elapsed := time.Since(started); elapsed != time.Second || ctx.Err() != nil {
			t.Fatalf("tool timeout ended the turn after %v, caller context: %v", elapsed, ctx.Err())
		}
		if len(result.Parts) != 1 || result.Parts[0].Status != string(run.Failed) {
			t.Fatalf("expected a failed tool part: %+v", result.Parts)
		}
		output := result.Parts[0].Output
		for _, detail := range []string{"context deadline exceeded", "partial stdout", "partial stderr"} {
			if !strings.Contains(output, detail) {
				t.Fatalf("tool output omitted %q: %q", detail, output)
			}
		}
	})
}

type timedWorkspace struct{ sandbox.Workspace }

func (*timedWorkspace) Exec(ctx context.Context, _ sandbox.Command) (sandbox.Result, error) {
	<-ctx.Done()
	return sandbox.Result{Stdout: "partial stdout", Stderr: "partial stderr"}, ctx.Err()
}

func TestAgentToolsUseProvidedWorkspace(t *testing.T) {
	agent := scriptedAgent(t,
		ai.AssistantMessage{StopReason: ai.StopToolUse, Usage: ai.Usage{Input: 1_000, CacheRead: 1_000_000, Output: 210}, Content: ai.ContentList{
			ai.ThinkingContent{Thinking: "Inspect the workspace."},
			ai.ThinkingContent{Thinking: "opaque provider data", Redacted: true},
			ai.ToolCall{ID: "tool-1", Name: "write", Arguments: map[string]any{"path": "example.txt", "content": "hello"}},
			ai.ToolCall{ID: "tool-2", Name: "read", Arguments: map[string]any{"path": "example.txt"}},
			ai.ToolCall{ID: "tool-3", Name: "bash", Arguments: map[string]any{"command": "cat example.txt"}},
		}},
		ai.AssistantMessage{Usage: ai.Usage{Input: 2_000, CacheWrite: 100, CacheRead: 1_122_000, Output: 30_000}, Content: ai.ContentList{ai.TextContent{Text: "Task complete."}}},
	)
	workspace := &recordingWorkspace{}
	var liveTool bool
	output, err := agent.Run(t.Context(), run.Input{
		Prompt: "Create and read example.txt.",
		ReportParts: func(_ int, parts []run.TurnPart) {
			for _, part := range parts {
				if part.Tool == "read" && part.Status == string(run.Running) {
					liveTool = true
				}
			}
		},
	}, workspace)
	if err != nil || output.Output != "Task complete." {
		t.Fatalf("run: %q, %v", output.Output, err)
	}
	if want := (run.TokenUsage{Input: 3_100, Cached: 2_122_000, Output: 30_210}); output.TokenUsage == nil || *output.TokenUsage != want {
		t.Fatalf("expected total usage across the tool call and final answer: got %v, want %v", output.TokenUsage, want)
	}
	if workspace.path != "example.txt" || workspace.text != "hello" || !workspace.read || !workspace.executed {
		t.Fatalf("unexpected workspace calls: %+v", workspace)
	}
	parts := output.Parts
	if !liveTool || len(parts) != 4 || parts[0].Text != "Inspect the workspace." || parts[0].Kind != "thinking" {
		t.Fatalf("expected live tool update and ordered visible content: %+v", parts)
	}
	for i, name := range []string{"write", "read", "bash"} {
		part := parts[i+1]
		if part.Kind != "tool" || part.Tool != name || part.Input == "" || part.Status != string(run.Succeeded) || part.Output == "" {
			t.Fatalf("tool part %q: %+v", name, part)
		}
	}
}

func TestThinkingEstimateUsesFullStreamedText(t *testing.T) {
	text := strings.Repeat("test", partBytes/4+20)
	block := ai.ThinkingContent{Thinking: text}
	current, ok := textPart(block, true)
	if !ok || current.Text != text[:partBytes]+"…" ||
		current.EstimatedTokens != int32(len(text)/4) || current.Status != string(run.Running) {
		t.Fatal("expected an estimate for the full thinking text and a clipped preview")
	}
	completed, ok := textPart(block, false)
	if !ok || completed.EstimatedTokens != 0 || completed.Status != "" {
		t.Fatal("completed thinking should not carry live progress")
	}
}

func TestPiValidatesWorkspaceToolArguments(t *testing.T) {
	fixture := scriptedAgent(t,
		ai.AssistantMessage{StopReason: ai.StopToolUse, Content: ai.ContentList{ai.ToolCall{ID: "bad-write", Name: "write", Arguments: map[string]any{"path": "example.txt"}}}},
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "The write was rejected."}}},
	)
	workspace := &recordingWorkspace{}
	result, err := fixture.Run(t.Context(), run.Input{Prompt: "Write a file."}, workspace)
	if err != nil || workspace.path != "" || len(result.Parts) != 1 || result.Parts[0].Status != string(run.Failed) || !strings.Contains(result.Parts[0].Output, "content") {
		t.Fatalf("expected Pi to reject the write before touching the workspace: path=%q, parts=%+v, error=%v", workspace.path, result.Parts, err)
	}
}

func TestLongAgentLoopKeepsReportingNewToolActivity(t *testing.T) {
	const calls = 60
	var responses []ai.AssistantMessage
	for index := range calls {
		responses = append(responses, ai.AssistantMessage{
			StopReason: ai.StopToolUse, Content: ai.ContentList{
				ai.ThinkingContent{Thinking: fmt.Sprintf("Inspect step %d.", index)},
				ai.ToolCall{ID: "read-file", Name: "read", Arguments: map[string]any{"path": fmt.Sprintf("step-%d", index)}},
			},
		})
	}
	responses = append(responses, ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "Final answer."}}})
	fixture := scriptedAgent(t, responses...)
	lastInput := fmt.Sprintf(`{"path":"step-%d"}`, calls-1)
	var liveTools, endedTools, publishedParts, finalizedParts int
	var progress []run.TurnPart
	result, err := fixture.Run(t.Context(), run.Input{
		Prompt: "Run the inspection.",
		ReportParts: func(offset int, suffix []run.TurnPart) {
			if offset < finalizedParts {
				t.Fatalf("Pi changed previously finalized parts: offset %d, finalized %d", offset, finalizedParts)
			}
			publishedParts += len(suffix)
			progress = append(progress[:offset], suffix...)
			parts := progress
			if len(parts) == 0 || parts[len(parts)-1].Kind != "tool" {
				return
			}
			last := parts[len(parts)-1]
			switch last.Status {
			case string(run.Running):
				want := fmt.Sprintf(`{"path":"step-%d"}`, liveTools)
				if len(parts) != 2*(liveTools+1) || last.Input != want || endedTools != liveTools {
					t.Errorf("step %d did not preserve its live history: %d parts, %d finished tools, current tool %+v", liveTools, len(parts), endedTools, last)
					return
				}
				for index := 0; index <= liveTools; index++ {
					thought, tool := parts[2*index], parts[2*index+1]
					if thought.Kind != "thinking" || thought.Text != fmt.Sprintf("Inspect step %d.", index) || thought.Status != "" {
						t.Fatalf("step %d lost a completed thought: %+v", index, thought)
					}
					if index < liveTools && (tool.Status != string(run.Succeeded) || tool.Input != fmt.Sprintf(`{"path":"step-%d"}`, index)) {
						t.Fatalf("step %d lost its completed tool: %+v", index, tool)
					}
				}
				liveTools++
			case string(run.Succeeded):
				endedTools++
			}
		},
		FinalizeParts: func(count int) {
			if count != len(progress) || count <= finalizedParts {
				t.Fatalf("Pi finalized %d parts after %d of %d", count, finalizedParts, len(progress))
			}
			finalizedParts = count
		},
	}, &recordingWorkspace{path: fmt.Sprintf("step-%d", calls-1), text: "hello"})
	if err != nil || result.Output != "Final answer." || liveTools != calls || endedTools != calls || len(result.Parts) != 2*calls {
		t.Fatalf("long turn did not preserve activity: output=%q, started=%d, ended=%d, saved=%d, error=%v", result.Output, liveTools, endedTools, len(result.Parts), err)
	}
	if publishedParts > 5*calls+1 {
		t.Fatalf("published %d parts for %d tools, expected only the changed suffixes", publishedParts, calls)
	}
	if finalizedParts != 2*calls+1 {
		t.Fatalf("expected finished tools, thoughts and final reply to be finalized, got %d parts", finalizedParts)
	}
	t.Logf("published %d parts for %d tools", publishedParts, calls)
	last := result.Parts[len(result.Parts)-1]
	if last.Input != lastInput || last.Status != string(run.Succeeded) || last.Output != "hello" {
		t.Fatalf("latest completed tool is missing: %+v", last)
	}
}

func TestLoadsStoredConversationHistory(t *testing.T) {
	const data = `[
		{"role":"assistant","api":"openai-completions","provider":"openai","model":"before","stopReason":"toolUse",
		 "content":[{"type":"thinking","thinking":"Read the file.","thinkingSignature":"reasoning_content"},
		            {"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"test.txt"}}]},
		{"role":"toolResult","toolCallId":"call_1","toolName":"read","content":[{"type":"text","text":"file contents"}]},
		{"role":"assistant","api":"openai-completions","provider":"openai","model":"before","stopReason":"stop",
		 "content":[{"type":"text","text":"I read the file."}]}
	]`
	history, err := conversationHistory([]run.Exchange{{Prompt: "Inspect.", AgentMessages: []byte(data)}},
		ai.Model{Api: ai.APIOpenAICompletions, Provider: "openai", ID: "before"})
	if err != nil || len(history.messages) != 4 {
		t.Fatalf("replay stored history: %v, %v", history, err)
	}
	response := assistantMessage(history.messages[1])
	thinking, ok := response.Content[0].(ai.ThinkingContent)
	if !ok || thinking.ThinkingSignature != "reasoning_content" {
		t.Fatalf("lost thinking signature: %+v", response.Content)
	}
	call, ok := response.Content[1].(ai.ToolCall)
	tool, isResult := history.messages[2].(ai.ToolResultMessage)
	if !ok || !isResult || call.Arguments["path"] != "test.txt" || call.ID != tool.ToolCallID || ai.ContentText(tool.Content) != "file contents" {
		t.Fatalf("lost tool call or matching result: %+v, %+v", call, tool)
	}
}

func TestAgentPersistsNewCompactionWithinTheSameMillisecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const firstSummary = "First snapshot."
		const latestSummary = "Snapshot after more work."
		const finalAnswer = "Inspection complete."
		fixture := scriptedAgent(t,
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: firstSummary}}},
			ai.AssistantMessage{
				StopReason: ai.StopToolUse,
				Usage:      ai.Usage{Input: 41_000, Output: 21_000},
				Content: ai.ContentList{
					ai.TextContent{Text: strings.Repeat("More evidence. ", 6_000)},
					ai.ToolCall{ID: "read-file", Name: "read", Arguments: map[string]any{"path": "answer.txt"}},
				},
			},
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: latestSummary}}},
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "The current request still needs the answer."}}},
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: finalAnswer}}},
		)
		fixture.model.ContextWindow = 50_000
		started := time.Now()
		result, err := fixture.Run(t.Context(), run.Input{
			Prompt: "Read answer.txt.",
			History: []run.Exchange{
				{Prompt: strings.Repeat("Past requirements. ", 8_000), Output: "Previous answer."},
				{Prompt: strings.Repeat("Recent instructions. ", 4_000), Output: "Ready."},
			},
		}, &recordingWorkspace{path: "answer.txt", text: "The answer."})
		if err != nil || result.Output != finalAnswer || time.Since(started) != 0 {
			t.Fatalf("Pi did not finish two compactions at the same instant: %q, %v, elapsed %v", result.Output, err, time.Since(started))
		}
		messages, boundary, err := decodeCompactedContext(result.CompactedContext)
		if err != nil || boundary == 0 || boundary >= len(messages) {
			t.Fatalf("missing compaction snapshot or model response: %d / %d, %v", boundary, len(messages), err)
		}
		summary, ok := savedPiSummary(messages[0])
		if !ok || !strings.Contains(summary, latestSummary) || strings.Contains(summary, firstSummary) {
			t.Fatalf("saved the wrong Pi compaction: %q", summary)
		}
		if reply := assistantMessage(messages[boundary]); reply == nil || ai.ContentText(reply.Content) != finalAnswer {
			t.Fatalf("the final reply did not stay after the latest boundary: %+v", reply)
		}
	})
}

type recordingWorkspace struct {
	path, text     string
	read, executed bool
}

func (w *recordingWorkspace) ReadFile(_ context.Context, path string) ([]byte, error) {
	w.read = path == w.path && path != ""
	return []byte(w.text), nil
}

func (w *recordingWorkspace) WriteFile(_ context.Context, path string, data []byte) error {
	w.path, w.text = path, string(data)
	return nil
}

func (w *recordingWorkspace) Exec(_ context.Context, command sandbox.Command) (sandbox.Result, error) {
	expected := sandbox.Shell("-c", "cat example.txt")
	if command.Program != expected.Program || !slices.Equal(command.Args, expected.Args) {
		return sandbox.Result{}, errors.New("unexpected command")
	}
	w.executed = true
	return sandbox.Result{Stdout: w.text}, nil
}

func (*recordingWorkspace) Close(context.Context) error { return nil }

func scriptedAgent(t *testing.T, responses ...ai.AssistantMessage) *Agent {
	t.Helper()
	model := ai.Model{ID: "test-model", Api: t.Name(), Provider: "fixture"}
	ai.RegisterApiProvider(ai.ApiProvider{Api: model.Api, StreamSimple: func(_ context.Context, model *ai.Model, _ ai.TranscriptContext,
		_ *ai.SimpleStreamOptions,
	) *ai.AssistantMessageEventStream {
		stream := ai.NewAssistantMessageEventStream()
		if len(responses) == 0 {
			message := ai.NewErrorMessage(model.Api, model.Provider, model.ID, ai.StopError, "unexpected model request")
			stream.Push(ai.AssistantMessageEvent{Type: ai.EventError, Reason: ai.StopError, Error: message})
			return stream
		}
		message := responses[0]
		responses = responses[1:]
		message.Api, message.Provider, message.Model = model.Api, model.Provider, model.ID
		if message.StopReason == "" {
			message.StopReason = ai.StopStop
		}
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: &ai.AssistantMessage{}})
		stream.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: message.StopReason, Message: &message})
		return stream
	}}, t.Name())
	t.Cleanup(func() { ai.UnregisterApiProviders(t.Name()) })
	return &Agent{model: model}
}
