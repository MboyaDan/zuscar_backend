package handler_test

import (
	"context"
	"testing"
	"time"

	"zuscar/backend/internal/identity/handler"
	"zuscar/backend/internal/identity/service"
)

func (e *testEnv) setPIN(username, pin string) error {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return e.svc.SetPIN(context.Background(), nil, id, pin, service.ClientInfo{})
}

func (e *testEnv) pinSwitch(j jar, username, pin string) int {
	return e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, body: map[string]string{"username": username, "pin": pin}}).Code
}

// A-07: PIN fast-switch for floor staff on a terminal someone has already signed in to.
func TestPINSwitchHandsTheTerminalToAnotherFloorUser(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	e.addUser("dev.waiter", false, "waiter")
	if err := e.setPIN("dev.waiter", "4821"); err != nil {
		t.Fatal(err)
	}
	j := e.mustLogin("dev.cashier")
	oldSession := jar{handler.SessionCookie: j[handler.SessionCookie]}
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, body: map[string]string{"username": "Dev.Waiter", "pin": "4821"}}), 200, "")
	w, m := e.me(j)
	if w.Code != 200 || m.User.Username != "dev.waiter" || m.AuthMethod != "pin" || !has(m.Roles, "waiter") {
		t.Fatalf("after the switch me = %d %+v", w.Code, m)
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: oldSession}), 401, "unauthenticated") // the previous user is signed out
	// A PIN session cannot satisfy password re-entry (S-16)...
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/reauth", jar: j, body: map[string]string{"password": pw}}), 403, "forbidden")
	expect(t, e.do(req{method: "GET", path: "/api/v1/_sensitive", jar: j}), 403, "reauth_required")
	// ...and the waiter can hand over again with another PIN.
	e.addUser("dev.bartender", false, "bartender")
	if err := e.setPIN("dev.bartender", "7391"); err != nil {
		t.Fatal(err)
	}
	if got := e.pinSwitch(j, "dev.bartender", "7391"); got != 200 {
		t.Fatalf("second switch = %d", got)
	}
}

func TestPINSwitchIsBoundByTheOriginalAbsoluteExpiry(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	e.addUser("dev.waiter", false, "waiter")
	_ = e.setPIN("dev.waiter", "4821")
	j := e.mustLogin("dev.cashier")
	_, orig := e.me(j)
	for i := 0; i < 6; i++ { // 6 x 100 min = 10 h of activity under the 2 h idle limit
		e.clock.Advance(100 * time.Minute)
		expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 200, "")
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, body: map[string]string{"username": "dev.waiter", "pin": "4821"}}), 200, "")
	_, m := e.me(j)
	if !m.Session.Abs.Equal(orig.Session.Abs) {
		t.Fatalf("PIN session absolute expiry %v must equal the original %v, not now+12h", m.Session.Abs, orig.Session.Abs)
	}
	e.clock.Advance(100 * time.Minute) // 11 h 40 min into the original login
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 200, "")
	e.clock.Advance(40 * time.Minute) // past 12 h from the original password login
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 401, "unauthenticated")
}

func TestPINIsRefusedForNonFloorRolesWithoutRevealingWhoTheyAre(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	mgr := e.addUser("dev.manager", false, "business_manager")
	owner := e.addUser("dev.owner", false, "super_admin", "business_manager")
	for _, id := range []int64{mgr, owner} {
		if err := e.svc.SetPIN(context.Background(), nil, id, "4821", service.ClientInfo{}); err != service.ErrPINNotAllowed {
			t.Fatalf("SetPIN for a manager or Super Admin: want ErrPINNotAllowed, got %v", err)
		}
	}
	// A PIN row planted directly in the database still cannot be used to switch into a manager.
	e.exec(`INSERT INTO user_pins (user_id, pin_hash) SELECT $1, password_hash FROM users WHERE id = $1`, mgr)
	j := e.mustLogin("dev.cashier")
	if got := e.pinSwitch(j, "dev.manager", "4821"); got != 401 {
		t.Fatalf("switch to a manager = %d, want a generic 401", got)
	}
	unknown := e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, body: map[string]string{"username": "ghost", "pin": "4821"}})
	forManager := e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, body: map[string]string{"username": "dev.manager", "pin": "4821"}})
	if unknown.Body.String()[:40] != forManager.Body.String()[:40] || unknown.Code != forManager.Code {
		t.Fatal("unknown user and manager must look identical")
	}
	// A user with several roles including a non-floor role is not a floor user either.
	e.addUser("dev.mixed", false, "waiter", "storekeeper")
	if err := e.setPIN("dev.mixed", "4821"); err != service.ErrPINNotAllowed {
		t.Fatalf("mixed roles: %v", err)
	}
}

func TestPINFormatRules(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.waiter", false, "waiter")
	for _, bad := range []string{"1234", "0000", "4321", "12", "abcd", "123456789", "1111"} {
		if err := e.setPIN("dev.waiter", bad); err != service.ErrInvalidPIN {
			t.Errorf("PIN %q: want ErrInvalidPIN, got %v", bad, err)
		}
	}
	if err := e.setPIN("dev.waiter", "4821"); err != nil {
		t.Fatal(err)
	}
	var hash string
	_ = e.pool.QueryRow(context.Background(), `SELECT pin_hash FROM user_pins`).Scan(&hash)
	if len(hash) < 50 || hash[:10] != "$argon2id$" {
		t.Fatalf("PIN must be stored as an Argon2id hash, got %q", hash)
	}
}

func TestPINLockoutAndRecovery(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	e.addUser("dev.waiter", false, "waiter")
	_ = e.setPIN("dev.waiter", "4821")
	j := e.mustLogin("dev.cashier")
	for i := 1; i <= 4; i++ {
		if got := e.pinSwitch(j, "dev.waiter", "9999"); got != 401 {
			t.Fatalf("wrong PIN %d = %d", i, got)
		}
	}
	if got := e.pinSwitch(j, "dev.waiter", "9999"); got != 423 { // fifth wrong PIN locks it
		t.Fatalf("fifth wrong PIN = %d, want 423", got)
	}
	if got := e.pinSwitch(j, "dev.waiter", "4821"); got != 423 { // right PIN refused while locked
		t.Fatalf("correct PIN while locked = %d, want 423", got)
	}
	e.clock.Advance(16 * time.Minute)
	if got := e.pinSwitch(j, "dev.waiter", "4821"); got != 200 {
		t.Fatalf("correct PIN after the lock expired = %d, want 200", got)
	}
}

func TestPINSwitchNeedsASessionAndAValidBody(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", body: map[string]string{"username": "x", "pin": "4821"}}), 401, "unauthenticated")
	j := e.mustLogin("dev.cashier")
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, body: map[string]string{"username": "x"}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, noCSRF: true, body: map[string]string{"username": "x", "pin": "4821"}}), 403, "csrf_failed")
}

func TestPINAuditTrail(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	e.addUser("dev.waiter", false, "waiter")
	_ = e.setPIN("dev.waiter", "4821")
	j := e.mustLogin("dev.cashier")
	e.pinSwitch(j, "dev.waiter", "9999")
	e.pinSwitch(j, "dev.waiter", "4821")
	for _, a := range []string{"auth.pin_set", "auth.pin_failed", "auth.pin_switch"} {
		var n int
		_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = $1 AND coalesce(after_json::text,'') NOT LIKE '%4821%' AND coalesce(after_json::text,'') NOT LIKE '%9999%'`, a).Scan(&n)
		if n < 1 {
			t.Errorf("audit action %s missing or contains a PIN", a)
		}
	}
}
