package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

type popServer struct {
	api              *bridge
	mu               sync.Mutex
	locked           map[string]bool
	handshakeTimeout time.Duration
}

func (s *popServer) serve(c net.Conn) {
	defer c.Close()
	timeout := s.handshakeTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return
	}
	if secure, ok := c.(*tls.Conn); ok {
		if err := secure.Handshake(); err != nil {
			return
		}
	}
	reader := bufio.NewReaderSize(c, 4096)
	w := textproto.NewWriter(bufio.NewWriter(c))
	reply := func(ok bool, msg string) error {
		prefix := "-ERR "
		if ok {
			prefix = "+OK "
		}
		return w.PrintfLine("%s%s", prefix, msg)
	}
	if err := reply(true, "Mail protocol ready"); err != nil {
		return
	}
	var cred credential
	var email string
	var rows []messageInfo
	deleted := map[int]bool{}
	sizes := map[int]int{}
	authed := false
	failures := 0
	defer func() {
		if authed {
			s.mu.Lock()
			delete(s.locked, cred.Email)
			s.mu.Unlock()
		}
	}()
	multiline := func(lines []string) {
		reply(true, "")
		d := w.DotWriter()
		for _, line := range lines {
			fmt.Fprintln(d, line)
		}
		d.Close()
	}
	messageIndex := func(arg string) (int, bool) {
		n, e := strconv.Atoi(arg)
		return n - 1, e == nil && n > 0 && n <= len(rows) && !deleted[n-1]
	}
	size := func(i int) (int, error) {
		if rows[i].RawSize != nil {
			return *rows[i].RawSize, nil
		}
		if n, ok := sizes[i]; ok {
			return n, nil
		}
		raw, e := s.api.raw(cred, rows[i].ID, rows[i].UID)
		if e != nil {
			return 0, e
		}
		sizes[i] = len(raw)
		return len(raw), nil
	}
	for {
		c.SetDeadline(time.Now().Add(10 * time.Minute))
		line, e := reader.ReadSlice('\n')
		if e != nil || len(line) > 2048 {
			return
		}
		parts := strings.SplitN(strings.TrimRight(string(line), "\r\n"), " ", 2)
		cmd := strings.ToUpper(parts[0])
		arg := ""
		if len(parts) > 1 {
			arg = parts[1]
		}
		if cmd == "CAPA" {
			multiline([]string{"USER", "UIDL", "TOP", "RESP-CODES", "IMPLEMENTATION mail-protocol"})
			continue
		}
		if cmd == "QUIT" {
			if authed {
				var ids []string
				for i := range deleted {
					ids = append(ids, rows[i].ID)
				}
				if len(ids) > 100 {
					reply(false, "Too many deletions; reset and delete at most 100 per session")
					continue
				}
				if len(ids) > 0 {
					if e = s.api.json(cred, "POST", "/pop-delete", map[string]any{"ids": ids}, nil); e != nil {
						reply(false, "Deletion could not be confirmed")
						return
					}
				}
			}
			reply(true, "Goodbye")
			return
		}
		if !authed {
			switch cmd {
			case "USER":
				email = strings.ToLower(arg)
				reply(true, "Password required")
			case "PASS":
				cred = credential{email, arg, "pop3"}
				if email == "" || s.api.login(cred) != nil {
					failures++
					reply(false, "Authentication failed")
					if failures >= 5 {
						return
					}
					time.Sleep(time.Second)
					continue
				}
				s.mu.Lock()
				busy := s.locked[email]
				if !busy {
					s.locked[email] = true
				}
				s.mu.Unlock()
				if busy {
					reply(false, "Mailbox already in use")
					continue
				}
				authed = true
				fs, err := s.api.folders(cred)
				if err != nil {
					reply(false, "Mailbox unavailable")
					return
				}
				inbox := ""
				for _, f := range fs {
					if f.Type == "inbox" {
						inbox = f.ID
					}
				}
				rows, _, err = s.api.messages(cred, inbox)
				if err != nil || len(rows) > 5000 {
					reply(false, "Mailbox unavailable or too large for POP3; use IMAP")
					return
				}
				reply(true, "Mailbox ready")
			default:
				reply(false, "Authenticate first")
			}
			continue
		}
		// Revocation and admin policy are checked even for metadata-only commands.
		if s.api.auth(cred) != nil {
			reply(false, "Access revoked or unavailable")
			return
		}
		switch cmd {
		case "NOOP":
			reply(true, "")
		case "RSET":
			deleted = map[int]bool{}
			reply(true, "Deletion marks cleared")
		case "STAT":
			count, total := 0, 0
			failed := false
			for i := range rows {
				if !deleted[i] {
					n, err := size(i)
					if err != nil {
						failed = true
						break
					}
					count++
					total += n
				}
			}
			if failed {
				reply(false, "Message unavailable")
			} else {
				reply(true, fmt.Sprintf("%d %d", count, total))
			}
		case "LIST", "UIDL":
			if arg != "" {
				i, ok := messageIndex(arg)
				if !ok {
					reply(false, "No such message")
					continue
				}
				if cmd == "UIDL" {
					reply(true, fmt.Sprintf("%d %s", i+1, rows[i].ID))
				} else {
					n, err := size(i)
					if err != nil {
						reply(false, "Message unavailable")
					} else {
						reply(true, fmt.Sprintf("%d %d", i+1, n))
					}
				}
			} else {
				var lines []string
				failed := false
				for i, r := range rows {
					if deleted[i] {
						continue
					}
					if cmd == "UIDL" {
						lines = append(lines, fmt.Sprintf("%d %s", i+1, r.ID))
					} else {
						n, err := size(i)
						if err != nil {
							failed = true
							break
						}
						lines = append(lines, fmt.Sprintf("%d %d", i+1, n))
					}
				}
				if failed {
					reply(false, "Message unavailable")
				} else {
					multiline(lines)
				}
			}
		case "DELE":
			i, ok := messageIndex(arg)
			if !ok {
				reply(false, "No such message")
			} else if len(deleted) >= 100 {
				reply(false, "At most 100 deletions per session")
			} else {
				deleted[i] = true
				reply(true, "Marked for deletion on QUIT")
			}
		case "RETR", "TOP":
			args := strings.Fields(arg)
			if len(args) == 0 {
				reply(false, "Message required")
				continue
			}
			i, ok := messageIndex(args[0])
			if !ok {
				reply(false, "No such message")
				continue
			}
			raw, err := s.api.raw(cred, rows[i].ID, rows[i].UID)
			if err != nil {
				reply(false, "Message unavailable")
				continue
			}
			if cmd == "TOP" {
				if len(args) != 2 {
					reply(false, "Line count required")
					continue
				}
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 0 {
					reply(false, "Invalid line count")
					continue
				}
				split := bytes.SplitN(raw, []byte("\r\n\r\n"), 2)
				if len(split) == 2 {
					lines := bytes.Split(split[1], []byte("\r\n"))
					if n < len(lines) {
						lines = lines[:n]
					}
					raw = append(append(append([]byte{}, split[0]...), []byte("\r\n\r\n")...), bytes.Join(lines, []byte("\r\n"))...)
				}
			}
			reply(true, "")
			d := w.DotWriter()
			io.Copy(d, bytes.NewReader(raw))
			if d.Close() != nil {
				return
			}
		default:
			reply(false, "Unsupported command")
		}
	}
}
