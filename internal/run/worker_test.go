package run

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"
)

type completingRepository struct {
	WorkerRepository
	writes chan Completion
}

func (r completingRepository) SessionProject(context.Context, string, string) (string, error) {
	panic("fixture panic")
}

func (r completingRepository) Complete(_ context.Context, _ string, completion Completion) error {
	r.writes <- completion
	if completion.UpdatedAt.IsZero() {
		return errors.New("missing completion time")
	}
	return nil
}

type retryCompletionRepository struct {
	completingRepository
	attempts int
}

func (r *retryCompletionRepository) Complete(ctx context.Context, id string, completion Completion) error {
	r.attempts++
	if err := r.completingRepository.Complete(ctx, id, completion); err != nil {
		return err
	}
	if r.attempts == 1 {
		return errors.New("temporary write failure")
	}
	return nil
}

func TestWorkerRetriesCompletionAfterPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &retryCompletionRepository{completingRepository{writes: make(chan Completion)}, 0}
		active := &activeTurn{item: Run{ID: "turn", OwnerID: "owner", SessionID: "session"}}
		active.ctx, active.cancel = context.WithCancel(t.Context())
		defer active.cancel()
		controls := &Controls{active: map[sessionKey]*activeTurn{active.key(): active}}
		worker := NewWorker(repo, nil, nil, slog.New(slog.DiscardHandler), controls, DefaultWorkerConcurrency)
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.execute(t.Context(), active, func() {})
		}()
		first := <-repo.writes
		if first.Status != Failed || len(first.Parts) != 1 || first.Parts[0].Output != "agent turn failed unexpectedly" {
			t.Fatalf("unexpected recovered result: %+v", first)
		}
		synctest.Wait()
		if controls.active[active.key()] != active {
			t.Fatal("completion failure discarded the active record")
		}
		second := <-repo.writes
		if second.UpdatedAt != first.UpdatedAt {
			t.Fatal("completion retry did not preserve its timestamp")
		}
		<-done
		if len(controls.active) != 0 {
			t.Fatal("durable completion retained the active record")
		}
	})
}

type pendingCompletionRepository struct {
	completingRepository
	queued chan Run
	writes chan string
	permit map[string]chan struct{}
}

func (r pendingCompletionRepository) ClaimNext(context.Context, time.Time) (*Run, error) {
	select {
	case item := <-r.queued:
		return &item, nil
	default:
		return nil, nil
	}
}

func (r pendingCompletionRepository) Complete(ctx context.Context, id string, _ Completion) error {
	r.writes <- id
	if permit := r.permit[id]; permit != nil {
		select {
		case <-permit:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestWorkerKeepsExecutingWhileCompletionsWaitForStorage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := make(chan struct{}), make(chan struct{})
		repo := pendingCompletionRepository{
			queued: make(chan Run, 3), writes: make(chan string, 3),
			permit: map[string]chan struct{}{"first": first, "second": second},
		}
		for _, id := range []string{"first", "second", "third"} {
			repo.queued <- Run{ID: id, OwnerID: id, SessionID: "session"}
		}
		controls := &Controls{}
		worker := NewWorker(repo, nil, nil, slog.New(slog.DiscardHandler), controls, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.Serve(ctx)
		}()

		synctest.Wait()
		if len(repo.writes) != 2 || len(repo.queued) != 1 {
			t.Fatalf("one executing slot should allow two outstanding turns, got %d writes and %d still queued", len(repo.writes), len(repo.queued))
		}
		close(first)
		synctest.Wait()
		if len(repo.writes) != 3 || len(repo.queued) != 0 {
			t.Fatal("releasing one completion did not let the next turn execute")
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("worker exited before draining its outstanding completion")
		default:
		}
		close(second)
		<-done
	})
}
