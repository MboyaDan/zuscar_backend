package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppEnv != EnvDevelopment || c.Port != "8080" || c.SessionIdleTTL != 2*time.Hour ||
		c.IdempotencyTTL != 24*time.Hour || c.MaxRequestBytes != 1<<20 || !c.CookieSecure {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	_, err := Load(env(map[string]string{
		"APP_ENV": "staging", "API_PORT": "abc", "SESSION_IDLE_TTL": "soon", "LOG_LEVEL": "loud",
		"LOGIN_MAX_ATTEMPTS": "0", "COOKIE_SECURE": "maybe",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"APP_ENV", "API_PORT", "SESSION_IDLE_TTL", "LOG_LEVEL", "LOGIN_MAX_ATTEMPTS", "COOKIE_SECURE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestProductionRules(t *testing.T) {
	_, err := Load(env(map[string]string{"APP_ENV": "production", "COOKIE_SECURE": "false", "FRONTEND_URL": "http://x"}))
	if err == nil {
		t.Fatal("production with insecure settings must fail")
	}
	for _, want := range []string{"DATABASE_URL", "COOKIE_SECURE", "FRONTEND_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
	c, err := Load(env(map[string]string{"APP_ENV": "production", "DATABASE_URL": "postgres://u:p@h/db", "FRONTEND_URL": "https://app.example.test"}))
	if err != nil || c.AppEnv != EnvProduction {
		t.Fatalf("valid production config rejected: %v", err)
	}
}

func TestAbsoluteTTLNotBelowIdle(t *testing.T) {
	if _, err := Load(env(map[string]string{"SESSION_IDLE_TTL": "3h", "SESSION_ABSOLUTE_TTL": "1h"})); err == nil {
		t.Fatal("expected error")
	}
}

func TestPortFallsBackToHostPortVariable(t *testing.T) {
	c, err := Load(env(map[string]string{"PORT": "10000"}))
	if err != nil || c.Port != "10000" {
		t.Fatalf("PORT fallback: %v %v", c.Port, err)
	}
	c, _ = Load(env(map[string]string{"PORT": "10000", "API_PORT": "9000"}))
	if c.Port != "9000" {
		t.Fatalf("API_PORT should win, got %s", c.Port)
	}
}

func TestFrontendURLDefaultsOnlyOutsideProduction(t *testing.T) {
	c, _ := Load(env(nil))
	if c.FrontendURL != "http://localhost:3000" {
		t.Fatalf("development default = %q", c.FrontendURL)
	}
	if _, err := Load(env(map[string]string{"APP_ENV": "production", "DATABASE_URL": "postgres://u:p@h/db"})); err == nil {
		t.Fatal("production without FRONTEND_URL must fail")
	}
}

func TestTrustedProxiesParsed(t *testing.T) {
	c, _ := Load(env(map[string]string{"TRUSTED_PROXIES": " 10.0.0.0/8, 192.168.1.1 ,"}))
	if len(c.TrustedProxies) != 2 || c.TrustedProxies[0] != "10.0.0.0/8" || c.TrustedProxies[1] != "192.168.1.1" {
		t.Fatalf("got %v", c.TrustedProxies)
	}
	if c, _ = Load(env(nil)); len(c.TrustedProxies) != 0 {
		t.Fatal("default must trust nobody")
	}
}
