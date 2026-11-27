package run

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/sandbox"
)

// Controls routes commands to the active turn of each session.
type Controls struct {
	mu           sync.Mutex
	claiming     chan struct{}
	leases       keylock.Locks[sandbox.ProjectKey]
	active       map[sessionKey]*activeTurn
	liveProjects map[sandbox.ProjectKey]*liveWorkspace
	watchers     map[sessionKey]map[*SessionWatch]struct{}
	wakeOnce     sync.Once
	wake         chan struct{}
}

type activeTurn struct {
	item      Run
	ctx       context.Context
	cancel    context.CancelFunc
	steering  Steering
	parts     []TurnPart
	finalized int
	usage     ContextUsage
	stopping  bool
	finishing bool
	workspace *liveWorkspace
}

func (a *activeTurn) matches(owner, session, id string) bool {
	return a != nil && a.item.ID == id && a.item.OwnerID == owner && a.item.SessionID == session
}

func (a *activeTurn) key() sessionKey {
	return sessionKey{owner: a.item.OwnerID, id: a.item.SessionID}
}

func (c *Controls) wakeQueue() {
	select {
	case c.queueWake() <- struct{}{}:
	default:
	}
}

func (c *Controls) queueWake() chan struct{} {
	c.wakeOnce.Do(func() { c.wake = make(chan struct{}, 1) })
	return c.wake
}

func (c *Controls) claim(ctx context.Context, repo turnClaimer) (*activeTurn, error) {
	if err := c.beginClaim(ctx); err != nil {
		return nil, err
	}
	defer c.endClaim()
	item, err := repo.ClaimNext(ctx, time.Now().UTC())
	if err != nil || item == nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	active := &activeTurn{item: *item, ctx: runCtx, cancel: cancel}
	key := active.key()
	active.steering.onDelivery = func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.active[key] == active {
			c.notifyLocked(key, false)
		}
	}
	if c.active == nil {
		c.active = make(map[sessionKey]*activeTurn)
	}
	c.active[key] = active
	c.notifyLocked(key, true)
	return active, nil
}

func (c *Controls) beginClaim(ctx context.Context) error {
	for ctx.Err() == nil {
		c.mu.Lock()
		claiming := c.claiming
		if claiming == nil {
			c.claiming = make(chan struct{})
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		select {
		case <-claiming:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (c *Controls) endClaim() {
	c.mu.Lock()
	defer c.mu.Unlock()
	close(c.claiming)
	c.claiming = nil
}

func (c *Controls) updateSteering(owner, session, id string, durable bool, change func(*Steering) error) error {
	c.mu.Lock()
	key := sessionKey{owner: owner, id: session}
	active := c.active[key]
	available := active.matches(owner, session, id) && !active.stopping && !active.finishing && active.ctx.Err() == nil
	c.mu.Unlock()
	if !available {
		return ErrNotRunning
	}
	if err := change(&active.steering); err != nil {
		return err
	}
	c.mu.Lock()
	if c.active[key] == active || durable {
		c.notifyLocked(key, durable)
	}
	c.mu.Unlock()
	return nil
}

func (c *Controls) stop(ctx context.Context, repo queuedTurnStopper, owner, session, id string) error {
	key := sessionKey{owner: owner, id: session}
	if c.cancelActive(key, id) {
		return nil
	}
	err := repo.StopQueued(ctx, owner, session, id, time.Now().UTC())
	switch {
	case err == nil:
		c.changed(owner, session)
		c.wakeQueue()
		return nil
	case !errors.Is(err, ErrTurnClaimed):
		return err
	}

	// The durable status can become RUNNING before the worker registers it.
	c.mu.Lock()
	active, claiming := c.active[key], c.claiming
	c.mu.Unlock()
	if !active.matches(owner, session, id) && claiming != nil {
		select {
		case <-claiming:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.cancelActive(key, id) {
		return nil
	}
	return ErrNotRunning
}

func (c *Controls) cancelActive(key sessionKey, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := c.active[key]
	if !active.matches(key.owner, key.id, id) || active.finishing {
		return false
	}
	active.stopping = true
	active.cancel()
	return true
}

func (c *Controls) beginCompletion(active *activeTurn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	active.finishing = true
	return active.stopping
}

func (c *Controls) stopAccount(owner string) {
	c.mu.Lock()
	for key, active := range c.active {
		if key.owner == owner {
			if !active.finishing {
				active.stopping = true
			}
			active.cancel()
		}
	}
	c.mu.Unlock()
	c.changedAccount(owner)
}

func (c *Controls) addProgress(turn *Run) int {
	progress := c.progress(sessionKey{turn.OwnerID, turn.SessionID}, turn.ID)
	if progress.TurnID == turn.ID {
		turn.Steering = progress.Steering
		turn.Parts = progress.Parts
		turn.LiveContext = progress.Context
	}
	return progress.FinalizedParts
}

func (c *Controls) reportParts(active *activeTurn, offset int, parts []TurnPart) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := active.key()
	if c.active[key] != active || offset < active.finalized || offset > len(active.parts) ||
		slices.Equal(active.parts[offset:], parts) {
		return
	}
	clear(active.parts[offset:])
	active.parts = append(active.parts[:offset], parts...)
	for watch := range c.watchers[key] {
		if watch.firstChangedPart < 0 || offset < watch.firstChangedPart {
			watch.firstChangedPart = offset
		}
		watch.notify(false)
	}
}

func (c *Controls) finalizeParts(active *activeTurn, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := active.key()
	if c.active[key] == active && count > active.finalized && count <= len(active.parts) {
		active.finalized = count
		c.notifyLocked(key, false)
	}
}

func (c *Controls) reportContext(active *activeTurn, usage ContextUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[active.key()] == active && active.usage != usage {
		active.usage = usage
		c.notifyLocked(active.key(), false)
	}
}
