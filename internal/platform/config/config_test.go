package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultsNeedOnlyASigningKey(t *testing.T) {
	t.Setenv("JWT_KEY", strings.Repeat("k", 32))
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.JWTAccessMinutes != 15 || c.JWTRefreshDays != 14 || !c.RequireConfirmedEmail || !c.MessagingEnabled || c.AuthPerMinute != 20 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.DrawInterval != time.Second || c.OutboxRetention != 7*24*time.Hour || c.InboxRetention != 30*24*time.Hour {
		t.Fatalf("unexpected timings: %+v", c)
	}
}

func TestTheProcessRefusesToStartWithoutAUsableKeyOrSaneLifetimes(t *testing.T) {
	t.Setenv("JWT_KEY", "short")
	if _, err := FromEnv(); err == nil {
		t.Fatal("a short signing key must be refused")
	}
	t.Setenv("JWT_KEY", strings.Repeat("k", 32))
	t.Setenv("JWT_ACCESS_MINUTES", "0")
	if _, err := FromEnv(); err == nil {
		t.Fatal("an access token lifetime of 0 must be refused")
	}
	t.Setenv("JWT_ACCESS_MINUTES", "15")
	t.Setenv("JWT_REFRESH_DAYS", "400")
	if _, err := FromEnv(); err == nil {
		t.Fatal("a refresh lifetime of 400 days must be refused")
	}
}

func TestEnvironmentOverridesAndBadValuesFallBack(t *testing.T) {
	t.Setenv("JWT_KEY", strings.Repeat("k", 32))
	t.Setenv("KAFKA_BROKERS", "a:9092,b:9092")
	t.Setenv("AUTH_REQUIRE_CONFIRMED_EMAIL", "false")
	t.Setenv("RATE_LIMIT_AUTH_PER_MINUTE", "not-a-number")
	t.Setenv("ORDERS_FINAL_MARK_DELAY_MS", "1500")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.KafkaBrokers) != 2 || c.RequireConfirmedEmail || c.AuthPerMinute != 20 || c.FinalMarkDelay != 1500*time.Millisecond {
		t.Fatalf("unexpected: %+v", c)
	}
}
