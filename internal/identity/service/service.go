// Package service holds the authentication rules: login, sessions, lockout, password change, re-authentication
// and PIN fast-switch (brief section 6, ADR-04, ADR-13, A-07, S-16). Everything is decided from the database
// on each call; time comes from an injectable Clock so expiry and lockout are testable.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/identity/password"
	"zuscar/backend/internal/identity/repository"
	"zuscar/backend/internal/platform/auth"
)

type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type Config struct {
	IdleTTL       time.Duration
	AbsoluteTTL   time.Duration
	MaxAttempts   int           // failed logins (or PIN tries) before a lock
	LockoutWindow time.Duration // lock length, and the throttle window
	IPMaxAttempts int
	ReauthWindow  time.Duration // how long a password re-entry stays valid (S-16: 5 minutes)
	TouchInterval time.Duration // minimum gap between last-seen writes
}

var (
	ErrInvalidCredentials  = errors.New("invalid username or password")
	ErrUnauthenticated     = errors.New("not signed in")
	ErrReauthNeedsPassword = errors.New("re-authentication needs a password session")
	ErrPINNotAllowed       = errors.New("PINs are only for floor roles (cashier, waiter, bartender)")
	ErrInvalidPIN          = errors.New("PIN must be 4 to 8 digits and not trivially guessable")
	ErrSamePassword        = errors.New("the new password must differ from the current one")
)

// LockedError is returned while an account (or PIN) is locked. RetryAfter is the remaining time.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string { return "account is temporarily locked" }

// RateLimitedError is returned when an IP or a username has too many recent failures.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "too many attempts" }

// PolicyError carries a human-readable password policy failure.
type PolicyError struct{ Msg string }

func (e *PolicyError) Error() string { return e.Msg }

type ClientInfo struct {
	IP        *netip.Addr
	UserAgent string
	RequestID string
}

type Service struct {
	pool      *pgxpool.Pool
	cfg       Config
	clock     Clock
	dummyHash string // verified against for unknown users so response time does not reveal them
}

func New(pool *pgxpool.Pool, cfg Config, clock Clock) (*Service, error) {
	if clock == nil {
		clock = SystemClock{}
	}
	d, err := password.Hash("dummy-password-for-timing-only")
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, cfg: cfg, clock: clock, dummyHash: d}, nil
}

// Floor roles may use a PIN (A-07). Managers, Super Admin and Accountant never may.
var floorRoles = map[string]bool{"cashier": true, "waiter": true, "bartender": true}

