package run

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/sandbox"
)

// Controls routes commands to the active turn of each session. The mutex covers
// the database claim and registration so Stop cannot miss a newly claimed turn.
type Controls struct {
	mu           sync.Mutex
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
	usage     ContextUsage
	stopping  bool
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

func (c *Controls) claim(ctx context.Context, repo Repository) (*activeTurn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, err := repo.ClaimNext(ctx, time.Now().UTC())
	if err != nil || item == nil {
		return nil, err
	}
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

func (c *Controls) updateSteering(owner, session, id string, durable bool, change func(*Steering) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey{owner: owner, id: session}
	active := c.active[key]
	if !active.matches(owner, session, id) || active.stopping || active.ctx.Err() != nil {
		return ErrNotRunning
	}
	if err := change(&active.steering); err != nil {
		return err
	}
	c.notifyLocked(key, durable)
	return nil
}

func (c *Controls) stop(ctx context.Context, repo Repository, owner, session, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey{owner: owner, id: session}
	if active := c.active[key]; active.matches(owner, session, id) {
		active.stopping = true
		active.cancel()
		return nil
	}
	err := repo.StopQueued(ctx, owner, session, id, time.Now().UTC())
	if err == nil {
		c.notifyLocked(key, true)
		c.wakeQueue()
	}
	return err
}

func (c *Controls) stopAccount(owner string) {
	c.mu.Lock()
	for key, active := range c.active {
		if key.owner == owner {
			active.stopping = true
			active.cancel()
		}
	}
	c.mu.Unlock()
	c.changedAccount(owner)
}

func (c *Controls) addProgress(turn *Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if active := c.active[sessionKey{turn.OwnerID, turn.SessionID}]; active.matches(turn.OwnerID, turn.SessionID, turn.ID) {
		turn.Steering = active.steering.snapshot()
		turn.Parts = slices.Clone(active.parts)
		turn.LiveContext = active.usage
	}
}

func (c *Controls) reportParts(active *activeTurn, parts []TurnPart) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[active.key()] == active && !slices.Equal(active.parts, parts) {
		active.parts = slices.Clone(parts)
		c.notifyLocked(active.key(), false)
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
