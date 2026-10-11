// Package mail is the outbound email transport (SMTP via the standard library).
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/gochathub/gochathub-server/internal/service"
)

// SMTP sends plain-text mail. TLS is "starttls" (default), "implicit" or "none".
type SMTP struct {
	Host, Port, User, Pass, From, TLS string
}

// Send delivers one message; ctx bounds the whole exchange.
func (s *SMTP) Send(ctx context.Context, m service.Mail) error {
	for _, v := range []string{m.To, m.Subject, s.From} {
		if strings.ContainsAny(v, "\r\n") {
			return errors.New("mail header contains a line break")
		}
	}
	addr := net.JoinHostPort(s.Host, s.Port)
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	if s.TLS == "implicit" {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp greet: %w", err)
	}
	defer c.Close()
	if s.TLS == "starttls" || s.TLS == "" {
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if s.User != "" {
		if s.TLS == "none" {
			return errors.New("smtp auth refused over plaintext (SMTP_TLS=none)")
		}
		if err := c.Auth(plainAuth{s.User, s.Pass}); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	msg := "From: " + s.From + "\r\nTo: " + m.To + "\r\nSubject: " + m.Subject +
		"\r\nDate: " + time.Now().Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data close: %w", err)
	}
	return c.Quit()
}

// plainAuth is smtp.PlainAuth without its localhost/TLS check: the caller has
// already negotiated TLS, and "none" is a deliberate dev-only choice.
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

// Enabled: a constructed sender is always usable (MailSettings gates the feature).
func (s *SMTP) Enabled(context.Context) bool { return true }