type LoginResult struct {
	Token              string // session token, sent once in an HttpOnly cookie
	CSRFToken          string // sent once in a script-readable cookie
	ExpiresAt          time.Time
	UserID             int64
	Username           string
	DisplayName        string
	MustChangePassword bool
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func textOf(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func normalise(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

func newToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:], nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

type auditEvent struct {
	actorID          int64 // 0 = none
	label, action    string
	entityType       string
	entityID, reason string
	result           string // success | denied | failed
	before, after    map[string]any
}

func (s *Service) audit(ctx context.Context, q *repository.Queries, now time.Time, ci ClientInfo, e auditEvent) error {
	var before, after []byte
	if e.before != nil {
		before, _ = json.Marshal(e.before) // callers never put secrets in here
	}
	if e.after != nil {
		after, _ = json.Marshal(e.after)
	}
	if e.result == "" {
		e.result = "success"
	}
	return q.InsertAuditFull(ctx, repository.InsertAuditFullParams{
		OccurredAt: ts(now), ActorUserID: pgtype.Int8{Int64: e.actorID, Valid: e.actorID != 0}, ActorLabel: e.label,
		Action: e.action, EntityType: e.entityType, EntityID: textOf(e.entityID), Reason: textOf(e.reason),
		Result: e.result, BeforeJson: before, AfterJson: after, RequestID: textOf(ci.RequestID), Ip: ci.IP,
	})
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Login checks a username and password and starts a session.
func (s *Service) Login(ctx context.Context, username, plain string, ci ClientInfo) (*LoginResult, error) {
	now := s.clock.Now()
	norm := normalise(username)
	q := repository.New(s.pool)
	windowStart := now.Add(-s.cfg.LockoutWindow)

	if ci.IP != nil {
		if n, err := q.CountFailedAttemptsByIP(ctx, repository.CountFailedAttemptsByIPParams{Ip: ci.IP, AttemptedAt: ts(windowStart)}); err != nil {
			return nil, err
		} else if int(n) >= s.cfg.IPMaxAttempts {
			return nil, &RateLimitedError{RetryAfter: s.cfg.LockoutWindow}
		}
	}

	user, err := q.GetUserForLogin(ctx, norm)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = password.Verify(plain, s.dummyHash) // equalise timing
		return nil, s.failUnknown(ctx, q, now, ci, norm, windowStart)
	}
	if err != nil {
		return nil, err
	}
	if user.Status != "active" {
		_ = password.Verify(plain, s.dummyHash)
		return nil, s.failKnown(ctx, q, now, ci, user.ID, norm, "inactive")
	}
	if user.LockedUntil.Valid {
		if user.LockedUntil.Time.After(now) {
			// Locked: do not even check the password. Record the attempt for IP accounting only.
			_ = q.InsertLoginAttempt(ctx, repository.InsertLoginAttemptParams{UsernameNorm: norm, Ip: ci.IP, AttemptedAt: ts(now), Success: false})
			return nil, &LockedError{RetryAfter: user.LockedUntil.Time.Sub(now)}
		}
		if err := q.ClearExpiredLock(ctx, repository.ClearExpiredLockParams{ID: user.ID, LockedUntil: ts(now)}); err != nil {
			return nil, err
		}
	}
	if err := password.Verify(plain, user.PasswordHash); err != nil {
		return nil, s.failKnown(ctx, q, now, ci, user.ID, norm, "bad_password")
	}

	// Success: update the user, start the session, record the attempt and audit it in one transaction.
	newHash := ""
	if password.NeedsRehash(user.PasswordHash) {
		newHash, _ = password.Hash(plain)
	}
	var res *LoginResult
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.RecordSuccessfulLogin(ctx, repository.RecordSuccessfulLoginParams{ID: user.ID, LastLoginAt: ts(now)}); err != nil {
			return err
		}
		if newHash != "" {
			if err := tq.SetPassword(ctx, repository.SetPasswordParams{ID: user.ID, PasswordHash: newHash, MustChangePassword: user.MustChangePassword}); err != nil {
				return err
			}
		}
		r, err := s.startSession(ctx, tq, now, ci, user.ID, "password", now.Add(s.cfg.AbsoluteTTL))
		if err != nil {
			return err
		}
		r.Username, r.DisplayName, r.MustChangePassword = user.Username, user.DisplayName, user.MustChangePassword
		res = r
		if err := tq.InsertLoginAttempt(ctx, repository.InsertLoginAttemptParams{UsernameNorm: norm, Ip: ci.IP, AttemptedAt: ts(now), Success: true}); err != nil {
			return err
		}
		return s.audit(ctx, tq, now, ci, auditEvent{actorID: user.ID, label: user.Username, action: "auth.login_success", entityType: "user", entityID: fmt.Sprint(user.ID), after: map[string]any{"method": "password"}})
	})
	return res, err
}

func (s *Service) startSession(ctx context.Context, q *repository.Queries, now time.Time, ci ClientInfo, userID int64, method string, absolute time.Time) (*LoginResult, error) {
	tok, tokHash, err := newToken()
	if err != nil {
		return nil, err
	}
	csrf, csrfHash, err := newToken()
	if err != nil {
		return nil, err
	}
	idle := now.Add(s.cfg.IdleTTL)
	if idle.After(absolute) {
		idle = absolute
	}
	if _, err := q.CreateSession(ctx, repository.CreateSessionParams{
		UserID: userID, TokenHash: tokHash, CsrfHash: csrfHash, AuthMethod: method, CreatedAt: ts(now),
		IdleExpiresAt: ts(idle), AbsoluteExpiresAt: ts(absolute), Ip: ci.IP, UserAgent: textOf(clip(ci.UserAgent, 300)),
	}); err != nil {
		return nil, err
	}
	return &LoginResult{Token: tok, CSRFToken: csrf, ExpiresAt: absolute, UserID: userID}, nil
}

