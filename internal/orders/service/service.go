// Package service is the orders module's rules (Stage 10a): dining tables, orders and their items. Sending to the
// kitchen and tickets arrive in Stage 10b. Prices are always read from the catalogue on the server and copied onto
// the line; a client can never supply one. Every order change locks the order row first, so two tills editing the
// same order take turns, and the order's version counts each change.
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

	"zuscar/backend/internal/orders/repository"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/platform/idempotency"
)

var ErrForbidden = errors.New("you do not have permission to do this")

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

// ConflictError is a clash with the current state; Code is the machine-readable reason the screens act on.
type ConflictError struct{ Code, Field, Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

type Service struct {
	pool *pgxpool.Pool
	rec  audit.Recorder
}

func New(pool *pgxpool.Pool, rec audit.Recorder) *Service { return &Service{pool: pool, rec: rec} }

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func (s *Service) need(actor *auth.Principal, permission string) error {
	if actor == nil || !actor.Has(permission) {
		return ErrForbidden
	}
	return nil
}

func (s *Service) record(ctx context.Context, tx pgx.Tx, actor *auth.Principal, m Meta, action, entity string, id int64, before, after map[string]any) error {
	return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: action, EntityType: entity,
		EntityID: itoa(id), Before: before, After: after, RequestID: m.RequestID, IP: m.IP})
}

// seesAllOrders: the owner's brief lists order visibility as "own, or all" without saying how a role is
// limited, so these roles see every order and any other role sees only the orders it opened (assumption S-43).
func seesAllOrders(actor *auth.Principal) bool {
	for _, r := range actor.Roles {
		switch r {
		case "super_admin", "business_manager", "cashier", "accountant_auditor":
			return true
		}
	}
	return false
}

// ---- tables ----

type TableView struct {
	ID          int64  `json:"id"`
	Label       string `json:"label"`
	Seats       int32  `json:"seats"`
	Active      bool   `json:"active"`
	OpenOrderID *int64 `json:"open_order_id"`
}

func checkLabel(v string) (string, error) {
	v = strings.TrimSpace(v)
	if n := len([]rune(v)); n < 1 || n > 40 {
		return "", &ValidationError{Field: "label", Msg: "must be 1 to 40 characters"}
	}
	return v, nil
}

func checkSeats(n int32) error {
	if n < 1 || n > 100 {
		return &ValidationError{Field: "seats", Msg: "must be between 1 and 100"}
	}
	return nil
}

func isUnique(err error, constraint string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == constraint
}

func (s *Service) ListTables(ctx context.Context, actor *auth.Principal) ([]TableView, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return nil, err
	}
	rows, err := repository.New(s.pool).ListTables(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TableView, 0, len(rows))
	for _, r := range rows {
		v := TableView{ID: r.ID, Label: r.Label, Seats: r.Seats, Active: r.Active}
		if r.OpenOrderID != 0 {
			id := r.OpenOrderID
			v.OpenOrderID = &id
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) CreateTable(ctx context.Context, actor *auth.Principal, label string, seats int32, m Meta) (TableView, error) {
	if err := s.need(actor, "catalogue.manage"); err != nil {
		return TableView{}, err
	}
	label, err := checkLabel(label)
	if err != nil {
		return TableView{}, err
	}
	if err := checkSeats(seats); err != nil {
		return TableView{}, err
	}
	var out TableView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		id, err := q.CreateTable(ctx, repository.CreateTableParams{Label: label, Seats: seats})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "table.created", "table", id, nil, map[string]any{"label": label, "seats": seats}); err != nil {
			return err
		}
		out = TableView{ID: id, Label: label, Seats: seats, Active: true}
		return nil
	})
	if isUnique(err, "dining_tables_label_key") {
		return TableView{}, &ConflictError{Code: "conflict", Field: "label", Msg: "is already used by another table"}
	}
	return out, err
}

type TableChange struct {
	Label  *string
	Seats  *int32
	Active *bool
}

