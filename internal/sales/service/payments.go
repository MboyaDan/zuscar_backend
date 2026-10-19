package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/sales/repository"
)

// PaymentRecord is a payment as the payments list and the confirm and fail endpoints show it.
type PaymentRecord struct {
	ID            int64      `json:"id"`
	SaleID        *int64     `json:"sale_id"`
	ReceiptNo     int64      `json:"receipt_no,omitempty"`
	Method        string     `json:"method"`
	Status        string     `json:"status"`
	AmountCents   int64      `json:"amount_cents"`
	Reference     string     `json:"reference,omitempty"`
	CreatedBy     int64      `json:"created_by"`
	CreatedByName string     `json:"created_by_name"`
	CreatedAt     time.Time  `json:"created_at"`
	ConfirmedBy   *int64     `json:"confirmed_by"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`
	FailedBy      *int64     `json:"failed_by"`
	FailedAt      *time.Time `json:"failed_at"`
	Note          string     `json:"note,omitempty"`
	ShiftID       *int64     `json:"shift_id"`
}

type SaleRow struct {
	ID            int64     `json:"id"`
	ReceiptNo     int64     `json:"receipt_no"`
	OrderID       *int64    `json:"order_id"`
	OrderNo       int64     `json:"order_no,omitempty"`
	BusinessDate  string    `json:"business_date"`
	SubtotalCents int64     `json:"subtotal_cents"`
	DiscountCents int64     `json:"discount_cents"`
	TaxCents      int64     `json:"tax_cents"`
	TotalCents    int64     `json:"total_cents"`
	PaidCents     int64     `json:"paid_cents"`
	PendingCents  int64     `json:"pending_cents"`
	BalanceCents  int64     `json:"balance_cents"`
	Status        string    `json:"status"`
	PaymentStatus string    `json:"payment_status"`
	Source        string    `json:"source"`
	CashierID     int64     `json:"cashier_id"`
	CashierName   string    `json:"cashier_name"`
	WaiterID      *int64    `json:"waiter_id"`
	CreatedAt     time.Time `json:"created_at"`
}

func optID(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	x := t.Time
	return &x
}

// ownScope is the user id a list is limited to, or 0 for no limit (S-47).
func ownScope(actor *auth.Principal) int64 {
	if seesAllSales(actor) {
		return 0
	}
	return actor.UserID
}

func clampLimit(n int) int32 {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return int32(n)
}

// ---- adding a payment to a finalised sale ----

type AddPaymentOutput struct {
	Payment PaymentView `json:"payment"`
	Sale    SaleView    `json:"sale"`
}

