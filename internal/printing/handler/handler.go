// Package handler exposes the printing module over HTTP (docs/api/route-plan.md, Stage 14).
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
	"zuscar/backend/internal/printing/service"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

// Register mounts the routes. Every route needs a signed-in user; each document type then checks its own permission
// in the service (receipt: sales.view, kitchen ticket: kitchen.view, bill: sales.create, test page: any signed-in user).
func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	p := v1.Group("/print", guard...)
	p.GET("/documents/receipt/:id", h.document(service.DocReceipt, "sale"))
	p.GET("/documents/ticket/:id", h.document(service.DocTicket, "kitchen ticket"))
	p.GET("/documents/bill/:id", h.document(service.DocBill, "order"))
	p.GET("/test-page", h.testPage)
	p.POST("/jobs", idempotency.Optional(), h.createJob)
	p.GET("/jobs", h.listJobs)
	p.GET("/jobs/:id", h.getJob)
	p.POST("/jobs/:id/attempts", h.attempt)
	p.POST("/jobs/:id/retry", h.retry)
	p.POST("/jobs/:id/reprint", h.reprint)
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

func (h *Handler) document(docType, what string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := idParam(c, what)
		if !ok {
			return
		}
		doc, err := h.svc.Document(c.Request.Context(), auth.From(c), docType, id)
		if err != nil {
			h.fail(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"document": doc})
	}
}

func (h *Handler) testPage(c *gin.Context) {
	doc, err := h.svc.Document(c.Request.Context(), auth.From(c), service.DocTest, 0)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"document": doc})
}

func (h *Handler) createJob(c *gin.Context) {
	var in struct {
		DocType string `json:"doc_type"`
		RefID   int64  `json:"ref_id"`
	}
	if !body(c, &in) {
		return
	}
	replayed, job, created, err := h.svc.CreateJob(c.Request.Context(), auth.From(c), service.CreateInput{DocType: in.DocType, RefID: in.RefID}, idempotency.From(c), meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	if replayed != nil {
		c.Header(idempotency.ReplayHeader, "true")
		c.Data(replayed.Status, "application/json; charset=utf-8", replayed.Body)
		return
	}
	status := http.StatusOK // an existing first print of this receipt or ticket
	if created {
		status = http.StatusCreated
		c.Header("Location", "/api/v1/print/jobs/"+strconv.FormatInt(job.ID, 10))
	}
	c.JSON(status, gin.H{"job": job, "created": created})
}

func (h *Handler) getJob(c *gin.Context) {
	id, ok := idParam(c, "print job")
	if !ok {
		return
	}
	job, err := h.svc.GetJob(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
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

func (h *Handler) listJobs(c *gin.Context) {
	var f service.JobFilter
	var ok bool
	if f.Status, ok = oneOf(c, "status", "queued", "printed", "failed", "reprinted"); !ok {
		return
	}
	if f.DocType, ok = oneOf(c, "doc_type", service.DocReceipt, service.DocTicket, service.DocBill, service.DocTest); !ok {
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
	items, more, err := h.svc.ListJobs(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.fail(c, err)
		return
	}
	res := gin.H{"jobs": items}
	if more {
		res["next_cursor"] = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) attempt(c *gin.Context) {
	id, ok := idParam(c, "print job")
	if !ok {
		return
	}
	var in struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if !body(c, &in) {
		return
	}
	job, err := h.svc.RecordAttempt(c.Request.Context(), auth.From(c), id, in.Outcome, in.Note, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}

func (h *Handler) retry(c *gin.Context) {
	id, ok := idParam(c, "print job")
	if !ok {
		return
	}
	job, err := h.svc.Retry(c.Request.Context(), auth.From(c), id, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}

func (h *Handler) reprint(c *gin.Context) {
	id, ok := idParam(c, "print job")
	if !ok {
		return
	}
	job, err := h.svc.Reprint(c.Request.Context(), auth.From(c), id, meta(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Location", "/api/v1/print/jobs/"+strconv.FormatInt(job.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"job": job})
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
	case errors.Is(err, idempotency.ErrKeyReused):
		zhttp.WriteError(c, http.StatusUnprocessableEntity, "idempotency_key_reused", "This Idempotency-Key was already used for a different request. Use a new key for a new action.")
	case errors.Is(err, idempotency.ErrInFlight):
		c.Header("Retry-After", "1")
		zhttp.WriteError(c, http.StatusConflict, "idempotency_conflict", "A request with this Idempotency-Key is still being processed. Try again in a moment.")
	case errors.As(err, &cf):
		zhttp.WriteError(c, http.StatusConflict, cf.Code, cf.Msg)
	case errors.As(err, &verr):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: verr.Field, Message: verr.Msg})
	default:
		h.log.Error("printing error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
		zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}
