-- Database-level backstops for the protected rules in docs/rbac.md section 3 (AC-11, AC-12, NN-07, NN-08).
-- The service layer gives friendly errors; these triggers hold even if a bug or a manual SQL session tries.

-- audit_logs is append-only (retention 7 years, DQ-18).
CREATE TRIGGER audit_logs_immutable BEFORE UPDATE OR DELETE ON audit_logs
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER audit_logs_no_truncate BEFORE TRUNCATE ON audit_logs
  FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A protected permission (system.admin) can only be held by a protected role.
CREATE FUNCTION guard_protected_permission() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT is_protected FROM permissions WHERE id = NEW.permission_id)
     AND NOT (SELECT is_protected FROM roles WHERE id = NEW.role_id) THEN
    RAISE EXCEPTION 'a protected permission can only be held by a protected role'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER role_permissions_protected BEFORE INSERT OR UPDATE ON role_permissions
  FOR EACH ROW EXECUTE FUNCTION guard_protected_permission();

-- Protected roles (super_admin) cannot be deleted, renamed or unprotected.
CREATE FUNCTION guard_protected_role() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'protected role % cannot be deleted', OLD.code USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.code <> OLD.code OR NEW.is_protected <> OLD.is_protected THEN
    RAISE EXCEPTION 'protected role % cannot be renamed or unprotected', OLD.code USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER roles_protected BEFORE UPDATE OR DELETE ON roles
  FOR EACH ROW WHEN (OLD.is_protected) EXECUTE FUNCTION guard_protected_role();

-- The last active Super Admin cannot be deactivated, deleted or lose the role. The advisory lock
-- serialises concurrent removals so two admins deactivating each other cannot both succeed.
CREATE FUNCTION guard_last_super_admin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
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
CREATE CONSTRAINT TRIGGER users_keep_super_admin AFTER UPDATE OF status OR DELETE ON users
  DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW WHEN (OLD.status = 'active')
  EXECUTE FUNCTION guard_last_super_admin();
CREATE CONSTRAINT TRIGGER user_roles_keep_super_admin AFTER DELETE ON user_roles
  DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE FUNCTION guard_last_super_admin();
