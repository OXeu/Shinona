package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	completionWriteTimeout   = 10 * time.Second
	queueInterval            = 5 * time.Second
	DefaultWorkerConcurrency = 4
	FailureInterrupted       = "interrupted"
)

var errWorkspaceSetup = errors.New("could not prepare the sandbox")
var errClaimedProjectChanged = errors.New("the conversation moved after this turn was claimed")

// Worker dispatches a bounded number of turns. Claims preserve ordering within
// each session and avoid filling execution slots with the same workspace.
type Worker struct {
	repo        WorkerRepository
	projects    ProjectSandboxRepository
	resolve     ResolveTarget
	logger      *slog.Logger
	controls    *Controls
	parallelism int
	workspace   WorkspaceTimeouts
}

type workerEvent uint8

const (
	executionFinished workerEvent = iota
	completionFinished
)

func NewWorker(repo WorkerRepository, projects ProjectSandboxRepository, resolve ResolveTarget, logger *slog.Logger,
	controls *Controls, parallelism int, workspace WorkspaceTimeouts,
) *Worker {
	return &Worker{
		repo: repo, projects: projects, resolve: resolve, logger: logger,
		controls: controls, parallelism: max(1, parallelism), workspace: workspace,
	}
}

func (w *Worker) Serve(ctx context.Context) {
	ticker := time.NewTicker(queueInterval)
	defer ticker.Stop()
	// Cancel execution immediately on shutdown. Give pending completion writes
	// one shared grace period to finish, including retries after transient errors.
	completions, cancelCompletions := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelCompletions()
	stopGrace := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(completionWriteTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancelCompletions()
		case <-completions.Done():
		}
	})
	defer stopGrace()
	wake := w.controls.queueWake()
	// Allow one pending completion per execution slot. A slow write can then
	// block its own project without immediately consuming a model slot.
	maxOutstanding := 2 * w.parallelism
	updates := make(chan workerEvent, maxOutstanding)
	executing, outstanding := 0, 0
	defer func() {
		// Completion still writes to the repository after cancellation.
		for outstanding != 0 {
			if <-updates == completionFinished {
				outstanding--
			}
		}
	}()

	for ctx.Err() == nil {
		if executing < w.parallelism && outstanding < maxOutstanding {
			item, err := w.controls.claim(ctx, w.repo)
			switch {
			case err != nil && ctx.Err() == nil:
				w.logger.Error("claim queued run", "error", err)
			case item != nil:
				executing++
				outstanding++
				go func() {
					defer func() { updates <- completionFinished }()
					w.execute(ctx, completions, item, func() { updates <- executionFinished })
				}()
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		case event := <-updates:
			if event == executionFinished {
				executing--
			} else {
				outstanding--
			}
		}
	}
}

func (w *Worker) execute(ctx, completions context.Context, active *activeTurn, releaseExecution func()) {
	defer active.cancel()
	item := active.item
	result, err := w.runSafely(active)

	stopping := w.controls.beginCompletion(active)
	unstarted := errors.Is(err, errClaimedProjectChanged)
	status := Succeeded
	switch {
	case stopping || unstarted:
		status = Stopped
	case err != nil:
		details := failureDetails(err)
		w.logger.Error("execute run", "run_id", item.ID, "error", details)
		status = Failed
		reason := turnFailureReason(active.ctx, err)
		if errors.Is(ctx.Err(), context.Canceled) && errors.Is(err, context.Canceled) {
			reason = FailureInterrupted
		}
		result.Parts = append(result.Parts, TurnPart{Kind: "failure", Text: reason, Output: details})
	}

	finished := time.Now().UTC()
	releaseExecution()
	completion := Completion{
		Result: result, Steering: active.steering.close(), Status: status, UpdatedAt: finished, Unstarted: unstarted,
	}
	for completions.Err() == nil {
		saveCtx, cancel := context.WithTimeout(completions, completionWriteTimeout)
		err := w.repo.Complete(saveCtx, item.ID, completion)
		cancel()
		if err == nil || errors.Is(err, ErrNotRunning) {
			w.controls.mu.Lock()
			key := active.key()
			if w.controls.active[key] == active {
				delete(w.controls.active, key)
			}
			w.controls.notifyLocked(key, true)
			w.controls.mu.Unlock()
			return
		}
		w.logger.Error("persist run completion", "run_id", item.ID, "error", err)
		retry := time.NewTimer(time.Second)
		select {
		case <-completions.Done():
			retry.Stop()
			return
		case <-retry.C:
		}
	}
}

