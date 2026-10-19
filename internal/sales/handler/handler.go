// Package handler exposes the sales module over HTTP (docs/api/routes.csv, Stage 11).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

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
	s.GET("/:id", requirePermission("sales.view"), h.getSale)
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
		c.Header("Idempotent-Replay", "true")
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
