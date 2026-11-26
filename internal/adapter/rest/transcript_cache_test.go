package rest

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestCompletedTranscriptTurnsReuseJSONAndKeepItsShape(t *testing.T) {
	ctx := t.Context()
	var cache encodedTranscriptTurns
	item := run.Run{
		OwnerID: "alice", SessionID: "session", ID: "turn", Status: run.Succeeded,
		UpdatedAt:  time.Now().UTC(),
		Parts:      []run.TurnPart{{Kind: "text", Text: "<script>&"}},
		TokenUsage: &run.TokenUsage{Input: 3},
	}
	expected, err := json.Marshal(publicTurn(item))
	if err != nil {
		t.Fatal(err)
	}
	item.Parts, item.TokenUsage = nil, nil
	fields := run.StoredTurnJSON{
		Parts: `[{"kind":"text","text":"<script>&"}]`, TokenUsage: `{"input":3,"cached":0,"output":0}`,
	}
	cold, err := cache.encode(ctx, item, fields)
	if err != nil {
		t.Fatal(err)
	}
	var got, want turnResponse
	if err := json.Unmarshal(cold, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &want); err != nil {
		t.Fatal(err)
	}
	if len(got.Parts) != 1 || got.Parts[0] != want.Parts[0] ||
		got.TokenUsage == nil || *got.TokenUsage != *want.TokenUsage || bytes.Contains(cold, []byte("<script>")) {
		t.Fatalf("JSON shape or escaping changed: %s", cold)
	}
	hot, err := cache.encode(ctx, item, fields)
	if err != nil || len(hot) == 0 || &hot[0] != &cold[0] {
		t.Fatalf("completed turn did not reuse its encoded bytes: %s, %v", hot, err)
	}

	item.OwnerID = "bob"
	fields.TokenUsage = `{"Input":7}`
	other, err := cache.encode(ctx, item, fields)
	if err != nil || !bytes.Contains(other, []byte(`"tokenUsage":{"input":7,"cached":0,"output":0}`)) {
		t.Fatalf("cache crossed owners or failed to normalize legacy usage: %s, %v", other, err)
	}
	item.OwnerID, item.Status = "alice", run.Running
	item.Parts = []run.TurnPart{{Kind: "text", Text: "in progress"}}
	live, err := cache.encode(ctx, item, run.StoredTurnJSON{})
	if err != nil || !bytes.Contains(live, []byte("in progress")) {
		t.Fatalf("running turn did not use its live parts: %s, %v", live, err)
	}
	item.Parts[0].Text = "new progress"
	live, err = cache.encode(ctx, item, run.StoredTurnJSON{})
	if err != nil || !bytes.Contains(live, []byte("new progress")) {
		t.Fatalf("running turn reused its old parts: %s, %v", live, err)
	}
}
