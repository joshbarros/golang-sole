// Package app wires the modules together: the one place that knows about all of them. Both the binary and the
// integration tests build the API from here, so the tests exercise the real middleware chain and routes.
package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-sole/internal/claims"
	"github.com/joshbarros/golang-sole/internal/identity"
	"github.com/joshbarros/golang-sole/internal/orders"
	"github.com/joshbarros/golang-sole/internal/platform/config"
	"github.com/joshbarros/golang-sole/internal/platform/httpx"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
	"github.com/joshbarros/golang-sole/internal/platform/pgdb"
	"github.com/joshbarros/golang-sole/internal/raffles"
)

type module struct {
	schema string
	files  fs.FS
}

var modules = []module{
	{"identity", identity.Migrations},
	{"orders", orders.Migrations},
	{"raffles", raffles.Migrations},
	{"claims", claims.Migrations},
}

// Migrate applies every module's migrations (each behind an advisory lock, so replicas starting together do it once) and
// seeds the first administrator if one is configured.
func Migrate(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) error {
	for _, m := range modules {
		if err := pgdb.Migrate(ctx, pool, m.schema, m.files, "migrations"); err != nil {
			return err
		}
	}
	svc, _, err := newIdentity(pool, cfg, log)
	if err != nil {
		return err
	}
	created, err := svc.Seed(ctx, cfg.AdminEmail, cfg.AdminPassword)
	if err != nil {
		return fmt.Errorf("the seeded administrator could not be created: %w", err)
	}
	if created {
		log.Info("seeded administrator", "email", cfg.AdminEmail)
	} else if cfg.AdminEmail == "" {
		if any, _ := svc.Store.AnyAdmin(ctx); !any {
			log.Warn("no administrator exists and ADMIN_EMAIL is not configured; nobody can manage the catalog")
		}
	}
	return nil
}

func newIdentity(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) (*identity.Service, identity.Mailbox, error) {
	tokens, err := identity.NewTokenService(identity.TokenConfig{
		Key: cfg.JWTKey, RSAPrivatePEM: cfg.JWTRSAPrivateKeyPEM, KeyID: cfg.JWTKeyID, Issuer: cfg.JWTIssuer, Audience: cfg.JWTAudience,
		AccessMinutes: cfg.JWTAccessMinutes, RefreshDays: cfg.JWTRefreshDays,
	})
	if err != nil {
		return nil, nil, err
	}
	var mailer identity.Mailer = identity.LogMailer{Log: log}
	if cfg.MailHost != "" {
		mailer = identity.SMTPMailer{Host: cfg.MailHost, Port: cfg.MailPort, User: cfg.MailUser, Password: cfg.MailPassword, From: cfg.MailFrom, StartTLS: cfg.MailStartTLS}
	}
	var box identity.Mailbox
	if cfg.MailDevMailbx {
		box = identity.PGMailbox{Pool: pool}
		mailer = identity.RecordingMailer{Inner: mailer, Mailbox: box}
	}
	svc := &identity.Service{
		Store: &identity.Store{Pool: pool}, Tokens: tokens, Mailer: mailer,
		Opts: identity.Options{RequireConfirmedEmail: cfg.RequireConfirmedEmail, PublicURL: cfg.PublicURL, AppName: cfg.AppName, RefreshDays: cfg.JWTRefreshDays},
	}
	return svc, box, nil
}

