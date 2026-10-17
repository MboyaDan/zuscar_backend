// Package handler exposes the orders module over HTTP (docs/api/routes.csv, Stage 10a).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/orders/service"
	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Register mounts the routes. guard is the identity module's sign-in and CSRF middleware, passed in so this
// module does not import identity (docs/architecture.md section 3).
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	orders := requirePermission("orders.manage")
	catalogue := requirePermission("catalogue.manage")

	t := v1.Group("/tables", guard...)
	t.GET("", orders, h.listTables)
	t.POST("", catalogue, h.createTable)
	t.PATCH("/:id", catalogue, h.updateTable)

	o := v1.Group("/orders", guard...)
	o.GET("", orders, h.listOrders)
	o.POST("", orders, h.createOrder)
	o.GET("/:id", orders, h.getOrder)
	o.POST("/:id/items", orders, h.addItem)
	o.PATCH("/:id/items/:itemId", orders, h.updateItem)
	o.POST("/:id/items/:itemId/cancel", orders, h.cancelItem)
	o.POST("/:id/cancel", orders, h.cancelOrder)
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

func decode(c *gin.Context, dst any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request body is not valid JSON.", zhttp.FieldError{Field: "body", Message: "invalid or unexpected JSON"})
		return false
	}
	return true
}

func idParam(c *gin.Context, name, what string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That "+what+" does not exist.")
		return 0, false
	}
	return id, true
}

func required(c *gin.Context, fields ...string) {
	bad := make([]zhttp.FieldError, 0, len(fields))
	for _, f := range fields {
		bad = append(bad, zhttp.FieldError{Field: f, Message: "is required"})
	}
	zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", bad...)
}

// ---- tables ----

func (h *Handler) listTables(c *gin.Context) {
	out, err := h.svc.ListTables(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *Handler) createTable(c *gin.Context) {
	var in struct {
		Label string `json:"label"`
		Seats *int32 `json:"seats"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Seats == nil {
		required(c, "seats")
		return
	}
	out, err := h.svc.CreateTable(c.Request.Context(), auth.From(c), in.Label, *in.Seats, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/tables/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"table": out})
}

func (h *Handler) updateTable(c *gin.Context) {
	id, ok := idParam(c, "id", "table")
	if !ok {
		return
	}
	var in struct {
		Label  *string `json:"label"`
		Seats  *int32  `json:"seats"`
		Active *bool   `json:"active"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Label == nil && in.Seats == nil && in.Active == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "body", Message: "give at least one of label, seats, active"})
		return
	}
	out, err := h.svc.UpdateTable(c.Request.Context(), auth.From(c), id, service.TableChange{Label: in.Label, Seats: in.Seats, Active: in.Active}, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"table": out})
}

// ---- orders ----

func (h *Handler) listOrders(c *gin.Context) {
	f := service.OrderFilter{Status: c.Query("status")}
	for name, dst := range map[string]**int64{"table_id": &f.TableID, "cursor": &f.Before} {
		if v := c.Query(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: name, Message: "must be a number"})
				return
			}
			*dst = &n
		}
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "limit", Message: "must be a number"})
			return
		}
		f.Limit = int32(n)
	}
	out, next, err := h.svc.ListOrders(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "next_cursor": next})
}

func (h *Handler) createOrder(c *gin.Context) {
	var in struct {
		OrderType string `json:"order_type"`
		TableID   *int64 `json:"table_id"`
		Note      string `json:"note"`
	}
	if !decode(c, &in) {
		return
	}
	out, err := h.svc.CreateOrder(c.Request.Context(), auth.From(c), in.OrderType, in.TableID, in.Note, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/orders/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"order": out})
}

func (h *Handler) getOrder(c *gin.Context) {
	id, ok := idParam(c, "id", "order")
	if !ok {
		return
	}
	out, err := h.svc.GetOrder(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": out})
}

func (h *Handler) addItem(c *gin.Context) {
	id, ok := idParam(c, "id", "order")
	if !ok {
		return
	}
	var in struct {
		ProductID       *int64  `json:"product_id"`
		Qty             *int32  `json:"qty"`
		Note            string  `json:"note"`
		ModifierIDs     []int64 `json:"modifier_ids"`
		ExpectedVersion *int32  `json:"expected_version"`
	}
	if !decode(c, &in) {
		return
	}
	var absent []string
	if in.ProductID == nil {
		absent = append(absent, "product_id")
	}
	if in.Qty == nil {
		absent = append(absent, "qty")
	}
	if len(absent) > 0 {
		required(c, absent...)
		return
	}
	out, err := h.svc.AddItem(c.Request.Context(), auth.From(c), id, service.NewItem{ProductID: *in.ProductID, Qty: *in.Qty, Note: in.Note, ModifierIDs: in.ModifierIDs}, in.ExpectedVersion, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"order": out})
}

func (h *Handler) updateItem(c *gin.Context) {
	id, ok := idParam(c, "id", "order")
	if !ok {
		return
	}
	item, ok := idParam(c, "itemId", "order item")
	if !ok {
		return
	}
	var in struct {
		Qty             *int32  `json:"qty"`
		Note            *string `json:"note"`
		ExpectedVersion *int32  `json:"expected_version"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Qty == nil && in.Note == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "body", Message: "give qty or note"})
		return
	}
	out, err := h.svc.UpdateItem(c.Request.Context(), auth.From(c), id, item, service.ItemChange{Qty: in.Qty, Note: in.Note}, in.ExpectedVersion, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": out})
}

type cancelBody struct {
	Reason          string `json:"reason"`
	ExpectedVersion *int32 `json:"expected_version"`
}

func (h *Handler) cancelItem(c *gin.Context) {
	id, ok := idParam(c, "id", "order")
	if !ok {
		return
	}
	item, ok := idParam(c, "itemId", "order item")
	if !ok {
		return
	}
	var in cancelBody
	if !decode(c, &in) {
		return
	}
	out, err := h.svc.CancelItem(c.Request.Context(), auth.From(c), id, item, in.Reason, in.ExpectedVersion, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": out})
}

func (h *Handler) cancelOrder(c *gin.Context) {
	id, ok := idParam(c, "id", "order")
	if !ok {
		return
	}
	var in cancelBody
	if !decode(c, &in) {
		return
	}
	out, err := h.svc.CancelOrder(c.Request.Context(), auth.From(c), id, in.Reason, in.ExpectedVersion, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": out})
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
		var fe []zhttp.FieldError
		if cf.Field != "" {
			fe = append(fe, zhttp.FieldError{Field: cf.Field, Message: cf.Msg})
		}
		msg := cf.Msg
		if cf.Code == "conflict" {
			msg = "That " + cf.Field + " is already in use."
		}
		zhttp.WriteError(c, http.StatusConflict, cf.Code, msg, fe...)
	case errors.As(err, &verr):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: verr.Field, Message: verr.Msg})
	default:
		h.log.Error("orders error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}
