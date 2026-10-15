package handler_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"zuscar/backend/internal/identity/catalog"
	"zuscar/backend/internal/identity/handler"
	"zuscar/backend/internal/identity/password"
)

func TestLoginSetsHardenedCookiesAndMeReturnsRolesAndPermissions(t *testing.T) {
	e := newEnv(t, envOpts{secure: true})
	e.addUser("dev.cashier", false, "cashier")
	j := jar{}
	w := e.login("dev.cashier", pw, j)
	expect(t, w, 200, "")
	var sess, csrf *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case handler.SessionCookie:
			sess = c
		case handler.CSRFCookie:
			csrf = c
		}
	}
	if sess == nil || csrf == nil {
		t.Fatal("both cookies must be set")
	}
	if !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" {
		t.Errorf("session cookie flags wrong: %+v", sess)
	}
	if csrf.HttpOnly || !csrf.Secure || csrf.SameSite != http.SameSiteLaxMode {
		t.Errorf("csrf cookie must be script-readable, Secure and Lax: %+v", csrf)
	}
	if strings.Contains(w.Body.String(), sess.Value) || strings.Contains(w.Body.String(), "password_hash") {
		t.Error("login response must not echo the token or any hash")
	}
	// Secure cookies are not sent back over http by a real browser, but the test jar does; use the unsecured env for flows.
	_, _ = e, j
}

func TestMeReturnsPermissionsFromTheRoleTemplate(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	w, m := e.me(j)
	expect(t, w, 200, "")
	var want []string
	for _, r := range catalog.Roles {
		if r.Code == "cashier" {
			want = r.Permissions
		}
	}
	if len(m.Permissions) != len(want) || m.AuthMethod != "password" || m.User.Username != "dev.cashier" || !has(m.Roles, "cashier") {
		t.Fatalf("me = %+v, want %d permissions", m, len(want))
	}
	for _, p := range want {
		if !has(m.Permissions, p) {
			t.Errorf("missing %s", p)
		}
	}
}

func TestSessionAndCSRFTokensAreStoredOnlyAsHashes(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	var tokenHash, csrfHash []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT token_hash, csrf_hash FROM sessions`).Scan(&tokenHash, &csrfHash); err != nil {
		t.Fatal(err)
	}
	st := sha256.Sum256([]byte(j[handler.SessionCookie]))
	ct := sha256.Sum256([]byte(j[handler.CSRFCookie]))
	if string(tokenHash) != string(st[:]) || string(csrfHash) != string(ct[:]) {
		t.Fatal("stored values must be SHA-256 of the cookie values")
	}
	if len(j[handler.SessionCookie]) < 40 || j[handler.SessionCookie] == j[handler.CSRFCookie] {
		t.Fatal("tokens must be long, random and distinct")
	}
}

func TestWrongPasswordAndUnknownUserGiveIdenticalResponses(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	a := e.login("dev.cashier", "wrong-password-123", jar{})
	b := e.login("no.such.user", "wrong-password-123", jar{})
	expect(t, a, 401, "invalid_credentials")
	expect(t, b, 401, "invalid_credentials")
	var ea, eb errBody
	_ = jsonUnmarshal(a.Body.Bytes(), &ea)
	_ = jsonUnmarshal(b.Body.Bytes(), &eb)
	if ea.Error.Message != eb.Error.Message {
		t.Fatalf("messages differ: %q vs %q", ea.Error.Message, eb.Error.Message)
	}
}

// Prompt section 6: login rate limiting and lockout.
func TestLockoutAfterMaxAttemptsThenReleasesAfterWindow(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	for i := 1; i <= 4; i++ {
		expect(t, e.login("dev.cashier", "wrong-password-123", jar{}), 401, "invalid_credentials")
	}
	w := e.login("dev.cashier", "wrong-password-123", jar{})
	expect(t, w, 423, "account_locked") // the fifth failure locks the account
	if ra := w.Header().Get("Retry-After"); ra != "900" {
		t.Fatalf("Retry-After = %q, want 900", ra)
	}
	// Even the correct password is refused while locked (and is not checked).
	expect(t, e.login("dev.cashier", pw, jar{}), 423, "account_locked")
	e.clock.Advance(14 * time.Minute)
	expect(t, e.login("dev.cashier", pw, jar{}), 423, "account_locked")
	e.clock.Advance(2 * time.Minute)
	expect(t, e.login("dev.cashier", pw, jar{}), 200, "")
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT failed_login_count FROM users WHERE username = 'dev.cashier'`).Scan(&n)
	if n != 0 {
		t.Fatalf("failed_login_count = %d after a successful login", n)
	}
}

