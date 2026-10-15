package handler_test

import (
	"context"
	"fmt"
	"testing"

	"zuscar/backend/internal/identity/service"
)

type roleResp struct {
	Role     service.RoleView       `json:"role"`
	Affected []service.AffectedUser `json:"affected_users"`
}

type roleList struct {
	Items []service.RoleView `json:"items"`
}

func (e *testEnv) roleID(code string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM roles WHERE code = $1`, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestRoleAdminLifecycle(t *testing.T) {
	e := newEnv(t)
	e.addUser("owner1", false, "super_admin")
	sj := e.mustLogin("owner1") // signed in, no password re-entry yet

	// reading needs only roles.manage; changing needs a recent password entry
	list := decodeInto[roleList](t, e.do(req{method: "GET", path: "/api/v1/roles", jar: sj}))
	protected := 0
	for _, r := range list.Items {
		if r.Protected {
			protected++
		}
	}
	if len(list.Items) < 9 || protected != 1 {
		t.Fatalf("roles = %d, protected = %d", len(list.Items), protected)
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/permissions", jar: sj}), 200, "")
	create := map[string]string{"code": "night_porter", "name": "Night porter", "description": "Overnight front desk"}
	expect(t, e.do(req{method: "POST", path: "/api/v1/roles", jar: sj, body: create}), 403, "reauth_required")
	e.reauth(sj)

	w := e.do(req{method: "POST", path: "/api/v1/roles", jar: sj, body: create})
	expect(t, w, 201, "")
	role := decodeInto[roleResp](t, w).Role
	if role.Protected || role.Template || len(role.Permissions) != 0 || role.UserCount != 0 {
		t.Fatalf("created = %+v", role)
	}
	id := fmt.Sprint(role.ID)
	expect(t, e.do(req{method: "POST", path: "/api/v1/roles", jar: sj, body: create}), 409, "role_code_taken")
	expect(t, e.do(req{method: "POST", path: "/api/v1/roles", jar: sj, body: map[string]string{"code": "Bad Code", "name": "X"}}), 400, "validation_failed")

	// edit: audited with before and after, code never changes
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/roles/" + id, jar: sj, body: map[string]string{"name": "Night desk", "description": ""}}), 200, "")
	var before, after string
	if err := e.pool.QueryRow(context.Background(), `SELECT before_json->>'name', after_json->>'name' FROM audit_logs WHERE action = 'role.updated' AND entity_type = 'role' AND entity_id = $1`, id).Scan(&before, &after); err != nil || before != "Night porter" || after != "Night desk" {
		t.Fatalf("audit = %q / %q / %v", before, after, err)
	}

	// permissions: grant two, the response lists affected users, then shrink
	holder := e.addUser("porter1", false, "night_porter")
	w = e.do(req{method: "PUT", path: "/api/v1/roles/" + id + "/permissions", jar: sj, body: map[string]any{"permissions": []string{"reports.view", "users.update"}}})
	expect(t, w, 200, "")
	got := decodeInto[roleResp](t, w)
	if len(got.Role.Permissions) != 2 || len(got.Affected) != 1 || got.Affected[0].ID != holder {
		t.Fatalf("after grant = %+v", got)
	}
	// permissions apply on the holder's next request
	pj := e.mustLogin("porter1")
	if _, me := e.me(pj); !has(me.Permissions, "reports.view") {
		t.Fatal("granted permission not visible on the next request")
	}
	expect(t, e.do(req{method: "PUT", path: "/api/v1/roles/" + id + "/permissions", jar: sj, body: map[string]any{"permissions": []string{"reports.view"}}}), 200, "")
	if _, me := e.me(pj); has(me.Permissions, "users.update") {
		t.Fatal("removed permission still effective")
	}
	var changes int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = 'role.permissions_changed' AND entity_id = $1 AND before_json IS NOT NULL AND after_json IS NOT NULL`, id).Scan(&changes)
	if changes != 2 {
		t.Fatalf("permission changes audited %d times, want 2", changes)
	}
	expect(t, e.do(req{method: "PUT", path: "/api/v1/roles/" + id + "/permissions", jar: sj, body: map[string]any{"permissions": []string{"no.such_key"}}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/roles/" + id + "/permissions", jar: sj, body: map[string]any{}}), 400, "validation_failed")

	// protected roles and the protected permission are out of reach, even for a Super Admin
	sa := fmt.Sprint(e.roleID("super_admin"))
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/roles/" + sa, jar: sj, body: map[string]string{"name": "Mine"}}), 403, "protected_role")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/roles/" + sa + "/permissions", jar: sj, body: map[string]any{"permissions": []string{}}}), 403, "protected_role")
	expect(t, e.do(req{method: "DELETE", path: "/api/v1/roles/" + sa, jar: sj}), 403, "protected_role")
	expect(t, e.do(req{method: "PUT", path: "/api/v1/roles/" + id + "/permissions", jar: sj, body: map[string]any{"permissions": []string{"system.admin"}}}), 403, "protected_role")

	// delete: refused while assigned, allowed once nobody holds it
	expect(t, e.do(req{method: "DELETE", path: "/api/v1/roles/" + id, jar: sj}), 409, "role_in_use")
	expect(t, e.do(req{method: "PUT", path: fmt.Sprintf("/api/v1/users/%d/roles", holder), jar: sj, body: map[string]any{"roles": []string{"cashier"}}}), 200, "")
	expect(t, e.do(req{method: "DELETE", path: "/api/v1/roles/" + id, jar: sj}), 204, "")
	expect(t, e.do(req{method: "DELETE", path: "/api/v1/roles/" + id, jar: sj}), 404, "not_found")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/roles/abc", jar: sj, body: map[string]string{"name": "X"}}), 404, "not_found")
}