// AddPayment records another payment against a sale that still has a balance. Cash counts at once; other methods
// wait for payments.reconcile (NN-11). The sale row is locked first, so two payments racing for the last of the
// balance are decided one after the other and the second sees the first (AC-02).
func (s *Service) AddPayment(ctx context.Context, actor *auth.Principal, saleID int64, in PaymentInput, idem *idempotency.Key, m Meta) (AddPaymentOutput, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("sales.create") {
		return AddPaymentOutput{}, nil, ErrForbidden
	}
	pays, err := checkPayments([]PaymentInput{in})
	if err != nil {
		return AddPaymentOutput{}, nil, err
	}
	p := pays[0]
	var out AddPaymentOutput
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
		q := repository.New(tx)
		sale, err := q.LockSale(ctx, saleID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "sale"}
		}
		if err != nil {
			return err
		}
		if !seesAllSales(actor) && sale.CashierID != actor.UserID && sale.WaiterID != actor.UserID {
			return &NotFoundError{What: "sale"}
		}
		if sale.Status != "finalised" || (sale.PaymentStatus != "unpaid" && sale.PaymentStatus != "partially_paid") {
			return &ConflictError{Code: "sale_not_payable", Msg: "This sale cannot take a payment (it is " + sale.Status + " and " + sale.PaymentStatus + ")."}
		}
		tot, err := q.PaymentTotals(ctx, pgtype.Int8{Int64: saleID, Valid: true})
		if err != nil {
			return err
		}
		// Pending money counts against the cap, so the same balance cannot be promised twice.
		if tot.ConfirmedCents+tot.PendingCents+p.AmountCents > sale.TotalCents {
			return &ValidationError{Field: "amount_cents", Msg: "is more than the balance still to pay"}
		}
		status, by, at := "pending_confirmation", pgtype.Int8{}, pgtype.Timestamptz{}
		if p.Method == "cash" {
			status, by, at = "confirmed", pgtype.Int8{Int64: actor.UserID, Valid: true}, pgtype.Timestamptz{Time: s.now(), Valid: true}
		}
		ref := pgtype.Text{}
		if p.Reference != "" {
			ref = pgtype.Text{String: p.Reference, Valid: true}
		}
		pid, err := q.InsertPayment(ctx, repository.InsertPaymentParams{SaleID: pgtype.Int8{Int64: saleID, Valid: true}, Method: p.Method, Status: status,
			AmountCents: p.AmountCents, Reference: ref, CreatedBy: actor.UserID, ConfirmedBy: by, ConfirmedAt: at})
		if isUniqueRef(err) {
			return &ConflictError{Code: "duplicate_reference", Field: "reference", Msg: "that " + p.Method + " reference was already recorded"}
		}
		if err != nil {
			return err
		}
		confirmed := tot.ConfirmedCents
		if status == "confirmed" {
			confirmed += p.AmountCents
		}
		if ns := paymentStatus(sale.TotalCents, confirmed); ns != sale.PaymentStatus {
			if err := q.SetSalePaymentStatus(ctx, repository.SetSalePaymentStatusParams{ID: saleID, PaymentStatus: ns}); err != nil {
				return err
			}
		}
		if err := s.record(ctx, tx, actor, m, "payment.recorded", "payment", pid, nil, map[string]any{"sale_id": saleID, "method": p.Method, "status": status, "amount_cents": p.AmountCents}); err != nil {
			return err
		}
		v, err := s.load(ctx, q, saleID)
		if err != nil {
			return err
		}
		out.Sale = v
		for _, pv := range v.Payments {
			if pv.ID == pid {
				out.Payment = pv
			}
		}
		if idem != nil {
			b, err := json.Marshal(out)
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

// ---- confirming or failing a manual payment ----

type resolveKind int

const (
	doConfirm resolveKind = iota
	doFail
)

func (s *Service) resolve(ctx context.Context, actor *auth.Principal, paymentID int64, kind resolveKind, note string, idem *idempotency.Key, m Meta) (PaymentRecord, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("payments.reconcile") {
		return PaymentRecord{}, nil, ErrForbidden
	}
	note = strings.TrimSpace(note)
	if runeLen(note) > 200 || (kind == doFail && runeLen(note) < 1) {
		return PaymentRecord{}, nil, &ValidationError{Field: "note", Msg: map[bool]string{true: "is required (1 to 200 characters): say why the payment did not arrive", false: "must be 200 characters or fewer"}[kind == doFail]}
	}
	var out PaymentRecord
	var replay *idempotency.Stored
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
		q := repository.New(tx)
		// The sale is locked before the payment, the same order AddPayment takes, so the two cannot deadlock.
		target, err := q.GetPaymentTarget(ctx, paymentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "payment"}
		}
		if err != nil {
			return err
		}
		if !target.SaleID.Valid {
			return &ConflictError{Code: "payment_not_on_sale", Msg: "This payment belongs to a folio; folio payments are handled with the folio."}
		}
		sale, err := q.LockSale(ctx, target.SaleID.Int64)
		if err != nil {
			return err
		}
		cur, err := q.LockPayment(ctx, paymentID)
		if err != nil {
			return err
		}
		if cur.Status != "pending_confirmation" {
			return &ConflictError{Code: "invalid_transition", Msg: "Only a payment waiting for confirmation can be confirmed or failed (this one is " + cur.Status + ")."}
		}
		target2 := "failed"
		action := "payment.failed"
		if kind == doConfirm {
			target2, action = "confirmed", "payment.confirmed"
			err = q.ConfirmPayment(ctx, repository.ConfirmPaymentParams{ID: paymentID, ConfirmedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}, Note: note})
		} else {
			err = q.FailPayment(ctx, repository.FailPaymentParams{ID: paymentID, FailedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}, Note: note})
		}
		if err != nil {
			return err
		}
		tot, err := q.PaymentTotals(ctx, pgtype.Int8{Int64: sale.ID, Valid: true})
		if err != nil {
			return err
		}
		if ns := paymentStatus(sale.TotalCents, tot.ConfirmedCents); ns != sale.PaymentStatus {
			if err := q.SetSalePaymentStatus(ctx, repository.SetSalePaymentStatusParams{ID: sale.ID, PaymentStatus: ns}); err != nil {
				return err
			}
		}
		if err := s.record(ctx, tx, actor, m, action, "payment", paymentID, map[string]any{"status": cur.Status},
			map[string]any{"status": target2, "sale_id": sale.ID, "note": note}); err != nil {
			return err
		}
		out, err = s.paymentRecord(ctx, q, paymentID)
		if err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(map[string]any{"payment": out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 200, b)
		}
		return nil
	})
	return out, replay, err
}

func (s *Service) ConfirmPayment(ctx context.Context, actor *auth.Principal, id int64, note string, idem *idempotency.Key, m Meta) (PaymentRecord, *idempotency.Stored, error) {
	return s.resolve(ctx, actor, id, doConfirm, note, idem, m)
}

