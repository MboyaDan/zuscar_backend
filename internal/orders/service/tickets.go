package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/orders/repository"
	"zuscar/backend/internal/platform/auth"
)

// Kitchen tickets (Stage 10b-1, docs/behaviour.md sections 1 and 2). A ticket is written in the same transaction as
// the order change that raises it, so it exists, with its number, before anything can be printed (AC-04, NN-05).
// Ticket status never reads or writes payment status (AC-04). The first ticket of an order is "original"; every later
// one is an "update" that points at the original and carries added, cancelled or modified lines. A ticket is never
// edited after it is raised, only its status moves forward.

// ticketLine is one line to put on a ticket. Prices are never copied to a ticket.
type ticketLine struct {
	OrderItemID int64
	Name        string
	Qty         int32
	ChangeType  string // add, cancel, modify
	Modifiers   string
	Note        string
}

// raiseTicket numbers and stores a ticket with its lines and its first history row. parent is zero for an original ticket.
func (s *Service) raiseTicket(ctx context.Context, q *repository.Queries, actor *auth.Principal, orderID, parent int64, lines []ticketLine) (int64, int64, error) {
	bd, err := q.BusinessDate(ctx)
	if err != nil {
		return 0, 0, err
	}
	scope := bd.Time.Format("2006-01-02")
	if err := q.OpenTicketCounter(ctx, scope); err != nil {
		return 0, 0, err
	}
	no, err := q.NextTicketNo(ctx, scope)
	if err != nil {
		return 0, 0, err
	}
	kind := "original"
	p := pgtype.Int8{}
	if parent != 0 {
		kind = "update"
		p = pgtype.Int8{Int64: parent, Valid: true}
	}
	id, err := q.InsertTicket(ctx, repository.InsertTicketParams{OrderID: orderID, BusinessDate: bd, TicketNo: no, Kind: kind, ParentTicketID: p, CreatedBy: actor.UserID})
	if err != nil {
		return 0, 0, err
	}
	if err := q.InsertTicketHistory(ctx, repository.InsertTicketHistoryParams{TicketID: id, ToStatus: "submitted", ChangedBy: actor.UserID}); err != nil {
		return 0, 0, err
	}
	for _, l := range lines {
		if err := q.InsertTicketItem(ctx, repository.InsertTicketItemParams{TicketID: id, OrderItemID: l.OrderItemID, NameSnapshot: l.Name, Qty: l.Qty,
			ChangeType: l.ChangeType, ModifiersText: l.Modifiers, Note: l.Note}); err != nil {
			return 0, 0, err
		}
	}
	return id, no, nil
}

// raiseUpdateTicket tells the kitchen about a change to an order that was already sent.
func (s *Service) raiseUpdateTicket(ctx context.Context, tx pgx.Tx, q *repository.Queries, actor *auth.Principal, m Meta, orderID int64, lines []ticketLine) error {
	parent, err := q.GetOriginalTicketID(ctx, orderID)
	if err != nil {
		return err
	}
	id, no, err := s.raiseTicket(ctx, q, actor, orderID, parent, lines)
	if err != nil {
		return err
	}
	return s.record(ctx, tx, actor, m, "kitchen.ticket_raised", "kitchen_ticket", id, nil,
		map[string]any{"order_id": orderID, "ticket_no": no, "kind": "update", "lines": len(lines)})
}

// SendResult is the order after sending and the ticket that was raised.
type SendResult struct {
	Order  OrderView  `json:"order"`
	Ticket TicketView `json:"ticket"`
}

// SendToKitchen puts every active kitchen-routed line that has not gone yet on a new ticket. The first send makes
// the original ticket and moves the order to sent; later sends make update tickets for lines added since. When
// nothing is new it answers 409 nothing_to_send and consumes no ticket number, so a double tap cannot print twice.
func (s *Service) SendToKitchen(ctx context.Context, actor *auth.Principal, orderID int64, expected *int32, m Meta) (SendResult, error) {
	if err := s.need(actor, "orders.manage"); err != nil {
		return SendResult{}, err
	}
	var out SendResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if err := s.editable(ctx, q, actor, orderID, expected); err != nil {
			return err
		}
		o, err := q.LockOrder(ctx, orderID)
		if err != nil {
			return err
		}
		items, err := q.ListUnsentKitchenItems(ctx, orderID)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return &ConflictError{Code: "nothing_to_send", Msg: "There is nothing new to send to the kitchen."}
		}
		lines := make([]ticketLine, 0, len(items))
		for _, it := range items {
			lines = append(lines, ticketLine{OrderItemID: it.ID, Name: it.Name, Qty: it.Qty, ChangeType: "add", Modifiers: it.ModifiersText, Note: it.Note})
		}
		var parent int64
		kind := "original"
		if o.Status == "sent" {
			if parent, err = q.GetOriginalTicketID(ctx, orderID); err != nil {
				return err
			}
			kind = "update"
		}
		id, no, err := s.raiseTicket(ctx, q, actor, orderID, parent, lines)
		if err != nil {
			return err
		}
		if o.Status == "open" {
			if err := q.MarkOrderSent(ctx, orderID); err != nil {
				return err
			}
		} else if err := q.BumpOrder(ctx, orderID); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "order.sent_to_kitchen", "order", orderID, map[string]any{"status": o.Status},
			map[string]any{"status": "sent", "ticket_id": id, "ticket_no": no, "kind": kind, "lines": len(lines)}); err != nil {
			return err
		}
		if out.Order, err = s.load(ctx, q, orderID); err != nil {
			return err
		}
		out.Ticket, err = s.loadTicket(ctx, q, id, false)
		return err
	})
	return out, err
}

