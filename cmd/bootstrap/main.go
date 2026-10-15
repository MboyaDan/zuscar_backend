// Command bootstrap creates the first Super Admin on an empty production-grade database, plus the
// permission and role templates, business settings, tax rate and document counters. Nothing else.
// The password is typed at a prompt or piped with --password-stdin; it is never read from source,
// a default, or an environment variable. The new user must change it at first login.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"zuscar/backend/internal/platform/config"
	"zuscar/backend/internal/platform/db"
	"zuscar/backend/internal/provision"
)

func main() {
	var o provision.BootstrapOptions
	var stdin bool
	flag.StringVar(&o.Username, "username", "", "login name for the first Super Admin (required)")
	flag.StringVar(&o.DisplayName, "display-name", "", "full name shown in the app (required)")
	flag.StringVar(&o.BusinessName, "business-name", "", "business name for receipts (required)")
	flag.IntVar(&o.TaxRateBP, "tax-rate-bp", provision.ProvisionalStandardRateBP, "standard-rated VAT in basis points; the default 1600 (16.00%) is a PROVISIONAL development value, verify before go-live")
	flag.StringVar(&o.TaxName, "tax-name", "Standard rate", "label for the tax rate")
	flag.StringVar(&o.Timezone, "timezone", "Africa/Nairobi", "business timezone")
	flag.Int64Var(&o.ReceiptStart, "receipt-start", 1, "first receipt number (continue your receipt book)")
	flag.BoolVar(&stdin, "password-stdin", false, "read the password from standard input instead of prompting")
	flag.Parse()

	if o.Username == "" || o.DisplayName == "" || o.BusinessName == "" {
		fmt.Fprintln(os.Stderr, "missing required flags: --username --display-name --business-name")
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := config.LoadCLI(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}
	pw, err := readPassword(stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	o.Password = pw

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.New(ctx, cfg.DatabaseURL, cfg.DBSimpleProtocol)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()
	if _, err := provision.Bootstrap(ctx, pool, o); err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap failed:", err)
		os.Exit(1)
	}
	fmt.Printf("Created Super Admin %q (must change password at first login), roles, permissions, settings, tax rate and counters (first receipt number %d).\n", o.Username, o.ReceiptStart)
	fmt.Printf("\nTAX CONFIGURATION IS PROVISIONAL (standard rate %d basis points, categories: standard, zero_rated, exempt).\n", o.TaxRateBP)
	fmt.Println("It is not verified against your VAT registration or KRA requirements. Verify it with your accountant BEFORE go-live (docs/tax-configuration.md).")
	fmt.Println("This system does not integrate with eTIMS: its receipts are not KRA tax invoices.")
	fmt.Println("\nNext: sign in, change the password, then create real staff and enter the menu, rooms and stock through the app.")
}

func readPassword(fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password received on standard input")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal available: use --password-stdin")
	}
	fmt.Fprint(os.Stderr, "Password (min 12 characters): ")
	a, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passwords do not match")
	}
	return string(a), nil
}
