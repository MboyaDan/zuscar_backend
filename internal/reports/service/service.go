// Package service builds the management reports (Stage 25). It only reads, through its own queries (docs/architecture.md
// section 3, rule 5), and never changes a number: every figure comes straight from the records the other modules keep.
package service

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/reports/repository"
)

var ErrForbidden = errorString("forbidden")

type errorString string

func (e errorString) Error() string { return string(e) }

type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

type Meta struct {
	RequestID string
	IP        *netip.Addr
}

type Settings interface {
	GetSettings(ctx context.Context, actor *auth.Principal) (map[string]json.RawMessage, error)
}

type Service struct {
	pool     *pgxpool.Pool
	rec      audit.Recorder
	settings Settings
	now      func() time.Time
}

func New(pool *pgxpool.Pool, rec audit.Recorder, settings Settings) *Service {
	return &Service{pool: pool, rec: rec, settings: settings, now: time.Now}
}

func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// ---- the shape every report has ----

// A Column says how its cells are shown: text, date (YYYY-MM-DD), int, money (cents) or bp (basis points: 6250 is 62.50%).
type Column struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

type Section struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Totals  []any    `json:"totals,omitempty"` // same length as Columns; nil cells are blank
}

type Report struct {
	Name     string    `json:"name"`
	Title    string    `json:"title"`
	From     string    `json:"from,omitempty"`
	To       string    `json:"to,omitempty"`
	Timezone string    `json:"timezone"`
	Notes    []string  `json:"notes"`
	Sections []Section `json:"sections"`
}

// Names are the reports that exist; a name outside this list is a 404.
var Names = []string{"sales", "collections", "balances-due", "discounts-voids-refunds", "stock", "rooms", "expenses-profit", "day-close"}

const maxDays = 366

func col(key, label, kind string) Column { return Column{Key: key, Label: label, Kind: kind} }

// ---- who may read ----

func hasRole(a *auth.Principal, codes ...string) bool {
	for _, r := range a.Roles {
		for _, c := range codes {
			if r == c {
				return true
			}
		}
	}
	return false
}

// A cashier holds reports.view for their own shift summary only (docs/rbac.md note 1); sales and collections show them only
// what they rang up or took, and the other reports are not theirs to read.
func ownOnly(a *auth.Principal) bool {
	return hasRole(a, "cashier") && !hasRole(a, "super_admin", "business_manager", "accountant_auditor")
}

func (s *Service) zone(ctx context.Context, actor *auth.Principal) (string, *time.Location) {
	zone := "Africa/Nairobi"
	if raw, err := s.settings.GetSettings(ctx, actor); err == nil {
		var z string
		if v, ok := raw["timezone"]; ok && json.Unmarshal(v, &z) == nil && z != "" {
			zone = z
		}
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		zone, loc = "Africa/Nairobi", time.FixedZone("EAT", 3*3600)
	}
	return zone, loc
}

func parseDay(field, v string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, &ValidationError{Field: field, Msg: "must be a date like 2026-10-20"}
	}
	return t, nil
}

// Range reads from and to (business dates, both included). Without them the report is for today.
func (s *Service) rangeOf(loc *time.Location, from, to string) (time.Time, time.Time, error) {
	n := s.now().In(loc)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	f, t := today, today
	var err error
	if from != "" {
		if f, err = parseDay("from", from); err != nil {
			return f, t, err
		}
		t = f
	}
	if to != "" {
		if t, err = parseDay("to", to); err != nil {
			return f, t, err
		}
		if from == "" {
			f = t
		}
	}
	if t.Before(f) {
		return f, t, &ValidationError{Field: "to", Msg: "must not be before from"}
	}
	if int(t.Sub(f).Hours()/24)+1 > maxDays {
		return f, t, &ValidationError{Field: "to", Msg: "the range can be at most 366 days"}
	}
	return f, t, nil
}

func day(t time.Time) string { return t.Format("2006-01-02") }