func (s *Service) FailPayment(ctx context.Context, actor *auth.Principal, id int64, note string, m Meta) (PaymentRecord, error) {
	r, _, err := s.resolve(ctx, actor, id, doFail, note, nil, m)
	return r, err
}

func (s *Service) paymentRecord(ctx context.Context, q *repository.Queries, id int64) (PaymentRecord, error) {
	r, err := q.GetPaymentRecord(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentRecord{}, &NotFoundError{What: "payment"}
	}
	if err != nil {
		return PaymentRecord{}, err
	}
	return PaymentRecord{ID: r.ID, SaleID: optID(r.SaleID.Int64), ReceiptNo: r.ReceiptNo, Method: r.Method, Status: r.Status, AmountCents: r.AmountCents, Reference: r.Reference,
		CreatedBy: r.CreatedBy, CreatedByName: r.CreatedByName, CreatedAt: r.CreatedAt.Time, ConfirmedBy: optID(r.ConfirmedBy), ConfirmedAt: optTime(r.ConfirmedAt),
		FailedBy: optID(r.FailedBy), FailedAt: optTime(r.FailedAt), Note: r.Note, ShiftID: optID(r.ShiftID)}, nil
}

// ---- lists ----

type PaymentFilter struct {
	Status, Method  string
	ShiftID, SaleID int64
	BeforeID        int64
	Limit           int
}

// ListPayments returns payments newest first. Reconcilers and anyone who sees every sale see all of them; a
// limited role sees payments on its own sales only.
func (s *Service) ListPayments(ctx context.Context, actor *auth.Principal, f PaymentFilter) ([]PaymentRecord, bool, error) {
	if actor == nil || !actor.Has("sales.view") {
		return nil, false, ErrForbidden
	}
	limit := clampLimit(f.Limit)
	rows, err := repository.New(s.pool).ListPayments(ctx, repository.ListPaymentsParams{Status: f.Status, Method: f.Method, ShiftID: f.ShiftID, SaleID: f.SaleID,
		OnlyUser: ownScope(actor), BeforeID: f.BeforeID, RowLimit: limit + 1})
	if err != nil {
		return nil, false, err
	}
	more := int32(len(rows)) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]PaymentRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, PaymentRecord{ID: r.ID, SaleID: optID(r.SaleID.Int64), ReceiptNo: r.ReceiptNo, Method: r.Method, Status: r.Status, AmountCents: r.AmountCents, Reference: r.Reference,
			CreatedBy: r.CreatedBy, CreatedByName: r.CreatedByName, CreatedAt: r.CreatedAt.Time, ConfirmedBy: optID(r.ConfirmedBy), ConfirmedAt: optTime(r.ConfirmedAt),
			FailedBy: optID(r.FailedBy), FailedAt: optTime(r.FailedAt), Note: r.Note, ShiftID: optID(r.ShiftID)})
	}
	return out, more, nil
}

type SaleFilter struct {
	Status, PaymentStatus string
	BusinessDate          *time.Time
	BeforeID              int64
	Limit                 int
}

func (s *Service) ListSales(ctx context.Context, actor *auth.Principal, f SaleFilter) ([]SaleRow, bool, error) {
	if actor == nil || !actor.Has("sales.view") {
		return nil, false, ErrForbidden
	}
	limit := clampLimit(f.Limit)
	bd := pgtype.Date{}
	if f.BusinessDate != nil {
		bd = pgtype.Date{Time: *f.BusinessDate, Valid: true}
	}
	rows, err := repository.New(s.pool).ListSales(ctx, repository.ListSalesParams{Status: f.Status, PaymentStatus: f.PaymentStatus, BusinessDate: bd,
		OnlyUser: ownScope(actor), BeforeID: f.BeforeID, RowLimit: limit + 1})
	if err != nil {
		return nil, false, err
	}
	more := int32(len(rows)) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]SaleRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SaleRow{ID: r.ID, ReceiptNo: r.ReceiptNo, OrderID: optID(r.OrderID), OrderNo: r.OrderNo, BusinessDate: r.BusinessDate.Time.Format("2006-01-02"),
			SubtotalCents: r.SubtotalCents, DiscountCents: r.DiscountCents, TaxCents: r.TaxCents, TotalCents: r.TotalCents, PaidCents: r.PaidCents, PendingCents: r.PendingCents,
			BalanceCents: r.TotalCents - r.PaidCents, Status: r.Status, PaymentStatus: r.PaymentStatus, Source: r.Source, CashierID: r.CashierID, CashierName: r.CashierName,
			WaiterID: optID(r.WaiterID), CreatedAt: r.CreatedAt.Time})
	}
	return out, more, nil
}
