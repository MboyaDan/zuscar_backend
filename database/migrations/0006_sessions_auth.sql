-- Authentication support on sessions (ADR-13). Any sessions that exist are discarded: this migration ships
-- before login existed, so none can exist in a real database, and a session without a CSRF binding is invalid.
DELETE FROM sessions;
ALTER TABLE sessions
  ADD COLUMN csrf_hash   BYTEA NOT NULL CHECK (octet_length(csrf_hash) = 32), -- SHA-256 of the CSRF token, bound to this session
  ADD COLUMN auth_method TEXT  NOT NULL DEFAULT 'password' CHECK (auth_method IN ('password', 'pin')),
  ADD COLUMN reauth_at   TIMESTAMPTZ; -- last password re-entry for sensitive actions (S-16); never set by PIN sessions
ALTER TABLE sessions ALTER COLUMN auth_method DROP DEFAULT;
