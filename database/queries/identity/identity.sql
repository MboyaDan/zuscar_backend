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

-- ---- authentication (Stage 6) ----

-- name: GetUserForLogin :one
SELECT id, username, display_name, password_hash, status, must_change_password, failed_login_count, locked_until
FROM users WHERE username = $1;

-- name: GetUserByID :one
SELECT id, username, display_name, password_hash, status, must_change_password, failed_login_count, locked_until
FROM users WHERE id = $1;

-- name: RecordFailedLogin :one
UPDATE users SET
  failed_login_count = failed_login_count + 1,
  locked_until = CASE WHEN failed_login_count + 1 >= sqlc.arg(max_attempts)::int THEN sqlc.arg(lock_until)::timestamptz ELSE locked_until END
WHERE id = sqlc.arg(id)
RETURNING failed_login_count, locked_until;

-- name: RecordSuccessfulLogin :exec
UPDATE users SET failed_login_count = 0, locked_until = NULL, last_login_at = $2 WHERE id = $1;

-- name: SetPassword :exec
UPDATE users SET password_hash = $2, must_change_password = $3, failed_login_count = 0, locked_until = NULL WHERE id = $1;

-- name: InsertLoginAttempt :exec
INSERT INTO login_attempts (username_norm, ip, attempted_at, success) VALUES ($1, $2, $3, $4);

-- name: CountFailedAttemptsByUsername :one
SELECT count(*) FROM login_attempts WHERE username_norm = $1 AND success = false AND attempted_at > $2;

-- name: CountFailedAttemptsByIP :one
SELECT count(*) FROM login_attempts WHERE ip = $1 AND success = false AND attempted_at > $2;

-- name: PruneLoginAttempts :exec
DELETE FROM login_attempts WHERE attempted_at < $1;

-- name: CreateSession :one
INSERT INTO sessions (user_id, token_hash, csrf_hash, auth_method, created_at, last_seen_at, idle_expires_at, absolute_expires_at, ip, user_agent)
VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9) RETURNING id;

-- name: GetSessionByTokenHash :one
SELECT s.id AS session_id, s.user_id, s.csrf_hash, s.auth_method, s.last_seen_at, s.idle_expires_at, s.absolute_expires_at, s.reauth_at,
       u.username, u.display_name, u.must_change_password
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND u.status = 'active';

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = $2, idle_expires_at = $3 WHERE id = $1 AND revoked_at IS NULL;

-- name: SetSessionReauth :exec
UPDATE sessions SET reauth_at = $2 WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = $2, revoke_reason = $3 WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeOtherSessions :exec
UPDATE sessions SET revoked_at = $3, revoke_reason = $4 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL;

-- name: RevokeAllUserSessions :exec
UPDATE sessions SET revoked_at = $2, revoke_reason = $3 WHERE user_id = $1 AND revoked_at IS NULL;

-- name: ListRolesForUser :many
SELECT r.code, r.name FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = $1 ORDER BY r.code;

-- name: ListPermissionKeysForUser :many
SELECT DISTINCT p.key FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN permissions p ON p.id = rp.permission_id
WHERE ur.user_id = $1 ORDER BY p.key;

-- name: GetPIN :one
SELECT user_id, pin_hash, failed_count, locked_until FROM user_pins WHERE user_id = $1;

-- name: UpsertPIN :exec
INSERT INTO user_pins (user_id, pin_hash, failed_count, locked_until, updated_at) VALUES ($1, $2, 0, NULL, $3)
ON CONFLICT (user_id) DO UPDATE SET pin_hash = EXCLUDED.pin_hash, failed_count = 0, locked_until = NULL, updated_at = EXCLUDED.updated_at;

-- name: RecordPinFailure :one
UPDATE user_pins SET
  failed_count = failed_count + 1,
  locked_until = CASE WHEN failed_count + 1 >= sqlc.arg(max_attempts)::int THEN sqlc.arg(lock_until)::timestamptz ELSE locked_until END