// A custom role that is given roles.manage and permissions.assign still cannot reach system.admin or Super Admin,
// and can only hand out permissions it holds (rbac.md section 3, rules 1 and 2).
func TestAC11_RoleAdminWithoutSystemAdminCannotGrantIt(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO roles (code, name) VALUES ('role_admin', 'Role admin')`)
	e.exec(`INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p
	        WHERE r.code = 'role_admin' AND p.key IN ('roles.manage', 'permissions.assign', 'users.update', 'reports.view')`)
	e.exec(`INSERT INTO roles (code, name) VALUES ('shift_lead', 'Shift lead')`)
	_, rj := e.signedInReauthed("roleadmin1", "role_admin")
	victim := e.addUser("victim1", false, "cashier")
	lead := fmt.Sprint(e.roleID("shift_lead"))
	path := "/api/v1/roles/" + lead + "/permissions"

	expect(t, e.do(req{method: "PUT", path: path, jar: rj, body: map[string]any{"permissions": []string{"system.admin"}}}), 403, "protected_role")
	expect(t, e.do(req{method: "PUT", path: path, jar: rj, body: map[string]any{"permissions": []string{"payments.reconcile"}}}), 403, "escalation_refused")
	expect(t, e.do(req{method: "PUT", path: path, jar: rj, body: map[string]any{"permissions": []string{"reports.view"}}}), 200, "")
	sa := fmt.Sprint(e.roleID("super_admin"))
	expect(t, e.do(req{method: "PUT", path: "/api/v1/roles/" + sa + "/permissions", jar: rj, body: map[string]any{"permissions": []string{"reports.view"}}}), 403, "protected_role")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/roles/" + sa, jar: rj, body: map[string]string{"name": "Mine"}}), 403, "protected_role")
	expect(t, e.do(req{method: "PUT", path: fmt.Sprintf("/api/v1/users/%d/roles", victim), jar: rj, body: map[string]any{"roles": []string{"super_admin"}}}), 403, "escalation_refused")
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id WHERE p.key = 'system.admin' AND rp.role_id NOT IN (SELECT id FROM roles WHERE is_protected)`).Scan(&n)
	if n != 0 {
		t.Fatal("system.admin ended up on a non-protected role")
	}
}
