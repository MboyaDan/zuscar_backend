// Package service is the printing module's rules (Stage 14): print jobs and their attempts for receipts, kitchen
// tickets, unpaid bills and the printer test page. Printing is separate from the record it prints (AC-03, NN-05): a
// job is created after the sale, ticket or bill exists, holds a frozen copy of the document, and only its own status
// ever moves. A failed print never touches a sale or a ticket and a retry never makes a second one.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	orderservice "zuscar/backend/internal/orders/service"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/printing/repository"
	salesservice "zuscar/backend/internal/sales/service"
)

var ErrForbidden = errors.New("you do not have permission to do this")

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

type ConflictError struct{ Code, Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

// What printing needs from the other modules (docs/architecture.md section 3, rule 4).
type Sales interface {
	GetSale(ctx context.Context, actor *auth.Principal, id int64) (salesservice.SaleView, error)
	Bill(ctx context.Context, actor *auth.Principal, orderID int64) (salesservice.BillView, error)
}
type Orders interface {
	GetTicket(ctx context.Context, actor *auth.Principal, id int64) (orderservice.TicketView, error)
}
type Settings interface {
	GetSettings(ctx context.Context, actor *auth.Principal) (map[string]json.RawMessage, error)
}

type Service struct {
	pool     *pgxpool.Pool
	rec      audit.Recorder
	sales    Sales
	orders   Orders
	settings Settings
	now      func() time.Time
}

func New(pool *pgxpool.Pool, rec audit.Recorder, sales Sales, orders Orders, settings Settings) *Service {
	return &Service{pool: pool, rec: rec, sales: sales, orders: orders, settings: settings, now: time.Now}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action string, id int64, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: "print_job",
		EntityID: itoa(id), Before: before, After: after, RequestID: m.RequestID, IP: m.IP})
}

func hasRole(actor *auth.Principal, codes ...string) bool {
	for _, r := range actor.Roles {
		for _, c := range codes {
			if r == c {
				return true
			}
		}
	}
	return false
}

// seesAllJobs: managers see every job; everyone else sees the jobs they created.
func seesAllJobs(actor *auth.Principal) bool {
	return hasRole(actor, "super_admin", "business_manager")
}

// translate turns another module's errors into this module's, so the handler maps one set.
func translate(err error) error {
	var snf *salesservice.NotFoundError
	var onf *orderservice.NotFoundError
	var sv *salesservice.ValidationError
	var ov *orderservice.ValidationError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, salesservice.ErrForbidden), errors.Is(err, orderservice.ErrForbidden):
		return ErrForbidden
	case errors.As(err, &snf):
		return &NotFoundError{What: snf.What}
	case errors.As(err, &onf):
		return &NotFoundError{What: onf.What}
	case errors.As(err, &sv):
		return &ValidationError{Field: sv.Field, Msg: sv.Msg}
	case errors.As(err, &ov):
		return &ValidationError{Field: ov.Field, Msg: ov.Msg}
	}
	return err
}

// ---- documents ----

const (
	DocReceipt = "receipt"
	DocTicket  = "kitchen_ticket"
	DocBill    = "bill"
	DocTest    = "test_page"
)

type Business struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Phone    string `json:"phone"`
	Footer   string `json:"footer"`
	Timezone string `json:"timezone"`
}

func (s *Service) business(ctx context.Context, actor *auth.Principal) (Business, bool, error) {
	all, err := s.settings.GetSettings(ctx, actor)
	if err != nil {
		return Business{}, false, translate(err)
	}
	str := func(k string) string {
		var v string
		_ = json.Unmarshal(all[k], &v)
		return v
	}
	inclusive := true
	_ = json.Unmarshal(all["tax_inclusive"], &inclusive)
	b := Business{Name: str("business_name"), Address: str("address"), Phone: str("phone"), Footer: str("receipt_footer"), Timezone: str("timezone")}
	if b.Name == "" {
		b.Name = "Zuscar Venue"
	}
	return b, inclusive, nil
}

