package http

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	nethttp "net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	RequestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// RequestID accepts a well-formed inbound ID (so Vercel and the API share one trace) or mints one.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			b := make([]byte, 12)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		c.Set(requestIDKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

func RequestIDFrom(c *gin.Context) string {
	if v, ok := c.Get(requestIDKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// AccessLog writes one structured line per request. It logs the path only, never the query
// string, headers, cookies or bodies, so tokens and secrets cannot leak.
func AccessLog(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		l.LogAttrs(c.Request.Context(), slog.LevelInfo, "request",
			slog.String("request_id", RequestIDFrom(c)),
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int("bytes", c.Writer.Size()),
		)
	}
}

// Recovery turns a panic into a 500 with the standard body and logs it without request data.
func Recovery(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				l.Error("panic recovered", "request_id", RequestIDFrom(c), "panic", rec)
				WriteError(c, nethttp.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
			}
		}()
		c.Next()
	}
}

// BodyLimit caps request bodies; oversize reads fail with a 413-mapped error in handlers.
func BodyLimit(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > max {
			WriteError(c, nethttp.StatusRequestEntityTooLarge, "request_too_large", "The request is too large.")
			return
		}
		c.Request.Body = nethttp.MaxBytesReader(c.Writer, c.Request.Body, max)
		c.Next()
	}
}

// SecurityHeaders sets conservative defaults for API responses.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		c.Next()
	}
}
