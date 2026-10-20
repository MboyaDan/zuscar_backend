package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/inventory/service"
	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/idempotency"
)

// registerPurchasing mounts suppliers and purchases (docs/api/routes.csv, Stage 19). One permission, purchasing.manage.
func (h *Handler) registerPurchasing(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
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
	}
	if !body(c, &in) {
		return
	}
	out, replayed, err := h.svc.ReceivePurchase(c.Request.Context(), auth.From(c), id, in.LocationID, in.InvoiceRef, idempotency.From(c), meta(c))
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
