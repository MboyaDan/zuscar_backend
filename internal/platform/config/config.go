// Package config loads and validates process configuration from environment variables.
// It never reads files and never logs values; secrets stay in the environment (AC-14).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Env string

const (
	EnvDevelopment Env = "development"
	EnvTest        Env = "test"
	EnvProduction  Env = "production"
)

type Config struct {
	AppEnv             Env
	Port               string
	DatabaseURL        string
	DBSimpleProtocol   bool // set true behind a transaction-mode pooler (no prepared statements)
	LogLevel           slog.Level
	SessionIdleTTL     time.Duration
	SessionAbsoluteTTL time.Duration
	CookieSecure       bool
	FrontendURL        string
	LoginMaxAttempts   int
	LoginLockoutWindow time.Duration
	IdempotencyTTL     time.Duration
	MaxRequestBytes    int64
}

// Load reads configuration through getenv (os.Getenv in production, a map in tests).
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: want a positive duration such as 30m, got %q", key, v))
			return def
		}
		return d
	}
	integer := func(key string, def int64) int64 {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s: want a positive integer, got %q", key, v))
			return def
		}
		return n
	}
	boolean := func(key string, def bool) bool {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: want true or false, got %q", key, v))
			return def
		}
		return b
	}

	c := Config{
		AppEnv:             Env(str("APP_ENV", string(EnvDevelopment))),
		Port:               str("API_PORT", str("PORT", "8080")), // hosts such as Render provide PORT
		DatabaseURL:        str("DATABASE_URL", ""),
		DBSimpleProtocol:   boolean("DB_SIMPLE_PROTOCOL", false),
		SessionIdleTTL:     dur("SESSION_IDLE_TTL", 2*time.Hour),
		SessionAbsoluteTTL: dur("SESSION_ABSOLUTE_TTL", 12*time.Hour),
		CookieSecure:       boolean("COOKIE_SECURE", true),
		FrontendURL:        str("FRONTEND_URL", ""),
		LoginMaxAttempts:   int(integer("LOGIN_MAX_ATTEMPTS", 5)),
		LoginLockoutWindow: dur("LOGIN_LOCKOUT_WINDOW", 15*time.Minute),
		IdempotencyTTL:     dur("IDEMPOTENCY_TTL", 24*time.Hour),
		MaxRequestBytes:    integer("MAX_REQUEST_BYTES", 1<<20),
	}

	switch c.AppEnv {
	case EnvDevelopment, EnvTest, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV: want development, test or production, got %q", c.AppEnv))
	}
	if err := c.LogLevel.UnmarshalText([]byte(str("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, errors.New("LOG_LEVEL: want debug, info, warn or error"))
	}
	if p, err := strconv.Atoi(c.Port); err != nil || p < 1 || p > 65535 {
		errs = append(errs, fmt.Errorf("API_PORT: want a port number, got %q", c.Port))
	}
	if c.SessionAbsoluteTTL < c.SessionIdleTTL {
		errs = append(errs, errors.New("SESSION_ABSOLUTE_TTL must be at least SESSION_IDLE_TTL"))
	}
	if c.AppEnv == EnvProduction {
		if c.DatabaseURL == "" {
			errs = append(errs, errors.New("DATABASE_URL is required in production"))
		}
		if !c.CookieSecure {
			errs = append(errs, errors.New("COOKIE_SECURE must be true in production"))
		}
		if u, err := url.Parse(c.FrontendURL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, errors.New("FRONTEND_URL must be an https URL in production"))
		}
	}
	return c, errors.Join(errs...)
}

// CLI is the small configuration the operator commands (migrate, bootstrap, seed-dev) need.
// It deliberately does not apply the API's production rules for cookies and origins.
type CLI struct {
	AppEnv           Env
	DatabaseURL      string
	DBSimpleProtocol bool
}

func LoadCLI(getenv func(string) string) (CLI, error) {
	c := CLI{AppEnv: Env(strings.TrimSpace(getenv("APP_ENV"))), DatabaseURL: strings.TrimSpace(getenv("DATABASE_URL"))}
	if c.AppEnv == "" {
		c.AppEnv = EnvDevelopment
	}
	switch c.AppEnv {
	case EnvDevelopment, EnvTest, EnvProduction:
	default:
		return c, fmt.Errorf("APP_ENV: want development, test or production, got %q", c.AppEnv)
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if v := strings.TrimSpace(getenv("DB_SIMPLE_PROTOCOL")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("DB_SIMPLE_PROTOCOL: want true or false, got %q", v)
		}
		c.DBSimpleProtocol = b
	}
	return c, nil
}
