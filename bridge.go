package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxMessage = 25 * 1024 * 1024

var errUnavailable = errors.New("mailbox service unavailable; reconnect and retry")

type apiError struct{ Status int }

func (e *apiError) Error() string { return fmt.Sprintf("mailbox operation rejected (%d)", e.Status) }

type bridge struct {
	scale     *scaleControl
	base, key string
	http      *http.Client
	mu        sync.Mutex
	logins    map[string]window
}
type credential struct{ Email, Password, Protocol string }
type folder struct {
	ID, Name, Type string
	Subscribed     int
}
type messageInfo struct {
	UID     uint32    `json:"uid"`
	ID      string    `json:"id"`
	Date    time.Time `json:"date"`
	Size    uint32    `json:"size"`
	RawSize *int      `json:"rawSize"`
	Flags   []string  `json:"flags"`
}

func (b *bridge) request(c credential, method, path string, body []byte, headers map[string]string) ([]byte, error) {
	if b.scale != nil {
		release, err := acquireSlot(b.scale.api)
		if err != nil {
			b.scale.rejected.Add(1)
			return nil, err
		}
		defer release()
		b.scale.requests.Add(1)
	}
	if method != "GET" {
		defer b.invalidate(c)
	}
	req, err := http.NewRequest(method, b.base+"/api/protocol-gateway"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.Email, c.Password)
	req.Header.Set("X-Gateway-Key", b.key)
	req.Header.Set("X-Mail-Protocol", c.Protocol)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := b.http.Do(req)
	if err != nil {
		if b.scale != nil {
			b.scale.failures.Add(1)
		}
		return nil, errUnavailable
	}
	defer res.Body.Close()
	if b.scale != nil {
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			b.scale.auth.put(credentialKey(c), true, 60*time.Second, 256)
		} else {
			b.scale.failures.Add(1)
			if res.StatusCode == 401 || res.StatusCode == 403 {
				b.scale.auth.invalidate(credentialKey(c))
			}
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return nil, &apiError{res.StatusCode}
	}
	limit := 2 * maxMessage
	if !strings.HasPrefix(path, "/raw/") {
		limit = 2 * 1024 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(limit+1)))
	if err != nil || len(data) > limit {
		return nil, errUnavailable
	}
	return data, nil
}
func (b *bridge) json(c credential, method, path string, in, out any) error {
	var data []byte
	var err error
	if in != nil {
		data, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	data, err = b.request(c, method, path, data, nil)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
func (b *bridge) auth(c credential) error {
	if b.scale == nil {
		return b.json(c, "GET", "/auth", nil, nil)
	}
	_, err := b.scale.auth.load(credentialKey(c), 60*time.Second, func() (bool, int64, error) { err := b.json(c, "GET", "/auth", nil, nil); return err == nil, 256, err })
	return err
}
func (b *bridge) folders(c credential) ([]folder, error) {
	var out struct{ Folders []folder }
	err := b.json(c, "GET", "/folders", nil, &out)
	return out.Folders, err
}
func (b *bridge) revision(c credential) (int64, error) {
	if b.scale == nil {
		return b.remoteRevision(c)
	}
	value, err := b.scale.revisions.load(mailboxKey(c), b.scale.poll, func() (int64, int64, error) { n, e := b.remoteRevision(c); return n, 256, e })
	if err != nil {
		return 0, err
	}
	// Never authorize another device just because its mailbox data is cached.
	if err = b.auth(c); err != nil {
		return 0, err
	}
	return value, nil
}
func (b *bridge) remoteRevision(c credential) (int64, error) {
	var out struct{ Revision int64 }
	err := b.json(c, "GET", "/revision", nil, &out)
	return out.Revision, err
}
func (b *bridge) snapshot(c credential, id string) ([]messageInfo, uint32, int64, error) {
	if b.scale == nil {
		return b.remoteSnapshot(c, id)
	}
	revision, err := b.revision(c)
	if err != nil {
		return nil, 0, 0, err
	}
	key := mailboxKey(c) + c.Protocol + "\x00" + id + fmt.Sprintf("\x00%d", revision)
	value, err := b.scale.snapshots.load(key, 5*time.Minute, func() (mailboxSnapshot, int64, error) {
		release, e := acquireSlot(b.scale.scans)
		if e != nil {
			return mailboxSnapshot{}, 0, e
		}
		defer release()
		rows, next, rev, e := b.remoteSnapshot(c, id)
		return mailboxSnapshot{rows, next, rev}, rowsBytes(rows), e
	})
	return value.rows, value.next, value.revision, err
}
func (b *bridge) remoteSnapshot(c credential, id string) ([]messageInfo, uint32, int64, error) {
	revision, e := b.remoteRevision(c)
	if e != nil {
		return nil, 0, 0, e
	}
	var all []messageInfo
	var after, next uint32
	for {
		var page struct {
			Messages []messageInfo
			Next     uint32
		}
		if err := b.json(c, "GET", "/messages?folder="+url.QueryEscape(id)+fmt.Sprintf("&after=%d", after), nil, &page); err != nil {
			return nil, 0, 0, err
		}
		next = page.Next
		all = append(all, page.Messages...)
		if len(page.Messages) < 500 {
			break
		}
		if page.Messages[len(page.Messages)-1].UID <= after {
			return nil, 0, 0, errUnavailable
		}
		after = page.Messages[len(page.Messages)-1].UID
		if len(all) > 250000 {
			return nil, 0, 0, errors.New("folder too large; split it in webmail")
		}
	}
	end, e := b.remoteRevision(c)
	if e != nil || revision != end {
		return nil, 0, 0, errUnavailable
	}
	return all, next, revision, nil
}
func (b *bridge) messages(c credential, id string) ([]messageInfo, uint32, error) {
	rows, next, _, e := b.snapshot(c, id)
	return rows, next, e
}
func (b *bridge) raw(c credential, id string, uid uint32) ([]byte, error) {
	return b.request(c, "GET", "/raw/"+url.PathEscape(id)+fmt.Sprintf("?uid=%d", uid), nil, nil)
}
func (b *bridge) flags(c credential, id string, flags []string) error {
	if flags == nil {
		flags = []string{}
	}
	return b.json(c, "PATCH", "/flags/"+url.PathEscape(id), map[string]any{"flags": flags}, nil)
}
func hasFlag(flags []string, f string) bool {
	for _, v := range flags {
		if strings.EqualFold(v, f) {
			return true
		}
	}
	return false
}

func (b *bridge) modifyFlags(c credential, id string, uid uint32, flags []string, op string) ([]string, error) {
	if flags == nil {
		flags = []string{}
	}
	var out struct{ Flags []string }
	e := b.json(c, "PATCH", "/flags/"+url.PathEscape(id)+fmt.Sprintf("?uid=%d", uid), map[string]any{"flags": flags, "operation": op}, &out)
	return out.Flags, e
}

// Login attempts are bounded independently of authenticated polling operations.
func (b *bridge) login(c credential) error {
	b.mu.Lock()
	if b.logins == nil {
		b.logins = map[string]window{}
	}
	now := time.Now()
	w := b.logins[c.Email]
	if now.Sub(w.start) > time.Minute {
		w = window{start: now}
	}
	w.n++
	b.logins[c.Email] = w
	for k, v := range b.logins {
		if now.Sub(v.start) > time.Minute {
			delete(b.logins, k)
		}
	}
	blocked := w.n > 30 || len(b.logins) > 10000
	b.mu.Unlock()
	if blocked {
		return errUnavailable
	}
	return b.json(c, "GET", "/auth", nil, nil)
}

type mailboxDelta struct {
	Messages []messageInfo
	Removed  []string
	Revision int64
}

func (b *bridge) changedRows(c credential, id string, after int64, previous []messageInfo) ([]messageInfo, int64, error) {
	if b.scale == nil {
		rows, _, revision, e := b.snapshot(c, id)
		return rows, revision, e
	}
	if e := b.auth(c); e != nil {
		return nil, 0, e
	}
	key := mailboxKey(c) + id + fmt.Sprintf("\x00%d", after)
	delta, err := b.scale.deltas.load(key, b.scale.poll, func() (mailboxDelta, int64, error) {
		var d mailboxDelta
		e := b.json(c, "GET", "/changes?folder="+url.QueryEscape(id)+fmt.Sprintf("&after=%d", after), nil, &d)
		if e == nil && (d.Revision < after || len(d.Messages)+len(d.Removed) > 1000) {
			e = errUnavailable
		}
		return d, rowsBytes(d.Messages) + int64(len(d.Removed))*64, e
	})
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && (apiErr.Status == 409 || apiErr.Status == 404) {
			rows, _, revision, e := b.snapshot(c, id)
			return rows, revision, e
		}
		return nil, 0, err
	}
	release, e := acquireSlot(b.scale.scans)
	if e != nil {
		return nil, 0, e
	}
	defer release()
	replaced := map[string]messageInfo{}
	removed := map[string]bool{}
	seen := map[string]bool{}
	for _, r := range delta.Messages {
		replaced[r.ID] = r
	}
	for _, id := range delta.Removed {
		removed[id] = true
	}
	rows := make([]messageInfo, 0, len(previous)+len(delta.Messages))
	for _, r := range previous {
		if removed[r.ID] || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		if replacement, ok := replaced[r.ID]; ok {
			rows = append(rows, replacement)
		} else {
			rows = append(rows, r)
		}
	}
	for _, r := range delta.Messages {
		if !seen[r.ID] && !removed[r.ID] {
			rows = append(rows, r)
			seen[r.ID] = true
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UID < rows[j].UID })
	return rows, delta.Revision, nil
}
