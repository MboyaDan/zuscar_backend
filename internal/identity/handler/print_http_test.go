package handler_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type jobResp struct {
	Job struct {
		ID        int64           `json:"id"`
		DocType   string          `json:"doc_type"`
		Status    string          `json:"status"`
		ReprintOf *int64          `json:"reprint_of"`
		CopyNo    int             `json:"copy_no"`
		IsReprint bool            `json:"is_reprint"`
		Document  json.RawMessage `json:"document"`
		Attempts  []struct {
			Outcome string `json:"outcome"`
			Note    string `json:"note"`
		} `json:"attempts"`
	} `json:"job"`
	Created bool `json:"created"`
}

// saleFor finalises a one-line order unpaid, as the cashier, and returns the sale id.
func (k *kitchenEnv) saleFor() int64 {
	k.t.Helper()
	o := k.open()
	k.add(o, k.tea, 1, "")
	w := k.e.do(req{method: "POST", path: "/api/v1/orders/" + itoa(o) + "/finalise", jar: k.cash, header: map[string]string{"Idempotency-Key": newKey()}, body: map[string]any{}})
	expect(k.t, w, 201, "")
	return decodeInto[saleResp](k.t, w).Sale.ID
}

func (k *kitchenEnv) job(j jar, method, path string, body any, want int, code string) jobResp {
	k.t.Helper()
	w := k.e.do(req{method: method, path: path, jar: j, body: body})
	expect(k.t, w, want, code)
	if want >= 400 {
		return jobResp{}
	}
	return decodeInto[jobResp](k.t, w)
}

func (k *kitchenEnv) newJob(j jar, doc string, ref int64, want int) jobResp {
	return k.job(j, "POST", "/api/v1/print/jobs", map[string]any{"doc_type": doc, "ref_id": ref}, want, "")
}

func (k *kitchenEnv) result(j jar, id int64, outcome string, want int) jobResp {
	return k.job(j, "POST", "/api/v1/print/jobs/"+itoa(id)+"/attempts", map[string]any{"outcome": outcome, "note": "test"}, want, "")
}

