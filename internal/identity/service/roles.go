package service

// Role and permission administration (Stage 7b). Rules from docs/rbac.md section 3: protected roles and the
// system.admin permission are never changed through the API, an actor can only grant permissions they hold
// (a system administrator excepted, S-34), every change needs a recent password entry and writes before and
// after values to the audit trail, and the response lists the users affected (permissions are read from the
// database on each request, so a change applies on the next request).

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zuscar/backend/internal/identity/repository"
	"zuscar/backend/internal/platform/auth"
)

var (
	ErrRoleNotFound        = errors.New("role not found")
	ErrRoleCodeTaken       = errors.New("that role code is already in use")
	ErrProtectedRole       = errors.New("protected roles cannot be changed or deleted")
	ErrProtectedPermission = errors.New("this permission can only belong to protected roles and cannot be granted")
	ErrRoleInUse           = errors.New("this role is assigned to users; remove it from them first")
)

type RoleView struct {
	ID          int64    `json:"id"`
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Protected   bool     `json:"is_protected"`
	Template    bool     `json:"is_template"`
	Permissions []string `json:"permissions"`
	UserCount   int      `json:"user_count"`
}

type PermissionView struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	Description string `json:"description"`
	Protected   bool   `json:"is_protected"`
}

type AffectedUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func roleView(r repository.ListRolesRow) RoleView {
	perms := r.PermissionKeys
	if perms == nil {
		perms = []string{}
	}
	return RoleView{ID: r.ID, Code: r.Code, Name: r.Name, Description: r.Description, Protected: r.IsProtected,
		Template: r.IsTemplate, Permissions: perms, UserCount: int(r.UserCount)}
}

func loadRole(ctx context.Context, q *repository.Queries, id int64) (RoleView, error) {
	row, err := q.GetRole(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoleView{}, ErrRoleNotFound
	}
	if err != nil {
		return RoleView{}, err
	}
	return roleView(repository.ListRolesRow(row)), nil
}

func (s *Service) roleEvent(actor *auth.Principal, action string, id int64) auditEvent {
	ev := s.actorEvent(actor, action, id)
	ev.entityType = "role"
	return ev
}

func (s *Service) ListRoles(ctx context.Context, actor *auth.Principal) ([]RoleView, error) {
	if err := require(actor, "roles.manage"); err != nil {
		return nil, err
	}
	rows, err := repository.New(s.pool).ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RoleView, 0, len(rows))
	for _, r := range rows {
		out = append(out, roleView(r))
	}
	return out, nil
}

func (s *Service) ListPermissions(ctx context.Context, actor *auth.Principal) ([]PermissionView, error) {
	if err := require(actor, "roles.manage"); err != nil {
		return nil, err
	}
	rows, err := repository.New(s.pool).ListPermissions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PermissionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, PermissionView{ID: r.ID, Key: r.Key, Description: r.Description, Protected: r.IsProtected})
	}
	return out, nil
}

var roleCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)

func validRoleText(name, description string) (string, string, error) {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if n := len([]rune(name)); n < 1 || n > 80 {
		return "", "", &ValidationError{Field: "name", Msg: "must be 1 to 80 characters"}
	}
	if len([]rune(description)) > 300 {
		return "", "", &ValidationError{Field: "description", Msg: "must be at most 300 characters"}
	}
	return name, description, nil
}

// CreateRole adds an empty, non-protected role. Permissions are granted separately (permissions.assign).
func (s *Service) CreateRole(ctx context.Context, actor *auth.Principal, code, name, description string, ci ClientInfo) (RoleView, error) {
	if err := require(actor, "roles.manage"); err != nil {
		return RoleView{}, err
	}
	if err := s.requireReauth(actor); err != nil {
		return RoleView{}, err
	}
	code = strings.TrimSpace(code)
	if !roleCodePattern.MatchString(code) {
		return RoleView{}, &ValidationError{Field: "code", Msg: "must be 2 to 40 characters: lower-case letters, digits and underscore, starting with a letter"}
	}
	name, description, err := validRoleText(name, description)
	if err != nil {
		return RoleView{}, err
	}
	now := s.clock.Now()
	var out RoleView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		id, err := q.CreateRole(ctx, repository.CreateRoleParams{Code: code, Name: name, Description: description})
		if err != nil {
			return err
		}
		ev := s.roleEvent(actor, "role.created", id)
		ev.after = map[string]any{"code": code, "name": name, "description": description}
		if err := s.audit(ctx, q, now, ci, ev); err != nil {
			return err
		}
		out, err = loadRole(ctx, q, id)
		return err
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return RoleView{}, ErrRoleCodeTaken
	}
	return out, err
}

// UpdateRole changes the name and description. The code never changes.
func (s *Service) UpdateRole(ctx context.Context, actor *auth.Principal, id int64, name, description string, ci ClientInfo) (RoleView, error) {
	if err := require(actor, "roles.manage"); err != nil {
		return RoleView{}, err
	}
	if err := s.requireReauth(actor); err != nil {
		return RoleView{}, err
	}
	name, description, err := validRoleText(name, description)
	if err != nil {
		return RoleView{}, err
	}
	now := s.clock.Now()
	var out RoleView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadRole(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Protected {
			return ErrProtectedRole
		}
		if cur.Name == name && cur.Description == description {
			out = cur
			return nil
		}
		if err := q.UpdateRoleText(ctx, repository.UpdateRoleTextParams{ID: id, Name: name, Description: description}); err != nil {
			return err
		}
		ev := s.roleEvent(actor, "role.updated", id)
		ev.before = map[string]any{"name": cur.Name, "description": cur.Description}
		ev.after = map[string]any{"name": name, "description": description}
		if err := s.audit(ctx, q, now, ci, ev); err != nil {
			return err
		}
		out, err = loadRole(ctx, q, id)
		return err
	})
	return out, err
}