type ReceiptLine struct {
	Qty            int32  `json:"qty"`
	Name           string `json:"name"`
	Modifiers      string `json:"modifiers,omitempty"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	ModifiersCents int64  `json:"modifiers_cents"`
	DiscountCents  int64  `json:"discount_cents"`
	DiscountReason string `json:"discount_reason,omitempty"`
	LineTotalCents int64  `json:"line_total_cents"`
}

type ReceiptPayment struct {
	Method      string `json:"method"`
	Status      string `json:"status"`
	AmountCents int64  `json:"amount_cents"`
	Reference   string `json:"reference,omitempty"`
}

type ReceiptDoc struct {
	Type          string           `json:"type"`
	Business      Business         `json:"business"`
	SaleID        int64            `json:"sale_id"`
	ReceiptNo     int64            `json:"receipt_no"`
	OrderNo       int64            `json:"order_no,omitempty"`
	BusinessDate  string           `json:"business_date"`
	CreatedAt     time.Time        `json:"created_at"`
	CashierName   string           `json:"cashier_name"`
	Lines         []ReceiptLine    `json:"lines"`
	SubtotalCents int64            `json:"subtotal_cents"`
	DiscountCents int64            `json:"discount_cents"`
	TaxCents      int64            `json:"tax_cents"`
	TotalCents    int64            `json:"total_cents"`
	TaxInclusive  bool             `json:"tax_inclusive"`
	TaxStatus     string           `json:"tax_status"`
	Payments      []ReceiptPayment `json:"payments"`
	PaidCents     int64            `json:"paid_cents"`
	PendingCents  int64            `json:"pending_cents"`
	BalanceCents  int64            `json:"balance_cents"`
	PaymentStatus string           `json:"payment_status"`
	SaleStatus    string           `json:"sale_status"`
}

type TicketLine struct {
	Qty        int32  `json:"qty"`
	Name       string `json:"name"`
	ChangeType string `json:"change_type"`
	Modifiers  string `json:"modifiers,omitempty"`
	Note       string `json:"note,omitempty"`
}

// TicketDoc has no prices on purpose: a kitchen ticket must be visibly different from a receipt.
type TicketDoc struct {
	Type         string       `json:"type"`
	BusinessName string       `json:"business_name"`
	TicketID     int64        `json:"ticket_id"`
	TicketNo     int64        `json:"ticket_no"`
	Kind         string       `json:"kind"`
	OrderNo      int64        `json:"order_no"`
	OrderType    string       `json:"order_type"`
	TableLabel   string       `json:"table_label"`
	BusinessDate string       `json:"business_date"`
	CreatedAt    time.Time    `json:"created_at"`
	CreatedBy    string       `json:"created_by"`
	Timezone     string       `json:"timezone"`
	Lines        []TicketLine `json:"lines"`
}

type BillDoc struct {
	Type         string        `json:"type"`
	Business     Business      `json:"business"`
	OrderID      int64         `json:"order_id"`
	OrderNo      int64         `json:"order_no"`
	GeneratedAt  time.Time     `json:"generated_at"`
	Lines        []ReceiptLine `json:"lines"`
	TotalCents   int64         `json:"total_cents"`
	TaxCents     int64         `json:"tax_cents"`
	TaxInclusive bool          `json:"tax_inclusive"`
	TaxStatus    string        `json:"tax_status"`
}

type TestDoc struct {
	Type        string    `json:"type"`
	Business    Business  `json:"business"`
	GeneratedAt time.Time `json:"generated_at"`
	PaperMM     int       `json:"paper_mm"`
	PrintMM     int       `json:"print_mm"`
}

// target says what a job or document is about; exactly the field its type uses is set.
type target struct {
	docType string
	sale    int64
	ticket  int64
	order   int64
}

func (s *Service) need(actor *auth.Principal, perm string) error {
	if actor == nil || (perm != "" && !actor.Has(perm)) {
		return ErrForbidden
	}
	return nil
}

// render builds the document for a target, applying that type's permission and the other module's own scope rules.
func (s *Service) render(ctx context.Context, actor *auth.Principal, t target) (any, error) {
	if actor == nil {
		return nil, ErrForbidden
	}
	switch t.docType {
	case DocReceipt:
		if err := s.need(actor, "sales.view"); err != nil {
			return nil, err
		}
		sale, err := s.sales.GetSale(ctx, actor, t.sale)
		if err != nil {
			return nil, translate(err)
		}
		if sale.Status == "voided" {
			return nil, &ConflictError{Code: "sale_voided", Msg: "This sale was voided, so its receipt cannot be printed."}
		}
		biz, inclusive, err := s.business(ctx, actor)
		if err != nil {
			return nil, err
		}
		d := ReceiptDoc{Type: DocReceipt, Business: biz, SaleID: sale.ID, ReceiptNo: sale.ReceiptNo, OrderNo: sale.OrderNo, BusinessDate: sale.BusinessDate,
			CreatedAt: sale.CreatedAt, CashierName: sale.CashierName, Lines: []ReceiptLine{}, SubtotalCents: sale.SubtotalCents, DiscountCents: sale.DiscountCents,
			TaxCents: sale.TaxCents, TotalCents: sale.TotalCents, TaxInclusive: inclusive, TaxStatus: sale.TaxStatus, Payments: []ReceiptPayment{},
			PaidCents: sale.PaidCents, PendingCents: sale.PendingCents, BalanceCents: sale.BalanceCents, PaymentStatus: sale.PaymentStatus, SaleStatus: sale.Status}
		for _, i := range sale.Items {
			d.Lines = append(d.Lines, ReceiptLine{Qty: i.Qty, Name: i.Name, Modifiers: i.ModifiersText, UnitPriceCents: i.UnitPriceCents, ModifiersCents: i.ModifiersCents,
				DiscountCents: i.DiscountCents, DiscountReason: i.DiscountReason, LineTotalCents: i.LineTotalCents})
		}
		for _, p := range sale.Payments {
			if p.Status == "failed" {
				continue // a payment that did not arrive is not part of what the customer paid
			}
			d.Payments = append(d.Payments, ReceiptPayment{Method: p.Method, Status: p.Status, AmountCents: p.AmountCents, Reference: p.Reference})
		}
		return d, nil
	case DocTicket:
		if err := s.need(actor, "kitchen.view"); err != nil {
			return nil, err
		}
		tk, err := s.orders.GetTicket(ctx, actor, t.ticket)
		if err != nil {
			return nil, translate(err)
		}
		biz, _, err := s.business(ctx, actor)
		if err != nil {
			return nil, err
		}
		d := TicketDoc{Type: DocTicket, BusinessName: biz.Name, TicketID: tk.ID, TicketNo: tk.TicketNo, Kind: tk.Kind, OrderNo: tk.OrderNo, OrderType: tk.OrderType,
			TableLabel: tk.TableLabel, BusinessDate: tk.BusinessDate, CreatedAt: tk.CreatedAt, CreatedBy: tk.CreatedByName, Timezone: biz.Timezone, Lines: []TicketLine{}}
		for _, l := range tk.Lines {
			d.Lines = append(d.Lines, TicketLine{Qty: l.Qty, Name: l.Name, ChangeType: l.ChangeType, Modifiers: l.Modifiers, Note: l.Note})
		}
		return d, nil
	case DocBill:
		if err := s.need(actor, "sales.create"); err != nil {
			return nil, err
		}
		b, err := s.sales.Bill(ctx, actor, t.order)
		if err != nil {
			return nil, translate(err)
		}
		biz, _, err := s.business(ctx, actor)
		if err != nil {
			return nil, err
		}
		d := BillDoc{Type: DocBill, Business: biz, OrderID: b.OrderID, OrderNo: b.OrderNo, GeneratedAt: s.now().UTC(), Lines: []ReceiptLine{}, TotalCents: b.TotalCents,
			TaxCents: b.TaxCents, TaxInclusive: b.TaxInclusive, TaxStatus: b.TaxStatus}
		for _, l := range b.Lines {
			d.Lines = append(d.Lines, ReceiptLine{Qty: l.Qty, Name: l.Name, Modifiers: l.ModifiersText, UnitPriceCents: l.UnitPriceCents, ModifiersCents: l.ModifiersCents, LineTotalCents: l.LineTotalCents})
		}
		return d, nil
	case DocTest:
		biz, _, err := s.business(ctx, actor)
		if err != nil {
			return nil, err
		}
		return TestDoc{Type: DocTest, Business: biz, GeneratedAt: s.now().UTC(), PaperMM: 80, PrintMM: 72}, nil
	}
	return nil, &ValidationError{Field: "doc_type", Msg: "must be receipt, kitchen_ticket, bill or test_page"}
}

// Document returns what would be printed, without creating a job.
func (s *Service) Document(ctx context.Context, actor *auth.Principal, docType string, id int64) (any, error) {
	return s.render(ctx, actor, targetFor(docType, id))
}

func targetFor(docType string, id int64) target {
	t := target{docType: docType}
	switch docType {
	case DocReceipt:
		t.sale = id
	case DocTicket:
		t.ticket = id
	case DocBill:
		t.order = id
	}
	return t
}

// ---- jobs ----

type AttemptView struct {
	ID            int64     `json:"id"`
	Outcome       string    `json:"outcome"`
	Note          string    `json:"note,omitempty"`
	CreatedBy     int64     `json:"created_by"`
	CreatedByName string    `json:"created_by_name"`
	CreatedAt     time.Time `json:"created_at"`
}

type JobView struct {
	ID            int64           `json:"id"`
	DocType       string          `json:"doc_type"`
	SaleID        *int64          `json:"sale_id"`
	TicketID      *int64          `json:"ticket_id"`
	OrderID       *int64          `json:"order_id"`
	Status        string          `json:"status"`
	ReprintOf     *int64          `json:"reprint_of"`
	CopyNo        int32           `json:"copy_no"`
	IsReprint     bool            `json:"is_reprint"`
	CreatedBy     int64           `json:"created_by"`
	CreatedByName string          `json:"created_by_name"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Document      json.RawMessage `json:"document,omitempty"`
	Attempts      []AttemptView   `json:"attempts,omitempty"`
}

