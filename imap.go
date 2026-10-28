package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	msgtext "github.com/emersion/go-message/textproto"
	"io"
	"net/url"
	"reflect"
	"strings"
	"time"
)

func randomID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

var permanent = []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft}

func asFlags(a []string) []imap.Flag {
	out := make([]imap.Flag, len(a))
	for i, v := range a {
		out[i] = imap.Flag(v)
	}
	return out
}
func stringsFlags(a []imap.Flag) []string {
	out := make([]string, len(a))
	for i, v := range a {
		out[i] = string(v)
	}
	return out
}
func folderValidity(f folder) uint32 {
	digest := sha256.Sum256([]byte(f.ID))
	n := binary.BigEndian.Uint32(digest[:4])
	if n == 0 {
		return 1
	}
	return n
}
func folderName(f folder) string {
	if f.Type == "inbox" {
		return "INBOX"
	}
	return f.Name
}

type imapSession struct {
	api        *bridge
	c          credential
	folder     *folder
	rows       []messageInfo
	readOnly   bool
	revision   int64
	sharedRows bool
	reserved   int64
}

var _ imapserver.Session = (*imapSession)(nil)

func (s *imapSession) Close() error {
	s.c.Password = ""
	s.rows = nil
	return s.api.metadataBudget(&s.reserved, 0)
}
func (s *imapSession) ownRows() {
	if s.sharedRows {
		s.rows = append([]messageInfo(nil), s.rows...)
		s.sharedRows = false
	}
}
func (s *imapSession) Login(user, password string) error {
	c := credential{strings.ToLower(user), password, "imap"}
	if e := s.api.login(c); e != nil {
		return imapserver.ErrAuthFailed
	}
	s.c = c
	return nil
}
func (s *imapSession) AppendLimit() uint32 { return maxMessage }
func (s *imapSession) find(name string) (folder, error) {
	fs, e := s.api.folders(s.c)
	if e != nil {
		return folder{}, e
	}
	for _, f := range fs {
		if folderName(f) == name || (f.Type == "inbox" && strings.EqualFold(name, "INBOX")) {
			return f, nil
		}
	}
	return folder{}, errors.New("no such mailbox")
}
func (s *imapSession) Select(name string, o *imap.SelectOptions) (*imap.SelectData, error) {
	f, e := s.find(name)
	if e != nil {
		return nil, e
	}
	rows, next, revision, e := s.api.snapshot(s.c, f.ID)
	if e != nil {
		return nil, e
	}
	if e = s.api.metadataBudget(&s.reserved, rowsBytes(rows)); e != nil {
		return nil, e
	}
	s.sharedRows = s.api.scale != nil
	s.revision = revision
	s.folder = &f
	s.rows = rows
	s.readOnly = o.ReadOnly
	var unseen uint32
	for i, r := range rows {
		if !hasFlag(r.Flags, "\\Seen") {
			unseen = uint32(i + 1)
			break
		}
	}
	flags := permanent
	if o.ReadOnly {
		flags = nil
	}
	return &imap.SelectData{Flags: permanent, PermanentFlags: flags, NumMessages: uint32(len(rows)), UIDNext: imap.UID(next), UIDValidity: folderValidity(f), FirstUnseenSeqNum: unseen}, nil
}
func (s *imapSession) Unselect() error {
	s.folder = nil
	s.rows = nil
	return s.api.metadataBudget(&s.reserved, 0)
}
func (s *imapSession) Create(name string, _ *imap.CreateOptions) error {
	return s.api.json(s.c, "POST", "/folders", map[string]string{"name": name}, nil)
}
func (s *imapSession) Delete(name string) error {
	f, e := s.find(name)
	if e != nil {
		return e
	}
	return s.api.json(s.c, "DELETE", "/folders/"+url.PathEscape(f.ID), nil, nil)
}
func (s *imapSession) Rename(name, newName string, _ *imap.RenameOptions) error {
	f, e := s.find(name)
	if e != nil {
		return e
	}
	return s.api.json(s.c, "PATCH", "/folders/"+url.PathEscape(f.ID), map[string]string{"name": newName}, nil)
}
func (s *imapSession) subscription(name string, v bool) error {
	f, e := s.find(name)
	if e != nil {
		return e
	}
	return s.api.json(s.c, "PATCH", "/folders/"+url.PathEscape(f.ID), map[string]bool{"subscribed": v}, nil)
}
func (s *imapSession) Subscribe(name string) error   { return s.subscription(name, true) }
func (s *imapSession) Unsubscribe(name string) error { return s.subscription(name, false) }
func (s *imapSession) List(w *imapserver.ListWriter, ref string, patterns []string, o *imap.ListOptions) error {
	fs, e := s.api.folders(s.c)
	if e != nil {
		return e
	}
	for _, f := range fs {
		if o.SelectSubscribed && f.Subscribed == 0 {
			continue
		}
		for _, pattern := range patterns {
			if imapserver.MatchList(folderName(f), 0, ref, pattern) {
				attrs := []imap.MailboxAttr{imap.MailboxAttrNoInferiors}
				if f.Subscribed != 0 {
					attrs = append(attrs, imap.MailboxAttrSubscribed)
				}
				if e = w.WriteList(&imap.ListData{Mailbox: folderName(f), Attrs: attrs}); e != nil {
					return e
				}
				break
			}
		}
	}
	return nil
}
func (s *imapSession) Status(name string, o *imap.StatusOptions) (*imap.StatusData, error) {
	f, e := s.find(name)
	if e != nil {
		return nil, e
	}
	rows, next, e := s.api.messages(s.c, f.ID)
	if e != nil {
		return nil, e
	}
	n, unseen, deleted, recent := uint32(len(rows)), uint32(0), uint32(0), uint32(0)
	for _, r := range rows {
		if !hasFlag(r.Flags, "\\Seen") {
			unseen++
		}
		if hasFlag(r.Flags, "\\Deleted") {
			deleted++
		}
	}
	d := &imap.StatusData{Mailbox: name}
	if o.NumMessages {
		d.NumMessages = &n
	}
	if o.NumUnseen {
		d.NumUnseen = &unseen
	}
	if o.NumDeleted {
		d.NumDeleted = &deleted
	}
	if o.NumRecent {
		d.NumRecent = &recent
	}
	if o.UIDNext {
		d.UIDNext = imap.UID(next)
	}
	if o.UIDValidity {
		d.UIDValidity = folderValidity(f)
	}
	return d, nil
}
func (s *imapSession) Append(name string, r imap.LiteralReader, o *imap.AppendOptions) (*imap.AppendData, error) {
	release, e := s.api.transferSlot()
	if e != nil {
		return nil, e
	}
	defer release()
	return s.appendMessage(name, r, o)
}
func (s *imapSession) appendMessage(name string, r imap.LiteralReader, o *imap.AppendOptions) (*imap.AppendData, error) {
	f, e := s.find(name)
	if e != nil {
		return nil, e
	}
	raw, e := io.ReadAll(io.LimitReader(r, maxMessage+1))
	if e != nil {
		return nil, e
	}
	if len(raw) > maxMessage {
		return nil, errors.New("message too large")
	}
	date := o.Time
	if date.IsZero() {
		date = time.Now()
	}
	_, e = s.api.request(s.c, "POST", "/append?folder="+url.QueryEscape(f.ID), raw, map[string]string{"Content-Type": "message/rfc822", "X-Mail-Flags": strings.Join(stringsFlags(o.Flags), ","), "X-Mail-Date": date.UTC().Format(time.RFC3339Nano), "X-Request-ID": randomID()})
	return &imap.AppendData{}, e
}
func (s *imapSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.folder == nil {
		return s.api.auth(s.c)
	}
	revision, e := s.api.revision(s.c)
	if e != nil {
		return e
	}
	if revision == s.revision {
		return nil
	}
	fresh, revision, e := s.api.changedRows(s.c, s.folder.ID, s.revision, s.rows)
	if e != nil {
		return e
	}
	if e = s.api.metadataBudget(&s.reserved, rowsBytes(s.rows)+rowsBytes(fresh)); e != nil {
		return e
	}
	defer func() { _ = s.api.metadataBudget(&s.reserved, rowsBytes(s.rows)) }()
	s.ownRows()
	byUID := map[uint32]messageInfo{}
	for _, r := range fresh {
		byUID[r.UID] = r
	}
	if allowExpunge {
		for i := len(s.rows) - 1; i >= 0; i-- {
			if _, ok := byUID[s.rows[i].UID]; !ok {
				if e = w.WriteExpunge(uint32(i + 1)); e != nil {
					return e
				}
				s.rows = append(s.rows[:i], s.rows[i+1:]...)
			}
		}
	}
	known := map[uint32]bool{}
	for i, r := range s.rows {
		known[r.UID] = true
		if n, ok := byUID[r.UID]; ok {
			if !reflect.DeepEqual(r.Flags, n.Flags) {
				if e = w.WriteMessageFlags(uint32(i+1), imap.UID(r.UID), asFlags(n.Flags)); e != nil {
					return e
				}
			}
			s.rows[i] = n
		}
	}
	grew := false
	for _, r := range fresh {
		if !known[r.UID] {
			s.rows = append(s.rows, r)
			grew = true
		}
	}
	if allowExpunge {
		s.revision = revision
		s.rows = fresh
		s.sharedRows = s.api.scale != nil
	}
	if grew {
		return w.WriteNumMessages(uint32(len(s.rows)))
	}
	return nil
}
func (s *imapSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if e := s.Poll(w, true); e != nil {
		return e
	}
	interval := 30 * time.Second
	if s.api.scale != nil {
		interval = s.api.scale.poll
	}
	// Spread employee/device timers across the polling interval.
	jitter := time.Duration(time.Now().UnixNano() % int64(interval/5+1))
	t := time.NewTicker(interval + jitter)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-t.C:
			if e := s.Poll(w, true); e != nil {
				return e
			}
		}
	}
}
func inRange(n, lo, hi, max uint32) bool {
	if lo == 0 {
		lo = max
	}
	if hi == 0 {
		hi = max
	}
	if lo > hi {
		lo, hi = hi, lo
	}
	return n >= lo && n <= hi
}
func selected(rows []messageInfo, set imap.NumSet) []int {
	var out []int
	if len(rows) == 0 {
		return out
	}
	for i, r := range rows {
		match := false
		switch v := set.(type) {
		case imap.SeqSet:
			for _, x := range v {
				if inRange(uint32(i+1), x.Start, x.Stop, uint32(len(rows))) {
					match = true
				}
			}
		case imap.UIDSet:
			for _, x := range v {
				if inRange(r.UID, uint32(x.Start), uint32(x.Stop), rows[len(rows)-1].UID) {
					match = true
				}
			}
		}
		if match {
			out = append(out, i)
		}
	}
	return out
}
func (s *imapSession) Fetch(w *imapserver.FetchWriter, set imap.NumSet, o *imap.FetchOptions) error {
	if o.Envelope || o.RFC822Size || o.BodyStructure != nil || len(o.BodySection) > 0 {
		release, e := s.api.transferSlot()
		if e != nil {
			return e
		}
		defer release()
	}
	if e := s.api.auth(s.c); e != nil {
		return e
	}
	for _, i := range selected(s.rows, set) {
		r := &s.rows[i]
		seen := false
		for _, b := range o.BodySection {
			if !b.Peek {
				seen = true
			}
		}
		if seen && !s.readOnly && !hasFlag(r.Flags, "\\Seen") {
			s.ownRows()
			r = &s.rows[i]
			flags, e := s.api.modifyFlags(s.c, r.ID, r.UID, []string{"\\Seen"}, "add")
			if e != nil {
				return e
			}
			r.Flags = flags
		}
		need := o.Envelope || o.RFC822Size || o.BodyStructure != nil || len(o.BodySection) > 0
		var raw []byte
		var e error
		if need {
			raw, e = s.api.raw(s.c, r.ID, r.UID)
			if e != nil {
				return e
			}
		}
		m := w.CreateMessage(uint32(i + 1))
		if o.UID {
			m.WriteUID(imap.UID(r.UID))
		}
		if o.Flags || seen {
			m.WriteFlags(asFlags(r.Flags))
		}
		if o.InternalDate {
			m.WriteInternalDate(r.Date)
		}
		if o.RFC822Size {
			m.WriteRFC822Size(int64(len(raw)))
		}
		if o.Envelope {
			h, e := msgtext.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
			if e != nil {
				return e
			}
			m.WriteEnvelope(imapserver.ExtractEnvelope(h))
		}
		if o.BodyStructure != nil {
			m.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
		}
		for _, b := range o.BodySection {
			data := imapserver.ExtractBodySection(bytes.NewReader(raw), b)
			out := m.WriteBodySection(b, int64(len(data)))
			if _, e = out.Write(data); e != nil {
				return e
			}
			if e = out.Close(); e != nil {
				return e
			}
		}
		if e = m.Close(); e != nil {
			return e
		}
	}
	return nil
}
func (s *imapSession) Store(w *imapserver.FetchWriter, set imap.NumSet, f *imap.StoreFlags, _ *imap.StoreOptions) error {
	s.ownRows()
	if s.readOnly {
		return errors.New("mailbox is read-only")
	}
	op := "set"
	switch f.Op {
	case imap.StoreFlagsAdd:
		op = "add"
	case imap.StoreFlagsDel:
		op = "remove"
	}
	for _, i := range selected(s.rows, set) {
		r := &s.rows[i]
		flags, e := s.api.modifyFlags(s.c, r.ID, r.UID, stringsFlags(f.Flags), op)
		if e != nil {
			return e
		}
		r.Flags = flags
		if !f.Silent {
			m := w.CreateMessage(uint32(i + 1))
			m.WriteUID(imap.UID(r.UID))
			m.WriteFlags(asFlags(flags))
			if e = m.Close(); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *imapSession) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	s.ownRows()
	if s.readOnly {
		return errors.New("mailbox is read-only")
	}
	for i := len(s.rows) - 1; i >= 0; i-- {
		r := s.rows[i]
		if !hasFlag(r.Flags, "\\Deleted") || (uids != nil && !uids.Contains(imap.UID(r.UID))) {
			continue
		}
		var result struct{ Deleted []string }
		if e := s.api.json(s.c, "POST", "/expunge", map[string]any{"ids": []string{r.ID}, "uids": map[string]uint32{r.ID: r.UID}}, &result); e != nil {
			return e
		}
		if len(result.Deleted) == 0 {
			continue
		}
		if e := w.WriteExpunge(uint32(i + 1)); e != nil {
			return e
		}
		s.rows = append(s.rows[:i], s.rows[i+1:]...)
	}
	return nil
}
func (s *imapSession) Copy(set imap.NumSet, dest string) (*imap.CopyData, error) {
	release, e := s.api.transferSlot()
	if e != nil {
		return nil, e
	}
	defer release()
	for _, i := range selected(s.rows, set) {
		r := s.rows[i]
		raw, e := s.api.raw(s.c, r.ID, r.UID)
		if e != nil {
			return nil, e
		}
		if _, e = s.appendMessage(dest, bytes.NewReader(raw), &imap.AppendOptions{Flags: asFlags(r.Flags), Time: r.Date}); e != nil {
			return nil, e
		}
	}
	return &imap.CopyData{}, nil
}
func (s *imapSession) Search(kind imapserver.NumKind, c *imap.SearchCriteria, _ *imap.SearchOptions) (*imap.SearchData, error) {
	if e := s.api.auth(s.c); e != nil {
		return nil, e
	}
	seqs, uids := imap.SeqSet{}, imap.UIDSet{}
	for i, r := range s.rows {
		var release func()
		var raw []byte
		load := func() ([]byte, error) {
			if raw != nil {
				return raw, nil
			}
			var err error
			if release == nil {
				release, err = s.api.transferSlot()
				if err != nil {
					return nil, err
				}
			}
			raw, err = s.api.raw(s.c, r.ID, r.UID)
			return raw, err
		}
		ok, e := s.matchesLazy(i, r, c, load)
		if release != nil {
			release()
		}
		if e != nil {
			return nil, e
		}
		if ok {
			seqs.AddNum(uint32(i + 1))
			uids.AddNum(imap.UID(r.UID))
		}
	}
	data := &imap.SearchData{All: seqs}
	if kind == imapserver.NumKindUID {
		data.All = uids
	}
	return data, nil
}
func (s *imapSession) matches(i int, r messageInfo, raw []byte, c *imap.SearchCriteria) (bool, error) {
	return s.matchesLazy(i, r, c, func() ([]byte, error) { return raw, nil })
}
func (s *imapSession) matchesLazy(i int, r messageInfo, c *imap.SearchCriteria, load func() ([]byte, error)) (bool, error) {
	for _, set := range c.SeqNum {
		found := false
		for _, span := range set {
			if inRange(uint32(i+1), span.Start, span.Stop, uint32(len(s.rows))) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	for _, set := range c.UID {
		found := false
		for _, span := range set {
			if inRange(r.UID, uint32(span.Start), uint32(span.Stop), s.rows[len(s.rows)-1].UID) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}

	if !dateMatch(r.Date, c.Since, c.Before) {
		return false, nil
	}
	for _, f := range c.Flag {
		if !hasFlag(r.Flags, string(f)) {
			return false, nil
		}
	}
	for _, f := range c.NotFlag {
		if hasFlag(r.Flags, string(f)) {
			return false, nil
		}
	}
	if c.Larger > 0 || c.Smaller > 0 {
		size := r.RawSize
		if size == nil {
			raw, err := load()
			if err != nil {
				return false, err
			}
			n := len(raw)
			size = &n
		}
		if (c.Larger > 0 && int64(*size) <= c.Larger) || (c.Smaller > 0 && int64(*size) >= c.Smaller) {
			return false, nil
		}
	}
	if len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 || !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		raw, err := load()
		if err != nil {
			return false, err
		}
		ent, e := message.Read(bytes.NewReader(raw))
		if e != nil && !message.IsUnknownCharset(e) {
			return false, e
		}
		if ent == nil {
			return false, errUnavailable
		}
		h := mail.Header{Header: ent.Header}
		if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
			date, e := h.Date()
			if e != nil || !dateMatch(date, c.SentSince, c.SentBefore) {
				return false, nil
			}
		}
		for _, wanted := range c.Header {
			matched := false
			fields := ent.Header.FieldsByKey(wanted.Key)
			for fields.Next() {
				value, e := fields.Text()
				if e == nil && strings.Contains(strings.ToLower(value), strings.ToLower(wanted.Value)) {
					matched = true
					break
				}
			}
			if !matched {
				return false, nil
			}
		}
		if len(c.Body) > 0 || len(c.Text) > 0 {
			body, e := decodedBody(ent)
			if e != nil {
				return false, e
			}
			body = strings.ToLower(body)
			for _, wanted := range c.Body {
				if !strings.Contains(body, strings.ToLower(wanted)) {
					return false, nil
				}
			}
			var headers strings.Builder
			for fields := ent.Header.Fields(); fields.Next(); {
				value, _ := fields.Text()
				headers.WriteString(fields.Key() + ": " + value + "\n")
			}
			all := strings.ToLower(headers.String()) + body
			for _, wanted := range c.Text {
				if !strings.Contains(all, strings.ToLower(wanted)) {
					return false, nil
				}
			}
		}
	}
	for j := range c.Not {
		ok, e := s.matchesLazy(i, r, &c.Not[j], load)
		if e != nil || ok {
			return false, e
		}
	}
	for j := range c.Or {
		a, e := s.matchesLazy(i, r, &c.Or[j][0], load)
		if e != nil {
			return false, e
		}
		if a {
			continue
		}
		b, e := s.matchesLazy(i, r, &c.Or[j][1], load)
		if e != nil || (!a && !b) {
			return false, e
		}
	}
	return true, nil
}

func dateMatch(t, since, before time.Time) bool {
	day := func(v time.Time) time.Time { return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC) }
	t = day(t)
	return (since.IsZero() || !t.Before(day(since))) && (before.IsZero() || t.Before(day(before)))
}
func decodedBody(e *message.Entity) (string, error) {
	if mr := e.MultipartReader(); mr != nil {
		var out strings.Builder
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil && !message.IsUnknownCharset(err) {
				return "", err
			}
			if part == nil {
				break
			}
			text, err := decodedBody(part)
			if err != nil {
				return "", err
			}
			out.WriteString(text)
			out.WriteByte('\n')
		}
		return out.String(), nil
	}
	data, err := io.ReadAll(io.LimitReader(e.Body, maxMessage+1))
	if len(data) > maxMessage {
		return "", errors.New("message too large")
	}
	return string(data), err
}
