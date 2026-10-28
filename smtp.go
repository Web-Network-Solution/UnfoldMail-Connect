package main

import (
	"encoding/json"
	"errors"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"io"
	"net/mail"
	"strings"
)

type smtpBackend struct{ api *bridge }

func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{api: b.api}, nil
}

type smtpSession struct {
	api           *bridge
	c             credential
	from          string
	to            []string
	authenticated bool
}

func (s *smtpSession) AuthMechanisms() []string { return []string{"PLAIN", "LOGIN"} }
func (s *smtpSession) authenticate(user, pass string) error {
	if len(user) > 254 || len(pass) > 512 {
		return smtp.ErrAuthFailed
	}
	c := credential{strings.ToLower(user), pass, "smtp"}
	if e := s.api.login(c); e != nil {
		return smtp.ErrAuthFailed
	}
	s.c = c
	s.authenticated = true
	return nil
}
func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case "LOGIN":
		return &loginServer{verify: s.authenticate}, nil
	case "PLAIN":
		return sasl.NewPlainServer(func(identity, user, pass string) error {
			if identity != "" && identity != user {
				return smtp.ErrAuthFailed
			}
			return s.authenticate(user, pass)
		}), nil
	default:
		return nil, smtp.ErrAuthUnsupported
	}
}

// Legacy Outlook clients may select LOGIN. It is still allowed only inside TLS.
type loginServer struct {
	step   int
	user   string
	verify func(string, string) error
}

func (s *loginServer) Next(response []byte) ([]byte, bool, error) {
	if len(response) > 512 {
		return nil, true, smtp.ErrAuthFailed
	}
	switch s.step {
	case 0:
		if response == nil {
			s.step = 1
			return []byte("Username:"), false, nil
		}
		s.user = string(response)
		s.step = 2
		return []byte("Password:"), false, nil
	case 1:
		s.user = string(response)
		s.step = 2
		return []byte("Password:"), false, nil
	case 2:
		s.step = 3
		return nil, true, s.verify(s.user, string(response))
	default:
		return nil, true, smtp.ErrAuthFailed
	}
}
func (s *smtpSession) Reset()        { s.from = ""; s.to = nil }
func (s *smtpSession) Logout() error { s.c.Password = ""; return nil }
func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authenticated {
		return smtp.ErrAuthRequired
	}
	s.Reset()
	if !strings.EqualFold(from, s.c.Email) {
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Sender must match authenticated mailbox"}
	}
	s.from = from
	return nil
}
func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.authenticated || s.from == "" {
		return smtp.ErrAuthRequired
	}
	a, e := mail.ParseAddress(to)
	if e != nil || a.Address != to || len(s.to) >= 50 {
		return &smtp.SMTPError{Code: 553, Message: "Invalid recipient or recipient limit exceeded"}
	}
	s.to = append(s.to, to)
	return nil
}
func (s *smtpSession) Data(r io.Reader) error {
	if !s.authenticated || s.from == "" || len(s.to) == 0 {
		return smtp.ErrAuthRequired
	}
	release, e := s.api.transferSlot()
	if e != nil {
		return &smtp.SMTPError{Code: 451, Message: "Server busy; retry shortly"}
	}
	defer release()
	raw, e := io.ReadAll(io.LimitReader(r, 5*1024*1024+1))
	if e != nil {
		return e
	}
	if len(raw) > 5*1024*1024 {
		return &smtp.SMTPError{Code: 552, Message: "Message exceeds 5 MiB"}
	}
	recipients, _ := json.Marshal(s.to)
	_, e = s.api.request(s.c, "POST", "/submit", raw, map[string]string{"Content-Type": "message/rfc822", "X-Mail-Recipients": string(recipients)})
	if e != nil {
		var rejected *apiError
		if errors.As(e, &rejected) && (rejected.Status == 400 || rejected.Status == 409) {
			if rejected.Status == 409 {
				return &smtp.SMTPError{Code: 554, Message: "Delivery is uncertain; check with the recipient before retrying"}
			}
			return &smtp.SMTPError{Code: 554, Message: "Message rejected; check sender, recipients, Reply-To and message size"}
		}
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Submission could not be confirmed; retry the same message later"}
	}
	return nil
}