func (s *Service) UpdateTable(ctx context.Context, actor *auth.Principal, id int64, ch TableChange, m Meta) (TableView, error) {
	if err := s.need(actor, "catalogue.manage"); err != nil {
		return TableView{}, err
	}
	var out TableView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := q.GetTable(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "table"}
		}
		if err != nil {
			return err
		}
		label, seats, active := cur.Label, cur.Seats, cur.Active
		if ch.Label != nil {
			if label, err = checkLabel(*ch.Label); err != nil {
				return err
			}
		}
		if ch.Seats != nil {
			if err := checkSeats(*ch.Seats); err != nil {
				return err
			}
			seats = *ch.Seats
		}
		if ch.Active != nil {
			active = *ch.Active
		}
		out = TableView{ID: id, Label: label, Seats: seats, Active: active}
		if label == cur.Label && seats == cur.Seats && active == cur.Active {
			return nil
		}
		if err := q.UpdateTable(ctx, repository.UpdateTableParams{ID: id, Label: label, Seats: seats, Active: active}); err != nil {
			return err
		}
		return s.record(ctx, tx, actor, m, "table.updated", "table", id,
			map[string]any{"label": cur.Label, "seats": cur.Seats, "active": cur.Active}, map[string]any{"label": label, "seats": seats, "active": active})
	})
	if isUnique(err, "dining_tables_label_key") {
		return TableView{}, &ConflictError{Code: "conflict", Field: "label", Msg: "is already used by another table"}
	}
	return out, err
}

// ---- views ----

type ModifierLine struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	PriceDeltaCents int64  `json:"price_delta_cents"`
}

type ItemView struct {
	ID             int64          `json:"id"`
	ProductID      int64          `json:"product_id"`
	Name           string         `json:"name"`
	KitchenRouted  bool           `json:"kitchen_routed"`
	Qty            int32          `json:"qty"`
	UnitPriceCents int64          `json:"unit_price_cents"`
	LineTotalCents int64          `json:"line_total_cents"`
	Status         string         `json:"status"`
	Note           string         `json:"note"`
	CancelReason   string         `json:"cancel_reason,omitempty"`
	SentToKitchen  bool           `json:"sent_to_kitchen"`
	Modifiers      []ModifierLine `json:"modifiers"`
}

