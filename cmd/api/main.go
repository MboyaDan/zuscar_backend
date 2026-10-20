// Command api is the Zuscar HTTP API. It never runs migrations or seeds on start.
package main

import (
	"context"
	"fmt"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	cathandler "zuscar/backend/internal/catalogue/handler"
	catservice "zuscar/backend/internal/catalogue/service"
	idhandler "zuscar/backend/internal/identity/handler"
	idservice "zuscar/backend/internal/identity/service"
	invhandler "zuscar/backend/internal/inventory/handler"
	invservice "zuscar/backend/internal/inventory/service"
	orderhandler "zuscar/backend/internal/orders/handler"
	orderservice "zuscar/backend/internal/orders/service"
	"zuscar/backend/internal/platform/config"
	"zuscar/backend/internal/platform/db"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/logging"
	printhandler "zuscar/backend/internal/printing/handler"
	printservice "zuscar/backend/internal/printing/service"
	purhandler "zuscar/backend/internal/purchasing/handler"
	purservice "zuscar/backend/internal/purchasing/service"
	reporthandler "zuscar/backend/internal/reports/handler"
	reportservice "zuscar/backend/internal/reports/service"
	roomhandler "zuscar/backend/internal/rooms/handler"
	roomservice "zuscar/backend/internal/rooms/service"
	saleshandler "zuscar/backend/internal/sales/handler"
	salesservice "zuscar/backend/internal/sales/service"
	sethandler "zuscar/backend/internal/settings/handler"
	setservice "zuscar/backend/internal/settings/service"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:\n"+err.Error())
		os.Exit(2)
	}
	log := logging.New(os.Stdout, cfg.LogLevel)

	var pinger zhttp.Pinger
	var authReady func(*gin.RouterGroup) error
	if cfg.DatabaseURL != "" {
		pool, err := db.New(context.Background(), cfg.DatabaseURL, cfg.DBSimpleProtocol)
		if err != nil {
			log.Error("database setup failed", "error", err.Error())
			os.Exit(1)
		}
		defer pool.Close()
		pinger = pool
		authReady = func(v1 *gin.RouterGroup) error {
			svc, err := idservice.New(pool, idservice.Config{
				IdleTTL: cfg.SessionIdleTTL, AbsoluteTTL: cfg.SessionAbsoluteTTL, MaxAttempts: cfg.LoginMaxAttempts,
				LockoutWindow: cfg.LoginLockoutWindow, IPMaxAttempts: cfg.LoginIPMaxAttempts,
				ReauthWindow: 5 * time.Minute, TouchInterval: time.Minute,
			}, nil)
			if err != nil {
				return err
			}
			h, err := idhandler.New(svc, cfg.FrontendURL, cfg.CookieSecure, log)
			if err != nil {
				return err
			}
			h.Register(v1)
			h.RegisterAdmin(v1)
			sethandler.New(setservice.New(pool, svc.Clock(), svc, svc.RecentReauth), log).Register(v1, h.Authenticate(false), h.CSRF())
			cathandler.New(catservice.New(pool, svc), log).Register(v1, h.Authenticate(false), h.CSRF())
			settings := setservice.New(pool, svc.Clock(), svc, svc.RecentReauth)
			orders := orderservice.New(pool, svc)
			orderhandler.New(orders, log).Register(v1, h.Authenticate(false), h.CSRF())
			inventory := invservice.New(pool, svc, settings)
			invhandler.New(inventory, log).Register(v1, h.Authenticate(false), h.CSRF())
			purhandler.New(purservice.New(pool, svc, inventory), log).Register(v1, h.Authenticate(false), h.CSRF())
			sales := salesservice.New(pool, svc, orders, settings, settings, inventory)
			saleshandler.New(sales, log).Register(v1, h.Authenticate(false), h.CSRF())
			roomhandler.New(roomservice.New(pool, svc, sales), log).Register(v1, h.Authenticate(false), h.CSRF())
			reporthandler.New(reportservice.New(pool, svc, settings), log).Register(v1, h.Authenticate(false), h.CSRF())
			printhandler.New(printservice.New(pool, svc, sales, orders, settings), log).Register(v1, h.Authenticate(false), h.CSRF())
			return nil
		}
	} else {
		log.Warn("DATABASE_URL not set: /readyz will report not ready")
	}

	router, v1 := zhttp.NewRouter(zhttp.Deps{
		Logger: log, DB: pinger, MaxRequestBytes: cfg.MaxRequestBytes, Production: cfg.AppEnv == config.EnvProduction,
		TrustedProxies: cfg.TrustedProxies,
	})
	if authReady != nil {
		if err := authReady(v1); err != nil {
			log.Error("authentication setup failed", "error", err.Error())
			os.Exit(1)
		}
	} else {
		log.Warn("authentication routes are disabled because DATABASE_URL is not set")
	}
	srv := &nethttp.Server{
		Addr: ":" + cfg.Port, Handler: router,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("api starting", "port", cfg.Port, "env", string(cfg.AppEnv))
	if err := zhttp.Serve(ctx, srv, 15*time.Second, log); err != nil {
		log.Error("server error", "error", err.Error())
		os.Exit(1)
	}
	log.Info("api stopped")
}

// healthcheck lets a distroless container probe itself (no curl in the image).
func healthcheck() int {
	port := os.Getenv("API_PORT")
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "8080"
	}
	c := nethttp.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}
