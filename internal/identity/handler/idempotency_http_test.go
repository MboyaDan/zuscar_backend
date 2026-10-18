package handler_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zuscar/backend/internal/platform/idempotency"
)

// Stage 10b-2: the Idempotency-Key header (ADR-08, S-06) on send-to-kitchen (required) and POST /orders (optional).

func (k *kitchenEnv) ticketOf(w *httptest.ResponseRecorder) kTicket {
	k.t.Helper()
	return decodeInto[struct {
		Ticket kTicket `json:"ticket"`
	}](k.t, w).Ticket
}

func TestIdempotency_SendToKitchen(t *testing.T) {
	k := newKitchenEnv(t)
	k.e.addUser("waiterb", false, "waiter")
	other := k.e.mustLogin("waiterb")
	order := k.open()
	k.add(order, k.tea, 1, "")
	path := "/api/v1/orders/" + itoa(order) + "/send-to-kitchen"

	// The header is required, and it must look like a key.
	expect(t, k.e.do(req{method: "POST", path: path, jar: k.wait}), 400, "validation_failed")
	for _, bad := range []string{"short", "has spaces in it", strings.Repeat("a", 129), "semi;colon;key"} {
		expect(t, k.e.do(req{method: "POST", path: path, jar: k.wait, header: map[string]string{"Idempotency-Key": bad}}), 400, "validation_failed")
	}
	if n := k.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1`, order); n != 0 {
		t.Fatalf("a rejected request raised %d tickets", n)
	}

	// First use does the work; the same key again returns the stored response and does nothing.
	key := newKey()
	first := k.sendWithKey(order, key)
	expect(t, first, 201, "")
	if first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("the first response must not be marked as a replay")
	}
	t1 := k.ticketOf(first)
	again := k.sendWithKey(order, key)
	expect(t, again, 201, "")
	t2 := k.ticketOf(again)
	if again.Header().Get("Idempotent-Replayed") != "true" || t2.ID != t1.ID || t2.TicketNo != t1.TicketNo || len(t2.Lines) != len(t1.Lines) {
		t.Fatalf("replay = %v %+v, first = %+v", again.Header(), t2, t1)
	}
	if first.Header().Get("Location") == "" || again.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("Location first %q, replay %q", first.Header().Get("Location"), again.Header().Get("Location"))
	}
	if n := k.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1`, order); n != 1 {
		t.Fatalf("tickets after a replay = %d", n)
	}
	if n := k.count(`SELECT count(*) FROM audit_logs WHERE action = 'order.sent_to_kitchen' AND entity_id = $1`, itoa(order)); n != 1 {
		t.Fatalf("send audit rows after a replay = %d", n)
	}

	// A replay still works after the order moved on, and the next new key gets the next number.
	k.add(order, k.tea, 2, "later")
	next := k.mustSend(order)
	if next.TicketNo != 2 {
		t.Fatalf("next ticket = %d, want 2 (the replay used no number)", next.TicketNo)
	}
	late := k.sendWithKey(order, key)
	expect(t, late, 201, "")
	if got := k.ticketOf(late); got.ID != t1.ID {
		t.Fatalf("replay after the order changed returned ticket %d, want %d", got.ID, t1.ID)
	}

	// The same key for a different request is refused: another order, another body, another user.
	o2 := k.open()
	k.add(o2, k.tea, 1, "")
	expect(t, k.sendWithKey(o2, key), 422, "idempotency_key_reused")
	expect(t, k.e.do(req{method: "POST", path: path, jar: k.wait, body: map[string]any{"expected_version": 99}, header: map[string]string{"Idempotency-Key": key}}), 422, "idempotency_key_reused")
	w := k.e.do(req{method: "POST", path: path, jar: other, header: map[string]string{"Idempotency-Key": key}})
	expect(t, w, 422, "idempotency_key_reused")
	if strings.Contains(w.Body.String(), "ticket_no") {
		t.Fatalf("a different user was shown the stored response: %s", w.Body.String())
	}
	if n := k.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1`, o2); n != 0 {
		t.Fatalf("a refused key reuse raised %d tickets", n)
	}

	// A request that fails does not use up its key: the corrected retry runs.
	empty := k.open()
	retry := newKey()
	expect(t, k.sendWithKey(empty, retry), 409, "nothing_to_send")
	if n := k.count(`SELECT count(*) FROM idempotency_records WHERE idem_key = $1`, retry); n != 0 {
		t.Fatalf("a failed request left %d idempotency records", n)
	}
	k.add(empty, k.tea, 1, "")
	expect(t, k.sendWithKey(empty, retry), 201, "")

	// Many identical requests at once: every caller gets the same ticket, one ticket exists, one number was used.
	conc := k.open()
	k.add(conc, k.tea, 1, "")
	ckey := newKey()
	var wg sync.WaitGroup
	got := make(chan kTicket, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := k.sendWithKey(conc, ckey)
			if w.Code != 201 {
				t.Errorf("concurrent send = %d %s", w.Code, w.Body.String())
				return
			}
			got <- k.ticketOf(w)
		}()
	}
	wg.Wait()
	close(got)
	ids := map[int64]bool{}
	for tk := range got {
		ids[tk.ID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("simultaneous identical sends returned tickets %v, want exactly one", ids)
	}
	if n := k.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1`, conc); n != 1 {
		t.Fatalf("tickets after simultaneous identical sends = %d", n)
	}
}

