package pigo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guanshan/pi-go/packages/ai"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestAgentContinuesAfterToolTimeout(t *testing.T) {
	ai.SetFauxResponses([]ai.FauxResponse{
		{Content: []ai.ContentBlock{ai.FauxToolCall("tool-1", "bash", map[string]string{"command": "inspect"})}},
		ai.NewFauxText("I can answer from the partial output."),
	})
	t.Cleanup(ai.ResetFauxResponses)
	model, ok := ai.Find(ai.BuiltinModels(), "faux", "faux")
	if !ok {
		t.Fatal("faux model missing")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	workspace := &timedWorkspace{}
	result, err := (&Agent{model: model, apiKey: "test"}).Run(ctx, run.Input{Prompt: "Inspect the workspace."}, workspace)
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
	ai.SetFauxResponses([]ai.FauxResponse{
		{Usage: ai.Usage{Input: 1_000, CacheRead: 1_000_000, CacheWrite: 100, Output: 210}, Content: []ai.ContentBlock{
			ai.FauxThinking("Inspect the workspace."),
			{Type: "thinking", Thinking: "opaque provider data", Redacted: true},
			ai.FauxToolCall("tool-1", "write", map[string]string{"path": "example.txt", "content": "hello"}),
			ai.FauxToolCall("tool-2", "read", map[string]string{"path": "example.txt"}),
			ai.FauxToolCall("tool-3", "bash", map[string]string{"command": "cat example.txt"}),
		}},
		{Usage: ai.Usage{Input: 2_000, CacheRead: 1_122_000, Output: 30_000}, Content: []ai.ContentBlock{ai.FauxText("Task complete.")}},
	})
	t.Cleanup(ai.ResetFauxResponses)
	model, ok := ai.Find(ai.BuiltinModels(), "faux", "faux")
	if !ok {
		t.Fatal("faux model missing from pi-go")
	}

	agent := &Agent{model: model, apiKey: "test"}
	workspace := &recordingWorkspace{}
	var liveTool bool
	output, err := agent.Run(t.Context(), run.Input{
		Prompt: "Create and read example.txt.",
		ReportParts: func(parts []run.TurnPart) {
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
	message := ai.AssistantMessage{Content: []ai.ContentBlock{ai.FauxThinking(text)}}
	parts := &turnParts{}
	current := parts.messageParts(message, false)
	if len(current) != 1 || current[0].Text != text[:partBytes]+"…" ||
		current[0].EstimatedTokens != int32(len(text)/4) || current[0].Status != string(run.Running) {
		t.Fatal("expected an estimate for the full thinking text and a clipped preview")
	}
	completed := parts.messageParts(message, true)
	if completed[0].EstimatedTokens != 0 || completed[0].Status != "" {
		t.Fatal("completed thinking should not carry live progress")
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
	if command.Program != "/bin/bash" || len(command.Args) != 2 || command.Args[0] != "-c" || command.Args[1] != "cat example.txt" {
		return sandbox.Result{}, errors.New("unexpected command")
	}
	w.executed = true
	return sandbox.Result{Stdout: w.text}, nil
}

func (*recordingWorkspace) Close(context.Context) error { return nil }
