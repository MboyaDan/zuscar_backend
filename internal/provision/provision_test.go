package provision

import (
	"context"
	"strings"
	"testing"
)

// Validation runs before any database access, so these need no database.
func TestBootstrapValidatesBeforeTouchingDatabase(t *testing.T) {
	good := BootstrapOptions{Username: "owner.one", DisplayName: "Owner One", Password: "a-very-long-secret-1",
		BaseOptions: BaseOptions{BusinessName: "Venue", TaxRateBP: 1600, Timezone: "Africa/Nairobi", ReceiptStart: 1}}
	cases := map[string]func(o *BootstrapOptions){
		"bad username":      func(o *BootstrapOptions) { o.Username = "Owner One" },
		"empty display":     func(o *BootstrapOptions) { o.DisplayName = " " },
		"empty business":    func(o *BootstrapOptions) { o.BusinessName = "" },
		"tax too high":      func(o *BootstrapOptions) { o.TaxRateBP = 10001 },
		"bad timezone":      func(o *BootstrapOptions) { o.Timezone = "Mars/Base" },
		"receipt start 0":   func(o *BootstrapOptions) { o.ReceiptStart = 0 },
		"short password":    func(o *BootstrapOptions) { o.Password = "short" },
		"dev seed password": func(o *BootstrapOptions) { o.Password = KnownDevPassword },
		"password has user": func(o *BootstrapOptions) { o.Password = "xx-owner.one-xx-xx" },
	}
	for name, mutate := range cases {
		o := good
		mutate(&o)
		if _, err := Bootstrap(context.Background(), nil, o); err == nil {
			t.Errorf("%s: expected a validation error", name)
		} else if strings.Contains(err.Error(), "nil pointer") {
			t.Errorf("%s: validation must happen before database use: %v", name, err)
		}
	}
}
