package sessiontransfer

import (
	"slices"
	"testing"

	"github.com/OXeu/shinona/internal/run"
)

func TestLiveProgressRetainsOnlyMutablePartsAfterFinalization(t *testing.T) {
	const count = 4096
	history := slices.Repeat([]run.TurnPart{{Kind: "thinking", Text: "Completed thought."}}, count)
	current := run.TurnPart{Kind: "text", Text: "First draft."}
	turn := run.Run{ID: "turn", Status: run.Running, Parts: append(history, current)}
	state := liveConnection{}
	snapshot := state.sync(&run.TranscriptUpdate{ChangedTurns: []run.Run{turn}, FinalizedParts: count}, run.Settings{})
	if parts := snapshot.GetChangedTurns()[0].GetParts(); len(parts) != count+1 || parts[count].Text != current.Text {
		t.Fatalf("reconnect received %d parts without the complete history", len(parts))
	}
	if cap(state.progress.Parts) > 2 {
		t.Fatalf("reconnect retained room for %d parts with one editable part", cap(state.progress.Parts))
	}
	// Handle a watcher notification queued before the sync.
	if delta := state.advanceProgress(run.LiveProgress{TurnID: turn.ID, Parts: turn.Parts, FinalizedParts: count}, 0); delta != nil {
		t.Fatalf("a queued notification replayed the sync: %v", delta)
	}
	next := run.TurnPart{Kind: "text", Text: "Next step."}
	finished := current
	finished.Text += " Ready."
	delta := state.advanceProgress(run.LiveProgress{
		TurnID: turn.ID, Parts: []run.TurnPart{finished, next}, FinalizedParts: count + 1,
	}, count)
	parts := delta.GetParts()
	if delta.GetPartCount() != count+2 || len(parts) != 2 ||
		parts[0].Index != count || parts[0].GetAppend().GetText() != " Ready." ||
		parts[1].Index != count+1 || parts[1].GetReplace().GetText() != next.Text {
		t.Fatalf("final edits and the next part were not delivered together: %v", delta)
	}
	if cap(state.progress.Parts) > 2 || !slices.Equal(state.progress.Parts, []run.TurnPart{next}) {
		t.Fatalf("the connection retained previously finalized parts: %+v", state.progress)
	}
	delta = state.advanceProgress(run.LiveProgress{TurnID: turn.ID, FinalizedParts: count + 2}, count+2)
	if delta != nil || cap(state.progress.Parts) != 0 {
		t.Fatalf("finalizing the last part left data or resent it: cap %d, delta %v", cap(state.progress.Parts), delta)
	}
}

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
	delta := progressDelta(previous, next, 0)
	if delta == nil || delta.PartCount != 1 || len(delta.Parts) != 1 || delta.Steering == nil {
		t.Fatalf("missing replace, trim or steering update: %v", delta)
	}
	edit := delta.Parts[0]
	if edit.GetIndex() != 0 || edit.GetReplace().GetStatus() != string(run.Succeeded) || edit.GetReplace().GetOutput() != "begin and end" {
		t.Fatalf("tool status change did not replace the part: %v", edit)
	}
	if progressDelta(next, next, 0) != nil {
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
	delta := progressDelta(previous, current, 0)
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
	delta := progressDelta(previous, current, 0)
	if delta == nil || len(delta.Parts) != 0 || delta.Context.GetTokens() != 130 || !delta.Context.GetEstimated() {
		t.Fatalf("expected a context-only progress update: %v", delta)
	}
	current.Context = run.ContextUsage{}
	clear := progressDelta(previous, current, 0)
	if clear == nil || clear.Context == nil || clear.Context.Tokens != 0 {
		t.Fatalf("expected a context reset: %v", clear)
	}
}
