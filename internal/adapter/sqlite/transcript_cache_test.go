package sqlite

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

func TestLatestTranscriptCacheTracksPersistedHeadAndSummary(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "latest-transcript.db")
	reader, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	writer, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	const owner, id = "owner", "session"
	seedAccounts(t, writer, owner, "other")
	now := time.Now().UTC()
	if err := writer.Conversations.CreateSession(ctx, run.Session{ID: id, OwnerID: owner, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	enqueue := func(turnID string) {
		t.Helper()
		_, err := writer.Conversations.Enqueue(ctx, run.Run{
			ID: turnID, OwnerID: owner, SessionID: id, Prompt: "Check status", Status: run.Queued,
			CreatedAt: now, UpdatedAt: now,
			Payload: run.Payload{Options: run.Options{MCPServers: []string{"fixture"}}},
		}, "Review", false)
		if err != nil {
			t.Fatal(err)
		}
	}
	claim := func(turnID string) {
		t.Helper()
		item, err := writer.Conversations.ClaimNext(ctx, now)
		if err != nil || item == nil || item.ID != turnID {
			t.Fatalf("claim %s: %v, %v", turnID, item, err)
		}
	}
	complete := func(turnID string) {
		t.Helper()
		if err := writer.Conversations.Complete(ctx, turnID, run.Completion{
			Result: run.Result{
				Output: "Done", Parts: []run.TurnPart{{Kind: "text", Text: "Original"}},
				TokenUsage: &run.TokenUsage{Input: 3},
			},
			Status: run.Succeeded, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(count int, status run.Status, hit bool) *run.Transcript {
		t.Helper()
		readCtx, stats := timing.WithRecorder(ctx)
		page, err := reader.Conversations.TranscriptPage(readCtx, owner, id, 0)
		if err != nil || page == nil || len(page.Turns) != count || page.Turns[count-1].Status != status {
			t.Fatalf("read %d turns with head %s: %+v, %v", count, status, page, err)
		}
		if got := slices.ContainsFunc(stats.Metrics(), func(metric timing.Metric) bool { return metric.Name == "transcript_cache_hit" }); got != hit {
			t.Fatalf("cache hit = %t, want %t", got, hit)
		}
		return page
	}

	enqueue("first")
	claim("first")
	complete("first")
	cold := read(1, run.Succeeded, false)
	cold.Turns[0].Parts[0].Text = "changed by caller"
	cold.Turns[0].Payload.Options.MCPServers[0] = "changed by caller"
	cold.Turns[0].TokenUsage.Input = 99
	warm := read(1, run.Succeeded, true)
	if turn := warm.Turns[0]; turn.Parts[0].Text != "Original" || turn.Payload.Options.MCPServers[0] != "fixture" || turn.TokenUsage.Input != 3 {
		t.Fatalf("cache retained caller mutations: %+v", turn)
	}
	warm.Turns[0].Parts[0].Text = "changed on hit"
	if got := read(1, run.Succeeded, true).Turns[0].Parts[0].Text; got != "Original" {
		t.Fatalf("cache exposed its parts slice: %q", got)
	}
	known := map[string]run.TurnVersion{"first": {ID: "first", UpdatedAt: now, Status: run.Succeeded}}
	update, err := reader.Conversations.TranscriptUpdate(ctx, owner, id, run.LiveLatestPage, known)
	if err != nil || update == nil || len(update.ChangedTurns) != 0 || len(update.Versions) != 1 ||
		update.LastOptions == nil || !slices.Equal(update.LastOptions.MCPServers, []string{"fixture"}) {
		t.Fatalf("cached live update included unchanged data or lost the last options: %+v, %v", update, err)
	}
	update.LastOptions.MCPServers[0] = "changed by live caller"
	if options := read(1, run.Succeeded, true).LastOptions(); !slices.Equal(options.MCPServers, []string{"fixture"}) {
		t.Fatalf("the live update exposed cached options: %+v", options)
	}

	title := "Updated"
	if _, err := writer.Conversations.UpdateSession(ctx, owner, id, run.SessionChanges{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Conversations.MarkSessionRead(ctx, owner, id, 1, "first"); err != nil {
		t.Fatal(err)
	}
	if page := read(1, run.Succeeded, true); page.Session.Title != title || page.Session.Unread {
		t.Fatalf("cache returned an old summary: %+v", page.Session)
	}
	if page, err := reader.Conversations.TranscriptPage(ctx, "other", id, 0); err != nil || page != nil {
		t.Fatalf("another owner received a cached page: %+v, %v", page, err)
	}

	enqueue("second")
	read(2, run.Queued, false)
	claim("second")
	read(2, run.Running, false)
	active, err := reader.Conversations.TranscriptPage(ctx, owner, id, 0)
	if err != nil || active == nil || len(active.Turns) != 2 || active.Turns[1].Status != run.Running ||
		active.Turns[0].Parts[0].Text != "Original" || len(active.Turns[1].Parts) != 0 {
		t.Fatalf("snapshot did not distinguish completed and running turns: %+v, %v", active, err)
	}
	complete("second")
	read(2, run.Succeeded, false)
	read(2, run.Succeeded, true)
	if _, err := writer.Conversations.RevertTurn(ctx, owner, id, "second", "second", now); err != nil {
		t.Fatal(err)
	}
	enqueue("replacement")
	claim("replacement")
	complete("replacement")
	if page := read(2, run.Succeeded, false); page.Turns[1].ID != "replacement" {
		t.Fatalf("cache returned the reverted head: %s", page.Turns[1].ID)
	}
	read(2, run.Succeeded, true)
	if _, err := writer.Conversations.RevertTurn(ctx, owner, id, "replacement", "replacement", now); err != nil {
		t.Fatal(err)
	}
	read(1, run.Succeeded, false)
	read(1, run.Succeeded, true)
	if err := writer.Conversations.ArchiveSession(ctx, owner, id, now); err != nil {
		t.Fatal(err)
	}
	if page, err := reader.Conversations.TranscriptPage(ctx, owner, id, 0); err != nil || page != nil {
		t.Fatalf("archived cached transcript: %+v, %v", page, err)
	}
}

func TestStoredTranscriptPartsAreAnArray(t *testing.T) {
	for _, stored := range []string{"", "null", "[]"} {
		row := transcriptRow{Parts: []byte(stored), TokenUsage: []byte(`{"Input":7}`)}
		turn, err := row.turn()
		if err != nil || len(turn.Parts) != 0 || turn.TokenUsage == nil || turn.TokenUsage.Input != 7 {
			t.Fatalf("parts %q: got %+v, %v", stored, turn, err)
		}
	}
	for _, stored := range []string{`{}`, `[`, `[] {}`} {
		row := transcriptRow{Parts: []byte(stored)}
		if _, err := row.turn(); err == nil {
			t.Errorf("accepted invalid parts %q", stored)
		}
	}
}

func TestLatestTranscriptCacheRejectsPagesOverItsBudget(t *testing.T) {
	page := &run.Transcript{Turns: []run.Run{{ID: "turn", Output: "Completed"}}}
	key := latestTranscriptKey{owner: "owner", session: "session"}
	size := latestTranscriptSize(page.Turns, nil) + len(key.owner) + len(key.session)
	for _, budget := range []int{0, size - 1, size} {
		cache := newLatestTranscriptCache(budget)
		cache.put(key, page)
		if hit := cache.get(key) != nil; hit != (budget == size) {
			t.Fatalf("cached page of %d bytes with budget %d: %t", size, budget, hit)
		}
	}
}
