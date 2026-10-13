-- Stage 2 design spike. NOT a migration. Proves the constraint designs in docs/schema-constraints.md
-- on a scratch database. Run: createdb spike && psql -d spike -f docs/spikes/stage2-constraints-spike.sql
\set ON_ERROR_STOP off
\echo '== 1. room overlap: partial exclusion constraint (AC-07, ADR-06)'
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE reservations (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  room_id bigint NOT NULL, check_in date NOT NULL, check_out date NOT NULL,
  status text NOT NULL CHECK (status IN ('tentative','confirmed','checked_in','checked_out','cancelled','no_show')),
  CHECK (check_out > check_in),
  CONSTRAINT reservations_no_overlap EXCLUDE USING gist
    (room_id WITH =, daterange(check_in, check_out, '[)') WITH &&)
    WHERE (status IN ('confirmed','checked_in')));
INSERT INTO reservations(room_id,check_in,check_out,status) VALUES (1,'2026-11-01','2026-11-05','confirmed');
\echo 'expect ERROR: overlapping confirmed'
INSERT INTO reservations(room_id,check_in,check_out,status) VALUES (1,'2026-11-04','2026-11-06','confirmed');
\echo 'expect OK: back-to-back (checkout day = next check-in)'
INSERT INTO reservations(room_id,check_in,check_out,status) VALUES (1,'2026-11-05','2026-11-07','confirmed');
\echo 'expect OK: tentative overlap does not block (DQ-09)'
INSERT INTO reservations(room_id,check_in,check_out,status) VALUES (1,'2026-11-02','2026-11-03','tentative');
\echo 'expect ERROR: promoting that tentative to confirmed now conflicts'
UPDATE reservations SET status='confirmed' WHERE status='tentative';
\echo 'expect OK: cancelled frees the room'
UPDATE reservations SET status='cancelled' WHERE check_in='2026-11-01';
INSERT INTO reservations(room_id,check_in,check_out,status) VALUES (1,'2026-11-01','2026-11-05','confirmed');

\echo '== 2. append-only ledger trigger (AC-06, ADR-07)'
CREATE TABLE inventory_movements (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  item_id bigint NOT NULL, qty_delta bigint NOT NULL CHECK (qty_delta <> 0),
  movement_type text NOT NULL, transfer_group uuid);
CREATE FUNCTION reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION '% on % is not allowed: append-only (use a compensating record)', TG_OP, TG_TABLE_NAME; END $$;
CREATE TRIGGER inventory_movements_immutable BEFORE UPDATE OR DELETE ON inventory_movements
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER inventory_movements_no_truncate BEFORE TRUNCATE ON inventory_movements
  FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
INSERT INTO inventory_movements(item_id,qty_delta,movement_type) VALUES (1,100,'opening_balance');
\echo 'expect ERROR x3: update, delete, truncate'
UPDATE inventory_movements SET qty_delta=5;
DELETE FROM inventory_movements;
TRUNCATE inventory_movements;

\echo '== 3. transfer pairs must net to zero (deferred constraint trigger)'
CREATE FUNCTION check_transfer_group() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT COALESCE(SUM(qty_delta),0) FROM inventory_movements WHERE transfer_group = NEW.transfer_group) <> 0 THEN
    RAISE EXCEPTION 'transfer group % does not net to zero', NEW.transfer_group; END IF;
  RETURN NULL; END $$;
CREATE CONSTRAINT TRIGGER transfer_pair_balanced AFTER INSERT ON inventory_movements
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.transfer_group IS NOT NULL)
  EXECUTE FUNCTION check_transfer_group();
\echo 'expect OK: balanced pair in one transaction'
BEGIN;
INSERT INTO inventory_movements(item_id,qty_delta,movement_type,transfer_group) VALUES (1,-10,'transfer_out','00000000-0000-0000-0000-000000000001');
INSERT INTO inventory_movements(item_id,qty_delta,movement_type,transfer_group) VALUES (1,10,'transfer_in','00000000-0000-0000-0000-000000000001');
COMMIT;
\echo 'expect ERROR at COMMIT: one-sided transfer'
BEGIN;
INSERT INTO inventory_movements(item_id,qty_delta,movement_type,transfer_group) VALUES (1,-10,'transfer_out','00000000-0000-0000-0000-000000000002');
COMMIT;

