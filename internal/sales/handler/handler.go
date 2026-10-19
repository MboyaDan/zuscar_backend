// Package handler exposes the sales module over HTTP (docs/api/routes.csv, Stage 11).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	orderservice "zuscar/backend/internal/orders/service"
	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/sales/service"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Register mounts the routes. guard is the identity module's sign-in and CSRF middleware, passed in so this
// module does not import identity (docs/architecture.md section 3).
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	create := requirePermission("sales.create")
	o := v1.Group("/orders", guard...)
	o.GET("/:id/bill", create, h.bill)
	o.POST("/:id/finalise", create, idempotency.Required(), h.finalise)

	s := v1.Group("/sales", guard...)
	s.GET("", requirePermission("sales.view"), h.listSales)
	s.GET("/:id", requirePermission("sales.view"), h.getSale)
	s.POST("/:id/payments", create, idempotency.Required(), h.addPayment)
	s.GET("/:id/refund-quote", requirePermission("sales.refund"), h.refundQuote)
	s.POST("/:id/refunds", requirePermission("sales.refund"), idempotency.Required(), h.refund)
	s.POST("/:id/void", requirePermission("sales.void"), idempotency.Required(), h.void)

	sh := v1.Group("/shifts", guard...)
	manage := requirePermission("shifts.manage")
	sh.POST("/open", manage, idempotency.Required(), h.openShift)
	sh.GET("/current", manage, h.currentShift)
	sh.GET("", requirePermission("shifts.close"), h.listShifts)
	sh.GET("/:id/summary", manage, h.shiftSummary)
	sh.POST("/:id/cash-movements", manage, idempotency.Required(), h.addMovement)
	sh.POST("/:id/close", requirePermission("shifts.close"), idempotency.Required(), h.closeShift)
	sh.POST("/:id/reconcile", requirePermission("payments.reconcile"), idempotency.Required(), h.reconcileShift)

	rc := v1.Group("/reconciliation", guard...)
	rc.POST("/manual-receipts", requirePermission("payments.reconcile"), idempotency.Required(), h.enterManualReceipt)
	rc.GET("/manual-receipts", requirePermission("payments.reconcile"), h.listManualReceipts)

	pm := v1.Group("/payments", guard...)
	pm.GET("", requirePermission("sales.view"), h.listPayments)
	reconcile := requirePermission("payments.reconcile")
	pm.POST("/:id/confirm", reconcile, idempotency.Required(), h.confirmPayment)
	pm.POST("/:id/fail", reconcile, h.failPayment)
}

func requirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !auth.From(c).Has(permission) {
			zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
			return
		}
		c.Next()
	}
}

func meta(c *gin.Context) service.Meta {
	m := service.Meta{RequestID: zhttp.RequestIDFrom(c)}
	if ip, err := netip.ParseAddr(c.ClientIP()); err == nil {
		m.IP = &ip
	}
	return m
}

func idParam(c *gin.Context, what string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That "+what+" does not exist.")
		return 0, false
	}
	return id, true
}

func (h *Handler) bill(c *gin.Context) {
	id, ok := idParam(c, "order")
	if !ok {
		return
	}
	out, err := h.svc.Bill(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"bill": out})
}

