package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	cleanupTimeout = 10 * time.Second
	queueInterval  = 5 * time.Second
	maxActiveRuns  = 4
)

var errWorkspaceSetup = errors.New("could not prepare the sandbox")

// Worker dispatches a bounded number of turns. Claims preserve ordering within
// each session and avoid filling execution slots with the same workspace.
type Worker struct {
	repo     Repository
	resolve  ResolveTarget
	logger   *slog.Logger
	controls *Controls
}

func NewWorker(repo Repository, resolve ResolveTarget, logger *slog.Logger, controls *Controls) *Worker {
	return &Worker{repo: repo, resolve: resolve, logger: logger, controls: controls}
}

func (w *Worker) Serve(ctx context.Context) {
	ticker := time.NewTicker(queueInterval)
	defer ticker.Stop()
	wake := w.controls.queueWake()
	finished := make(chan struct{}, maxActiveRuns)
	inFlight := 0
	defer func() {
		// Completion still writes to the repository after cancellation.
		for range inFlight {
			<-finished
		}
	}()

	for ctx.Err() == nil {
		if inFlight < maxActiveRuns {
			item, err := w.controls.claim(ctx, w.repo)
			switch {
			case err != nil && ctx.Err() == nil:
				w.logger.Error("claim queued run", "error", err)
			case item != nil:
				inFlight++
				go func() {
					defer func() { finished <- struct{}{} }()
					w.execute(ctx, item)
				}()
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		case <-finished:
			inFlight--
		}
	}
}

func (w *Worker) execute(ctx context.Context, active *activeTurn) {
	defer active.cancel()
	item := active.item
	result, err := w.run(active.ctx, active)

	// Serialize the final state with Stop and reject steering after cleanup.
	w.controls.mu.Lock()
	defer w.controls.mu.Unlock()
	defer func() {
		delete(w.controls.active, active.key())
		w.controls.notifyLocked(active.key(), true)
	}()
	status := Succeeded
	switch {
	case active.stopping:
		status = Stopped
	case err != nil:
		details := failureDetails(err)
		w.logger.Error("execute run", "run_id", item.ID, "error", details)
		status = Failed
		result.Parts = append(result.Parts, TurnPart{Kind: "failure", Text: turnFailureReason(active.ctx, err), Output: details})
	}

	saveCtx, stopSave := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer stopSave()
	completion := Completion{
		Result: result, Steering: active.steering.close(), Status: status, UpdatedAt: time.Now().UTC(),
	}
	if err := w.repo.Complete(saveCtx, item.ID, completion); err != nil && !errors.Is(err, ErrNotRunning) {
		w.logger.Error("persist run completion", "run_id", item.ID, "error", err)
	}
}

var diagnosticCredentials = regexp.MustCompile(`(?i)(\b(?:api[ _-]?key|client[ _-]?secret|access[ _-]?token|refresh[ _-]?token|authorization)\b["']?\s*[:=]\s*["']?(?:(?:bearer|basic)[ \t]+)?|\b(?:bearer|basic)[ \t]+)[^\s"',;&}\]]+`)

func failureDetails(err error) string {
	const limit = 4096
	details := strings.ToValidUTF8(strings.TrimSpace(diagnosticCredentials.ReplaceAllString(err.Error(), "${1}[redacted]")), "�")
	if len(details) > limit {
		end := limit
		for !utf8.RuneStart(details[end]) {
			end--
		}
		details = details[:end] + "…"
	}
	return details
}

func turnFailureReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, ErrModelIdle):
		return "model_idle"
	case errors.Is(err, ErrModelResponse), errors.Is(err, ErrModelConfig):
		return "model"
	case errors.Is(err, errWorkspaceSetup), errors.Is(err, ErrPolicy):
		return "sandbox"
	default:
		return ""
	}
}

func (w *Worker) run(ctx context.Context, active *activeTurn) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	item := active.item
	spec, release, err := lockSessionProject(ctx, w.repo, item.OwnerID, item.SessionID, func(key sandbox.ProjectKey) (func(), error) {
		return w.controls.leases.Acquire(ctx, key, false)
	})
	if err != nil {
		return Result{}, fmt.Errorf("lock workspace project: %w", err)
	}
	defer release()
	spec, target, err := projectTarget(ctx, w.repo, w.resolve, spec, item.Backend, true)
	if err != nil {
		return Result{}, fmt.Errorf("load workspace project: %w", err)
	}
	spec.RunID = item.ID
	var projectID *string
	if _, ok := target.Provider.(sandbox.ProjectProvider); ok {
		projectID = &spec.ProjectID
	} else {
		spec.Policy = item.Policy
	}
	if !slices.Contains(target.policies(item.OwnerID), spec.Policy) {
		return Result{}, ErrPolicy
	}
	agent, err := target.ResolveAgent(ctx, item.OwnerID)
	if err != nil {
		return Result{}, err
	}
	history, err := w.repo.History(ctx, item, 0)
	if err != nil {
		return Result{}, fmt.Errorf("load conversation: %w", err)
	}
	workspace, err := target.Provider.Acquire(ctx, spec)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", errWorkspaceSetup, err)
	}

	detach := w.controls.publishWorkspace(active, workspace, projectID)
	result, runErr := agent.Run(ctx, Input{
		OwnerID: item.OwnerID, Prompt: item.Prompt, History: history, Payload: item.Payload, Steering: &active.steering,
		ReportParts:   func(parts []TurnPart) { w.controls.reportParts(active, parts) },
		ReportContext: func(usage ContextUsage) { w.controls.reportContext(active, usage) },
	}, workspace)
	detach()
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	closeErr := workspace.Close(closeCtx)
	return result, errors.Join(runErr, closeErr)
}
