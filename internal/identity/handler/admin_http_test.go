package handler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zuscar/backend/internal/identity/service"
	"zuscar/backend/internal/platform/auth"
)

const newPw = "Another-Passw0rd-77"

type userResp struct {
	User service.UserView `json:"user"`
}

type listResp struct {
	Items      []service.UserView `json:"items"`
	NextCursor string             `json:"next_cursor"`
}

func decodeInto[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := jsonUnmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

// reauth records a fresh password entry on the session so sensitive admin actions are allowed.
func (e *testEnv) reauth(j jar) {
	e.t.Helper()
	expect(e.t, e.do(req{method: "POST", path: "/api/v1/auth/reauth", jar: j, body: map[string]string{"password": pw}}), 204, "")
}

// signedInReauthed creates a user with the roles, signs them in and re-enters the password.
func (e *testEnv) signedInReauthed(username string, roles ...string) (int64, jar) {
	e.t.Helper()
	id := e.addUser(username, false, roles...)
	j := e.mustLogin(username)
	e.reauth(j)
	return id, j
}

// principal builds a caller for service-level tests (a signed-in user with a recent password entry).
func (e *testEnv) principal(id int64, username string, perms ...string) *auth.Principal {
	m := map[string]bool{}
	for _, p := range perms {
		m[p] = true
	}
	now := e.clock.Now()
	return &auth.Principal{UserID: id, Username: username, AuthMethod: "password", ReauthAt: &now, Permissions: m}
}

func (e *testEnv) auditCount(action string, entityID int64) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = $1 AND entity_id = $2`, action, fmt.Sprint(entityID)).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *testEnv) activeSuperAdmins() int {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_roles ur JOIN roles r ON r.id = ur.role_id AND r.code = 'super_admin' JOIN users u ON u.id = ur.user_id AND u.status = 'active'`).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

var userRoutes = []struct{ method, path string }{
	{"GET", "/api/v1/users"}, {"POST", "/api/v1/users"}, {"GET", "/api/v1/users/1"}, {"PATCH", "/api/v1/users/1"},
	{"PUT", "/api/v1/users/1/roles"}, {"POST", "/api/v1/users/1/deactivate"}, {"POST", "/api/v1/users/1/reactivate"},
	{"POST", "/api/v1/users/1/reset-password"}, {"PUT", "/api/v1/users/1/pin"}, {"DELETE", "/api/v1/users/1"},
}

// AC-10 for the user routes: signed out is 401, signed in without the permission is 403. The full route-table
// version over docs/api/routes.csv arrives with the rest of Stage 7.
func TestAC10_UserAdminRoutesDenyWithoutPermission(t *testing.T) {
	e := newEnv(t)
	e.addUser("cashier1", false, "cashier")
	j := e.mustLogin("cashier1")
	for _, r := range userRoutes {
		expect(t, e.do(req{method: r.method, path: r.path}), 401, "unauthenticated")
		expect(t, e.do(req{method: r.method, path: r.path, jar: j, body: map[string]any{}}), 403, "forbidden")
	}
	// The forced password change blocks admin routes even for an administrator.
	e.addUser("newadmin", true, "super_admin")
	j2 := jar{}
	if w := e.login("newadmin", pw, j2); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/users", jar: j2}), 403, "password_change_required")
}

func TestAC11_ManagerHasNoRoleAdminPermissions(t *testing.T) {
	e := newEnv(t)
	e.addUser("manager1", false, "business_manager")
	_, me := e.me(e.mustLogin("manager1"))
	for _, k := range []string{"roles.manage", "permissions.assign", "system.admin"} {
		if has(me.Permissions, k) {
			t.Errorf("business manager holds %s", k)
		}
	}
	if !has(me.Permissions, "users.create") {
		t.Error("business manager should manage ordinary users")
	}
}