func TestIdempotency_CreateOrder(t *testing.T) {
	k := newKitchenEnv(t)
	post := func(key string, body map[string]any) *httptest.ResponseRecorder {
		h := map[string]string{}
		if key != "" {
			h["Idempotency-Key"] = key
		}
		return k.e.do(req{method: "POST", path: "/api/v1/orders", jar: k.wait, body: body, header: h})
	}
	orderOf := func(w *httptest.ResponseRecorder) (int64, int64) {
		o := decodeInto[struct {
			Order struct {
				ID      int64 `json:"id"`
				OrderNo int64 `json:"order_no"`
			} `json:"order"`
		}](t, w).Order
		return o.ID, o.OrderNo
	}

	// Optional: without a key every request opens an order.
	a, b := post("", map[string]any{"order_type": "takeaway"}), post("", map[string]any{"order_type": "takeaway"})
	expect(t, a, 201, "")
	expect(t, b, 201, "")
	_, n1 := orderOf(a)
	_, n2 := orderOf(b)
	if n2 != n1+1 {
		t.Fatalf("order numbers without a key = %d, %d", n1, n2)
	}

	// With a key a retry returns the same order and uses no further number.
	key := newKey()
	first := post(key, map[string]any{"order_type": "takeaway", "note": "table by the window"})
	expect(t, first, 201, "")
	id1, no1 := orderOf(first)
	again := post(key, map[string]any{"order_type": "takeaway", "note": "table by the window"})
	expect(t, again, 201, "")
	id2, no2 := orderOf(again)
	if id1 != id2 || no1 != no2 || again.Header().Get("Idempotent-Replayed") != "true" || again.Header().Get("Location") != "/api/v1/orders/"+itoa(id1) {
		t.Fatalf("replay = %d/%d %v, first = %d/%d", id2, no2, again.Header(), id1, no1)
	}
	third := post(newKey(), map[string]any{"order_type": "takeaway"})
	expect(t, third, 201, "")
	if _, no3 := orderOf(third); no3 != no1+1 {
		t.Fatalf("the order after a replay is number %d, want %d", no3, no1+1)
	}
	expect(t, post(key, map[string]any{"order_type": "takeaway", "note": "something else"}), 422, "idempotency_key_reused")

	// A request refused for its content does not use up its key, whether it fails before or inside the transaction.
	retry := newKey()
	expect(t, post(retry, map[string]any{"order_type": "eat_in"}), 400, "validation_failed")
	expect(t, post(retry, map[string]any{"order_type": "dine_in", "table_id": 99999}), 400, "validation_failed")
	expect(t, post(retry, map[string]any{"order_type": "takeaway"}), 201, "")
	if n := k.count(`SELECT count(*) FROM idempotency_records WHERE idem_key = $1 AND status = 'completed'`, retry); n != 1 {
		t.Fatalf("completed records for the retried key = %d", n)
	}
}