func (s *Service) failUnknown(ctx context.Context, q *repository.Queries, now time.Time, ci ClientInfo, norm string, windowStart time.Time) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.InsertLoginAttempt(ctx, repository.InsertLoginAttemptParams{UsernameNorm: norm, Ip: ci.IP, AttemptedAt: ts(now), Success: false}); err != nil {
			return err
		}
		return s.audit(ctx, tq, now, ci, auditEvent{label: "anonymous", action: "auth.login_failed", entityType: "user", entityID: clip(norm, 64), result: "failed", after: map[string]any{"reason": "unknown_user"}})
	})
	if err != nil {
		return err
	}
	if n, err := q.CountFailedAttemptsByUsername(ctx, repository.CountFailedAttemptsByUsernameParams{UsernameNorm: norm, AttemptedAt: ts(windowStart)}); err == nil && int(n) >= s.cfg.MaxAttempts {
		return &RateLimitedError{RetryAfter: s.cfg.LockoutWindow} // unknown names are throttled without revealing that they are unknown
	}
	return ErrInvalidCredentials
}

func (s *Service) failKnown(ctx context.Context, q *repository.Queries, now time.Time, ci ClientInfo, userID int64, norm, why string) error {
	var locked *LockedError
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.InsertLoginAttempt(ctx, repository.InsertLoginAttemptParams{UsernameNorm: norm, Ip: ci.IP, AttemptedAt: ts(now), Success: false}); err != nil {
			return err
		}
		if err := s.audit(ctx, tq, now, ci, auditEvent{actorID: 0, label: norm, action: "auth.login_failed", entityType: "user", entityID: fmt.Sprint(userID), result: "failed", after: map[string]any{"reason": why}}); err != nil {
			return err
		}
		if why != "bad_password" {
			return nil
		}
		row, err := tq.RecordFailedLogin(ctx, repository.RecordFailedLoginParams{MaxAttempts: int32(s.cfg.MaxAttempts), LockUntil: ts(now.Add(s.cfg.LockoutWindow)), ID: userID})
		if err != nil {
			return err
		}
		if row.LockedUntil.Valid && row.LockedUntil.Time.After(now) {
			locked = &LockedError{RetryAfter: row.LockedUntil.Time.Sub(now)}
			return s.audit(ctx, tq, now, ci, auditEvent{label: norm, action: "auth.account_locked", entityType: "user", entityID: fmt.Sprint(userID), result: "denied", after: map[string]any{"failed_attempts": row.FailedLoginCount}})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if locked != nil {
		return locked
	}
	return ErrInvalidCredentials
}

// Authenticate resolves a session token into a Principal, reading roles and permissions from the database now.
func (s *Service) Authenticate(ctx context.Context, token string) (*auth.Principal, error) {
	if token == "" || len(token) > 200 {
		return nil, ErrUnauthenticated
	}
	now := s.clock.Now()
	q := repository.New(s.pool)
	row, err := q.GetSessionByTokenHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if !row.IdleExpiresAt.Time.After(now) || !row.AbsoluteExpiresAt.Time.After(now) {
		_ = q.RevokeSession(ctx, repository.RevokeSessionParams{ID: row.SessionID, RevokedAt: ts(now), RevokeReason: textOf("expired")})
		return nil, ErrUnauthenticated
	}
	idle := row.IdleExpiresAt.Time
	if now.Sub(row.LastSeenAt.Time) >= s.cfg.TouchInterval {
		idle = now.Add(s.cfg.IdleTTL)
		if idle.After(row.AbsoluteExpiresAt.Time) {
			idle = row.AbsoluteExpiresAt.Time
		}
		if err := q.TouchSession(ctx, repository.TouchSessionParams{ID: row.SessionID, LastSeenAt: ts(now), IdleExpiresAt: ts(idle)}); err != nil {
			return nil, err
		}
	}
	roles, err := q.ListRolesForUser(ctx, row.UserID)
	if err != nil {
		return nil, err
	}
	keys, err := q.ListPermissionKeysForUser(ctx, row.UserID)
	if err != nil {
		return nil, err
	}
	p := &auth.Principal{
		UserID: row.UserID, Username: row.Username, DisplayName: row.DisplayName, SessionID: row.SessionID,
		AuthMethod: row.AuthMethod, MustChangePassword: row.MustChangePassword, CSRFHash: row.CsrfHash,
		AbsoluteExpiresAt: row.AbsoluteExpiresAt.Time, IdleExpiresAt: idle, Permissions: map[string]bool{},
	}
	if row.ReauthAt.Valid {
		t := row.ReauthAt.Time
		p.ReauthAt = &t
	}
	for _, r := range roles {
		p.Roles = append(p.Roles, r.Code)
	}
	for _, k := range keys {
		p.Permissions[k] = true
	}
	return p, nil
}

// CSRFMatches compares a presented CSRF token with the hash stored on the session, in constant time.
func CSRFMatches(p *auth.Principal, presented string) bool {
	if p == nil || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare(hashToken(presented), p.CSRFHash) == 1
}

func (s *Service) Logout(ctx context.Context, p *auth.Principal, ci ClientInfo) error {
	now := s.clock.Now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.RevokeSession(ctx, repository.RevokeSessionParams{ID: p.SessionID, RevokedAt: ts(now), RevokeReason: textOf("logout")}); err != nil {
			return err
		}
		return s.audit(ctx, tq, now, ci, auditEvent{actorID: p.UserID, label: p.Username, action: "auth.logout", entityType: "session", entityID: fmt.Sprint(p.SessionID)})
	})
}

