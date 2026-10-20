// Package handler exposes the reports module over HTTP (docs/api/routes.csv, Stage 25).
package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/reports/service"
)

type Handler struct {
	svc *service.Service
	log *slog.Logger
}

func New(svc *service.Service, log *slog.Logger) *Handler { return &Handler{svc: svc, log: log} }

func (h *Handler) Register(v1 *gin.RouterGroup, guard ...gin.HandlerFunc) {
	g := v1.Group("/reports", guard...)
	view := func(c *gin.Context) {
		if !auth.From(c).Has("reports.view") {
			zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
			return
		}
		c.Next()
	}
	for _, name := range service.Names {
		g.GET("/"+name, view, h.report(name))
	}
}

func (h *Handler) report(name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := auth.From(c)
		from, to := c.Query("from"), c.Query("to")
		format := strings.ToLower(c.Query("format"))
		if format != "" && format != "json" && format != "csv" {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request is not valid.", zhttp.FieldError{Field: "format", Message: "must be json or csv"})
			return
		}
		var (
			r   service.Report
			err error
		)
		if format == "csv" {
			m := service.Meta{RequestID: zhttp.RequestIDFrom(c)}
			if ip, perr := netip.ParseAddr(c.ClientIP()); perr == nil {
				m.IP = &ip
			}
			r, err = h.svc.Export(c.Request.Context(), actor, name, from, to, m)
		} else {
			r, err = h.svc.Build(c.Request.Context(), actor, name, from, to)
		}
		if err != nil {
			h.fail(c, err)
			return
		}
		if format == "csv" {
			file := "zuscar-" + name
			if r.From != "" {
				file += "-" + r.From + "-to-" + r.To
			}
			c.Header("Content-Type", "text/csv; charset=utf-8")
			c.Header("Content-Disposition", `attachment; filename="`+file+`.csv"`)
			c.Header("Cache-Control", "no-store")
			c.Status(http.StatusOK)
			if err := service.WriteCSV(c.Writer, r); err != nil {
				h.log.Error("report csv", "report", name, "error", err)
			}
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"report": r})
	}
}

func (h *Handler) fail(c *gin.Context, err error) {
	var ve *service.ValidationError
	var nf *service.NotFoundError
	switch {
	case errors.As(err, &ve):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request is not valid.", zhttp.FieldError{Field: ve.Field, Message: ve.Msg})
	case errors.As(err, &nf):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That report does not exist.")
	case errors.Is(err, service.ErrForbidden):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	default:
		h.log.Error("reports", "error", err)
		zhttp.WriteError(c, http.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
	}
}
