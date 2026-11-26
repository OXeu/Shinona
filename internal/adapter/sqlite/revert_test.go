package sqlite

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestRevertTurnRestoresDraftAndRemovesLaterTurnsFromContext(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC()
	session := run.Session{ID: "session", OwnerID: "owner", Title: run.DefaultSessionTitle, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	image := run.Image{Name: "image.png", MediaType: "image/png", Data: "iVBORw0KGgo="}
	enqueue := func(id string, payload run.Payload) run.Run {
		t.Helper()
		now = now.Add(time.Second)
		item, err := repo.Enqueue(ctx, run.Run{
			ID: id, OwnerID: session.OwnerID, SessionID: session.ID, Prompt: id,
			Payload: payload, Status: run.Queued, CreatedAt: now, UpdatedAt: now,
		}, id, false)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	complete := func(item run.Run) {
		t.Helper()
		claimed, err := repo.ClaimNext(ctx, now)
		if err != nil || claimed == nil || claimed.ID != item.ID {
			t.Fatalf("claim %s: %+v, %v", item.ID, claimed, err)
		}
		if err := repo.Complete(ctx, item.ID, run.Completion{
			Result: run.Result{Output: item.Prompt + " reply"}, Status: run.Succeeded, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := enqueue("first", run.Payload{})
	complete(first)
	second := enqueue("second", run.Payload{Images: []run.Image{image}})
	complete(second)
	third := enqueue("third", run.Payload{})
	if _, err := repo.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, third.ID, now); !errors.Is(err, run.ErrActiveRun) {
		t.Fatalf("revert with queued turn: %v", err)
	}
	complete(third)
	if _, err := repo.RevertTurn(ctx, "other", session.ID, second.ID, third.ID, now); !errors.Is(err, run.ErrSession) {
		t.Fatalf("revert from another account: %v", err)
	}
	if _, err := repo.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, second.ID, now); !errors.Is(err, run.ErrHistoryChanged) {
		t.Fatalf("revert with stale transcript: %v", err)
	}
	if err := repo.MarkSessionRead(ctx, session.OwnerID, session.ID, third.Sequence, third.ID); err != nil {
		t.Fatal(err)
	}
	rewound, err := repo.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, third.ID, now.Add(time.Millisecond))
	if err != nil || rewound.Session.TurnCount != 1 || rewound.Session.Unread ||
		rewound.Prompt != second.Prompt || !slices.Equal(rewound.Images, []run.Image{image}) {
		t.Fatalf("reverted draft: %+v, %v", rewound, err)
	}
	if _, err := repo.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, third.ID, now); !errors.Is(err, run.ErrHistoryChanged) {
		t.Fatalf("repeat revert: %v", err)
	}
	var retained int64
	if err := repo.db.WithContext(ctx).Model(&row{}).Where("id IN ? AND reverted_at IS NOT NULL", []string{second.ID, third.ID}).Count(&retained).Error; err != nil || retained != 2 {
		t.Fatalf("reverted records were not retained: %d, %v", retained, err)
	}
	replacement := enqueue("replacement", run.Payload{})
	complete(replacement)
	if replacement.Sequence != second.Sequence {
		t.Fatalf("replacement turn number: %d", replacement.Sequence)
	}
	if err := repo.MarkSessionRead(ctx, session.OwnerID, session.ID, second.Sequence, second.ID); err != nil {
		t.Fatal(err)
	}
	transcript, err := repo.Transcript(ctx, session.OwnerID, session.ID, 50)
	if err != nil || transcript == nil || !transcript.Session.Unread || len(transcript.Turns) != 2 ||
		transcript.Turns[0].ID != first.ID || transcript.Turns[1].ID != replacement.ID {
		t.Fatalf("transcript after replacement: %+v, %v", transcript, err)
	}
	history, err := repo.History(ctx, run.Run{OwnerID: session.OwnerID, SessionID: session.ID, Sequence: 3}, 12)
	if err != nil || len(history) != 2 || history[0].Output != "first reply" || history[1].Output != "replacement reply" {
		t.Fatalf("model history after revert: %+v, %v", history, err)
	}
	empty, err := repo.RevertTurn(ctx, session.OwnerID, session.ID, first.ID, replacement.ID, now)
	if err != nil || empty.Session.TurnCount != 0 || empty.Session.Title != run.DefaultSessionTitle {
		t.Fatalf("reverted all turns: %+v, %v", empty, err)
	}
}
