// Command seed-dev loads fictional development data. It refuses to run when APP_ENV=production.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"zuscar/backend/internal/devseed"
	"zuscar/backend/internal/platform/config"
	"zuscar/backend/internal/platform/db"
)

func main() {
	cfg, err := config.LoadCLI(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}
	if err := devseed.Guard(cfg.AppEnv); err != nil { // before any connection is opened
		fmt.Fprintln(os.Stderr, "REFUSED:", err)
		os.Exit(3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBSimpleProtocol)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()
	n, err := devseed.Run(ctx, pool, cfg.AppEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed-dev failed:", err)
		os.Exit(1)
	}
	fmt.Printf("seed-dev: created %d fictional users (all dev.*). Dev-only password for every one: %s\n", n, devseed.Password)
	fmt.Println("A small fake menu and six dining tables are added when the database has no categories yet (SKUs start with DEV-).")
	fmt.Println("This data is FAKE. Never use it in production.")
}