func optID(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func (s *Service) loadJob(ctx context.Context, q *repository.Queries, id int64) (JobView, error) {
	r, err := q.GetPrintJob(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobView{}, &NotFoundError{What: "print job"}
	}
	if err != nil {
		return JobView{}, err
	}
	v := JobView{ID: r.ID, DocType: r.DocType, SaleID: optID(r.SaleID), TicketID: optID(r.TicketID), OrderID: optID(r.OrderID), Status: r.Status, ReprintOf: optID(r.ReprintOf),
		CopyNo: r.CopyNo, IsReprint: r.ReprintOf.Valid, CreatedBy: r.CreatedBy, CreatedByName: r.CreatedByName, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time, Document: r.Document}
	at, err := q.ListPrintAttempts(ctx, id)
	if err != nil {
		return JobView{}, err
	}
	for _, a := range at {
		v.Attempts = append(v.Attempts, AttemptView{ID: a.ID, Outcome: a.Outcome, Note: a.Note, CreatedBy: a.CreatedBy, CreatedByName: a.CreatedByName, CreatedAt: a.CreatedAt.Time})
	}
	return v, nil
}

// visible hides another person's job from anyone who is not a manager: it answers "not found", not "forbidden".
func visible(actor *auth.Principal, createdBy int64) error {
	if !seesAllJobs(actor) && createdBy != actor.UserID {
		return &NotFoundError{What: "print job"}
	}
	return nil
}

func i8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: v != 0} }

