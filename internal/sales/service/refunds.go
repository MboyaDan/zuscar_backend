package service

// Stage 15a: refunds and voids (ASSUMPTIONS.md S-18, S-63).
//
// A sale and its payments are never edited. A correction adds records: a row in `refunds` (kind refund or void),
// the lines taken back, and the money handed back by each method. The sale row is locked first, then the shift
// (the same order as taking a payment), so two corrections racing for the same line or the same cash are decided
// one after the other and the second sees the first.

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/sales/repository"
)

// ---- input and views ----

type RefundLineInput struct {
	SaleItemID int64 `json:"sale_item_id"`
	Qty        int32 `json:"qty"`
}

type PayoutInput struct {
	Method      string `json:"method"`
	AmountCents int64  `json:"amount_cents"`
	Reference   string `json:"reference"`
}

type RefundInput struct {
	Reason  string            `json:"reason"`
	Lines   []RefundLineInput `json:"lines"`
	Payouts []PayoutInput     `json:"payouts"`
}

type RefundItemView struct {
	SaleItemID  int64  `json:"sale_item_id"`
	Name        string `json:"name"`
	Qty         int32  `json:"qty"`
	AmountCents int64  `json:"amount_cents"`
	TaxCents    int64  `json:"tax_cents"`
}

type PayoutView struct {
	Method      string `json:"method"`
	AmountCents int64  `json:"amount_cents"`
	Reference   string `json:"reference,omitempty"`
	ShiftID     *int64 `json:"shift_id"`
}

type RefundView struct {
	ID             int64            `json:"id"`
	RefundNo       int64            `json:"refund_no"`
	Kind           string           `json:"kind"` // refund | void
	Reason         string           `json:"reason"`
	ApprovedBy     int64            `json:"approved_by"`
	ApprovedByName string           `json:"approved_by_name"`
	CreatedBy      int64            `json:"created_by"`
	CreatedByName  string           `json:"created_by_name"`
	BusinessDate   string           `json:"business_date"`
	AmountCents    int64            `json:"amount_cents"`
	TaxCents       int64            `json:"tax_cents"`
	CreatedAt      time.Time        `json:"created_at"`
	Items          []RefundItemView `json:"items"`
	Payouts        []PayoutView     `json:"payouts"`
}

type CorrectionOutput struct {
	Sale   SaleView   `json:"sale"`
	Refund RefundView `json:"refund"`
}

type QuoteLine struct {
	SaleItemID  int64  `json:"sale_item_id"`
	Name        string `json:"name"`
	Qty         int32  `json:"qty"`
	AmountCents int64  `json:"amount_cents"`
	TaxCents    int64  `json:"tax_cents"`
}

type MethodRoom struct {
	Method          string `json:"method"`
	ReturnableCents int64  `json:"returnable_cents"` // confirmed money received by this method, minus what was already returned
}

// RefundQuote is what a refund of the chosen lines comes to, worked out by the server so the screen never does the sums.
type RefundQuote struct {
	AmountCents int64        `json:"amount_cents"`
	TaxCents    int64        `json:"tax_cents"`
	Lines       []QuoteLine  `json:"lines"`
	Methods     []MethodRoom `json:"methods"`
}

// ---- the sums ----

// share is total*n/of rounded half up. Refunds take the difference between the share after and the share before,
// so however a line is refunded in pieces the pieces add up to exactly the line.
func share(total int64, n, of int32) int64 {
	if of <= 0 {
		return 0
	}
	return (total*int64(n)*2 + int64(of)) / (2 * int64(of))
}

type taken struct {
	item   repository.ListSaleItemsRow
	qty    int32
	amount int64
	tax    int64
}

type priorTake struct {
	qty    int32
	amount int64
	tax    int64
}

func priorByItem(rows []repository.RefundedQtyBySaleItemRow) map[int64]priorTake {
	m := make(map[int64]priorTake, len(rows))
	for _, r := range rows {
		m[r.SaleItemID] = priorTake{qty: r.Qty, amount: r.AmountCents, tax: r.TaxCents}
	}
	return m
}

