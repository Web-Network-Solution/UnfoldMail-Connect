package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedSnapshotIsImmutableAndCredentialScoped(t *testing.T) {
	b, _ := mockAPI(t)
	b.scale = newScaleControl(time.Minute, 8, 2, 1<<20)
	c := credential{"a@example.com", "app-secret", "imap"}
	rows, _, _, err := b.snapshot(c, "in")
	if err != nil {
		t.Fatal(err)
	}
	again, _, _, err := b.snapshot(c, "in")
	if err != nil {
		t.Fatal(err)
	}
	if &rows[0] != &again[0] {
		t.Fatal("snapshot was not shared")
	}
	a := &imapSession{api: b, rows: rows, sharedRows: true}
	a.ownRows()
	a.rows[0].Flags = []string{"changed"}
	if hasFlag(again[0].Flags, "changed") {
		t.Fatal("one session modified cached metadata")
	}
	bad := c
	bad.Password = "wrong-device-password"
	if _, _, _, err = b.snapshot(bad, "in"); err == nil {
		t.Fatal("shared cache bypassed device authentication")
	}
	b.invalidate(c)
	next, _, _, err := b.snapshot(c, "in")
	if err != nil {
		t.Fatal(err)
	}
	if &rows[0] == &next[0] {
		t.Fatal("mutation failed to invalidate cache")
	}
}
func TestCacheInvalidationFencesInFlightLoad(t *testing.T) {
	cache := newCache[int](2, 20)
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		cache.load("user/key", time.Minute, func() (int, int64, error) { close(started); <-finish; return 1, 10, nil })
	}()
	<-started
	cache.invalidate("user/")
	close(finish)
	<-done
	if _, ok := cache.get("user/key"); ok {
		t.Fatal("stale in-flight load repopulated cache")
	}
	for i := 0; i < 10; i++ {
		cache.put(fmt.Sprint(i), i, time.Minute, 10)
	}
	if cache.size() > 20 || len(cache.entries) > 2 {
		t.Fatal("cache exceeded bounds")
	}
}
func TestUpstreamConcurrencyAndMetadataAdmission(t *testing.T) {
	var active, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	b := &bridge{base: srv.URL, http: srv.Client(), scale: newScaleControl(time.Minute, 4, 2, 100)}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.request(credential{}, "GET", "/auth", nil, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 4 {
		t.Fatalf("upstream concurrency %d", peak.Load())
	}
	var first, second int64
	if b.metadataBudget(&first, 60) != nil {
		t.Fatal("first admission failed")
	}
	if b.metadataBudget(&second, 60) == nil {
		t.Fatal("memory budget not enforced")
	}
	b.metadataBudget(&first, 0)
	if b.metadataBudget(&second, 60) != nil {
		t.Fatal("released memory was not reusable")
	}
}
func TestSingleFlightSharesConcurrentLoads(t *testing.T) {
	cache := newCache[int](10, 100)
	var calls atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := cache.load("same", time.Minute, func() (int, int64, error) { calls.Add(1); time.Sleep(10 * time.Millisecond); return 42, 1, nil })
			if e != nil || v != 42 {
				t.Error(v, e)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times", calls.Load())
	}
}

func TestIncrementalRowsMergeAndFallback(t *testing.T) {
	b, _ := mockAPI(t)
	b.scale = newScaleControl(time.Minute, 8, 2, 1<<20)
	c := credential{"a@example.com", "app-secret", "imap"}
	if e := b.auth(c); e != nil {
		t.Fatal(e)
	}
	key := mailboxKey(c) + "in\x001"
	b.scale.deltas.put(key, mailboxDelta{Messages: []messageInfo{{ID: "a", UID: 4, Flags: []string{`\Seen`}}, {ID: "c", UID: 5}}, Removed: []string{"b"}, Revision: 2}, time.Minute, 1024)
	old := []messageInfo{{ID: "a", UID: 1}, {ID: "b", UID: 2}, {ID: "a", UID: 4}}
	rows, rev, e := b.changedRows(c, "in", 1, old)
	if e != nil || rev != 2 || len(rows) != 2 || rows[0].UID != 4 || rows[1].ID != "c" || !hasFlag(rows[0].Flags, `\Seen`) {
		t.Fatalf("merge: %+v %d %v", rows, rev, e)
	}
	if old[0].UID != 1 {
		t.Fatal("modified session source")
	}
	// mockAPI has no changes endpoint; 404 must fall back to a consistent full snapshot.
	rows, _, e = b.changedRows(c, "in", 999, old)
	if e != nil || len(rows) == 0 {
		t.Fatalf("resync fallback: %v", e)
	}
}
