-- Stage 26: expenses. A plain ledger of money the business spent that is not stock bought through purchasing
-- (rent, power, wages, repairs, transport). An entry is never edited or deleted: a wrong one is voided with a reason and
-- a new one is entered. Cash taken from a till drawer stays on the shift as a cash movement (S-57); this ledger is for
-- everything else, so the profit report adds the two together and nothing is counted twice.

CREATE TABLE expenses (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  expense_date DATE        NOT NULL,
  description  TEXT        NOT NULL CHECK (char_length(btrim(description)) BETWEEN 1 AND 200),
  category     TEXT        NOT NULL CHECK (category IN ('rent', 'utilities', 'wages', 'transport', 'repairs', 'supplies', 'licences', 'marketing', 'other')),
  amount_cents BIGINT      NOT NULL CHECK (amount_cents > 0 AND amount_cents <= 100000000000),
  paid_by      TEXT        NOT NULL CHECK (char_length(btrim(paid_by)) BETWEEN 1 AND 80),            -- who handed the money over, as the owner says it
  paid_with    TEXT        NOT NULL CHECK (paid_with IN ('cash', 'mpesa', 'bank', 'card', 'other')),
  created_by   BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  voided_at    TIMESTAMPTZ,
  voided_by    BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  void_reason  TEXT        CHECK (void_reason IS NULL OR char_length(btrim(void_reason)) BETWEEN 1 AND 200),
  CONSTRAINT expenses_void_shape CHECK ((voided_at IS NULL) = (voided_by IS NULL) AND (voided_at IS NULL) = (void_reason IS NULL))
);
CREATE INDEX expenses_date_idx ON expenses (expense_date DESC, id DESC);

-- The only change a row ever takes is being voided, once.
CREATE FUNCTION guard_expense_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.voided_at IS NOT NULL THEN
    RAISE EXCEPTION 'a voided expense cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.expense_date <> OLD.expense_date OR NEW.description <> OLD.description OR NEW.category <> OLD.category OR NEW.amount_cents <> OLD.amount_cents
     OR NEW.paid_by <> OLD.paid_by OR NEW.paid_with <> OLD.paid_with OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'an expense can only be voided, not edited' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER expenses_guard BEFORE UPDATE ON expenses FOR EACH ROW EXECUTE FUNCTION guard_expense_update();
CREATE TRIGGER expenses_no_delete BEFORE DELETE ON expenses FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER expenses_no_truncate BEFORE TRUNCATE ON expenses FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A new permission. A database that was set up before this stage has no row for it and no grant, and bootstrap is not
-- re-run on a live system, so this migration adds both there. A database that has not been bootstrapped yet is left alone
-- (it must hold no permissions before bootstrap) and gets the same through bootstrap, which reads the catalogue.
-- Both statements are safe to repeat.
INSERT INTO permissions (key, description)
SELECT 'expenses.manage', 'Record and void expenses' WHERE EXISTS (SELECT 1 FROM roles WHERE code = 'business_manager')
ON CONFLICT (key) DO NOTHING;
INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r, permissions p WHERE r.code = 'business_manager' AND p.key = 'expenses.manage'
ON CONFLICT DO NOTHING;