func takeLines(items []repository.ListSaleItemsRow, prior map[int64]priorTake, lines []RefundLineInput) ([]taken, int64, int64, error) {
	if len(lines) == 0 {
		return nil, 0, 0, &ValidationError{Field: "lines", Msg: "name at least one line to take back"}
	}
	byID := make(map[int64]repository.ListSaleItemsRow, len(items))
	for _, i := range items {
		byID[i.ID] = i
	}
	seen := map[int64]bool{}
	var out []taken
	var amount, tax int64
	for _, l := range lines {
		it, ok := byID[l.SaleItemID]
		if !ok {
			return nil, 0, 0, &ValidationError{Field: "lines", Msg: "refers to a line that is not on this sale"}
		}
		if seen[l.SaleItemID] {
			return nil, 0, 0, &ValidationError{Field: "lines", Msg: "names the same line twice"}
		}
		seen[l.SaleItemID] = true
		if l.Qty <= 0 {
			return nil, 0, 0, &ValidationError{Field: "lines", Msg: "each quantity must be 1 or more"}
		}
		p := prior[l.SaleItemID]
		if p.qty+l.Qty > it.Qty {
			return nil, 0, 0, &ConflictError{Code: "refund_exceeds_sold", Field: "lines",
				Msg: it.NameSnapshot + ": " + strconv.Itoa(int(it.Qty-p.qty)) + " left to take back, you asked for " + strconv.Itoa(int(l.Qty)) + "."}
		}
		a := share(it.LineTotalCents, p.qty+l.Qty, it.Qty) - p.amount
		t := share(it.TaxCents, p.qty+l.Qty, it.Qty) - p.tax
		out = append(out, taken{item: it, qty: l.Qty, amount: a, tax: t})
		amount += a
		tax += t
	}
	return out, amount, tax, nil
}

func (s *Service) methodRoom(ctx context.Context, q *repository.Queries, saleID int64) (map[string]int64, error) {
	got, err := q.ConfirmedByMethod(ctx, pgtype.Int8{Int64: saleID, Valid: true})
	if err != nil {
		return nil, err
	}
	back, err := q.ReturnedByMethod(ctx, saleID)
	if err != nil {
		return nil, err
	}
	room := map[string]int64{}
	for _, g := range got {
		room[g.Method] = g.AmountCents
	}
	for _, b := range back {
		room[b.Method] -= b.AmountCents
	}
	return room, nil
}

// ---- reading ----

func (s *Service) refundsFor(ctx context.Context, q *repository.Queries, saleID int64) ([]RefundView, error) {
	rows, err := q.ListRefundsForSale(ctx, saleID)
	if err != nil {
		return nil, err
	}
	items, err := q.ListRefundItemsForSale(ctx, saleID)
	if err != nil {
		return nil, err
	}
	pays, err := q.ListRefundPayoutsForSale(ctx, saleID)
	if err != nil {
		return nil, err
	}
	out := make([]RefundView, 0, len(rows))
	idx := map[int64]int{}
	for _, r := range rows {
		idx[r.ID] = len(out)
		out = append(out, RefundView{ID: r.ID, RefundNo: r.RefundNo, Kind: r.Kind, Reason: r.Reason, ApprovedBy: r.ApprovedBy, ApprovedByName: r.ApprovedByName,
			CreatedBy: r.CreatedBy, CreatedByName: r.CreatedByName, BusinessDate: r.BusinessDate.Time.Format("2006-01-02"), AmountCents: r.AmountCents,
			TaxCents: r.TaxCents, CreatedAt: r.CreatedAt.Time, Items: []RefundItemView{}, Payouts: []PayoutView{}})
	}
	for _, i := range items {
		v := &out[idx[i.RefundID]]
		v.Items = append(v.Items, RefundItemView{SaleItemID: i.SaleItemID, Name: i.Name, Qty: i.Qty, AmountCents: i.AmountCents, TaxCents: i.TaxCents})
	}
	for _, p := range pays {
		v := &out[idx[p.RefundID]]
		v.Payouts = append(v.Payouts, PayoutView{Method: p.Method, AmountCents: p.AmountCents, Reference: p.Reference, ShiftID: optID(p.ShiftID)})
	}
	return out, nil
}

// businessDay is today's business date at the venue (S-05), as a date at midnight UTC.
func (s *Service) businessDay(ctx context.Context, actor *auth.Principal) (time.Time, error) {
	ts, err := s.taxSettings(ctx, actor)
	if err != nil {
		return time.Time{}, err
	}
	loc, lerr := time.LoadLocation(ts.zone)
	if lerr != nil {
		loc = time.FixedZone("EAT", 3*3600)
	}
	n := s.now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC), nil
}

// lockForCorrection locks the sale and checks the actor may act on it.
func lockForCorrection(ctx context.Context, q *repository.Queries, actor *auth.Principal, saleID int64) (repository.LockSaleForRefundRow, error) {
	sale, err := q.LockSaleForRefund(ctx, saleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sale, &NotFoundError{What: "sale"}
	}
	if err != nil {
		return sale, err
	}
	if !seesAllSales(actor) && sale.CashierID != actor.UserID && sale.WaiterID != actor.UserID {
		return sale, &NotFoundError{What: "sale"}
	}
	return sale, nil
}

