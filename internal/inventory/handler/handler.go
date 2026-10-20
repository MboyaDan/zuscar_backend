// Package handler exposes the inventory module over HTTP (docs/api/routes.csv, Stage 17).
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/inventory/service"
	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/idempotency"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Register mounts the routes. guard is the identity module's sign-in and CSRF middleware.
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	view := requirePermission("inventory.view")
	adjust := requirePermission("inventory.adjust")
	g := v1.Group("/inventory", guard...)
	g.GET("/items", view, h.listItems)
	g.POST("/items", adjust, h.createItem)
	g.PATCH("/items/:id", adjust, h.updateItem)
	g.PUT("/items/:id/conversions", adjust, h.setConversions)
	g.GET("/units", view, h.listUnits)
	g.POST("/units", adjust, h.createUnit)
	g.GET("/locations", view, h.listLocations)
	g.POST("/locations", adjust, h.createLocation)
	g.GET("/balances", view, h.balances)
	g.GET("/movements", view, h.movements)
	g.POST("/opening-balances", adjust, idempotency.Required(), h.openingBalances)
	g.POST("/adjustments", adjust, idempotency.Required(), h.adjustments)
	g.POST("/movements/:id/reverse", adjust, idempotency.Required(), h.reverse)
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
	var fe *service.ForbiddenError
	switch {
	case errors.Is(err, service.ErrForbidden):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.As(err, &fe):
		zhttp.WriteError(c, http.StatusForbidden, fe.Code, fe.Msg)
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
		h.log.Error("inventory error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}

func (h *Handler) listUnits(c *gin.Context) {
	out, err := h.svc.ListUnits(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"units": out})
}

func (h *Handler) createUnit(c *gin.Context) {
	var in struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CreateUnit(c.Request.Context(), auth.From(c), in.Code, in.Name, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"unit": out})
}

func (h *Handler) listLocations(c *gin.Context) {
	out, err := h.svc.ListLocations(c.Request.Context(), auth.From(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"locations": out})
}

func (h *Handler) createLocation(c *gin.Context) {
	var in struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CreateLocation(c.Request.Context(), auth.From(c), in.Code, in.Name, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"location": out})
}

