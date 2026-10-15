// Package handler exposes the authentication endpoints and the middleware other modules use:
// Authenticate (session to Principal), CSRF protection and recent re-authentication.
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/identity/service"
	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
)

const (
	SessionCookie = "zuscar_session"
	CSRFCookie    = "zuscar_csrf"
	CSRFHeader    = "X-CSRF-Token"
)

type Handler struct {
	svc          *service.Service
	origin       string // normalised scheme://host[:port] of the web app
	cookieSecure bool
	log          *slog.Logger
}

func New(svc *service.Service, frontendURL string, cookieSecure bool, log *slog.Logger) (*Handler, error) {
	u, err := url.Parse(frontendURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("FRONTEND_URL must be an absolute URL such as http://localhost:3000")
	}
	return &Handler{svc: svc, origin: u.Scheme + "://" + u.Host, cookieSecure: cookieSecure, log: log}, nil
}

// Register mounts /auth routes on the /api/v1 group.
func (h *Handler) Register(v1 *gin.RouterGroup) {
	v1.POST("/auth/login", h.originCheck(), h.login)
	a := v1.Group("/auth", h.Authenticate(true), h.CSRF())
	a.GET("/me", h.me)
	a.POST("/logout", h.logout)
	a.POST("/change-password", h.changePassword)
	a.POST("/reauth", h.reauth)
	a.POST("/pin-switch", h.pinSwitch)
}

// ---- middleware ----

func (h *Handler) originOK(c *gin.Context) bool {
	if o := c.GetHeader("Origin"); o != "" {
		return o == h.origin
	}
	if ref := c.GetHeader("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Scheme+"://"+u.Host == h.origin
	}
	return true // no browser provenance (scripts, curl): the CSRF token check still applies when signed in
}

func (h *Handler) originCheck() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.originOK(c) {
			zhttp.WriteError(c, http.StatusForbidden, "csrf_failed", "The request origin is not allowed.")
			return
		}
		c.Next()
	}
}

// Authenticate turns the session cookie into a Principal. Routes that must stay reachable while a forced
// password change is pending pass allowPasswordChangePending = true.
func (h *Handler) Authenticate(allowPasswordChangePending bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		tok, _ := c.Cookie(SessionCookie)
		p, err := h.svc.Authenticate(c.Request.Context(), tok)
		if errors.Is(err, service.ErrUnauthenticated) {
			h.clearCookies(c)
			zhttp.WriteError(c, http.StatusUnauthorized, "unauthenticated", "Please sign in.")
			return
		}
		if err != nil {
			h.internal(c, err)
			return
		}
		if p.MustChangePassword && !allowPasswordChangePending {
			zhttp.WriteError(c, http.StatusForbidden, "password_change_required", "You must change your password before continuing.")
			return
		}
		auth.Set(c, p)
		c.Next()
	}
}

// CSRF protects state-changing methods: the Origin (or Referer) must be the web app, and the X-CSRF-Token header
// must equal the CSRF cookie and match the hash held on this session (ADR-13).
func (h *Handler) CSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		fail := func() {
			zhttp.WriteError(c, http.StatusForbidden, "csrf_failed", "The request could not be verified. Reload the page and try again.")
		}
		if !h.originOK(c) {
			fail()
			return
		}
		header := c.GetHeader(CSRFHeader)
		cookie, err := c.Cookie(CSRFCookie)
		if err != nil || header == "" || header != cookie || !service.CSRFMatches(auth.From(c), header) {
			fail()
			return
		}
		c.Next()
	}
}

// RequireRecentReauth blocks sensitive actions unless the user re-entered their password in the last few minutes (S-16).
func (h *Handler) RequireRecentReauth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.svc.RecentReauth(auth.From(c)) {
			zhttp.WriteError(c, http.StatusForbidden, "reauth_required", "Please confirm your password to continue.")
			return
		}
		c.Next()
	}
}

// ---- endpoints ----

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func decode(c *gin.Context, dst any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request body is not valid JSON.", zhttp.FieldError{Field: "body", Message: "invalid or unexpected JSON"})
		return false
	}
	return true
}

func required(c *gin.Context, fields map[string]string, maxLen int) bool {
	var bad []zhttp.FieldError
	for name, v := range fields {
		if strings.TrimSpace(v) == "" {
			bad = append(bad, zhttp.FieldError{Field: name, Message: "is required"})
		} else if len(v) > maxLen {
			bad = append(bad, zhttp.FieldError{Field: name, Message: "is too long"})
		}
	}
	if len(bad) > 0 {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", bad...)
		return false
	}
	return true
}

func clientInfo(c *gin.Context) service.ClientInfo {
	ci := service.ClientInfo{UserAgent: c.GetHeader("User-Agent"), RequestID: zhttp.RequestIDFrom(c)}
	if ip, err := netip.ParseAddr(c.ClientIP()); err == nil {
		ci.IP = &ip
	}
	return ci
}

func (h *Handler) login(c *gin.Context) {
	var in credentials
	if !decode(c, &in) || !required(c, map[string]string{"username": in.Username, "password": in.Password}, 1024) {
		return
	}
	res, err := h.svc.Login(c.Request.Context(), in.Username, in.Password, clientInfo(c))
	if err != nil {
		h.authError(c, err)
		return
	}
	h.setCookies(c, res)
	c.JSON(http.StatusOK, gin.H{
		"user":                 gin.H{"id": res.UserID, "username": res.Username, "display_name": res.DisplayName},
		"must_change_password": res.MustChangePassword,
		"expires_at":           res.ExpiresAt,
	})
}

