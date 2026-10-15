-- The last-Super-Admin guard (0003) fired on EVERY deactivation, deletion or role removal, so in a database with
-- no active Super Admin nobody could be deactivated, even a cashier. It now acts only when the user being
-- deactivated or deleted actually holds the super_admin role, or when the role being removed is super_admin.
-- Found by the Stage 6 authentication tests.
CREATE OR REPLACE FUNCTION guard_last_super_admin() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  affected boolean;
BEGIN
  IF TG_TABLE_NAME = 'users' THEN
    SELECT EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id AND r.code = 'super_admin'
                   WHERE ur.user_id = OLD.id) INTO affected;
  ELSE
    SELECT EXISTS (SELECT 1 FROM roles r WHERE r.id = OLD.role_id AND r.code = 'super_admin') INTO affected;
  END IF;
  IF NOT affected THEN
    RETURN NULL;
  END IF;
  PERFORM pg_advisory_xact_lock(724001);
  IF NOT EXISTS (
    SELECT 1 FROM user_roles ur
    JOIN roles r ON r.id = ur.role_id AND r.code = 'super_admin'
    JOIN users u ON u.id = ur.user_id AND u.status = 'active'
  ) THEN
    RAISE EXCEPTION 'operation would leave no active Super Admin' USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NULL;
END $$;