// How long a key lives, what the database refuses, and the cleanup of expired keys (S-06).
func TestIdempotency_RetentionAndPurge(t *testing.T) {
	k := newKitchenEnv(t)
	ctx := context.Background()
	open := func() (int64, string) {
		o := k.open()
		k.add(o, k.tea, 1, "")
		key := newKey()
		expect(t, k.sendWithKey(o, key), 201, "")
		return o, key
	}
	span := func(key string) time.Duration {
		var secs float64
		if err := k.e.pool.QueryRow(ctx, `SELECT extract(epoch FROM expires_at - created_at) FROM idempotency_records WHERE idem_key = $1`, key).Scan(&secs); err != nil {
			t.Fatal(err)
		}
		return time.Duration(secs * float64(time.Second))
	}

	// Default 24 hours; the setting can lengthen it; nothing, not even a tiny setting, can shorten it below 24 hours.
	_, k24 := open()
	if d := span(k24); d < 24*time.Hour || d > 25*time.Hour {
		t.Fatalf("default retention = %s", d)
	}
	k.e.exec(`INSERT INTO system_settings (key, value) VALUES ('idempotency_ttl_hours', '72'::jsonb) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	_, k72 := open()
	if d := span(k72); d < 72*time.Hour || d > 73*time.Hour {
		t.Fatalf("retention with the setting at 72 = %s", d)
	}
	k.e.exec(`INSERT INTO system_settings (key, value) VALUES ('idempotency_ttl_hours', '1'::jsonb) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	_, k1 := open()
	if d := span(k1); d < 24*time.Hour {
		t.Fatalf("a setting of 1 hour gave retention %s, below the 24 hour floor", d)
	}

	// The table itself refuses a shorter life, edits to a completed record, and truncation.
	for _, sql := range []string{
		`INSERT INTO idempotency_records (idem_key, endpoint, user_id, request_hash, expires_at) SELECT 'short-life-key', 'POST /x', id, decode(repeat('00', 32), 'hex'), now() + interval '1 hour' FROM users LIMIT 1`,
		`UPDATE idempotency_records SET response_status = 200 WHERE idem_key = '` + k24 + `'`,
		`UPDATE idempotency_records SET expires_at = now() WHERE idem_key = '` + k24 + `'`,
		`TRUNCATE idempotency_records`,
	} {
		if _, err := k.e.pool.Exec(ctx, sql); err == nil {
			t.Errorf("the database accepted: %s", sql)
		}
	}

	// A key past its life is forgotten: the same key runs again as new work. Only expired records are purged.
	o, old := open()
	if _, err := k.e.pool.Exec(ctx, `ALTER TABLE idempotency_records DISABLE TRIGGER idempotency_records_guard`); err != nil {
		t.Fatal(err)
	}
	k.e.exec(`UPDATE idempotency_records SET created_at = now() - interval '4 days', expires_at = now() - interval '2 days' WHERE idem_key = $1`, old)
	if _, err := k.e.pool.Exec(ctx, `ALTER TABLE idempotency_records ENABLE TRIGGER idempotency_records_guard`); err != nil {
		t.Fatal(err)
	}
	k.add(o, k.tea, 3, "after expiry")
	renewed := k.sendWithKey(o, old)
	expect(t, renewed, 201, "")
	if got := k.ticketOf(renewed); got.Kind != "update" || len(got.Lines) != 1 || got.Lines[0].Qty != 3 {
		t.Fatalf("an expired key should run again as new work: %+v", got)
	}

	k.e.exec(`ALTER TABLE idempotency_records DISABLE TRIGGER idempotency_records_guard`)
	k.e.exec(`UPDATE idempotency_records SET created_at = now() - interval '4 days', expires_at = now() - interval '2 days' WHERE idem_key = $1`, k24)
	k.e.exec(`ALTER TABLE idempotency_records ENABLE TRIGGER idempotency_records_guard`)
	before := k.count(`SELECT count(*) FROM idempotency_records`)
	removed, err := idempotency.Purge(ctx, k.e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || k.count(`SELECT count(*) FROM idempotency_records`) != before-1 || k.count(`SELECT count(*) FROM idempotency_records WHERE idem_key = $1`, k24) != 0 {
		t.Fatalf("purge removed %d of %d records", removed, before)
	}
}

// A request that cannot get the key because another is still using it waits, then answers 409 idempotency_conflict,
// and the key is usable once the other request is gone.
func TestIdempotency_InFlightAnswers409(t *testing.T) {
	k := newKitchenEnv(t)
	ctx := context.Background()
	order := k.open()
	k.add(order, k.tea, 1, "")
	key := newKey()

	old := idempotency.LockTimeout
	idempotency.LockTimeout = "300ms"
	defer func() { idempotency.LockTimeout = old }()

	// Hold the key in an open transaction, as a slow first request would.
	holder, err := k.e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `INSERT INTO idempotency_records (idem_key, endpoint, user_id, request_hash, expires_at)
		SELECT $1, 'POST /api/v1/orders/:id/send-to-kitchen', id, decode(repeat('00', 32), 'hex'), now() + interval '24 hours' FROM users LIMIT 1`, key); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	w := k.sendWithKey(order, key)
	expect(t, w, 409, "idempotency_conflict")
	if w.Header().Get("Retry-After") == "" || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("expected a wait and a Retry-After header, got %v after %s", w.Header(), time.Since(start))
	}
	if n := k.count(`SELECT count(*) FROM kitchen_tickets WHERE order_id = $1`, order); n != 0 {
		t.Fatalf("a request that gave up raised %d tickets", n)
	}

	// The other request fails and rolls back: the key is free again.
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	expect(t, k.sendWithKey(order, key), 201, "")
}
