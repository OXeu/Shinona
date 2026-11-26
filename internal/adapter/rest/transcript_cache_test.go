package rest

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
)

func decodeTranscriptTurn(t *testing.T, wire []byte, err error) *sessionpb.Turn {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var page sessionpb.TranscriptPage
	if err := proto.Unmarshal(wire, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 {
		t.Fatalf("expected one turn, got %v", &page)
	}
	return page.Turns[0]
}

func TestCompletedTranscriptTurnsReuseProtobuf(t *testing.T) {
	ctx := t.Context()
	var cache encodedTranscriptTurns
	item := run.Run{
		OwnerID: "alice", SessionID: "session", ID: "turn", Status: run.Succeeded,
		UpdatedAt: time.Now().UTC(),
	}
	fields := run.StoredTurnJSON{
		Parts: `[{"kind":"text","text":"<script>&"}]`, TokenUsage: `{"input":3,"cached":0,"output":0}`,
	}
	cold, err := cache.encode(ctx, item, fields)
	got := decodeTranscriptTurn(t, cold, err)
	if len(got.Parts) != 1 || got.Parts[0].Kind != "text" || got.Parts[0].Text != "<script>&" || got.GetTokenUsage().GetInput() != 3 {
		t.Fatalf("completed turn fields changed: %v", got)
	}
	hot, err := cache.encode(ctx, item, fields)
	if err != nil || len(hot) == 0 || &hot[0] != &cold[0] {
		t.Fatalf("completed turn did not reuse its encoded bytes: %s, %v", hot, err)
	}

	item.OwnerID = "bob"
	fields.TokenUsage = `{"Input":7}`
	other, err := cache.encode(ctx, item, fields)
	if usage := decodeTranscriptTurn(t, other, err).GetTokenUsage(); usage.GetInput() != 7 {
		t.Fatalf("cache crossed owners or failed to normalize legacy usage: %v", usage)
	}
	item.OwnerID, item.Status = "alice", run.Running
	item.Parts = []run.TurnPart{{Kind: "text", Text: "in progress"}}
	live, err := cache.encode(ctx, item, run.StoredTurnJSON{})
	if turn := decodeTranscriptTurn(t, live, err); len(turn.Parts) != 1 || turn.Parts[0].Text != "in progress" {
		t.Fatalf("running turn did not use its live parts: %v", turn)
	}
	item.Parts[0].Text = "new progress"
	live, err = cache.encode(ctx, item, run.StoredTurnJSON{})
	if turn := decodeTranscriptTurn(t, live, err); len(turn.Parts) != 1 || turn.Parts[0].Text != "new progress" {
		t.Fatalf("running turn reused its old parts: %v", turn)
	}
}

func TestStoredTranscriptPartsAreAnArray(t *testing.T) {
	for _, stored := range []string{"", "null", "[]"} {
		body, err := encodeStoredTurn(run.Run{}, run.StoredTurnJSON{Parts: stored})
		if turn := decodeTranscriptTurn(t, body, err); len(turn.Parts) != 0 {
			t.Fatalf("parts %q: got %v", stored, turn.Parts)
		}
	}
	for _, stored := range []string{`{}`, `[`, `[] {}`} {
		if _, err := encodeStoredTurn(run.Run{}, run.StoredTurnJSON{Parts: stored}); err == nil {
			t.Errorf("accepted invalid parts %q", stored)
		}
	}
}
