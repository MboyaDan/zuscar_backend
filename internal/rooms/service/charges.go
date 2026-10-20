package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/rooms/repository"
	salesservice "zuscar/backend/internal/sales/service"
)

// Charging a POS order to a guest's room (Stage 24, S-103). It is a sale: it gets a receipt number, takes stock and is
// counted in sales, but nothing is paid; the amount goes on the folio as a charge that the guest settles at the desk.
// A charge is a posting, never a payment (NN-10).

type ChargeDiscount struct {
	OrderItemID int64  `json:"order_item_id"`
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason"`
}

type ChargeInput struct {
	OrderID         int64            `json:"order_id"`
	Discounts       []ChargeDiscount `json:"discounts"`
	ExpectedVersion *int32           `json:"expected_version"`
}

// ChargeResult is what the waiter needs back: the sale that was made and the folio row it became.
type ChargeResult struct {
	SaleID     int64  `json:"sale_id"`
	ReceiptNo  int64  `json:"receipt_no"`
	TotalCents int64  `json:"total_cents"`
	FolioID    int64  `json:"folio_id"`
	ItemID     int64  `json:"item_id"`
	RoomNumber string `json:"room_number"`
	GuestName  string `json:"guest_name"`
}

func canPost(a *auth.Principal) bool { return a != nil && a.Has("folios.post") }

func (s *Service) PostCharge(ctx context.Context, actor *auth.Principal, folioID int64, in ChargeInput, idem *idempotency.Key, m Meta) (ChargeResult, *idempotency.Stored, error) {
	if !canPost(actor) {
		return ChargeResult{}, nil, ErrForbidden
	}
	if in.OrderID <= 0 {
		return ChargeResult{}, nil, &ValidationError{Field: "order_id", Msg: "is required"}
	}
	fin := salesservice.FinaliseInput{ExpectedVersion: in.ExpectedVersion}
	for _, d := range in.Discounts {
		fin.Discounts = append(fin.Discounts, salesservice.DiscountInput{OrderItemID: d.OrderItemID, AmountCents: d.AmountCents, Reason: d.Reason})
	}
	prep, err := s.pay.PrepareFolioCharge(ctx, actor, fin)
	if err != nil {
		return ChargeResult{}, nil, translate(err)
	}
	var out ChargeResult
	var replay *idempotency.Stored
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		fol, st, err := s.folioForUpdate(ctx, tx, q, folioID, idem)
		if err != nil || st != nil {
			replay = st
			return err
		}
		if fol.Status != "open" {
			return &ConflictError{Code: "folio_closed", Msg: "This folio is closed, so nothing can be charged to it."}
		}
		head, err := q.GetFolioHead(ctx, folioID)
		if err != nil {
			return err
		}
		if head.StayStatus != "in_house" {
			return &ConflictError{Code: "guest_not_in_house", Msg: "This guest has left. A room charge can only go to a guest who is in the house."}
		}
		sale, err := s.pay.FinaliseToFolio(ctx, tx, actor, folioID, in.OrderID, fin, prep, salesservice.Meta{RequestID: m.RequestID, IP: m.IP})
		if err != nil {
			return translate(err)
		}
		desc := fmt.Sprintf("Receipt #%d", sale.ReceiptNo)
		itemID, err := q.InsertPosCharge(ctx, repository.InsertPosChargeParams{FolioID: folioID, Description: desc, AmountCents: sale.TotalCents, TaxCents: sale.TaxCents,
			SaleID: pgtype.Int8{Int64: sale.ID, Valid: true}, PostedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "rooms.pos_charge", "folio", folioID, nil, map[string]any{"item_id": itemID, "sale_id": sale.ID, "receipt_no": sale.ReceiptNo,
			"amount_cents": sale.TotalCents, "room": head.RoomNumber}); err != nil {
			return err
		}
		out = ChargeResult{SaleID: sale.ID, ReceiptNo: sale.ReceiptNo, TotalCents: sale.TotalCents, FolioID: folioID, ItemID: itemID, RoomNumber: head.RoomNumber, GuestName: head.GuestName}
		return complete(ctx, tx, idem, 201, map[string]any{"charge": out})
	})
	return out, replay, err
}