// QuoteRefund works out what taking back the chosen lines comes to and how much each method can still return.
func (s *Service) QuoteRefund(ctx context.Context, actor *auth.Principal, saleID int64, lines []RefundLineInput) (RefundQuote, error) {
	if actor == nil || !actor.Has("sales.refund") {
		return RefundQuote{}, ErrForbidden
	}
	q := repository.New(s.pool)
	h, err := q.GetSaleHead(ctx, saleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundQuote{}, &NotFoundError{What: "sale"}
	}
	if err != nil {
		return RefundQuote{}, err
	}
	if !seesAllSales(actor) && h.CashierID != actor.UserID && h.WaiterID != actor.UserID {
		return RefundQuote{}, &NotFoundError{What: "sale"}
	}
	items, err := q.ListSaleItems(ctx, saleID)
	if err != nil {
		return RefundQuote{}, err
	}
	pr, err := q.RefundedQtyBySaleItem(ctx, saleID)
	if err != nil {
		return RefundQuote{}, err
	}
	tk, amount, tax, err := takeLines(items, priorByItem(pr), lines)
	if err != nil {
		return RefundQuote{}, err
	}
	room, err := s.methodRoom(ctx, q, saleID)
	if err != nil {
		return RefundQuote{}, err
	}
	out := RefundQuote{AmountCents: amount, TaxCents: tax, Lines: []QuoteLine{}, Methods: []MethodRoom{}}
	for _, t := range tk {
		out.Lines = append(out.Lines, QuoteLine{SaleItemID: t.item.ID, Name: t.item.NameSnapshot, Qty: t.qty, AmountCents: t.amount, TaxCents: t.tax})
	}
	for m, c := range room {
		if c > 0 {
			out.Methods = append(out.Methods, MethodRoom{Method: m, ReturnableCents: c})
		}
	}
	sort.Slice(out.Methods, func(i, j int) bool { return out.Methods[i].Method < out.Methods[j].Method })
	return out, nil
}

// ---- writing ----

func cleanReason(r string) (string, error) {
	r = strings.TrimSpace(r)
	if n := runeLen(r); n < 1 || n > 200 {
		return "", &ValidationError{Field: "reason", Msg: "is required and must be 200 characters or fewer"}
	}
	return r, nil
}

// cashOutShift finds the drawer cash will leave: the actor's own open shift, held FOR SHARE so a close waits for us.
func cashOutShift(ctx context.Context, q *repository.Queries, actor *auth.Principal) (pgtype.Int8, error) {
	id, err := q.OpenShiftForUser(ctx, actor.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.Int8{}, &ConflictError{Code: "shift_required", Msg: "Open your shift before handing cash back."}
	}
	if err != nil {
		return pgtype.Int8{}, err
	}
	return pgtype.Int8{Int64: id, Valid: true}, nil
}

// record writes the correction: the refund row, the lines, the payouts, the new sale status and the audit row.
func (s *Service) writeCorrection(ctx context.Context, tx pgx.Tx, q *repository.Queries, actor *auth.Principal, m Meta, sale repository.LockSaleForRefundRow,
	kind, reason string, tk []taken, amount, tax int64, payouts []PayoutInput, items []repository.ListSaleItemsRow, prior map[int64]priorTake, day time.Time) (int64, error) {
	no, err := s.counters.NextNumber(ctx, tx, "refund")
	if err != nil {
		return 0, err
	}
	rid, err := q.InsertRefund(ctx, repository.InsertRefundParams{RefundNo: no, SaleID: sale.ID, Kind: kind, Reason: reason, ApprovedBy: actor.UserID, CreatedBy: actor.UserID,
		BusinessDate: pgtype.Date{Time: day, Valid: true}, AmountCents: amount, TaxCents: tax})
	if err != nil {
		return 0, err
	}
	now := map[int64]int32{}
	for _, t := range tk {
		if err := q.InsertRefundItem(ctx, repository.InsertRefundItemParams{RefundID: rid, SaleItemID: t.item.ID, Qty: t.qty, AmountCents: t.amount, TaxCents: t.tax}); err != nil {
			return 0, err
		}
		now[t.item.ID] = t.qty
	}
	for _, p := range payouts {
		shift := pgtype.Int8{}
		if p.Method == "cash" {
			if shift, err = cashOutShift(ctx, q, actor); err != nil {
				return 0, err
			}
		}
		ref := pgtype.Text{}
		if p.Reference != "" {
			ref = pgtype.Text{String: p.Reference, Valid: true}
		}
		if err := q.InsertRefundPayout(ctx, repository.InsertRefundPayoutParams{RefundID: rid, Method: p.Method, AmountCents: p.AmountCents, Reference: ref, ShiftID: shift}); err != nil {
			return 0, err
		}
	}
	whole := true
	for _, i := range items {
		if prior[i.ID].qty+now[i.ID] < i.Qty {
			whole = false
		}
	}
	status := "partially_refunded"
	switch {
	case kind == "void":
		status = "voided"
	case whole:
		status = "refunded"
	}
	if err := q.SetSaleStatus(ctx, repository.SetSaleStatusParams{ID: sale.ID, Status: status}); err != nil {
		return 0, err
	}
	var returned int64
	for _, p := range payouts {
		returned += p.AmountCents
	}
	action := "sale.refunded"
	if kind == "void" {
		action = "sale.voided"
	}
	if err := s.record(ctx, tx, actor, m, action, "sale", sale.ID, map[string]any{"status": sale.Status},
		map[string]any{"status": status, "refund_id": rid, "refund_no": no, "reason": reason, "amount_cents": amount, "tax_cents": tax, "returned_cents": returned, "approved_by": actor.UserID}); err != nil {
		return 0, err
	}
	return rid, nil
}

