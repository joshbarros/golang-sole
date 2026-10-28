// Command api is the Sole API: a modular monolith (identity, orders, raffles, claims) that talks to itself through the
// outbox, RabbitMQ and Kafka. `api migrate` applies the schema and exits (the Kubernetes init container); `api` serves.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joshbarros/golang-sole/internal/app"
	"github.com/joshbarros/golang-sole/internal/platform/config"
	"github.com/joshbarros/golang-sole/internal/platform/pgdb"
)

func main() {
	cfg, err := config.FromEnv()
	log := newLogger(cfg.LogJSON)
	if err != nil {
		log.Error("configuration is not valid", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "migrate":
		err = migrateOnly(ctx, cfg, log)
	case "serve":
		err = serve(ctx, cfg, log)
	default:
		err = fmt.Errorf("unknown command %q (use serve or migrate)", command)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func newLogger(json bool) *slog.Logger {
	if json {
		return slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

func migrateOnly(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := pgdb.Open(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := app.Migrate(ctx, pool, cfg, log); err != nil {
		return err
	}
	log.Info("migrations applied")
	return nil
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := pgdb.Open(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Kubernetes runs `api migrate` in an init container and sets MIGRATE_ON_START=false; on a laptop the API migrates itself.
	if os.Getenv("MIGRATE_ON_START") != "false" {
		if err := app.Migrate(ctx, pool, cfg, log); err != nil {
			return err
		}
	}
	a, err := app.New(cfg, pool, log)
	if err != nil {
		return err
	}

	workers := make(chan struct{})
	go func() {
		defer close(workers)
		a.Run(ctx)
	}()

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: a.Handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		errs <- srv.ListenAndServe()
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdown)
	<-workers
	return err
}