func (h *Handler) listItems(c *gin.Context) {
	out, err := h.svc.ListItems(c.Request.Context(), auth.From(c), service.ItemFilter{Query: c.Query("q"), OnlyActive: c.Query("active") == "true"})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *Handler) createItem(c *gin.Context) {
	var in struct {
		Name              string `json:"name"`
		SKU               string `json:"sku"`
		BaseUnitID        int64  `json:"base_unit_id"`
		LowStockThreshold *int64 `json:"low_stock_threshold"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CreateItem(c.Request.Context(), auth.From(c), service.NewItem{Name: in.Name, SKU: in.SKU, BaseUnitID: in.BaseUnitID, LowStockThreshold: in.LowStockThreshold}, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/inventory/items/"+strconv.FormatInt(out.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"item": out})
}

func (h *Handler) updateItem(c *gin.Context) {
	id, ok := idParam(c, "item")
	if !ok {
		return
	}
	var raw map[string]json.RawMessage
	if !body(c, &raw) {
		return
	}
	var ch service.ItemChange
	for k, v := range raw {
		var err error
		switch k {
		case "name":
			var s string
			err = json.Unmarshal(v, &s)
			ch.Name = &s
		case "sku":
			var s string
			err = json.Unmarshal(v, &s)
			ch.SKU = &s
		case "active":
			var b bool
			err = json.Unmarshal(v, &b)
			ch.Active = &b
		case "low_stock_threshold":
			var n *int64
			err = json.Unmarshal(v, &n)
			ch.LowStockThreshold = service.NullableInt{Set: true, Value: n}
		default:
			err = errors.New("unknown field")
		}
		if err != nil {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: k, Message: "is not valid"})
			return
		}
	}
	out, err := h.svc.UpdateItem(c.Request.Context(), auth.From(c), id, ch, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"item": out})
}

func (h *Handler) setConversions(c *gin.Context) {
	id, ok := idParam(c, "item")
	if !ok {
		return
	}
	var in struct {
		Conversions []struct {
			UnitID    int64 `json:"unit_id"`
			FactorNum int64 `json:"factor_num"`
			FactorDen int64 `json:"factor_den"`
		} `json:"conversions"`
	}
	if !body(c, &in) {
		return
	}
	cs := make([]service.ConversionInput, 0, len(in.Conversions))
	for _, x := range in.Conversions {
		if x.FactorDen == 0 {
			x.FactorDen = 1 // "1 crate = 24 pieces" needs no denominator
		}
		cs = append(cs, service.ConversionInput{UnitID: x.UnitID, FactorNum: x.FactorNum, FactorDen: x.FactorDen})
	}
	out, err := h.svc.SetConversions(c.Request.Context(), auth.From(c), id, cs, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"item": out})
}

func (h *Handler) balances(c *gin.Context) {
	item, ok := queryInt(c, "item_id")
	if !ok {
		return
	}
	loc, ok := queryInt(c, "location_id")
	if !ok {
		return
	}
	out, err := h.svc.Balances(c.Request.Context(), auth.From(c), service.BalanceFilter{ItemID: item, LocationID: loc})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"balances": out})
}

func (h *Handler) movements(c *gin.Context) {
	item, ok := queryInt(c, "item_id")
	if !ok {
		return
	}
	loc, ok := queryInt(c, "location_id")
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
	out, more, err := h.svc.Movements(c.Request.Context(), auth.From(c), service.MovementFilter{ItemID: item, LocationID: loc, Type: c.Query("type"), BeforeID: before, Limit: int(limit)})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"movements": out, "more": more})
}

func (h *Handler) openingBalances(c *gin.Context) {
	var in struct {
		LocationID int64  `json:"location_id"`
		Note       string `json:"note"`
		Lines      []struct {
			ItemID int64  `json:"item_id"`
			Qty    int64  `json:"qty"`
			UnitID *int64 `json:"unit_id"`
		} `json:"lines"`
	}
	if !body(c, &in) {
		return
	}
	oi := service.OpeningInput{LocationID: in.LocationID, Note: in.Note}
	for _, l := range in.Lines {
		oi.Lines = append(oi.Lines, service.OpeningLine{ItemID: l.ItemID, Qty: l.Qty, UnitID: l.UnitID})
	}
	out, replayed, err := h.svc.OpeningBalances(c.Request.Context(), auth.From(c), oi, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		replay(c, replayed)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"movements": out})
}

func (h *Handler) adjustments(c *gin.Context) {
	var in struct {
		ItemID           int64  `json:"item_id"`
		LocationID       int64  `json:"location_id"`
		Qty              int64  `json:"qty"`
		UnitID           *int64 `json:"unit_id"`
		Reason           string `json:"reason"`
		OverrideNegative bool   `json:"override_negative"`
		OverrideReason   string `json:"override_reason"`
	}
	if !body(c, &in) {
		return
	}
	out, replayed, err := h.svc.Adjust(c.Request.Context(), auth.From(c), service.AdjustInput{ItemID: in.ItemID, LocationID: in.LocationID, Qty: in.Qty, UnitID: in.UnitID,
		Reason: in.Reason, OverrideNegative: in.OverrideNegative, OverrideReason: in.OverrideReason}, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		replay(c, replayed)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"movement": out})
}

func (h *Handler) reverse(c *gin.Context) {
	id, ok := idParam(c, "movement")
	if !ok {
		return
	}
	var in struct {
		Reason           string `json:"reason"`
		OverrideNegative bool   `json:"override_negative"`
		OverrideReason   string `json:"override_reason"`
	}
	if !body(c, &in) {
		return
	}
	out, replayed, err := h.svc.Reverse(c.Request.Context(), auth.From(c), service.ReverseInput{MovementID: id, Reason: in.Reason, OverrideNegative: in.OverrideNegative, OverrideReason: in.OverrideReason},
		idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		replay(c, replayed)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"movement": out})
}