func (s *Service) correctionOutput(ctx context.Context, q *repository.Queries, saleID, refundID int64) (CorrectionOutput, error) {
	v, err := s.load(ctx, q, saleID)
	if err != nil {
		return CorrectionOutput{}, err
	}
	out := CorrectionOutput{Sale: v}
	for _, r := range v.Refunds {
		if r.ID == refundID {
			out.Refund = r
		}
	}
	return out, nil
}

// Refund takes some lines of a finalised sale back and returns the money. Any business day; needs sales.refund and a reason.
func (s *Service) Refund(ctx context.Context, actor *auth.Principal, saleID int64, in RefundInput, idem *idempotency.Key, m Meta) (CorrectionOutput, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("sales.refund") {
		return CorrectionOutput{}, nil, ErrForbidden
	}
	reason, err := cleanReason(in.Reason)
	if err != nil {
		return CorrectionOutput{}, nil, err
	}
	if len(in.Payouts) == 0 {
		return CorrectionOutput{}, nil, &ValidationError{Field: "payouts", Msg: "say how the money goes back (cash, mpesa_manual, card_manual or bank_manual)"}
	}
	seen := map[string]bool{}
	pay := make([]PayoutInput, 0, len(in.Payouts))
	var paySum int64
	for _, p := range in.Payouts {
		if !methods[p.Method] {
			return CorrectionOutput{}, nil, &ValidationError{Field: "payouts", Msg: "method must be cash, mpesa_manual, card_manual or bank_manual"}
		}
		if seen[p.Method] {
			return CorrectionOutput{}, nil, &ValidationError{Field: "payouts", Msg: "lists " + p.Method + " twice"}
		}
		seen[p.Method] = true
		if p.AmountCents <= 0 || p.AmountCents > maxCents {
			return CorrectionOutput{}, nil, &ValidationError{Field: "payouts", Msg: "each amount_cents must be greater than 0"}
		}
		ref := strings.ToUpper(strings.TrimSpace(p.Reference))
		if len(ref) > 64 {
			return CorrectionOutput{}, nil, &ValidationError{Field: "payouts", Msg: "reference must be 64 characters or fewer"}
		}
		if p.Method == "cash" {
			ref = ""
		}
		paySum += p.AmountCents
		pay = append(pay, PayoutInput{Method: p.Method, AmountCents: p.AmountCents, Reference: ref})
	}
	day, err := s.businessDay(ctx, actor)
	if err != nil {
		return CorrectionOutput{}, nil, err
	}
	var out CorrectionOutput
	replay, _, err := s.runIdem(ctx, idem, 201, func(tx pgx.Tx, q *repository.Queries) (any, error) {
		sale, err := lockForCorrection(ctx, q, actor, saleID)
		if err != nil {
			return nil, err
		}
		if sale.PaymentStatus == "charged_to_folio" {
			return nil, &ConflictError{Code: "refund_unsupported", Msg: "A sale charged to a room folio is corrected on the folio."}
		}
		if sale.Status != "finalised" && sale.Status != "partially_refunded" {
			return nil, &ConflictError{Code: "sale_not_refundable", Msg: "This sale is " + sale.Status + " and cannot be refunded."}
		}
		items, err := q.ListSaleItems(ctx, saleID)
		if err != nil {
			return nil, err
		}
		pr, err := q.RefundedQtyBySaleItem(ctx, saleID)
		if err != nil {
			return nil, err
		}
		prior := priorByItem(pr)
		tk, amount, tax, err := takeLines(items, prior, in.Lines)
		if err != nil {
			return nil, err
		}
		if paySum != amount {
			return nil, &ValidationError{Field: "payouts", Msg: "must add up to the refund (" + strconv.FormatInt(amount, 10) + " cents); they add up to " + strconv.FormatInt(paySum, 10)}
		}
		room, err := s.methodRoom(ctx, q, saleID)
		if err != nil {
			return nil, err
		}
		for _, p := range pay {
			if p.AmountCents > room[p.Method] {
				return nil, &ConflictError{Code: "payout_exceeds_received", Field: "payouts",
					Msg: "Only " + strconv.FormatInt(max64(room[p.Method], 0), 10) + " cents can still go back by " + p.Method + " (that is what was received that way, less what was already returned)."}
			}
		}
		rid, err := s.writeCorrection(ctx, tx, q, actor, m, sale, "refund", reason, tk, amount, tax, pay, items, prior, day)
		if err != nil {
			return nil, err
		}
		out, err = s.correctionOutput(ctx, q, saleID, rid)
		return out, err
	})
	return out, replay, err
}