func (h *Handler) finalise(c *gin.Context) {
	id, ok := idParam(c, "order")
	if !ok {
		return
	}
	var in struct {
		Discounts []struct {
			OrderItemID int64  `json:"order_item_id"`
			AmountCents int64  `json:"amount_cents"`
			Reason      string `json:"reason"`
		} `json:"discounts"`
		Payments []struct {
			Method      string `json:"method"`
			AmountCents int64  `json:"amount_cents"`
			Reference   string `json:"reference"`
		} `json:"payments"`
		ExpectedVersion *int32 `json:"expected_version"`
	}
	if c.Request.ContentLength != 0 {
		dec := json.NewDecoder(c.Request.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request body is not valid JSON.", zhttp.FieldError{Field: "body", Message: "invalid or unexpected JSON"})
			return
		}
	}
	fin := service.FinaliseInput{ExpectedVersion: in.ExpectedVersion}
	for _, d := range in.Discounts {
		fin.Discounts = append(fin.Discounts, service.DiscountInput{OrderItemID: d.OrderItemID, AmountCents: d.AmountCents, Reason: d.Reason})
	}
	for _, p := range in.Payments {
		fin.Payments = append(fin.Payments, service.PaymentInput{Method: p.Method, AmountCents: p.AmountCents, Reference: p.Reference})
	}
	out, replayed, err := h.svc.Finalise(c.Request.Context(), auth.From(c), id, fin, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		var r struct {
			Sale struct {
				ID int64 `json:"id"`
			} `json:"sale"`
		}
		_ = json.Unmarshal(replayed.Body, &r)
		c.Header("Location", "/api/v1/sales/"+strconv.FormatInt(r.Sale.ID, 10))
		c.Header(idempotency.ReplayHeader, "true")
		c.Data(replayed.Status, "application/json; charset=utf-8", replayed.Body)
		return
	}
	c.Header("Location", "/api/v1/sales/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"sale": out})
}

func (h *Handler) getSale(c *gin.Context) {
	id, ok := idParam(c, "sale")
	if !ok {
		return
	}
	out, err := h.svc.GetSale(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sale": out})
}

// body decodes an optional JSON body, refusing unknown fields.
func body(c *gin.Context, into any) bool {
	if c.Request.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request body is not valid JSON.", zhttp.FieldError{Field: "body", Message: "invalid or unexpected JSON"})
		return false
	}
	return true
}

func replay(c *gin.Context, st *idempotency.Stored) {
	c.Header(idempotency.ReplayHeader, "true")
	c.Data(st.Status, "application/json; charset=utf-8", st.Body)
}

func (h *Handler) addPayment(c *gin.Context) {
	id, ok := idParam(c, "sale")
	if !ok {
		return
	}
	var in struct {
		Method      string `json:"method"`
		AmountCents int64  `json:"amount_cents"`
		Reference   string `json:"reference"`
	}
	if !body(c, &in) {
		return
	}
	out, st, err := h.svc.AddPayment(c.Request.Context(), auth.From(c), id, service.PaymentInput{Method: in.Method, AmountCents: in.AmountCents, Reference: in.Reference}, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.Header("Location", "/api/v1/sales/"+strconv.FormatInt(id, 10))
	c.JSON(http.StatusCreated, out)
}

func (h *Handler) refund(c *gin.Context) {
	id, ok := idParam(c, "sale")
	if !ok {
		return
	}
	var in service.RefundInput
	if !body(c, &in) {
		return
	}
	out, st, err := h.svc.Refund(c.Request.Context(), auth.From(c), id, in, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.Header("Location", "/api/v1/sales/"+strconv.FormatInt(id, 10))
	c.JSON(http.StatusCreated, out)
}

func (h *Handler) void(c *gin.Context) {
	id, ok := idParam(c, "sale")
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !body(c, &in) {
		return
	}
	out, st, err := h.svc.Void(c.Request.Context(), auth.From(c), id, in.Reason, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.Header("Location", "/api/v1/sales/"+strconv.FormatInt(id, 10))
	c.JSON(http.StatusCreated, out)
}

// refundQuote answers "what would taking these lines back come to": ?lines=ITEM:QTY,ITEM:QTY
func (h *Handler) refundQuote(c *gin.Context) {
	id, ok := idParam(c, "sale")
	if !ok {
		return
	}
	var lines []service.RefundLineInput
	if raw := strings.TrimSpace(c.Query("lines")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			a, b, found := strings.Cut(part, ":")
			item, e1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
			qty, e2 := strconv.ParseInt(strings.TrimSpace(b), 10, 32)
			if !found || e1 != nil || e2 != nil || item <= 0 || qty <= 0 {
				zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "lines", Message: "use lines=ITEM:QTY,ITEM:QTY with whole numbers above 0"})
				return
			}
			lines = append(lines, service.RefundLineInput{SaleItemID: item, Qty: int32(qty)})
		}
	}
	out, err := h.svc.QuoteRefund(c.Request.Context(), auth.From(c), id, lines)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"quote": out})
}

func (h *Handler) confirmPayment(c *gin.Context) {
	id, ok := idParam(c, "payment")
	if !ok {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if !body(c, &in) {
		return
	}
	out, st, err := h.svc.ConfirmPayment(c.Request.Context(), auth.From(c), id, in.Note, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment": out})
}

func (h *Handler) failPayment(c *gin.Context) {
	id, ok := idParam(c, "payment")
	if !ok {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.FailPayment(c.Request.Context(), auth.From(c), id, in.Note, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment": out})
}

func queryInt(c *gin.Context, name string) (int64, bool) {
	v := c.Query(name)
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: name, Message: "must be a positive whole number"})
		return 0, false
	}
	return n, true
}

func oneOf(c *gin.Context, name string, allowed ...string) (string, bool) {
	v := c.Query(name)
	if v == "" {
		return "", true
	}
	for _, a := range allowed {
		if v == a {
			return v, true
		}
	}
	zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: name, Message: "is not a recognised value"})
	return "", false
}

