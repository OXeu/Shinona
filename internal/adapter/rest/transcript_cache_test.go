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
	var turn sessionpb.Turn
	if err := proto.Unmarshal(wire, &turn); err != nil {
		t.Fatal(err)
	}
	return &turn
}

func TestCompletedTranscriptTurnsReuseProtobuf(t *testing.T) {
	ctx := t.Context()
	var cache encodedTranscriptTurns
	item := run.Run{
		OwnerID: "alice", SessionID: "session", ID: "turn", Status: run.Succeeded,
		UpdatedAt: time.Now().UTC(),
	}
	item.Parts = []run.TurnPart{{Kind: "text", Text: "<script>&"}}
	item.TokenUsage = &run.TokenUsage{Input: 3}
	cold, err := cache.encode(ctx, item)
	got := decodeTranscriptTurn(t, cold, err)
	if len(got.Parts) != 1 || got.Parts[0].Kind != "text" || got.Parts[0].Text != "<script>&" || got.GetTokenUsage().GetInput() != 3 {
		t.Fatalf("completed turn fields changed: %v", got)
	}
	hot, err := cache.encode(ctx, item)
	if err != nil || len(hot) == 0 || &hot[0] != &cold[0] {
		t.Fatalf("completed turn did not reuse its encoded bytes: %s, %v", hot, err)
	}

	item.OwnerID = "bob"
	item.TokenUsage = &run.TokenUsage{Input: 7}
	other, err := cache.encode(ctx, item)
	if usage := decodeTranscriptTurn(t, other, err).GetTokenUsage(); usage.GetInput() != 7 {
		t.Fatalf("cache crossed owners: %v", usage)
	}
	item.OwnerID, item.Status = "alice", run.Running
	item.Parts = []run.TurnPart{{Kind: "text", Text: "in progress"}}
	live, err := cache.encode(ctx, item)
	if turn := decodeTranscriptTurn(t, live, err); len(turn.Parts) != 1 || turn.Parts[0].Text != "in progress" {
		t.Fatalf("running turn did not use its live parts: %v", turn)
	}
	item.Parts[0].Text = "new progress"
	live, err = cache.encode(ctx, item)
	if turn := decodeTranscriptTurn(t, live, err); len(turn.Parts) != 1 || turn.Parts[0].Text != "new progress" {
		t.Fatalf("running turn reused its old parts: %v", turn)
	}
}