// checkCurrentPassword verifies the signed-in user's password and counts a failure toward lockout.
func (s *Service) checkCurrentPassword(ctx context.Context, p *auth.Principal, plain string, ci ClientInfo) (repository.GetUserByIDRow, error) {
	now := s.clock.Now()
	q := repository.New(s.pool)
	user, err := q.GetUserByID(ctx, p.UserID)
	if err != nil {
		return user, err
	}
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(now) {
		return user, &LockedError{RetryAfter: user.LockedUntil.Time.Sub(now)}
	}
	if password.Verify(plain, user.PasswordHash) != nil {
		if err := s.failKnown(ctx, q, now, ci, user.ID, user.Username, "bad_password_reauth"); err != nil && !errors.Is(err, ErrInvalidCredentials) {
			return user, err
		}
		// failKnown only counts toward lockout for "bad_password"; count reauth failures the same way
		if row, err := q.RecordFailedLogin(ctx, repository.RecordFailedLoginParams{MaxAttempts: int32(s.cfg.MaxAttempts), LockUntil: ts(now.Add(s.cfg.LockoutWindow)), ID: user.ID}); err == nil && row.LockedUntil.Valid && row.LockedUntil.Time.After(now) {
			return user, &LockedError{RetryAfter: row.LockedUntil.Time.Sub(now)}
		}
		return user, ErrInvalidCredentials
	}
	return user, nil
}

// ChangePassword needs the current password, applies the policy, and ends every other session of the user.
func (s *Service) ChangePassword(ctx context.Context, p *auth.Principal, current, next string, ci ClientInfo) error {
	user, err := s.checkCurrentPassword(ctx, p, current, ci)
	if err != nil {
		return err
	}
	if current == next {
		return ErrSamePassword
	}
	if err := password.Policy(next, user.Username, password.DevSeedPassword); err != nil {
		return &PolicyError{Msg: err.Error()}
	}
	hash, err := password.Hash(next)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.SetPassword(ctx, repository.SetPasswordParams{ID: user.ID, PasswordHash: hash, MustChangePassword: false}); err != nil {
			return err
		}
		if err := tq.RevokeOtherSessions(ctx, repository.RevokeOtherSessionsParams{UserID: user.ID, ID: p.SessionID, RevokedAt: ts(now), RevokeReason: textOf("password_changed")}); err != nil {
			return err
		}
		return s.audit(ctx, tq, now, ci, auditEvent{actorID: user.ID, label: user.Username, action: "auth.password_changed", entityType: "user", entityID: fmt.Sprint(user.ID)})
	})
}

