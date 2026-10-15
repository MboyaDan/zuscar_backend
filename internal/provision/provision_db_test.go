package provision_test

import (
	"context"
	"encoding/json"
	"testing"

	"zuscar/backend/internal/identity/catalog"
	"zuscar/backend/internal/identity/password"
	"zuscar/backend/internal/platform/testdb"
	"zuscar/backend/internal/provision"
)

func opts() provision.BootstrapOptions {
	return provision.BootstrapOptions{Username: "owner.real", DisplayName: "Real Owner", Password: "correct-horse-battery-9",
		BaseOptions: provision.BaseOptions{BusinessName: "Test Venue", TaxName: "Test VAT", TaxRateBP: 1600, Timezone: "Africa/Nairobi", ReceiptStart: 5001}}
}

// Prompt section 8: bootstrap creates the first Super Admin and the templates, and nothing else.
func TestBootstrapCreatesOnlyTheMinimum(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	uid, err := provision.Bootstrap(ctx, pool, opts())
	if err != nil {
		t.Fatal(err)
	}
	n := func(sql string) int {
		var c int
		if err := pool.QueryRow(ctx, sql).Scan(&c); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return c
	}
	if got := n(`SELECT count(*) FROM users`); got != 1 {
		t.Fatalf("users = %d, want 1", got)
	}
	var must bool
	var hash, status string
	if err := pool.QueryRow(ctx, `SELECT must_change_password, password_hash, status FROM users WHERE id = $1`, uid).Scan(&must, &hash, &status); err != nil {
		t.Fatal(err)
	}
	if !must || status != "active" {
		t.Fatalf("must_change_password=%v status=%s", must, status)
	}
	if password.Verify("correct-horse-battery-9", hash) != nil || password.Verify(provision.KnownDevPassword, hash) == nil {
		t.Fatal("stored hash must verify the supplied password and not the dev password")
	}
	if got := n(`SELECT count(*) FROM permissions`); got != len(catalog.Permissions) {
		t.Fatalf("permissions = %d", got)
	}
	if got := n(`SELECT count(*) FROM roles`); got != len(catalog.Roles) {
		t.Fatalf("roles = %d", got)
	}
	want := 0
	for _, r := range catalog.Roles {
		want += len(r.Permissions)
	}
	if got := n(`SELECT count(*) FROM role_permissions`); got != want {
		t.Fatalf("role_permissions = %d, want %d", got, want)
	}
	if got := n(`SELECT count(*) FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE r.code = 'super_admin'`); got != 1 {
		t.Fatalf("super admins = %d", got)
	}
	// Owner instruction: standard, zero-rated and exempt categories; the 16 percent value is provisional.
	if got := n(`SELECT count(*) FROM tax_rates WHERE (code, treatment, rate_bp) IN (('standard','standard',1600), ('zero_rated','zero_rated',0), ('exempt','exempt',0))`); got != 3 {
		t.Fatalf("expected the three tax categories, matched %d", got)
	}
	var taxStatus []byte
	_ = pool.QueryRow(ctx, `SELECT value FROM system_settings WHERE key = 'tax_config_status'`).Scan(&taxStatus)
	if string(taxStatus) != `"provisional_unverified"` {
		t.Fatalf("tax_config_status = %s, want provisional_unverified", taxStatus)
	}
	var bn []byte
	_ = pool.QueryRow(ctx, `SELECT value FROM system_settings WHERE key = 'business_name'`).Scan(&bn)
	var name string
	_ = json.Unmarshal(bn, &name)
	if name != "Test Venue" {
		t.Fatalf("business_name = %q", name)
	}
	if got := n(`SELECT next_value FROM document_counters WHERE doc_type = 'receipt'`); got != 5001 {
		t.Fatalf("receipt counter = %d, want 5001 (continues the receipt book)", got)
	}
	if got := n(`SELECT count(*) FROM document_counters WHERE doc_type <> 'receipt' AND next_value <> 1`); got != 0 {
		t.Fatal("other counters must start at 1")
	}
	for _, tbl := range []string{"sessions", "login_attempts", "user_pins"} {
		if got := n(`SELECT count(*) FROM ` + tbl); got != 0 {
			t.Errorf("%s should be empty after bootstrap, has %d", tbl, got)
		}
	}
	if got := n(`SELECT count(*) FROM audit_logs WHERE action = 'bootstrap.super_admin_created' AND actor_label = 'cli:bootstrap'`); got != 1 {
		t.Fatal("bootstrap must write exactly one audit row")
	}
}

func TestBootstrapRefusesWhenUsersExist(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	if _, err := provision.Bootstrap(ctx, pool, opts()); err != nil {
		t.Fatal(err)
	}
	second := opts()
	second.Username = "owner.two"
	if _, err := provision.Bootstrap(ctx, pool, second); err != provision.ErrAlreadyBootstrapped {
		t.Fatalf("second bootstrap: want ErrAlreadyBootstrapped, got %v", err)
	}
	var users int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users)
	if users != 1 {
		t.Fatalf("users = %d after refused bootstrap", users)
	}
}

func TestFailedBootstrapLeavesNothingBehind(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	bad := opts()
	bad.Username = "x" // fails validation
	if _, err := provision.Bootstrap(ctx, pool, bad); err == nil {
		t.Fatal("expected error")
	}
	var perms int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM permissions`).Scan(&perms)
	if perms != 0 {
		t.Fatalf("a failed bootstrap left %d permissions behind", perms)
	}
}

// A-05: re-applying base data must never reset a counter (that would break gapless numbering).
func TestApplyBaseDataKeepsExistingCounters(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	apply := func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := provision.ApplyBaseData(ctx, tx, opts().BaseOptions); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	if _, err := pool.Exec(ctx, `UPDATE document_counters SET next_value = 77 WHERE doc_type = 'receipt'`); err != nil {
		t.Fatal(err)
	}
	apply()
	var v int
	_ = pool.QueryRow(ctx, `SELECT next_value FROM document_counters WHERE doc_type = 'receipt'`).Scan(&v)
	if v != 77 {
		t.Fatalf("receipt counter reset to %d", v)
	}
}

func TestProvisionalRateConstantIsOwnerValue(t *testing.T) {
	if provision.ProvisionalStandardRateBP != 1600 {
		t.Fatalf("provisional standard rate = %d basis points, want 1600", provision.ProvisionalStandardRateBP)
	}
}

// Owner instruction: the tax rate stays configuration and the categories are constrained in the database.
func TestTaxCategoryConstraints(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	bad := []string{
		`INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ('z5', 'Zero with a rate', 500, 'zero_rated')`,
		`INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ('e5', 'Exempt with a rate', 500, 'exempt')`,
		`INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ('odd', 'Unknown treatment', 0, 'reduced')`,
		`INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ('neg', 'Negative', -1, 'standard')`,
		`INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ('huge', 'Over 100 percent', 10001, 'standard')`,
	}
	for _, q := range bad {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("should have been rejected: %s", q)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ('special_std', 'Another standard-type rate', 800, 'standard')`); err != nil {
		t.Fatalf("a second standard-treatment category must be possible (rates are configuration): %v", err)
	}
}