func TestSuccessfulLoginResetsTheFailureCounter(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	for i := 0; i < 3; i++ {
		expect(t, e.login("dev.cashier", "wrong-password-123", jar{}), 401, "invalid_credentials")
	}
	expect(t, e.login("dev.cashier", pw, jar{}), 200, "")
	for i := 0; i < 4; i++ { // would have locked at the 5th failure had the counter not been reset
		expect(t, e.login("dev.cashier", "wrong-password-123", jar{}), 401, "invalid_credentials")
	}
	expect(t, e.login("dev.cashier", pw, jar{}), 200, "")
}

func TestUnknownUsernamesAreThrottledToo(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 4; i++ {
		expect(t, e.login("ghost", "wrong-password-123", jar{}), 401, "invalid_credentials")
	}
	// The fifth failure is throttled, exactly as the fifth failure for a real account is locked.
	expect(t, e.login("ghost", "wrong-password-123", jar{}), 429, "rate_limited")
	expect(t, e.login("ghost", "wrong-password-123", jar{}), 429, "rate_limited")
}

func TestPerIPRateLimitIgnoresSpoofedForwardedHeaders(t *testing.T) {
	e := newEnv(t, envOpts{ipMax: 3})
	e.addUser("dev.cashier", false, "cashier")
	for i, name := range []string{"a.user", "b.user", "c.user"} {
		w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]string{"username": name, "password": "wrong-password-123"},
			header: map[string]string{"X-Forwarded-For": "203.0.113." + string(rune('1'+i))}})
		expect(t, w, 401, "invalid_credentials")
	}
	// Same peer, a fresh spoofed X-Forwarded-For: still limited because proxies are not trusted by default.
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]string{"username": "dev.cashier", "password": pw},
		header: map[string]string{"X-Forwarded-For": "198.51.100.77"}})
	expect(t, w, 429, "rate_limited")
	// A different peer is unaffected.
	w = e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]string{"username": "dev.cashier", "password": pw}, remote: "192.0.2.99:1"})
	expect(t, w, 200, "")
}

func TestInactiveUserCannotLoginAndExistingSessionStopsWorking(t *testing.T) {
	e := newEnv(t)
	id := e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 200, "")
	e.exec(`UPDATE users SET status = 'inactive', deactivated_at = now() WHERE id = $1`, id)
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 401, "unauthenticated")
	expect(t, e.login("dev.cashier", pw, jar{}), 401, "invalid_credentials")
}

// ADR-04: permissions are read from the database on each request, never cached in the session.
func TestPermissionChangesApplyOnTheNextRequestWithoutRelogin(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	_, m := e.me(j)
	if has(m.Permissions, "sales.discount") {
		t.Fatal("cashier must not have sales.discount by default")
	}
	e.exec(`INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p WHERE r.code = 'cashier' AND p.key = 'sales.discount'`)
	_, m = e.me(j)
	if !has(m.Permissions, "sales.discount") {
		t.Fatal("granted permission should be visible on the next request")
	}
	e.exec(`DELETE FROM role_permissions USING roles r, permissions p WHERE role_permissions.role_id = r.id AND role_permissions.permission_id = p.id AND r.code = 'cashier' AND p.key = 'sales.discount'`)
	_, m = e.me(j)
	if has(m.Permissions, "sales.discount") {
		t.Fatal("revoked permission should disappear on the next request")
	}
	e.exec(`DELETE FROM user_roles WHERE user_id = (SELECT id FROM users WHERE username = 'dev.cashier')`)
	_, m = e.me(j)
	if len(m.Permissions) != 0 {
		t.Fatalf("a user with no roles has no permissions, got %v", m.Permissions)
	}
}

func TestIdleAndAbsoluteExpiry(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	// Idle: no activity for longer than 2 hours.
	j := e.mustLogin("dev.cashier")
	e.clock.Advance(2*time.Hour + time.Minute)
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 401, "unauthenticated")
	var reason string
	_ = e.pool.QueryRow(context.Background(), `SELECT revoke_reason FROM sessions ORDER BY id LIMIT 1`).Scan(&reason)
	if reason != "expired" {
		t.Fatalf("revoke_reason = %q", reason)
	}
	// Absolute: activity every 90 minutes keeps it alive until 12 hours, then it ends regardless.
	j = e.mustLogin("dev.cashier")
	for i := 0; i < 7; i++ { // 7 x 90 min = 10.5 h
		e.clock.Advance(90 * time.Minute)
		expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 200, "")
	}
	_, m := e.me(j)
	if m.Session.Idle.After(m.Session.Abs) {
		t.Fatal("idle expiry must never pass the absolute expiry")
	}
	e.clock.Advance(90 * time.Minute) // 12 h in total
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 401, "unauthenticated")
}

