-- name: UpsertPermission :one
INSERT INTO permissions (key, description, is_protected) VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET description = EXCLUDED.description, is_protected = EXCLUDED.is_protected
RETURNING id;

-- name: UpsertRole :one
INSERT INTO roles (code, name, description, is_protected, is_template) VALUES ($1, $2, $3, $4, true)
ON CONFLICT (code) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id;

-- name: GrantRolePermission :exec
INSERT INTO role_permissions (role_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: GetRoleIDByCode :one
SELECT id FROM roles WHERE code = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CountActiveSuperAdmins :one
SELECT count(*) FROM user_roles ur
JOIN roles r ON r.id = ur.role_id AND r.code = 'super_admin'
JOIN users u ON u.id = ur.user_id AND u.status = 'active';

-- name: CreateUser :one
INSERT INTO users (username, display_name, password_hash, must_change_password, created_by)
VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: AssignRole :exec
INSERT INTO user_roles (user_id, role_id, assigned_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: GetUserByUsername :one
SELECT id, username, display_name, password_hash, status, must_change_password FROM users WHERE username = $1;

-- name: ListUsernamesNotDev :many
SELECT username FROM users WHERE username NOT LIKE 'dev.%' ORDER BY username;

-- name: InsertAudit :exec
INSERT INTO audit_logs (actor_user_id, actor_label, action, entity_type, entity_id, reason, result, before_json, after_json)
VALUES ($1, $2, $3, $4, $5, $6, 'success', $7, $8);
