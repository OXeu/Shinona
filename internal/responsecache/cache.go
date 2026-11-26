// Package responsecache holds bounded caches ordered by recent use.
package responsecache

import (
	"container/list"
	"sync"
)

type entry[K comparable, V any] struct {
	key   K
	value V
	bytes int
}

type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	store Store[K, V]
}

func New[K comparable, V any](maxCount, maxBytes int) *Cache[K, V] {
	return &Cache[K, V]{store: NewStore[K, V](maxCount, maxBytes)}
}

func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store.Get(key)
}

func (c *Cache[K, V]) Put(key K, value V, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store.Put(key, value, size, nil)
}

func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store.Delete(key)
}

func (c *Cache[K, V]) DeleteIf(key K, match func(V) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.store.Peek(key); ok && match(value) {
		c.store.Delete(key)
	}
}

// Store keeps the cache order and bounds. Callers hold their own lock when
// updating related state together with the cache.
type Store[K comparable, V any] struct {
	keys               map[K]*list.Element
	order              list.List
	bytes              int
	maxCount, maxBytes int
}

func NewStore[K comparable, V any](maxCount, maxBytes int) Store[K, V] {
	return Store[K, V]{maxCount: maxCount, maxBytes: maxBytes}
}

func (c *Store[K, V]) Get(key K) (value V, ok bool) {
	if element := c.keys[key]; element != nil {
		c.order.MoveToFront(element)
		return element.Value.(entry[K, V]).value, true
	}
	return value, false
}

func (c *Store[K, V]) Peek(key K) (value V, ok bool) {
	if element := c.keys[key]; element != nil {
		return element.Value.(entry[K, V]).value, true
	}
	return value, false
}

// Put calls evict for entries removed to make room. The callback may update
// the caller's related state without accessing this Store.
func (c *Store[K, V]) Put(key K, value V, size int, evict func(K)) {
	if c.maxCount <= 0 || c.maxBytes <= 0 || size < 0 || size > c.maxBytes {
		return
	}
	c.Delete(key)
	for len(c.keys) >= c.maxCount || size > c.maxBytes-c.bytes {
		oldest := c.remove(c.order.Back())
		if evict != nil {
			evict(oldest)
		}
	}
	if c.keys == nil {
		c.keys = make(map[K]*list.Element)
	}
	c.keys[key] = c.order.PushFront(entry[K, V]{key, value, size})
	c.bytes += size
}

func (c *Store[K, V]) Delete(key K) {
	if element := c.keys[key]; element != nil {
		c.remove(element)
	}
}

func (c *Store[K, V]) DeleteWhere(match func(V) bool) {
	for _, element := range c.keys {
		if match(element.Value.(entry[K, V]).value) {
			c.remove(element)
		}
	}
}

func (c *Store[K, V]) remove(element *list.Element) K {
	item := element.Value.(entry[K, V])
	delete(c.keys, item.key)
	c.bytes -= item.bytes
	c.order.Remove(element)
	return item.key
}
