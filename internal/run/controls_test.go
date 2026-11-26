package run

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestSessionWatchCoalescesChangedPartSuffixes(t *testing.T) {
	controls := &Controls{}
	active, err := controls.claim(t.Context(), claimRepository{})
	if err != nil || active == nil {
		t.Fatalf("claim: %v", err)
	}
	t.Cleanup(active.cancel)
	finished := TurnPart{Kind: "thinking", Text: "finished"}
	first := TurnPart{Kind: "tool", Tool: "read", Status: string(Running)}
	second := TurnPart{Kind: "text", Text: "draft"}
	controls.reportParts(active, 0, []TurnPart{finished, first, second})
	watch := controls.watch(active.key())
	t.Cleanup(watch.Close)

	second.Text = "revised draft"
	controls.reportParts(active, 2, []TurnPart{second})
	first.Status = string(Succeeded)
	controls.reportParts(active, 1, []TurnPart{first, second})
	changes := watch.Read()
	if changes.PartOffset != 1 || !slices.Equal(changes.Progress.Parts, []TurnPart{first, second}) {
		t.Fatalf("unexpected coalesced progress: %+v", changes)
	}
	controls.reportParts(active, 2, nil)
	if change := watch.Read(); change.PartOffset != 2 || len(change.Progress.Parts) != 0 {
		t.Fatalf("truncation must replace the tail with no parts: %+v", change)
	}
	controls.reportContext(active, ContextUsage{Tokens: 100})
	if change := watch.Read(); change.PartOffset != 2 || len(change.Progress.Parts) != 0 || change.Progress.Context.Tokens != 100 {
		t.Fatalf("context-only update recopied completed parts: %+v", change)
	}
	if got := controls.progress(active.key()).Parts; !slices.Equal(got, []TurnPart{finished, first}) {
		t.Fatalf("reconnect must include the full active transcript: %+v", got)
	}
}

type claimRepository struct{}

func (claimRepository) ClaimNext(context.Context, time.Time) (*Run, error) {
	return &Run{ID: "turn", OwnerID: "owner", SessionID: "session"}, nil
}

type schedulerFixture struct {
	claim func() (*Run, error)
	stop  func(id string) error
}

func (r schedulerFixture) ClaimNext(context.Context, time.Time) (*Run, error) { return r.claim() }

func (r schedulerFixture) StopQueued(_ context.Context, _, _, id string, _ time.Time) error {
	return r.stop(id)
}

func TestQueuedStopAndClaimReachStorageIndependently(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	controls := &Controls{}
	entered, unblock := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	defer release()
	repository := schedulerFixture{
		claim: func() (*Run, error) { return nil, nil },
		stop: func(string) error {
			close(entered)
			<-unblock
			return nil
		},
	}
	stop := make(chan error, 1)
	go func() { stop <- controls.stop(ctx, repository, "alice", "pending", "turn") }()
	awaitHandoff(t, ctx, entered, "queued stop to reach SQLite")
	claim := make(chan error, 1)
	go func() {
		_, err := controls.claim(ctx, repository)
		claim <- err
	}()
	if err := awaitHandoff(t, ctx, claim, "claim to finish during a slow queued stop"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := awaitHandoff(t, ctx, stop, "queued stop to finish"); err != nil {
		t.Fatal(err)
	}
}

func TestStopWaitsOnlyForAClaimedTurnAndHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	controls := &Controls{}
	entered, unblock := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	defer release()
	stopAttempt := make(chan struct{}, 2)
	repository := schedulerFixture{
		claim: func() (*Run, error) {
			close(entered)
			<-unblock
			return &Run{ID: "claimed", OwnerID: "alice", SessionID: "session"}, nil
		},
		stop: func(id string) error {
			switch id {
			case "claimed":
				stopAttempt <- struct{}{}
				return ErrTurnClaimed
			case "queued":
				return nil
			default:
				return ErrNotRunning
			}
		},
	}
	claimed := make(chan *activeTurn, 1)
	go func() {
		turn, err := controls.claim(ctx, repository)
		if err != nil {
			t.Error(err)
		}
		claimed <- turn
	}()
	awaitHandoff(t, ctx, entered, "claim to reach SQLite")
	for id, want := range map[string]error{"queued": nil, "missing": ErrNotRunning} {
		stop := make(chan error, 1)
		go func() { stop <- controls.stop(ctx, repository, "alice", "session", id) }()
		if err := awaitHandoff(t, ctx, stop, "stop "+id+" during an unrelated claim"); !errors.Is(err, want) {
			t.Fatalf("stop %s: %v, want %v", id, err, want)
		}
	}
	request, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	canceled := make(chan error, 1)
	go func() { canceled <- controls.stop(request, repository, "alice", "session", "claimed") }()
	awaitHandoff(t, ctx, stopAttempt, "stop to observe the committed claim")
	cancelRequest()
	if err := awaitHandoff(t, ctx, canceled, "canceled stop to return while the claim is still registering"); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop returned %v after cancellation", err)
	}
	stop := make(chan error, 1)
	go func() { stop <- controls.stop(ctx, repository, "alice", "session", "claimed") }()
	awaitHandoff(t, ctx, stopAttempt, "second stop to observe the committed claim")
	release()
	turn := awaitHandoff(t, ctx, claimed, "claim to register")
	if err := awaitHandoff(t, ctx, stop, "stop after claim registration"); err != nil || turn == nil || !errors.Is(turn.ctx.Err(), context.Canceled) {
		t.Fatalf("stop did not cancel the registered turn: %v", err)
	}
}

func awaitHandoff[T any](t *testing.T, ctx context.Context, result <-chan T, action string) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", action)
		var zero T
		return zero
	}
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
		if err := controls.stop(t.Context(), nil, "owner", "session", "turn"); err != nil {
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
	owned, cancelOwned := context.WithCancelCause(t.Context())
	other, cancelOther := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancelOwned(nil) })
	t.Cleanup(func() { cancelOther(nil) })
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
