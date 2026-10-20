package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/sales/repository"
)

// Outage reconciliation (Stage 16, AC-17). While the system is down, sales go on a paper receipt book. Afterwards a
// manager enters each paper receipt once. The entry is a normal sale (so reports, shifts' cash and refunds treat it
// like any other) plus a manual_receipt_reconciliations row. The paper number is unique, so a receipt entered twice is
// refused (S-66 to S-70).

// MaxOutageAge is how far back a paper receipt may be dated (S-67).
const MaxOutageAge = 31 * 24 * time.Hour

var manualNoPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9/._-]{0,31}$`)

// NormaliseManualNo makes "ab 123", " AB123 " and "ab123" the same paper receipt number.
func NormaliseManualNo(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

type OutageLineInput struct {
	ProductID      int64
	Qty            int32
	UnitPriceCents int64 // what the paper receipt charged per unit, tax included
}

type OutageInput struct {
	ManualReceiptNo string
	OccurredAt      time.Time
	Lines           []OutageLineInput
	Payments        []PaymentInput
	PaperTotalCents int64
	DiscrepancyNote string
	SourceNote      string
}

// ManualReceiptInfo is the paper-receipt side of an outage sale, shown on the sale.
type ManualReceiptInfo struct {
	ManualReceiptNo string    `json:"manual_receipt_no"`
	OccurredAt      time.Time `json:"occurred_at"`
	PaperTotalCents int64     `json:"paper_total_cents"`
	DiscrepancyNote string    `json:"discrepancy_note,omitempty"`
	SourceNote      string    `json:"source_note,omitempty"`
	EnteredBy       int64     `json:"entered_by"`
	EnteredByName   string    `json:"entered_by_name"`
}

type ManualReceiptView struct {
	ID              int64     `json:"id"`
	ManualReceiptNo string    `json:"manual_receipt_no"`
	SaleID          int64     `json:"sale_id"`
	ReceiptNo       int64     `json:"receipt_no"`
	OccurredAt      time.Time `json:"occurred_at"`
	PaperTotalCents int64     `json:"paper_total_cents"`
	SaleTotalCents  int64     `json:"sale_total_cents"`
	SaleStatus      string    `json:"sale_status"`
	EnteredBy       int64     `json:"entered_by"`
	EnteredByName   string    `json:"entered_by_name"`
	DiscrepancyNote string    `json:"discrepancy_note,omitempty"`
	SourceNote      string    `json:"source_note,omitempty"`
	EnteredAt       time.Time `json:"entered_at"`
}

func isManualNoUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" &&
		(pe.ConstraintName == "sales_manual_receipt_no_key" || pe.ConstraintName == "manual_receipt_reconciliations_no_key")
}

func dupManual(no string) error {
	return &ConflictError{Code: "duplicate_manual_receipt", Field: "manual_receipt_no", Msg: "Paper receipt " + no + " was already entered."}
}

// EnterManualReceipt records one paper receipt. It needs payments.reconcile and an Idempotency-Key; the claim, the
// sale, its lines and payments, the entry and the audit event commit together.
func (s *Service) EnterManualReceipt(ctx context.Context, actor *auth.Principal, in OutageInput, idem *idempotency.Key, m Meta) (SaleView, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("payments.reconcile") {
		return SaleView{}, nil, ErrForbidden
	}
	no := NormaliseManualNo(in.ManualReceiptNo)
	if !manualNoPattern.MatchString(no) {
		return SaleView{}, nil, &ValidationError{Field: "manual_receipt_no", Msg: "must be 1 to 32 letters, digits, / . _ or -, as printed in the receipt book"}
	}
	now := s.now()
	if in.OccurredAt.IsZero() {
		return SaleView{}, nil, &ValidationError{Field: "occurred_at", Msg: "is required: when the sale was made"}
	}
	if in.OccurredAt.After(now.Add(5 * time.Minute)) {
		return SaleView{}, nil, &ValidationError{Field: "occurred_at", Msg: "cannot be in the future"}
	}
	if in.OccurredAt.Before(now.Add(-MaxOutageAge)) {
		return SaleView{}, nil, &ValidationError{Field: "occurred_at", Msg: "is more than 31 days ago; older paper receipts need the owner"}
	}
	if len(in.Lines) == 0 || len(in.Lines) > 60 {
		return SaleView{}, nil, &ValidationError{Field: "lines", Msg: "needs 1 to 60 lines"}
	}
	ids := make([]int64, 0, len(in.Lines))
	for _, l := range in.Lines {
		if l.ProductID <= 0 || l.Qty < 1 || l.Qty > 999 {
			return SaleView{}, nil, &ValidationError{Field: "lines", Msg: "each line needs a product and a quantity of 1 to 999"}
		}
		if l.UnitPriceCents < 0 || l.UnitPriceCents > 100_000_000_00 {
			return SaleView{}, nil, &ValidationError{Field: "lines", Msg: "unit_price_cents must be 0 or more"}
		}
		ids = append(ids, l.ProductID)
	}
	if in.PaperTotalCents < 0 {
		return SaleView{}, nil, &ValidationError{Field: "paper_total_cents", Msg: "must be 0 or more: the total written on the paper receipt"}
	}
	note := strings.TrimSpace(in.DiscrepancyNote)
	src := strings.TrimSpace(in.SourceNote)
	if runeLen(note) > 200 || runeLen(src) > 200 {
		return SaleView{}, nil, &ValidationError{Field: "discrepancy_note", Msg: "and source_note must be 200 characters or fewer"}
	}
	pays, err := checkPayments(in.Payments)
	if err != nil {
		return SaleView{}, nil, err
	}
	ts, err := s.taxSettings(ctx, actor)
	if err != nil {
		return SaleView{}, nil, err
	}
	blockNeg := s.stock.BlocksNegative(ctx, actor)
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
		q := repository.New(tx)
		taken, err := q.ManualReceiptTaken(ctx, pgtype.Text{String: no, Valid: true})
		if err != nil {
			return err
		}
		if taken {
			return dupManual(no)
		}
		prods, err := s.orders.ProductsForSale(ctx, tx, ids)
		if err != nil {
			return err
		}
		var sub, tax int64
		lines := make([]priced, 0, len(in.Lines))
		for _, l := range in.Lines {
			p, ok := prods[l.ProductID]
			if !ok {
				return &ValidationError{Field: "lines", Msg: "refers to a product that does not exist"}
			}
			mm := calcLine(l.Qty, l.UnitPriceCents, 0, 0, p.TaxRateBP)
			lines = append(lines, priced{l, mm})
			sub += mm.Gross
			tax += mm.Tax
		}
		total := sub
		if in.PaperTotalCents != total && runeLen(note) < 1 {
			return &ValidationError{Field: "discrepancy_note", Msg: "is required because the paper total (" + itoa(in.PaperTotalCents) + " cents) differs from the lines (" + itoa(total) + " cents)"}
		}
		var paid int64
		for _, p := range pays {
			paid += p.AmountCents
		}
		if paid > total {
			return &ValidationError{Field: "payments", Msg: "add up to more than the total (" + itoa(total) + " cents)"}
		}
		no2, err := s.counters.NextNumber(ctx, tx, "receipt")
		if err != nil {
			return err
		}
		loc, lerr := time.LoadLocation(ts.zone)
		if lerr != nil {
			loc = time.FixedZone("EAT", 3*3600)
		}
		at := in.OccurredAt.In(loc)
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		saleID, err := q.InsertOutageSale(ctx, repository.InsertOutageSaleParams{ReceiptNo: no2, BusinessDate: pgtype.Date{Time: day, Valid: true}, SubtotalCents: sub,
			TaxCents: tax, TotalCents: total, PaymentStatus: paymentStatus(total, paid), ManualReceiptNo: pgtype.Text{String: no, Valid: true}, CashierID: actor.UserID, TaxStatus: ts.status})
		if isManualNoUnique(err) {
			return dupManual(no)
		}
		if err != nil {
			return err
		}
		plan, err := s.newStockPlanner(ctx, tx, productIDsOf(lines))
		if err != nil {
			return err
		}
		for _, l := range lines {
			p := prods[l.in.ProductID]
			sn, err := plan.snap(p.ID, p.Module, l.in.Qty)
			if err != nil {
				return err
			}
			if err := q.InsertSaleItem(ctx, repository.InsertSaleItemParams{SaleID: saleID, ProductID: p.ID, NameSnapshot: p.Name, Module: p.Module, Qty: l.in.Qty,
				UnitPriceCents: l.in.UnitPriceCents, TaxCode: p.TaxCode, TaxTreatment: p.TaxTreatment, TaxRateBp: p.TaxRateBP, TaxCents: l.m.Tax, LineTotalCents: l.m.Net,
				InventoryItemID: sn.item, StockUnitsPerSale: sn.per, StockLocationID: sn.loc}); err != nil {
				return err
			}
		}
		if err := s.stock.Consume(ctx, tx, actor, saleID, plan.take, blockNeg); err != nil {
			return err
		}
		confirmedAt := pgtype.Timestamptz{Time: now, Valid: true}
		for _, p := range pays {
			ref := pgtype.Text{}
			if p.Reference != "" {
				ref = pgtype.Text{String: p.Reference, Valid: true}
			}
			pid, err := q.InsertPayment(ctx, repository.InsertPaymentParams{SaleID: pgtype.Int8{Int64: saleID, Valid: true}, Method: p.Method, Status: "confirmed", AmountCents: p.AmountCents,
				Reference: ref, CreatedBy: actor.UserID, ConfirmedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}, ConfirmedAt: confirmedAt, Note: "outage entry " + no})
			if isUniqueRef(err) {
				return &ConflictError{Code: "duplicate_reference", Field: "payments", Msg: "that " + p.Method + " reference was already recorded"}
			}
			if err != nil {
				return err
			}
			if err := s.record(ctx, tx, actor, m, "payment.recorded", "payment", pid, nil, map[string]any{"sale_id": saleID, "method": p.Method, "status": "confirmed", "amount_cents": p.AmountCents, "outage_entry": no}); err != nil {
				return err
			}
		}
		dn := pgtype.Text{}
		if note != "" {
			dn = pgtype.Text{String: note, Valid: true}
		}
		if _, err := q.InsertManualReceipt(ctx, repository.InsertManualReceiptParams{ManualReceiptNo: no, SaleID: saleID, OriginalOccurredAt: pgtype.Timestamptz{Time: in.OccurredAt, Valid: true},
			PaperTotalCents: in.PaperTotalCents, EnteredBy: actor.UserID, DiscrepancyNote: dn, SourceNote: src}); err != nil {
			if isManualNoUnique(err) {
				return dupManual(no)
			}
			return err
		}
		if err := s.record(ctx, tx, actor, m, "sale.outage_entered", "sale", saleID, nil, map[string]any{"receipt_no": no2, "manual_receipt_no": no, "occurred_at": in.OccurredAt.UTC().Format(time.RFC3339),
			"total_cents": total, "paper_total_cents": in.PaperTotalCents, "discrepancy_note": note, "payments": len(pays), "tax_status": ts.status}); err != nil {
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

type ManualReceiptFilter struct {
	ManualReceiptNo string
	BeforeID        int64
	Limit           int
}

// ListManualReceipts lists the entries newest first, or the one entry for a paper number (how the screen warns about a
// duplicate before it is submitted). Needs payments.reconcile.
func (s *Service) ListManualReceipts(ctx context.Context, actor *auth.Principal, f ManualReceiptFilter) ([]ManualReceiptView, bool, error) {
	if actor == nil || !actor.Has("payments.reconcile") {
		return nil, false, ErrForbidden
	}
	limit := clampLimit(f.Limit)
	rows, err := repository.New(s.pool).ListManualReceipts(ctx, repository.ListManualReceiptsParams{ManualReceiptNo: NormaliseManualNo(f.ManualReceiptNo), BeforeID: f.BeforeID, RowLimit: limit + 1})
	if err != nil {
		return nil, false, err
	}
	more := int32(len(rows)) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]ManualReceiptView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ManualReceiptView{ID: r.ID, ManualReceiptNo: r.ManualReceiptNo, SaleID: r.SaleID, ReceiptNo: r.ReceiptNo, OccurredAt: r.OriginalOccurredAt.Time,
			PaperTotalCents: r.PaperTotalCents, SaleTotalCents: r.SaleTotalCents, SaleStatus: r.SaleStatus, EnteredBy: r.EnteredBy, EnteredByName: r.EnteredByName,
			DiscrepancyNote: r.DiscrepancyNote, SourceNote: r.SourceNote, EnteredAt: r.CreatedAt.Time})
	}
	return out, more, nil
}

type priced struct {
	in OutageLineInput
	m  lineMoney
}

func productIDsOf(lines []priced) []int64 {
	ids := make([]int64, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.in.ProductID)
	}
	return ids
}
