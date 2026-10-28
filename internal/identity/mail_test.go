package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDevMailboxIsNewestFirstAndPerAddress(t *testing.T) {
	ctx := context.Background()
	box := &DevMailbox{}
	box.Add(ctx, MailMessage{To: "A@x.co", Subject: "one"})
	box.Add(ctx, MailMessage{To: "b@x.co", Subject: "other"})
	box.Add(ctx, MailMessage{To: "a@x.co", Subject: "two"})
	got, _ := box.For(ctx, "a@X.co")
	if len(got) != 2 || got[0].Subject != "two" || got[1].Subject != "one" {
		t.Fatalf("want newest first for one address, got %+v", got)
	}
	for i := 0; i < 250; i++ {
		box.Add(ctx, MailMessage{To: "c@x.co"})
	}
	if cs, _ := box.For(ctx, "c@x.co"); len(cs) > 200 {
		t.Fatalf("the mailbox keeps at most 200 messages, has %d", len(cs))
	}
}

func TestRecordingMailerRecordsThenForwards(t *testing.T) {
	box := &DevMailbox{}
	var sent []string
	inner := mailerFunc(func(_ context.Context, m MailMessage) error { sent = append(sent, m.Subject); return nil })
	if err := (RecordingMailer{Inner: inner, Mailbox: box}).Send(context.Background(), MailMessage{To: "a@x.co", Subject: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := box.For(context.Background(), "a@x.co"); len(sent) != 1 || len(got) != 1 {
		t.Fatal("the message must reach both the real mailer and the dev mailbox")
	}
}

type mailerFunc func(context.Context, MailMessage) error

func (f mailerFunc) Send(ctx context.Context, m MailMessage) error { return f(ctx, m) }

func TestClientKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := ClientKey(r, false); got != "10.1.2.3" {
		t.Errorf("without trusting the proxy the connection address counts, got %s", got)
	}
	if got := ClientKey(r, true); got != "203.0.113.9" {
		t.Errorf("trusting the proxy the first hop counts, got %s", got)
	}
	r.RemoteAddr = "garbage"
	r.Header.Del("X-Forwarded-For")
	if got := ClientKey(r, true); got != "unknown" {
		t.Errorf("no usable address is 'unknown', got %s", got)
	}
}

func TestEmailValidation(t *testing.T) {
	for email, ok := range map[string]bool{"ana@example.com": true, "a@b.co": true, "": false, "nope": false, "a b@c.co": false, "Ana <ana@example.com>": false} {
		if isEmail(email) != ok {
			t.Errorf("isEmail(%q) = %v, want %v", email, !ok, ok)
		}
	}
}