WHERE user_id = sqlc.arg(user_id)
RETURNING failed_count, locked_until;

-- name: ResetPinFailures :exec
UPDATE user_pins SET failed_count = 0, locked_until = NULL WHERE user_id = $1;

-- name: ClearExpiredLock :exec
UPDATE users SET failed_login_count = 0, locked_until = NULL WHERE id = $1 AND locked_until IS NOT NULL AND locked_until <= $2;

-- name: ClearExpiredPinLock :exec
UPDATE user_pins SET failed_count = 0, locked_until = NULL WHERE user_id = $1 AND locked_until IS NOT NULL AND locked_until <= $2;

-- Security events carry a result, request id and client address (before/after never hold secrets).
-- name: InsertAuditFull :exec
INSERT INTO audit_logs (occurred_at, actor_user_id, actor_label, action, entity_type, entity_id, reason, result, before_json, after_json, request_id, ip)
VALUES (sqlc.arg(occurred_at), sqlc.arg(actor_user_id), sqlc.arg(actor_label), sqlc.arg(action), sqlc.arg(entity_type), sqlc.arg(entity_id),
        sqlc.arg(reason), sqlc.arg(result), sqlc.arg(before_json), sqlc.arg(after_json), sqlc.arg(request_id), sqlc.arg(ip));

-- ---- Stage 7: user administration ----

-- name: ListUsers :many
SELECT u.id, u.username, u.display_name, u.status, u.must_change_password, u.last_login_at, u.created_at, u.deactivated_at,
       COALESCE(array_agg(r.code ORDER BY r.code) FILTER (WHERE r.code IS NOT NULL), '{}')::text[] AS role_codes
FROM users u
LEFT JOIN user_roles ur ON ur.user_id = u.id
LEFT JOIN roles r ON r.id = ur.role_id
WHERE u.id > sqlc.arg(after_id)::bigint
  AND (sqlc.arg(status_filter)::text = '' OR u.status = sqlc.arg(status_filter)::text)
  AND (sqlc.arg(search)::text = '' OR u.username LIKE sqlc.arg(search)::text || '%' ESCAPE '\'
       OR lower(u.display_name) LIKE '%' || lower(sqlc.arg(search)::text) || '%' ESCAPE '\')
GROUP BY u.id
ORDER BY u.id
LIMIT sqlc.arg(row_limit)::int;

-- name: GetUserSummary :one
SELECT u.id, u.username, u.display_name, u.status, u.must_change_password, u.last_login_at, u.created_at, u.deactivated_at,
       COALESCE(array_agg(r.code ORDER BY r.code) FILTER (WHERE r.code IS NOT NULL), '{}')::text[] AS role_codes
FROM users u
LEFT JOIN user_roles ur ON ur.user_id = u.id
LEFT JOIN roles r ON r.id = ur.role_id
WHERE u.id = $1
GROUP BY u.id;

-- name: UpdateDisplayName :exec
UPDATE users SET display_name = $2 WHERE id = $1;

-- name: DeactivateUser :exec
UPDATE users SET status = 'inactive', deactivated_at = $2 WHERE id = $1;

-- name: ReactivateUser :exec
UPDATE users SET status = 'active', deactivated_at = NULL, failed_login_count = 0, locked_until = NULL WHERE id = $1;

-- name: ListRolesByCodes :many
SELECT id, code, name, is_protected FROM roles WHERE code = ANY(sqlc.arg(codes)::text[]) ORDER BY code;

-- name: ListUserRoleDetails :many
SELECT r.id, r.code, r.is_protected FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = $1 ORDER BY r.code;

-- name: ListPermissionKeysForRole :many
SELECT p.key FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id WHERE rp.role_id = $1 ORDER BY p.key;

-- name: RemoveUserRole :exec
DELETE FROM user_roles WHERE user_id = $1 AND role_id = $2;

-- name: DeleteUserRoles :exec
DELETE FROM user_roles WHERE user_id = $1;

-- name: DeleteUserPIN :exec
DELETE FROM user_pins WHERE user_id = $1;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;