// AC-03: a print that fails leaves exactly the one sale and its receipt number; retrying the job makes no second record.
func TestAC03_PrintFailureKeepsOneSale(t *testing.T) {
	k := newKitchenEnv(t)
	sale := k.saleFor()
	sales, receipts := k.e.count(`SELECT count(*) FROM sales`), k.e.count(`SELECT next_value FROM document_counters WHERE doc_type = 'receipt' AND scope = ''`)
	var total int64
	if err := k.e.pool.QueryRow(t.Context(), `SELECT total_cents FROM sales WHERE id = $1`, sale).Scan(&total); err != nil {
		t.Fatal(err)
	}

	j := k.newJob(k.cash, "receipt", sale, 201)
	if !j.Created || j.Job.Status != "queued" || j.Job.IsReprint || j.Job.CopyNo != 0 {
		t.Fatalf("job = %+v", j)
	}
	failed := k.result(k.cash, j.Job.ID, "failed", 200)
	if failed.Job.Status != "failed" || len(failed.Job.Attempts) != 1 {
		t.Fatalf("after a failed print: %+v", failed.Job)
	}
	// Nothing about the sale moved because the printer failed.
	if k.e.count(`SELECT count(*) FROM sales`) != sales || k.e.count(`SELECT next_value FROM document_counters WHERE doc_type = 'receipt' AND scope = ''`) != receipts {
		t.Fatal("a failed print must not create or number anything")
	}
	var after int64
	_ = k.e.pool.QueryRow(t.Context(), `SELECT total_cents FROM sales WHERE id = $1`, sale).Scan(&after)
	if after != total {
		t.Fatal("the sale changed")
	}
	// A failed job takes no result until it is retried; the retry is the same job.
	k.result(k.cash, j.Job.ID, "printed", 409)
	k.job(k.cash, "POST", "/api/v1/print/jobs/"+itoa(j.Job.ID)+"/retry", nil, 200, "")
	k.job(k.cash, "POST", "/api/v1/print/jobs/"+itoa(j.Job.ID)+"/retry", nil, 409, "invalid_transition") // only a failed job retries
	done := k.result(k.cash, j.Job.ID, "printed", 200)
	if done.Job.ID != j.Job.ID || done.Job.Status != "printed" || len(done.Job.Attempts) != 2 {
		t.Fatalf("after the retry: %+v", done.Job)
	}
	if k.e.count(`SELECT count(*) FROM print_jobs WHERE sale_id = $1`, sale) != 1 || k.e.count(`SELECT count(*) FROM sales`) != sales {
		t.Fatal("one sale and one job expected")
	}
	k.result(k.cash, j.Job.ID, "printed", 409) // a printed job is finished
	for _, a := range []string{"print.job_created", "print.attempt", "print.retry"} {
		if k.e.count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND entity_id = $2`, a, itoa(j.Job.ID)) == 0 {
			t.Fatalf("no audit row for %s", a)
		}
	}
}

// AC-03: retrying or double-tapping print never makes a second kitchen ticket or a second first copy.
func TestAC03_RetryNeverDuplicatesTicket(t *testing.T) {
	k := newKitchenEnv(t)
	o := k.open()
	k.add(o, k.steak, 2, "no salt")
	tk := k.mustSend(o)
	tickets := k.e.count(`SELECT count(*) FROM kitchen_tickets`)

	first := k.newJob(k.cook, "kitchen_ticket", tk.ID, 201)
	again := k.newJob(k.cook, "kitchen_ticket", tk.ID, 200)
	if again.Created || again.Job.ID != first.Job.ID {
		t.Fatalf("asking again must return the first job: %+v vs %+v", again.Job, first.Job)
	}
	// Eight taps at once still make one original job.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k.e.do(req{method: "POST", path: "/api/v1/print/jobs", jar: k.cook, body: map[string]any{"doc_type": "kitchen_ticket", "ref_id": tk.ID}})
		}()
	}
	wg.Wait()
	if n := k.e.count(`SELECT count(*) FROM print_jobs WHERE ticket_id = $1 AND reprint_of IS NULL`, tk.ID); n != 1 {
		t.Fatalf("%d original jobs for one ticket", n)
	}
	k.result(k.cook, first.Job.ID, "failed", 200)
	k.job(k.cook, "POST", "/api/v1/print/jobs/"+itoa(first.Job.ID)+"/retry", nil, 200, "")
	k.result(k.cook, first.Job.ID, "printed", 200)
	if k.e.count(`SELECT count(*) FROM kitchen_tickets`) != tickets {
		t.Fatal("printing changed the number of tickets")
	}
	var no int64
	_ = k.e.pool.QueryRow(t.Context(), `SELECT ticket_no FROM kitchen_tickets WHERE id = $1`, tk.ID).Scan(&no)
	if no != tk.TicketNo {
		t.Fatalf("ticket number changed from %d to %d", tk.TicketNo, no)
	}
	// The ticket document has no prices and says what the kitchen needs.
	w := k.e.do(req{method: "GET", path: "/api/v1/print/documents/ticket/" + itoa(tk.ID), jar: k.cook})
	expect(t, w, 200, "")
	body := w.Body.String()
	if strings.Contains(body, "cents") || strings.Contains(body, "price") || !strings.Contains(body, "no salt") {
		t.Fatalf("ticket document: %s", body)
	}
}

// AC-03: a reprint is a new job, linked to the first, numbered, audited, and carries the same frozen document.
func TestAC03_ReprintLinkedAndMarked(t *testing.T) {
	k := newKitchenEnv(t)
	sale := k.saleFor()
	first := k.newJob(k.cash, "receipt", sale, 201)

	k.job(k.cash, "POST", "/api/v1/print/jobs/"+itoa(first.Job.ID)+"/reprint", nil, 409, "not_printed") // not printed yet: retry or print first
	k.result(k.cash, first.Job.ID, "printed", 200)

	c1 := k.job(k.cash, "POST", "/api/v1/print/jobs/"+itoa(first.Job.ID)+"/reprint", nil, 201, "")
	if !c1.Job.IsReprint || c1.Job.CopyNo != 1 || c1.Job.ReprintOf == nil || *c1.Job.ReprintOf != first.Job.ID || c1.Job.Status != "queued" {
		t.Fatalf("first copy = %+v", c1.Job)
	}
	var a, b map[string]any
	_ = json.Unmarshal(first.Job.Document, &a)
	_ = json.Unmarshal(c1.Job.Document, &b)
	if a["receipt_no"] == nil || a["receipt_no"] != b["receipt_no"] || a["total_cents"] != b["total_cents"] {
		t.Fatalf("a reprint must carry the original document: %v vs %v", a, b)
	}
	done := k.result(k.cash, c1.Job.ID, "printed", 200)
	if done.Job.Status != "reprinted" {
		t.Fatalf("a printed copy is %q", done.Job.Status)
	}
	// A reprint of a reprint points at the original and counts on.
	c2 := k.job(k.cash, "POST", "/api/v1/print/jobs/"+itoa(c1.Job.ID)+"/reprint", nil, 201, "")
	if c2.Job.CopyNo != 2 || c2.Job.ReprintOf == nil || *c2.Job.ReprintOf != first.Job.ID {
		t.Fatalf("second copy = %+v", c2.Job)
	}
	if k.e.count(`SELECT count(*) FROM audit_logs WHERE action = 'print.reprint'`) != 2 {
		t.Fatal("every reprint is audited")
	}
	if k.e.count(`SELECT count(*) FROM sales WHERE id = $1`, sale) != 1 {
		t.Fatal("a reprint must not touch the sale")
	}
	// Asking for a "first" print again does not make more paper: it returns the original.
	if again := k.newJob(k.cash, "receipt", sale, 200); again.Created || again.Job.ID != first.Job.ID {
		t.Fatalf("%+v", again.Job)
	}
	// Permission: the cook may reprint a kitchen ticket but not a receipt.
	r := k.newJob(k.cash, "receipt", sale, 200)
	k.job(k.cook, "POST", "/api/v1/print/jobs/"+itoa(r.Job.ID)+"/reprint", nil, 404, "not_found") // not their job
	k.job(k.mgr, "POST", "/api/v1/print/jobs/"+itoa(r.Job.ID)+"/reprint", nil, 201, "")           // a manager may
}

func TestPrintPermissionsAndVisibility(t *testing.T) {
	k := newKitchenEnv(t)
	sale := k.saleFor()
	o := k.open()
	k.add(o, k.steak, 1, "")
	tk := k.mustSend(o)

	// Each document type asks for its own permission.
	// The waiter holds sales.view for their own orders only (rbac note 1), and this sale is from their order.
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/documents/receipt/" + itoa(sale), jar: k.wait}), 200, "")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/documents/receipt/" + itoa(sale), jar: k.cook}), 403, "forbidden")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/documents/ticket/" + itoa(tk.ID), jar: k.cash}), 200, "")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/documents/bill/" + itoa(o), jar: k.cook}), 403, "forbidden")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/documents/bill/" + itoa(o), jar: k.cash}), 200, "")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/test-page", jar: k.wait}), 200, "")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/documents/receipt/999999", jar: k.cash}), 404, "not_found")
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/print/jobs", jar: k.cash, body: map[string]any{"doc_type": "poster", "ref_id": 1}}), 400, "validation_failed")
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/print/jobs", jar: k.cook, body: map[string]any{"doc_type": "receipt", "ref_id": sale}}), 403, "forbidden")

	// The receipt document comes from the stored sale.
	w := k.e.do(req{method: "GET", path: "/api/v1/print/documents/receipt/" + itoa(sale), jar: k.cash})
	expect(t, w, 200, "")
	var doc struct {
		Document struct {
			Type      string                  `json:"type"`
			ReceiptNo int64                   `json:"receipt_no"`
			Business  struct{ Name string }   `json:"business"`
			Lines     []struct{ Name string } `json:"lines"`
		} `json:"document"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc.Document.Type != "receipt" || doc.Document.ReceiptNo != 1 || doc.Document.Business.Name == "" || len(doc.Document.Lines) != 1 {
		t.Fatalf("receipt document: %s", w.Body.String())
	}

	// A job is visible to who made it and to managers, and hidden from everyone else.
	j := k.newJob(k.cook, "kitchen_ticket", tk.ID, 201)
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/jobs/" + itoa(j.Job.ID), jar: k.cook}), 200, "")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/jobs/" + itoa(j.Job.ID), jar: k.mgr}), 200, "")
	expect(t, k.e.do(req{method: "GET", path: "/api/v1/print/jobs/" + itoa(j.Job.ID), jar: k.cash}), 404, "not_found")
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/print/jobs/" + itoa(j.Job.ID) + "/attempts", jar: k.cash, body: map[string]any{"outcome": "printed"}}), 404, "not_found")
	var list struct {
		Jobs []struct{ ID int64 } `json:"jobs"`
	}
	lw := k.e.do(req{method: "GET", path: "/api/v1/print/jobs", jar: k.cash})
	expect(t, lw, 200, "")
	_ = json.Unmarshal(lw.Body.Bytes(), &list)
	if len(list.Jobs) != 0 {
		t.Fatalf("the cashier sees %d jobs that are not theirs", len(list.Jobs))
	}
	lw = k.e.do(req{method: "GET", path: "/api/v1/print/jobs?status=queued", jar: k.mgr})
	expect(t, lw, 200, "")
	_ = json.Unmarshal(lw.Body.Bytes(), &list)
	if len(list.Jobs) != 1 {
		t.Fatalf("the manager sees %d queued jobs", len(list.Jobs))
	}
	expect(t, k.e.do(req{method: "POST", path: "/api/v1/print/jobs/" + itoa(j.Job.ID) + "/attempts", jar: k.cook, body: map[string]any{"outcome": "maybe"}}), 400, "validation_failed")
}

