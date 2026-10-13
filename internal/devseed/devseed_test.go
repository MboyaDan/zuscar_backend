package devseed

import (
	"context"
	"strings"
	"testing"

	"zuscar/backend/internal/platform/config"
)

// Prompt section 8: seed-dev must refuse in production. No database is needed to prove the guard.
func TestGuardRefusesProduction(t *testing.T) {
	if err := Guard(config.EnvProduction); err != ErrProduction {
		t.Fatalf("production: want ErrProduction, got %v", err)
	}
	if err := Guard(config.Env("staging")); err == nil {
		t.Fatal("unknown env must be refused")
	}
	for _, e := range []config.Env{config.EnvDevelopment, config.EnvTest} {
		if err := Guard(e); err != nil {
			t.Fatalf("%s should be allowed: %v", e, err)
		}
	}
}

func TestRunRefusesProductionBeforeAnyDatabaseAccess(t *testing.T) {
	// A nil pool would panic if Run touched it, so this also proves the guard comes first.
	if _, err := Run(context.Background(), nil, config.EnvProduction); err != ErrProduction {
		t.Fatalf("want ErrProduction, got %v", err)
	}
}

func TestSeedUsersAreObviouslyFake(t *testing.T) {
	for _, u := range Users {
		if !strings.HasPrefix(u.username, "dev.") || !strings.Contains(u.display, "(fake)") {
			t.Errorf("seed user %q / %q is not obviously fake", u.username, u.display)
		}
	}
}
