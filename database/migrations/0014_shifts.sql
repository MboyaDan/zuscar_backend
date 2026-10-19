-- Stage 12: cashier shifts and cash movements (docs/schema-constraints.md section 4, behaviour.md section 10, AC-09).
-- A shift is opened with a float, takes cash payments and paid-in/paid-out/expense entries, and is closed with a
-- declared count. Expected cash is never stored while the shift is open: it is derived from rows. The expected figure
-- and the variance are snapshotted once at close and never absorbed or edited (AC-09).

CREATE TABLE cashier_shifts (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id             BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  status              TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed', 'reconciled')),
  opened_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  opening_float_cents BIGINT      NOT NULL CHECK (opening_float_cents >= 0),
  closed_at           TIMESTAMPTZ,
  closed_by           BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  declared_cash_cents BIGINT      CHECK (declared_cash_cents IS NULL OR declared_cash_cents >= 0),
  expected_cash_cents BIGINT,
  variance_cents      BIGINT,
  reconciled_at       TIMESTAMPTZ,
  reconciled_by       BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  notes               TEXT        NOT NULL DEFAULT '' CHECK (char_length(notes) <= 500),
  CONSTRAINT cashier_shifts_closed_fields CHECK (
    (status = 'open') = (closed_at IS NULL AND closed_by IS NULL AND declared_cash_cents IS NULL AND expected_cash_cents IS NULL AND variance_cents IS NULL)),
  CONSTRAINT cashier_shifts_variance CHECK (variance_cents IS NULL OR variance_cents = declared_cash_cents - expected_cash_cents),
  CONSTRAINT cashier_shifts_reconciled_fields CHECK ((status = 'reconciled') = (reconciled_at IS NOT NULL AND reconciled_by IS NOT NULL))
);
-- One open shift per person.
CREATE UNIQUE INDEX cashier_shifts_one_open_key ON cashier_shifts (user_id) WHERE status = 'open';
CREATE INDEX cashier_shifts_user_idx ON cashier_shifts (user_id, id DESC);

-- Who the shift belongs to, when it opened and its float never change; the status only moves open -> closed -> reconciled,
-- and the closing figures are written once.
CREATE FUNCTION guard_shift_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.user_id <> OLD.user_id OR NEW.opened_at <> OLD.opened_at OR NEW.opening_float_cents <> OLD.opening_float_cents THEN
    RAISE EXCEPTION 'a shift keeps its owner, opening time and float' USING ERRCODE = '23514';
  END IF;
  IF NEW.status <> OLD.status AND NOT ((OLD.status = 'open' AND NEW.status = 'closed') OR (OLD.status = 'closed' AND NEW.status = 'reconciled')) THEN
    RAISE EXCEPTION 'a shift cannot move from % to %', OLD.status, NEW.status USING ERRCODE = '23514';
  END IF;
  IF OLD.status <> 'open' AND (NEW.closed_at IS DISTINCT FROM OLD.closed_at OR NEW.closed_by IS DISTINCT FROM OLD.closed_by
     OR NEW.declared_cash_cents IS DISTINCT FROM OLD.declared_cash_cents OR NEW.expected_cash_cents IS DISTINCT FROM OLD.expected_cash_cents
     OR NEW.variance_cents IS DISTINCT FROM OLD.variance_cents) THEN
    RAISE EXCEPTION 'the closing figures of a shift are never changed' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER cashier_shifts_guard BEFORE UPDATE ON cashier_shifts FOR EACH ROW EXECUTE FUNCTION guard_shift_update();
CREATE TRIGGER cashier_shifts_no_delete BEFORE DELETE ON cashier_shifts FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER cashier_shifts_no_truncate BEFORE TRUNCATE ON cashier_shifts FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TABLE cash_movements (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  shift_id     BIGINT      NOT NULL REFERENCES cashier_shifts (id) ON DELETE RESTRICT,
  kind         TEXT        NOT NULL CHECK (kind IN ('paid_in', 'paid_out', 'expense')),
  amount_cents BIGINT      NOT NULL CHECK (amount_cents > 0),
  reason       TEXT        NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 200),
  created_by   BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cash_movements_shift_idx ON cash_movements (shift_id, id);
CREATE TRIGGER cash_movements_immutable BEFORE UPDATE OR DELETE ON cash_movements FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER cash_movements_no_truncate BEFORE TRUNCATE ON cash_movements FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Money only lands on a shift that is open: a cash movement, or a payment, cannot be added to a closed shift.
CREATE FUNCTION require_open_shift() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE st TEXT;
BEGIN
  IF NEW.shift_id IS NULL THEN RETURN NEW; END IF;
  SELECT status INTO st FROM cashier_shifts WHERE id = NEW.shift_id;
  IF st IS DISTINCT FROM 'open' THEN
    RAISE EXCEPTION 'shift % is not open', NEW.shift_id USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER cash_movements_open_shift BEFORE INSERT ON cash_movements FOR EACH ROW EXECUTE FUNCTION require_open_shift();
CREATE TRIGGER payments_open_shift BEFORE INSERT ON payments FOR EACH ROW EXECUTE FUNCTION require_open_shift();

-- The foreign keys Stage 11 left open.
ALTER TABLE sales ADD CONSTRAINT sales_shift_fk FOREIGN KEY (shift_id) REFERENCES cashier_shifts (id) ON DELETE RESTRICT;
ALTER TABLE payments ADD CONSTRAINT payments_shift_fk FOREIGN KEY (shift_id) REFERENCES cashier_shifts (id) ON DELETE RESTRICT;
CREATE INDEX payments_shift_idx ON payments (shift_id);