\echo '== 4. system.admin only on the protected super_admin role (AC-11, NN-07)'
CREATE TABLE roles (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, code text UNIQUE NOT NULL, is_protected boolean NOT NULL DEFAULT false);
CREATE TABLE permissions (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, key text UNIQUE NOT NULL);
CREATE TABLE role_permissions (role_id bigint REFERENCES roles, permission_id bigint REFERENCES permissions, PRIMARY KEY (role_id, permission_id));
CREATE FUNCTION guard_system_admin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT key FROM permissions WHERE id = NEW.permission_id) = 'system.admin'
     AND NOT (SELECT is_protected FROM roles WHERE id = NEW.role_id) THEN
    RAISE EXCEPTION 'system.admin can only be held by a protected role'; END IF;
  RETURN NEW; END $$;
CREATE TRIGGER role_permissions_guard BEFORE INSERT OR UPDATE ON role_permissions FOR EACH ROW EXECUTE FUNCTION guard_system_admin();
INSERT INTO roles(code,is_protected) VALUES ('super_admin',true),('business_manager',false);
INSERT INTO permissions(key) VALUES ('system.admin'),('sales.view');
\echo 'expect OK then ERROR'
INSERT INTO role_permissions SELECT r.id,p.id FROM roles r, permissions p WHERE r.code='super_admin' AND p.key='system.admin';
INSERT INTO role_permissions SELECT r.id,p.id FROM roles r, permissions p WHERE r.code='business_manager' AND p.key='system.admin';
INSERT INTO role_permissions SELECT r.id,p.id FROM roles r, permissions p WHERE r.code='business_manager' AND p.key='sales.view';

\echo '== 5. last active Super Admin cannot be removed (AC-12), including concurrent attempts'
CREATE TABLE users (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, username text UNIQUE NOT NULL, status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')));
CREATE TABLE user_roles (user_id bigint REFERENCES users, role_id bigint REFERENCES roles, PRIMARY KEY (user_id, role_id));
CREATE FUNCTION guard_last_super_admin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(724001);  -- serialises concurrent removals; held to end of transaction
  IF NOT EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id JOIN users u ON u.id = ur.user_id
                 WHERE r.code = 'super_admin' AND u.status = 'active') THEN
    RAISE EXCEPTION 'operation would leave no active Super Admin'; END IF;
  RETURN NULL; END $$;
CREATE CONSTRAINT TRIGGER users_keep_super_admin AFTER UPDATE OF status OR DELETE ON users
  DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW WHEN (OLD.status = 'active') EXECUTE FUNCTION guard_last_super_admin();
CREATE CONSTRAINT TRIGGER user_roles_keep_super_admin AFTER DELETE ON user_roles
  DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE FUNCTION guard_last_super_admin();
INSERT INTO users(username) VALUES ('owner_a'),('owner_b'),('cashier_x');
INSERT INTO user_roles SELECT u.id, r.id FROM users u, roles r WHERE u.username IN ('owner_a','owner_b') AND r.code='super_admin';
\echo 'expect OK: deactivating a non-admin / one of two admins'
UPDATE users SET status='inactive' WHERE username='cashier_x';
UPDATE users SET status='inactive' WHERE username='owner_a';
\echo 'expect ERROR: deactivating the last active Super Admin; and removing their role; and deleting them'
UPDATE users SET status='inactive' WHERE username='owner_b';
DELETE FROM user_roles WHERE user_id=(SELECT id FROM users WHERE username='owner_b');
DELETE FROM users WHERE username='owner_b';
UPDATE users SET status='active' WHERE username='owner_a';  -- reset to two admins for the race test