func (w *Worker) runSafely(active *activeTurn) (result Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("panic in agent turn", "run_id", active.item.ID, "stack", string(debug.Stack()))
			err = errors.New("agent turn failed unexpectedly")
		}
	}()
	return w.run(active.ctx, active)
}

func (w *Worker) closeTurnWorkspace(ctx context.Context, id string, workspace sandbox.Workspace) {
	defer func() {
		if recover() != nil {
			w.logger.Error("panic closing turn workspace", "run_id", id, "stack", string(debug.Stack()))
		}
	}()
	_ = w.workspace.closeResource(ctx, workspace.Close)
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
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
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
	startup, cancelStartup := context.WithTimeout(ctx, w.workspace.Lifecycle)
	defer cancelStartup()
	item := active.item
	spec := sandbox.Spec{OwnerID: item.OwnerID, SessionID: item.SessionID, ProjectID: item.ProjectID, Policy: sandbox.Shared}
	release, err := w.controls.leases.Acquire(startup, spec.ProjectKey(), false)
	if err != nil {
		return Result{}, fmt.Errorf("lock workspace project: %w", err)
	}
	defer release()
	project, err := w.repo.SessionProject(startup, item.OwnerID, item.SessionID)
	if err != nil {
		return Result{}, fmt.Errorf("load session project: %w", err)
	}
	if project != item.ProjectID {
		return Result{}, errClaimedProjectChanged
	}
	spec, target, err := projectTarget(startup, w.projects, w.resolve, spec, item.Backend, true)
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
	agent, err := target.ResolveAgent(startup, item.OwnerID)
	if err != nil {
		return Result{}, err
	}
	if len(item.Payload.Images) != 0 {
		item.Payload.Images, err = w.repo.LoadTurnImages(startup, item)
		if err != nil {
			return Result{}, fmt.Errorf("load turn attachments: %w", err)
		}
	}
	history, err := w.repo.History(startup, item, 0)
	if err != nil {
		return Result{}, fmt.Errorf("load conversation: %w", err)
	}
	if err := startup.Err(); err != nil {
		return Result{}, err
	}
	// Providers can use Acquire's context to maintain the lease after startup.
	lease, cancelLease := context.WithCancel(ctx)
	defer cancelLease()
	stopStartup := context.AfterFunc(startup, cancelLease)
	defer stopStartup()
	workspace, err := target.Provider.Acquire(lease, spec)
	stopStartup()
	startupErr := startup.Err()
	cancelStartup()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", errWorkspaceSetup, errors.Join(err, startupErr))
	}
	defer w.closeTurnWorkspace(ctx, item.ID, workspace)
	if startupErr != nil {
		w.controls.beginCompletion(active)
		return Result{}, startupErr
	}
	detach := w.controls.publishWorkspace(active, workspace, projectID)
	defer detach()
	// Freeze the outcome before draining borrowed readers or closing the
	// workspace. Neither cleanup step should accept a late stop request.
	defer w.controls.beginCompletion(active)
	return agent.Run(ctx, Input{
		OwnerID: item.OwnerID, SessionID: item.SessionID, Prompt: item.Prompt, History: history, Payload: item.Payload, Steering: &active.steering,
		ReportParts:   func(offset int, parts []TurnPart) { w.controls.reportParts(active, offset, parts) },
		FinalizeParts: func(count int) { w.controls.finalizeParts(active, count) },
		ReportContext: func(usage ContextUsage) { w.controls.reportContext(active, usage) },
	}, workspace)
}
