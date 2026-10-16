package handler_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func errCode(w *httptest.ResponseRecorder) string {
	var b struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Error.Code
}

// AC-10: every protected route from docs/api/routes.csv that exists at this stage denies a signed-out caller (401)
// and a signed-in caller without the permission (403 forbidden), for each of the nine default roles; a caller who
// holds the permission is never turned away with "forbidden" (other answers such as 400 or reauth_required are fine).
// The route list is read from the file, so a route added to the CSV without a guard fails here.
func TestAC10_EveryProtectedRouteDeniesWithoutPermission(t *testing.T) {
	f, err := os.Open("../../../docs/api/routes.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	type route struct{ method, path, perm string }
	var routes []route
	special := map[string]bool{"public": true, "terminal session": true, "system": true, "provider authenticity (unspecified)": true}
	for _, r := range recs[1:] {
		stage, _ := strconv.Atoi(r[4])
		if stage > 9 || special[r[2]] {
			continue
		}
		// Stage 6 session routes (logout, change-password) would end the shared test session; their guards are
		// covered by the stage 6 tests. Stage 7, 8 and 9 routes and every permission-named route are checked here.
		if stage < 7 && r[2] == "authenticated" {
			continue
		}
		routes = append(routes, route{r[0], strings.NewReplacer("{id}", "1").Replace(r[1]), r[2]})
	}
	if len(routes) < 35 { // stage 7, 8 and 9 rows
		t.Fatalf("only %d routes read from routes.csv", len(routes))
	}

	e := newEnv(t)
	for _, rt := range routes {
		w := e.do(req{method: rt.method, path: rt.path, body: map[string]any{}})
		if w.Code != 401 {
			t.Errorf("signed out %s %s = %d, want 401 (is the route registered and guarded?)", rt.method, rt.path, w.Code)
		}
	}
	rows, err := e.pool.Query(context.Background(), `SELECT code FROM roles WHERE is_template ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		codes = append(codes, c)
	}
	rows.Close()
	if len(codes) != 9 {
		t.Fatalf("template roles = %v", codes)
	}
	for _, code := range codes {
		username := "ac10_" + code
		e.addUser(username, false, code)
		j := e.mustLogin(username)
		_, me := e.me(j)
		for _, rt := range routes {
			w := e.do(req{method: rt.method, path: rt.path, jar: j, body: map[string]any{}})
			holds := rt.perm == "authenticated" || has(me.Permissions, rt.perm)
			switch {
			case !holds && (w.Code != 403 || errCode(w) != "forbidden"):
				t.Errorf("role %s without %s: %s %s = %d %s, want 403 forbidden", code, rt.perm, rt.method, rt.path, w.Code, errCode(w))
			case holds && (w.Code == 401 || errCode(w) == "forbidden"):
				t.Errorf("role %s holds %s: %s %s was refused with %d %s", code, rt.perm, rt.method, rt.path, w.Code, errCode(w))
			}
		}
	}
}
