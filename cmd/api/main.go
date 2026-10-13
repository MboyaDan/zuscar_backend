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

	"zuscar/backend/internal/platform/config"
	"zuscar/backend/internal/platform/db"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/logging"
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
	if cfg.DatabaseURL != "" {
		pool, err := db.New(context.Background(), cfg.DatabaseURL, cfg.DBSimpleProtocol)
		if err != nil {
			log.Error("database setup failed", "error", err.Error())
			os.Exit(1)
		}
		defer pool.Close()
		pinger = pool
	} else {
		log.Warn("DATABASE_URL not set: /readyz will report not ready")
	}

	router, _ := zhttp.NewRouter(zhttp.Deps{
		Logger: log, DB: pinger, MaxRequestBytes: cfg.MaxRequestBytes, Production: cfg.AppEnv == config.EnvProduction,
	})
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
