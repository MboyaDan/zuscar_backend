-- Stage 23: check-in, the stay, the guest folio, and room status history (docs/schema-constraints.md section 6, AC-08).
-- A folio is the guest's running account. Charges and adjustments are rows that never change (NN-08); a payment is not
-- a folio row but a payment (NN-10). The balance is always worked out: charges minus confirmed payments minus any write-off.

CREATE TABLE room_status_history (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  room_id     BIGINT      NOT NULL REFERENCES rooms (id) ON DELETE RESTRICT,
  from_status TEXT        NOT NULL,
  to_status   TEXT        NOT NULL CHECK (to_status IN ('vacant_ready', 'reserved', 'occupied', 'dirty', 'cleaning', 'out_of_service')),
  changed_by  BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  reason      TEXT        CHECK (reason IS NULL OR char_length(btrim(reason)) BETWEEN 1 AND 200),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT room_status_history_changed CHECK (from_status <> to_status)
);
CREATE INDEX room_status_history_room_idx ON room_status_history (room_id, id DESC);
CREATE TRIGGER room_status_history_no_update BEFORE UPDATE ON room_status_history FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER room_status_history_no_delete BEFORE DELETE ON room_status_history FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER room_status_history_no_truncate BEFORE TRUNCATE ON room_status_history FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- One stay per reservation (S-12). expected_out and room_id follow the reservation when a stay is extended or moved.
CREATE TABLE stays (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  reservation_id BIGINT      NOT NULL REFERENCES reservations (id) ON DELETE RESTRICT,
  room_id        BIGINT      NOT NULL REFERENCES rooms (id) ON DELETE RESTRICT,
  guest_id       BIGINT      NOT NULL REFERENCES guests (id) ON DELETE RESTRICT,
  status         TEXT        NOT NULL DEFAULT 'in_house' CHECK (status IN ('in_house', 'checked_out')),
  expected_out   DATE        NOT NULL,
  checked_in_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  checked_in_by  BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  checked_out_at TIMESTAMPTZ,
  checked_out_by BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT stays_reservation_key UNIQUE (reservation_id),
  CONSTRAINT stays_out_shape CHECK ((status = 'checked_out') = (checked_out_at IS NOT NULL AND checked_out_by IS NOT NULL))
);
-- A room holds one guest at a time, whatever the application checked first.
CREATE UNIQUE INDEX stays_one_in_house_per_room ON stays (room_id) WHERE status = 'in_house';

CREATE FUNCTION guard_stay_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'checked_out' THEN
    RAISE EXCEPTION 'a stay that has ended cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.reservation_id <> OLD.reservation_id OR NEW.guest_id <> OLD.guest_id OR NEW.checked_in_at <> OLD.checked_in_at OR NEW.checked_in_by <> OLD.checked_in_by THEN
    RAISE EXCEPTION 'a stay keeps its reservation, guest and check-in' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER stays_guard BEFORE UPDATE ON stays FOR EACH ROW EXECUTE FUNCTION guard_stay_update();
CREATE TRIGGER stays_no_delete BEFORE DELETE ON stays FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER stays_no_truncate BEFORE TRUNCATE ON stays FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A guest in the house may change room (a room move) and leave later (an extension); the arrival date, the guest and the
-- rate stay as they were. This replaces the Stage 22 guard, which froze the room.
CREATE OR REPLACE FUNCTION guard_reservation_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IN ('checked_out', 'cancelled', 'no_show') THEN
    RAISE EXCEPTION 'a reservation that has ended cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.reservation_no <> OLD.reservation_no OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a reservation keeps its number and author' USING ERRCODE = '23514';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'tentative'  AND NEW.status IN ('confirmed', 'cancelled'))
    OR (OLD.status = 'confirmed'  AND NEW.status IN ('cancelled', 'no_show', 'checked_in'))
    OR (OLD.status = 'checked_in' AND NEW.status = 'checked_out')) THEN
    RAISE EXCEPTION 'a % reservation cannot become %', OLD.status, NEW.status USING ERRCODE = '23514';
  END IF;
  IF OLD.status = 'checked_in' AND (NEW.guest_id <> OLD.guest_id OR NEW.check_in <> OLD.check_in OR NEW.nightly_rate_cents <> OLD.nightly_rate_cents) THEN
    RAISE EXCEPTION 'a guest who is in the house keeps the guest, the arrival date and the rate' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;

CREATE TABLE guest_folios (
  id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  folio_no              BIGINT      NOT NULL, -- from document_counters 'folio': gapless, never reused (A-05)
  stay_id               BIGINT      REFERENCES stays (id) ON DELETE RESTRICT, -- null is allowed for folios that are not a stay, later
  guest_id              BIGINT      NOT NULL REFERENCES guests (id) ON DELETE RESTRICT,
  status                TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
  write_off_cents       BIGINT      NOT NULL DEFAULT 0 CHECK (write_off_cents >= 0),
  write_off_approved_by BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  write_off_reason      TEXT        CHECK (write_off_reason IS NULL OR char_length(btrim(write_off_reason)) BETWEEN 1 AND 200),
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  closed_at             TIMESTAMPTZ,
  closed_by             BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT guest_folios_no_key UNIQUE (folio_no),
  CONSTRAINT guest_folios_stay_key UNIQUE (stay_id),
  CONSTRAINT guest_folios_write_off_approved CHECK (write_off_cents = 0 OR (write_off_approved_by IS NOT NULL AND write_off_reason IS NOT NULL)),
  CONSTRAINT guest_folios_closed_shape CHECK ((status = 'closed') = (closed_at IS NOT NULL AND closed_by IS NOT NULL))
);
CREATE INDEX guest_folios_guest_idx ON guest_folios (guest_id);