func (h *Handler) listPayments(c *gin.Context) {
	var f service.PaymentFilter
	var ok bool
	if f.Status, ok = oneOf(c, "status", "pending_confirmation", "confirmed", "failed", "reversed"); !ok {
		return
	}
	if f.Method, ok = oneOf(c, "method", "cash", "mpesa_manual", "card_manual", "bank_manual"); !ok {
		return
	}
	if f.ShiftID, ok = queryInt(c, "shift_id"); !ok {
		return
	}
	if f.SaleID, ok = queryInt(c, "sale_id"); !ok {
		return
	}
	if f.BeforeID, ok = queryInt(c, "cursor"); !ok {
		return
	}
	lim, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	f.Limit = int(lim)
	items, more, err := h.svc.ListPayments(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	res := gin.H{"payments": items}
	if more {
		res["next_cursor"] = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) listSales(c *gin.Context) {
	var f service.SaleFilter
	var ok bool
	if f.Status, ok = oneOf(c, "status", "finalised", "partially_refunded", "refunded", "voided"); !ok {
		return
	}
	if f.PaymentStatus, ok = oneOf(c, "payment_status", "unpaid", "partially_paid", "paid", "charged_to_folio"); !ok {
		return
	}
	if d := c.Query("business_date"); d != "" {
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "business_date", Message: "must be a date like 2026-10-19"})
			return
		}
		f.BusinessDate = &t
	}
	if f.BeforeID, ok = queryInt(c, "cursor"); !ok {
		return
	}
	lim, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	f.Limit = int(lim)
	items, more, err := h.svc.ListSales(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	res := gin.H{"sales": items}
	if more {
		res["next_cursor"] = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) openShift(c *gin.Context) {
	var in struct {
		OpeningFloatCents int64  `json:"opening_float_cents"`
		Notes             string `json:"notes"`
	}
	if !body(c, &in) {
		return
	}
	st, out, err := h.svc.OpenShift(c.Request.Context(), auth.From(c), service.OpenShiftInput{OpeningFloatCents: in.OpeningFloatCents, Notes: in.Notes}, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.JSON(http.StatusCreated, out)
}

func (h *Handler) currentShift(c *gin.Context) {
	out, err := h.svc.CurrentShift(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": out})
}

func (h *Handler) shiftSummary(c *gin.Context) {
	id, ok := idParam(c, "shift")
	if !ok {
		return
	}
	out, err := h.svc.ShiftSummary(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": out})
}

func (h *Handler) listShifts(c *gin.Context) {
	var f service.ShiftFilter
	var ok bool
	if f.Status, ok = oneOf(c, "status", "open", "closed", "reconciled"); !ok {
		return
	}
	if f.UserID, ok = queryInt(c, "user_id"); !ok {
		return
	}
	if f.BeforeID, ok = queryInt(c, "cursor"); !ok {
		return
	}
	lim, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	f.Limit = int(lim)
	items, more, err := h.svc.ListShifts(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	res := gin.H{"shifts": items}
	if more {
		res["next_cursor"] = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) addMovement(c *gin.Context) {
	id, ok := idParam(c, "shift")
	if !ok {
		return
	}
	var in struct {
		Kind        string `json:"kind"`
		AmountCents int64  `json:"amount_cents"`
		Reason      string `json:"reason"`
	}
	if !body(c, &in) {
		return
	}
	st, out, err := h.svc.AddCashMovement(c.Request.Context(), auth.From(c), id, service.MovementInput{Kind: in.Kind, AmountCents: in.AmountCents, Reason: in.Reason}, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.JSON(http.StatusCreated, out)
}

func (h *Handler) closeShift(c *gin.Context) {
	id, ok := idParam(c, "shift")
	if !ok {
		return
	}
	var in struct {
		DeclaredCashCents *int64 `json:"declared_cash_cents"`
		Notes             string `json:"notes"`
	}
	if !body(c, &in) {
		return
	}
	if in.DeclaredCashCents == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "declared_cash_cents", Message: "is required: count the cash in the drawer"})
		return
	}
	st, out, err := h.svc.CloseShift(c.Request.Context(), auth.From(c), id, service.CloseInput{DeclaredCashCents: *in.DeclaredCashCents, Notes: in.Notes}, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) reconcileShift(c *gin.Context) {
	id, ok := idParam(c, "shift")
	if !ok {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if !body(c, &in) {
		return
	}
	st, out, err := h.svc.ReconcileShift(c.Request.Context(), auth.From(c), id, in.Note, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if st != nil {
		replay(c, st)
		return
	}
	c.JSON(http.StatusOK, gin.H{"shift": out})
}

func (h *Handler) fail(c *gin.Context, err error) {
	var verr *service.ValidationError
	var nf *service.NotFoundError
	var cf *service.ConflictError
	var fe *service.ForbiddenError
	var onf *orderservice.NotFoundError
	var ocf *orderservice.ConflictError
	switch {
	case errors.Is(err, service.ErrForbidden):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.As(err, &fe):
		zhttp.WriteError(c, http.StatusForbidden, fe.Code, fe.Msg)
	case errors.As(err, &nf):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That "+nf.What+" does not exist.")
	case errors.As(err, &onf):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That "+onf.What+" does not exist.")
	case errors.As(err, &ocf):
		zhttp.WriteError(c, http.StatusConflict, ocf.Code, ocf.Msg)
	case errors.As(err, &cf):
		var f []zhttp.FieldError
		if cf.Field != "" {
			f = append(f, zhttp.FieldError{Field: cf.Field, Message: cf.Msg})
		}
		zhttp.WriteError(c, http.StatusConflict, cf.Code, cf.Msg, f...)
	case errors.As(err, &verr):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: verr.Field, Message: verr.Msg})
	case errors.Is(err, idempotency.ErrKeyReused):
		zhttp.WriteError(c, http.StatusUnprocessableEntity, "idempotency_key_reused", "This Idempotency-Key was already used for a different request. Use a new key for a new action.")
	case errors.Is(err, idempotency.ErrInFlight):
		c.Header("Retry-After", "1")
		zhttp.WriteError(c, http.StatusConflict, "idempotency_conflict", "A request with this Idempotency-Key is still being processed. Try again in a moment.")
	default:
		h.log.Error("sales error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}

// enterManualReceipt records one paper receipt written during an outage (Stage 16, AC-17).
func (h *Handler) enterManualReceipt(c *gin.Context) {
	var in struct {
		ManualReceiptNo string    `json:"manual_receipt_no"`
		OccurredAt      time.Time `json:"occurred_at"`
		Lines           []struct {
			ProductID      int64 `json:"product_id"`
			Qty            int32 `json:"qty"`
			UnitPriceCents int64 `json:"unit_price_cents"`
		} `json:"lines"`
		Payments []struct {
			Method      string `json:"method"`
			AmountCents int64  `json:"amount_cents"`
			Reference   string `json:"reference"`
		} `json:"payments"`
		PaperTotalCents *int64 `json:"paper_total_cents"`
		DiscrepancyNote string `json:"discrepancy_note"`
		SourceNote      string `json:"source_note"`
	}
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request body is not valid JSON.", zhttp.FieldError{Field: "body", Message: "invalid or unexpected JSON"})
		return
	}
	if in.PaperTotalCents == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "paper_total_cents", Message: "is required: the total written on the paper receipt"})
		return
	}
	req := service.OutageInput{ManualReceiptNo: in.ManualReceiptNo, OccurredAt: in.OccurredAt, PaperTotalCents: *in.PaperTotalCents, DiscrepancyNote: in.DiscrepancyNote, SourceNote: in.SourceNote}
	for _, l := range in.Lines {
		req.Lines = append(req.Lines, service.OutageLineInput{ProductID: l.ProductID, Qty: l.Qty, UnitPriceCents: l.UnitPriceCents})
	}
	for _, p := range in.Payments {
		req.Payments = append(req.Payments, service.PaymentInput{Method: p.Method, AmountCents: p.AmountCents, Reference: p.Reference})
	}
	out, replayed, err := h.svc.EnterManualReceipt(c.Request.Context(), auth.From(c), req, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		var r struct {
			Sale struct {
				ID int64 `json:"id"`
			} `json:"sale"`
		}
		_ = json.Unmarshal(replayed.Body, &r)
		c.Header("Location", "/api/v1/sales/"+strconv.FormatInt(r.Sale.ID, 10))
		c.Header(idempotency.ReplayHeader, "true")
		c.Data(replayed.Status, "application/json; charset=utf-8", replayed.Body)
		return
	}
	c.Header("Location", "/api/v1/sales/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"sale": out})
}

func (h *Handler) listManualReceipts(c *gin.Context) {
	var f service.ManualReceiptFilter
	var ok bool
	f.ManualReceiptNo = c.Query("manual_receipt_no")
	if f.BeforeID, ok = queryInt(c, "cursor"); !ok {
		return
	}
	lim, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	f.Limit = int(lim)
	items, more, err := h.svc.ListManualReceipts(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	res := gin.H{"entries": items}
	if more {
		res["next_cursor"] = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	c.JSON(http.StatusOK, res)
}
