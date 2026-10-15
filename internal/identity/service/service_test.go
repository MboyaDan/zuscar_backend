package service

import (
	"testing"

	"zuscar/backend/internal/platform/auth"
)

func TestValidPIN(t *testing.T) {
	good := []string{"4821", "7391", "58204", "90417352"}
	bad := []string{"", "123", "1234", "4321", "0000", "7777", "123456", "987654", "12a4", "123456789", " 4821", "48 21", "4821\n"}
	for _, p := range good {
		if !validPIN(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range bad {
		if validPIN(p) {
			t.Errorf("%q should be rejected", p)
		}
	}
}

func TestCSRFMatchesComparesHashes(t *testing.T) {
	tok := "a-session-bound-csrf-token"
	p := &auth.Principal{CSRFHash: hashToken(tok)}
	if !CSRFMatches(p, tok) {
		t.Fatal("matching token rejected")
	}
	for _, bad := range []string{"", "another", tok + "x"} {
		if CSRFMatches(p, bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if CSRFMatches(nil, tok) {
		t.Fatal("nil principal accepted")
	}
}

func TestFloorRolesAreExactlyCashierWaiterBartender(t *testing.T) {
	want := map[string]bool{"cashier": true, "waiter": true, "bartender": true}
	if len(floorRoles) != len(want) {
		t.Fatalf("floorRoles = %v", floorRoles)
	}
	for r := range want {
		if !floorRoles[r] {
			t.Errorf("%s missing", r)
		}
	}
	for _, never := range []string{"super_admin", "business_manager", "accountant_auditor", "receptionist", "storekeeper", "kitchen_staff"} {
		if floorRoles[never] {
			t.Errorf("%s must not be a PIN role", never)
		}
	}
}
