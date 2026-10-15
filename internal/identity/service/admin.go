package service

// User administration (Stage 7): create, edit, roles, deactivate, reactivate, password reset, PIN and delete.
// Every method checks the caller's permission itself (the route middleware is a second layer, AC-10), applies the
// protected rules from docs/rbac.md section 3 and writes an audit event with before and after values in the same
// transaction as the change (NN-08). Sensitive changes need a password re-entered in the last 5 minutes (S-16).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/identity/password"
	"zuscar/backend/internal/identity/repository"
	"zuscar/backend/internal/platform/audit"
	"zuscar/backend/internal/platform/auth"
)

var (
	ErrForbidden       = errors.New("you do not have permission to do this")
	ErrReauthRequired  = errors.New("confirm your password to continue")
	ErrUserNotFound    = errors.New("user not found")
	ErrUsernameTaken   = errors.New("that username is already in use")
	ErrProtectedTarget = errors.New("only a system administrator can change this account")
	ErrEscalation      = errors.New("you cannot grant a role that holds permissions you do not hold")
	ErrSelfAction      = errors.New("you cannot do this to your own account")
	ErrLastSuperAdmin  = errors.New("this would leave the system without an active Super Admin")
	ErrHasHistory      = errors.New("this account has history and cannot be deleted; deactivate it instead")
)

// ValidationError is a field-level input problem.
type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + " " + e.Msg }

// UnknownRoleError names a role code that does not exist.
type UnknownRoleError struct{ Code string }

func (e *UnknownRoleError) Error() string { return "unknown role: " + e.Code }

type UserView struct {
	ID                 int64      `json:"id"`
	Username           string     `json:"username"`
	DisplayName        string     `json:"display_name"`
	Status             string     `json:"status"`
	MustChangePassword bool       `json:"must_change_password"`
	LastLoginAt        *time.Time `json:"last_login_at"`
	CreatedAt          time.Time  `json:"created_at"`
	DeactivatedAt      *time.Time `json:"deactivated_at"`
	Roles              []string   `json:"roles"`
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func userView(r repository.ListUsersRow) UserView {
	roles := r.RoleCodes
	if roles == nil {
		roles = []string{}
	}
	return UserView{ID: r.ID, Username: r.Username, DisplayName: r.DisplayName, Status: r.Status, MustChangePassword: r.MustChangePassword,
		LastLoginAt: tsPtr(r.LastLoginAt), CreatedAt: r.CreatedAt.Time, DeactivatedAt: tsPtr(r.DeactivatedAt), Roles: roles}
}

func require(actor *auth.Principal, permission string) error {
	if !actor.Has(permission) {
		return ErrForbidden
	}
	return nil
}

func (s *Service) requireReauth(actor *auth.Principal) error {
	if !s.RecentReauth(actor) {
		return ErrReauthRequired
	}
	return nil
}

func loadUser(ctx context.Context, q *repository.Queries, id int64) (UserView, error) {
	row, err := q.GetUserSummary(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserView{}, ErrUserNotFound
	}
	if err != nil {
		return UserView{}, err
	}
	return userView(repository.ListUsersRow(row)), nil
}

// assertCanTouch applies protected rule 1: an account that holds a protected role (Super Admin) can only be
// changed by someone who holds system.admin.
func assertCanTouch(ctx context.Context, q *repository.Queries, actor *auth.Principal, targetID int64) error {
	roles, err := q.ListUserRoleDetails(ctx, targetID)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if r.IsProtected && !actor.Has("system.admin") {
			return ErrProtectedTarget
		}
	}
	return nil
}

// resolveRoles maps role codes to roles, rejecting unknown codes and duplicates.
func resolveRoles(ctx context.Context, q *repository.Queries, codes []string) ([]repository.ListRolesByCodesRow, error) {
	seen := map[string]bool{}
	clean := make([]string, 0, len(codes))
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c != "" && !seen[c] {
			seen[c] = true
			clean = append(clean, c)
		}
	}
	rows, err := q.ListRolesByCodes(ctx, clean)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, r := range rows {
		found[r.Code] = true
	}
	for _, c := range clean {
		if !found[c] {
			return nil, &UnknownRoleError{Code: c}
		}
	}
	return rows, nil
}

// checkGrantable is the no-escalation guard (protected rule 2): a protected role needs system.admin, and anyone
// without system.admin can only grant a role whose permissions they already hold.
func checkGrantable(ctx context.Context, q *repository.Queries, actor *auth.Principal, roles []repository.ListRolesByCodesRow) error {
	if actor.Has("system.admin") {
		return nil // S-34: a system administrator can already edit any role, so the subset rule adds nothing
	}
	for _, r := range roles {
		if r.IsProtected {
			if !actor.Has("system.admin") {
				return ErrEscalation
			}
			continue
		}
		keys, err := q.ListPermissionKeysForRole(ctx, r.ID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if !actor.Has(k) {
				return ErrEscalation
			}
		}
	}
	return nil
}

