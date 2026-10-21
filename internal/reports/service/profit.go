package service

import (
	"context"
	"sort"
	"time"

	"zuscar/backend/internal/reports/repository"
)

// categoryLabel names an expense category the way the screens do.
func categoryLabel(c string) string {
	switch c {
	case "rent":
		return "Rent"
	case "utilities":
		return "Power, water, internet"
	case "wages":
		return "Wages"
	case "transport":
		return "Transport"
	case "repairs":
		return "Repairs and maintenance"
	case "supplies":
		return "Cleaning and other supplies"
	case "licences":
		return "Licences and fees"
	case "marketing":
		return "Marketing"
	case "drawer":
		return "Paid from a till drawer"
	}
	return "Other"
}

func expMethodLabel(m string) string {
	switch m {
	case "cash":
		return "Cash"
	case "mpesa":
		return "M-Pesa"
	case "bank":
		return "Bank"
	case "card":
		return "Card"
	}
	return "Other"
}

// spend is every shilling spent in the range that is not stock bought: the expenses ledger and the cash expenses
// entered on shifts. The two are kept apart on purpose (S-111), so each is counted once.
type spend struct {
	rows       [][]any // date, what, who paid, how, amount
	byCategory map[string]struct{ n, cents int64 }
	ledger     int64
	drawer     int64
}

func (s *Service) spending(ctx context.Context, q *repository.Queries, f, t time.Time, zone string) (spend, error) {
	out := spend{byCategory: map[string]struct{ n, cents int64 }{}}
	type line struct {
		date string
		row  []any
	}
	var all []line
	rows, err := q.ExpenseRows(ctx, repository.ExpenseRowsParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		d := day(r.ExpenseDate.Time)
		all = append(all, line{d, []any{d, categoryLabel(r.Category) + ": " + r.Description, r.PaidBy, expMethodLabel(r.PaidWith), r.AmountCents}})
		c := out.byCategory[r.Category]
		out.byCategory[r.Category] = struct{ n, cents int64 }{c.n + 1, c.cents + r.AmountCents}
		out.ledger += r.AmountCents
	}
	dr, err := q.DrawerExpenses(ctx, repository.DrawerExpensesParams{Tz: zone, FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return out, err
	}
	for _, r := range dr {
		d := day(r.Day.Time)
		all = append(all, line{d, []any{d, "Till drawer: " + r.Reason, r.PaidBy, "Cash", r.AmountCents}})
		c := out.byCategory["drawer"]
		out.byCategory["drawer"] = struct{ n, cents int64 }{c.n + 1, c.cents + r.AmountCents}
		out.drawer += r.AmountCents
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].date < all[j].date })
	for _, l := range all {
		out.rows = append(out.rows, l.row)
	}
	return out, nil
}

