package server

import (
	"container/list"
	"sync"
)

// entryLRU is a concurrency-safe LRU keyed by string, bounded by the number
// of entries. Used for small values where the relevant cost is "how many
// distinct entries" — e.g. signed ManifestResponse structs.
//
// The cap is enforced strictly: a Put that exceeds it evicts the least
// recently used entry. A cap <= 0 is clamped to 1 to preserve the "bounded
// growth" invariant that motivates this type.
type entryLRU[V any] struct {
	cap int

	mu  sync.Mutex
	ll  *list.List // front = MRU, back = LRU
	idx map[string]*list.Element
}

type entryLRUItem[V any] struct {
	key   string
	value V
}

func newEntryLRU[V any](cap int) *entryLRU[V] {
	if cap <= 0 {
		cap = 1
	}
	return &entryLRU[V]{
		cap: cap,
		ll:  list.New(),
		idx: make(map[string]*list.Element, cap),
	}
}

// Get returns the value for k and marks it most-recently-used.
func (c *entryLRU[V]) Get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*entryLRUItem[V]).value, true
	}
	var zero V
	return zero, false
}

// Put inserts or refreshes k → v and marks it most-recently-used, evicting
// the LRU entry if the cap would be exceeded.
func (c *entryLRU[V]) Put(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[k]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*entryLRUItem[V]).value = v
		return
	}
	el := c.ll.PushFront(&entryLRUItem[V]{key: k, value: v})
	c.idx[k] = el
	if c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.idx, oldest.Value.(*entryLRUItem[V]).key)
		}
	}
}

// Clear empties the cache; cap stays the same.
func (c *entryLRU[V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	clear(c.idx)
}

// Len returns the current number of entries.
func (c *entryLRU[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// byteBudgetLRU is a concurrency-safe LRU keyed by string with []byte values,
// bounded by the SUM of value sizes (a "byte budget"). Designed for caching
// large payloads (e.g. compressed deltas) where the cost of each entry is
// its own length, not a flat 1.
//
// Semantics:
//
//   - Put(k, v) where len(v) > capBytes: the value is rejected silently
//     (too big to ever fit; caching it would evict everything else and then
//     itself). Callers that care can check Len() afterwards.
//   - Put(k, v) where existing size + len(v) > capBytes: evict LRU entries
//     until the new value fits.
//
// A capBytes <= 0 is clamped to 1.
type byteBudgetLRU struct {
	capBytes int64

	mu         sync.Mutex
	ll         *list.List // front = MRU
	idx        map[string]*list.Element
	totalBytes int64
}

type byteBudgetItem struct {
	key   string
	value []byte
}

func newByteBudgetLRU(capBytes int64) *byteBudgetLRU {
	if capBytes <= 0 {
		capBytes = 1
	}
	return &byteBudgetLRU{
		capBytes: capBytes,
		ll:       list.New(),
		idx:      make(map[string]*list.Element),
	}
}

// Get returns the value and marks it most-recently-used.
func (c *byteBudgetLRU) Get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*byteBudgetItem).value, true
	}
	return nil, false
}

// Put inserts or updates k → v, evicting LRU entries until the byte budget
// fits. Values larger than the whole budget are rejected and NOT inserted.
// Returns true when the value was accepted.
func (c *byteBudgetLRU) Put(k string, v []byte) bool {
	size := int64(len(v))
	if size > c.capBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[k]; ok {
		// Replace in place; adjust total accordingly.
		old := el.Value.(*byteBudgetItem)
		c.totalBytes -= int64(len(old.value))
		old.value = v
		c.totalBytes += size
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&byteBudgetItem{key: k, value: v})
		c.idx[k] = el
		c.totalBytes += size
	}
	for c.totalBytes > c.capBytes {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		item := oldest.Value.(*byteBudgetItem)
		c.ll.Remove(oldest)
		delete(c.idx, item.key)
		c.totalBytes -= int64(len(item.value))
	}
	return true
}

// Clear empties the cache; cap stays the same.
func (c *byteBudgetLRU) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	clear(c.idx)
	c.totalBytes = 0
}

// Len returns the current number of entries.
func (c *byteBudgetLRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Bytes returns the current total bytes held.
func (c *byteBudgetLRU) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalBytes
}

// MaxValueBytes returns the per-value acceptance threshold (the whole budget).
// Values larger than this can never be cached.
func (c *byteBudgetLRU) MaxValueBytes() int64 {
	return c.capBytes
}