// Reauth records a fresh password entry on this session for sensitive actions (S-16). PIN sessions cannot do it.
func (s *Service) Reauth(ctx context.Context, p *auth.Principal, plain string, ci ClientInfo) error {
	if p.AuthMethod != "password" {
		return ErrReauthNeedsPassword
	}
	if _, err := s.checkCurrentPassword(ctx, p, plain, ci); err != nil {
		return err
	}
	now := s.clock.Now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.SetSessionReauth(ctx, repository.SetSessionReauthParams{ID: p.SessionID, ReauthAt: ts(now)}); err != nil {
			return err
		}
		return s.audit(ctx, tq, now, ci, auditEvent{actorID: p.UserID, label: p.Username, action: "auth.reauth", entityType: "session", entityID: fmt.Sprint(p.SessionID)})
	})
}

// RecentReauth reports whether p re-entered their password within the configured window.
func (s *Service) RecentReauth(p *auth.Principal) bool {
	return p.RecentlyReauthenticated(s.clock.Now(), s.cfg.ReauthWindow)
}

var pinDigits = regexp.MustCompile(`^[0-9]{4,8}$`)

func validPIN(pin string) bool {
	if !pinDigits.MatchString(pin) {
		return false
	}
	same, up, down := true, true, true
	for i := 1; i < len(pin); i++ {
		if pin[i] != pin[0] {
			same = false
		}
		if pin[i] != pin[i-1]+1 {
			up = false
		}
		if pin[i] != pin[i-1]-1 {
			down = false
		}
	}
	return !same && !up && !down
}

func onlyFloorRoles(roles []repository.ListRolesForUserRow) bool {
	if len(roles) == 0 {
		return false
	}
	for _, r := range roles {
		if !floorRoles[r.Code] {
			return false
		}
	}
	return true
}

// SetPIN stores a PIN for a floor-role user. Who may call it is decided by the Stage 7 admin endpoint; actor is
// only used for the audit trail (nil means a system action).
func (s *Service) SetPIN(ctx context.Context, actor *auth.Principal, targetUserID int64, pin string, ci ClientInfo) error {
	if !validPIN(pin) {
		return ErrInvalidPIN
	}
	q := repository.New(s.pool)
	roles, err := q.ListRolesForUser(ctx, targetUserID)
	if err != nil {
		return err
	}
	if !onlyFloorRoles(roles) {
		return ErrPINNotAllowed
	}
	hash, err := password.Hash(pin)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.UpsertPIN(ctx, repository.UpsertPINParams{UserID: targetUserID, PinHash: hash, UpdatedAt: ts(now)}); err != nil {
			return err
		}
		ev := auditEvent{label: "system", action: "auth.pin_set", entityType: "user", entityID: fmt.Sprint(targetUserID)}
		if actor != nil {
			ev.actorID, ev.label = actor.UserID, actor.Username
		}
		return s.audit(ctx, tq, now, ci, ev)
	})
}