// ---- reading and moving tickets ----

type TicketLineView struct {
	OrderItemID int64  `json:"order_item_id"`
	Name        string `json:"name"`
	Qty         int32  `json:"qty"`
	ChangeType  string `json:"change_type"`
	Modifiers   string `json:"modifiers"`
	Note        string `json:"note"`
}

type TicketChange struct {
	From      string    `json:"from_status"`
	To        string    `json:"to_status"`
	ChangedBy string    `json:"changed_by"`
	At        time.Time `json:"at"`
}

type TicketView struct {
	ID             int64            `json:"id"`
	OrderID        int64            `json:"order_id"`
	OrderNo        int64            `json:"order_no"`
	OrderType      string           `json:"order_type"`
	TableLabel     string           `json:"table_label"`
	BusinessDate   string           `json:"business_date"`
	TicketNo       int64            `json:"ticket_no"`
	Kind           string           `json:"kind"`
	ParentTicketID *int64           `json:"parent_ticket_id"`
	Status         string           `json:"status"`
	CreatedByName  string           `json:"created_by_name"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Lines          []TicketLineView `json:"lines"`
	History        []TicketChange   `json:"history,omitempty"`
}

func ticketHead(r repository.GetTicketHeadRow) TicketView {
	v := TicketView{ID: r.ID, OrderID: r.OrderID, OrderNo: r.OrderNo, OrderType: r.OrderType, TableLabel: r.TableLabel,
		BusinessDate: r.BusinessDate.Time.Format("2006-01-02"), TicketNo: r.TicketNo, Kind: r.Kind, Status: r.Status,
		CreatedByName: r.CreatedByName, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time, Lines: []TicketLineView{}}
	if r.ParentTicketID.Valid {
		id := r.ParentTicketID.Int64
		v.ParentTicketID = &id
	}
	return v
}

func (s *Service) fillLines(ctx context.Context, q *repository.Queries, tickets []TicketView) error {
	if len(tickets) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(tickets))
	at := map[int64]int{}
	for i, t := range tickets {
		ids = append(ids, t.ID)
		at[t.ID] = i
	}
	rows, err := q.ListTicketItems(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range rows {
		i := at[r.TicketID]
		tickets[i].Lines = append(tickets[i].Lines, TicketLineView{OrderItemID: r.OrderItemID, Name: r.NameSnapshot, Qty: r.Qty,
			ChangeType: r.ChangeType, Modifiers: r.ModifiersText, Note: r.Note})
	}
	return nil
}

func (s *Service) loadTicket(ctx context.Context, q *repository.Queries, id int64, withHistory bool) (TicketView, error) {
	head, err := q.GetTicketHead(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketView{}, &NotFoundError{What: "kitchen ticket"}
	}
	if err != nil {
		return TicketView{}, err
	}
	one := []TicketView{ticketHead(head)}
	if err := s.fillLines(ctx, q, one); err != nil {
		return TicketView{}, err
	}
	if withHistory {
		hist, err := q.ListTicketHistory(ctx, id)
		if err != nil {
			return TicketView{}, err
		}
		for _, h := range hist {
			one[0].History = append(one[0].History, TicketChange{From: h.FromStatus, To: h.ToStatus, ChangedBy: h.ChangedByName, At: h.ChangedAt.Time})
		}
	}
	return one[0], nil
}

// GetTicket returns one ticket with its lines and status history. Anyone with kitchen.view may read any ticket (S-44).
func (s *Service) GetTicket(ctx context.Context, actor *auth.Principal, id int64) (TicketView, error) {
	if err := s.need(actor, "kitchen.view"); err != nil {
		return TicketView{}, err
	}
	return s.loadTicket(ctx, repository.New(s.pool), id, true)
}

type TicketFilter struct {
	Status  string // "" means the tickets still to do (submitted, preparing, ready); "all" means every status
	OrderID *int64
	After   *int64
	Limit   int32
}

// ListTickets lists tickets oldest first, the order the kitchen works in.
func (s *Service) ListTickets(ctx context.Context, actor *auth.Principal, f TicketFilter) ([]TicketView, string, error) {
	if err := s.need(actor, "kitchen.view"); err != nil {
		return nil, "", err
	}
	var statuses []string
	switch f.Status {
	case "":
		statuses = []string{"submitted", "preparing", "ready"}
	case "all":
		statuses = []string{"submitted", "preparing", "ready", "served", "cancelled"}
	case "submitted", "preparing", "ready", "served", "cancelled":
		statuses = []string{f.Status}
	default:
		return nil, "", &ValidationError{Field: "status", Msg: "must be submitted, preparing, ready, served, cancelled or all"}
	}
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 100
	}
	p := repository.ListTicketsParams{Statuses: statuses, RowLimit: f.Limit + 1}
	if f.OrderID != nil {
		p.OrderID = pgtype.Int8{Int64: *f.OrderID, Valid: true}
	}
	if f.After != nil {
		p.AfterID = pgtype.Int8{Int64: *f.After, Valid: true}
	}
	q := repository.New(s.pool)
	rows, err := q.ListTickets(ctx, p)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if int32(len(rows)) > f.Limit {
		rows = rows[:f.Limit]
		next = itoa(rows[len(rows)-1].ID)
	}
	out := make([]TicketView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ticketHead(repository.GetTicketHeadRow(r)))
	}
	if err := s.fillLines(ctx, q, out); err != nil {
		return nil, "", err
	}
	return out, next, nil
}

// SetTicketStatus moves a ticket forward: submitted, preparing, ready, served (skipping forward is allowed, S-17).
// Cancelled is only ever set by cancelling the order, never directly. Payment is not consulted (AC-04).
func (s *Service) SetTicketStatus(ctx context.Context, actor *auth.Principal, id int64, to string, m Meta) (TicketView, error) {
	if err := s.need(actor, "kitchen.update"); err != nil {
		return TicketView{}, err
	}
	switch to {
	case "preparing", "ready", "served":
	case "submitted", "cancelled":
		return TicketView{}, &ValidationError{Field: "status", Msg: "can only be preparing, ready or served; a ticket is cancelled by cancelling the order"}
	default:
		return TicketView{}, &ValidationError{Field: "status", Msg: "must be preparing, ready or served"}
	}
	var out TicketView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := q.LockTicket(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "kitchen ticket"}
		}
		if err != nil {
			return err
		}
		if !ticketMayMove(cur.Status, to) {
			return &ConflictError{Code: "invalid_transition", Msg: "A ticket that is " + cur.Status + " cannot become " + to + "."}
		}
		if err := q.SetTicketStatus(ctx, repository.SetTicketStatusParams{ID: id, Status: to}); err != nil {
			return err
		}
		if err := q.InsertTicketHistory(ctx, repository.InsertTicketHistoryParams{TicketID: id, FromStatus: pgtype.Text{String: cur.Status, Valid: true}, ToStatus: to, ChangedBy: actor.UserID}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "kitchen.ticket_status", "kitchen_ticket", id, map[string]any{"status": cur.Status}, map[string]any{"status": to}); err != nil {
			return err
		}
		out, err = s.loadTicket(ctx, q, id, true)
		return err
	})
	return out, err
}

// ticketMayMove is the forward-only rule; the database trigger enforces the same table.
func ticketMayMove(from, to string) bool {
	rank := map[string]int{"submitted": 0, "preparing": 1, "ready": 2, "served": 3}
	f, ok1 := rank[from]
	t, ok2 := rank[to]
	return ok1 && ok2 && t > f
}

// cancelOpenTickets marks every ticket of the order that is still to do as cancelled, with history, because the
// order was cancelled. Call it before raising the cancel update ticket, which must stay submitted.
func (s *Service) cancelOpenTickets(ctx context.Context, tx pgx.Tx, q *repository.Queries, actor *auth.Principal, m Meta, orderID int64) error {
	open, err := q.LockOpenTicketsForOrder(ctx, orderID)
	if err != nil {
		return err
	}
	for _, t := range open {
		if err := q.SetTicketStatus(ctx, repository.SetTicketStatusParams{ID: t.ID, Status: "cancelled"}); err != nil {
			return err
		}
		if err := q.InsertTicketHistory(ctx, repository.InsertTicketHistoryParams{TicketID: t.ID, FromStatus: pgtype.Text{String: t.Status, Valid: true}, ToStatus: "cancelled", ChangedBy: actor.UserID}); err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "kitchen.ticket_status", "kitchen_ticket", t.ID, map[string]any{"status": t.Status}, map[string]any{"status": "cancelled", "order_id": orderID}); err != nil {
			return err
		}
	}
	return nil
}
