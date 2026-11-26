// Package keylock provides cancellation-aware locks for independent resources.
package keylock

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

const exclusive = 1 << 30

type entry struct {
	gate *semaphore.Weighted
	refs int
}

// Locks removes keys when their last holder or waiter leaves. The zero value
// is ready to use.
type Locks[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*entry
}

func (l *Locks[K]) reserve(key K, shared bool) (*semaphore.Weighted, int64, func()) {
	l.mu.Lock()
	e := l.entries[key]
	if e == nil {
		if l.entries == nil {
			l.entries = make(map[K]*entry)
		}
		e = &entry{gate: semaphore.NewWeighted(exclusive)}
		l.entries[key] = e
	}
	e.refs++
	l.mu.Unlock()
	weight := int64(exclusive)
	if shared {
		weight = 1
	}
	return e.gate, weight, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		e.refs--
		if e.refs == 0 {
			delete(l.entries, key)
		}
	}
}

func (l *Locks[K]) Acquire(ctx context.Context, key K, shared bool) (func(), error) {
	gate, weight, drop := l.reserve(key, shared)
	if err := gate.Acquire(ctx, weight); err != nil {
		drop()
		return nil, err
	}
	return sync.OnceFunc(func() {
		gate.Release(weight)
		drop()
	}), nil
}

func (l *Locks[K]) TryAcquire(key K, shared bool) (release func(), ok bool) {
	gate, weight, drop := l.reserve(key, shared)
	if !gate.TryAcquire(weight) {
		drop()
		return nil, false
	}
	return sync.OnceFunc(func() {
		gate.Release(weight)
		drop()
	}), true
}
