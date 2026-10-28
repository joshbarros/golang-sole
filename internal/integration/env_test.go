// Package integration runs the real API (every module, the real middleware chain) against a real Postgres. Brokers are not
// needed: drain() plays their part by handing each outbox row to the handlers bound to its routing key.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-sole/internal/app"
	"github.com/joshbarros/golang-sole/internal/platform/config"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
	"github.com/joshbarros/golang-sole/internal/testutil"
)

const (
	adminEmail    = "admin@test.local"
	adminPassword = "Admin-Passw0rd1"
	password      = "Passw0rd-Test1"
)

var emails atomic.Int64

func newEmail() string {
	return fmt.Sprintf("user-%d-%s@test.local", emails.Add(1), uuid.NewString()[:8])
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	app  *app.App
	srv  *httptest.Server
	cfg  config.Config

	redeliverLater []messaging.Delivery
}

type option func(*config.Config)

func withConfirmedEmail() option { return func(c *config.Config) { c.RequireConfirmedEmail = true } }
func withRateLimit(n int) option {
	return func(c *config.Config) { c.AuthPerMinute = n; c.ForwardedHeaders = true }
}
func withDevMailbox(on bool) option {
	return func(c *config.Config) { c.MailDevMailbx = on }
}
func withRSA(pem string) option { return func(c *config.Config) { c.JWTRSAPrivateKeyPEM = pem } }

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	pool := testutil.Reset(t)
	cfg := config.Config{
		JWTKey: "integration-test-signing-key-0123456789abcdef", JWTKeyID: "it-1", JWTIssuer: "sole-api", JWTAudience: "sole-web",
		JWTAccessMinutes: 15, JWTRefreshDays: 14, RevocationCacheSeconds: 0,
		RequireConfirmedEmail: false, PublicURL: "http://web.test", AppName: "Sole", MailDevMailbx: true,
		AuthPerMinute: 100000, MessagingEnabled: false, DrawInterval: time.Hour,
	}
	for _, o := range opts {
		o(&cfg)
	}
	log := slog.New(slog.DiscardHandler)
	a, err := app.New(cfg, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Identity.Seed(context.Background(), adminEmail, adminPassword); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, pool: pool, app: a, srv: srv, cfg: cfg}
}

// reply is a decoded HTTP response.
type reply struct {
	Status  int
	Header  http.Header
	Cookies []*http.Cookie
	Body    map[string]any
	Raw     []byte
}

func (r reply) str(key string) string {
	v, _ := r.Body[key].(string)
	return v
}

func (r reply) obj(key string) map[string]any {
	v, _ := r.Body[key].(map[string]any)
	return v
}

type call struct {
	method, path string
	body         any
	token        string
	headers      map[string]string
	cookie       string
}

