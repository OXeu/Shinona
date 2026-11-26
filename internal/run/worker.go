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
	queueInterval  = 500 * time.Millisecond
)

var errWorkspaceSetup = errors.New("could not prepare the sandbox")

// Worker executes one run at a time. The application starts exactly one worker.
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

	for ctx.Err() == nil {
		item, err := w.controls.claim(ctx, w.repo)
		switch {
		case err != nil:
			w.logger.Error("claim queued run", "error", err)
		case item != nil:
			w.execute(ctx, item)
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
		w.controls.active = nil
		w.controls.notifyLocked(sessionKey{owner: item.OwnerID, id: item.SessionID}, true)
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
	w.controls.leaseMu.Lock()
	defer w.controls.leaseMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	item := active.item
	spec, target, err := sessionSandbox(ctx, w.repo, w.resolve, item.OwnerID, item.SessionID, item.Backend, true)
	if err != nil {
		return Result{}, fmt.Errorf("load workspace project: %w", err)
	}
	spec.RunID = item.ID
	if _, ok := target.Provider.(sandbox.ProjectProvider); !ok {
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

	detach := w.controls.publishWorkspace(active, workspace)
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