func TestAC11_ManagerCannotGrantOrTouchSuperAdmin(t *testing.T) {
	e := newEnv(t)
	_, mj := e.signedInReauthed("manager1", "business_manager")
	superID := e.addUser("owner1", false, "super_admin")
	plainID := e.addUser("someone1", false, "cashier")

	body := map[string]any{"username": "sneaky1", "display_name": "Sneaky", "password": newPw, "roles": []string{"super_admin"}}
	expect(t, e.do(req{method: "POST", path: "/api/v1/users", jar: mj, body: body}), 403, "escalation_refused")
	expect(t, e.do(req{method: "PUT", path: fmt.Sprintf("/api/v1/users/%d/roles", plainID), jar: mj, body: map[string]any{"roles": []string{"super_admin"}}}), 403, "escalation_refused")

	// Existing Super Admin accounts are out of reach: edit, roles, deactivate, password reset.
	for _, r := range []req{
		{method: "PATCH", path: fmt.Sprintf("/api/v1/users/%d", superID), body: map[string]string{"display_name": "Mine now"}},
		{method: "PUT", path: fmt.Sprintf("/api/v1/users/%d/roles", superID), body: map[string]any{"roles": []string{"cashier"}}},
		{method: "POST", path: fmt.Sprintf("/api/v1/users/%d/deactivate", superID)},
		{method: "POST", path: fmt.Sprintf("/api/v1/users/%d/reset-password", superID), body: map[string]string{"new_password": newPw}},
	} {
		r.jar = mj
		expect(t, e.do(r), 403, "protected_account")
	}
	if e.activeSuperAdmins() != 1 {
		t.Fatal("super admin count changed")
	}
	// What a manager can do: create and assign ordinary roles whose permissions they already hold.
	w := e.do(req{method: "POST", path: "/api/v1/users", jar: mj, body: map[string]any{"username": "till1", "display_name": "Till One", "password": newPw, "roles": []string{"cashier"}}})
	expect(t, w, 201, "")
	if u := decodeInto[userResp](t, w).User; !u.MustChangePassword || len(u.Roles) != 1 || u.Roles[0] != "cashier" {
		t.Fatalf("created user = %+v", u)
	}
}

func TestAC11_RoleGrantNeedsHeldPermissions(t *testing.T) {
	e := newEnv(t)
	// A custom role carrying a permission the manager does not hold (roles.manage, owner decision S-13).
	e.exec(`INSERT INTO roles (code, name) VALUES ('role_admin', 'Role admin')`)
	e.exec(`INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p WHERE r.code = 'role_admin' AND p.key = 'roles.manage'`)
	target := e.addUser("someone1", false, "cashier")
	_, mj := e.signedInReauthed("manager1", "business_manager")
	path := fmt.Sprintf("/api/v1/users/%d/roles", target)
	expect(t, e.do(req{method: "PUT", path: path, jar: mj, body: map[string]any{"roles": []string{"role_admin"}}}), 403, "escalation_refused")
	// A system administrator can grant it (S-34).
	_, sj := e.signedInReauthed("owner1", "super_admin")
	expect(t, e.do(req{method: "PUT", path: path, jar: sj, body: map[string]any{"roles": []string{"role_admin"}}}), 200, "")
	// An unknown role code is a validation error, not a server error.
	expect(t, e.do(req{method: "PUT", path: path, jar: sj, body: map[string]any{"roles": []string{"no_such_role"}}}), 400, "validation_failed")
}

