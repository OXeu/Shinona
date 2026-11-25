package sessiontransfer

import (
	"testing"

	"github.com/OXeu/shinona/internal/run"
)

func TestProgressDeltaReplacesChangedToolMetadataAndTrimsParts(t *testing.T) {
	previous := run.LiveProgress{
		TurnID: "turn",
		Parts: []run.TurnPart{
			{Kind: "tool", Tool: "read", Output: "begin", Status: string(run.Running)},
			{Kind: "text", Text: "provisional text"},
		},
		Steering: []run.SteeringMessage{{Text: "steering", Delivered: true}},
	}
	next := run.LiveProgress{TurnID: "turn", Parts: []run.TurnPart{
		{Kind: "tool", Tool: "read", Output: "begin and end", Status: string(run.Succeeded)},
	}}
	delta := progressDelta(previous, next)
	if delta == nil || delta.PartCount != 1 || len(delta.Parts) != 1 || delta.Steering == nil {
		t.Fatalf("missing replace, trim or steering update: %v", delta)
	}
	edit := delta.Parts[0]
	if edit.GetIndex() != 0 || edit.GetReplace().GetStatus() != string(run.Succeeded) || edit.GetReplace().GetOutput() != "begin and end" {
		t.Fatalf("tool status change did not replace the part: %v", edit)
	}
	if progressDelta(next, next) != nil {
		t.Fatal("identical progress emitted an update")
	}
}

func TestProgressDeltaUpdatesThinkingEstimateWithoutRepeatingText(t *testing.T) {
	previous := run.LiveProgress{TurnID: "turn", Parts: []run.TurnPart{
		{Kind: "thinking", Status: string(run.Running), Text: "clipped…", EstimatedTokens: 2049},
	}}
	current := run.LiveProgress{TurnID: "turn", Parts: []run.TurnPart{
		{Kind: "thinking", Status: string(run.Running), Text: "clipped…", EstimatedTokens: 2100},
	}}
	delta := progressDelta(previous, current)
	if delta == nil || len(delta.Parts) != 1 {
		t.Fatalf("missing thinking progress: %v", delta)
	}
	update := delta.Parts[0].GetAppend()
	if update == nil || update.EstimatedTokens != 2100 || update.Text != "" {
		t.Fatalf("expected only the new estimate: %v", delta)
	}
}

func TestProgressDeltaCarriesContextWithoutTranscriptChanges(t *testing.T) {
	previous := run.LiveProgress{TurnID: "turn", Context: run.ContextUsage{Tokens: 120, Window: 64000, Estimated: true}}
	current := previous
	current.Context.Tokens = 130
	delta := progressDelta(previous, current)
	if delta == nil || len(delta.Parts) != 0 || delta.Context.GetTokens() != 130 || !delta.Context.GetEstimated() {
		t.Fatalf("expected a context-only progress update: %v", delta)
	}
	current.Context = run.ContextUsage{}
	clear := progressDelta(previous, current)
	if clear == nil || clear.Context == nil || clear.Context.Tokens != 0 {
		t.Fatalf("expected a context reset: %v", clear)
	}
}
