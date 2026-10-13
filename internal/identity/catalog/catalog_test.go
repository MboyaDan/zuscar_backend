package catalog

import (
	"encoding/csv"
	"os"
	"regexp"
	"strings"
	"testing"
)

var matrixRow = regexp.MustCompile("(?m)^\\| `([a-z_.]+)\\*?` \\| (.*) \\|$")

func TestCatalogInternallyConsistent(t *testing.T) {
	keys := map[string]bool{}
	for _, p := range Permissions {
		if keys[p.Key] {
			t.Errorf("duplicate permission %s", p.Key)
		}
		keys[p.Key] = true
	}
	if len(keys) != 34 {
		t.Errorf("expected 34 permissions, have %d", len(keys))
	}
	codes := map[string]bool{}
	for _, r := range Roles {
		if codes[r.Code] {
			t.Errorf("duplicate role %s", r.Code)
		}
		codes[r.Code] = true
		for _, k := range r.Permissions {
			if !keys[k] {
				t.Errorf("role %s grants unknown permission %s", r.Code, k)
			}
		}
	}
}

// AC-11 / NN-07: only the protected Super Admin role may carry a protected permission,
// and Business Manager holds neither role administration key (owner decision, S-13).
func TestProtectedRulesInTemplates(t *testing.T) {
	protected := map[string]bool{}
	for _, p := range Permissions {
		if p.Protected {
			protected[p.Key] = true
		}
	}
	for _, r := range Roles {
		for _, k := range r.Permissions {
			if protected[k] && !r.Protected {
				t.Errorf("non-protected role %s holds protected permission %s", r.Code, k)
			}
			if r.Code == "business_manager" && (k == "roles.manage" || k == "permissions.assign" || k == "system.admin") {
				t.Errorf("business_manager must not hold %s", k)
			}
		}
	}
}

// The templates must match the matrix in docs/rbac.md exactly (cells marked full or limited are grants).
func TestCatalogMatchesRBACDocument(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/rbac.md")
	if err != nil {
		t.Fatal(err)
	}
	codes := []string{"super_admin", "business_manager", "cashier", "waiter", "bartender", "receptionist", "storekeeper", "kitchen_staff", "accountant_auditor"}
	want := map[string]map[string]bool{}
	for _, c := range codes {
		want[c] = map[string]bool{}
	}
	rows := matrixRow.FindAllStringSubmatch(string(raw), -1)
	if len(rows) != 34 {
		t.Fatalf("expected 34 matrix rows in docs/rbac.md, parsed %d", len(rows))
	}
	for _, m := range rows {
		cells := strings.Split(m[2], "|")
		if len(cells) != len(codes) {
			t.Fatalf("row %s has %d cells", m[1], len(cells))
		}
		for i, cell := range cells {
			if cell = strings.TrimSpace(cell); cell != "" && cell != "·" {
				want[codes[i]][m[1]] = true
			}
		}
	}
	for _, r := range Roles {
		got := map[string]bool{}
		for _, k := range r.Permissions {
			got[k] = true
		}
		for k := range want[r.Code] {
			if !got[k] {
				t.Errorf("%s: docs/rbac.md grants %s but catalog does not", r.Code, k)
			}
		}
		for k := range got {
			if !want[r.Code][k] {
				t.Errorf("%s: catalog grants %s but docs/rbac.md does not", r.Code, k)
			}
		}
	}
}

// AC-10 groundwork: every permission named by a planned route exists in the catalog.
func TestRoutePermissionsExistInCatalog(t *testing.T) {
	f, err := os.Open("../../../docs/api/routes.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, p := range Permissions {
		keys[p.Key] = true
	}
	special := map[string]bool{"public": true, "authenticated": true, "terminal session": true, "system": true, "provider authenticity (unspecified)": true}
	for _, r := range recs[1:] {
		if perm := r[2]; !keys[perm] && !special[perm] {
			t.Errorf("route %s %s names unknown permission %q", r[0], r[1], perm)
		}
	}
}
