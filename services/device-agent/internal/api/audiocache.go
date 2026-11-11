package api

import (
	"container/list"
	"strconv"
	"sync"
	"time"
)

// audioCache holds synthesised reply audio in RAM only: a bounded LRU
// (clip count and total bytes) keyed by session id and clip number, plus a
// single "grace" slot for the goodbye clip of a session that just ended.
// Nothing here ever touches the disk.
type audioCache struct {
	mu       sync.Mutex
	maxClips int
	maxBytes int64
	now      func() time.Time

	order *list.List // front = most recently used; values are *clipEntry
	idx   map[string]*list.Element
	bytes int64

	grace      *clipEntry
	graceUntil time.Time
}

type clipEntry struct {
	key  string
	sid  string
	data []byte
}

func clipKey(sid string, n int) string { return sid + "/" + strconv.Itoa(n) }

func newAudioCache(maxClips int, maxBytes int64, now func() time.Time) *audioCache {
	if now == nil {
		now = time.Now
	}
	return &audioCache{maxClips: maxClips, maxBytes: maxBytes, now: now, order: list.New(), idx: map[string]*list.Element{}}
}

// Put stores a clip, evicting least recently used clips to stay within
// both bounds. A clip larger than the byte bound is not stored.
func (c *audioCache) Put(sid string, n int, data []byte) bool {
	if int64(len(data)) > c.maxBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := clipKey(sid, n)
	if el, ok := c.idx[k]; ok {
		c.removeLocked(el)
	}
	el := c.order.PushFront(&clipEntry{key: k, sid: sid, data: data})
	c.idx[k] = el
	c.bytes += int64(len(data))
	for c.order.Len() > c.maxClips || c.bytes > c.maxBytes {
		c.removeLocked(c.order.Back())
	}
	return true
}

func (c *audioCache) removeLocked(el *list.Element) {
	e := el.Value.(*clipEntry)
	c.order.Remove(el)
	delete(c.idx, e.key)
	c.bytes -= int64(len(e.data))
}

// Get returns a clip (or the unexpired grace clip).
func (c *audioCache) Get(sid string, n int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := clipKey(sid, n)
	if el, ok := c.idx[k]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*clipEntry).data, true
	}
	if c.grace != nil {
		if !c.now().Before(c.graceUntil) {
			c.grace = nil
		} else if c.grace.key == k {
			return c.grace.data, true
		}
	}
	return nil, false
}

// DropSession removes every LRU clip of a session (session end).
func (c *audioCache) DropSession(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if el.Value.(*clipEntry).sid == sid {
			c.removeLocked(el)
		}
		el = next
	}
}

// PutGrace keeps one clip of an ended session for ttl (the goodbye, so the
// UI can still play it after /end). It replaces any previous grace clip.
func (c *audioCache) PutGrace(sid string, n int, data []byte, ttl time.Duration) bool {
	if ttl <= 0 || int64(len(data)) > c.maxBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.grace = &clipEntry{key: clipKey(sid, n), sid: sid, data: data}
	c.graceUntil = c.now().Add(ttl)
	return true
}

// ClearGrace drops the grace clip (a new session started).
func (c *audioCache) ClearGrace() {
	c.mu.Lock()
	c.grace = nil
	c.mu.Unlock()
}

// Stats returns the clip count and bytes held (LRU + grace).
func (c *audioCache) Stats() (clips int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clips, bytes = c.order.Len(), c.bytes
	if c.grace != nil && c.now().Before(c.graceUntil) {
		clips++
		bytes += int64(len(c.grace.data))
	}
	return clips, bytes
}
