package sqlite

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestTranscriptUpdateReadsChangedTurnsAndKeepsPageMetadata(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner, session = "owner", "session"
	seedAccounts(t, repo, owner)
	newSession(t, repo, owner, session)
	now := time.Now().UTC()
	known := make(map[string]run.TurnVersion)
	const count = run.TranscriptPageSize + 2
	for i := range count {
		id := fmt.Sprintf("turn-%02d", i+1)
		status := run.Succeeded
		if i == count-1 {
			status = run.Running
		}
		task := run.Run{ID: id, OwnerID: owner, SessionID: session, Status: status, CreatedAt: now, UpdatedAt: now,
			Payload: run.Payload{Options: run.Options{Model: id}}}
		if _, err := repo.Conversations.Enqueue(ctx, task, "Fixture", false); err != nil {
			t.Fatal(err)
		}
		known[id] = run.TurnVersion{ID: id, Status: status, UpdatedAt: now}
	}
	// A known completed row must not be decoded when building the delta.
	if err := repo.db.Exec(`UPDATE runs SET parts = 'invalid json' WHERE id = 'turn-03'`).Error; err != nil {
		t.Fatal(err)
	}
	for _, window := range []run.LiveTranscriptWindow{run.LiveLatestPage, run.LiveFullHistory} {
		update, err := repo.Conversations.TranscriptUpdate(ctx, owner, session, window, known)
		last := fmt.Sprintf("turn-%02d", count)
		if err != nil || update == nil || len(update.ChangedTurns) != 1 || update.ChangedTurns[0].ID != last ||
			update.LastOptions == nil || update.LastOptions.Model != last {
			t.Fatalf("expected only the live turn and the head options: %+v, %v", update, err)
		}
		wantCount, before, first := count, 0, "turn-01"
		if window == run.LiveLatestPage {
			wantCount, before, first = run.TranscriptPageSize, 3, "turn-03"
		}
		if len(update.Versions) != wantCount || update.Versions[0].ID != first || update.NextBeforeSequence != before {
			t.Fatalf("wrong page metadata: %+v", update)
		}
	}
	if err := repo.db.Exec(`UPDATE runs SET status = 'SUCCEEDED' WHERE session_id = ? AND status = 'RUNNING'`, session).Error; err != nil {
		t.Fatal(err)
	}
	last := fmt.Sprintf("turn-%02d", count)
	known[last] = run.TurnVersion{ID: last, Status: run.Succeeded, UpdatedAt: now}
	update, err := repo.Conversations.TranscriptUpdate(ctx, owner, session, run.LiveLatestPage, known)
	if err != nil || update == nil || len(update.ChangedTurns) != 0 || update.LastOptions == nil || update.LastOptions.Model != last {
		t.Fatalf("unchanged head still needs its model options: %+v, %v", update, err)
	}
	if update, err := repo.Conversations.TranscriptUpdate(ctx, "another", session, run.LiveLatestPage, known); update != nil || err != nil {
		t.Fatalf("another account observed an update: %+v, %v", update, err)
	}
	if err := repo.db.Model(&row{}).Where("id = ?", "turn-03").UpdateColumn("parts", "[]").Error; err != nil {
		t.Fatal(err)
	}
	for i := count; i <= run.MaxLiveTranscriptTurns; i++ {
		id := fmt.Sprintf("turn-%02d", i+1)
		if err := repo.db.Create(&row{ID: id, OwnerID: owner, SessionID: session, Sequence: i + 1,
			CreatedAt: now, UpdatedAt: now, Status: run.Succeeded}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if update, err := repo.Conversations.TranscriptUpdate(ctx, owner, session, run.LiveFullHistory, known); !errors.Is(err, run.ErrTranscriptExportLimit) {
		t.Fatalf("full live history exceeded its limit: %+v, %v", update, err)
	}
}

func TestTranscriptPageSequence(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner, session = "owner", "session"
	seedAccounts(t, repo, owner)
	now := time.Now().UTC()
	if err := repo.Conversations.CreateSession(ctx, run.Session{ID: session, OwnerID: owner, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for sequence := range 65 {
		item, err := repo.Conversations.Enqueue(ctx, run.Run{
			ID: fmt.Sprintf("turn-%d", sequence+1), OwnerID: owner, SessionID: session,
			Status: run.Queued, CreatedAt: now, UpdatedAt: now,
		}, "Fixture", false)
		if err != nil {
			t.Fatal(err)
		}
		if claimed, err := repo.Conversations.ClaimNext(ctx, now); err != nil || claimed == nil || claimed.ID != item.ID {
			t.Fatalf("claim turn %s: %v, %v", item.ID, claimed, err)
		}
		if err := repo.Conversations.Complete(ctx, item.ID, run.Completion{Status: run.Succeeded, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	check := func(before, first, last, next int) *run.Transcript {
		t.Helper()
		page, err := repo.Conversations.TranscriptPage(ctx, owner, session, before)
		if err != nil || page == nil {
			t.Fatalf("get page before %d: %v", before, err)
		}
		if len(page.Turns) != last-first+1 || page.NextBeforeSequence != next {
			t.Fatalf("before %d: got %d turns, next = %d", before, len(page.Turns), page.NextBeforeSequence)
		}
		for i, turn := range page.Turns {
			if turn.Sequence != first+i {
				t.Fatalf("turn %d: got sequence %d, want %d", i, turn.Sequence, first+i)
			}
		}
		return page
	}
	latest := check(0, 36, 65, 36)
	if latest.Session.TurnCount != 65 {
		t.Fatalf("total turn count: %d", latest.Session.TurnCount)
	}
	check(0, 36, 65, 36)
	if _, err := repo.Conversations.Enqueue(ctx, run.Run{
		ID: "appended", OwnerID: owner, SessionID: session, Status: run.Queued,
		CreatedAt: now, UpdatedAt: now,
	}, "", false); err != nil {
		t.Fatal(err)
	}
	older := check(latest.NextBeforeSequence, 6, 35, 6)
	check(older.NextBeforeSequence, 1, 5, 0)
	check(0, 37, 66, 37)
	check(31, 1, 30, 0)
	if err := repo.Conversations.StopQueued(ctx, owner, session, "appended", now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Conversations.RevertTurn(ctx, owner, session, older.Turns[0].ID, "appended", now); err != nil {
		t.Fatal(err)
	}
	check(0, 1, 5, 0)
	if page, err := repo.Conversations.TranscriptPage(ctx, "other-owner", session, 36); err != nil || page != nil {
		t.Fatalf("cursor must not expose another owner's session: %v, %v", page, err)
	}
}