func (e *env) do(c call) reply {
	e.t.Helper()
	var rdr io.Reader
	if c.body != nil {
		b, _ := json.Marshal(c.body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(c.method, e.srv.URL+c.path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	if c.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sole_refresh", Value: c.cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{Status: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies(), Raw: raw}
	_ = json.Unmarshal(raw, &r.Body)
	return r
}

func (e *env) get(path, token string) reply {
	return e.do(call{method: "GET", path: path, token: token})
}
func (e *env) post(path string, body any, token string) reply {
	return e.do(call{method: "POST", path: path, body: body, token: token})
}

// session is a signed-in user.
type session struct {
	Token   string
	Refresh string
	ID      uuid.UUID
	Email   string
}

func sessionOf(t *testing.T, r reply, email string) session {
	t.Helper()
	if r.Status != 200 && r.Status != 201 {
		t.Fatalf("expected a session, got %d %s", r.Status, r.Raw)
	}
	s := session{Token: r.str("accessToken"), Email: email}
	s.ID, _ = uuid.Parse(fmt.Sprint(r.obj("user")["id"]))
	for _, c := range r.Cookies {
		if c.Name == "sole_refresh" {
			s.Refresh = c.Value
		}
	}
	return s
}

func (e *env) register() session {
	email := newEmail()
	return sessionOf(e.t, e.post("/api/auth/register", map[string]any{"email": email, "password": password}, ""), email)
}

func (e *env) admin() session {
	return sessionOf(e.t, e.post("/api/auth/login", map[string]any{"email": adminEmail, "password": adminPassword}, ""), adminEmail)
}

func code() string { return "S" + strings.ReplaceAll(uuid.NewString(), "-", "")[:14] }

// openRaffle opens a raffle through the API as the administrator and returns its code.
func (e *env) openRaffle(pairs int, drawIn time.Duration) string {
	e.t.Helper()
	c := code()
	r := e.post("/api/raffles", map[string]any{
		"code": c, "name": "Test Raffle", "colorway": "Volt", "brand": "Nimbus", "priceCents": 5000, "pairsTotal": pairs,
		"drawAt": time.Now().Add(drawIn).UTC().Format(time.RFC3339Nano),
	}, e.admin().Token)
	if r.Status != 201 {
		e.t.Fatalf("open raffle: %d %s", r.Status, r.Raw)
	}
	return c
}

// enter creates an order (an entry) for the user.
func (e *env) enter(s session, raffle, reference string, pairs int) reply {
	return e.post("/api/orders", map[string]any{"raffleCode": raffle, "reference": reference, "pairs": pairs}, s.Token)
}

func (e *env) order(s session, id string) reply { return e.get("/api/orders/"+id, s.Token) }

// drawNow makes a raffle due and draws it.
func (e *env) drawNow(raffleCode string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE raffles.raffles SET draw_at = now() - interval '1 second' WHERE code = $1`, raffleCode); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.app.Raffles.DrawDue(context.Background(), time.Now()); err != nil {
		e.t.Fatal(err)
	}
}

// bindings: which handler gets which routing key (the queue bindings the broker would apply).
func (e *env) deliver(key string, d messaging.Delivery) (poison bool, err error) {
	handlers := map[string][]messaging.Handler{
		messaging.ReserveKey:  {e.app.Raffles},
		messaging.ReleaseKey:  {e.app.Raffles},
		messaging.HeldKey:     {e.app.Claims, e.app.Orders},
		messaging.ShortKey:    {e.app.Orders},
		messaging.EnteredKey:  {e.app.Orders},
		messaging.IssuedKey:   {e.app.Orders},
		messaging.RejectedKey: {e.app.Orders},
		messaging.ReleasedKey: {e.app.Orders},
	}
	for _, h := range handlers[key] {
		if herr := h.Handle(context.Background(), d); herr != nil {
			if errors.Is(herr, messaging.ErrPoison) {
				poison = true
				continue
			}
			err = herr
		}
	}
	return poison, err
}

type outboxRow struct {
	schema string
	id     int64
	d      messaging.Delivery
}

func (e *env) unsent() []outboxRow {
	var rows []outboxRow
	for _, schema := range []string{"orders", "raffles", "claims"} {
		r, err := e.pool.Query(context.Background(), fmt.Sprintf(`SELECT id, message_id, routing_key, payload FROM %s.outbox_messages WHERE sent_at IS NULL ORDER BY id`, schema))
		if err != nil {
			e.t.Fatal(err)
		}
		for r.Next() {
			var row outboxRow
			row.schema = schema
			if err := r.Scan(&row.id, &row.d.MessageID, &row.d.RoutingKey, &row.d.Body); err != nil {
				e.t.Fatal(err)
			}
			rows = append(rows, row)
		}
		r.Close()
	}
	return rows
}

func (e *env) markSent(row outboxRow) {
	if _, err := e.pool.Exec(context.Background(), fmt.Sprintf(`UPDATE %s.outbox_messages SET sent_at = now() WHERE id = $1`, row.schema), row.id); err != nil {
		e.t.Fatal(err)
	}
}

// drain plays the broker: it hands every unsent outbox row to its handlers until nothing is left. A message whose handler
// asks to be retried (out of order) goes to the back of the line. It returns how many messages were dead-lettered.
func (e *env) drain() (deadLettered int) {
	e.t.Helper()
	for round := 0; round < 200; round++ {
		rows := e.unsent()
		if len(rows) == 0 {
			return deadLettered
		}
		for _, row := range rows {
			e.markSent(row)
			poison, err := e.deliver(row.d.RoutingKey, row.d)
			if poison {
				deadLettered++
			}
			if err != nil {
				// requeue: a fresh outbox row with the same content would change ids, so just redeliver after the others
				e.redeliverLater = append(e.redeliverLater, row.d)
			}
		}
		for _, d := range e.redeliverLater {
			_, _ = e.deliver(d.RoutingKey, d)
		}
		e.redeliverLater = nil
	}
	e.t.Fatal("drain did not settle")
	return
}

func jsonInto(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func errorsAs(err error, target any) bool { return errors.As(err, target) }