// Void cancels a sale that should never have happened: only while finalised, with no refund, on the day it was made,
// and only as a whole. The money received goes back by the methods it came in by. Needs sales.void and a reason.
func (s *Service) Void(ctx context.Context, actor *auth.Principal, saleID int64, reasonIn string, idem *idempotency.Key, m Meta) (CorrectionOutput, *idempotency.Stored, error) {
	if actor == nil || !actor.Has("sales.void") {
		return CorrectionOutput{}, nil, ErrForbidden
	}
	reason, err := cleanReason(reasonIn)
	if err != nil {
		return CorrectionOutput{}, nil, err
	}
	day, err := s.businessDay(ctx, actor)
	if err != nil {
		return CorrectionOutput{}, nil, err
	}
	var out CorrectionOutput
	replay, _, err := s.runIdem(ctx, idem, 201, func(tx pgx.Tx, q *repository.Queries) (any, error) {
		sale, err := lockForCorrection(ctx, q, actor, saleID)
		if err != nil {
			return nil, err
		}
		switch sale.Status {
		case "finalised":
		case "voided":
			return nil, &ConflictError{Code: "sale_already_voided", Msg: "This sale is already voided."}
		default:
			return nil, &ConflictError{Code: "void_after_refund", Msg: "This sale already has a refund, so it cannot be voided. Refund the rest instead."}
		}
		if sale.PaymentStatus == "charged_to_folio" {
			return nil, &ConflictError{Code: "refund_unsupported", Msg: "A sale charged to a room folio is corrected on the folio."}
		}
		if !sale.BusinessDate.Time.Equal(day) {
			return nil, &ConflictError{Code: "void_window_closed", Msg: "A sale can be voided only on the day it was made. Refund it instead."}
		}
		pending, err := q.CountPendingPayments(ctx, pgtype.Int8{Int64: saleID, Valid: true})
		if err != nil {
			return nil, err
		}
		if pending > 0 {
			return nil, &ConflictError{Code: "pending_payments", Msg: "This sale has payments waiting for confirmation. Confirm or fail them first."}
		}
		items, err := q.ListSaleItems(ctx, saleID)
		if err != nil {
			return nil, err
		}
		prior := map[int64]priorTake{}
		var all []RefundLineInput
		for _, i := range items {
			all = append(all, RefundLineInput{SaleItemID: i.ID, Qty: i.Qty})
		}
		tk, amount, tax, err := takeLines(items, prior, all)
		if err != nil {
			return nil, err
		}
		room, err := s.methodRoom(ctx, q, saleID)
		if err != nil {
			return nil, err
		}
		var pay []PayoutInput
		for method, c := range room {
			if c > 0 {
				pay = append(pay, PayoutInput{Method: method, AmountCents: c})
			}
		}
		sort.Slice(pay, func(i, j int) bool { return pay[i].Method < pay[j].Method })
		rid, err := s.writeCorrection(ctx, tx, q, actor, m, sale, "void", reason, tk, amount, tax, pay, items, prior, day)
		if err != nil {
			return nil, err
		}
		out, err = s.correctionOutput(ctx, q, saleID, rid)
		return out, err
	})
	return out, replay, err
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