func (sp spend) sections() (byCat, list Section) {
	byCat = Section{Key: "by_category", Title: "Spending by kind", Columns: []Column{col("category", "Kind", "text"), col("entries", "Entries", "int"), col("amount", "Amount", "money")}}
	keys := make([]string, 0, len(sp.byCategory))
	for k := range sp.byCategory {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var n, sum int64
	for _, k := range keys {
		v := sp.byCategory[k]
		byCat.Rows = append(byCat.Rows, []any{categoryLabel(k), v.n, v.cents})
		n, sum = n+v.n, sum+v.cents
	}
	byCat.Totals = []any{"Total", n, sum}
	list = Section{Key: "expenses", Title: "Every expense", Columns: []Column{col("date", "Day", "date"), col("what", "What", "text"), col("paid_by", "Who paid", "text"), col("how", "How", "text"), col("amount", "Amount", "money")}, Rows: sp.rows}
	list.Totals = []any{"Total", nil, nil, nil, sp.ledger + sp.drawer}
	return byCat, list
}

// ---- expenses and estimated profit ----

func (s *Service) expensesProfit(ctx context.Context, q *repository.Queries, f, t time.Time, zone string) (Report, error) {
	sales, err := q.SalesTotals(ctx, repository.SalesTotalsParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	back, err := q.ReturnsTotals(ctx, repository.ReturnsTotalsParams{FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	stock, err := q.StockReceivedByDay(ctx, repository.StockReceivedByDayParams{Tz: zone, FromDay: dt(f), ToDay: dt(t)})
	if err != nil {
		return Report{}, err
	}
	sp, err := s.spending(ctx, q, f, t, zone)
	if err != nil {
		return Report{}, err
	}
	var stockCost, purchases int64
	stockSec := Section{Key: "stock_bought", Title: "Stock received", Columns: []Column{col("date", "Day", "date"), col("purchases", "Deliveries", "int"), col("cost", "Cost of what arrived", "money")}}
	for _, r := range stock {
		stockSec.Rows = append(stockSec.Rows, []any{day(r.Day.Time), r.Purchases, r.CostCents})
		stockCost, purchases = stockCost+r.CostCents, purchases+r.Purchases
	}
	stockSec.Totals = []any{"Total", purchases, stockCost}

	net := sales.TotalCents - back.AmountCents
	tax := sales.TaxCents - back.TaxCents
	before := net - tax
	profit := before - stockCost - sp.ledger - sp.drawer
	sum := Section{Key: "profit", Title: "Estimated profit", Columns: []Column{col("what", "", "text"), col("amount", "Amount", "money")}}
	sum.Rows = [][]any{
		{"Sales (tax included)", sales.TotalCents},
		{"Refunds and voids", -back.AmountCents},
		{"Net sales (tax included)", net},
		{"Tax included in those sales (not yours to keep)", -tax},
		{"Sales before tax", before},
		{"Stock received", -stockCost},
		{"Expenses entered", -sp.ledger},
		{"Cash expenses from till drawers", -sp.drawer},
	}
	sum.Totals = []any{"Estimated profit", profit}
	byCat, list := sp.sections()
	return Report{Title: "Expenses and estimated profit", From: day(f), To: day(t), Sections: []Section{sum, byCat, list, stockSec},
		Notes: []string{
			"This is an estimate, not accounts. It takes the stock that arrived in these days, not the stock that was sold, so a big delivery makes a quiet profit and a month of selling old stock makes a rich one. Use a longer range to smooth that out (S-111).",
			"Sales before tax uses the tax recorded on each sale and refund. Wages, rent and anything else you pay must be entered as expenses to count here. Voided expenses are left out.",
			"Cash expenses entered on a shift count once, here, and are not also in the expenses you entered (S-111).",
		}}, nil
}

// ---- the end-of-day closing sheet ----

func (s *Service) dayClose(ctx context.Context, q *repository.Queries, f, t time.Time, zone string, loc *time.Location) (Report, error) {
	if !f.Equal(t) {
		return Report{}, &ValidationError{Field: "to", Msg: "the closing summary is for one day: leave out to, or make it the same as from"}
	}
	var secs []Section
	pick := func(r Report, keys ...string) {
		for _, k := range keys {
			for _, sec := range r.Sections {
				if sec.Key == k {
					secs = append(secs, sec)
				}
			}
		}
	}
	sales, err := s.sales(ctx, q, f, t, 0)
	if err != nil {
		return Report{}, err
	}
	pick(sales, "days", "modules", "payment_status")
	col2, err := s.collections(ctx, q, f, t, zone, 0)
	if err != nil {
		return Report{}, err
	}
	pick(col2, "collected", "paid_out", "net_by_method", "pending")
	dvr, err := s.dvr(ctx, q, f, t)
	if err != nil {
		return Report{}, err
	}
	pick(dvr, "discounts", "returns")

	shifts, err := q.ShiftsOnDay(ctx, repository.ShiftsOnDayParams{Tz: zone, Day: dt(f)})
	if err != nil {
		return Report{}, err
	}
	sh := Section{Key: "shifts", Title: "Cashier shifts that started today", Columns: []Column{col("cashier", "Cashier", "text"), col("status", "Status", "text"), col("float", "Opening float", "money"),
		col("expected", "Cash expected", "money"), col("declared", "Cash counted", "money"), col("variance", "Over / short", "money")}}
	for _, r := range shifts {
		exp, dec, vr := any("still open"), any("still open"), any("still open")
		if r.ExpectedCashCents.Valid {
			exp = r.ExpectedCashCents.Int64
		}
		if r.DeclaredCashCents.Valid {
			dec = r.DeclaredCashCents.Int64
		}
		if r.VarianceCents.Valid {
			vr = r.VarianceCents.Int64
		}
		sh.Rows = append(sh.Rows, []any{r.Cashier, shiftLabel(r.Status), r.OpeningFloatCents, exp, dec, vr})
	}
	mv, err := q.CashMovementsOnDay(ctx, repository.CashMovementsOnDayParams{Tz: zone, Day: dt(f)})
	if err != nil {
		return Report{}, err
	}
	cm := Section{Key: "cash_movements", Title: "Cash put in and taken out of drawers", Columns: []Column{col("kind", "What", "text"), col("entries", "Entries", "int"), col("amount", "Amount", "money")}}
	for _, r := range mv {
		cm.Rows = append(cm.Rows, []any{movementLabel(r.Kind), r.Entries, r.AmountCents})
	}
	sp, err := s.spending(ctx, q, f, t, zone)
	if err != nil {
		return Report{}, err
	}
	_, list := sp.sections()
	list.Title = "Expenses today"
	secs = append(secs, sh, cm, list)

	rooms, err := s.rooms(ctx, q, f, t, loc)
	if err != nil {
		return Report{}, err
	}
	pick(rooms, "nights", "today")
	return Report{Title: "End-of-day closing summary", From: day(f), To: day(t), Sections: secs,
		Notes: []string{"Sales are counted on the day they were made; money received on the day it was confirmed; refunds and voids on the day they were done (S-107). Payments still waiting for confirmation are not in any total.",
			"Cash expected, counted and over/short are the figures each cashier's shift recorded when it was closed. A shift still open shows no figures until it is."}}, nil
}

func shiftLabel(st string) string {
	switch st {
	case "open":
		return "Open"
	case "reconciled":
		return "Checked"
	}
	return "Closed"
}

func movementLabel(k string) string {
	switch k {
	case "paid_in":
		return "Cash put in"
	case "paid_out":
		return "Cash paid out"
	}
	return "Expenses paid in cash"
}
