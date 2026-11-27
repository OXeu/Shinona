package e2b

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/sandbox"
)

const renewEvery = time.Minute

type projectWorkspace struct {
	sandbox.Workspace
	terminal     sandbox.TerminalWorkspace
	sandboxID    string
	refresh      func(context.Context, time.Duration) error
	releaseLease func(context.Context) error
}

func (w *projectWorkspace) OpenTerminal(ctx context.Context, size sandbox.TerminalSize) (sandbox.Terminal, error) {
	return w.terminal.OpenTerminal(ctx, size)
}

// ProjectLeases shares provisioning and active renewals across provider
// versions. Its zero value is ready to use.
type ProjectLeases struct {
	gates  keylock.Locks[leaseKey]
	mu     sync.Mutex
	active map[leaseKey]*projectLease
}

type leaseKey struct {
	endpoint string
	project  sandbox.ProjectKey
}

type projectLease struct {
	id      string
	users   int
	window  time.Duration
	refresh func(context.Context, time.Duration) error
	changed chan struct{}
	stop    context.CancelFunc
	done    chan struct{}
}

// update runs under the project's gate. A reconnect updates an existing
// renewal task without starting one for a workspace inspection.
func (p *ProjectLeases) update(key leaseKey, id string, window time.Duration, refresh func(context.Context, time.Duration) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	lease := p.active[key]
	if lease == nil {
		return
	}
	if lease.id != id || window/3 <= 0 {
		delete(p.active, key)
		lease.stop()
		return
	}
	lease.window, lease.refresh = window, refresh
	close(lease.changed)
	lease.changed = make(chan struct{})
}

func (p *ProjectLeases) retain(ctx context.Context, key leaseKey, workspace *projectWorkspace, window time.Duration) {
	if window/3 <= 0 {
		return
	}
	p.mu.Lock()
	lease := p.active[key]
	if lease == nil {
		lifetime, stop := context.WithCancel(context.Background())
		lease = &projectLease{id: workspace.sandboxID, window: window, refresh: workspace.refresh,
			changed: make(chan struct{}), stop: stop, done: make(chan struct{})}
		if p.active == nil {
			p.active = make(map[leaseKey]*projectLease)
		}
		p.active[key] = lease
		go p.renew(lifetime, key, lease)
	}
	lease.users++
	p.mu.Unlock()

	release := sync.OnceValue(func() <-chan struct{} {
		p.mu.Lock()
		defer p.mu.Unlock()
		lease.users--
		if lease.users != 0 {
			return nil
		}
		if p.active[key] == lease {
			delete(p.active, key)
		}
		lease.stop()
		return lease.done
	})
	stop := context.AfterFunc(ctx, func() { release() })
	workspace.releaseLease = func(ctx context.Context) error {
		stop()
		if done := release(); done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
}

func (p *ProjectLeases) renew(ctx context.Context, key leaseKey, lease *projectLease) {
	defer close(lease.done)
	for ctx.Err() == nil {
		p.mu.Lock()
		window, changed, refresh := lease.window, lease.changed, lease.refresh
		p.mu.Unlock()
		timer := time.NewTimer(min(renewEvery, window/3))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
			continue
		case <-timer.C:
		}

		request, cancel := context.WithTimeout(ctx, apiTimeout)
		release, err := p.gates.Acquire(request, key, true)
		if err == nil {
			// A reconnect could apply a new timeout while this tick waited.
			p.mu.Lock()
			current := p.active[key] == lease && lease.changed == changed
			p.mu.Unlock()
			if current {
				err = refresh(request, window)
			}
			release()
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("renew E2B workspace lease", "sandbox_id", lease.id, "error", err)
		}
	}
}

func (w *projectWorkspace) Close(ctx context.Context) error {
	var err error
	if w.releaseLease != nil {
		err = w.releaseLease(ctx)
	}
	return errors.Join(err, w.Workspace.Close(ctx))
}
