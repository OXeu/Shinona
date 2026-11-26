package pigo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestAgentContinuesAfterToolTimeout(t *testing.T) {
	fixture := scriptedAgent(t,
		ai.AssistantMessage{StopReason: ai.StopToolUse, Content: ai.ContentList{
			ai.ToolCall{ID: "tool-1", Name: "bash", Arguments: map[string]any{"command": "inspect"}},
		}},
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "I can answer from the partial output."}}},
	)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	workspace := &timedWorkspace{}
	result, err := fixture.Run(ctx, run.Input{Prompt: "Inspect the workspace."}, workspace)
	if err != nil || result.Output != "I can answer from the partial output." {
		t.Fatalf("expected a final answer after the tool timeout: %q, %v", result.Output, err)
	}
	if deadline, _ := ctx.Deadline(); !workspace.deadline.Equal(deadline) {
		t.Fatalf("tool received deadline %v, want caller deadline %v", workspace.deadline, deadline)
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
}

type timedWorkspace struct {
	sandbox.Workspace
	deadline time.Time
}

func (w *timedWorkspace) Exec(ctx context.Context, _ sandbox.Command) (sandbox.Result, error) {
	w.deadline, _ = ctx.Deadline()
	return sandbox.Result{Stdout: "partial stdout", Stderr: "partial stderr"}, context.DeadlineExceeded
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
	var liveTools, endedTools, publishedParts int
	var progress []run.TurnPart
	result, err := fixture.Run(t.Context(), run.Input{
		Prompt: "Run the inspection.",
		ReportParts: func(offset int, suffix []run.TurnPart) {
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
	}, &recordingWorkspace{path: fmt.Sprintf("step-%d", calls-1), text: "hello"})
	if err != nil || result.Output != "Final answer." || liveTools != calls || endedTools != calls || len(result.Parts) != 2*calls {
		t.Fatalf("long turn did not preserve activity: output=%q, started=%d, ended=%d, saved=%d, error=%v", result.Output, liveTools, endedTools, len(result.Parts), err)
	}
	if publishedParts > 5*calls+1 {
		t.Fatalf("published %d parts for %d tools, expected only the changed suffixes", publishedParts, calls)
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