func (s *Service) existingOriginal(ctx context.Context, q *repository.Queries, t target) (int64, bool, error) {
	var id int64
	var err error
	switch t.docType {
	case DocReceipt:
		id, err = q.FindOriginalReceiptJob(ctx, i8(t.sale))
	case DocTicket:
		id, err = q.FindOriginalTicketJob(ctx, i8(t.ticket))
	default:
		return 0, false, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

type CreateInput struct {
	DocType string
	RefID   int64
}

// CreateJob queues the first print of a document. The sale, ticket or bill already exists; this never changes it.
// A receipt or ticket has exactly one original job: asking again returns that job (created = false), so a double
// tap or a retry cannot queue a second first copy. A bill or test page may legitimately be printed more than once,
// so a caller that retries (a screen that opens twice, a dropped connection) sends an Idempotency-Key and gets the
// first answer back; the key is optional because nothing money-related is created here.
func (s *Service) CreateJob(ctx context.Context, actor *auth.Principal, in CreateInput, idem *idempotency.Key, m Meta) (*idempotency.Stored, JobView, bool, error) {
	if actor == nil {
		return nil, JobView{}, false, ErrForbidden
	}
	t := targetFor(in.DocType, in.RefID)
	if in.DocType != DocTest && in.RefID <= 0 {
		return nil, JobView{}, false, &ValidationError{Field: "ref_id", Msg: "is required"}
	}
	doc, err := s.render(ctx, actor, t)
	if err != nil {
		return nil, JobView{}, false, err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, JobView{}, false, err
	}
	q := repository.New(s.pool)
	if id, found, err := s.existingOriginal(ctx, q, t); err != nil {
		return nil, JobView{}, false, err
	} else if found {
		v, err := s.loadJob(ctx, q, id)
		return nil, v, false, err
	}
	var replay *idempotency.Stored
	var view JobView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if idem != nil {
			st, err := idempotency.Begin(ctx, tx, idem)
			if err != nil {
				return err
			}
			if st != nil {
				replay = st
				return nil
			}
		}
		tq := repository.New(tx)
		id, err := tq.InsertPrintJob(ctx, repository.InsertPrintJobParams{DocType: in.DocType, SaleID: i8(t.sale), TicketID: i8(t.ticket), OrderID: i8(t.order), Document: raw, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "print.job_created", id, nil, map[string]any{"doc_type": in.DocType, "ref_id": in.RefID}); err != nil {
			return err
		}
		view, err = s.loadJob(ctx, tq, id)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"job": view, "created": true})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" { // another tap won the race: return its job
		if id, found, e2 := s.existingOriginal(ctx, q, t); e2 == nil && found {
			v, e3 := s.loadJob(ctx, q, id)
			return nil, v, false, e3
		}
	}
	if err != nil {
		return nil, JobView{}, false, err
	}
	if replay != nil {
		return replay, JobView{}, false, nil
	}
	return nil, view, true, nil
}

