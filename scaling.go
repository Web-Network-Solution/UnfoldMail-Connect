package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errBusy = errors.New("mail gateway is busy; retry shortly")

type cacheEntry[T any] struct {
	value       T
	until, used time.Time
	bytes       int64
}
type cacheFlight[T any] struct {
	done        chan struct{}
	value       T
	err         error
	invalidated bool
}

// Bounded LRU/TTL storage with coalesced loads. Values must be immutable.
type boundedCache[T any] struct {
	mu              sync.Mutex
	entries         map[string]cacheEntry[T]
	flights         map[string]*cacheFlight[T]
	maxEntries      int
	maxBytes, bytes int64
}

func newCache[T any](entries int, bytes int64) *boundedCache[T] {
	return &boundedCache[T]{entries: map[string]cacheEntry[T]{}, flights: map[string]*cacheFlight[T]{}, maxEntries: entries, maxBytes: bytes}
}
func (c *boundedCache[T]) get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if ok && time.Now().Before(e.until) {
		e.used = time.Now()
		c.entries[key] = e
		return e.value, true
	}
	if ok {
		delete(c.entries, key)
		c.bytes -= e.bytes
	}
	var zero T
	return zero, false
}
func (c *boundedCache[T]) putLocked(key string, v T, ttl time.Duration, size int64) {
	if old, ok := c.entries[key]; ok {
		c.bytes -= old.bytes
		delete(c.entries, key)
	}
	if size > c.maxBytes {
		return
	}
	for len(c.entries) >= c.maxEntries || c.bytes+size > c.maxBytes {
		var oldest string
		var when time.Time
		for k, e := range c.entries {
			if when.IsZero() || e.used.Before(when) {
				oldest = k
				when = e.used
			}
		}
		if when.IsZero() {
			break
		}
		c.bytes -= c.entries[oldest].bytes
		delete(c.entries, oldest)
	}
	now := time.Now()
	c.entries[key] = cacheEntry[T]{v, now.Add(ttl), now, size}
	c.bytes += size
}
func (c *boundedCache[T]) put(key string, v T, ttl time.Duration, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(key, v, ttl, size)
}
func (c *boundedCache[T]) invalidate(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, f := range c.flights {
		if strings.HasPrefix(k, prefix) {
			f.invalidated = true
		}
	}
	for k, e := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
			c.bytes -= e.bytes
		}
	}
}
func (c *boundedCache[T]) load(key string, ttl time.Duration, fn func() (T, int64, error)) (T, error) {
	if v, ok := c.get(key); ok {
		return v, nil
	}
	c.mu.Lock()
	// Recheck under the same lock used to publish/finish flights.
	if e, ok := c.entries[key]; ok && time.Now().Before(e.until) {
		c.mu.Unlock()
		return e.value, nil
	}
	if flight, ok := c.flights[key]; ok {
		c.mu.Unlock()
		timer := time.NewTimer(90 * time.Second)
		defer timer.Stop()
		select {
		case <-flight.done:
			return flight.value, flight.err
		case <-timer.C:
			var zero T
			return zero, errBusy
		}
	}
	if len(c.flights) >= c.maxEntries {
		c.mu.Unlock()
		var zero T
		return zero, errBusy
	}
	flight := &cacheFlight[T]{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()
	v, size, err := fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && !flight.invalidated {
		c.putLocked(key, v, ttl, size)
	}
	flight.value, flight.err = v, err
	delete(c.flights, key)
	close(flight.done)
	return v, err
}
func (c *boundedCache[T]) size() int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.bytes }

type mailboxSnapshot struct {
	rows     []messageInfo
	next     uint32
	revision int64
}
type scaleControl struct {
	auth                         *boundedCache[bool]
	revisions                    *boundedCache[int64]
	snapshots                    *boundedCache[mailboxSnapshot]
	deltas                       *boundedCache[mailboxDelta]
	api, heavy, scans            chan struct{}
	poll                         time.Duration
	metadataMu                   sync.Mutex
	metadata, metadataLimit      int64
	requests, failures, rejected atomic.Int64
}

func newScaleControl(poll time.Duration, upstream, transfers int, metadataLimit int64) *scaleControl {
	return &scaleControl{auth: newCache[bool](10000, 4<<20), revisions: newCache[int64](2000, 2<<20), snapshots: newCache[mailboxSnapshot](2000, 256<<20), deltas: newCache[mailboxDelta](2000, 32<<20), api: make(chan struct{}, upstream), heavy: make(chan struct{}, transfers), scans: make(chan struct{}, 8), poll: poll, metadataLimit: metadataLimit}
}
func credentialKey(c credential) string {
	digest := sha256.Sum256([]byte(c.Email + "\x00" + c.Protocol + "\x00" + c.Password))
	return hex.EncodeToString(digest[:])
}
func mailboxKey(c credential) string { return c.Email + "\x00" }
func acquireSlot(slots chan struct{}) (func(), error) {
	if slots == nil {
		return func() {}, nil
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-timer.C:
		return nil, errBusy
	}
}
func (b *bridge) transferSlot() (func(), error) {
	if b.scale == nil {
		return func() {}, nil
	}
	return acquireSlot(b.scale.heavy)
}
func (b *bridge) invalidate(c credential) {
	if b.scale != nil {
		b.scale.revisions.invalidate(mailboxKey(c))
		b.scale.snapshots.invalidate(mailboxKey(c))
		b.scale.deltas.invalidate(mailboxKey(c))
	}
}
func (b *bridge) metadataBudget(held *int64, bytes int64) error {
	if b.scale == nil {
		return nil
	}
	s := b.scale
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	if s.metadata-*held+bytes > s.metadataLimit {
		s.rejected.Add(1)
		return errBusy
	}
	s.metadata += bytes - *held
	*held = bytes
	return nil
}
func rowsBytes(rows []messageInfo) int64 {
	// Conservative accounting includes slice slack, identifiers, flags and optional size pointers.
	n := int64(cap(rows)) * 128
	for _, r := range rows {
		n += int64(len(r.ID) + len(r.Flags)*24 + 16)
		for _, f := range r.Flags {
			n += int64(len(f))
		}
	}
	return n
}