func TestUserAdminLifecycle(t *testing.T) {
	e := newEnv(t)
	_, sj := e.signedInReauthed("owner1", "super_admin")

	// create: no password echoed back, must change at first login, duplicate refused
	body := map[string]any{"username": "Waiter.One", "display_name": "  Waiter One ", "password": newPw, "roles": []string{"waiter"}}
	w := e.do(req{method: "POST", path: "/api/v1/users", jar: sj, body: body})
	expect(t, w, 201, "")
	if got := w.Body.String(); strings.Contains(got, newPw) || strings.Contains(got, "password_hash") {
		t.Fatalf("response leaks credentials: %s", got)
	}
	u := decodeInto[userResp](t, w).User
	if u.Username != "waiter.one" || u.DisplayName != "Waiter One" || !u.MustChangePassword {
		t.Fatalf("created = %+v", u)
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/users", jar: sj, body: body}), 409, "username_taken")
	expect(t, e.do(req{method: "POST", path: "/api/v1/users", jar: sj, body: map[string]any{"username": "x", "display_name": "X", "password": newPw}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/users", jar: sj, body: map[string]any{"username": "weakpw1", "display_name": "W", "password": "short"}}), 400, "validation_failed")
	if e.auditCount("user.created", u.ID) != 1 {
		t.Fatal("create not audited")
	}
	id := fmt.Sprint(u.ID)

	// list: filter, search, keyset paging
	for i := 0; i < 3; i++ {
		e.addUser(fmt.Sprintf("extra%d", i), false, "cashier")
	}
	page1 := decodeInto[listResp](t, e.do(req{method: "GET", path: "/api/v1/users?limit=2", jar: sj}))
	if len(page1.Items) != 2 || page1.NextCursor == "" {
		t.Fatalf("page1 = %+v", page1)
	}
	page2 := decodeInto[listResp](t, e.do(req{method: "GET", path: "/api/v1/users?limit=2&cursor=" + page1.NextCursor, jar: sj}))
	if len(page2.Items) == 0 || page2.Items[0].ID <= page1.Items[1].ID {
		t.Fatalf("page2 = %+v", page2)
	}
	found := decodeInto[listResp](t, e.do(req{method: "GET", path: "/api/v1/users?q=waiter", jar: sj}))
	if len(found.Items) != 1 || found.Items[0].ID != u.ID {
		t.Fatalf("search = %+v", found)
	}
	if n := len(decodeInto[listResp](t, e.do(req{method: "GET", path: "/api/v1/users?q=%25", jar: sj})).Items); n != 0 {
		t.Fatalf("a literal %% must not match everything, got %d", n)
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/users?status=bogus", jar: sj}), 400, "validation_failed")
	expect(t, e.do(req{method: "GET", path: "/api/v1/users?cursor=!!", jar: sj}), 400, "validation_failed")
	expect(t, e.do(req{method: "GET", path: "/api/v1/users/999999", jar: sj}), 404, "not_found")
	expect(t, e.do(req{method: "GET", path: "/api/v1/users/abc", jar: sj}), 404, "not_found")

	// edit: audited with before and after
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/users/" + id, jar: sj, body: map[string]string{"display_name": "Waiter Uno"}}), 200, "")
	var before, after string
	if err := e.pool.QueryRow(context.Background(), `SELECT before_json->>'display_name', after_json->>'display_name' FROM audit_logs WHERE action = 'user.updated' AND entity_id = $1`, id).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if before != "Waiter One" || after != "Waiter Uno" {
		t.Fatalf("audit before/after = %q / %q", before, after)
	}

	// PIN: floor roles only
	expect(t, e.do(req{method: "PUT", path: "/api/v1/users/" + id + "/pin", jar: sj, body: map[string]string{"pin": "4821"}}), 204, "")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/users/" + id + "/pin", jar: sj, body: map[string]string{"pin": "1234"}}), 400, "validation_failed")
	mgr := e.addUser("mgr9", false, "business_manager")
	expect(t, e.do(req{method: "PUT", path: fmt.Sprintf("/api/v1/users/%d/pin", mgr), jar: sj, body: map[string]string{"pin": "4821"}}), 409, "pin_not_allowed")

	// the waiter signs in (after a forced change), then deactivation ends the session at once
	wj := jar{}
	if w := e.login("waiter.one", newPw, wj); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/auth/change-password", jar: wj, body: map[string]string{"current_password": newPw, "new_password": "Waiter-own-Passw0rd-5"}}), 204, "")
	expect(t, e.do(req{method: "GET", path: "/api/v1/auth/me", jar: wj}), 200, "")
	w = e.do(req{method: "POST", path: "/api/v1/users/" + id + "/deactivate", jar: sj, body: map[string]string{"reason": "left the venue"}})
	expect(t, w, 200, "")
	if got := decodeInto[userResp](t, w).User; got.Status != "inactive" || got.DeactivatedAt == nil {
		t.Fatalf("deactivated = %+v", got)
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/auth/me", jar: wj}), 401, "unauthenticated")
	if w := e.login("waiter.one", "Waiter-own-Passw0rd-5", jar{}); w.Code == 200 {
		t.Fatal("a deactivated user could sign in")
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/users/" + id + "/deactivate", jar: sj}), 200, "") // repeating is harmless
	if e.auditCount("user.deactivated", u.ID) != 1 {
		t.Fatal("deactivation should be audited once")
	}

	// reactivate, then a password reset forces a change and ends sessions
	expect(t, e.do(req{method: "POST", path: "/api/v1/users/" + id + "/reactivate", jar: sj}), 200, "")
	expect(t, e.do(req{method: "POST", path: "/api/v1/users/" + id + "/reset-password", jar: sj, body: map[string]string{"new_password": "weak"}}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/users/" + id + "/reset-password", jar: sj, body: map[string]string{"new_password": newPw}}), 204, "")
	if w := e.login("waiter.one", "Waiter-own-Passw0rd-5", jar{}); w.Code == 200 {
		t.Fatal("old password still works after a reset")
	}
	rj := jar{}
	if w := e.login("waiter.one", newPw, rj); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, me := e.me(rj); !me.MustChange {
		t.Fatal("reset must force a password change")
	}
	var secrets int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE after_json::text LIKE '%' || $1 || '%' OR before_json::text LIKE '%' || $1 || '%'`, newPw).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatalf("audit holds a password: %d %v", secrets, err)
	}

	// delete: only an account with no history
	fresh := decodeInto[userResp](t, e.do(req{method: "POST", path: "/api/v1/users", jar: sj, body: map[string]any{"username": "never.used", "display_name": "Never Used", "password": newPw, "roles": []string{"cashier"}}})).User
	expect(t, e.do(req{method: "DELETE", path: fmt.Sprintf("/api/v1/users/%d", fresh.ID), jar: sj}), 204, "")
	expect(t, e.do(req{method: "GET", path: fmt.Sprintf("/api/v1/users/%d", fresh.ID), jar: sj}), 404, "not_found")
	expect(t, e.do(req{method: "DELETE", path: "/api/v1/users/" + id, jar: sj}), 409, "has_history")
	if got := decodeInto[userResp](t, e.do(req{method: "GET", path: "/api/v1/users/" + id, jar: sj})).User; got.Username != "waiter.one" {
		t.Fatal("a refused delete must leave the user in place")
	}
}

func TestReauthRequiredForSensitiveUserChanges(t *testing.T) {
	e := newEnv(t)
	e.addUser("owner1", false, "super_admin")
	sj := e.mustLogin("owner1") // signed in, but no password re-entry yet
	target := e.addUser("target1", false, "cashier")
	id := fmt.Sprint(target)
	sensitive := []req{
		{method: "PUT", path: "/api/v1/users/" + id + "/roles", body: map[string]any{"roles": []string{"waiter"}}},
		{method: "POST", path: "/api/v1/users/" + id + "/deactivate"},
		{method: "POST", path: "/api/v1/users/" + id + "/reactivate"},
		{method: "POST", path: "/api/v1/users/" + id + "/reset-password", body: map[string]string{"new_password": newPw}},
		{method: "DELETE", path: "/api/v1/users/" + id},
	}
	for _, r := range sensitive {
		r.jar = sj
		expect(t, e.do(r), 403, "reauth_required")
	}
	// Ordinary edits do not need it.
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/users/" + id, jar: sj, body: map[string]string{"display_name": "Target One"}}), 200, "")
	e.reauth(sj)
	expect(t, e.do(req{method: "PUT", path: "/api/v1/users/" + id + "/roles", jar: sj, body: map[string]any{"roles": []string{"waiter"}}}), 200, "")
	e.clock.Advance(6 * time.Minute) // the 5-minute window (S-16) has passed
	expect(t, e.do(req{method: "POST", path: "/api/v1/users/" + id + "/deactivate", jar: sj}), 403, "reauth_required")
	// Nobody changes their own status, roles or password through the admin routes.
	e.reauth(sj)
	_, me := e.me(sj)
	self := fmt.Sprint(me.User.ID)
	expect(t, e.do(req{method: "POST", path: "/api/v1/users/" + self + "/deactivate", jar: sj}), 409, "self_action_refused")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/users/" + self + "/roles", jar: sj, body: map[string]any{"roles": []string{}}}), 409, "self_action_refused")
	expect(t, e.do(req{method: "DELETE", path: "/api/v1/users/" + self, jar: sj}), 409, "self_action_refused")
}

// AC-12: the last active Super Admin can never be deactivated, lose the role or be deleted, whoever asks.
func TestAC12_LastSuperAdminCannotBeRemoved(t *testing.T) {
	e := newEnv(t)
	sole := e.addUser("sole.owner", false, "super_admin")
	actorID := e.addUser("operator1", false) // holds nothing in the database; the principal below stands in for any admin
	actor := e.principal(actorID, "operator1", "users.update", "users.deactivate", "users.delete", "system.admin")
	ctx := context.Background()

	if _, err := e.svc.Deactivate(ctx, actor, sole, "", service.ClientInfo{}); !errors.Is(err, service.ErrLastSuperAdmin) {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := e.svc.SetUserRoles(ctx, actor, sole, []string{}, service.ClientInfo{}); !errors.Is(err, service.ErrLastSuperAdmin) {
		t.Fatalf("remove role: %v", err)
	}
	if _, err := e.svc.SetUserRoles(ctx, actor, sole, []string{"business_manager"}, service.ClientInfo{}); !errors.Is(err, service.ErrLastSuperAdmin) {
		t.Fatalf("replace role: %v", err)
	}
	if err := e.svc.DeleteUser(ctx, actor, sole, service.ClientInfo{}); !errors.Is(err, service.ErrLastSuperAdmin) {
		t.Fatalf("delete: %v", err)
	}
	if e.activeSuperAdmins() != 1 {
		t.Fatal("the last Super Admin was removed")
	}
	if n := e.auditCount("user.deactivated", sole) + e.auditCount("user.roles_changed", sole) + e.auditCount("user.deleted", sole); n != 0 {
		t.Fatalf("refused operations must not leave audit rows of success, got %d", n)
	}
	// With a second Super Admin in place, the first can go.
	second := e.addUser("second.owner", false, "super_admin")
	if _, err := e.svc.Deactivate(ctx, actor, second, "", service.ClientInfo{}); err != nil {
		t.Fatalf("deactivating one of two: %v", err)
	}
	if _, err := e.svc.Deactivate(ctx, actor, sole, "", service.ClientInfo{}); !errors.Is(err, service.ErrLastSuperAdmin) {
		t.Fatalf("the survivor must stay: %v", err)
	}
}

func TestAC12_ConcurrentDeactivationLeavesOne(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for round := 0; round < 5; round++ {
		a := e.addUser(fmt.Sprintf("admin.a%d", round), false, "super_admin")
		b := e.addUser(fmt.Sprintf("admin.b%d", round), false, "super_admin")
		// Each round starts with exactly these two active Super Admins: retire any survivor from an earlier round.
		e.exec(`UPDATE users SET status = 'inactive', deactivated_at = now()
		        WHERE status = 'active' AND id NOT IN ($1, $2)
		          AND id IN (SELECT ur.user_id FROM user_roles ur JOIN roles r ON r.id = ur.role_id AND r.code = 'super_admin')`, a, b)
		if e.activeSuperAdmins() != 2 {
			t.Fatalf("round %d: setup wants exactly two active Super Admins", round)
		}
		pa := e.principal(a, "a", "users.deactivate", "system.admin")
		pb := e.principal(b, "b", "users.deactivate", "system.admin")
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i, c := range []struct {
			actor  *auth.Principal
			target int64
		}{{pa, b}, {pb, a}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = e.svc.Deactivate(ctx, c.actor, c.target, "", service.ClientInfo{})
			}()
		}
		close(start)
		wg.Wait()
		ok, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, service.ErrLastSuperAdmin):
				refused++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("round %d: %d succeeded, %d refused (want 1 and 1)", round, ok, refused)
		}
		if got := e.activeSuperAdmins(); got < 1 {
			t.Fatalf("round %d: no active Super Admin left", round)
		}
	}
}

func TestAC12_DeactivationKeepsHistory(t *testing.T) {
	e := newEnv(t)
	_, sj := e.signedInReauthed("owner1", "super_admin")
	waiter := e.addUser("waiter9", false, "waiter")
	wj := e.mustLogin("waiter9")
	expect(t, e.do(req{method: "GET", path: "/api/v1/auth/me", jar: wj}), 200, "")
	var auditBefore, sessionsBefore int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE actor_user_id = $1`, waiter).Scan(&auditBefore)
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id = $1`, waiter).Scan(&sessionsBefore)
	if auditBefore == 0 || sessionsBefore == 0 {
		t.Fatalf("test setup: expected history, got audit=%d sessions=%d", auditBefore, sessionsBefore)
	}
	expect(t, e.do(req{method: "POST", path: fmt.Sprintf("/api/v1/users/%d/deactivate", waiter), jar: sj}), 200, "")
	var status string
	var auditAfter, sessionsAfter, active int
	_ = e.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, waiter).Scan(&status)
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE actor_user_id = $1`, waiter).Scan(&auditAfter)
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL) FROM sessions WHERE user_id = $1`, waiter).Scan(&sessionsAfter, &active)
	if status != "inactive" || auditAfter != auditBefore || sessionsAfter != sessionsBefore || active != 0 {
		t.Fatalf("status=%s audit %d->%d sessions %d->%d active=%d", status, auditBefore, auditAfter, sessionsBefore, sessionsAfter, active)
	}
	expect(t, e.do(req{method: "DELETE", path: fmt.Sprintf("/api/v1/users/%d", waiter), jar: sj}), 409, "has_history")
}