func (s *Service) GetJob(ctx context.Context, actor *auth.Principal, id int64) (JobView, error) {
	if actor == nil {
		return JobView{}, ErrForbidden
	}
	v, err := s.loadJob(ctx, repository.New(s.pool), id)
	if err != nil {
		return JobView{}, err
	}
	return v, visible(actor, v.CreatedBy)
}

type JobFilter struct {
	Status   string
	DocType  string
	BeforeID int64
	Limit    int
}

func (s *Service) ListJobs(ctx context.Context, actor *auth.Principal, f JobFilter) ([]JobView, bool, error) {
	if actor == nil {
		return nil, false, ErrForbidden
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	p := repository.ListPrintJobsParams{Status: f.Status, DocType: f.DocType, BeforeID: f.BeforeID, RowLimit: int32(limit + 1)}
	if !seesAllJobs(actor) {
		p.CreatedBy = actor.UserID
	}
	rows, err := repository.New(s.pool).ListPrintJobs(ctx, p)
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]JobView, 0, len(rows))
	for _, r := range rows {
		out = append(out, JobView{ID: r.ID, DocType: r.DocType, SaleID: optID(r.SaleID), TicketID: optID(r.TicketID), OrderID: optID(r.OrderID), Status: r.Status,
			ReprintOf: optID(r.ReprintOf), CopyNo: r.CopyNo, IsReprint: r.ReprintOf.Valid, CreatedBy: r.CreatedBy, CreatedByName: r.CreatedByName,
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, more, nil
}

// RecordAttempt stores what the person saw at the printer. "printed" is their confirmation (DG-03); "failed" keeps
// the job so it can be retried. Only a queued job takes an attempt.
func (s *Service) RecordAttempt(ctx context.Context, actor *auth.Principal, id int64, outcome, note string, m Meta) (JobView, error) {
	if actor == nil {
		return JobView{}, ErrForbidden
	}
	note = strings.TrimSpace(note)
	if outcome != "printed" && outcome != "failed" {
		return JobView{}, &ValidationError{Field: "outcome", Msg: "must be printed or failed"}
	}
	if len([]rune(note)) > 200 {
		return JobView{}, &ValidationError{Field: "note", Msg: "must be 200 characters or fewer"}
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		j, err := q.LockPrintJob(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "print job"}
		}
		if err != nil {
			return err
		}
		if err := visible(actor, j.CreatedBy); err != nil {
			return err
		}
		if j.Status != "queued" {
			return &ConflictError{Code: "invalid_transition", Msg: "That print job is " + j.Status + "; only a queued job takes a result. Retry a failed job first."}
		}
		to := "failed"
		if outcome == "printed" {
			to = "printed"
			if j.ReprintOf.Valid {
				to = "reprinted"
			}
		}
		if _, err := q.InsertPrintAttempt(ctx, repository.InsertPrintAttemptParams{JobID: id, Outcome: outcome, Note: note, CreatedBy: actor.UserID}); err != nil {
			return err
		}
		if err := q.SetPrintJobStatus(ctx, repository.SetPrintJobStatusParams{ID: id, Status: to}); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "print.attempt", id, map[string]any{"status": j.Status}, map[string]any{"status": to, "outcome": outcome, "note": note})
	})
	if err != nil {
		return JobView{}, err
	}
	return s.loadJob(ctx, repository.New(s.pool), id)
}

