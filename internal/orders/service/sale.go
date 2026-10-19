package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"zuscar/backend/internal/orders/repository"
	"zuscar/backend/internal/platform/auth"
)

// These two methods are what the sales module calls while it finalises a sale (docs/architecture.md section 3,
// rule 4): the sales service opens the transaction and hands it in; orders never commits it. Sales never reads
// or writes the order tables itself.

type SaleLine struct {
	OrderItemID    int64
	ProductID      int64
	Name           string
	Module         string
	Qty            int32
	UnitPriceCents int64
	ModifiersCents int64 // per unit
	ModifiersText  string
	TaxCode        string
	TaxTreatment   string
	TaxRateBP      int32
}

type OrderForSale struct {
	ID        int64
	OrderNo   int64
	OrderType string
	TableID   *int64
	OpenedBy  int64
	Version   int32
	Lines     []SaleLine // active lines only, with the server's prices
}

// OrderForSale loads an order for billing or finalising. With lock it takes the order row FOR UPDATE, so two
// finalisations of the same order take turns. The person must be allowed to see the order (S-43); a finalised
// order answers order_already_finalised, a cancelled one order_closed.
func (s *Service) OrderForSale(ctx context.Context, tx pgx.Tx, actor *auth.Principal, orderID int64, lock bool, expected *int32) (OrderForSale, error) {
	q := repository.New(tx)
	var status string
	var version int32
	var openedBy int64
	if lock {
		o, err := q.LockOrder(ctx, orderID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return OrderForSale{}, &NotFoundError{What: "order"}
			}
			return OrderForSale{}, err
		}
		status, version, openedBy = o.Status, o.Version, o.OpenedBy
	} else {
		o, err := q.GetOrderHead(ctx, orderID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return OrderForSale{}, &NotFoundError{What: "order"}
			}
			return OrderForSale{}, err
		}
		status, version, openedBy = o.Status, o.Version, o.OpenedBy
	}
	if !seesAllOrders(actor) && openedBy != actor.UserID {
		return OrderForSale{}, &NotFoundError{What: "order"}
	}
	switch status {
	case "finalised":
		return OrderForSale{}, &ConflictError{Code: "order_already_finalised", Msg: "This order was already finalised."}
	case "cancelled":
		return OrderForSale{}, &ConflictError{Code: "order_closed", Msg: "This order was cancelled."}
	}
	if expected != nil && *expected != version {
		return OrderForSale{}, &ConflictError{Code: "order_changed", Msg: "Someone else changed this order. Reload it and try again."}
	}
	head, err := q.GetOrderHead(ctx, orderID)
	if err != nil {
		return OrderForSale{}, err
	}
	rows, err := q.ListSaleLines(ctx, orderID)
	if err != nil {
		return OrderForSale{}, err
	}
	out := OrderForSale{ID: orderID, OrderNo: head.OrderNo, OrderType: head.OrderType, OpenedBy: openedBy, Version: version}
	if head.TableID.Valid {
		id := head.TableID.Int64
		out.TableID = &id
	}
	for _, r := range rows {
		out.Lines = append(out.Lines, SaleLine{OrderItemID: r.ID, ProductID: r.ProductID, Name: r.ProductName, Module: r.Module, Qty: r.Qty, UnitPriceCents: r.UnitPriceCents,
			ModifiersCents: r.ModifiersCents, ModifiersText: r.ModifiersText, TaxCode: r.TaxCode, TaxTreatment: r.TaxTreatment, TaxRateBP: r.TaxRateBp})
	}
	return out, nil
}

// MarkFinalised closes the order once its sale exists. The database then refuses any further change to it.
func (s *Service) MarkFinalised(ctx context.Context, tx pgx.Tx, orderID int64) error {
	return repository.New(tx).MarkOrderFinalised(ctx, orderID)
}
