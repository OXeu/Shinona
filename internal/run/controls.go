package run

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Controls routes commands to the single worker process. The mutex covers the
// database claim and registration so Stop cannot miss a newly claimed turn.
type Controls struct {
	mu       sync.Mutex
	leaseMu  sync.RWMutex
	active   *activeTurn
	watchers map[sessionKey]map[*SessionWatch]struct{}
}

type activeTurn struct {
	item      Run
	ctx       context.Context
	cancel    context.CancelFunc
	steering  Steering
	parts     []TurnPart
	usage     ContextUsage
	stopping  bool
	workspace *liveWorkspace
}

func (a *activeTurn) matches(owner, session, id string) bool {
	return a != nil && a.item.ID == id && a.item.OwnerID == owner && a.item.SessionID == session
}

func (c *Controls) claim(ctx context.Context, repo Repository) (*activeTurn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, err := repo.ClaimNext(ctx, time.Now().UTC())
	if err != nil || item == nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	active := &activeTurn{item: *item, ctx: runCtx, cancel: cancel}
	key := sessionKey{owner: item.OwnerID, id: item.SessionID}
	active.steering.onDelivery = func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.active == active {
			c.notifyLocked(key, false)
		}
	}
	c.active = active
	c.notifyLocked(key, true)
	return c.active, nil
}

func (c *Controls) steer(owner, session, id, prompt string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := c.active
	if !active.matches(owner, session, id) || active.stopping || active.ctx.Err() != nil {
		return ErrNotRunning
	}
	if err := active.steering.enqueue(prompt); err != nil {
		return err
	}
	c.notifyLocked(sessionKey{owner: owner, id: session}, false)
	return nil
}

func (c *Controls) stop(ctx context.Context, repo Repository, owner, session, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active.matches(owner, session, id) {
		c.active.stopping = true
		c.active.cancel()
		return nil
	}
	err := repo.StopQueued(ctx, owner, session, id, time.Now().UTC())
	if err == nil {
		c.notifyLocked(sessionKey{owner: owner, id: session}, true)
	}
	return err
}

func (c *Controls) stopAccount(owner string) {
	c.mu.Lock()
	if c.active != nil && c.active.item.OwnerID == owner {
		c.active.stopping = true
		c.active.cancel()
	}
	c.mu.Unlock()
	c.changedAccount(owner)
}

func (c *Controls) addProgress(turn *Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active.matches(turn.OwnerID, turn.SessionID, turn.ID) {
		turn.Steering = c.active.steering.snapshot()
		turn.Parts = slices.Clone(c.active.parts)
		turn.LiveContext = c.active.usage
	}
}

func (c *Controls) reportParts(active *activeTurn, parts []TurnPart) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == active && !slices.Equal(active.parts, parts) {
		active.parts = slices.Clone(parts)
		c.notifyLocked(sessionKey{owner: active.item.OwnerID, id: active.item.SessionID}, false)
	}
}

func (c *Controls) reportContext(active *activeTurn, usage ContextUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == active && active.usage != usage {
		active.usage = usage
		c.notifyLocked(sessionKey{owner: active.item.OwnerID, id: active.item.SessionID}, false)
	}
}
