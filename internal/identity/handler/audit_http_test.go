package handler_test

import (
	"fmt"
	"testing"
)

func TestAuditLogsScope(t *testing.T) {
	e := newEnv(t)
	_, sj := e.signedInReauthed("owner1", "super_admin")
	for i := 0; i < 3; i++ {
		expect(t, e.do(req{method: "POST", path: "/api/v1/users", jar: sj, body: map[string]any{"username": fmt.Sprintf("staff%d", i), "display_name": "Staff", "password": newPw, "roles": []string{"cashier"}}}), 201, "")
	}
	e.addUser("mgr1", false, "business_manager")
	e.addUser("auditor1", false, "accountant_auditor")
	e.addUser("till1", false, "cashier")
	type page struct {
		Items []struct {
			ID         int64  `json:"id"`
			Action     string `json:"action"`
			EntityType string `json:"entity_type"`
			ActorID    *int64 `json:"actor_user_id"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	get := func(j jar, query string) page {
		w := e.do(req{method: "GET", path: "/api/v1/audit-logs" + query, jar: j})
		expect(t, w, 200, "")
		return decodeInto[page](t, w)
	}
	all := get(sj, "?limit=200")
	if len(all.Items) < 5 {
		t.Fatalf("super admin sees %d events", len(all.Items))
	}
	for i := 1; i < len(all.Items); i++ {
		if all.Items[i].ID >= all.Items[i-1].ID {
			t.Fatal("events must be newest first")
		}
	}
	// the auditor sees security events; a business manager (operational only) sees none of them; a cashier has no access
	if n := len(get(e.mustLogin("auditor1"), "?limit=200").Items); n < len(all.Items) {
		t.Fatalf("auditor sees %d of %d events", n, len(all.Items))
	}
	if n := len(get(e.mustLogin("mgr1"), "?limit=200").Items); n != 0 {
		t.Fatalf("business manager saw %d security events", n)
	}
	expect(t, e.do(req{method: "GET", path: "/api/v1/audit-logs", jar: e.mustLogin("till1")}), 403, "forbidden")
	expect(t, e.do(req{method: "GET", path: "/api/v1/audit-logs"}), 401, "")

	// filters and paging
	created := get(sj, "?action=user.created&limit=200")
	if len(created.Items) != 3 {
		t.Fatalf("user.created = %d", len(created.Items))
	}
	p1 := get(sj, "?limit=2")
	if len(p1.Items) != 2 || p1.NextCursor == "" {
		t.Fatalf("page1 = %+v", p1)
	}
	p2 := get(sj, "?limit=2&cursor="+p1.NextCursor)
	if len(p2.Items) == 0 || p2.Items[0].ID >= p1.Items[1].ID {
		t.Fatalf("page2 = %+v", p2)
	}
	if n := len(get(sj, "?entity_type=role").Items); n != 0 {
		t.Fatalf("role events = %d", n)
	}
	if n := len(get(sj, "?from=2099-01-01T00:00:00Z").Items); n != 0 {
		t.Fatal("future from-date must return nothing")
	}
	for _, q := range []string{"?result=bogus", "?actor_id=x", "?from=yesterday", "?limit=0", "?cursor=abc"} {
		expect(t, e.do(req{method: "GET", path: "/api/v1/audit-logs" + q, jar: sj}), 400, "validation_failed")
	}
}