func roleCodes(rows []repository.ListRolesByCodesRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Code)
	}
	return out
}

// mapAdminDBError turns the database backstops (migration 0003) into friendly errors.
func mapAdminDBError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch {
		case pe.Code == "23505" && pe.ConstraintName == "users_username_key":
			return ErrUsernameTaken
		case pe.Code == "23000" && strings.Contains(pe.Message, "no active Super Admin"):
			return ErrLastSuperAdmin
		}
	}
	return err
}

func (s *Service) actorEvent(actor *auth.Principal, action string, targetID int64) auditEvent {
	return auditEvent{actorID: actor.UserID, label: actor.Username, action: action, entityType: "user", entityID: fmt.Sprint(targetID)}
}

// ListFilter selects users for ListUsers. AfterID is the keyset cursor.
type ListFilter struct {
	Status, Search string
	AfterID        int64
	Limit          int
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ListUsers returns one page ordered by id plus the cursor for the next page (0 when there is none).
func (s *Service) ListUsers(ctx context.Context, actor *auth.Principal, f ListFilter) ([]UserView, int64, error) {
	if err := require(actor, "users.update"); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	rows, err := repository.New(s.pool).ListUsers(ctx, repository.ListUsersParams{
		AfterID: f.AfterID, StatusFilter: f.Status, Search: likeEscaper.Replace(strings.TrimSpace(f.Search)), RowLimit: int32(f.Limit + 1),
	})
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		next = rows[len(rows)-1].ID
	}
	out := make([]UserView, 0, len(rows))
	for _, r := range rows {
		out = append(out, userView(r))
	}
	return out, next, nil
}

