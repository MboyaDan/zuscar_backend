// Package handler exposes the rooms module over HTTP (docs/api/routes.csv, Stage 22).
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
	"zuscar/backend/internal/platform/idempotency"
	"zuscar/backend/internal/rooms/service"
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
		h.log.Error("rooms error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}

// Register mounts room types, rooms, guests, availability and reservations. guard is the identity module's sign-in and CSRF middleware.
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	manage := requirePermission("rooms.manage")
	t := v1.Group("/room-types", guard...)
	t.GET("", manage, h.listTypes)
	t.POST("", manage, h.createType)
	t.PATCH("/:id", manage, h.updateType)
	r := v1.Group("/rooms", guard...)
	r.GET("", manage, h.listRooms)
	r.POST("", manage, h.createRoom)
	r.PATCH("/:id", manage, h.updateRoom)
	g := v1.Group("/guests", guard...)
	g.GET("", manage, h.listGuests)
	g.POST("", manage, h.createGuest)
	g.PATCH("/:id", manage, h.updateGuest)
	v1.GET("/availability", append(append([]gin.HandlerFunc{}, guard...), manage, h.availability)...)
	b := v1.Group("/reservations", guard...)
	b.GET("", manage, h.listReservations)
	b.POST("", manage, idempotency.Optional(), h.createReservation)
	b.GET("/:id", manage, h.getReservation)
	b.PATCH("/:id", manage, h.updateReservation)
	b.POST("/:id/confirm", manage, h.confirmReservation)
	b.POST("/:id/cancel", manage, h.cancelReservation)
	b.POST("/:id/no-show", manage, h.noShow)
}

func (h *Handler) listTypes(c *gin.Context) {
	out, err := h.svc.ListRoomTypes(c.Request.Context(), auth.From(c), c.Query("active") == "true")
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"room_types": out})
}

func (h *Handler) createType(c *gin.Context) {
	var in struct {
		Name          string `json:"name"`
		BaseRateCents int64  `json:"base_rate_cents"`
		Capacity      int    `json:"capacity"`
		TaxRateID     int64  `json:"tax_rate_id"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CreateRoomType(c.Request.Context(), auth.From(c), in.Name, in.BaseRateCents, in.Capacity, in.TaxRateID, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"room_type": out})
}

func (h *Handler) updateType(c *gin.Context) {
	id, ok := idParam(c, "room type")
	if !ok {
		return
	}
	var in service.RoomTypePatch
	if !body(c, &in) {
		return
	}
	out, err := h.svc.UpdateRoomType(c.Request.Context(), auth.From(c), id, in, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"room_type": out})
}

func (h *Handler) listRooms(c *gin.Context) {
	out, err := h.svc.ListRooms(c.Request.Context(), auth.From(c), c.Query("active") == "true")
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rooms": out})
}

func (h *Handler) createRoom(c *gin.Context) {
	var in struct {
		Number     string `json:"number"`
		RoomTypeID int64  `json:"room_type_id"`
		Floor      *int   `json:"floor"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CreateRoom(c.Request.Context(), auth.From(c), in.Number, in.RoomTypeID, in.Floor, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"room": out})
}

func (h *Handler) updateRoom(c *gin.Context) {
	id, ok := idParam(c, "room")
	if !ok {
		return
	}
	var raw map[string]json.RawMessage
	if !body(c, &raw) {
		return
	}
	var ch service.RoomPatch
	for k, v := range raw {
		var err error
		switch k {
		case "number":
			var s string
			err = json.Unmarshal(v, &s)
			ch.Number = &s
		case "room_type_id":
			var n int64
			err = json.Unmarshal(v, &n)
			ch.RoomTypeID = &n
		case "floor":
			if string(v) == "null" {
				ch.ClearFloor = true
			} else {
				var n int
				err = json.Unmarshal(v, &n)
				ch.Floor = &n
			}
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
	out, err := h.svc.UpdateRoom(c.Request.Context(), auth.From(c), id, ch, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"room": out})
}

func (h *Handler) listGuests(c *gin.Context) {
	before, ok := queryInt(c, "before_id")
	if !ok {
		return
	}
	limit, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	out, err := h.svc.ListGuests(c.Request.Context(), auth.From(c), strings.TrimSpace(c.Query("q")), before, int(limit))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"guests": out})
}

func (h *Handler) createGuest(c *gin.Context) {
	var in service.GuestInput
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CreateGuest(c.Request.Context(), auth.From(c), in, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"guest": out})
}

func (h *Handler) updateGuest(c *gin.Context) {
	id, ok := idParam(c, "guest")
	if !ok {
		return
	}
	var in service.GuestPatch
	if !body(c, &in) {
		return
	}
	out, err := h.svc.UpdateGuest(c.Request.Context(), auth.From(c), id, in, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"guest": out})
}

func (h *Handler) availability(c *gin.Context) {
	typeID, ok := queryInt(c, "room_type_id")
	if !ok {
		return
	}
	out, err := h.svc.Availability(c.Request.Context(), auth.From(c), c.Query("from"), c.Query("to"), typeID)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rooms": out})
}

func (h *Handler) listReservations(c *gin.Context) {
	status := c.Query("status")
	switch status {
	case "", "tentative", "confirmed", "checked_in", "checked_out", "cancelled", "no_show":
	default:
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "status", Message: "is not a booking status"})
		return
	}
	f := service.ReservationFilter{Status: status, From: c.Query("from"), To: c.Query("to")}
	var ok bool
	if f.RoomID, ok = queryInt(c, "room_id"); !ok {
		return
	}
	if f.GuestID, ok = queryInt(c, "guest_id"); !ok {
		return
	}
	if f.BeforeID, ok = queryInt(c, "before_id"); !ok {
		return
	}
	limit, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	f.Limit = int(limit)
	out, err := h.svc.ListReservations(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reservations": out})
}

func (h *Handler) getReservation(c *gin.Context) {
	id, ok := idParam(c, "reservation")
	if !ok {
		return
	}
	out, err := h.svc.GetReservation(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reservation": out})
}

func (h *Handler) createReservation(c *gin.Context) {
	var in service.ReservationInput
	if !body(c, &in) {
		return
	}
	out, replayed, err := h.svc.CreateReservation(c.Request.Context(), auth.From(c), in, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		replay(c, replayed)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"reservation": out})
}

func (h *Handler) updateReservation(c *gin.Context) {
	id, ok := idParam(c, "reservation")
	if !ok {
		return
	}
	var in service.ReservationPatch
	if !body(c, &in) {
		return
	}
	out, err := h.svc.UpdateReservation(c.Request.Context(), auth.From(c), id, in, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reservation": out})
}

func (h *Handler) confirmReservation(c *gin.Context) {
	id, ok := idParam(c, "reservation")
	if !ok {
		return
	}
	out, err := h.svc.ConfirmReservation(c.Request.Context(), auth.From(c), id, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reservation": out})
}

func (h *Handler) cancelReservation(c *gin.Context) {
	id, ok := idParam(c, "reservation")
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !body(c, &in) {
		return
	}
	out, err := h.svc.CancelReservation(c.Request.Context(), auth.From(c), id, in.Reason, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reservation": out})
}

func (h *Handler) noShow(c *gin.Context) {
	id, ok := idParam(c, "reservation")
	if !ok {
		return
	}
	out, err := h.svc.NoShow(c.Request.Context(), auth.From(c), id, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reservation": out})
}