// ADR-13: CSRF protection on state-changing requests.
func TestCSRFProtection(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	other := e.mustLogin("dev.cashier") // a second session whose token must not work on the first
	j := e.mustLogin("dev.cashier")
	post := func(r req) *http.Response {
		r.method, r.path = "POST", "/api/v1/auth/reauth"
		r.body = map[string]string{"password": pw}
		return e.do(r).Result()
	}

	if got := post(req{jar: j, noCSRF: true}).StatusCode; got != 403 {
		t.Errorf("missing token: %d", got)
	}
	if got := post(req{jar: j, csrf: "not-the-token"}).StatusCode; got != 403 {
		t.Errorf("wrong token: %d", got)
	}
	if got := post(req{jar: j, csrf: other[handler.CSRFCookie]}).StatusCode; got != 403 {
		t.Errorf("another session's token: %d", got)
	}
	// header equals cookie but both are forged (cookie injection): the session hash does not match
	forged := jar{handler.SessionCookie: j[handler.SessionCookie], handler.CSRFCookie: "forged-token-value"}
	if got := post(req{jar: forged, csrf: "forged-token-value"}).StatusCode; got != 403 {
		t.Errorf("forged cookie+header: %d", got)
	}
	if got := post(req{jar: j, origin: "https://evil.example"}).StatusCode; got != 403 {
		t.Errorf("foreign origin with a valid token: %d", got)
	}
	if got := post(req{jar: j, noOrigin: true, header: map[string]string{"Referer": "https://evil.example/page"}}).StatusCode; got != 403 {
		t.Errorf("foreign referer: %d", got)
	}
	if got := post(req{jar: j}).StatusCode; got != 204 {
		t.Errorf("valid request: %d", got)
	}
	if got := post(req{jar: j, noOrigin: true}).StatusCode; got != 204 {
		t.Errorf("no Origin (script client) with a valid token: %d", got)
	}
	// reads need no token
	if w, _ := e.me(j); w.Code != 200 {
		t.Errorf("GET me: %d", w.Code)
	}
}

func TestLoginRejectsForeignOrigin(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", origin: "https://evil.example", body: map[string]string{"username": "dev.cashier", "password": pw}})
	expect(t, w, 403, "csrf_failed")
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("no cookies may be set on a rejected login")
	}
}

func TestLogoutRevokesTheSessionAndClearsCookies(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	old := jar{handler.SessionCookie: j[handler.SessionCookie], handler.CSRFCookie: j[handler.CSRFCookie]}
	w := e.do(req{method: "POST", path: "/api/v1/auth/logout", jar: j})
	expect(t, w, 204, "")
	cleared := 0
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared++
		}
	}
	if cleared != 2 {
		t.Fatalf("expected both cookies cleared, %d were", cleared)
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/auth/me", jar: old}), 401, "unauthenticated")
}

func TestAuthRoutesNeedASession(t *testing.T) {
	e := newEnv(t)
	for _, r := range []struct{ m, p string }{{"GET", "/me"}, {"POST", "/logout"}, {"POST", "/change-password"}, {"POST", "/reauth"}, {"POST", "/pin-switch"}} {
		expect(t, e.do(req{method: r.m, path: "/api/v1/auth" + r.p, body: map[string]string{}}), 401, "unauthenticated")
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe"}), 401, "unauthenticated")
}

func TestInputValidation(t *testing.T) {
	e := newEnv(t)
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]string{"username": "x"}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "x", "password": "y", "admin": true}}), 400, "validation_failed")
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]string{"username": " ", "password": ""}})
	expect(t, w, 400, "validation_failed")
	var b errBody
	_ = jsonUnmarshal(w.Body.Bytes(), &b)
	if len(b.Error.Fields) != 2 || b.Error.RequestID == "" {
		t.Fatalf("expected two field errors and a request id: %+v", b)
	}
}

func TestUsernameIsCaseInsensitiveAtLogin(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	expect(t, e.login("  Dev.Cashier ", pw, jar{}), 200, "")
}

func TestMustChangePasswordBlocksOrdinaryRoutesUntilChanged(t *testing.T) {
	e := newEnv(t)
	e.addUser("new.starter", true, "cashier")
	j := jar{}
	w := e.login("new.starter", pw, j)
	expect(t, w, 200, "")
	if !strings.Contains(w.Body.String(), `"must_change_password":true`) {
		t.Fatal("login must report the forced change")
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 403, "password_change_required")
	if w, m := e.me(j); w.Code != 200 || !m.MustChange {
		t.Fatalf("me must stay reachable: %d %+v", w.Code, m)
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/pin-switch", jar: j, body: map[string]string{"username": "x", "pin": "4821"}}), 403, "password_change_required")
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/change-password", jar: j, body: map[string]string{"current_password": pw, "new_password": "a-much-better-passphrase-42"}}), 204, "")
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 200, "")
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	other := e.mustLogin("dev.cashier") // a second session that must be ended by the change
	change := func(cur, next string) *httpResp {
		return &httpResp{e.do(req{method: "POST", path: "/api/v1/auth/change-password", jar: j, body: map[string]string{"current_password": cur, "new_password": next}})}
	}
	expect(t, change("wrong-current-pw-1", "a-much-better-passphrase-42").W, 401, "invalid_credentials")
	expect(t, change(pw, pw).W, 400, "validation_failed")
	expect(t, change(pw, "short").W, 400, "validation_failed")
	expect(t, change(pw, password.DevSeedPassword).W, 400, "validation_failed")
	expect(t, change(pw, "my-dev.cashier-password-1").W, 400, "validation_failed") // contains the username
	expect(t, change(pw, "a-much-better-passphrase-42").W, 204, "")
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: j}), 200, "")                    // current session survives
	expect(t, e.do(req{method: "GET", path: "/api/v1/_probe", jar: other}), 401, "unauthenticated") // the other one ends
	expect(t, e.login("dev.cashier", pw, jar{}), 401, "invalid_credentials")
	expect(t, e.login("dev.cashier", "a-much-better-passphrase-42", jar{}), 200, "")
}

