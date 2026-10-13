// Command migrate applies SQL migrations. Explicit operator command: `migrate up` or `migrate status`.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"zuscar/backend/internal/platform/config"
	"zuscar/backend/internal/platform/db"
	"zuscar/backend/internal/platform/migrate"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "up" && os.Args[1] != "status") {
		fmt.Fprintln(os.Stderr, "usage: migrate up | migrate status   (env: DATABASE_URL, optional MIGRATIONS_DIR)")
		os.Exit(2)
	}
	cfg, err := config.LoadCLI(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}
	dir := os.Getenv("MIGRATIONS_DIR")
	if dir == "" {
		dir = "database/migrations"
	}
	migs, err := migrate.Load(os.DirFS(dir), ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBSimpleProtocol)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()
	if os.Args[1] == "status" {
		st, err := migrate.StatusOf(ctx, pool, migs)
		for _, s := range st {
			mark := "pending"
			if s.Applied {
				mark = "applied"
			}
			fmt.Printf("%04d_%s  %s\n", s.Version, s.Name, mark)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	ran, err := migrate.Up(ctx, pool, migs)
	for _, v := range ran {
		fmt.Printf("applied %04d\n", v)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(ran) == 0 {
		fmt.Println("nothing to apply: database is up to date")
	}
}
