// Package handler exposes the catalogue module over HTTP (docs/api/routes.csv, Stage 9a).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/catalogue/service"
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
	v1.GET("/menu", append(append([]gin.HandlerFunc{}, guard...), requirePermission("orders.manage"), h.menu)...)

	manage := requirePermission("catalogue.manage")
	c := v1.Group("/categories", guard...)
	c.GET("", manage, h.listCategories)
	c.POST("", manage, h.createCategory)
	c.PATCH("/:id", manage, h.updateCategory)

	p := v1.Group("/products", guard...)
	p.GET("", manage, h.listProducts)
	p.POST("", manage, h.createProduct)
	p.PATCH("/:id", manage, h.updateProduct)
	p.PUT("/:id/price", manage, h.setPrice)
	p.PUT("/:id/availability", manage, h.setAvailability)
	p.GET("/:id/price-history", manage, h.priceHistory)

	m := v1.Group("/modifiers", guard...)
	m.GET("", manage, h.listModifiers)
	m.POST("", manage, h.createModifier)
	m.PATCH("/:id", manage, h.updateModifier)
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

func idParam(c *gin.Context, what string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That "+what+" does not exist.")
		return 0, false
	}
	return id, true
}

func missing(c *gin.Context, fields ...string) {
	bad := make([]zhttp.FieldError, 0, len(fields))
	for _, f := range fields {
		bad = append(bad, zhttp.FieldError{Field: f, Message: "is required"})
	}
	zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", bad...)
}

// ---- menu and categories ----

func (h *Handler) menu(c *gin.Context) {
	out, err := h.svc.Menu(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"categories": out})
}

func (h *Handler) listCategories(c *gin.Context) {
	out, err := h.svc.ListCategories(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *Handler) createCategory(c *gin.Context) {
	var in struct {
		Module    string `json:"module"`
		Name      string `json:"name"`
		SortOrder int32  `json:"sort_order"`
	}
	if !decode(c, &in) {
		return
	}
	out, err := h.svc.CreateCategory(c.Request.Context(), auth.From(c), in.Module, in.Name, in.SortOrder, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/categories/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"category": out})
}

