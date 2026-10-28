// Package config reads the process configuration from the environment, the way a Kubernetes ConfigMap and Secret deliver it.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything the API process needs. Every field has a default that works against the local dev dependencies.
type Config struct {
	ListenAddr  string
	LogJSON     bool
	DatabaseURL string

	RabbitURL    string
	KafkaBrokers []string

	MessagingEnabled bool
	OutboxRetention  time.Duration
	InboxRetention   time.Duration

	JWTKey                 string
	JWTRSAPrivateKeyPEM    string
	JWTKeyID               string
	JWTIssuer              string
	JWTAudience            string
	JWTAccessMinutes       int
	JWTRefreshDays         int
	JWTSecureCookies       bool
	RevocationCacheSeconds int

	AdminEmail    string
	AdminPassword string

	RequireConfirmedEmail bool
	PublicURL             string
	AppName               string

	MailHost      string
	MailPort      int
	MailUser      string
	MailPassword  string
	MailFrom      string
	MailStartTLS  bool
	MailDevMailbx bool

	AuthPerMinute    int
	ForwardedHeaders bool
	DocsPublic       bool

	FinalMarkDelay time.Duration
	DrawInterval   time.Duration
}

// FromEnv builds a Config from environment variables and validates it.
func FromEnv() (Config, error) {
	c := Config{
		ListenAddr:  str("LISTEN_ADDR", ":8080"),
		LogJSON:     boolean("LOG_JSON", false),
		DatabaseURL: str("DATABASE_URL", "postgres://sole:sole@localhost:5488/sole?sslmode=disable"),

		RabbitURL:    str("RABBITMQ_URL", "amqp://platform:platform@localhost:5718/"),
		KafkaBrokers: strings.Split(str("KAFKA_BROKERS", "localhost:9138"), ","),

		MessagingEnabled: boolean("MESSAGING_ENABLED", true),
		OutboxRetention:  time.Duration(integer("OUTBOX_RETENTION_DAYS", 7)) * 24 * time.Hour,
		InboxRetention:   time.Duration(integer("INBOX_RETENTION_DAYS", 30)) * 24 * time.Hour,

		JWTKey:                 str("JWT_KEY", ""),
		JWTRSAPrivateKeyPEM:    str("JWT_RSA_PRIVATE_KEY_PEM", ""),
		JWTKeyID:               str("JWT_KEY_ID", "sole-1"),
		JWTIssuer:              str("JWT_ISSUER", "sole-api"),
		JWTAudience:            str("JWT_AUDIENCE", "sole-web"),
		JWTAccessMinutes:       integer("JWT_ACCESS_MINUTES", 15),
		JWTRefreshDays:         integer("JWT_REFRESH_DAYS", 14),
		JWTSecureCookies:       boolean("JWT_SECURE_COOKIES", false),
		RevocationCacheSeconds: integer("JWT_REVOCATION_CACHE_SECONDS", 5),

		AdminEmail:    str("ADMIN_EMAIL", ""),
		AdminPassword: str("ADMIN_PASSWORD", ""),

		RequireConfirmedEmail: boolean("AUTH_REQUIRE_CONFIRMED_EMAIL", true),
		PublicURL:             str("AUTH_PUBLIC_URL", "http://localhost:4308"),
		AppName:               str("AUTH_APP_NAME", "Sole"),

		MailHost:      str("MAIL_HOST", ""),
		MailPort:      integer("MAIL_PORT", 587),
		MailUser:      str("MAIL_USER", ""),
		MailPassword:  str("MAIL_PASSWORD", ""),
		MailFrom:      str("MAIL_FROM", "no-reply@sole.local"),
		MailStartTLS:  boolean("MAIL_START_TLS", true),
		MailDevMailbx: boolean("MAIL_DEV_MAILBOX", false),

		AuthPerMinute:    integer("RATE_LIMIT_AUTH_PER_MINUTE", 20),
		ForwardedHeaders: boolean("FORWARDED_HEADERS", false),
		DocsPublic:       boolean("DOCS_PUBLIC", false),

		FinalMarkDelay: time.Duration(integer("ORDERS_FINAL_MARK_DELAY_MS", 0)) * time.Millisecond,
		DrawInterval:   time.Duration(integer("RAFFLES_DRAW_INTERVAL_MS", 1000)) * time.Millisecond,
	}
	return c, c.Validate()
}

// Validate refuses a configuration the process must not start with.
func (c Config) Validate() error {
	if len(c.JWTKey) < 32 && strings.TrimSpace(c.JWTRSAPrivateKeyPEM) == "" {
		return fmt.Errorf("JWT_KEY must be at least 32 bytes (or set JWT_RSA_PRIVATE_KEY_PEM)")
	}
	if c.JWTAccessMinutes < 1 || c.JWTAccessMinutes > 120 {
		return fmt.Errorf("JWT_ACCESS_MINUTES must be between 1 and 120")
	}
	if c.JWTRefreshDays < 1 || c.JWTRefreshDays > 90 {
		return fmt.Errorf("JWT_REFRESH_DAYS must be between 1 and 90")
	}
	return nil
}

func str(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

func integer(name string, fallback int) int {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return fallback
}

func boolean(name string, fallback bool) bool {
	if v, ok := os.LookupEnv(name); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return fallback
}