func (h *Handler) me(c *gin.Context) {
	p := auth.From(c)
	perms := make([]string, 0, len(p.Permissions))
	for k := range p.Permissions {
		perms = append(perms, k)
	}
	sortStrings(perms)
	c.JSON(http.StatusOK, gin.H{
		"user":                 gin.H{"id": p.UserID, "username": p.Username, "display_name": p.DisplayName},
		"roles":                p.Roles,
		"permissions":          perms,
		"must_change_password": p.MustChangePassword,
		"auth_method":          p.AuthMethod,
		"session":              gin.H{"idle_expires_at": p.IdleExpiresAt, "absolute_expires_at": p.AbsoluteExpiresAt, "recently_reauthenticated": h.svc.RecentReauth(p)},
	})
}

func (h *Handler) logout(c *gin.Context) {
	if err := h.svc.Logout(c.Request.Context(), auth.From(c), clientInfo(c)); err != nil {
		h.internal(c, err)
		return
	}
	h.clearCookies(c)
	c.Status(http.StatusNoContent)
}

func (h *Handler) changePassword(c *gin.Context) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"current_password": in.CurrentPassword, "new_password": in.NewPassword}, 1024) {
		return
	}
	if err := h.svc.ChangePassword(c.Request.Context(), auth.From(c), in.CurrentPassword, in.NewPassword, clientInfo(c)); err != nil {
		h.authError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) reauth(c *gin.Context) {
	var in struct {
		Password string `json:"password"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"password": in.Password}, 1024) {
		return
	}
	if err := h.svc.Reauth(c.Request.Context(), auth.From(c), in.Password, clientInfo(c)); err != nil {
		h.authError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) pinSwitch(c *gin.Context) {
	var in struct {
		Username string `json:"username"`
		PIN      string `json:"pin"`
	}
	if p := auth.From(c); p.MustChangePassword {
		zhttp.WriteError(c, http.StatusForbidden, "password_change_required", "You must change your password before continuing.")
		return
	}
	if !decode(c, &in) || !required(c, map[string]string{"username": in.Username, "pin": in.PIN}, 64) {
		return
	}
	res, err := h.svc.PINSwitch(c.Request.Context(), auth.From(c), in.Username, in.PIN, clientInfo(c))
	if err != nil {
		h.authError(c, err)
		return
	}
	h.setCookies(c, res)
	c.JSON(http.StatusOK, gin.H{
		"user":                 gin.H{"id": res.UserID, "username": res.Username, "display_name": res.DisplayName},
		"must_change_password": res.MustChangePassword,
		"expires_at":           res.ExpiresAt,
	})
}

// ---- helpers ----

func (h *Handler) authError(c *gin.Context, err error) {
	var locked *service.LockedError
	var limited *service.RateLimitedError
	var policy *service.PolicyError
	switch {
	case errors.As(err, &locked):
		c.Header("Retry-After", seconds(locked.RetryAfter))
		zhttp.WriteError(c, http.StatusLocked, "account_locked", "Too many failed attempts. Try again later.")
	case errors.As(err, &limited):
		c.Header("Retry-After", seconds(limited.RetryAfter))
		zhttp.WriteError(c, http.StatusTooManyRequests, "rate_limited", "Too many attempts. Try again later.")
	case errors.Is(err, service.ErrInvalidCredentials):
		zhttp.WriteError(c, http.StatusUnauthorized, "invalid_credentials", "Invalid username or password.")
	case errors.As(err, &policy):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The new password is not acceptable.", zhttp.FieldError{Field: "new_password", Message: policy.Msg})
	case errors.Is(err, service.ErrSamePassword):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The new password is not acceptable.", zhttp.FieldError{Field: "new_password", Message: err.Error()})
	case errors.Is(err, service.ErrReauthNeedsPassword):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "Sign in with your password to do this.")
	default:
		h.internal(c, err)
	}
}

func (h *Handler) internal(c *gin.Context, err error) {
	h.log.Error("auth error", "request_id", zhttp.RequestIDFrom(c), "error", err.Error())
	zhttp.WriteError(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
}

func seconds(d time.Duration) string { return strconv.Itoa(int(math.Ceil(d.Seconds()))) }

func (h *Handler) setCookies(c *gin.Context, r *service.LoginResult) {
	http.SetCookie(c.Writer, &http.Cookie{Name: SessionCookie, Value: r.Token, Path: "/", Expires: r.ExpiresAt, HttpOnly: true, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(c.Writer, &http.Cookie{Name: CSRFCookie, Value: r.CSRFToken, Path: "/", Expires: r.ExpiresAt, HttpOnly: false, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode})
}

func (h *Handler) clearCookies(c *gin.Context) {
	for _, n := range []string{SessionCookie, CSRFCookie} {
		http.SetCookie(c.Writer, &http.Cookie{Name: n, Value: "", Path: "/", MaxAge: -1, HttpOnly: n == SessionCookie, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode})
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