func (s *Service) GetUser(ctx context.Context, actor *auth.Principal, id int64) (UserView, error) {
	if err := require(actor, "users.update"); err != nil {
		return UserView{}, err
	}
	return loadUser(ctx, repository.New(s.pool), id)
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,39}$`)

type NewUser struct {
	Username, DisplayName, Password string
	Roles                           []string
}

func validDisplayName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := len([]rune(name)); n < 1 || n > 120 {
		return "", &ValidationError{Field: "display_name", Msg: "must be 1 to 120 characters"}
	}
	return name, nil
}

// CreateUser adds an account that must change its password at first login.
func (s *Service) CreateUser(ctx context.Context, actor *auth.Principal, in NewUser, ci ClientInfo) (UserView, error) {
	if err := require(actor, "users.create"); err != nil {
		return UserView{}, err
	}
	username := normalise(in.Username)
	if !usernamePattern.MatchString(username) {
		return UserView{}, &ValidationError{Field: "username", Msg: "must be 3 to 40 characters: lower-case letters, digits, dot, dash or underscore"}
	}
	display, err := validDisplayName(in.DisplayName)
	if err != nil {
		return UserView{}, err
	}
	if err := password.Policy(in.Password, username, password.DevSeedPassword); err != nil {
		return UserView{}, &ValidationError{Field: "password", Msg: err.Error()}
	}
	hash, err := password.Hash(in.Password)
	if err != nil {
		return UserView{}, err
	}
	now := s.clock.Now()
	var out UserView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		roles, err := resolveRoles(ctx, q, in.Roles)
		if err != nil {
			return err
		}
		if err := checkGrantable(ctx, q, actor, roles); err != nil {
			return err
		}
		id, err := q.CreateUser(ctx, repository.CreateUserParams{Username: username, DisplayName: display, PasswordHash: hash,
			MustChangePassword: true, CreatedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}})
		if err != nil {
			return err
		}
		for _, r := range roles {
			if err := q.AssignRole(ctx, repository.AssignRoleParams{UserID: id, RoleID: r.ID, AssignedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
				return err
			}
		}
		ev := s.actorEvent(actor, "user.created", id)
		ev.after = map[string]any{"username": username, "display_name": display, "roles": roleCodes(roles)}
		if err := s.audit(ctx, q, now, ci, ev); err != nil {
			return err
		}
		out, err = loadUser(ctx, q, id)
		return err
	})
	return out, mapAdminDBError(err)
}

// UpdateUser changes the display name. The username never changes: it is the audit and login identity.
func (s *Service) UpdateUser(ctx context.Context, actor *auth.Principal, id int64, displayName string, ci ClientInfo) (UserView, error) {
	if err := require(actor, "users.update"); err != nil {
		return UserView{}, err
	}
	display, err := validDisplayName(displayName)
	if err != nil {
		return UserView{}, err
	}
	now := s.clock.Now()
	var out UserView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadUser(ctx, q, id)
		if err != nil {
			return err
		}
		if err := assertCanTouch(ctx, q, actor, id); err != nil {
			return err
		}
		if cur.DisplayName == display {
			out = cur
			return nil
		}
		if err := q.UpdateDisplayName(ctx, repository.UpdateDisplayNameParams{ID: id, DisplayName: display}); err != nil {
			return err
		}
		ev := s.actorEvent(actor, "user.updated", id)
		ev.before, ev.after = map[string]any{"display_name": cur.DisplayName}, map[string]any{"display_name": display}
		if err := s.audit(ctx, q, now, ci, ev); err != nil {
			return err
		}
		out, err = loadUser(ctx, q, id)
		return err
	})
	return out, err
}

// SetUserRoles replaces the target's roles. Protected rules 1 to 3 and 6 apply.
func (s *Service) SetUserRoles(ctx context.Context, actor *auth.Principal, id int64, codes []string, ci ClientInfo) (UserView, error) {
	if err := require(actor, "users.update"); err != nil {
		return UserView{}, err
	}
	if err := s.requireReauth(actor); err != nil {
		return UserView{}, err
	}
	if id == actor.UserID {
		return UserView{}, ErrSelfAction
	}
	now := s.clock.Now()
	var out UserView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadUser(ctx, q, id)
		if err != nil {
			return err
		}
		if err := assertCanTouch(ctx, q, actor, id); err != nil {
			return err
		}
		want, err := resolveRoles(ctx, q, codes)
		if err != nil {
			return err
		}
		have, err := q.ListUserRoleDetails(ctx, id)
		if err != nil {
			return err
		}
		haveSet, wantSet := map[string]int64{}, map[string]bool{}
		for _, r := range have {
			haveSet[r.Code] = r.ID
		}
		var added []repository.ListRolesByCodesRow
		for _, r := range want {
			wantSet[r.Code] = true
			if _, ok := haveSet[r.Code]; !ok {
				added = append(added, r)
			}
		}
		if err := checkGrantable(ctx, q, actor, added); err != nil {
			return err
		}
		for _, r := range added {
			if err := q.AssignRole(ctx, repository.AssignRoleParams{UserID: id, RoleID: r.ID, AssignedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}}); err != nil {
				return err
			}
		}
		for code, rid := range haveSet {
			if !wantSet[code] {
				if err := q.RemoveUserRole(ctx, repository.RemoveUserRoleParams{UserID: id, RoleID: rid}); err != nil {
					return err
				}
			}
		}
		ev := s.actorEvent(actor, "user.roles_changed", id)
		ev.before, ev.after = map[string]any{"roles": cur.Roles}, map[string]any{"roles": roleCodes(want)}
		if len(added) > 0 || len(have) != len(want) {
			if err := s.audit(ctx, q, now, ci, ev); err != nil {
				return err
			}
		}
		out, err = loadUser(ctx, q, id)
		return err
	})
	return out, mapAdminDBError(err)
}

// Deactivate blocks sign-in, ends every session of the account in the same transaction and keeps its history.
func (s *Service) Deactivate(ctx context.Context, actor *auth.Principal, id int64, reason string, ci ClientInfo) (UserView, error) {
	if err := require(actor, "users.deactivate"); err != nil {
		return UserView{}, err
	}
	if err := s.requireReauth(actor); err != nil {
		return UserView{}, err
	}
	if id == actor.UserID {
		return UserView{}, ErrSelfAction
	}
	now := s.clock.Now()
	var out UserView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadUser(ctx, q, id)
		if err != nil {
			return err
		}
		if err := assertCanTouch(ctx, q, actor, id); err != nil {
			return err
		}
		if cur.Status == "inactive" {
			out = cur
			return nil
		}
		if err := q.DeactivateUser(ctx, repository.DeactivateUserParams{ID: id, DeactivatedAt: ts(now)}); err != nil {
			return err
		}
		if err := q.RevokeAllUserSessions(ctx, repository.RevokeAllUserSessionsParams{UserID: id, RevokedAt: ts(now), RevokeReason: textOf("user_deactivated")}); err != nil {
			return err
		}
		ev := s.actorEvent(actor, "user.deactivated", id)
		ev.reason, ev.before, ev.after = clip(strings.TrimSpace(reason), 500), map[string]any{"status": "active"}, map[string]any{"status": "inactive"}
		if err := s.audit(ctx, q, now, ci, ev); err != nil {
			return err
		}
		out, err = loadUser(ctx, q, id)
		return err
	})
	return out, mapAdminDBError(err)
}

// Reactivate lets a deactivated account sign in again (it keeps its roles and its history).
func (s *Service) Reactivate(ctx context.Context, actor *auth.Principal, id int64, ci ClientInfo) (UserView, error) {
	if err := require(actor, "users.update"); err != nil {
		return UserView{}, err
	}
	if err := s.requireReauth(actor); err != nil {
		return UserView{}, err
	}
	now := s.clock.Now()
	var out UserView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadUser(ctx, q, id)
		if err != nil {
			return err
		}
		if err := assertCanTouch(ctx, q, actor, id); err != nil {
			return err
		}
		if cur.Status == "active" {
			out = cur
			return nil
		}
		if err := q.ReactivateUser(ctx, id); err != nil {
			return err
		}
		ev := s.actorEvent(actor, "user.reactivated", id)
		ev.before, ev.after = map[string]any{"status": "inactive"}, map[string]any{"status": "active"}
		if err := s.audit(ctx, q, now, ci, ev); err != nil {
			return err
		}
		out, err = loadUser(ctx, q, id)
		return err
	})
	return out, err
}

// ResetPassword sets a temporary password, forces a change at next login and ends the account's sessions.
// Nobody resets their own password here: they use change-password. The audit row never holds the password.
func (s *Service) ResetPassword(ctx context.Context, actor *auth.Principal, id int64, temporary string, ci ClientInfo) error {
	if err := require(actor, "users.update"); err != nil {
		return err
	}
	if err := s.requireReauth(actor); err != nil {
		return err
	}
	if id == actor.UserID {
		return ErrSelfAction
	}
	now := s.clock.Now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		target, err := loadUser(ctx, q, id)
		if err != nil {
			return err
		}
		if err := assertCanTouch(ctx, q, actor, id); err != nil {
			return err
		}
		if err := password.Policy(temporary, target.Username, password.DevSeedPassword); err != nil {
			return &ValidationError{Field: "new_password", Msg: err.Error()}
		}
		hash, err := password.Hash(temporary)
		if err != nil {
			return err
		}
		if err := q.SetPassword(ctx, repository.SetPasswordParams{ID: id, PasswordHash: hash, MustChangePassword: true}); err != nil {
			return err
		}
		if err := q.RevokeAllUserSessions(ctx, repository.RevokeAllUserSessionsParams{UserID: id, RevokedAt: ts(now), RevokeReason: textOf("password_reset")}); err != nil {
			return err
		}
		return s.audit(ctx, q, now, ci, s.actorEvent(actor, "user.password_reset", id))
	})
}

// AdminSetPIN stores a fast-switch PIN for a floor-role user (A-07); the rules about who may hold one live in SetPIN.
func (s *Service) AdminSetPIN(ctx context.Context, actor *auth.Principal, id int64, pin string, ci ClientInfo) error {
	if err := require(actor, "users.update"); err != nil {
		return err
	}
	if _, err := loadUser(ctx, repository.New(s.pool), id); err != nil {
		return err
	}
	return s.SetPIN(ctx, actor, id, pin, ci)
}

// DeleteUser removes an account that never did anything. Foreign keys protect history: any account that signed
// in, assigned a role, created another user or appears in the audit trail is refused (protected rule 4).
func (s *Service) DeleteUser(ctx context.Context, actor *auth.Principal, id int64, ci ClientInfo) error {
	if err := require(actor, "users.delete"); err != nil {
		return err
	}
	if err := s.requireReauth(actor); err != nil {
		return err
	}
	if id == actor.UserID {
		return ErrSelfAction
	}
	now := s.clock.Now()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadUser(ctx, q, id)
		if err != nil {
			return err
		}
		if err := assertCanTouch(ctx, q, actor, id); err != nil {
			return err
		}
		if err := q.DeleteUserRoles(ctx, id); err != nil {
			return err
		}
		if err := q.DeleteUserPIN(ctx, id); err != nil {
			return err
		}
		if err := q.DeleteUser(ctx, id); err != nil {
			return err
		}
		ev := s.actorEvent(actor, "user.deleted", id)
		ev.before = map[string]any{"username": cur.Username, "display_name": cur.DisplayName, "roles": cur.Roles}
		return s.audit(ctx, q, now, ci, ev)
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		return ErrHasHistory
	}
	return mapAdminDBError(err)
}

// Record lets other modules write audit rows inside their own transaction (platform/audit.Recorder).
func (s *Service) Record(ctx context.Context, tx pgx.Tx, e audit.Entry) error {
	return s.audit(ctx, repository.New(tx), s.clock.Now(), ClientInfo{IP: e.IP, RequestID: e.RequestID}, auditEvent{
		actorID: e.ActorID, label: e.ActorLabel, action: e.Action, entityType: e.EntityType, entityID: e.EntityID,
		reason: e.Reason, before: e.Before, after: e.After,
	})
}

// Clock exposes the injected clock so modules that share the re-authentication window also share the time source.
func (s *Service) Clock() Clock { return s.clock }
