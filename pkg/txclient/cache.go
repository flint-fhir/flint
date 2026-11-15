package txclient

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

type cacheEntry struct {
	key       string
	value     map[string]any
	expiresAt time.Time
}

type lruCache struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	nowFunc    func() time.Time
	ll         *list.List
	items      map[string]*list.Element
	hits       atomic.Uint64
	misses     atomic.Uint64
}

func newLRUCache(maxEntries int, ttl time.Duration, nowFunc func() time.Time) *lruCache {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &lruCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		nowFunc:    nowFunc,
		ll:         list.New(),
		items:      make(map[string]*list.Element),
	}
}

func (c *lruCache) enabled() bool {
	return c != nil && c.maxEntries > 0 && c.ttl > 0
}

func (c *lruCache) get(key string) (map[string]any, bool) {
	if !c.enabled() {
		return nil, false
	}
	now := c.nowFunc()
	c.mu.Lock()
	ele, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	entry := ele.Value.(*cacheEntry)
	if !now.Before(entry.expiresAt) {
		c.ll.Remove(ele)
		delete(c.items, key)
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	c.ll.MoveToFront(ele)
	cloned := cloneMap(entry.value)
	c.mu.Unlock()
	c.hits.Add(1)
	return cloned, true
}

func (c *lruCache) put(key string, val map[string]any) {
	if !c.enabled() || val == nil {
		return
	}
	cloned := cloneMap(val)
	exp := c.nowFunc().Add(c.ttl)

	c.mu.Lock()
	defer c.mu.Unlock()

	if ele, ok := c.items[key]; ok {
		c.ll.MoveToFront(ele)
		entry := ele.Value.(*cacheEntry)
		entry.value = cloned
		entry.expiresAt = exp
		return
	}

	ele := c.ll.PushFront(&cacheEntry{
		key:       key,
		value:     cloned,
		expiresAt: exp,
	})
	c.items[key] = ele

	for c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}

func (c *lruCache) purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.ll.Init()
	clear(c.items)
	c.mu.Unlock()
}

func (c *lruCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

type inFlightCall struct {
	wg  sync.WaitGroup
	val map[string]any
	err error
}

type inFlightGroup struct {
	mu    sync.Mutex
	calls map[string]*inFlightCall
}

func (g *inFlightGroup) do(key string, fn func() (map[string]any, error)) (map[string]any, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*inFlightCall)
	}
	if call, ok := g.calls[key]; ok {
		g.mu.Unlock()
		call.wg.Wait()
		if call.err != nil {
			return nil, call.err
		}
		return cloneMap(call.val), nil
	}
	call := &inFlightCall{}
	call.wg.Add(1)
	g.calls[key] = call
	g.mu.Unlock()

	val, err := fn()
	call.val = cloneMap(val)
	call.err = err
	call.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return cloneMap(call.val), nil
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneSlice(in []any) []any {
	if in == nil {
		return nil
	}
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		return cloneMap(tv)
	case []any:
		return cloneSlice(tv)
	case []map[string]any:
		out := make([]map[string]any, len(tv))
		for i, m := range tv {
			out[i] = cloneMap(m)
		}
		return out
	case []string:
		out := make([]string, len(tv))
		copy(out, tv)
		return out
	default:
		return v
	}
}
