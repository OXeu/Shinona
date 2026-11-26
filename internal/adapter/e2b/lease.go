package e2b

import (
	"context"
	"log/slog"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

const renewEvery = time.Minute

type projectWorkspace struct {
	sandbox.Workspace
	terminal  sandbox.TerminalWorkspace
	sandboxID string
	refresh   func(context.Context, time.Duration) error
	stop      context.CancelFunc
	done      chan struct{}
}

func (w *projectWorkspace) OpenTerminal(ctx context.Context, size sandbox.TerminalSize) (sandbox.Terminal, error) {
	return w.terminal.OpenTerminal(ctx, size)
}

// Active turns renew the lease. Inspection only uses the window requested
// when reconnecting, so opening the workspace panel does not start a keepalive.
func (w *projectWorkspace) keepAwake(ctx context.Context, window time.Duration) {
	ctx, w.stop = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(min(renewEvery, window/3))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				request, cancel := context.WithTimeout(ctx, apiTimeout)
				err := w.refresh(request, window)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("renew E2B workspace lease", "sandbox_id", w.sandboxID, "error", err)
				}
			}
		}
	}()
}

func (w *projectWorkspace) Close(ctx context.Context) error {
	if w.stop != nil {
		w.stop()
		select {
		case <-w.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return w.Workspace.Close(ctx)
}
