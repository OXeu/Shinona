// Package responsecache holds bounded, immutable response fragments.
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
	mu                 sync.Mutex
	keys               map[K]*list.Element
	order              list.List
	bytes              int
	maxCount, maxBytes int
}

func New[K comparable, V any](maxCount, maxBytes int) *Cache[K, V] {
	return &Cache[K, V]{keys: make(map[K]*list.Element), maxCount: maxCount, maxBytes: maxBytes}
}

func (c *Cache[K, V]) Get(key K) (value V, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.keys[key]; element != nil {
		c.order.MoveToFront(element)
		return element.Value.(entry[K, V]).value, true
	}
	return value, false
}

func (c *Cache[K, V]) Put(key K, value V, size int) {
	if c.maxCount <= 0 || c.maxBytes <= 0 || size < 0 || size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous := c.keys[key]; previous != nil {
		c.remove(previous)
	}
	for len(c.keys) >= c.maxCount || size > c.maxBytes-c.bytes {
		c.remove(c.order.Back())
	}
	c.keys[key] = c.order.PushFront(entry[K, V]{key, value, size})
	c.bytes += size
}

func (c *Cache[K, V]) remove(element *list.Element) {
	item := element.Value.(entry[K, V])
	delete(c.keys, item.key)
	c.bytes -= item.bytes
	c.order.Remove(element)
}
