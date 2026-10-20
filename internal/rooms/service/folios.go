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
	"zuscar/backend/internal/rooms/repository"
	salesservice "zuscar/backend/internal/sales/service"
)

// The guest folio (ASSUMPTIONS.md S-97 to S-102). Charges and adjustments are rows that never change; money taken is a
// payment held by the sales module. Balance = charges + adjustments - confirmed payments - write-off (AC-08).

const maxAdjustment = 100_000_000 // cents per adjustment (KES 1,000,000)

type FolioItemView struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Desc       string    `json:"description"`
	Amount     int64     `json:"amount_cents"`
	Tax        int64     `json:"tax_cents"`
	NightDate  string    `json:"night_date,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	ReversesID *int64    `json:"reverses_item_id"`
	SaleID     *int64    `json:"sale_id"`
	PostedBy   string    `json:"posted_by_name"`
	CreatedAt  time.Time `json:"created_at"`
	Reversed   bool      `json:"reversed"`
}

type FolioTotals struct {
	ItemsCents    int64 `json:"items_cents"`     // charges plus adjustments (credits are negative)
	PaidCents     int64 `json:"paid_cents"`      // confirmed payments
	PendingCents  int64 `json:"pending_cents"`   // payments waiting for confirmation, not yet counted
	WriteOffCents int64 `json:"write_off_cents"` // approved and given up
	BalanceCents  int64 `json:"balance_cents"`   // what the guest still owes
}

type FolioView struct {
	ID            int64                        `json:"id"`
	FolioNo       int64                        `json:"folio_no"`
	Status        string                       `json:"status"`
	GuestID       int64                        `json:"guest_id"`
	GuestName     string                       `json:"guest_name"`
	GuestPhone    string                       `json:"guest_phone"`
	StayID        *int64                       `json:"stay_id"`
	StayStatus    string                       `json:"stay_status"`
	RoomID        *int64                       `json:"room_id"`
	RoomNumber    string                       `json:"room_number"`
	ReservationID *int64                       `json:"reservation_id"`
	ReservationNo *int64                       `json:"reservation_no"`
	CheckIn       string                       `json:"check_in"`
	ExpectedOut   string                       `json:"expected_out"`
	WriteOffNote  string                       `json:"write_off_reason,omitempty"`
	CreatedAt     time.Time                    `json:"created_at"`
	ClosedAt      *time.Time                   `json:"closed_at"`
	Totals        FolioTotals                  `json:"totals"`
	Items         []FolioItemView              `json:"items"`
	Payments      []salesservice.PaymentRecord `json:"payments"`
}

func (s *Service) totals(ctx context.Context, tx pgx.Tx, q *repository.Queries, folioID, writeOff int64) (FolioTotals, error) {
	items, err := q.FolioItemsTotal(ctx, folioID)
	if err != nil {
		return FolioTotals{}, err
	}
	paid, pending, err := s.pay.FolioPaymentTotals(ctx, tx, folioID)
	if err != nil {
		return FolioTotals{}, err
	}
	return FolioTotals{ItemsCents: items, PaidCents: paid, PendingCents: pending, WriteOffCents: writeOff, BalanceCents: items - paid - writeOff}, nil
}

func (s *Service) folioView(ctx context.Context, tx pgx.Tx, q *repository.Queries, id int64) (FolioView, error) {
	h, err := q.GetFolioHead(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return FolioView{}, &NotFoundError{What: "folio"}
	}
	if err != nil {
		return FolioView{}, err
	}
	rows, err := q.ListFolioItems(ctx, id)
	if err != nil {
		return FolioView{}, err
	}
	items := make([]FolioItemView, 0, len(rows))
	for _, r := range rows {
		it := FolioItemView{ID: r.ID, Kind: r.Kind, Desc: r.Description, Amount: r.AmountCents, Tax: r.TaxCents, NightDate: dateStr(r.NightDate), Reason: r.Reason,
			ReversesID: idPtr(r.ReversesItemID), SaleID: idPtr(r.SaleID), PostedBy: r.PostedByName, CreatedAt: r.CreatedAt.Time, Reversed: r.Reversed}
		items = append(items, it)
	}
	pays, err := s.pay.FolioPayments(ctx, tx, id)
	if err != nil {
		return FolioView{}, err
	}
	tot, err := s.totals(ctx, tx, q, id, h.WriteOffCents)
	if err != nil {
		return FolioView{}, err
	}
	v := FolioView{ID: h.ID, FolioNo: h.FolioNo, Status: h.Status, GuestID: h.GuestID, GuestName: h.GuestName, GuestPhone: h.GuestPhone, StayID: idPtr(h.StayID), StayStatus: h.StayStatus,
		RoomID: idPtr(h.RoomID), RoomNumber: h.RoomNumber, ReservationID: idPtr(h.ReservationID), ReservationNo: idPtr(h.ReservationNo), CheckIn: dateStr(h.CheckIn),
		ExpectedOut: dateStr(h.ExpectedOut), WriteOffNote: h.WriteOffReason, CreatedAt: h.CreatedAt.Time, Totals: tot, Items: items, Payments: pays}
	if h.ClosedAt.Valid {
		t := h.ClosedAt.Time
		v.ClosedAt = &t
	}
	return v, nil
}

func (s *Service) GetFolio(ctx context.Context, actor *auth.Principal, id int64) (FolioView, error) {
	if actor == nil || !actor.Has("folios.view") {
		return FolioView{}, ErrForbidden
	}
	var out FolioView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.folioView(ctx, tx, repository.New(tx), id)
		return err
	})
	return out, err
}

// settled is the rule for ending a folio: no payment still waiting, and the balance exactly zero.
func (s *Service) settled(ctx context.Context, tx pgx.Tx, q *repository.Queries, fol repository.LockFolioRow) error {
	if fol.Status != "open" {
		return &ConflictError{Code: "folio_closed", Msg: "This folio is already closed."}
	}
	t, err := s.totals(ctx, tx, q, fol.ID, fol.WriteOffCents)
	if err != nil {
		return err
	}
	if t.PendingCents > 0 {
		return &ConflictError{Code: "payments_pending", Msg: "A payment on this folio is still waiting to be confirmed. Confirm it or mark it failed first."}
	}
	if t.BalanceCents > 0 {
		return &ConflictError{Code: "balance_due", Msg: "The guest still owes " + kes(t.BalanceCents) + ". Take payment, or have a manager write it off."}
	}
	if t.BalanceCents < 0 {
		return &ConflictError{Code: "folio_overpaid", Msg: "The folio is overpaid by " + kes(-t.BalanceCents) + ". Correct the charges or the payments first."}
	}
	return nil
}

func kes(cents int64) string {
	whole := cents / 100
	s := itoa(whole)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	frac := cents % 100
	return "KES " + b.String() + "." + string("0123456789"[frac/10]) + string("0123456789"[frac%10])
}

// folioForUpdate opens a transaction step for anything that changes a folio: the idempotency check, then the folio lock.
func (s *Service) folioForUpdate(ctx context.Context, tx pgx.Tx, q *repository.Queries, id int64, idem *idempotency.Key) (repository.LockFolioRow, *idempotency.Stored, error) {
	if idem != nil {
		st, err := idempotency.Begin(ctx, tx, idem)
		if err != nil || st != nil {
			return repository.LockFolioRow{}, st, err
		}
	}
	f, err := q.LockFolio(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil, &NotFoundError{What: "folio"}
	}
	return f, nil, err
}

func complete(ctx context.Context, tx pgx.Tx, idem *idempotency.Key, status int, v any) error {
	if idem == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return idempotency.Complete(ctx, tx, idem, status, b)
}

// ---- adjustments ----

type AdjustmentInput struct {
	Description  string `json:"description"`
	AmountCents  int64  `json:"amount_cents"` // positive adds to the bill, negative takes off
	Reason       string `json:"reason"`
	ReversesItem int64  `json:"reverses_item_id"` // credit one item back in full; the amount is then taken from the item
}

// canCredit is the manager scope on money given back: adjustments that lower a bill and write-offs need sales.refund as well.
func canCredit(a *auth.Principal) bool { return a != nil && a.Has("sales.refund") }

func (s *Service) PostAdjustment(ctx context.Context, actor *auth.Principal, folioID int64, in AdjustmentInput, idem *idempotency.Key, m Meta) (FolioView, *idempotency.Stored, error) {
	if !canFolios(actor) {
		return FolioView{}, nil, ErrForbidden
	}
	reason, err := required(in.Reason, "reason", 200)
	if err != nil {
		return FolioView{}, nil, err
	}
	desc := strings.TrimSpace(in.Description)
	if in.ReversesItem == 0 {
		if desc, err = required(in.Description, "description", 200); err != nil {
			return FolioView{}, nil, err
		}
		if in.AmountCents == 0 || in.AmountCents > maxAdjustment || in.AmountCents < -maxAdjustment {
			return FolioView{}, nil, &ValidationError{Field: "amount_cents", Msg: "must be more than 0, or less than 0 for a credit, and within KES 1,000,000"}
		}
	}
	var out FolioView
	var replay *idempotency.Stored
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		fol, st, err := s.folioForUpdate(ctx, tx, q, folioID, idem)
		if err != nil || st != nil {
			replay = st
			return err
		}
		if fol.Status != "open" {
			return &ConflictError{Code: "folio_closed", Msg: "This folio is closed. Reopen it to post to it."}
		}
		amount, rateBP, tax := in.AmountCents, int32(0), int64(0)
		var reverses pgtype.Int8
		if in.ReversesItem != 0 {
			it, err := q.GetFolioItem(ctx, repository.GetFolioItemParams{ID: in.ReversesItem, FolioID: folioID})
			if errors.Is(err, pgx.ErrNoRows) {
				return &ValidationError{Field: "reverses_item_id", Msg: "is not an item on this folio"}
			}
			if err != nil {
				return err
			}
			if it.Kind == "adjustment" && it.AmountCents < 0 {
				return &ValidationError{Field: "reverses_item_id", Msg: "is a credit already; post a new charge instead"}
			}
			if done, err := q.ItemReversed(ctx, pgtype.Int8{Int64: in.ReversesItem, Valid: true}); err != nil {
				return err
			} else if done {
				return &ConflictError{Code: "already_reversed", Field: "reverses_item_id", Msg: "That item was already credited back."}
			}
			amount, rateBP, tax = -it.AmountCents, it.TaxRateBp, -it.TaxCents
			if amount == 0 {
				return &ValidationError{Field: "reverses_item_id", Msg: "has no amount to credit"}
			}
			reverses = pgtype.Int8{Int64: it.ID, Valid: true}
			if desc == "" {
				desc = "Credit: " + it.Description
				if runeLen(desc) > 200 {
					desc = string([]rune(desc)[:200])
				}
			}
		}
		if amount < 0 && !canCredit(actor) {
			return &ConflictError{Code: "manager_required", Msg: "Taking something off a bill needs a manager (the refund permission)."}
		}
		id, err := q.InsertAdjustment(ctx, repository.InsertAdjustmentParams{FolioID: folioID, Description: desc, AmountCents: amount, TaxRateBp: rateBP, TaxCents: tax,
			Reason: pgtype.Text{String: reason, Valid: true}, ReversesItemID: reverses, PostedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "rooms.folio_adjustment", "folio", folioID, nil, map[string]any{"item_id": id, "amount_cents": amount, "reason": reason, "reverses_item_id": in.ReversesItem}); err != nil {
			return err
		}
		if out, err = s.folioView(ctx, tx, q, folioID); err != nil {
			return err
		}
		return complete(ctx, tx, idem, 201, map[string]any{"folio": out})
	})
	return out, replay, err
}

// ---- payments ----

type PaymentInput struct {
	Purpose     string `json:"purpose"` // deposit or settlement (default)
	Method      string `json:"method"`
	AmountCents int64  `json:"amount_cents"`
	Reference   string `json:"reference"`
	Note        string `json:"note"`
}

// TakePayment records a deposit or a payment against the folio. The most it accepts is what is still owed after money
// already promised, so the same balance is never taken twice.
func (s *Service) TakePayment(ctx context.Context, actor *auth.Principal, folioID int64, in PaymentInput, idem *idempotency.Key, m Meta) (FolioView, *idempotency.Stored, error) {
	if !canFolios(actor) {
		return FolioView{}, nil, ErrForbidden
	}
	purpose := "folio_settlement"
	switch in.Purpose {
	case "", "settlement":
	case "deposit":
		purpose = "deposit"
	default:
		return FolioView{}, nil, &ValidationError{Field: "purpose", Msg: "must be deposit or settlement"}
	}
	var out FolioView
	var replay *idempotency.Stored
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		fol, st, err := s.folioForUpdate(ctx, tx, q, folioID, idem)
		if err != nil || st != nil {
			replay = st
			return err
		}
		if fol.Status != "open" {
			return &ConflictError{Code: "folio_closed", Msg: "This folio is closed. Reopen it to take a payment."}
		}
		t, err := s.totals(ctx, tx, q, folioID, fol.WriteOffCents)
		if err != nil {
			return err
		}
		if in.AmountCents > t.BalanceCents-t.PendingCents {
			return &ValidationError{Field: "amount_cents", Msg: "is more than the guest still owes (" + kes(max(t.BalanceCents-t.PendingCents, 0)) + ")"}
		}
		p, err := s.pay.RecordFolioPayment(ctx, tx, actor, salesservice.FolioPayment{FolioID: folioID, Purpose: purpose,
			PaymentInput: salesservice.PaymentInput{Method: in.Method, AmountCents: in.AmountCents, Reference: in.Reference}, Note: in.Note})
		if err != nil {
			return translate(err)
		}
		if err := s.record(ctx, tx, actor, m, "rooms.folio_payment", "folio", folioID, nil, map[string]any{"payment_id": p.ID, "method": p.Method, "status": p.Status, "amount_cents": p.AmountCents, "purpose": purpose}); err != nil {
			return err
		}
		if out, err = s.folioView(ctx, tx, q, folioID); err != nil {
			return err
		}
		return complete(ctx, tx, idem, 201, map[string]any{"folio": out})
	})
	return out, replay, err
}

// ---- write-off ----

// WriteOff gives up part or all of what a guest owes. It needs a manager (folios.manage and the refund permission),
// a reason, and is decided once; later corrections are adjustments.
func (s *Service) WriteOff(ctx context.Context, actor *auth.Principal, folioID, amount int64, reason string, idem *idempotency.Key, m Meta) (FolioView, *idempotency.Stored, error) {
	if !canFolios(actor) {
		return FolioView{}, nil, ErrForbidden
	}
	if !canCredit(actor) {
		return FolioView{}, nil, &ConflictError{Code: "manager_required", Msg: "Writing off a bill needs a manager (the refund permission)."}
	}
	why, err := required(reason, "reason", 200)
	if err != nil {
		return FolioView{}, nil, err
	}
	if amount <= 0 || amount > maxAdjustment {
		return FolioView{}, nil, &ValidationError{Field: "amount_cents", Msg: "must be more than 0"}
	}
	var out FolioView
	var replay *idempotency.Stored
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		fol, st, err := s.folioForUpdate(ctx, tx, q, folioID, idem)
		if err != nil || st != nil {
			replay = st
			return err
		}
		if fol.Status != "open" {
			return &ConflictError{Code: "folio_closed", Msg: "This folio is closed."}
		}
		if fol.WriteOffCents > 0 {
			return &ConflictError{Code: "write_off_exists", Msg: "A write-off was already made on this folio. Post an adjustment for any further change."}
		}
		t, err := s.totals(ctx, tx, q, folioID, 0)
		if err != nil {
			return err
		}
		if amount > t.BalanceCents-t.PendingCents {
			return &ValidationError{Field: "amount_cents", Msg: "is more than the guest still owes (" + kes(max(t.BalanceCents-t.PendingCents, 0)) + ")"}
		}
		if err := q.SetWriteOff(ctx, repository.SetWriteOffParams{ID: folioID, WriteOffCents: amount, WriteOffApprovedBy: pgtype.Int8{Int64: actor.UserID, Valid: true},
			WriteOffReason: pgtype.Text{String: why, Valid: true}}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "rooms.folio_write_off", "folio", folioID, nil, map[string]any{"amount_cents": amount, "reason": why}); err != nil {
			return err
		}
		if out, err = s.folioView(ctx, tx, q, folioID); err != nil {
			return err
		}
		return complete(ctx, tx, idem, 200, map[string]any{"folio": out})
	})
	return out, replay, err
}

// ---- close and reopen ----

// CloseFolio closes a folio that was reopened for a late posting. A folio of a guest still in the house is closed by
// checking the guest out.
func (s *Service) CloseFolio(ctx context.Context, actor *auth.Principal, folioID int64, m Meta) (FolioView, error) {
	if !canFolios(actor) {
		return FolioView{}, ErrForbidden
	}
	var out FolioView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		fol, _, err := s.folioForUpdate(ctx, tx, q, folioID, nil)
		if err != nil {
			return err
		}
		if fol.StayID.Valid {
			st, err := q.StayStatus(ctx, fol.StayID.Int64)
			if err != nil {
				return err
			}
			if st == "in_house" {
				return &ConflictError{Code: "guest_in_house", Msg: "The guest is still in the house. Check them out to close the folio."}
			}
		}
		if err := s.settled(ctx, tx, q, fol); err != nil {
			return err
		}
		if err := q.CloseFolio(ctx, repository.CloseFolioParams{ID: folioID, ClosedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if out, err = s.folioView(ctx, tx, q, folioID); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "rooms.folio_closed", "folio", folioID, map[string]any{"status": "open"}, map[string]any{"status": "closed"})
	})
	return out, err
}

// ReopenFolio opens a closed folio for a late posting (S-20). It does not put the guest back in the room.
func (s *Service) ReopenFolio(ctx context.Context, actor *auth.Principal, folioID int64, reason string, m Meta) (FolioView, error) {
	if !canFolios(actor) {
		return FolioView{}, ErrForbidden
	}
	why, err := required(reason, "reason", 200)
	if err != nil {
		return FolioView{}, err
	}
	var out FolioView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		fol, _, err := s.folioForUpdate(ctx, tx, q, folioID, nil)
		if err != nil {
			return err
		}
		if fol.Status != "closed" {
			return &ConflictError{Code: "folio_open", Msg: "This folio is already open."}
		}
		if err := q.ReopenFolio(ctx, folioID); err != nil {
			return err
		}
		if out, err = s.folioView(ctx, tx, q, folioID); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "rooms.folio_reopened", "folio", folioID, map[string]any{"status": "closed"}, map[string]any{"status": "open", "reason": why})
	})
	return out, err
}