// App is the wired API.
type App struct {
	Handler     http.Handler
	Identity    *identity.Service
	IdentityAPI *identity.API
	Raffles     *raffles.Service
	Claims      *claims.Service
	Orders      *orders.Service
	Mailbox     identity.Mailbox
	Kafka       *messaging.Kafka
	Rabbit      *messaging.Rabbit
	DeadLetters *messaging.DeadLetters

	cfg  config.Config
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New builds the modules, the routes and the middleware chain. It starts nothing.
func New(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) (*App, error) {
	idSvc, box, err := newIdentity(pool, cfg, log)
	if err != nil {
		return nil, err
	}
	idAPI := &identity.API{Svc: idSvc, Log: log, Opts: identity.HTTPOptions{
		SecureCookies: cfg.JWTSecureCookies, RevocationCacheSeconds: cfg.RevocationCacheSeconds,
		AuthPerMinute: cfg.AuthPerMinute, TrustForwarded: cfg.ForwardedHeaders, DevMailbox: box,
	}}
	a := &App{Identity: idSvc, IdentityAPI: idAPI, Mailbox: box, cfg: cfg, pool: pool, log: log}
	a.Raffles = raffles.NewService(pool, log)
	a.Claims = claims.NewService(pool, log)
	a.Orders = orders.NewService(pool, raffles.Catalog{Pool: pool}, log)
	a.Orders.FinalMarkDelay = cfg.FinalMarkDelay

	if cfg.MessagingEnabled {
		a.Kafka = &messaging.Kafka{Brokers: cfg.KafkaBrokers, Log: log}
		a.Rabbit = &messaging.Rabbit{URL: cfg.RabbitURL, Log: log}
		a.DeadLetters = &messaging.DeadLetters{Rabbit: a.Rabbit, Queues: []string{messaging.RafflesQueue, messaging.ClaimsQueue, messaging.OrdersQueue}}
	}

	mux := http.NewServeMux()
	idAPI.Routes(mux)
	(&orders.API{Svc: a.Orders, Kafka: a.Kafka, Auth: idAPI, Log: log}).Routes(mux)
	(&raffles.API{Pool: pool, Auth: idAPI, Log: log}).Routes(mux)
	(&claims.API{Pool: pool, Auth: idAPI, Log: log}).Routes(mux)
	registerAdmin(mux, log, idAPI, a.DeadLetters)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("Healthy")) })
	mux.HandleFunc("GET /api/health/ready", ready(pool))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteProblem(w, r, http.StatusNotFound, "Not found.")
	})

	var h http.Handler = idAPI.RateLimit(mux)
	h = httpx.AccessLog(log, h)
	h = httpx.Recover(log, h)
	h = httpx.SecurityHeaders(h)
	a.Handler = httpx.Correlation(h)
	return a, nil
}

// Run starts the background workers (the raffle drawer, and with messaging on the outbox relay and the consumers) and
// blocks until the context ends and they have stopped.
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(name string, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
			a.log.Info("stopped", "worker", name)
		}()
	}
	start("raffle drawer", func(ctx context.Context) { a.Raffles.Run(ctx, a.cfg.DrawInterval) })
	if a.cfg.MessagingEnabled {
		start("outbox relay", (&messaging.Relay{
			Pool: a.pool, Rabbit: a.Rabbit, Kafka: a.Kafka, Log: a.log, OutboxRetention: a.cfg.OutboxRetention, InboxRetention: a.cfg.InboxRetention,
			Outbox: []messaging.Outbox{a.Orders.Outbox, a.Raffles.Outbox, a.Claims.Outbox},
		}).Run)
		for _, c := range []messaging.Consumer{
			{Queue: messaging.RafflesQueue, RoutingKeys: []string{messaging.ReserveKey, messaging.ReleaseKey}, Prefetch: 1, Handler: a.Raffles},
			{Queue: messaging.ClaimsQueue, RoutingKeys: []string{messaging.HeldKey}, Prefetch: 4, Handler: a.Claims},
			{Queue: messaging.OrdersQueue, RoutingKeys: []string{messaging.HeldKey, messaging.ShortKey, messaging.EnteredKey, messaging.IssuedKey, messaging.RejectedKey, messaging.ReleasedKey}, Prefetch: 4, Handler: a.Orders},
		} {
			start("consumer "+c.Queue, func(ctx context.Context) { c.Run(ctx, a.Rabbit, a.log) })
		}
	}
	wg.Wait()
	if a.Kafka != nil {
		a.Kafka.Close()
	}
}

func ready(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var ok bool
		err := pool.QueryRow(ctx, `SELECT to_regclass('identity.users') IS NOT NULL AND to_regclass('orders.orders') IS NOT NULL
			AND to_regclass('raffles.raffles') IS NOT NULL AND to_regclass('claims.claims') IS NOT NULL`).Scan(&ok)
		if err != nil || !ok {
			httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "Not ready: a module schema cannot be reached.")
			return
		}
		_, _ = w.Write([]byte("Healthy"))
	}
}
