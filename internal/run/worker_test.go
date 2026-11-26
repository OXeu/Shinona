package run

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
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
		worker := NewWorker(repo, nil, nil, slog.New(slog.DiscardHandler), controls, DefaultWorkerConcurrency, DefaultWorkspaceTimeouts())
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
		worker := NewWorker(repo, nil, nil, slog.New(slog.DiscardHandler), controls, 1, DefaultWorkspaceTimeouts())
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

type workerWorkspaceRepository struct {
	WorkerRepository
	completed chan<- Completion
}

func (workerWorkspaceRepository) SessionProject(context.Context, string, string) (string, error) {
	return "", nil
}

func (workerWorkspaceRepository) History(context.Context, Run, int) ([]Exchange, error) {
	return nil, nil
}

func (r workerWorkspaceRepository) Complete(_ context.Context, _ string, completion Completion) error {
	r.completed <- completion
	return nil
}

type workerWorkspaceProvider func(context.Context) (sandbox.Workspace, error)

func (workerWorkspaceProvider) Policies(string) []sandbox.Policy {
	return []sandbox.Policy{sandbox.Shared}
}

func (p workerWorkspaceProvider) Acquire(ctx context.Context, _ sandbox.Spec) (sandbox.Workspace, error) {
	return p(ctx)
}

type workerTestAgent func(context.Context) (Result, error)

func (a workerTestAgent) Run(ctx context.Context, _ Input, _ sandbox.Workspace) (Result, error) {
	return a(ctx)
}

func TestWorkerBoundsStartupAndCompletesTimedOutTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limits := DefaultWorkspaceTimeouts()
		limits.Lifecycle = 4 * time.Second
		start := time.Now()
		writes := make(chan Completion, 1)
		repo := workerWorkspaceRepository{completed: writes}
		active := &activeTurn{item: Run{ID: "turn", OwnerID: "owner", SessionID: "session", Policy: sandbox.Shared}}
		active.ctx, active.cancel = context.WithCancel(t.Context())
		defer active.cancel()
		controls := &Controls{active: map[sessionKey]*activeTurn{active.key(): active}}
		provider := workerWorkspaceProvider(func(lease context.Context) (sandbox.Workspace, error) {
			<-lease.Done()
			return nil, lease.Err()
		})
		resolve := func(ctx context.Context, _, _ string) (Target, error) {
			if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(start.Add(limits.Lifecycle)) {
				t.Errorf("provider resolution deadline = %v (%t)", deadline, ok)
			}
			return Target{Provider: provider, ResolveAgent: func(context.Context, string) (Agent, error) {
				time.Sleep(time.Second)
				return workerTestAgent(func(context.Context) (Result, error) {
					t.Error("agent ran before the workspace was acquired")
					return Result{}, nil
				}), nil
			}}, nil
		}
		worker := NewWorker(repo, workspaceTestRepository{}, resolve, slog.New(slog.DiscardHandler), controls, 1, limits)
		worker.execute(t.Context(), active, func() {})
		completion := <-writes
		if time.Since(start) != limits.Lifecycle || completion.Status != Failed || len(completion.Parts) != 1 || completion.Parts[0].Text != "deadline" {
			t.Fatalf("startup ended after %v with %+v", time.Since(start), completion)
		}
		if len(controls.active) != 0 {
			t.Fatal("timed-out turn remained active after completion")
		}
	})
}

func TestWorkerKeepsAcquiredLeasePastStartupDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limits := DefaultWorkspaceTimeouts()
		limits.Lifecycle = time.Second
		var lease context.Context
		var closed bool
		provider := workerWorkspaceProvider(func(ctx context.Context) (sandbox.Workspace, error) {
			lease = ctx
			return timedWorkspace{close: func(context.Context) error {
				closed = true
				return nil
			}}, nil
		})
		resolve := func(context.Context, string, string) (Target, error) {
			return Target{Provider: provider, ResolveAgent: func(context.Context, string) (Agent, error) {
				return workerTestAgent(func(ctx context.Context) (Result, error) {
					time.Sleep(2 * limits.Lifecycle)
					if lease.Err() != nil {
						t.Errorf("startup canceled an active workspace lease: %v", lease.Err())
					}
					return Result{Output: "done"}, ctx.Err()
				}), nil
			}}, nil
		}
		ctx := t.Context()
		worker := NewWorker(workerWorkspaceRepository{}, workspaceTestRepository{}, resolve, slog.New(slog.DiscardHandler), &Controls{}, 1, limits)
		active := &activeTurn{item: Run{OwnerID: "owner", SessionID: "session", Policy: sandbox.Shared}, ctx: ctx}
		result, err := worker.run(ctx, active)
		if err != nil || result.Output != "done" || !closed || lease.Err() != context.Canceled {
			t.Fatalf("completed run = %+v, %v, closed = %t, lease error = %v", result, err, closed, lease.Err())
		}
	})
}

func TestWorkerUsesConfiguredCleanupForCanceledTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limits := DefaultWorkspaceTimeouts()
		limits.Cleanup = 2 * time.Second
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		workspace := timedWorkspace{close: func(cleanup context.Context) error {
			deadline, ok := cleanup.Deadline()
			if !ok || cleanup.Err() != nil || !deadline.Equal(start.Add(limits.Cleanup)) {
				t.Errorf("workspace cleanup deadline = %v (%t), context error = %v", deadline, ok, cleanup.Err())
			}
			<-cleanup.Done()
			return cleanup.Err()
		}}
		provider := workerWorkspaceProvider(func(context.Context) (sandbox.Workspace, error) { return workspace, nil })
		resolve := func(context.Context, string, string) (Target, error) {
			return Target{Provider: provider, ResolveAgent: func(context.Context, string) (Agent, error) {
				return workerTestAgent(func(ctx context.Context) (Result, error) {
					cancel()
					return Result{}, ctx.Err()
				}), nil
			}}, nil
		}
		worker := NewWorker(workerWorkspaceRepository{}, workspaceTestRepository{}, resolve, slog.New(slog.DiscardHandler), &Controls{}, 1, limits)
		active := &activeTurn{item: Run{OwnerID: "owner", SessionID: "session", Policy: sandbox.Shared}, ctx: ctx}
		_, err := worker.run(ctx, active)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != limits.Cleanup {
			t.Fatalf("worker cleanup finished after %v, error = %v", time.Since(start), err)
		}
	})
}