func (h *Handler) updateCategory(c *gin.Context) {
	id, ok := idParam(c, "category")
	if !ok {
		return
	}
	var in struct {
		Name      *string `json:"name"`
		SortOrder *int32  `json:"sort_order"`
		Active    *bool   `json:"active"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Name == nil && in.SortOrder == nil && in.Active == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "body", Message: "give at least one of name, sort_order, active"})
		return
	}
	out, err := h.svc.UpdateCategory(c.Request.Context(), auth.From(c), id, service.CategoryChange{Name: in.Name, SortOrder: in.SortOrder, Active: in.Active}, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"category": out})
}

// ---- products ----

func (h *Handler) listProducts(c *gin.Context) {
	f := service.ProductFilter{Module: c.Query("module"), Query: c.Query("q")}
	if v := c.Query("category_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "category_id", Message: "must be a number"})
			return
		}
		f.CategoryID = &id
	}
	if v := c.Query("available"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "available", Message: "must be true or false"})
			return
		}
		f.Available = &b
	}
	out, err := h.svc.ListProducts(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *Handler) createProduct(c *gin.Context) {
	var in struct {
		CategoryID    *int64  `json:"category_id"`
		Name          string  `json:"name"`
		SKU           string  `json:"sku"`
		PriceCents    *int64  `json:"price_cents"`
		TaxRateID     *int64  `json:"tax_rate_id"`
		KitchenRouted *bool   `json:"kitchen_routed"`
		ModifierIDs   []int64 `json:"modifier_ids"`
	}
	if !decode(c, &in) {
		return
	}
	var absent []string
	if in.CategoryID == nil {
		absent = append(absent, "category_id")
	}
	if in.PriceCents == nil {
		absent = append(absent, "price_cents")
	}
	if in.TaxRateID == nil {
		absent = append(absent, "tax_rate_id") // never defaulted: the tax treatment is a decision, not an assumption
	}
	if in.KitchenRouted == nil {
		absent = append(absent, "kitchen_routed")
	}
	if len(absent) > 0 {
		missing(c, absent...)
		return
	}
	out, err := h.svc.CreateProduct(c.Request.Context(), auth.From(c), service.NewProduct{CategoryID: *in.CategoryID, Name: in.Name, SKU: in.SKU,
		PriceCents: *in.PriceCents, TaxRateID: *in.TaxRateID, KitchenRouted: *in.KitchenRouted, ModifierIDs: in.ModifierIDs}, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/products/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"product": out})
}

func (h *Handler) updateProduct(c *gin.Context) {
	id, ok := idParam(c, "product")
	if !ok {
		return
	}
	var in struct {
		CategoryID    *int64   `json:"category_id"`
		Name          *string  `json:"name"`
		SKU           *string  `json:"sku"`
		TaxRateID     *int64   `json:"tax_rate_id"`
		KitchenRouted *bool    `json:"kitchen_routed"`
		ModifierIDs   *[]int64 `json:"modifier_ids"`
	}
	if !decode(c, &in) {
		return
	}
	if in.CategoryID == nil && in.Name == nil && in.SKU == nil && in.TaxRateID == nil && in.KitchenRouted == nil && in.ModifierIDs == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "body", Message: "give at least one field to change (price and availability have their own endpoints)"})
		return
	}
	out, err := h.svc.UpdateProduct(c.Request.Context(), auth.From(c), id, service.ProductChange{CategoryID: in.CategoryID, Name: in.Name, SKU: in.SKU,
		TaxRateID: in.TaxRateID, KitchenRouted: in.KitchenRouted, ModifierIDs: in.ModifierIDs}, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"product": out})
}

func (h *Handler) setPrice(c *gin.Context) {
	id, ok := idParam(c, "product")
	if !ok {
		return
	}
	var in struct {
		PriceCents *int64 `json:"price_cents"`
	}
	if !decode(c, &in) {
		return
	}
	if in.PriceCents == nil {
		missing(c, "price_cents")
		return
	}
	out, err := h.svc.SetPrice(c.Request.Context(), auth.From(c), id, *in.PriceCents, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"product": out})
}

func (h *Handler) setAvailability(c *gin.Context) {
	id, ok := idParam(c, "product")
	if !ok {
		return
	}
	var in struct {
		IsAvailable *bool `json:"is_available"`
	}
	if !decode(c, &in) {
		return
	}
	if in.IsAvailable == nil {
		missing(c, "is_available")
		return
	}
	out, err := h.svc.SetAvailability(c.Request.Context(), auth.From(c), id, *in.IsAvailable, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"product": out})
}

func (h *Handler) priceHistory(c *gin.Context) {
	id, ok := idParam(c, "product")
	if !ok {
		return
	}
	out, err := h.svc.PriceHistory(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// ---- modifiers ----

func (h *Handler) listModifiers(c *gin.Context) {
	out, err := h.svc.ListModifiers(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *Handler) createModifier(c *gin.Context) {
	var in struct {
		Name            string `json:"name"`
		PriceDeltaCents int64  `json:"price_delta_cents"`
	}
	if !decode(c, &in) {
		return
	}
	out, err := h.svc.CreateModifier(c.Request.Context(), auth.From(c), in.Name, in.PriceDeltaCents, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/modifiers/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"modifier": out})
}

func (h *Handler) updateModifier(c *gin.Context) {
	id, ok := idParam(c, "modifier")
	if !ok {
		return
	}
	var in struct {
		Name            *string `json:"name"`
		PriceDeltaCents *int64  `json:"price_delta_cents"`
		Active          *bool   `json:"active"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Name == nil && in.PriceDeltaCents == nil && in.Active == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "body", Message: "give at least one of name, price_delta_cents, active"})
		return
	}
	out, err := h.svc.UpdateModifier(c.Request.Context(), auth.From(c), id, service.ModifierChange{Name: in.Name, PriceDeltaCents: in.PriceDeltaCents, Active: in.Active}, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"modifier": out})
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
		zhttp.WriteError(c, http.StatusConflict, "conflict", "That "+cf.Field+" is already in use.", zhttp.FieldError{Field: cf.Field, Message: cf.Msg})
	case errors.As(err, &verr):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: verr.Field, Message: verr.Msg})
	default:
		h.log.Error("catalogue error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}