// The database itself keeps a job's document and links, and the allowed moves of its status.
func TestPrintJobsAreProtectedInTheDatabase(t *testing.T) {
	k := newKitchenEnv(t)
	sale := k.saleFor()
	j := k.newJob(k.cash, "receipt", sale, 201)
	for _, q := range []string{
		`UPDATE print_jobs SET document = '{}'::jsonb WHERE id = $1`,
		`UPDATE print_jobs SET sale_id = NULL, doc_type = 'test_page' WHERE id = $1`,
		`UPDATE print_jobs SET status = 'reprinted' WHERE id = $1`, // an original is never "reprinted"
		`DELETE FROM print_jobs WHERE id = $1`,
	} {
		if _, err := k.e.pool.Exec(t.Context(), q, j.Job.ID); err == nil {
			t.Errorf("expected the database to refuse: %s", q)
		}
	}
	k.result(k.cash, j.Job.ID, "printed", 200)
	if _, err := k.e.pool.Exec(t.Context(), `UPDATE print_jobs SET status = 'queued' WHERE id = $1`, j.Job.ID); err == nil {
		t.Error("a printed job must not go back to queued")
	}
	if _, err := k.e.pool.Exec(t.Context(), `UPDATE print_attempts SET outcome = 'failed' WHERE job_id = $1`, j.Job.ID); err == nil {
		t.Error("attempts are never edited")
	}
}