// Build makes one report. from and to are business dates and only matter to the reports that are about a period.
func (s *Service) Build(ctx context.Context, actor *auth.Principal, name, from, to string) (Report, error) {
	if actor == nil || !actor.Has("reports.view") {
		return Report{}, ErrForbidden
	}
	known := false
	for _, n := range Names {
		known = known || n == name
	}
	if !known {
		return Report{}, &NotFoundError{What: "report"}
	}
	if ownOnly(actor) && name != "sales" && name != "collections" {
		return Report{}, ErrForbidden
	}
	zone, loc := s.zone(ctx, actor)
	f, t, err := s.rangeOf(loc, from, to)
	if err != nil {
		return Report{}, err
	}
	q := repository.New(s.pool)
	var own int64
	if ownOnly(actor) {
		own = actor.UserID
	}
	var r Report
	switch name {
	case "sales":
		r, err = s.sales(ctx, q, f, t, own)
	case "collections":
		r, err = s.collections(ctx, q, f, t, zone, own)
	case "balances-due":
		r, err = s.balancesDue(ctx, q)
	case "discounts-voids-refunds":
		r, err = s.dvr(ctx, q, f, t)
	case "stock":
		r, err = s.stock(ctx, q)
	case "rooms":
		r, err = s.rooms(ctx, q, f, t, loc)
	case "expenses-profit":
		r, err = s.expensesProfit(ctx, q, f, t, zone)
	case "day-close":
		r, err = s.dayClose(ctx, q, f, t, zone, loc)
	}
	if err != nil {
		return Report{}, err
	}
	r.Name, r.Timezone = name, zone
	if own != 0 {
		r.Notes = append(r.Notes, "Showing only the sales and payments you made.")
	}
	return r, nil
}

type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return e.What + " not found" }

// ---- sales ----