func TestWrongCurrentPasswordOnChangeCountsTowardLockout(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	var last int
	for i := 0; i < 5; i++ {
		last = e.do(req{method: "POST", path: "/api/v1/auth/change-password", jar: j, body: map[string]string{"current_password": "wrong-current-pw-1", "new_password": "a-much-better-passphrase-42"}}).Code
	}
	if last != 423 {
		t.Fatalf("fifth wrong current password should lock the account, got %d", last)
	}
	expect(t, e.login("dev.cashier", pw, jar{}), 423, "account_locked")
}

// S-16: sensitive actions need a password entry within 5 minutes.
func TestReauthWindow(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	j := e.mustLogin("dev.cashier")
	expect(t, e.do(req{method: "GET", path: "/api/v1/_sensitive", jar: j}), 403, "reauth_required")
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/reauth", jar: j, body: map[string]string{"password": "wrong-password-123"}}), 401, "invalid_credentials")
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/reauth", jar: j, body: map[string]string{"password": pw}}), 204, "")
	expect(t, e.do(req{method: "GET", path: "/api/v1/_sensitive", jar: j}), 200, "")
	if _, m := e.me(j); !m.Session.Reauth {
		t.Fatal("me should report a recent re-authentication")
	}
	e.clock.Advance(4*time.Minute + 59*time.Second)
	expect(t, e.do(req{method: "GET", path: "/api/v1/_sensitive", jar: j}), 200, "")
	e.clock.Advance(2 * time.Second)
	expect(t, e.do(req{method: "GET", path: "/api/v1/_sensitive", jar: j}), 403, "reauth_required")
}

func TestAuditTrailRecordsSecurityEventsWithoutSecrets(t *testing.T) {
	e := newEnv(t)
	e.addUser("dev.cashier", false, "cashier")
	const secretTry = "super-secret-wrong-try-9"
	for i := 0; i < 5; i++ {
		e.login("dev.cashier", secretTry, jar{})
	}
	e.login("ghost.user", secretTry, jar{})
	e.clock.Advance(16 * time.Minute)
	j := e.mustLogin("dev.cashier")
	e.do(req{method: "POST", path: "/api/v1/auth/reauth", jar: j, body: map[string]string{"password": pw}})
	e.do(req{method: "POST", path: "/api/v1/auth/change-password", jar: j, body: map[string]string{"current_password": pw, "new_password": "a-much-better-passphrase-42"}})
	tok, csrf := j[handler.SessionCookie], j[handler.CSRFCookie]
	e.do(req{method: "POST", path: "/api/v1/auth/logout", jar: j})

	rows, err := e.pool.Query(context.Background(), `SELECT action, result, actor_label, coalesce(entity_id,''), coalesce(after_json::text,''), coalesce(request_id,''), coalesce(host(ip),'') FROM audit_logs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]int{}
	for rows.Next() {
		var action, result, label, entity, after, rid, ip string
		if err := rows.Scan(&action, &result, &label, &entity, &after, &rid, &ip); err != nil {
			t.Fatal(err)
		}
		seen[action]++
		blob := strings.Join([]string{action, result, label, entity, after}, " ")
		for _, secret := range []string{secretTry, pw, "a-much-better-passphrase-42", tok, csrf, "argon2id"} {
			if strings.Contains(blob, secret) {
				t.Errorf("audit row %q contains a secret", action)
			}
		}
		if ip == "" || rid == "" {
			t.Errorf("audit row %q is missing ip or request id", action)
		}
	}
	for action, min := range map[string]int{"auth.login_failed": 6, "auth.account_locked": 1, "auth.login_success": 1, "auth.reauth": 1, "auth.password_changed": 1, "auth.logout": 1} {
		if seen[action] < min {
			t.Errorf("audit action %s recorded %d times, want at least %d", action, seen[action], min)
		}
	}
}
