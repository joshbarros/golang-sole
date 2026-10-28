package identity

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MailMessage is one email the sign-in flows send. Link is the URL inside it (for tests and the dev mailbox).
type MailMessage struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Link    string `json:"link,omitempty"`
	seq     int64
}

var mailSeq atomic.Int64

// Mailer sends the emails the sign-in flows need: confirm an address, reset a password.
type Mailer interface {
	Send(ctx context.Context, m MailMessage) error
}

// LogMailer writes the message to the log. It is the default when no SMTP host is configured, so nothing leaves the
// machine by accident.
type LogMailer struct{ Log *slog.Logger }

// Send implements Mailer.
func (l LogMailer) Send(_ context.Context, m MailMessage) error {
	l.Log.Info("mail", "to", m.To, "subject", m.Subject)
	return nil
}

// SMTPMailer sends through an SMTP server, upgrading with STARTTLS when asked.
type SMTPMailer struct {
	Host, User, Password, From string
	Port                       int
	StartTLS                   bool
}

// Send implements Mailer.
func (s SMTPMailer) Send(ctx context.Context, m MailMessage) error {
	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if s.StartTLS {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Password, s.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.From); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + s.From + "\r\nTo: " + m.To + "\r\nSubject: " + strings.NewReplacer("\r", " ", "\n", " ").Replace(m.Subject) +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + m.Body
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Mailbox keeps the most recent messages so a browser test or a demo can read them. For demos and tests only.
type Mailbox interface {
	Add(ctx context.Context, m MailMessage) error
	// For returns the messages sent to one address, newest first.
	For(ctx context.Context, to string) ([]MailMessage, error)
}

// DevMailbox is an in-memory Mailbox (one process).
type DevMailbox struct {
	mu       sync.Mutex
	messages []MailMessage
}

// Add records a message.
func (d *DevMailbox) Add(_ context.Context, m MailMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	m.seq = mailSeq.Add(1)
	d.messages = append(d.messages, m)
	if len(d.messages) > 200 {
		d.messages = d.messages[len(d.messages)-200:]
	}
	return nil
}

// For implements Mailbox.
func (d *DevMailbox) For(_ context.Context, to string) ([]MailMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []MailMessage{}
	for _, m := range d.messages {
		if strings.EqualFold(m.To, to) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out, nil
}

// PGMailbox is a Mailbox in Postgres, so every API replica sees the same messages (a request for the mailbox can land on a
// different replica than the one that sent the mail).
type PGMailbox struct{ Pool *pgxpool.Pool }

// Add records a message and forgets all but the newest 200.
func (p PGMailbox) Add(ctx context.Context, m MailMessage) error {
	if _, err := p.Pool.Exec(ctx, `INSERT INTO identity.dev_mailbox (to_address, subject, body, link) VALUES ($1,$2,$3,$4)`, m.To, m.Subject, m.Body, m.Link); err != nil {
		return err
	}
	_, err := p.Pool.Exec(ctx, `DELETE FROM identity.dev_mailbox WHERE id < (SELECT max(id) FROM identity.dev_mailbox) - 200`)
	return err
}

// For implements Mailbox.
func (p PGMailbox) For(ctx context.Context, to string) ([]MailMessage, error) {
	rows, err := p.Pool.Query(ctx, `SELECT to_address, subject, body, link FROM identity.dev_mailbox WHERE lower(to_address) = lower($1) ORDER BY id DESC`, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailMessage{}
	for rows.Next() {
		var m MailMessage
		if err := rows.Scan(&m.To, &m.Subject, &m.Body, &m.Link); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordingMailer records every message in the mailbox, then hands it to the real mailer.
type RecordingMailer struct {
	Inner   Mailer
	Mailbox Mailbox
}

// Send implements Mailer.
func (r RecordingMailer) Send(ctx context.Context, m MailMessage) error {
	if err := r.Mailbox.Add(ctx, m); err != nil {
		return err
	}
	return r.Inner.Send(ctx, m)
}
