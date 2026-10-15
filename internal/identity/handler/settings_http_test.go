package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

type settingsResp struct {
	Settings map[string]json.RawMessage `json:"settings"`
}

type taxRateItem struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	RateBP    int32  `json:"rate_bp"`
	Treatment string `json:"treatment"`
	Active    bool   `json:"active"`
}

func (e *testEnv) setting(j jar, key string) string {
	e.t.Helper()
	w := e.do(req{method: "GET", path: "/api/v1/settings", jar: j})
	expect(e.t, w, 200, "")
	return string(decodeInto[settingsResp](e.t, w).Settings[key])
}

func (e *testEnv) taxRates(j jar) []taxRateItem {
	e.t.Helper()
	w := e.do(req{method: "GET", path: "/api/v1/tax-rates", jar: j})
	expect(e.t, w, 200, "")
	return decodeInto[struct {
		Items []taxRateItem `json:"items"`
	}](e.t, w).Items
}

func (e *testEnv) patchSettings(j jar, changes map[string]any) *httptest.ResponseRecorder {
	return e.do(req{method: "PATCH", path: "/api/v1/settings", jar: j, body: map[string]any{"settings": changes}})
}

func (e *testEnv) auditN(action string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestSettingsAndTaxLifecycle(t *testing.T) {
	e := newEnv(t)
	e.addUser("till1", false, "cashier")
	cj := e.mustLogin("till1")
	// Anyone signed in can read; only a system administrator can change.
	expect(t, e.do(req{method: "GET", path: "/api/v1/settings"}), 401, "")
	if got := e.setting(cj, "business_name"); got != `"Test"` {
		t.Fatalf("business_name = %s", got)
	}
	if len(e.taxRates(cj)) != 3 {
		t.Fatal("want the three provisional tax categories")
	}
	expect(t, e.patchSettings(cj, map[string]any{"business_name": "Mine"}), 403, "forbidden")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: cj, body: map[string]any{"code": "x1", "name": "X", "rate_bp": 0, "treatment": "exempt"}}), 403, "forbidden")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/tax-rates/1", jar: cj, body: map[string]any{"active": false}}), 403, "forbidden")

	e.addUser("owner1", false, "super_admin")
	sj := e.mustLogin("owner1")
	expect(t, e.patchSettings(sj, map[string]any{"business_name": "Zuscar Lodge"}), 403, "reauth_required")
	e.reauth(sj)

	// validation: unknown, read-only and malformed values are refused and nothing is written
	for name, changes := range map[string]map[string]any{
		"unknown key": {"colour": "red"}, "currency is read-only": {"currency": "USD"}, "verification record is read-only": {"tax_config_verified_by": "me"},
		"bad time": {"checkin_time": "25:00"}, "bad zone": {"timezone": "Mars/Base"}, "bad policy": {"negative_stock_policy": "bogus"},
		"empty name": {"business_name": "  "}, "bad ttl": {"idempotency_ttl_hours": 0}, "wrong type": {"tax_inclusive": "yes"}, "empty": {},
	} {
		if w := e.patchSettings(sj, changes); w.Code != 400 {
			t.Errorf("%s: status %d body %s", name, w.Code, w.Body.String())
		}
	}
	if e.setting(sj, "business_name") != `"Test"` {
		t.Fatal("a refused change wrote something")
	}

	// a valid multi-change is atomic, audited with before and after, and an identical repeat is silent
	changes := map[string]any{"business_name": "Zuscar Lodge", "checkin_time": "13:00", "receipt_footer": "Thank you", "idempotency_ttl_hours": 48}
	expect(t, e.patchSettings(sj, changes), 200, "")
	if e.setting(sj, "checkin_time") != `"13:00"` || e.setting(sj, "idempotency_ttl_hours") != `48` {
		t.Fatal("changes not stored")
	}
	var before, after string
	if err := e.pool.QueryRow(context.Background(), `SELECT before_json->>'business_name', after_json->>'business_name' FROM audit_logs WHERE action = 'settings.updated'`).Scan(&before, &after); err != nil || before != "Test" || after != "Zuscar Lodge" {
		t.Fatalf("audit = %q / %q / %v", before, after, err)
	}
	expect(t, e.patchSettings(sj, changes), 200, "")
	if e.auditN("settings.updated") != 1 {
		t.Fatal("an unchanged repeat must not write another audit row")
	}

	// verifying the tax configuration records who and when, and is audited under its own action
	if e.setting(sj, "tax_config_status") != `"provisional_unverified"` {
		t.Fatal("tax configuration should start provisional")
	}
	expect(t, e.patchSettings(sj, map[string]any{"tax_config_status": "verified"}), 200, "")
	if e.setting(sj, "tax_config_status") != `"verified"` || e.setting(sj, "tax_config_verified_by") != `"owner1"` || e.setting(sj, "tax_config_verified_at") == "null" {
		t.Fatal("verification record missing")
	}
	if e.auditN("settings.tax_config_verified") != 1 {
		t.Fatal("verification not audited")
	}
	// editing what the verification covered returns it to provisional (S-41)
	expect(t, e.patchSettings(sj, map[string]any{"tax_inclusive": false}), 200, "")
	if e.setting(sj, "tax_config_status") != `"provisional_unverified"` {
		t.Fatal("changing tax_inclusive must reset verification")
	}

	// tax categories
	expect(t, e.patchSettings(sj, map[string]any{"tax_config_status": "verified"}), 200, "")
	var standard taxRateItem
	for _, r := range e.taxRates(sj) {
		if r.Code == "standard" {
			standard = r
		}
	}
	id := fmt.Sprint(standard.ID)
	w := e.do(req{method: "PATCH", path: "/api/v1/tax-rates/" + id, jar: sj, body: map[string]any{"name": "Standard VAT"}})
	expect(t, w, 200, "")
	if e.setting(sj, "tax_config_status") != `"verified"` {
		t.Fatal("renaming must not reset verification")
	}
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/tax-rates/" + id, jar: sj, body: map[string]any{"rate_bp": 1800}}), 200, "")
	if e.setting(sj, "tax_config_status") != `"provisional_unverified"` {
		t.Fatal("changing a rate must reset verification")
	}
	var oldBP, newBP string
	if err := e.pool.QueryRow(context.Background(), `SELECT before_json->>'rate_bp', after_json->>'rate_bp' FROM audit_logs WHERE action = 'tax_rate.updated' AND after_json ? 'rate_bp' AND before_json->>'rate_bp' <> after_json->>'rate_bp'`).Scan(&oldBP, &newBP); err != nil || oldBP != "1600" || newBP != "1800" {
		t.Fatalf("audit rate change = %q -> %q (%v)", oldBP, newBP, err)
	}
	body := func(code, treatment string, bp any) map[string]any {
		return map[string]any{"code": code, "name": "Cat " + code, "rate_bp": bp, "treatment": treatment}
	}
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: sj, body: body("catering", "standard", 200)}), 201, "")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: sj, body: body("catering", "standard", 200)}), 409, "tax_code_taken")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: sj, body: body("zr_bad", "zero_rated", 100)}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: sj, body: body("over", "standard", 10001)}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: sj, body: body("odd", "reduced", 100)}), 400, "validation_failed")
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: sj, body: map[string]any{"code": "nobp", "name": "N", "treatment": "standard"}}), 400, "validation_failed")
	var exempt taxRateItem
	for _, r := range e.taxRates(sj) {
		if r.Code == "exempt" {
			exempt = r
		}
	}
	expect(t, e.do(req{method: "PATCH", path: fmt.Sprintf("/api/v1/tax-rates/%d", exempt.ID), jar: sj, body: map[string]any{"rate_bp": 5}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/tax-rates/" + id, jar: sj, body: map[string]any{}}), 400, "validation_failed")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/tax-rates/999999", jar: sj, body: map[string]any{"active": false}}), 404, "not_found")
	expect(t, e.do(req{method: "PATCH", path: "/api/v1/tax-rates/abc", jar: sj, body: map[string]any{"active": false}}), 404, "not_found")
	// after the reauth window, tax changes need the password again
	e.clock.Advance(6 * time.Minute)
	expect(t, e.do(req{method: "POST", path: "/api/v1/tax-rates", jar: sj, body: body("late", "exempt", 0)}), 403, "reauth_required")
}