-- The write-off is decided once. A folio keeps its number, stay, guest and date.
CREATE FUNCTION guard_folio_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.folio_no <> OLD.folio_no OR NEW.stay_id IS DISTINCT FROM OLD.stay_id OR NEW.guest_id <> OLD.guest_id OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a folio keeps its number, stay and guest' USING ERRCODE = '23514';
  END IF;
  IF OLD.write_off_cents > 0 AND (NEW.write_off_cents <> OLD.write_off_cents OR NEW.write_off_approved_by IS DISTINCT FROM OLD.write_off_approved_by) THEN
    RAISE EXCEPTION 'a write-off cannot be changed; post an adjustment instead' USING ERRCODE = '23514';
  END IF;
  IF OLD.status = 'closed' AND NEW.status = 'closed' THEN
    RAISE EXCEPTION 'a closed folio cannot change until it is reopened' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER guest_folios_guard BEFORE UPDATE ON guest_folios FOR EACH ROW EXECUTE FUNCTION guard_folio_update();
CREATE TRIGGER guest_folios_no_delete BEFORE DELETE ON guest_folios FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER guest_folios_no_truncate BEFORE TRUNCATE ON guest_folios FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- amount_cents is signed: a charge is positive, a credit is negative. Tax is already inside the amount (A-04); tax_cents
-- is the part of it that is tax, kept so reports need no recalculation. Rows never change; a correction is a new row.
CREATE TABLE folio_items (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  folio_id         BIGINT      NOT NULL REFERENCES guest_folios (id) ON DELETE RESTRICT,
  kind             TEXT        NOT NULL CHECK (kind IN ('room_night', 'pos_charge', 'adjustment')),
  description      TEXT        NOT NULL CHECK (char_length(btrim(description)) BETWEEN 1 AND 200),
  amount_cents     BIGINT      NOT NULL CHECK (amount_cents BETWEEN -10000000000 AND 10000000000),
  tax_rate_bp      INTEGER     NOT NULL DEFAULT 0 CHECK (tax_rate_bp BETWEEN 0 AND 10000),
  tax_cents        BIGINT      NOT NULL DEFAULT 0,
  night_date       DATE,
  sale_id          BIGINT      REFERENCES sales (id) ON DELETE RESTRICT,
  reason           TEXT        CHECK (reason IS NULL OR char_length(btrim(reason)) BETWEEN 1 AND 200),
  reverses_item_id BIGINT      REFERENCES folio_items (id) ON DELETE RESTRICT,
  posted_by        BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT folio_items_night_shape CHECK ((kind = 'room_night') = (night_date IS NOT NULL)),
  CONSTRAINT folio_items_sale_shape CHECK ((kind = 'pos_charge') = (sale_id IS NOT NULL)),
  CONSTRAINT folio_items_adjustment_reason CHECK (kind <> 'adjustment' OR reason IS NOT NULL),
  CONSTRAINT folio_items_amount_sign CHECK ((kind = 'room_night' AND amount_cents >= 0) OR (kind = 'pos_charge' AND amount_cents > 0) OR (kind = 'adjustment' AND amount_cents <> 0))
);
CREATE INDEX folio_items_folio_idx ON folio_items (folio_id, id);
CREATE UNIQUE INDEX folio_items_one_night_key ON folio_items (folio_id, night_date) WHERE kind = 'room_night';
CREATE UNIQUE INDEX folio_items_one_sale_key ON folio_items (sale_id) WHERE kind = 'pos_charge';
-- An item is credited back once, in full.
CREATE UNIQUE INDEX folio_items_one_reversal_key ON folio_items (reverses_item_id) WHERE reverses_item_id IS NOT NULL;
CREATE TRIGGER folio_items_no_update BEFORE UPDATE ON folio_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER folio_items_no_delete BEFORE DELETE ON folio_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER folio_items_no_truncate BEFORE TRUNCATE ON folio_items FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Nothing is posted to, or paid into, a folio that is closed.
CREATE FUNCTION require_open_folio() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE st TEXT;
BEGIN
  IF NEW.folio_id IS NULL THEN RETURN NEW; END IF;
  SELECT status INTO st FROM guest_folios WHERE id = NEW.folio_id;
  IF st IS DISTINCT FROM 'open' THEN
    RAISE EXCEPTION 'folio % is not open', NEW.folio_id USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER folio_items_open_folio BEFORE INSERT ON folio_items FOR EACH ROW EXECUTE FUNCTION require_open_folio();
