package sqlite

import (
	"fmt"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

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
