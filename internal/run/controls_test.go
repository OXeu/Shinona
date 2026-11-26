package run

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

type claimRepository struct{ Repository }

func (claimRepository) ClaimNext(context.Context, time.Time) (*Run, error) {
	return &Run{ID: "turn", OwnerID: "owner", SessionID: "session"}, nil
}

func TestClaimedTurnHasNoExecutionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controls := &Controls{}
		repository := claimRepository{}
		active, err := controls.claim(t.Context(), repository)
		if err != nil || active == nil {
			t.Fatalf("claim: %v", err)
		}
		defer active.cancel()
		if _, exists := active.ctx.Deadline(); exists {
			t.Fatal("claimed turn has an execution deadline")
		}
		time.Sleep(3 * time.Minute)
		if err := active.ctx.Err(); err != nil {
			t.Fatalf("turn canceled after three minutes: %v", err)
		}
		if err := controls.stop(t.Context(), repository, "owner", "session", "turn"); err != nil {
			t.Fatal(err)
		}
		if err := active.ctx.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop did not cancel the active turn: %v", err)
		}
	})
}

func TestAccountDeletedStopsOnlyOwnedWork(t *testing.T) {
	controls := &Controls{}
	active, err := controls.claim(t.Context(), claimRepository{})
	if err != nil || active == nil {
		t.Fatalf("claim: %v", err)
	}
	t.Cleanup(active.cancel)
	owned, cancelOwned := context.WithCancel(t.Context())
	other, cancelOther := context.WithCancel(t.Context())
	t.Cleanup(cancelOwned)
	t.Cleanup(cancelOther)
	service := &Service{controls: controls}
	service.terminals.tabs = map[sessionKey][]*TerminalSession{
		{owner: "owner", id: "session"}: {{ctx: owned, cancel: cancelOwned}},
		{owner: "other", id: "session"}: {{ctx: other, cancel: cancelOther}},
	}
	service.AccountDeleted("other")
	if active.ctx.Err() != nil || owned.Err() != nil || !errors.Is(other.Err(), context.Canceled) {
		t.Fatal("account deletion did not isolate terminal cleanup by owner")
	}
	service.AccountDeleted("owner")
	if !active.stopping || !errors.Is(active.ctx.Err(), context.Canceled) || !errors.Is(owned.Err(), context.Canceled) {
		t.Fatal("account deletion did not stop owned work")
	}
	if len(service.terminals.tabs) != 0 {
		t.Fatal("deleted accounts retained terminal tabs")
	}
}