// A bill or test page may be printed more than once, so a screen that opens twice must not queue two jobs: the optional
// Idempotency-Key returns the first answer. Without a key, each request is its own job.
func TestPrintCreateJobIdempotencyKey(t *testing.T) {
	k := newKitchenEnv(t)
	o := k.open()
	k.add(o, k.tea, 1, "")
	post := func(key string, body map[string]any) *httptest.ResponseRecorder {
		h := map[string]string{}
		if key != "" {
			h["Idempotency-Key"] = key
		}
		return k.e.do(req{method: "POST", path: "/api/v1/print/jobs", jar: k.cash, header: h, body: body})
	}
	bill := map[string]any{"doc_type": "bill", "ref_id": o}
	a, b := post("print-key-0001", bill), post("print-key-0001", bill)
	expect(t, a, 201, "")
	expect(t, b, 201, "")
	ja, jb := decodeInto[jobResp](t, a), decodeInto[jobResp](t, b)
	if ja.Job.ID == 0 || ja.Job.ID != jb.Job.ID || b.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("a retry with the same key must return the first job: %s vs %s", a.Body.String(), b.Body.String())
	}
	if n := k.e.count(`SELECT count(*) FROM print_jobs WHERE order_id = $1`, o); n != 1 {
		t.Fatalf("%d bill jobs after a retry with the same key", n)
	}
	expect(t, post("print-key-0001", map[string]any{"doc_type": "test_page"}), 422, "idempotency_key_reused")
	expect(t, post("short", bill), 400, "validation_failed")
	// No key: every request is its own job, which is how a bill is printed again after the order changes.
	expect(t, post("", bill), 201, "")
	expect(t, post("", bill), 201, "")
	if n := k.e.count(`SELECT count(*) FROM print_jobs WHERE order_id = $1`, o); n != 3 {
		t.Fatalf("%d bill jobs, want 3", n)
	}
}