// Retry puts a failed job back in the queue. It is the same job: no new record, no new number.
func (s *Service) Retry(ctx context.Context, actor *auth.Principal, id int64, m Meta) (JobView, error) {
	if actor == nil {
		return JobView{}, ErrForbidden
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		j, err := q.LockPrintJob(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "print job"}
		}
		if err != nil {
			return err
		}
		if err := visible(actor, j.CreatedBy); err != nil {
			return err
		}
		if j.Status != "failed" {
			return &ConflictError{Code: "invalid_transition", Msg: "Only a failed print job can be retried (this one is " + j.Status + ")."}
		}
		if err := q.SetPrintJobStatus(ctx, repository.SetPrintJobStatusParams{ID: id, Status: "queued"}); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "print.retry", id, map[string]any{"status": "failed"}, map[string]any{"status": "queued"})
	})
	if err != nil {
		return JobView{}, err
	}
	return s.loadJob(ctx, repository.New(s.pool), id)
}

// reprintPermission is what a reprint of each document type needs. Receipts are the ones that can be abused, so they
// need their own permission; a kitchen ticket needs the kitchen view and a bill the right to sell.
func reprintPermission(docType string) (string, bool) {
	switch docType {
	case DocReceipt:
		return "sales.reprint", true
	case DocTicket:
		return "kitchen.view", true
	case DocBill:
		return "sales.create", true
	}
	return "", false
}

// Reprint makes a new job from a job that already printed. The copy is rendered from the frozen document, carries a
// copy number and a link to the original, and is audited with who asked (S-19). The sale and ticket are untouched.
func (s *Service) Reprint(ctx context.Context, actor *auth.Principal, id int64, m Meta) (JobView, error) {
	if actor == nil {
		return JobView{}, ErrForbidden
	}
	var newID int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		src, err := q.LockPrintJob(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "print job"}
		}
		if err != nil {
			return err
		}
		if err := visible(actor, src.CreatedBy); err != nil {
			return err
		}
		perm, ok := reprintPermission(src.DocType)
		if !ok {
			return &ValidationError{Field: "doc_type", Msg: "a test page is printed again with a new test, not a reprint"}
		}
		if !actor.Has(perm) {
			return ErrForbidden
		}
		if src.DocType == DocReceipt && src.SaleID.Valid {
			if sale, err := s.sales.GetSale(ctx, actor, src.SaleID.Int64); err == nil && sale.Status == "voided" {
				return &ConflictError{Code: "sale_voided", Msg: "This sale was voided, so its receipt cannot be printed again."}
			}
		}
		if src.Status != "printed" && src.Status != "reprinted" {
			return &ConflictError{Code: "not_printed", Msg: "Only a job that has printed can be reprinted (this one is " + src.Status + "). Retry a failed job instead."}
		}
		root := src.ID
		if src.ReprintOf.Valid {
			root = src.ReprintOf.Int64
		}
		n, err := q.CountReprints(ctx, i8(root))
		if err != nil {
			return err
		}
		full, err := q.GetPrintJob(ctx, src.ID)
		if err != nil {
			return err
		}
		newID, err = q.InsertPrintJob(ctx, repository.InsertPrintJobParams{DocType: src.DocType, SaleID: src.SaleID, TicketID: src.TicketID, OrderID: src.OrderID,
			ReprintOf: i8(root), CopyNo: n + 1, Document: full.Document, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "print.reprint", newID, nil, map[string]any{"doc_type": src.DocType, "reprint_of": root, "copy_no": n + 1})
	})
	if err != nil {
		return JobView{}, err
	}
	return s.loadJob(ctx, repository.New(s.pool), newID)
}
