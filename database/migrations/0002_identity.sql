-- Identity and access (docs/schema-constraints.md section 1). Money and stock tables come later.

CREATE TABLE users (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  username             TEXT        NOT NULL CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,39}$'),
  display_name         TEXT        NOT NULL CHECK (length(btrim(display_name)) BETWEEN 1 AND 120),
  password_hash        TEXT        NOT NULL,
  status               TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
  must_change_password BOOLEAN     NOT NULL DEFAULT false,
  failed_login_count   INTEGER     NOT NULL DEFAULT 0 CHECK (failed_login_count >= 0),
  locked_until         TIMESTAMPTZ,
  last_login_at        TIMESTAMPTZ,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by           BIGINT REFERENCES users (id) ON DELETE RESTRICT,
  deactivated_at       TIMESTAMPTZ,
  CONSTRAINT users_username_key UNIQUE (username),
  CONSTRAINT users_deactivated_consistent CHECK ((status = 'inactive') = (deactivated_at IS NOT NULL))
);

CREATE TABLE roles (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  code         TEXT        NOT NULL CHECK (code ~ '^[a-z][a-z0-9_]{1,39}$'),
  name         TEXT        NOT NULL,
  description  TEXT        NOT NULL DEFAULT '',
  is_protected BOOLEAN     NOT NULL DEFAULT false,
  is_template  BOOLEAN     NOT NULL DEFAULT false,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT roles_code_key UNIQUE (code)
);

CREATE TABLE permissions (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key          TEXT    NOT NULL CHECK (key ~ '^[a-z_]+(\.[a-z_]+)+$'),
  description  TEXT    NOT NULL DEFAULT '',
  is_protected BOOLEAN NOT NULL DEFAULT false, -- only protected roles may hold it (system.admin)
  CONSTRAINT permissions_key_key UNIQUE (key)
);

CREATE TABLE user_roles (
  user_id     BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  role_id     BIGINT      NOT NULL REFERENCES roles (id) ON DELETE RESTRICT,
  assigned_by BIGINT REFERENCES users (id) ON DELETE RESTRICT,
  assigned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, role_id)
);

CREATE TABLE role_permissions (
  role_id       BIGINT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
  permission_id BIGINT NOT NULL REFERENCES permissions (id) ON DELETE RESTRICT,
  PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE sessions (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id             BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  token_hash          BYTEA       NOT NULL CHECK (octet_length(token_hash) = 32), -- SHA-256 only, never the token
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  idle_expires_at     TIMESTAMPTZ NOT NULL,
  absolute_expires_at TIMESTAMPTZ NOT NULL,
  revoked_at          TIMESTAMPTZ,
  revoke_reason       TEXT,
  ip                  INET,
  user_agent          TEXT,
  CONSTRAINT sessions_token_hash_key UNIQUE (token_hash),
  CONSTRAINT sessions_expiry_order CHECK (absolute_expires_at >= created_at)
);
CREATE INDEX sessions_active_by_user ON sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE user_pins (
  user_id      BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE RESTRICT,
  pin_hash     TEXT        NOT NULL,
  failed_count INTEGER     NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
  locked_until TIMESTAMPTZ,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE login_attempts (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  username_norm TEXT        NOT NULL,
  ip            INET,
  attempted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  success       BOOLEAN     NOT NULL
);
CREATE INDEX login_attempts_by_user ON login_attempts (username_norm, attempted_at);
CREATE INDEX login_attempts_by_ip ON login_attempts (ip, attempted_at);

CREATE TABLE audit_logs (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  actor_user_id BIGINT REFERENCES users (id) ON DELETE RESTRICT, -- NULL for CLI and system actors
  actor_label   TEXT        NOT NULL,
  action        TEXT        NOT NULL,
  entity_type   TEXT        NOT NULL,
  entity_id     TEXT,
  reason        TEXT,
  result        TEXT        NOT NULL DEFAULT 'success' CHECK (result IN ('success', 'denied', 'failed')),
  before_json   JSONB,
  after_json    JSONB,
  request_id    TEXT,
  ip            INET
);
CREATE INDEX audit_logs_by_entity ON audit_logs (entity_type, entity_id);
CREATE INDEX audit_logs_by_actor ON audit_logs (actor_user_id, occurred_at);
CREATE INDEX audit_logs_by_action ON audit_logs (action, occurred_at);