// DeleteRole removes a non-protected role that nobody holds (foreign keys refuse the rest).
func (s *Service) DeleteRole(ctx context.Context, actor *auth.Principal, id int64, ci ClientInfo) error {
	if err := require(actor, "roles.manage"); err != nil {
		return err
	}
	if err := s.requireReauth(actor); err != nil {
		return err
	}
	now := s.clock.Now()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadRole(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Protected {
			return ErrProtectedRole
		}
		if err := q.DeleteRole(ctx, id); err != nil {
			return err
		}
		ev := s.roleEvent(actor, "role.deleted", id)
		ev.before = map[string]any{"code": cur.Code, "name": cur.Name, "permissions": cur.Permissions}
		return s.audit(ctx, q, now, ci, ev)
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		return ErrRoleInUse
	}
	return err
}

// SetRolePermissions replaces a role's permission set. system.admin is refused for everyone; anyone without
// system.admin can only add permissions they hold themselves. Removing a permission is always allowed.
func (s *Service) SetRolePermissions(ctx context.Context, actor *auth.Principal, id int64, keys []string, ci ClientInfo) (RoleView, []AffectedUser, error) {
	if err := require(actor, "permissions.assign"); err != nil {
		return RoleView{}, nil, err
	}
	if err := s.requireReauth(actor); err != nil {
		return RoleView{}, nil, err
	}
	now := s.clock.Now()
	var out RoleView
	var affected []AffectedUser
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		cur, err := loadRole(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.Protected {
			return ErrProtectedRole
		}
		seen, clean := map[string]bool{}, []string{}
		for _, k := range keys {
			if k = strings.TrimSpace(k); k != "" && !seen[k] {
				seen[k] = true
				clean = append(clean, k)
			}
		}
		perms, err := q.ListPermissionsByKeys(ctx, clean)
		if err != nil {
			return err
		}
		if len(perms) != len(clean) {
			found := map[string]bool{}
			for _, p := range perms {
				found[p.Key] = true
			}
			for _, k := range clean {
				if !found[k] {
					return &ValidationError{Field: "permissions", Msg: "unknown permission: " + k}
				}
			}
		}
		have := map[string]bool{}
		for _, k := range cur.Permissions {
			have[k] = true
		}
		want := map[string]bool{}
		for _, p := range perms {
			want[p.Key] = true
			if p.IsProtected {
				return ErrProtectedPermission
			}
			if !have[p.Key] && !actor.Has(p.Key) && !actor.Has("system.admin") {
				return ErrEscalation
			}
		}
		for _, p := range perms {
			if !have[p.Key] {
				if err := q.GrantRolePermission(ctx, repository.GrantRolePermissionParams{RoleID: id, PermissionID: p.ID}); err != nil {
					return err
				}
			}
		}
		if len(have) > 0 {
			all, err := q.ListPermissionsByKeys(ctx, cur.Permissions)
			if err != nil {
				return err
			}
			for _, p := range all {
				if !want[p.Key] {
					if err := q.RevokeRolePermission(ctx, repository.RevokeRolePermissionParams{RoleID: id, PermissionID: p.ID}); err != nil {
						return err
					}
				}
			}
		}
		out, err = loadRole(ctx, q, id)
		if err != nil {
			return err
		}
		users, err := q.ListUsersWithRole(ctx, id)
		if err != nil {
			return err
		}
		affected = make([]AffectedUser, 0, len(users))
		for _, u := range users {
			affected = append(affected, AffectedUser{ID: u.ID, Username: u.Username})
		}
		if len(out.Permissions) == len(cur.Permissions) && sameSet(out.Permissions, have) {
			return nil
		}
		ev := s.roleEvent(actor, "role.permissions_changed", id)
		ev.before, ev.after = map[string]any{"permissions": cur.Permissions}, map[string]any{"permissions": out.Permissions, "affected_users": len(affected)}
		return s.audit(ctx, q, now, ci, ev)
	})
	return out, affected, err
}

func sameSet(list []string, set map[string]bool) bool {
	for _, k := range list {
		if !set[k] {
			return false
		}
	}
	return true
}

// AssignableRole is what a user administrator sees when choosing roles for someone.
type AssignableRole struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListAssignableRoles returns the roles the caller could grant with the same rule SetUserRoles applies
// (checkGrantable): protected roles only with system.admin, otherwise only roles whose permissions the caller
// holds. It needs users.update, not roles.manage, so a Business Manager can fill in the role picker.
func (s *Service) ListAssignableRoles(ctx context.Context, actor *auth.Principal) ([]AssignableRole, error) {
	if err := require(actor, "users.update"); err != nil {
		return nil, err
	}
	q := repository.New(s.pool)
	rows, err := q.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AssignableRole, 0, len(rows))
	for _, r := range rows {
		err := checkGrantable(ctx, q, actor, []repository.ListRolesByCodesRow{{ID: r.ID, Code: r.Code, Name: r.Name, IsProtected: r.IsProtected}})
		if errors.Is(err, ErrEscalation) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, AssignableRole{Code: r.Code, Name: r.Name, Description: r.Description})
	}
	return out, nil
}
