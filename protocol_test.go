package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testRaw = []byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: Test\r\nDate: Tue, 06 Oct 2026 10:00:00 +0000\r\nMessage-ID: <test@example.com>\r\nContent-Type: text/plain\r\n\r\nhello\r\n.line\r\n")

type fakeAPI struct {
	mu             sync.Mutex
	revoked        bool
	flags          []string
	deleted, sends int
	rawReads       int
}

func mockAPI(t *testing.T) (*bridge, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		u, p, ok := r.BasicAuth()
		if !ok || u != "a@example.com" || p != "app-secret" || r.Header.Get("X-Gateway-Key") != "gateway-key" || f.revoked {
			w.WriteHeader(401)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/protocol-gateway")
		w.Header().Set("Content-Type", "application/json")
		var out any = map[string]any{"ok": true}
		switch path {
		case "/revision":
			out = map[string]any{"revision": 1 + len(f.flags) + f.deleted}
		case "/auth":
			out = map[string]string{"email": u}
		case "/folders":
			out = map[string]any{"folders": []folder{{ID: "in", Name: "Inbox", Type: "inbox", Subscribed: 1}}}
		case "/messages":
			rows := []messageInfo{}
			if f.deleted == 0 {
				rows = append(rows, messageInfo{UID: 7, ID: "m1", Date: time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), Size: uint32(len(testRaw)), Flags: f.flags})
			}
			out = map[string]any{"next": 8, "messages": rows}
		case "/raw/m1":
			if r.URL.Query().Get("uid") != "7" {
				w.WriteHeader(409)
				return
			}
			f.rawReads++
			w.Write(testRaw)
			return
		case "/flags/m1":
			var b struct {
				Flags     []string
				Operation string
			}
			json.NewDecoder(r.Body).Decode(&b)
			switch b.Operation {
			case "add":
				for _, v := range b.Flags {
					if !hasFlag(f.flags, v) {
						f.flags = append(f.flags, v)
					}
				}
			case "remove":
				var kept []string
				for _, v := range f.flags {
					if !hasFlag(b.Flags, v) {
						kept = append(kept, v)
					}
				}
				f.flags = kept
			default:
				f.flags = b.Flags
			}
			out = map[string]any{"flags": f.flags}
		case "/pop-delete", "/expunge":
			f.deleted++
			out = map[string]any{"deleted": []string{"m1"}}
		case "/submit":
			f.sends++
		default:
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return &bridge{base: srv.URL, key: "gateway-key", http: srv.Client()}, f
}
func certificate(t *testing.T) *tls.Config {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := s.TLS.Certificates
	s.Close()
	return &tls.Config{Certificates: cert, MinVersion: tls.VersionTLS12}
}
func imapClient(t *testing.T, b *bridge) (*imapclient.Client, *imapserver.Server) {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &imapSession{api: b}, nil, nil
	}})
	go server.Serve(tls.NewListener(ln, certificate(t)))
	t.Cleanup(func() { server.Close() })
	c, e := imapclient.DialTLS(ln.Addr().String(), &imapclient.Options{TLSConfig: &tls.Config{InsecureSkipVerify: true}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	if e = c.Login("a@example.com", "app-secret").Wait(); e != nil {
		t.Fatal(e)
	}
	return c, server
}
func TestIMAPTLSFetchFlagsReadOnlyAndRevocation(t *testing.T) {
	b, f := mockAPI(t)
	c, _ := imapClient(t, b)
	d, e := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if e != nil || d.NumMessages != 1 || d.UIDValidity != folderValidity(folder{ID: "in"}) {
		t.Fatalf("select: %+v %v", d, e)
	}
	messages, e := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{UID: true, Envelope: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	if e != nil || len(messages) != 1 || messages[0].UID != 7 || messages[0].Envelope.Subject != "Test" || messages[0].RFC822Size != int64(len(testRaw)) {
		t.Fatalf("fetch failed: %v", e)
	}
	f.mu.Lock()
	seen := hasFlag(f.flags, "\\Seen")
	f.mu.Unlock()
	if seen {
		t.Fatal("read-only FETCH changed Seen")
	}
	if _, e = c.Select("INBOX", nil).Wait(); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Store(imap.UIDSetNum(7), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect(); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	flagged := hasFlag(f.flags, "\\Flagged")
	f.revoked = true
	f.mu.Unlock()
	if !flagged {
		t.Fatal("STORE did not persist")
	}
	if _, e = c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Flags: true}).Collect(); e == nil {
		t.Fatal("revoked device retained access")
	}
}
func TestSMTPAuthenticationSenderAndDurableSubmission(t *testing.T) {
	b, f := mockAPI(t)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	srv := smtp.NewServer(&smtpBackend{b})
	srv.Domain = "example.com"
	srv.AllowInsecureAuth = false
	srv.MaxMessageBytes = 5 * 1024 * 1024
	srv.MaxRecipients = 50
	go srv.Serve(tls.NewListener(ln, certificate(t)))
	defer srv.Close()
	c, e := smtp.DialTLS(ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Mail("a@example.com", nil); e == nil {
		t.Fatal("unauthenticated relay allowed")
	}
	if e = c.Auth(sasl.NewPlainClient("", "a@example.com", "app-secret")); e != nil {
		t.Fatal(e)
	}
	if e = c.Mail("spoof@example.com", nil); e == nil {
		t.Fatal("sender spoof allowed")
	}
	if e = c.Mail("a@example.com", nil); e != nil {
		t.Fatal(e)
	}
	if e = c.Rcpt("b@example.com", nil); e != nil {
		t.Fatal(e)
	}
	w, e := c.Data()
	if e != nil {
		t.Fatal(e)
	}
	w.Write(testRaw)
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 1 {
		t.Fatal("not submitted")
	}
}
func popConnect(t *testing.T, s *popServer) (net.Conn, *bufio.Reader) {
	a, b := net.Pipe()
	go s.serve(b)
	a.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(a)
	r.ReadString('\n')
	for _, cmd := range []string{"USER a@example.com", "PASS app-secret"} {
		io.WriteString(a, cmd+"\r\n")
		line, _ := r.ReadString('\n')
		if !strings.HasPrefix(line, "+OK") {
			t.Fatal(line)
		}
	}
	return a, r
}
func TestPOP3DisconnectDoesNotDeleteAndQuitCommits(t *testing.T) {
	b, f := mockAPI(t)
	s := &popServer{api: b, locked: map[string]bool{}}
	c, r := popConnect(t, s)
	io.WriteString(c, "DELE 1\r\n")
	r.ReadString('\n')
	c.Close()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		busy := s.locked["a@example.com"]
		s.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("POP lock leaked")
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	n := f.deleted
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("disconnect deleted message")
	}
	c, r = popConnect(t, s)
	io.WriteString(c, "DELE 1\r\n")
	r.ReadString('\n')
	io.WriteString(c, "RSET\r\n")
	r.ReadString('\n')
	io.WriteString(c, "QUIT\r\n")
	r.ReadString('\n')
	c.Close()
	time.Sleep(5 * time.Millisecond)
	f.mu.Lock()
	n = f.deleted
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("RSET did not cancel deletion")
	}
	c, r = popConnect(t, s)
	io.WriteString(c, "DELE 1\r\n")
	r.ReadString('\n')
	io.WriteString(c, "QUIT\r\n")
	line, _ := r.ReadString('\n')
	c.Close()
	if !strings.HasPrefix(line, "+OK") {
		t.Fatal(line)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleted != 1 {
		t.Fatal("QUIT did not commit")
	}
}
func TestSequenceWildcard(t *testing.T) {
	rows := []messageInfo{{UID: 4}, {UID: 9}}
	if got := selected(rows, imap.UIDSet{{Start: 10, Stop: 0}}); len(got) != 1 || got[0] != 1 {
		t.Fatalf("UID n:* incorrect: %v", got)
	}
	if got := selected(rows, imap.SeqSet{{Start: 0, Stop: 0}}); len(got) != 1 || got[0] != 1 {
		t.Fatal(got)
	}
	if !bytes.Contains(testRaw, []byte("\r\n")) {
		t.Fatal("invalid fixture")
	}
}

func TestIMAPExpungeAndDecodedSearch(t *testing.T) {
	b, f := mockAPI(t)
	c, _ := imapClient(t, b)
	if _, e := c.Select("INBOX", nil).Wait(); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); e != nil {
		t.Fatal(e)
	}
	removed, e := c.Expunge().Collect()
	if e != nil || len(removed) != 1 || removed[0] != 1 {
		t.Fatalf("expunge: %v %v", removed, e)
	}
	f.mu.Lock()
	n := f.deleted
	f.mu.Unlock()
	if n != 1 {
		t.Fatal("delete was not persisted")
	}
	s := &imapSession{rows: []messageInfo{{UID: 1}}}
	mime := []byte("Content-Type: multipart/alternative; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29ybGQ=\r\n--x--\r\n")
	ok, e := s.matches(0, s.rows[0], mime, &imap.SearchCriteria{Body: []string{"HELLO world"}})
	if e != nil || !ok {
		t.Fatalf("decoded search failed: %v", e)
	}
}
func TestIMAPRefusesPlaintextLogin(t *testing.T) {
	b, _ := mockAPI(t)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	srv := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &imapSession{api: b}, nil, nil
	}})
	go srv.Serve(ln)
	defer srv.Close()
	c, e := imapclient.DialInsecure(ln.Addr().String(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Login("a@example.com", "app-secret").Wait(); e == nil {
		t.Fatal("plaintext authentication allowed")
	}
}

func TestSMTPLoginCompatibility(t *testing.T) {
	b, _ := mockAPI(t)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	srv := smtp.NewServer(&smtpBackend{b})
	srv.Domain = "example.com"
	go srv.Serve(tls.NewListener(ln, certificate(t)))
	defer srv.Close()
	c, e := smtp.DialTLS(ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Auth(sasl.NewLoginClient("a@example.com", "app-secret")); e != nil {
		t.Fatal(e)
	}
}

func TestMetadataSearchDoesNotReadMIME(t *testing.T) {
	api, backend := mockAPI(t)
	size := len(testRaw)
	rows := []messageInfo{{UID: 7, ID: "m1", Date: time.Now(), RawSize: &size}}
	session := &imapSession{api: api, c: credential{"a@example.com", "app-secret", "imap"}, rows: rows}
	for _, criteria := range []*imap.SearchCriteria{
		{}, {NotFlag: []imap.Flag{imap.FlagSeen}}, {Larger: 1},
		{Or: [][2]imap.SearchCriteria{{{}, {Body: []string{"hello"}}}}},
		{Not: []imap.SearchCriteria{{Flag: []imap.Flag{imap.FlagSeen}}}},
	} {
		data, err := session.Search(imapserver.NumKindUID, criteria, nil)
		if err != nil || !data.All.(imap.UIDSet).Contains(7) {
			t.Fatalf("search: %v %v", data, err)
		}
	}
	if backend.rawReads != 0 {
		t.Fatalf("metadata downloaded %d bodies", backend.rawReads)
	}
	// Metadata rejection also skips the body component of mixed criteria.
	_, err := session.Search(imapserver.NumKindUID, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}, Body: []string{"hello"}}, nil)
	if err != nil || backend.rawReads != 0 {
		t.Fatal("unnecessary body download", err)
	}
	_, err = session.Search(imapserver.NumKindUID, &imap.SearchCriteria{Body: []string{"hello"}}, nil)
	if err != nil || backend.rawReads != 1 {
		t.Fatal("body search did not fetch expected UID", err)
	}
}

func TestPOP3InitialTLSAndGreetingHaveDeadline(t *testing.T) {
	for _, secure := range []bool{false, true} {
		server, client := net.Pipe()
		var conn net.Conn = server
		if secure {
			conn = tls.Server(server, certificate(t))
		}
		pop := &popServer{locked: map[string]bool{}, handshakeTimeout: 25 * time.Millisecond}
		done := make(chan struct{})
		go func() { defer close(done); pop.serve(conn) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			client.Close()
			t.Fatal("initial handshake/greeting did not time out")
		}
		client.Close()
	}
}

func TestSMTPUncertainSubmissionIsNotAccepted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) }))
	defer server.Close()
	s := &smtpSession{api: &bridge{base: server.URL, http: server.Client()}, authenticated: true, c: credential{Email: "a@example.com"}, from: "a@example.com", to: []string{"b@example.com"}}
	err := s.Data(bytes.NewReader(testRaw))
	response, ok := err.(*smtp.SMTPError)
	if !ok || response.Code != 554 || !strings.Contains(response.Message, "uncertain") {
		t.Fatalf("unexpected response: %v", err)
	}
}
