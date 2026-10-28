package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Entirely local: real TLS clients and protocol listeners, simulated Worker.
// Does not accept a production endpoint, credentials, or send external email.
func TestCapacityProfile(t *testing.T) {
	if os.Getenv("PROTOCOL_LOAD_TEST") != "1" {
		t.Skip("opt-in local capacity harness")
	}
	setting := func(key string, def, max int) int {
		v := def
		if s := os.Getenv(key); s != "" {
			n, e := strconv.Atoi(s)
			if e != nil || n < 1 || n > max {
				t.Fatalf("invalid %s", key)
			}
			v = n
		}
		return v
	}
	users := setting("PROTOCOL_LOAD_USERS", 500, 1000)
	messages := setting("PROTOCOL_LOAD_MESSAGES", 1000, 20000)
	seconds := setting("PROTOCOL_LOAD_IDLE_SECONDS", 5, 600)
	latency := time.Duration(setting("PROTOCOL_LOAD_LATENCY_MS", 5, 1000)) * time.Millisecond
	var requests, pages, sends, changes, revision atomic.Int64
	revision.Store(1)
	// Reuse server-side encoded fixture pages; gateway still decodes every page as in production.
	encoded := map[int][]byte{}
	for after := 0; after <= messages; after += 500 {
		rows := []messageInfo{}
		for i := after + 1; i <= messages && i <= after+500; i++ {
			rows = append(rows, messageInfo{UID: uint32(i), ID: fmt.Sprint("m", i), Date: time.Now(), Flags: []string{`\Seen`}})
		}
		encoded[after], _ = json.Marshal(map[string]any{"next": messages + 1, "messages": rows})
	}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(latency)
		user, pass, ok := r.BasicAuth()
		if !ok || !strings.HasSuffix(user, "@load.example") || !strings.HasPrefix(pass, "device-") || r.Header.Get("X-Gateway-Key") != "local-load-secret" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/api/protocol-gateway") {
		case "/auth":
			w.Write([]byte(`{}`))
		case "/revision":
			fmt.Fprintf(w, `{"revision":%d}`, revision.Load())
		case "/changes":
			changes.Add(1)
			fmt.Fprintf(w, `{"revision":%d,"messages":[{"id":"m1","uid":1,"date":"2026-01-01T00:00:00Z","flags":["\\Seen","\\Flagged"]}],"removed":[]}`, revision.Load())
		case "/folders":
			w.Write([]byte(`{"folders":[{"id":"in","name":"Inbox","type":"inbox","subscribed":1}]}`))
		case "/messages":
			pages.Add(1)
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			w.Write(encoded[after])
		case "/raw/m1":
			if r.URL.Query().Get("uid") != "1" {
				w.WriteHeader(409)
				return
			}
			w.Write(testRaw)
		case "/submit":
			sends.Add(1)
			w.Write([]byte(`{"id":"accepted-locally"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer worker.Close()
	b := &bridge{base: worker.URL, key: "local-load-secret", http: &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{MaxIdleConns: 128, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 128}}, scale: newScaleControl(time.Minute, 128, 8, 3072<<20)}
	defer b.http.CloseIdleConnections()
	cert := certificate(t)
	listen := func() *limitListener {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		return &limitListener{Listener: l, max: 4000, perIP: 4000, perMinute: 6000, byIP: map[string]int{}, recent: map[string]window{}}
	}
	il := listen()
	im := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &imapSession{api: b}, nil, nil
	}})
	go im.Serve(tls.NewListener(il, cert))
	defer im.Close()
	sl := listen()
	sm := smtp.NewServer(&smtpBackend{b})
	sm.Domain = "load.example"
	sm.ReadTimeout = 60 * time.Second
	sm.WriteTimeout = 60 * time.Second
	sm.MaxMessageBytes = 5 << 20
	sm.MaxRecipients = 50
	go sm.Serve(tls.NewListener(sl, cert))
	defer sm.Close()
	clients := make([]*imapclient.Client, users*2)
	idles := make([]*imapclient.IdleCommand, users*2)
	durations := make([]time.Duration, users*2)
	var failures atomic.Int64
	var wg sync.WaitGroup
	ramp := make(chan struct{}, 32)
	connect := func(index int) {
		start := time.Now()
		user := fmt.Sprintf("user%d@load.example", index/2)
		password := fmt.Sprintf("device-%d", index%2)
		c, e := imapclient.DialTLS(il.Addr().String(), &imapclient.Options{TLSConfig: &tls.Config{InsecureSkipVerify: true}})
		if e == nil {
			e = c.Login(user, password).Wait()
		}
		if e == nil {
			var data *imap.SelectData
			data, e = c.Select("INBOX", nil).Wait()
			if e == nil && data.NumMessages != uint32(messages) {
				e = fmt.Errorf("message count %d", data.NumMessages)
			}
		}
		if e == nil {
			_, e = c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{UID: true, Envelope: true}).Collect()
		}
		if e == nil {
			idles[index], e = c.Idle()
		}
		if e != nil {
			failures.Add(1)
			if c != nil {
				c.Close()
			}
			t.Errorf("client %d: %v", index, e)
			return
		}
		clients[index] = c
		durations[index] = time.Since(start)
	}
	for u := 0; u < users; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			ramp <- struct{}{}
			defer func() { <-ramp }()
			connect(2 * u)
			connect(2*u + 1)
		}(u)
	}
	wg.Wait()
	defer func() {
		for _, c := range clients {
			if c != nil {
				c.Close()
			}
		}
	}()
	if failures.Load() != 0 {
		t.Fatalf("%d initial connection failures", failures.Load())
	}
	il.mu.Lock()
	connections := il.active
	il.mu.Unlock()
	if connections != users*2 {
		t.Fatalf("active connections %d", connections)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("users=%d TLS_IMAP_sessions=%d messages_per_mailbox=%d SELECT+FETCH+IDLE_p95=%s heap_MiB=%.1f runtime_MiB=%.1f pages=%d upstream=%d", users, connections, messages, durations[len(durations)*95/100], float64(memory.HeapAlloc)/(1<<20), float64(memory.Sys)/(1<<20), pages.Load(), requests.Load())
	// Independent SMTP TLS clients submit only into the local fake Worker.
	for u := 0; u < users; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			ramp <- struct{}{}
			defer func() { <-ramp }()
			user := fmt.Sprintf("user%d@load.example", u)
			c, e := smtp.DialTLS(sl.Addr().String(), &tls.Config{InsecureSkipVerify: true})
			if e != nil {
				t.Error(e)
				return
			}
			defer c.Close()
			if e = c.Auth(sasl.NewPlainClient("", user, "device-0")); e == nil {
				e = c.Mail(user, nil)
			}
			if e == nil {
				e = c.Rcpt("recipient@load.example", nil)
			}
			if e == nil {
				var w interface {
					Write([]byte) (int, error)
					Close() error
				}
				w, e = c.Data()
				if e == nil {
					_, e = w.Write(bytes.ReplaceAll(testRaw, []byte("a@example.com"), []byte(user)))
					if closeErr := w.Close(); e == nil {
						e = closeErr
					}
				}
			}
			if e != nil {
				t.Error(e)
			}
		}(u)
	}
	wg.Wait()
	before := requests.Load()
	time.Sleep(time.Duration(seconds) * time.Second)
	t.Logf("idle_seconds=%d idle_upstream_requests=%d SMTP_submissions=%d capacity_rejections=%d", seconds, requests.Load()-before, sends.Load(), b.scale.rejected.Load())
	if sends.Load() != int64(users) {
		t.Fatalf("SMTP submissions %d", sends.Load())
	}
	// A change to every mailbox must not reload its 10,000-row snapshot.
	beforePages := pages.Load()
	revision.Store(2)
	b.scale.revisions.invalidate("")
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ramp <- struct{}{}
			defer func() { <-ramp }()
			if e := idles[i].Close(); e != nil {
				t.Error(e)
				return
			}
			if e := idles[i].Wait(); e != nil {
				t.Error(e)
				return
			}
			if e := clients[i].Noop().Wait(); e != nil {
				t.Error(e)
				return
			}
			fetched, e := clients[i].Fetch(imap.UIDSetNum(1), &imap.FetchOptions{UID: true, Flags: true}).Collect()
			if e != nil || len(fetched) != 1 {
				t.Errorf("delta fetch %d: %v", i, e)
				return
			}
			found := false
			for _, f := range fetched[0].Flags {
				if f == imap.FlagFlagged {
					found = true
				}
			}
			if !found {
				t.Errorf("client %d missed incremental flags", i)
			}
		}(i)
	}
	wg.Wait()
	if pages.Load() != beforePages {
		t.Fatalf("incremental sync downloaded %d full pages", pages.Load()-beforePages)
	}
	t.Logf("incremental_clients=%d delta_requests=%d extra_full_pages=%d", len(clients), changes.Load(), pages.Load()-beforePages)
	// Office-network reconnect: disconnect both apps, then reconnect the full population.
	for _, c := range clients {
		if c != nil {
			c.Close()
		}
	}
	for u := 0; u < users; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			ramp <- struct{}{}
			defer func() { <-ramp }()
			connect(2 * u)
			connect(2*u + 1)
		}(u)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("reconnect failures %d", failures.Load())
	}
	t.Logf("reconnect_sessions=%d failures=%d", users*2, failures.Load())
}
