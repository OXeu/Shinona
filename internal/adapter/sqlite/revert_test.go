package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestConcurrentTurnEditsKeepAdmissionAndHistoryChecksAtomic(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "turn-edits.db")
	var stores []*ConversationStore
	for range 2 {
		repo, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = repo.Close() })
		if len(stores) == 0 {
			seedAccounts(t, repo, "owner")
		}
		stores = append(stores, repo.Conversations)
	}
	now := time.Now().UTC()
	const owner, sessionID = "owner", "session"
	if err := stores[0].CreateSession(ctx, run.Session{OwnerID: owner, ID: sessionID, Title: run.DefaultSessionTitle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	item := run.Run{ID: "original", OwnerID: owner, SessionID: sessionID, Prompt: "Inspect the failure", Status: run.Queued, CreatedAt: now, UpdatedAt: now}
	if _, err := stores[0].Enqueue(ctx, item, item.Prompt, false); err != nil {
		t.Fatal(err)
	}
	if got, err := stores[0].ClaimNext(ctx, now); err != nil || got == nil || got.ID != item.ID {
		t.Fatalf("claim original = %+v, %v", got, err)
	}
	if err := stores[0].Complete(ctx, item.ID, run.Completion{Status: run.Failed, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	editConcurrently := func(expected error, edit func(int, *ConversationStore) error) {
		t.Helper()
		start := make(chan struct{})
		results := make(chan error, len(stores))
		for i, store := range stores {
			go func() {
				<-start
				results <- edit(i, store)
			}()
		}
		close(start)
		first, second := <-results, <-results
		if first != nil {
			first, second = second, first
		}
		if first != nil || !errors.Is(second, expected) {
			t.Fatalf("concurrent edits = %v and %v, want one success and %v", first, second, expected)
		}
	}
	editConcurrently(run.ErrActiveRun, func(i int, store *ConversationStore) error {
		retry := run.Run{ID: fmt.Sprintf("retry-%d", i), OwnerID: owner, SessionID: sessionID, RetryOf: item.ID,
			Status: run.Queued, CreatedAt: now, UpdatedAt: now}
		_, err := store.EnqueueRetry(ctx, retry, run.RetryOptions{})
		return err
	})
	state, err := stores[0].TranscriptState(ctx, owner, sessionID)
	if err != nil || state == nil || state.Session.TurnCount != 2 || !state.Session.Active {
		t.Fatalf("retry did not become the single active head: %+v, %v", state, err)
	}
	if err := stores[0].StopQueued(ctx, owner, sessionID, state.HeadID, now); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].db.Exec("UPDATE runs SET payload = ? WHERE id = ?", "{invalid", state.HeadID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := stores[0].EnqueueRetry(ctx, run.Run{ID: "invalid-retry", OwnerID: owner, SessionID: sessionID, RetryOf: state.HeadID},
		run.RetryOptions{}); !errors.Is(err, run.ErrRetryTurn) {
		t.Fatalf("ineligible retry attempted to decode the payload: %v", err)
	}
	editConcurrently(run.ErrHistoryChanged, func(_ int, store *ConversationStore) error {
		_, err := store.RevertTurn(ctx, owner, sessionID, item.ID, state.HeadID, now)
		return err
	})
	state, err = stores[0].TranscriptState(ctx, owner, sessionID)
	if err != nil || state == nil || state.HeadID != "" || state.Session.TurnCount != 0 || state.Session.Title != run.DefaultSessionTitle {
		t.Fatalf("revert did not restore the empty history: %+v, %v", state, err)
	}
}

func TestRevertTurnRestoresDraftAndRemovesLaterTurnsFromContext(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC()
	seedAccounts(t, repo, "owner")
	session := run.Session{ID: "session", OwnerID: "owner", Title: run.DefaultSessionTitle, CreatedAt: now, UpdatedAt: now}
	if err := repo.Conversations.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	image := run.Image{Name: "image.png", MediaType: "image/png", Data: "iVBORw0KGgo="}
	enqueue := func(id string, payload run.Payload) run.Run {
		t.Helper()
		now = now.Add(time.Second)
		item, err := repo.Conversations.Enqueue(ctx, run.Run{
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
		claimed, err := repo.Conversations.ClaimNext(ctx, now)
		if err != nil || claimed == nil || claimed.ID != item.ID {
			t.Fatalf("claim %s: %+v, %v", item.ID, claimed, err)
		}
		if err := repo.Conversations.Complete(ctx, item.ID, run.Completion{
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
	if _, err := repo.Conversations.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, third.ID, now); !errors.Is(err, run.ErrActiveRun) {
		t.Fatalf("revert with queued turn: %v", err)
	}
	complete(third)
	if _, err := repo.Conversations.RevertTurn(ctx, "other", session.ID, second.ID, third.ID, now); !errors.Is(err, run.ErrSession) {
		t.Fatalf("revert from another account: %v", err)
	}
	if _, err := repo.Conversations.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, second.ID, now); !errors.Is(err, run.ErrHistoryChanged) {
		t.Fatalf("revert with stale transcript: %v", err)
	}
	if err := repo.Conversations.MarkSessionRead(ctx, session.OwnerID, session.ID, third.Sequence, third.ID); err != nil {
		t.Fatal(err)
	}
	rewound, err := repo.Conversations.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, third.ID, now.Add(time.Millisecond))
	if err != nil || rewound.Session.TurnCount != 1 || rewound.Session.Unread ||
		rewound.Prompt != second.Prompt || !slices.Equal(rewound.Images, []run.Image{image}) {
		t.Fatalf("reverted draft: %+v, %v", rewound, err)
	}
	if _, err := repo.Conversations.RevertTurn(ctx, session.OwnerID, session.ID, second.ID, third.ID, now); !errors.Is(err, run.ErrHistoryChanged) {
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
	if err := repo.Conversations.MarkSessionRead(ctx, session.OwnerID, session.ID, second.Sequence, second.ID); err != nil {
		t.Fatal(err)
	}
	transcript, err := repo.Conversations.Transcript(ctx, session.OwnerID, session.ID, 50)
	if err != nil || transcript == nil || !transcript.Session.Unread || len(transcript.Turns) != 2 ||
		transcript.Turns[0].ID != first.ID || transcript.Turns[1].ID != replacement.ID {
		t.Fatalf("transcript after replacement: %+v, %v", transcript, err)
	}
	history, err := repo.Conversations.History(ctx, run.Run{OwnerID: session.OwnerID, SessionID: session.ID, Sequence: 3}, 12)
	if err != nil || len(history) != 2 || history[0].Output != "first reply" || history[1].Output != "replacement reply" {
		t.Fatalf("model history after revert: %+v, %v", history, err)
	}
	empty, err := repo.Conversations.RevertTurn(ctx, session.OwnerID, session.ID, first.ID, replacement.ID, now)
	if err != nil || empty.Session.TurnCount != 0 || empty.Session.Title != run.DefaultSessionTitle {
		t.Fatalf("reverted all turns: %+v, %v", empty, err)
	}
}
