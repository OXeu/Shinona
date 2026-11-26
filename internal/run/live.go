package run

import (
	"context"
	"slices"
)

type sessionKey struct {
	owner string
	id    string
}

type LiveProgress struct {
	TurnID   string
	Parts    []TurnPart
	Steering []SteeringMessage
	Context  ContextUsage
}

type SessionChange struct {
	// Transcript indicates that the persisted transcript or session summary changed.
	Transcript bool
	Progress   LiveProgress
}

// SessionWatch coalesces notifications for one viewer. Slow viewers can read
// the latest state without retaining every intermediate version in memory.
type SessionWatch struct {
	controls *Controls
	key      sessionKey
	changed  chan struct{}
	dirty    bool
}

func (s *Service) WatchSession(ctx context.Context, owner, id string) (*SessionWatch, error) {
	if _, err := s.stores.Sessions.SessionProject(ctx, owner, id); err != nil {
		return nil, err
	}
	return s.controls.watch(sessionKey{owner: owner, id: id}), nil
}

func (c *Controls) watch(key sessionKey) *SessionWatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watchers == nil {
		c.watchers = make(map[sessionKey]map[*SessionWatch]struct{})
	}
	if c.watchers[key] == nil {
		c.watchers[key] = make(map[*SessionWatch]struct{})
	}
	watch := &SessionWatch{controls: c, key: key, changed: make(chan struct{}, 1)}
	c.watchers[key][watch] = struct{}{}
	return watch
}

func (w *SessionWatch) Changes() <-chan struct{} { return w.changed }

func (w *SessionWatch) Read() SessionChange {
	c := w.controls
	c.mu.Lock()
	change := SessionChange{Transcript: w.dirty}
	w.dirty = false
	active := c.active[w.key]
	change.Progress = active.progress()
	c.mu.Unlock()
	if active != nil {
		change.Progress.Steering = active.steering.snapshot()
	}
	return change
}

func (c *Controls) progress(key sessionKey) LiveProgress {
	c.mu.Lock()
	active := c.active[key]
	progress := active.progress()
	c.mu.Unlock()
	if active != nil {
		progress.Steering = active.steering.snapshot()
	}
	return progress
}

func (a *activeTurn) progress() LiveProgress {
	if a == nil {
		return LiveProgress{}
	}
	return LiveProgress{
		TurnID: a.item.ID, Parts: slices.Clone(a.parts), Context: a.usage,
	}
}

func (w *SessionWatch) Close() {
	c := w.controls
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.watchers[w.key], w)
	if len(c.watchers[w.key]) == 0 {
		delete(c.watchers, w.key)
	}
}

func (c *Controls) changed(owner, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifyLocked(sessionKey{owner: owner, id: id}, true)
}

func (c *Controls) changedAccount(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.watchers {
		if key.owner == owner {
			c.notifyLocked(key, true)
		}
	}
}

func (c *Controls) notifyLocked(key sessionKey, durable bool) {
	for watch := range c.watchers[key] {
		watch.dirty = watch.dirty || durable
		select {
		case watch.changed <- struct{}{}:
		default:
		}
	}
}