type OrderSummary struct {
	ID            int64     `json:"id"`
	OrderNo       int64     `json:"order_no"`
	OrderType     string    `json:"order_type"`
	TableID       *int64    `json:"table_id"`
	TableLabel    string    `json:"table_label"`
	Status        string    `json:"status"`
	OpenedBy      int64     `json:"opened_by"`
	OpenedByName  string    `json:"opened_by_name"`
	Note          string    `json:"note"`
	Version       int32     `json:"version"`
	CancelReason  string    `json:"cancel_reason,omitempty"`
	SubtotalCents int64     `json:"subtotal_cents"`
	ItemCount     int32     `json:"item_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type OrderView struct {
	OrderSummary
	Items []ItemView `json:"items"`
}

func summary(r repository.GetOrderHeadRow) OrderSummary {
	v := OrderSummary{ID: r.ID, OrderNo: r.OrderNo, OrderType: r.OrderType, TableLabel: r.TableLabel, Status: r.Status, OpenedBy: r.OpenedBy,
		OpenedByName: r.OpenedByName, Note: r.Note, Version: r.Version, SubtotalCents: r.SubtotalCents, ItemCount: r.ItemCount,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}
	if r.TableID.Valid {
		id := r.TableID.Int64
		v.TableID = &id
	}
	if r.CancelReason.Valid {
		v.CancelReason = r.CancelReason.String
	}
	return v
}

func (s *Service) load(ctx context.Context, q *repository.Queries, id int64) (OrderView, error) {
	head, err := q.GetOrderHead(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderView{}, &NotFoundError{What: "order"}
	}
	if err != nil {
		return OrderView{}, err
	}
	rows, err := q.ListOrderItems(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	mods, err := q.ListOrderItemModifiers(ctx, ids)
	if err != nil {
		return OrderView{}, err
	}
	byItem := map[int64][]ModifierLine{}
	delta := map[int64]int64{}
	for _, m := range mods {
		byItem[m.OrderItemID] = append(byItem[m.OrderItemID], ModifierLine{ID: m.ModifierID, Name: m.Name, PriceDeltaCents: m.PriceDeltaCents})
		delta[m.OrderItemID] += m.PriceDeltaCents
	}
	items := make([]ItemView, 0, len(rows))
	for _, r := range rows {
		ml := byItem[r.ID]
		if ml == nil {
			ml = []ModifierLine{}
		}
		items = append(items, ItemView{ID: r.ID, ProductID: r.ProductID, Name: r.ProductName, KitchenRouted: r.KitchenRouted, Qty: r.Qty,
			UnitPriceCents: r.UnitPriceCents, LineTotalCents: (r.UnitPriceCents + delta[r.ID]) * int64(r.Qty), Status: r.Status, Note: r.Note,
			CancelReason: r.CancelReason, SentToKitchen: r.SentToKitchen, Modifiers: ml})
	}
	return OrderView{OrderSummary: summary(head), Items: items}, nil
}

// ---- orders ----

// CreateOrder opens an order. A dine-in order needs an active table; a takeaway order has none. The order number
// comes from the gapless counter inside the same transaction. With an idempotency key (optional on this route) a
// retry returns the order that was already opened and uses no further order number.
func (s *Service) CreateOrder(ctx context.Context, actor *auth.Principal, orderType string, tableID *int64, note string, idem *idempotency.Key, m Meta) (OrderView, *idempotency.Stored, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return OrderView{}, nil, err
	}
	if orderType != "dine_in" && orderType != "takeaway" {
		return OrderView{}, nil, &ValidationError{Field: "order_type", Msg: "must be dine_in or takeaway"}
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 200 {
		return OrderView{}, nil, &ValidationError{Field: "note", Msg: "must be 200 characters or fewer"}
	}
	if orderType == "dine_in" && tableID == nil {
		return OrderView{}, nil, &ValidationError{Field: "table_id", Msg: "is required for a dine-in order"}
	}
	if orderType == "takeaway" && tableID != nil {
		return OrderView{}, nil, &ValidationError{Field: "table_id", Msg: "must be empty for a takeaway order"}
	}
	var out OrderView
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
		tid := pgtype.Int8{}
		if tableID != nil {
			t, err := q.GetTable(ctx, *tableID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !t.Active) {
				return &ValidationError{Field: "table_id", Msg: "must be an active table"}
			}
			if err != nil {
				return err
			}
			tid = pgtype.Int8{Int64: t.ID, Valid: true}
		}
		no, err := q.NextCounterValue(ctx, "order")
		if err != nil {
			return err
		}
		id, err := q.CreateOrder(ctx, repository.CreateOrderParams{OrderNo: no, OrderType: orderType, TableID: tid, OpenedBy: actor.UserID, Note: note})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "order.created", "order", id, nil, map[string]any{"order_no": no, "order_type": orderType, "table_id": tableID}); err != nil {
			return err
		}
		if out, err = s.load(ctx, q, id); err != nil {
			return err
		}
		if idem != nil {
			b, err := json.Marshal(struct {
				Order OrderView `json:"order"`
			}{out})
			if err != nil {
				return err
			}
			return idempotency.Complete(ctx, tx, idem, 201, b)
		}
		return nil
	})
	return out, replay, err
}

type OrderFilter struct {
	Status  string
	TableID *int64
	Before  *int64
	Limit   int32
}

func (s *Service) ListOrders(ctx context.Context, actor *auth.Principal, f OrderFilter) ([]OrderSummary, string, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return nil, "", err
	}
	if f.Status != "" && f.Status != "open" && f.Status != "sent" && f.Status != "finalised" && f.Status != "cancelled" {
		return nil, "", &ValidationError{Field: "status", Msg: "must be open, sent, finalised or cancelled"}
	}
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 50
	}
	p := repository.ListOrdersParams{RowLimit: f.Limit + 1}
	if f.Status != "" {
		p.Status = pgtype.Text{String: f.Status, Valid: true}
	}
	if f.TableID != nil {
		p.TableID = pgtype.Int8{Int64: *f.TableID, Valid: true}
	}
	if f.Before != nil {
		p.BeforeID = pgtype.Int8{Int64: *f.Before, Valid: true}
	}
	if !seesAllOrders(actor) {
		p.OpenedBy = pgtype.Int8{Int64: actor.UserID, Valid: true}
	}
	rows, err := repository.New(s.pool).ListOrders(ctx, p)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if int32(len(rows)) > f.Limit {
		rows = rows[:f.Limit]
		next = itoa(rows[len(rows)-1].ID)
	}
	out := make([]OrderSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, summary(repository.GetOrderHeadRow(r)))
	}
	return out, next, nil
}

func (s *Service) GetOrder(ctx context.Context, actor *auth.Principal, id int64) (OrderView, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return OrderView{}, err
	}
	o, err := s.load(ctx, repository.New(s.pool), id)
	if err != nil {
		return OrderView{}, err
	}
	// Someone who may only see their own orders gets "not found" for another person's order, not "forbidden".
	if !seesAllOrders(actor) && o.OpenedBy != actor.UserID {
		return OrderView{}, &NotFoundError{What: "order"}
	}
	return o, nil
}

// editable locks the order and checks the person may see it and that it can still be changed. expected, when given, is the version the person
// was looking at; a different version means someone else changed the order first.
func (s *Service) editable(ctx context.Context, q *repository.Queries, actor *auth.Principal, id int64, expected *int32) error {
	o, err := q.LockOrder(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return &NotFoundError{What: "order"}
	}
	if err != nil {
		return err
	}
	if !seesAllOrders(actor) && o.OpenedBy != actor.UserID {
		return &NotFoundError{What: "order"} // an order this person may not see behaves as if it does not exist
	}
	if o.Status != "open" && o.Status != "sent" {
		return &ConflictError{Code: "order_closed", Msg: "This order is " + o.Status + " and cannot be changed."}
	}
	if expected != nil && *expected != o.Version {
		return &ConflictError{Code: "order_changed", Msg: "Someone else changed this order. Reload it and try again."}
	}
	return nil
}

type NewItem struct {
	ProductID   int64
	Qty         int32
	Note        string
	ModifierIDs []int64
}

func checkNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 200 {
		return "", &ValidationError{Field: "note", Msg: "must be 200 characters or fewer"}
	}
	return note, nil
}

func checkQty(qty int32) error {
	if qty < 1 || qty > 999 {
		return &ValidationError{Field: "qty", Msg: "must be between 1 and 999"}
	}
	return nil
}

// AddItem adds a line. The price is the product's price now, copied onto the line; modifiers must be ones the
// product offers and that are still active.
func (s *Service) AddItem(ctx context.Context, actor *auth.Principal, orderID int64, in NewItem, expected *int32, m Meta) (OrderView, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return OrderView{}, err
	}
	if err := checkQty(in.Qty); err != nil {
		return OrderView{}, err
	}
	note, err := checkNote(in.Note)
	if err != nil {
		return OrderView{}, err
	}
	seen := map[int64]bool{}
	mods := make([]int64, 0, len(in.ModifierIDs))
	for _, id := range in.ModifierIDs {
		if id <= 0 {
			return OrderView{}, &ValidationError{Field: "modifier_ids", Msg: "contains an invalid id"}
		}
		if !seen[id] {
			seen[id] = true
			mods = append(mods, id)
		}
	}
	if len(mods) > 10 {
		return OrderView{}, &ValidationError{Field: "modifier_ids", Msg: "allows at most 10 modifiers on one line"}
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i] < mods[j] })

	var out OrderView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if err := s.editable(ctx, q, actor, orderID, expected); err != nil {
			return err
		}
		p, err := q.GetProductForOrder(ctx, in.ProductID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &ValidationError{Field: "product_id", Msg: "does not exist"}
		}
		if err != nil {
			return err
		}
		if !p.Sellable.Bool {
			return &ConflictError{Code: "product_unavailable", Field: "product_id", Msg: p.Name + " is not available right now."}
		}
		offered, err := q.ListOfferedModifiers(ctx, repository.ListOfferedModifiersParams{ProductID: p.ID, Column2: mods})
		if err != nil {
			return err
		}
		if len(offered) != len(mods) {
			return &ValidationError{Field: "modifier_ids", Msg: "contains a modifier this product does not offer"}
		}
		itemID, err := q.InsertOrderItem(ctx, repository.InsertOrderItemParams{OrderID: orderID, ProductID: p.ID, Qty: in.Qty, UnitPriceCents: p.PriceCents, Note: note})
		if err != nil {
			return err
		}
		for _, mo := range offered {
			if err := q.InsertOrderItemModifier(ctx, repository.InsertOrderItemModifierParams{OrderItemID: itemID, ModifierID: mo.ID, PriceDeltaCents: mo.PriceDeltaCents}); err != nil {
				return err
			}
		}
		if err := q.BumpOrder(ctx, orderID); err != nil {
			return err
		}
		out, err = s.load(ctx, q, orderID)
		return err
	})
	return out, err
}

type ItemChange struct {
	Qty  *int32
	Note *string
}

func (s *Service) UpdateItem(ctx context.Context, actor *auth.Principal, orderID, itemID int64, ch ItemChange, expected *int32, m Meta) (OrderView, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return OrderView{}, err
	}
	var out OrderView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if err := s.editable(ctx, q, actor, orderID, expected); err != nil {
			return err
		}
		cur, err := q.GetOrderItem(ctx, repository.GetOrderItemParams{ID: itemID, OrderID: orderID})
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "order item"}
		}
		if err != nil {
			return err
		}
		if cur.Status != "active" {
			return &ConflictError{Code: "item_cancelled", Msg: "This item was cancelled and cannot be changed."}
		}
		qty, note := cur.Qty, cur.Note
		if ch.Qty != nil {
			if err := checkQty(*ch.Qty); err != nil {
				return err
			}
			qty = *ch.Qty
		}
		if ch.Note != nil {
			if note, err = checkNote(*ch.Note); err != nil {
				return err
			}
		}
		if qty != cur.Qty || note != cur.Note {
			if err := q.UpdateOrderItem(ctx, repository.UpdateOrderItemParams{ID: itemID, Qty: qty, Note: note}); err != nil {
				return err
			}
			if err := q.BumpOrder(ctx, orderID); err != nil {
				return err
			}
			// A line the kitchen already has is changed with an update ticket, not by editing the old one (AC-04).
			st, gerr := q.GetItemForTicket(ctx, repository.GetItemForTicketParams{ID: itemID, OrderID: orderID})
			if gerr != nil {
				return gerr
			}
			if st.Sent {
				if err := s.raiseUpdateTicket(ctx, tx, q, actor, m, orderID, []ticketLine{{OrderItemID: itemID, Name: st.Name, Qty: qty, ChangeType: "modify", Modifiers: st.ModifiersText, Note: note}}); err != nil {
					return err
				}
			}
		}
		out, err = s.load(ctx, q, orderID)
		return err
	})
	return out, err
}

func checkReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if n := len([]rune(reason)); n < 1 || n > 200 {
		return "", &ValidationError{Field: "reason", Msg: "is required (1 to 200 characters)"}
	}
	return reason, nil
}

// CancelItem cancels one line with a reason. A line that was never sent needs no ticket; a line the kitchen already
// has is cancelled with a linked update ticket.
func (s *Service) CancelItem(ctx context.Context, actor *auth.Principal, orderID, itemID int64, reason string, expected *int32, m Meta) (OrderView, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return OrderView{}, err
	}
	reason, err := checkReason(reason)
	if err != nil {
		return OrderView{}, err
	}
	var out OrderView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if err := s.editable(ctx, q, actor, orderID, expected); err != nil {
			return err
		}
		cur, err := q.GetOrderItem(ctx, repository.GetOrderItemParams{ID: itemID, OrderID: orderID})
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "order item"}
		}
		if err != nil {
			return err
		}
		if cur.Status != "active" {
			return &ConflictError{Code: "item_cancelled", Msg: "This item was already cancelled."}
		}
		st, err := q.GetItemForTicket(ctx, repository.GetItemForTicketParams{ID: itemID, OrderID: orderID})
		if err != nil {
			return err
		}
		if err := q.CancelOrderItem(ctx, repository.CancelOrderItemParams{ID: itemID, CancelReason: pgtype.Text{String: reason, Valid: true}, CancelledBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := q.BumpOrder(ctx, orderID); err != nil {
			return err
		}
		// A line the kitchen already has is cancelled with an update ticket so the kitchen is told (AC-04).
		if st.Sent {
			if err := s.raiseUpdateTicket(ctx, tx, q, actor, m, orderID, []ticketLine{{OrderItemID: itemID, Name: st.Name, Qty: cur.Qty, ChangeType: "cancel", Modifiers: st.ModifiersText, Note: cur.Note}}); err != nil {
				return err
			}
		}
		if err := s.record(ctx, tx, actor, m, "order.item_cancelled", "order", orderID, map[string]any{"item_id": itemID, "qty": cur.Qty}, map[string]any{"item_id": itemID, "reason": reason}); err != nil {
			return err
		}
		out, err = s.load(ctx, q, orderID)
		return err
	})
	return out, err
}

// CancelOrder cancels an open or sent order. A sent order raises a cancel update ticket for the lines the kitchen
// still holds, and the tickets that were still to do are marked cancelled, all in the same transaction.
func (s *Service) CancelOrder(ctx context.Context, actor *auth.Principal, id int64, reason string, expected *int32, m Meta) (OrderView, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return OrderView{}, err
	}
	reason, err := checkReason(reason)
	if err != nil {
		return OrderView{}, err
	}
	var out OrderView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if err := s.editable(ctx, q, actor, id, expected); err != nil {
			return err
		}
		cur, err := q.LockOrder(ctx, id)
		if err != nil {
			return err
		}
		if cur.Status == "sent" {
			// Close the tickets still to do first, then tell the kitchen to drop what it still holds with a new cancel
			// ticket. The order matters: the new ticket must stay submitted so the kitchen sees it.
			live, err := q.ListLiveSentItems(ctx, id)
			if err != nil {
				return err
			}
			if err := s.cancelOpenTickets(ctx, tx, q, actor, m, id); err != nil {
				return err
			}
			if len(live) > 0 {
				lines := make([]ticketLine, 0, len(live))
				for _, it := range live {
					lines = append(lines, ticketLine{OrderItemID: it.ID, Name: it.Name, Qty: it.Qty, ChangeType: "cancel", Modifiers: it.ModifiersText, Note: it.Note})
				}
				if err := s.raiseUpdateTicket(ctx, tx, q, actor, m, id, lines); err != nil {
					return err
				}
			}
		}
		if err := q.CancelOrder(ctx, repository.CancelOrderParams{ID: id, CancelReason: pgtype.Text{String: reason, Valid: true}, CancelledBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "order.cancelled", "order", id, map[string]any{"status": cur.Status}, map[string]any{"status": "cancelled", "reason": reason}); err != nil {
			return err
		}
		out, err = s.load(ctx, q, id)
		return err
	})
	return out, err
}
