// Package auth defines the authenticated caller (Principal) that the identity module puts on each request
// and every other module reads. Authorisation is always decided from the database on each request (ADR-04):
// Permissions here were loaded for this request, never copied from a token.
package auth

import (
	"time"

	"github.com/gin-gonic/gin"
)

type Principal struct {
	UserID             int64
	Username           string
	DisplayName        string
	SessionID          int64
	AuthMethod         string // "password" or "pin"
	MustChangePassword bool
	ReauthAt           *time.Time
	Roles              []string
	Permissions        map[string]bool
	CSRFHash           []byte    // SHA-256 of this session's CSRF token (never the token itself)
	AbsoluteExpiresAt  time.Time // the session's hard end; a PIN hand-over can never outlive it
	IdleExpiresAt      time.Time
}

func (p *Principal) Has(permission string) bool { return p != nil && p.Permissions[permission] }

// RecentlyReauthenticated reports whether the user re-entered their password within window (S-16).
func (p *Principal) RecentlyReauthenticated(now time.Time, window time.Duration) bool {
	return p != nil && p.AuthMethod == "password" && p.ReauthAt != nil && now.Sub(*p.ReauthAt) <= window
}

const key = "zuscar.principal"

func Set(c *gin.Context, p *Principal) { c.Set(key, p) }

// From returns the caller, or nil when the request is not authenticated.
func From(c *gin.Context) *Principal {
	if v, ok := c.Get(key); ok {
		if p, ok := v.(*Principal); ok {
			return p
		}
	}
	return nil
}
