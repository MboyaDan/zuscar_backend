// Package handler exposes the expenses module over HTTP (docs/api/routes.csv, Stage 26).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/expenses/service"
	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/idempotency"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

func manage(c *gin.Context) {
	if !auth.From(c).Has("expenses.manage") {
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
		return
	}
	c.Next()
}

func meta(c *gin.Context) service.Meta {
	m := service.Meta{RequestID: zhttp.RequestIDFrom(c)}
	if ip, err := netip.ParseAddr(c.ClientIP()); err == nil {
		m.IP = &ip
	}
	return m
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

func (h *Handler) fail(c *gin.Context, err error) {
	var ve *service.ValidationError
	var nf *service.NotFoundError
	var cf *service.ConflictError
	switch {
	case errors.Is(err, service.ErrForbidden):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.As(err, &nf):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That "+nf.What+" does not exist.")
	case errors.As(err, &cf):
		zhttp.WriteError(c, http.StatusConflict, cf.Code, cf.Msg)
	case errors.As(err, &ve):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: ve.Field, Message: ve.Msg})
	case errors.Is(err, idempotency.ErrKeyReused):
		zhttp.WriteError(c, http.StatusUnprocessableEntity, "idempotency_key_reused", "This Idempotency-Key was already used for a different request. Use a new key for a new action.")
	case errors.Is(err, idempotency.ErrInFlight):
		c.Header("Retry-After", "1")
		zhttp.WriteError(c, http.StatusConflict, "idempotency_conflict", "A request with this Idempotency-Key is still being processed. Try again in a moment.")
	default:
		h.log.Error("expenses error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}

// Register mounts the expense routes. guard is the identity module's sign-in and CSRF middleware.
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	g := v1.Group("/expenses", guard...)
	g.GET("", manage, h.list)
	g.POST("", manage, idempotency.Required(), h.create)
	g.POST("/:id/void", manage, h.void)
}

func (h *Handler) list(c *gin.Context) {
	limit := 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "limit", Message: "must be a positive whole number"})
			return
		}
		limit = n
	}
	items, more, err := h.svc.List(c.Request.Context(), auth.From(c), c.Query("from"), c.Query("to"), c.Query("include_voided") == "true", limit)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"expenses": items, "more": more, "categories": service.Categories, "methods": service.Methods})
}

func (h *Handler) create(c *gin.Context) {
	var in struct {
		ExpenseDate string `json:"expense_date"`
		Description string `json:"description"`
		Category    string `json:"category"`
		AmountCents int64  `json:"amount_cents"`
		PaidBy      string `json:"paid_by"`
		PaidWith    string `json:"paid_with"`
	}
	if !body(c, &in) {
		return
	}
	out, replayed, err := h.svc.Create(c.Request.Context(), auth.From(c), service.CreateInput{Date: in.ExpenseDate, Description: in.Description, Category: in.Category,
		AmountCents: in.AmountCents, PaidBy: in.PaidBy, PaidWith: in.PaidWith}, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		c.Header(idempotency.ReplayHeader, "true")
		c.Data(replayed.Status, "application/json; charset=utf-8", replayed.Body)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"expense": out})
}

func (h *Handler) void(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That expense does not exist.")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.Void(c.Request.Context(), auth.From(c), id, in.Reason, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"expense": out})
}