// PINSwitch hands a signed-in terminal over to another floor user. The new session can never outlive the
// session it replaces (A-07), is marked as a PIN session, and cannot satisfy password re-entry.
func (s *Service) PINSwitch(ctx context.Context, current *auth.Principal, username, pin string, ci ClientInfo) (*LoginResult, error) {
	now := s.clock.Now()
	norm := normalise(username)
	q := repository.New(s.pool)
	windowStart := now.Add(-s.cfg.LockoutWindow)
	if ci.IP != nil {
		if n, err := q.CountFailedAttemptsByIP(ctx, repository.CountFailedAttemptsByIPParams{Ip: ci.IP, AttemptedAt: ts(windowStart)}); err != nil {
			return nil, err
		} else if int(n) >= s.cfg.IPMaxAttempts {
			return nil, &RateLimitedError{RetryAfter: s.cfg.LockoutWindow}
		}
	}
	generic := func() error {
		_ = password.Verify(pin, s.dummyHash)
		_ = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			tq := repository.New(tx)
			if err := tq.InsertLoginAttempt(ctx, repository.InsertLoginAttemptParams{UsernameNorm: norm, Ip: ci.IP, AttemptedAt: ts(now), Success: false}); err != nil {
				return err
			}
			return s.audit(ctx, tq, now, ci, auditEvent{actorID: current.UserID, label: current.Username, action: "auth.pin_failed", entityType: "user", entityID: clip(norm, 64), result: "failed"})
		})
		return ErrInvalidCredentials
	}
	user, err := q.GetUserForLogin(ctx, norm)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && user.Status != "active") {
		return nil, generic()
	}
	if err != nil {
		return nil, err
	}
	roles, err := q.ListRolesForUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if !onlyFloorRoles(roles) {
		return nil, generic() // managers and admins never use PINs, and we do not reveal who they are
	}
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(now) {
		return nil, &LockedError{RetryAfter: user.LockedUntil.Time.Sub(now)}
	}
	pinRow, err := q.GetPIN(ctx, user.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, generic()
	}
	if err != nil {
		return nil, err
	}
	if pinRow.LockedUntil.Valid {
		if pinRow.LockedUntil.Time.After(now) {
			return nil, &LockedError{RetryAfter: pinRow.LockedUntil.Time.Sub(now)}
		}
		if err := q.ClearExpiredPinLock(ctx, repository.ClearExpiredPinLockParams{UserID: user.ID, LockedUntil: ts(now)}); err != nil {
			return nil, err
		}
	}
	if password.Verify(pin, pinRow.PinHash) != nil {
		var locked *LockedError
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			tq := repository.New(tx)
			if err := tq.InsertLoginAttempt(ctx, repository.InsertLoginAttemptParams{UsernameNorm: norm, Ip: ci.IP, AttemptedAt: ts(now), Success: false}); err != nil {
				return err
			}
			row, err := tq.RecordPinFailure(ctx, repository.RecordPinFailureParams{MaxAttempts: int32(s.cfg.MaxAttempts), LockUntil: ts(now.Add(s.cfg.LockoutWindow)), UserID: user.ID})
			if err != nil {
				return err
			}
			ev := auditEvent{actorID: current.UserID, label: current.Username, action: "auth.pin_failed", entityType: "user", entityID: fmt.Sprint(user.ID), result: "failed"}
			if row.LockedUntil.Valid && row.LockedUntil.Time.After(now) {
				locked = &LockedError{RetryAfter: row.LockedUntil.Time.Sub(now)}
				ev.action, ev.result = "auth.pin_locked", "denied"
			}
			return s.audit(ctx, tq, now, ci, ev)
		})
		if err != nil {
			return nil, err
		}
		if locked != nil {
			return nil, locked
		}
		return nil, ErrInvalidCredentials
	}

	absolute := now.Add(s.cfg.AbsoluteTTL)
	if current.AbsoluteExpiresAt.Before(absolute) {
		absolute = current.AbsoluteExpiresAt
	}
	var res *LoginResult
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := repository.New(tx)
		if err := tq.ResetPinFailures(ctx, user.ID); err != nil {
			return err
		}
		if err := tq.RevokeSession(ctx, repository.RevokeSessionParams{ID: current.SessionID, RevokedAt: ts(now), RevokeReason: textOf("pin_switch")}); err != nil {
			return err
		}
		r, err := s.startSession(ctx, tq, now, ci, user.ID, "pin", absolute)
		if err != nil {
			return err
		}
		r.Username, r.DisplayName, r.MustChangePassword = user.Username, user.DisplayName, user.MustChangePassword
		res = r
		if err := tq.InsertLoginAttempt(ctx, repository.InsertLoginAttemptParams{UsernameNorm: norm, Ip: ci.IP, AttemptedAt: ts(now), Success: true}); err != nil {
			return err
		}
		return s.audit(ctx, tq, now, ci, auditEvent{actorID: user.ID, label: user.Username, action: "auth.pin_switch", entityType: "user", entityID: fmt.Sprint(user.ID),
			after: map[string]any{"from_user_id": current.UserID, "method": "pin"}})
	})
	return res, err
}
