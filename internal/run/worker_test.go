package run

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
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
			worker.execute(t.Context(), active)
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
