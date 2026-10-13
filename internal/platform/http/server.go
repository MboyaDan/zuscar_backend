package http

import (
	"context"
	"errors"
	"log/slog"
	nethttp "net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Pinger reports whether a dependency (the database) is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Deps struct {
	Logger          *slog.Logger
	DB              Pinger
	MaxRequestBytes int64
	Production      bool
}

// NewRouter builds the engine with the middleware chain from architecture.md section 4.
// Domain modules register their routes on the returned group in later stages.
func NewRouter(d Deps) (*gin.Engine, *gin.RouterGroup) {
	if d.Production {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.HandleMethodNotAllowed = true
	_ = r.SetTrustedProxies(nil) // real client IP handling is decided with rate limiting (Stage 6)
	r.Use(RequestID(), AccessLog(d.Logger), Recovery(d.Logger), SecurityHeaders(), BodyLimit(d.MaxRequestBytes))
	r.NoRoute(notFound)
	r.NoMethod(methodNotAllowed)

	r.GET("/healthz", func(c *gin.Context) { c.JSON(nethttp.StatusOK, gin.H{"status": "ok"}) })
	// Reachable through the Next.js /api rewrite, so the web app can show a connection indicator (Stage 8).
	// Liveness only: it says nothing about the database and exposes no configuration.
	r.GET("/api/v1/health", func(c *gin.Context) { c.JSON(nethttp.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if d.DB == nil || d.DB.Ping(ctx) != nil {
			WriteError(c, nethttp.StatusServiceUnavailable, "not_ready", "The database is not reachable.")
			return
		}
		c.JSON(nethttp.StatusOK, gin.H{"status": "ready"})
	})
	return r, r.Group("/api/v1")
}

// Serve runs srv until ctx is cancelled, then shuts down gracefully.
func Serve(ctx context.Context, srv *nethttp.Server, grace time.Duration, l *slog.Logger) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, nethttp.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	l.Info("shutting down", "grace", grace.String())
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return srv.Shutdown(sctx)
}
