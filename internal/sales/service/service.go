// Package service is the sales module's rules (Stage 11a): the bill, atomic finalisation and the sale record.
// The sales service owns the transaction: in one commit it claims the Idempotency-Key, locks the order, takes the
// next receipt number, writes the sale, its immutable line snapshots and its payments, closes the order and
// writes the audit row. A failure anywhere leaves nothing behind, so there is no receipt-number gap (NN-04, A-05).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
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
	"zuscar/backend/internal/sales/repository"
)

var ErrForbidden = errors.New("you do not have permission to do this")

type ForbiddenError struct{ Code, Msg string }

func (e *ForbiddenError) Error() string { return e.Msg }

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

type ConflictError struct{ Code, Field, Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

// Orders is what sales needs from the orders module (docs/architecture.md section 3, rule 4).
type Orders interface {
	OrderForSale(ctx context.Context, tx pgx.Tx, actor *auth.Principal, orderID int64, lock bool, expected *int32) (orderservice.OrderForSale, error)
	MarkFinalised(ctx context.Context, tx pgx.Tx, orderID int64) error
}

// Counters hands out gapless document numbers inside the caller's transaction (settings owns the counters).
type Counters interface {
	NextNumber(ctx context.Context, tx pgx.Tx, docType string) (int64, error)
}

type Settings interface {
	GetSettings(ctx context.Context, actor *auth.Principal) (map[string]json.RawMessage, error)
}

type Service struct {
	pool     *pgxpool.Pool
	rec      audit.Recorder
	orders   Orders
	counters Counters
	settings Settings
	now      func() time.Time
}

func New(pool *pgxpool.Pool, rec audit.Recorder, orders Orders, counters Counters, settings Settings) *Service {
	return &Service{pool: pool, rec: rec, orders: orders, counters: counters, settings: settings, now: time.Now}
}

// WithClock replaces the clock; tests use it to pick the business day.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action, entity string, id int64, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: entity,
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

// Roles that hold sales.create or sales.view in full. Any other role with the permission is limited
// (rbac.md notes 1 and 2): to its own sales, and to bar-module products. See assumption S-47.
func anyModule(actor *auth.Principal) bool {
	return hasRole(actor, "super_admin", "business_manager", "cashier")
}
func seesAllSales(actor *auth.Principal) bool {
	return hasRole(actor, "super_admin", "business_manager", "cashier", "accountant_auditor")
}

// ---- views ----

type SaleItemView struct {
	ID             int64  `json:"id"`
	ProductID      int64  `json:"product_id"`
	Name           string `json:"name"`
	Module         string `json:"module"`
	Qty            int32  `json:"qty"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	ModifiersText  string `json:"modifiers_text"`
	ModifiersCents int64  `json:"modifiers_cents"`
	DiscountCents  int64  `json:"discount_cents"`
	DiscountReason string `json:"discount_reason,omitempty"`
	TaxCode        string `json:"tax_code"`
	TaxTreatment   string `json:"tax_treatment"`
	TaxRateBP      int32  `json:"tax_rate_bp"`
	TaxCents       int64  `json:"tax_cents"`
	LineTotalCents int64  `json:"line_total_cents"`
}

type PaymentView struct {
	ID          int64      `json:"id"`
	Method      string     `json:"method"`
	Status      string     `json:"status"`
	AmountCents int64      `json:"amount_cents"`
	Reference   string     `json:"reference,omitempty"`
	CreatedBy   int64      `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
}

type SaleView struct {
	ID            int64          `json:"id"`
	ReceiptNo     int64          `json:"receipt_no"`
	OrderID       *int64         `json:"order_id"`
	OrderNo       int64          `json:"order_no,omitempty"`
	BusinessDate  string         `json:"business_date"`
	SubtotalCents int64          `json:"subtotal_cents"`
	DiscountCents int64          `json:"discount_cents"`
	TaxCents      int64          `json:"tax_cents"`
	TotalCents    int64          `json:"total_cents"`
	PaidCents     int64          `json:"paid_cents"`    // confirmed payments only (AC-20)
	PendingCents  int64          `json:"pending_cents"` // recorded but not yet confirmed
	BalanceCents  int64          `json:"balance_cents"` // total minus confirmed payments
	Status        string         `json:"status"`
	PaymentStatus string         `json:"payment_status"`
	Source        string         `json:"source"`
	CashierID     int64          `json:"cashier_id"`
	CashierName   string         `json:"cashier_name"`
	WaiterID      *int64         `json:"waiter_id"`
	TaxStatus     string         `json:"tax_status"`
	CreatedAt     time.Time      `json:"created_at"`
	Items         []SaleItemView `json:"items"`
	Payments      []PaymentView  `json:"payments"`
}

func (s *Service) load(ctx context.Context, q *repository.Queries, id int64) (SaleView, error) {
	h, err := q.GetSaleHead(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return SaleView{}, &NotFoundError{What: "sale"}
	}
	if err != nil {
		return SaleView{}, err
	}
	items, err := q.ListSaleItems(ctx, id)
	if err != nil {
		return SaleView{}, err
	}
	pays, err := q.ListSalePayments(ctx, pgtype.Int8{Int64: id, Valid: true})
	if err != nil {
		return SaleView{}, err
	}
	tot, err := q.PaymentTotals(ctx, pgtype.Int8{Int64: id, Valid: true})
	if err != nil {
		return SaleView{}, err
	}
	v := SaleView{ID: h.ID, ReceiptNo: h.ReceiptNo, OrderNo: h.OrderNo, BusinessDate: h.BusinessDate.Time.Format("2006-01-02"), SubtotalCents: h.SubtotalCents,
		DiscountCents: h.DiscountCents, TaxCents: h.TaxCents, TotalCents: h.TotalCents, PaidCents: tot.ConfirmedCents, PendingCents: tot.PendingCents,
		BalanceCents: h.TotalCents - tot.ConfirmedCents, Status: h.Status, PaymentStatus: h.PaymentStatus, Source: h.Source, CashierID: h.CashierID,
		CashierName: h.CashierName, TaxStatus: h.TaxStatus, CreatedAt: h.CreatedAt.Time, Items: []SaleItemView{}, Payments: []PaymentView{}}
	if h.OrderID != 0 {
		o := h.OrderID
		v.OrderID = &o
	}
	if h.WaiterID != 0 {
		w := h.WaiterID
		v.WaiterID = &w
	}
	for _, i := range items {
		v.Items = append(v.Items, SaleItemView{ID: i.ID, ProductID: i.ProductID, Name: i.NameSnapshot, Module: i.Module, Qty: i.Qty, UnitPriceCents: i.UnitPriceCents,
			ModifiersText: i.ModifiersText, ModifiersCents: i.ModifiersCents, DiscountCents: i.DiscountCents, DiscountReason: i.DiscountReason, TaxCode: i.TaxCode,
			TaxTreatment: i.TaxTreatment, TaxRateBP: i.TaxRateBp, TaxCents: i.TaxCents, LineTotalCents: i.LineTotalCents})
	}
	for _, p := range pays {
		pv := PaymentView{ID: p.ID, Method: p.Method, Status: p.Status, AmountCents: p.AmountCents, Reference: p.Reference, CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt.Time}
		if p.ConfirmedAt.Valid {
			t := p.ConfirmedAt.Time
			pv.ConfirmedAt = &t
		}
		v.Payments = append(v.Payments, pv)
	}
	return v, nil
}

// ---- bill ----

type BillLine struct {
	OrderItemID    int64  `json:"order_item_id"`
	Name           string `json:"name"`
	Qty            int32  `json:"qty"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	ModifiersText  string `json:"modifiers_text"`
	ModifiersCents int64  `json:"modifiers_cents"`
	TaxRateBP      int32  `json:"tax_rate_bp"`
	TaxCents       int64  `json:"tax_cents"`
	LineTotalCents int64  `json:"line_total_cents"`
}

type BillView struct {
	OrderID      int64      `json:"order_id"`
	OrderNo      int64      `json:"order_no"`
	Lines        []BillLine `json:"lines"`
	TotalCents   int64      `json:"total_cents"`
	TaxCents     int64      `json:"tax_cents"`
	TaxInclusive bool       `json:"tax_inclusive"`
	TaxStatus    string     `json:"tax_status"`
}

type taxSettings struct {
	inclusive bool
	status    string
	zone      string
}

func (s *Service) taxSettings(ctx context.Context, actor *auth.Principal) (taxSettings, error) {
	raw, err := s.settings.GetSettings(ctx, actor)
	if err != nil {
		return taxSettings{}, err
	}
	out := taxSettings{inclusive: true, status: "provisional_unverified", zone: "Africa/Nairobi"}
	if v, ok := raw["tax_inclusive"]; ok {
		_ = json.Unmarshal(v, &out.inclusive)
	}
	var st string
	if v, ok := raw["tax_config_status"]; ok && json.Unmarshal(v, &st) == nil && st == "verified" {
		out.status = "verified"
	}
	var z string
	if v, ok := raw["timezone"]; ok && json.Unmarshal(v, &z) == nil && z != "" {
		out.zone = z
	}
	return out, nil
}

func checkSalesScope(actor *auth.Principal, lines []orderservice.SaleLine) error {
	if anyModule(actor) {
		return nil
	}
	for _, l := range lines {
		if l.Module != "bar" {
			return &ForbiddenError{Code: "bar_only", Msg: "Your role can only sell bar products."}
		}
	}
	return nil
}

// Bill shows what an order would cost now, from the server's prices. It writes nothing.
func (s *Service) Bill(ctx context.Context, actor *auth.Principal, orderID int64) (BillView, error) {
	if actor == nil || !actor.Has("sales.create") {
		return BillView{}, ErrForbidden
	}
	ts, err := s.taxSettings(ctx, actor)
	if err != nil {
		return BillView{}, err
	}
	var out BillView
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		o, err := s.orders.OrderForSale(ctx, tx, actor, orderID, false, nil)
		if err != nil {
			return err
		}
		if err := checkSalesScope(actor, o.Lines); err != nil {
			return err
		}
		out = BillView{OrderID: o.ID, OrderNo: o.OrderNo, Lines: []BillLine{}, TaxInclusive: ts.inclusive, TaxStatus: ts.status}
		for _, l := range o.Lines {
			m := calcLine(l.Qty, l.UnitPriceCents, l.ModifiersCents, 0, l.TaxRateBP)
			out.Lines = append(out.Lines, BillLine{OrderItemID: l.OrderItemID, Name: l.Name, Qty: l.Qty, UnitPriceCents: l.UnitPriceCents, ModifiersText: l.ModifiersText,
				ModifiersCents: l.ModifiersCents, TaxRateBP: l.TaxRateBP, TaxCents: m.Tax, LineTotalCents: m.Net})
			out.TotalCents += m.Net
			out.TaxCents += m.Tax
		}
		return nil
	})
	return out, err
}

// ---- finalise ----

type PaymentInput struct {
	Method      string
	AmountCents int64
	Reference   string
}

type DiscountInput struct {
	OrderItemID int64
	AmountCents int64
	Reason      string
}

type FinaliseInput struct {
	Discounts       []DiscountInput
	Payments        []PaymentInput
	ExpectedVersion *int32
}

var methods = map[string]bool{"cash": true, "mpesa_manual": true, "card_manual": true, "bank_manual": true}

// checkPayments validates the payments that come with a finalisation or are added later, and normalises references.
// Cash is confirmed at once; every other method is recorded as pending until a person confirms it against the
// statement (NN-11), so it needs a reference to confirm against.
func checkPayments(in []PaymentInput) ([]PaymentInput, error) {
	out := make([]PaymentInput, 0, len(in))
	for _, p := range in {
		if !methods[p.Method] {
			return nil, &ValidationError{Field: "payments", Msg: "method must be cash, mpesa_manual, card_manual or bank_manual"}
		}
		if p.AmountCents <= 0 || p.AmountCents > 100_000_000_00 {
			return nil, &ValidationError{Field: "payments", Msg: "amount_cents must be greater than 0"}
		}
		ref := strings.ToUpper(strings.TrimSpace(p.Reference))
		if p.Method != "cash" && ref == "" {
			return nil, &ValidationError{Field: "payments", Msg: p.Method + " needs a reference (the M-Pesa code, card approval or bank reference)"}
		}
		if len(ref) > 64 {
			return nil, &ValidationError{Field: "payments", Msg: "reference must be 64 characters or fewer"}
		}
		if p.Method == "cash" {
			ref = ""
		}
		out = append(out, PaymentInput{Method: p.Method, AmountCents: p.AmountCents, Reference: ref})
	}
	return out, nil
}

func paymentStatus(total, confirmed int64) string {
	switch {
	case confirmed >= total:
		return "paid"
	case confirmed > 0:
		return "partially_paid"
	}
	return "unpaid"
}

func isUniqueRef(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "payments_method_reference_key"
}

func runeLen(s string) int { return len([]rune(s)) }

// Finalise turns an order into a sale in one transaction. The prices come from the order (set by the server when
// each line was added), never from the request. A retry with the same Idempotency-Key returns the stored sale.
func (s *Service) Finalise(ctx context.Context, actor *auth.Principal, orderID int64, in FinaliseInput, idem *idempotency.Key, m Meta) (SaleView, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("sales.create") {
		return SaleView{}, nil, ErrForbidden
	}
	if len(in.Discounts) > 0 && !actor.Has("sales.discount") {
		return SaleView{}, nil, &ForbiddenError{Code: "discount_not_permitted", Msg: "Your role cannot give discounts."}
	}
	pays, err := checkPayments(in.Payments)
	if err != nil {
		return SaleView{}, nil, err
	}
	disc := map[int64]DiscountInput{}
	for _, d := range in.Discounts {
		reason := strings.TrimSpace(d.Reason)
		if d.AmountCents <= 0 || runeLen(reason) < 1 || runeLen(reason) > 200 {
			return SaleView{}, nil, &ValidationError{Field: "discounts", Msg: "each discount needs an amount above 0 and a reason of 1 to 200 characters"}
		}
		if _, dup := disc[d.OrderItemID]; dup {
			return SaleView{}, nil, &ValidationError{Field: "discounts", Msg: "gives the same line two discounts"}
		}
		disc[d.OrderItemID] = DiscountInput{OrderItemID: d.OrderItemID, AmountCents: d.AmountCents, Reason: reason}
	}
	ts, err := s.taxSettings(ctx, actor)
	if err != nil {
		return SaleView{}, nil, err
	}
	if !ts.inclusive {
		return SaleView{}, nil, &ConflictError{Code: "tax_exclusive_unsupported", Msg: "Tax-exclusive pricing is not supported until the accountant confirms the rule (docs/tax-configuration.md)."}
	}

	var out SaleView
	var replay *idempotency.Stored
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
		o, err := s.orders.OrderForSale(ctx, tx, actor, orderID, true, in.ExpectedVersion)
		if err != nil {
			return err
		}
		if len(o.Lines) == 0 {
			return &ConflictError{Code: "order_empty", Msg: "This order has no items to sell."}
		}
		if err := checkSalesScope(actor, o.Lines); err != nil {
			return err
		}
		known := map[int64]bool{}
		for _, l := range o.Lines {
			known[l.OrderItemID] = true
		}
		for id := range disc {
			if !known[id] {
				return &ValidationError{Field: "discounts", Msg: "refers to a line that is not on this order"}
			}
		}

		var sub, discount, tax int64
		type priced struct {
			line orderservice.SaleLine
			m    lineMoney
		}
		lines := make([]priced, 0, len(o.Lines))
		for _, l := range o.Lines {
			d := disc[l.OrderItemID].AmountCents
			gross := (l.UnitPriceCents + l.ModifiersCents) * int64(l.Qty)
			if d > gross {
				return &ValidationError{Field: "discounts", Msg: "a discount cannot be more than its line (" + l.Name + ")"}
			}
			m := calcLine(l.Qty, l.UnitPriceCents, l.ModifiersCents, d, l.TaxRateBP)
			lines = append(lines, priced{l, m})
			sub += m.Gross
			discount += m.Discount
			tax += m.Tax
		}
		total := sub - discount
		var paid, confirmed int64
		for _, p := range pays {
			paid += p.AmountCents
			if p.Method == "cash" {
				confirmed += p.AmountCents
			}
		}
		if paid > total {
			return &ValidationError{Field: "payments", Msg: "add up to more than the total (" + itoa(total) + " cents); record only the amount that settles the bill"}
		}

		q := repository.New(tx)
		no, err := s.counters.NextNumber(ctx, tx, "receipt")
		if err != nil {
			return err
		}
		loc, lerr := time.LoadLocation(ts.zone)
		if lerr != nil {
			loc = time.FixedZone("EAT", 3*3600)
		}
		nowLocal := s.now().In(loc)
		day := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, time.UTC)
		waiter := pgtype.Int8{Int64: o.OpenedBy, Valid: true}
		saleID, err := q.InsertSale(ctx, repository.InsertSaleParams{ReceiptNo: no, OrderID: pgtype.Int8{Int64: o.ID, Valid: true}, BusinessDate: pgtype.Date{Time: day, Valid: true},
			SubtotalCents: sub, DiscountCents: discount, TaxCents: tax, TotalCents: total, PaymentStatus: paymentStatus(total, confirmed), CashierID: actor.UserID, WaiterID: waiter, TaxStatus: ts.status})
		if err != nil {
			return err
		}
		for _, p := range lines {
			d := disc[p.line.OrderItemID]
			ip := repository.InsertSaleItemParams{SaleID: saleID, OrderItemID: pgtype.Int8{Int64: p.line.OrderItemID, Valid: true}, ProductID: p.line.ProductID, NameSnapshot: p.line.Name,
				Module: p.line.Module, Qty: p.line.Qty, UnitPriceCents: p.line.UnitPriceCents, ModifiersText: p.line.ModifiersText, ModifiersCents: p.line.ModifiersCents,
				DiscountCents: p.m.Discount, TaxCode: p.line.TaxCode, TaxTreatment: p.line.TaxTreatment, TaxRateBp: p.line.TaxRateBP, TaxCents: p.m.Tax, LineTotalCents: p.m.Net}
			if d.AmountCents > 0 {
				ip.DiscountReason = pgtype.Text{String: d.Reason, Valid: true}
				ip.DiscountApprovedBy = pgtype.Int8{Int64: actor.UserID, Valid: true} // the person holding sales.discount who applied it
			}
			if err := q.InsertSaleItem(ctx, ip); err != nil {
				return err
			}
		}
		now := pgtype.Timestamptz{Time: s.now(), Valid: true}
		for _, p := range pays {
			status, by, at := "pending_confirmation", pgtype.Int8{}, pgtype.Timestamptz{}
			if p.Method == "cash" {
				status, by, at = "confirmed", pgtype.Int8{Int64: actor.UserID, Valid: true}, now
			}
			ref := pgtype.Text{}
			if p.Reference != "" {
				ref = pgtype.Text{String: p.Reference, Valid: true}
			}
			pid, err := q.InsertPayment(ctx, repository.InsertPaymentParams{SaleID: pgtype.Int8{Int64: saleID, Valid: true}, Method: p.Method, Status: status, AmountCents: p.AmountCents,
				Reference: ref, CreatedBy: actor.UserID, ConfirmedBy: by, ConfirmedAt: at})
			if isUniqueRef(err) {
				return &ConflictError{Code: "duplicate_reference", Field: "payments", Msg: "that " + p.Method + " reference was already recorded"}
			}
			if err != nil {
				return err
			}
			if err := s.record(ctx, tx, actor, m, "payment.recorded", "payment", pid, nil, map[string]any{"sale_id": saleID, "method": p.Method, "status": status, "amount_cents": p.AmountCents}); err != nil {
				return err
			}
		}
		if err := s.orders.MarkFinalised(ctx, tx, o.ID); err != nil {
			return err
		}
		ids := make([]int64, 0, len(disc))
		for id := range disc {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if err := s.record(ctx, tx, actor, m, "sale.finalised", "sale", saleID, nil, map[string]any{"receipt_no": no, "order_id": o.ID, "order_no": o.OrderNo, "total_cents": total,
			"discount_cents": discount, "tax_cents": tax, "payments": len(pays), "discounted_lines": ids, "tax_status": ts.status}); err != nil {
			return err
		}
		out, err = s.load(ctx, q, saleID)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"sale": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

// ---- reading a sale ----

// GetSale returns a sale with its lines and payments. Roles limited to their own records see only sales they
// made or took the order for; another person's sale answers 404 (S-47).
func (s *Service) GetSale(ctx context.Context, actor *auth.Principal, id int64) (SaleView, error) {
	if actor == nil || !actor.Has("sales.view") {
		return SaleView{}, ErrForbidden
	}
	v, err := s.load(ctx, repository.New(s.pool), id)
	if err != nil {
		return SaleView{}, err
	}
	if !seesAllSales(actor) && v.CashierID != actor.UserID && (v.WaiterID == nil || *v.WaiterID != actor.UserID) {
		return SaleView{}, &NotFoundError{What: "sale"}
	}
	return v, nil
}
