// Package handler exposes the settings module over HTTP (docs/api/routes.csv, Stage 7b).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/settings/service"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Register mounts the routes. guard is the identity module's sign-in and CSRF middleware, passed in so this
// module does not import identity (docs/architecture.md section 3).
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	s := v1.Group("/settings", guard...)
	s.GET("", h.getSettings)
	s.PATCH("", requirePermission("system.admin"), h.updateSettings)
	t := v1.Group("/tax-rates", guard...)
	t.GET("", h.listTaxRates)
	t.POST("", requirePermission("system.admin"), h.createTaxRate)
	t.PATCH("/:id", requirePermission("system.admin"), h.updateTaxRate)
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

func (h *Handler) getSettings(c *gin.Context) {
	out, err := h.svc.GetSettings(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": out})
}

func (h *Handler) updateSettings(c *gin.Context) {
	var in struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if !decode(c, &in) {
		return
	}
	out, err := h.svc.UpdateSettings(c.Request.Context(), auth.From(c), in.Settings, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": out})
}

func (h *Handler) listTaxRates(c *gin.Context) {
	items, err := h.svc.ListTaxRates(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) createTaxRate(c *gin.Context) {
	var in struct {
		Code      string `json:"code"`
		Name      string `json:"name"`
		RateBP    *int32 `json:"rate_bp"`
		Treatment string `json:"treatment"`
	}
	if !decode(c, &in) {
		return
	}
	var bad []zhttp.FieldError
	for f, v := range map[string]string{"code": in.Code, "name": in.Name, "treatment": in.Treatment} {
		if strings.TrimSpace(v) == "" {
			bad = append(bad, zhttp.FieldError{Field: f, Message: "is required"})
		}
	}
	if in.RateBP == nil {
		bad = append(bad, zhttp.FieldError{Field: "rate_bp", Message: "is required"})
	}
	if len(bad) > 0 {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", bad...)
		return
	}
	rate, err := h.svc.CreateTaxRate(c.Request.Context(), auth.From(c), in.Code, in.Name, *in.RateBP, in.Treatment, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/tax-rates/"+strconv.FormatInt(rate.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"tax_rate": rate})
}

func (h *Handler) updateTaxRate(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That tax category does not exist.")
		return
	}
	var in struct {
		Name   *string `json:"name"`
		RateBP *int32  `json:"rate_bp"`
		Active *bool   `json:"active"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Name == nil && in.RateBP == nil && in.Active == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "body", Message: "give at least one of name, rate_bp, active"})
		return
	}
	rate, err := h.svc.UpdateTaxRate(c.Request.Context(), auth.From(c), id, service.TaxRateChange{Name: in.Name, RateBP: in.RateBP, Active: in.Active}, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tax_rate": rate})
}

func (h *Handler) fail(c *gin.Context, err error) {
	var verr *service.ValidationError
	switch {
	case errors.Is(err, service.ErrForbidden):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, service.ErrReauthRequired):
		zhttp.WriteError(c, http.StatusForbidden, "reauth_required", "Please confirm your password to continue.")
	case errors.Is(err, service.ErrTaxRateMissing):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That tax category does not exist.")
	case errors.Is(err, service.ErrTaxCodeTaken):
		zhttp.WriteError(c, http.StatusConflict, "tax_code_taken", "That tax category code is already in use.", zhttp.FieldError{Field: "code", Message: "is already in use"})
	case errors.As(err, &verr):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: verr.Field, Message: verr.Msg})
	default:
		h.log.Error("settings error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}
