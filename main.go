package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	imapserver "github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-smtp"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

// Bound sockets and connection churn independently of protocol authentication.
type limitListener struct {
	net.Listener
	mu                    sync.Mutex
	active                int
	byIP                  map[string]int
	recent                map[string]window
	max, perIP, perMinute int
}
type window struct {
	start time.Time
	n     int
}
type trackedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *trackedConn) Close() error { e := c.Conn.Close(); c.once.Do(c.release); return e }
func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}
		ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		l.mu.Lock()
		now := time.Now()
		w := l.recent[ip]
		if now.Sub(w.start) > time.Minute {
			w = window{start: now}
		}
		w.n++
		l.recent[ip] = w
		if len(l.recent) > 10000 {
			for k, v := range l.recent {
				if now.Sub(v.start) > time.Minute {
					delete(l.recent, k)
				}
			}
		}
		blocked := l.active >= l.max || l.byIP[ip] >= l.perIP || w.n > l.perMinute || len(l.recent) > 10000
		if !blocked {
			l.active++
			l.byIP[ip]++
		}
		l.mu.Unlock()
		if blocked {
			c.Close()
			continue
		}
		return &trackedConn{Conn: c, release: func() {
			l.mu.Lock()
			l.active--
			l.byIP[ip]--
			if l.byIP[ip] == 0 {
				delete(l.byIP, ip)
			}
			l.mu.Unlock()
		}}, nil
	}
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		r, e := c.Get("http://127.0.0.1:8080/healthz")
		if e != nil || r.StatusCode != 200 {
			os.Exit(1)
		}
		r.Body.Close()
		return
	}
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
func run() error {
	base := os.Getenv("WORKER_URL")
	u, e := url.Parse(base)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("WORKER_URL must be an HTTPS origin without a trailing slash")
	}
	key := os.Getenv("GATEWAY_SECRET")
	if len(key) < 32 {
		return errors.New("GATEWAY_SECRET must contain at least 32 characters")
	}
	certFile, keyFile := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	cert, e := tls.LoadX509KeyPair(certFile, keyFile)
	if e != nil {
		return fmt.Errorf("load TLS certificate: %w", e)
	}
	var certMu sync.Mutex
	certLoaded := time.Now()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		certMu.Lock()
		defer certMu.Unlock()
		if time.Since(certLoaded) > time.Minute {
			fresh, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			cert = fresh
			certLoaded = time.Now()
		}
		return &cert, nil
	}}
	pollSeconds, e := strconv.Atoi(env("IMAP_POLL_SECONDS", "60"))
	if e != nil || pollSeconds < 15 || pollSeconds > 300 {
		return errors.New("IMAP_POLL_SECONDS must be 15..300")
	}
	upstream, e := strconv.Atoi(env("MAX_UPSTREAM_REQUESTS", "128"))
	if e != nil || upstream < 1 || upstream > 512 {
		return errors.New("invalid MAX_UPSTREAM_REQUESTS")
	}
	transfers, e := strconv.Atoi(env("MAX_TRANSFERS", "8"))
	if e != nil || transfers < 1 || transfers > 32 {
		return errors.New("invalid MAX_TRANSFERS")
	}
	metadataMB, e := strconv.Atoi(env("SESSION_METADATA_MIB", "3072"))
	if e != nil || metadataMB < 64 || metadataMB > 8192 {
		return errors.New("invalid SESSION_METADATA_MIB")
	}
	api := &bridge{base: base, key: key, http: &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }, Transport: &http.Transport{MaxIdleConns: upstream, MaxIdleConnsPerHost: upstream, MaxConnsPerHost: upstream, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}}}
	api.scale = newScaleControl(time.Duration(pollSeconds)*time.Second, upstream, transfers, int64(metadataMB)<<20)
	max, e := strconv.Atoi(env("MAX_CONNECTIONS", "4000"))
	if e != nil || max < 1 {
		return errors.New("invalid MAX_CONNECTIONS")
	}
	perIP, e := strconv.Atoi(env("MAX_CONNECTIONS_PER_IP", "4000"))
	if e != nil || perIP < 1 {
		return errors.New("invalid MAX_CONNECTIONS_PER_IP")
	}
	perMinute, e := strconv.Atoi(env("MAX_NEW_CONNECTIONS_PER_IP_MINUTE", "6000"))
	if e != nil || perMinute < 1 {
		return errors.New("invalid connection rate")
	}
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	listen := func(address string) (net.Listener, error) {
		l, e := net.Listen("tcp", address)
		if e != nil {
			return nil, e
		}
		limited := &limitListener{Listener: l, max: max, perIP: perIP, perMinute: perMinute, byIP: map[string]int{}, recent: map[string]window{}}
		listeners = append(listeners, limited)
		return tls.NewListener(limited, tlsConfig), nil
	}
	failures := make(chan error, 3)
	im := imapserver.New(&imapserver.Options{NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &imapSession{api: api}, nil, nil
	}, TLSConfig: tlsConfig})
	defer im.Close()
	il, e := listen(env("IMAP_LISTEN", ":1993"))
	if e != nil {
		return e
	}
	go func() { failures <- im.Serve(il) }()
	sm := smtp.NewServer(&smtpBackend{api})
	sm.Domain = env("MAIL_HOSTNAME", "localhost")
	sm.AllowInsecureAuth = false
	sm.ReadTimeout = 60 * time.Second
	sm.WriteTimeout = 60 * time.Second
	sm.MaxMessageBytes = 5 * 1024 * 1024
	sm.MaxRecipients = 50
	sm.TLSConfig = tlsConfig
	defer sm.Close()
	sl, e := listen(env("SMTP_LISTEN", ":1465"))
	if e != nil {
		return e
	}
	go func() { failures <- sm.Serve(sl) }()
	if env("ENABLE_POP3", "false") == "true" {
		pl, e := listen(env("POP3_LISTEN", ":1995"))
		if e != nil {
			return e
		}
		pop := &popServer{api: api, locked: map[string]bool{}}
		go func() {
			for {
				c, e := pl.Accept()
				if e != nil {
					failures <- e
					return
				}
				go pop.serve(c)
			}
		}()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		api.scale.metadataMu.Lock()
		metadata := api.scale.metadata
		api.scale.metadataMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"heap_bytes": mem.HeapAlloc, "runtime_bytes": mem.Sys, "goroutines": runtime.NumGoroutine(), "upstream_active": len(api.scale.api), "transfer_active": len(api.scale.heavy), "snapshot_loads_active": len(api.scale.scans), "session_metadata_bytes": metadata, "snapshot_cache_bytes": api.scale.snapshots.size(), "upstream_requests": api.scale.requests.Load(), "upstream_failures": api.scale.failures.Load(), "capacity_rejections": api.scale.rejected.Load()})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok")) })
	health := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer health.Close()
	go func() { failures <- health.ListenAndServe() }()
	log.Print("TLS mail listeners started; Cloudflare remains the mailbox and delivery backend")
	stop, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	select {
	case <-stop.Done():
		return nil
	case e := <-failures:
		return e
	}
}