func (s *Service) sales(ctx context.Context, q *repository.Queries, f, t time.Time, own int64) (Report, error) {
	days, err := q.SalesByDay(ctx, repository.SalesByDayParams{FromDay: dt(f), ToDay: dt(t), CashierID: own})
	if err != nil {
		return Report{}, err
	}
	rets, err := q.ReturnsByDay(ctx, repository.ReturnsByDayParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	back := map[string]repository.ReturnsByDayRow{}
	for _, r := range rets {
		back[day(r.BusinessDate.Time)] = r
	}
	byDay := Section{Key: "days", Title: "Sales by day", Columns: []Column{col("date", "Day", "date"), col("sales", "Sales", "int"), col("gross", "Before discounts", "money"), col("discounts", "Discounts", "money"),
		col("total", "Sales total", "money"), col("tax", "Tax included", "money"), col("room", "Charged to rooms", "money"), col("returned", "Refunds and voids", "money"), col("net", "Net sales", "money")}}
	var tot struct{ n, gross, disc, total, tax, room, ret int64 }
	seen := map[string]bool{}
	add := func(d string, n, gross, disc, total, tax, room, ret int64) {
		byDay.Rows = append(byDay.Rows, []any{d, n, gross, disc, total, tax, room, ret, total - ret})
		tot.n, tot.gross, tot.disc, tot.total, tot.tax, tot.room, tot.ret = tot.n+n, tot.gross+gross, tot.disc+disc, tot.total+total, tot.tax+tax, tot.room+room, tot.ret+ret
	}
	for _, d := range days {
		k := day(d.BusinessDate.Time)
		seen[k] = true
		add(k, d.SalesCount, d.GrossCents, d.DiscountCents, d.TotalCents, d.TaxCents, d.RoomChargedCents, back[k].AmountCents)
	}
	if own == 0 {
		for _, r := range rets { // a day with only refunds still shows
			if k := day(r.BusinessDate.Time); !seen[k] {
				add(k, 0, 0, 0, 0, 0, 0, r.AmountCents)
			}
		}
		sortRowsByFirst(byDay.Rows)
	}
	byDay.Totals = []any{"Total", tot.n, tot.gross, tot.disc, tot.total, tot.tax, tot.room, tot.ret, tot.total - tot.ret}

	mods, err := q.SalesByModule(ctx, repository.SalesByModuleParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	byMod := Section{Key: "modules", Title: "By restaurant and bar", Columns: []Column{col("module", "Part of the business", "text"), col("items", "Items sold", "int"), col("total", "Sales total", "money"), col("tax", "Tax included", "money")}}
	for _, m := range mods {
		byMod.Rows = append(byMod.Rows, []any{moduleLabel(m.Module), m.ItemsSold, m.TotalCents, m.TaxCents})
	}
	pay, err := q.SalesByPaymentStatus(ctx, repository.SalesByPaymentStatusParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	byPay := Section{Key: "payment_status", Title: "By how they were settled", Columns: []Column{col("status", "Status", "text"), col("sales", "Sales", "int"), col("total", "Sales total", "money")}}
	for _, p := range pay {
		byPay.Rows = append(byPay.Rows, []any{payLabel(p.PaymentStatus), p.SalesCount, p.TotalCents})
	}
	top, err := q.TopProducts(ctx, repository.TopProductsParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	best := Section{Key: "top_products", Title: "Best sellers", Columns: []Column{col("name", "Item", "text"), col("module", "Part of the business", "text"), col("qty", "Sold", "int"), col("total", "Sales total", "money")}}
	for _, p := range top {
		best.Rows = append(best.Rows, []any{p.Name, moduleLabel(p.Module), p.Qty, p.TotalCents})
	}
	r := Report{Title: "Sales", From: day(f), To: day(t), Sections: []Section{byDay}}
	if own == 0 {
		r.Sections = append(r.Sections, byMod, byPay, best)
	} else {
		r.Sections = append(r.Sections, byPay)
	}
	r.Notes = []string{"Sales are counted on the day they were made. Refunds and voids are counted on the day they were done, so a correction never changes an earlier day (S-107).",
		"Prices include tax. Sales charged to rooms are counted as sales when the order is charged, and collected later on the folio."}
	return r, nil
}

func moduleLabel(m string) string {
	if m == "bar" {
		return "Bar"
	}
	return "Restaurant"
}

func payLabel(p string) string {
	switch p {
	case "paid":
		return "Paid"
	case "partially_paid":
		return "Part paid"
	case "charged_to_folio":
		return "Charged to a room"
	}
	return "Unpaid"
}

func methodLabel(m string) string {
	switch m {
	case "cash":
		return "Cash"
	case "mpesa_manual":
		return "M-Pesa"
	case "card_manual":
		return "Card"
	case "bank_manual":
		return "Bank transfer"
	}
	return m
}

func purposeLabel(p string) string {
	switch p {
	case "deposit":
		return "Room deposit"
	case "folio_settlement":
		return "Room bill"
	}
	return "Sale"
}

// ---- collections ----

func (s *Service) collections(ctx context.Context, q *repository.Queries, f, t time.Time, zone string, own int64) (Report, error) {
	rows, err := q.CollectionsByMethod(ctx, repository.CollectionsByMethodParams{Tz: zone, FromDay: dt(f), ToDay: dt(t), CashierID: own})
	if err != nil {
		return Report{}, err
	}
	main := Section{Key: "collected", Title: "Money received (confirmed)", Columns: []Column{col("date", "Day", "date"), col("method", "How", "text"), col("purpose", "For", "text"), col("payments", "Payments", "int"), col("amount", "Amount", "money")}}
	var n, sum int64
	byMethod := map[string]int64{}
	for _, r := range rows {
		main.Rows = append(main.Rows, []any{day(r.Day.Time), methodLabel(r.Method), purposeLabel(r.Purpose), r.Payments, r.AmountCents})
		n, sum = n+r.Payments, sum+r.AmountCents
		byMethod[r.Method] += r.AmountCents
	}
	main.Totals = []any{"Total", nil, nil, n, sum}
	r := Report{Title: "Collections", From: day(f), To: day(t), Sections: []Section{main}}
	if own != 0 {
		r.Notes = []string{"Only confirmed payments count. Cash is confirmed when taken; M-Pesa, card and bank payments count once someone confirms them against the statement (AC-20)."}
		return r, nil
	}
	pay, err := q.PayoutsByMethod(ctx, repository.PayoutsByMethodParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	out := Section{Key: "paid_out", Title: "Money paid back (refunds and voids)", Columns: []Column{col("date", "Day", "date"), col("method", "How", "text"), col("payouts", "Payouts", "int"), col("amount", "Amount", "money")}}
	var pn, ps int64
	for _, p := range pay {
		out.Rows = append(out.Rows, []any{day(p.Day.Time), methodLabel(p.Method), p.Payouts, p.AmountCents})
		pn, ps = pn+p.Payouts, ps+p.AmountCents
		byMethod[p.Method] -= p.AmountCents
	}
	out.Totals = []any{"Total", nil, pn, ps}
	net := Section{Key: "net_by_method", Title: "Net received by method", Columns: []Column{col("method", "How", "text"), col("amount", "Received minus paid back", "money")}}
	var nt int64
	for _, m := range []string{"cash", "mpesa_manual", "card_manual", "bank_manual"} {
		if v, ok := byMethod[m]; ok {
			net.Rows = append(net.Rows, []any{methodLabel(m), v})
			nt += v
		}
	}
	net.Totals = []any{"Total", nt}
	wait, err := q.PendingPayments(ctx)
	if err != nil {
		return Report{}, err
	}
	pend := Section{Key: "pending", Title: "Waiting for confirmation (right now)", Columns: []Column{col("method", "How", "text"), col("payments", "Payments", "int"), col("amount", "Amount", "money")}}
	for _, w := range wait {
		pend.Rows = append(pend.Rows, []any{methodLabel(w.Method), w.Payments, w.AmountCents})
	}
	r.Sections = append(r.Sections, out, net, pend)
	r.Notes = []string{"Only confirmed payments count, on the day they were confirmed (AC-20). Payments still waiting for confirmation are listed apart and are not in any total above.",
		"Room charges are not payments: the money shows here when the guest pays the folio (NN-10)."}
	return r, nil
}

// ---- balances due ----

func (s *Service) balancesDue(ctx context.Context, q *repository.Queries) (Report, error) {
	sales, err := q.SalesDue(ctx)
	if err != nil {
		return Report{}, err
	}
	a := Section{Key: "sales", Title: "Sales not fully paid", Columns: []Column{col("receipt", "Receipt", "int"), col("date", "Day", "date"), col("cashier", "Rung up by", "text"), col("status", "Status", "text"),
		col("total", "Total", "money"), col("paid", "Paid", "money"), col("refunded", "Refunded", "money"), col("owed", "Still owed", "money")}}
	var owedSales int64
	for _, r := range sales {
		owed := r.TotalCents - r.PaidCents - r.RefundedCents
		if owed <= 0 {
			continue
		}
		a.Rows = append(a.Rows, []any{r.ReceiptNo, day(r.BusinessDate.Time), r.CashierName, payLabel(r.PaymentStatus), r.TotalCents, r.PaidCents, r.RefundedCents, owed})
		owedSales += owed
	}
	a.Totals = []any{"Total", nil, nil, nil, nil, nil, nil, owedSales}
	fol, err := q.FoliosDue(ctx)
	if err != nil {
		return Report{}, err
	}
	b := Section{Key: "folios", Title: "Guest bills (folios) not settled", Columns: []Column{col("folio", "Folio", "int"), col("guest", "Guest", "text"), col("room", "Room", "text"), col("stay", "Guest is", "text"),
		col("items", "Charges", "money"), col("paid", "Paid", "money"), col("pending", "Waiting for confirmation", "money"), col("written_off", "Written off", "money"), col("owed", "Still owed", "money")}}
	var owedFolios int64
	for _, r := range fol {
		owed := r.ItemsCents - r.PaidCents - r.WriteOffCents
		if owed <= 0 {
			continue
		}
		stay := "Gone"
		if r.StayStatus == "in_house" {
			stay = "In the house"
		}
		b.Rows = append(b.Rows, []any{r.FolioNo, r.GuestName, r.RoomNumber, stay, r.ItemsCents, r.PaidCents, r.PendingCents, r.WriteOffCents, owed})
		owedFolios += owed
	}
	b.Totals = []any{"Total", nil, nil, nil, nil, nil, nil, nil, owedFolios}
	return Report{Title: "Balances due", Sections: []Section{a, b},
		Notes: []string{"This is who owes money right now, not a period. A payment waiting for confirmation is not counted as paid until it is confirmed.",
			"Money owed on a sale is what is left after confirmed payments and refunds."}}, nil
}

// ---- discounts, voids and refunds ----

func (s *Service) dvr(ctx context.Context, q *repository.Queries, f, t time.Time) (Report, error) {
	disc, err := q.DiscountLines(ctx, repository.DiscountLinesParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	a := Section{Key: "discounts", Title: "Discounts given", Columns: []Column{col("date", "Day", "date"), col("receipt", "Receipt", "int"), col("item", "Item", "text"), col("qty", "Qty", "int"),
		col("amount", "Discount", "money"), col("reason", "Reason", "text"), col("approved", "Approved by", "text"), col("cashier", "Rung up by", "text")}}
	var ds int64
	for _, d := range disc {
		a.Rows = append(a.Rows, []any{day(d.BusinessDate.Time), d.ReceiptNo, d.Item, int64(d.Qty), d.DiscountCents, d.Reason, d.ApprovedBy, d.CashierName})
		ds += d.DiscountCents
	}
	a.Totals = []any{"Total", nil, nil, nil, ds, nil, nil, nil}
	ref, err := q.RefundRows(ctx, repository.RefundRowsParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	b := Section{Key: "returns", Title: "Refunds and voids", Columns: []Column{col("date", "Day", "date"), col("no", "Refund no.", "int"), col("kind", "Kind", "text"), col("receipt", "Sale receipt", "int"),
		col("amount", "Amount", "money"), col("reason", "Reason", "text"), col("approved", "Approved by", "text"), col("by", "Done by", "text"), col("paid_out", "Paid back by", "text")}}
	var rs int64
	for _, r := range ref {
		kind := "Refund"
		if r.Kind == "void" {
			kind = "Void"
		}
		b.Rows = append(b.Rows, []any{day(r.BusinessDate.Time), r.RefundNo, kind, r.ReceiptNo, r.AmountCents, r.Reason, r.ApprovedBy, r.CreatedBy, r.PaidOutBy})
		rs += r.AmountCents
	}
	b.Totals = []any{"Total", nil, nil, nil, rs, nil, nil, nil, nil}
	return Report{Title: "Discounts, voids and refunds", From: day(f), To: day(t), Sections: []Section{a, b},
		Notes: []string{"Every row names the person who approved it and why. Lists stop at 1,000 rows; narrow the dates to see the rest."}}, nil
}

// ---- stock ----

func (s *Service) stock(ctx context.Context, q *repository.Queries) (Report, error) {
	rows, err := q.StockOnHand(ctx)
	if err != nil {
		return Report{}, err
	}
	a := Section{Key: "on_hand", Title: "Stock on hand, by place", Columns: []Column{col("item", "Item", "text"), col("place", "Place", "text"), col("qty", "On hand", "int"), col("unit", "Unit", "text"), col("low", "Low-stock level", "int"), col("flag", "Flag", "text")}}
	low := Section{Key: "low", Title: "Running low (all places together)", Columns: []Column{col("item", "Item", "text"), col("total", "On hand in total", "int"), col("unit", "Unit", "text"), col("level", "Low-stock level", "int")}}
	doneLow := map[int64]bool{}
	for _, r := range rows {
		flag := ""
		var lv any
		if r.LowThreshold >= 0 {
			lv = r.LowThreshold
			if r.ItemTotal <= r.LowThreshold {
				flag = "Low"
			}
		}
		if r.QtyBase < 0 {
			flag = strings.TrimSpace(flag + " Below zero")
		}
		a.Rows = append(a.Rows, []any{r.ItemName, r.LocationName, r.QtyBase, r.UnitCode, lv, flag})
		if r.LowThreshold >= 0 && r.ItemTotal <= r.LowThreshold && !doneLow[r.ItemID] {
			doneLow[r.ItemID] = true
			low.Rows = append(low.Rows, []any{r.ItemName, r.ItemTotal, r.UnitCode, r.LowThreshold})
		}
	}
	return Report{Title: "Stock", Sections: []Section{low, a},
		Notes: []string{"This is stock right now, worked out from every stock movement. Quantities are in the item's own unit (pieces, grams, millilitres). It does not give stock value or cost of goods yet."}}, nil
}

// ---- rooms ----

func (s *Service) rooms(ctx context.Context, q *repository.Queries, f, t time.Time, loc *time.Location) (Report, error) {
	nights, err := q.RoomNights(ctx, repository.RoomNightsParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	a := Section{Key: "nights", Title: "Rooms by night", Columns: []Column{col("date", "Night", "date"), col("rooms", "Rooms", "int"), col("occupied", "Rooms sold", "int"), col("occupancy", "Occupancy", "bp"),
		col("revenue", "Room revenue", "money"), col("adr", "Average per room sold", "money"), col("revpar", "Per room available", "money"), col("tax", "Tax included", "money")}}
	var sumRooms, sumNights, sumRev, sumTax int64
	for _, n := range nights {
		a.Rows = append(a.Rows, []any{day(n.Night.Time), n.Rooms, n.Nights, ratio(n.Nights*10000, n.Rooms), n.RevenueCents, ratio(n.RevenueCents, n.Nights), ratio(n.RevenueCents, n.Rooms), n.TaxCents})
		sumRooms, sumNights, sumRev, sumTax = sumRooms+n.Rooms, sumNights+n.Nights, sumRev+n.RevenueCents, sumTax+n.TaxCents
	}
	a.Totals = []any{"Total", sumRooms, sumNights, ratio(sumNights*10000, sumRooms), sumRev, ratio(sumRev, sumNights), ratio(sumRev, sumRooms), sumTax}
	st, err := q.RoomStatusCounts(ctx)
	if err != nil {
		return Report{}, err
	}
	b := Section{Key: "status", Title: "Rooms right now", Columns: []Column{col("status", "Status", "text"), col("rooms", "Rooms", "int")}}
	labels := map[string]string{"vacant_ready": "Ready", "occupied": "Occupied", "dirty": "Needs cleaning", "cleaning": "Being cleaned", "out_of_service": "Out of service", "reserved": "Reserved"}
	for _, r := range st {
		b.Rows = append(b.Rows, []any{labels[r.Status], r.Rooms})
	}
	n := s.now().In(loc)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	h, err := q.HouseToday(ctx, dt(today))
	if err != nil {
		return Report{}, err
	}
	c := Section{Key: "today", Title: "Today (" + day(today) + ")", Columns: []Column{col("what", "", "text"), col("count", "Guests", "int")},
		Rows: [][]any{{"In the house now", h.InHouse}, {"Arriving today (booked, not yet in)", h.Arrivals}, {"Due to leave today", h.Departures}}}
	return Report{Title: "Rooms", From: day(f), To: day(t), Sections: []Section{a, b, c},
		Notes: []string{"A room is sold for a night when a room charge was posted for that night. Revenue is those charges minus any night a manager credited back, tax included.",
			"Occupancy uses the rooms that are active today for every night in the range, so it is exact for recent dates and approximate if rooms were added or switched off in the period (S-108)."}}, nil
}

// ratio is a/b rounded to the nearest whole unit, or 0 when b is 0.
func ratio(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	return (a + b/2) / b
}

// ---- CSV ----

func cell(c Column, v any) string {
	if v == nil {
		return ""
	}
	switch c.Kind {
	case "money":
		if n, ok := v.(int64); ok {
			sign := ""
			if n < 0 {
				sign, n = "-", -n
			}
			return fmt.Sprintf("%s%d.%02d", sign, n/100, n%100)
		}
	case "bp":
		if n, ok := v.(int64); ok {
			return fmt.Sprintf("%d.%02d", n/100, n%100)
		}
	}
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case string:
		return csvSafe(x)
	}
	return fmt.Sprint(v)
}

// csvSafe stops a value that starts like a spreadsheet formula from being run when the file is opened.
func csvSafe(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@\t\r") {
		return "'" + s
	}
	return s
}

// WriteCSV writes a report as CSV: a title line, then each section as a heading, a header row and its rows. Money is in
// KES with two decimals and occupancy is a percentage.
func WriteCSV(w io.Writer, r Report) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{r.Title})
	if r.From != "" {
		_ = cw.Write([]string{"From " + r.From + " to " + r.To})
	}
	for _, sec := range r.Sections {
		_ = cw.Write(nil)
		_ = cw.Write([]string{sec.Title})
		head := make([]string, len(sec.Columns))
		for i, c := range sec.Columns {
			head[i] = c.Label
			if c.Kind == "money" {
				head[i] += " (KES)"
			}
			if c.Kind == "bp" {
				head[i] += " (%)"
			}
		}
		_ = cw.Write(head)
		write := func(row []any) {
			out := make([]string, len(sec.Columns))
			for i, c := range sec.Columns {
				if i < len(row) {
					out[i] = cell(c, row[i])
				}
			}
			_ = cw.Write(out)
		}
		for _, row := range sec.Rows {
			write(row)
		}
		if sec.Totals != nil {
			write(sec.Totals)
		}
	}
	cw.Flush()
	return cw.Error()
}

// Export builds the report and records who exported it (exports are audited). The caller streams the CSV.
func (s *Service) Export(ctx context.Context, actor *auth.Principal, name, from, to string, m Meta) (Report, error) {
	if actor == nil || !actor.Has("reports.export") {
		return Report{}, ErrForbidden
	}
	r, err := s.Build(ctx, actor, name, from, to)
	if err != nil {
		return Report{}, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return s.rec.Record(ctx, tx, audit.Entry{ActorID: actor.UserID, ActorLabel: actor.Username, Action: "report.exported", EntityType: "report", EntityID: name,
			After: map[string]any{"report": name, "from": r.From, "to": r.To, "format": "csv"}, RequestID: m.RequestID, IP: m.IP})
	})
	return r, err
}
