// Package handler exposes the purchasing module over HTTP (docs/api/routes.csv, Stages 19 and 20).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/purchasing/service"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

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

func body(c *gin.Context, into any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request body is not valid JSON.", zhttp.FieldError{Field: "body", Message: "invalid or unexpected JSON"})
		return false
	}
	return true
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

func replay(c *gin.Context, st *idempotency.Stored) {
	c.Header(idempotency.ReplayHeader, "true")
	c.Data(st.Status, "application/json; charset=utf-8", st.Body)
}

func (h *Handler) fail(c *gin.Context, err error) {
	var verr *service.ValidationError
	var nf *service.NotFoundError
	var cf *service.ConflictError
	switch {
	case errors.Is(err, service.ErrForbidden):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.As(err, &nf):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That "+nf.What+" does not exist.")
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
		h.log.Error("purchasing error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}

// Register mounts suppliers and purchases. guard is the identity module's sign-in and CSRF middleware.
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	manage := requirePermission("purchasing.manage")
	s := v1.Group("/suppliers", guard...)
	s.GET("", manage, h.listSuppliers)
	s.POST("", manage, h.createSupplier)
	s.PATCH("/:id", manage, h.updateSupplier)
	p := v1.Group("/purchases", guard...)
	p.GET("", manage, h.listPurchases)
	p.POST("", manage, idempotency.Required(), h.createPurchase)
	p.GET("/:id", manage, h.getPurchase)
	p.POST("/:id/receive", manage, idempotency.Required(), h.receivePurchase)
	p.POST("/:id/cancel", manage, h.cancelPurchase)
}

func (h *Handler) listSuppliers(c *gin.Context) {
	out, err := h.svc.ListSuppliers(c.Request.Context(), auth.From(c), c.Query("active") == "true")
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"suppliers": out})
}

func (h *Handler) createSupplier(c *gin.Context) {
	var in struct {
		Name    string `json:"name"`
		Contact string `json:"contact"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CreateSupplier(c.Request.Context(), auth.From(c), in.Name, in.Contact, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"supplier": out})
}

func (h *Handler) updateSupplier(c *gin.Context) {
	id, ok := idParam(c, "supplier")
	if !ok {
		return
	}
	var raw map[string]json.RawMessage
	if !body(c, &raw) {
		return
	}
	var ch service.SupplierPatch
	for k, v := range raw {
		var err error
		switch k {
		case "name":
			var s string
			err = json.Unmarshal(v, &s)
			ch.Name = &s
		case "contact":
			var s string
			err = json.Unmarshal(v, &s)
			ch.Contact = &s
		case "active":
			var b bool
			err = json.Unmarshal(v, &b)
			ch.Active = &b
		default:
			err = errors.New("unknown field")
		}
		if err != nil {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: k, Message: "is not valid"})
			return
		}
	}
	out, err := h.svc.UpdateSupplier(c.Request.Context(), auth.From(c), id, ch, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"supplier": out})
}

func (h *Handler) listPurchases(c *gin.Context) {
	status := c.Query("status")
	switch status {
	case "", "draft", "received", "cancelled":
	default:
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "status", Message: "must be draft, received or cancelled"})
		return
	}
	sup, ok := queryInt(c, "supplier_id")
	if !ok {
		return
	}
	before, ok := queryInt(c, "before_id")
	if !ok {
		return
	}
	limit, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	out, err := h.svc.ListPurchases(c.Request.Context(), auth.From(c), status, sup, before, int(limit))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"purchases": out})
}

func (h *Handler) getPurchase(c *gin.Context) {
	id, ok := idParam(c, "purchase")
	if !ok {
		return
	}
	out, err := h.svc.GetPurchase(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"purchase": out})
}

func (h *Handler) createPurchase(c *gin.Context) {
	var in struct {
		SupplierID int64  `json:"supplier_id"`
		InvoiceRef string `json:"invoice_ref"`
		Note       string `json:"note"`
		Lines      []struct {
			ItemID        int64  `json:"item_id"`
			Qty           int64  `json:"qty"`
			UnitID        *int64 `json:"unit_id"`
			UnitCostCents int64  `json:"unit_cost_cents"`
		} `json:"lines"`
	}
	if !body(c, &in) {
		return
	}
	pi := service.PurchaseInput{SupplierID: in.SupplierID, InvoiceRef: in.InvoiceRef, Note: in.Note}
	for _, l := range in.Lines {
		pi.Lines = append(pi.Lines, service.PurchaseLineInput{ItemID: l.ItemID, Qty: l.Qty, UnitID: l.UnitID, UnitCostCents: l.UnitCostCents})
	}
	out, replayed, err := h.svc.CreatePurchase(c.Request.Context(), auth.From(c), pi, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		replay(c, replayed)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"purchase": out})
}

func (h *Handler) receivePurchase(c *gin.Context) {
	id, ok := idParam(c, "purchase")
	if !ok {
		return
	}
	var in struct {
		LocationID int64  `json:"location_id"`
		InvoiceRef string `json:"invoice_ref"`
		Lines      []struct {
			ItemID        int64  `json:"item_id"`
			Qty           int64  `json:"qty"`
			UnitCostCents *int64 `json:"unit_cost_cents"`
		} `json:"lines"`
	}
	if !body(c, &in) {
		return
	}
	ri := service.ReceiveInput{LocationID: in.LocationID, InvoiceRef: in.InvoiceRef}
	for _, l := range in.Lines {
		ri.Lines = append(ri.Lines, service.ReceivedLine{ItemID: l.ItemID, Qty: l.Qty, UnitCostCents: l.UnitCostCents})
	}
	out, replayed, err := h.svc.ReceivePurchase(c.Request.Context(), auth.From(c), id, ri, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		replay(c, replayed)
		return
	}
	c.JSON(http.StatusOK, gin.H{"purchase": out})
}

func (h *Handler) cancelPurchase(c *gin.Context) {
	id, ok := idParam(c, "purchase")
	if !ok {
		return
	}
	out, err := h.svc.CancelPurchase(c.Request.Context(), auth.From(c), id, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"purchase": out})
}
