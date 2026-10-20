-- Stage 22: room types, rooms, guests and reservations (docs/schema-constraints.md section 6, AC-07).
-- The number of rooms is data, not code. A room can never hold two blocking reservations on the same night:
-- the exclusion constraint is the authority, whatever the application checks first (ADR-06).

CREATE TABLE room_types (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name            TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 60),
  base_rate_cents BIGINT      NOT NULL CHECK (base_rate_cents BETWEEN 0 AND 10000000), -- per night, tax included (A-04)
  capacity        INTEGER     NOT NULL CHECK (capacity BETWEEN 1 AND 12),
  tax_rate_id     BIGINT      NOT NULL REFERENCES tax_rates (id) ON DELETE RESTRICT,
  active          BOOLEAN     NOT NULL DEFAULT true,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX room_types_name_key ON room_types (lower(name));
CREATE TRIGGER room_types_no_delete BEFORE DELETE ON room_types FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER room_types_no_truncate BEFORE TRUNCATE ON room_types FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- physical_status is the room's state on the floor; it is separate from any booking (brief 7.3). Changing it is Stage 23.
CREATE TABLE rooms (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  number          TEXT        NOT NULL CHECK (char_length(btrim(number)) BETWEEN 1 AND 10),
  room_type_id    BIGINT      NOT NULL REFERENCES room_types (id) ON DELETE RESTRICT,
  floor           INTEGER     CHECK (floor IS NULL OR floor BETWEEN -3 AND 60),
  physical_status TEXT        NOT NULL DEFAULT 'vacant_ready'
                  CHECK (physical_status IN ('vacant_ready', 'reserved', 'occupied', 'dirty', 'cleaning', 'out_of_service')),
  active          BOOLEAN     NOT NULL DEFAULT true,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX rooms_number_key ON rooms (lower(number));
CREATE INDEX rooms_type_idx ON rooms (room_type_id);
CREATE TRIGGER rooms_no_delete BEFORE DELETE ON rooms FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER rooms_no_truncate BEFORE TRUNCATE ON rooms FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- No ID document number is collected by default (S-11).
CREATE TABLE guests (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  full_name  TEXT        NOT NULL CHECK (char_length(btrim(full_name)) BETWEEN 1 AND 120),
  phone      TEXT        CHECK (phone IS NULL OR char_length(btrim(phone)) BETWEEN 5 AND 30),
  email      TEXT        CHECK (email IS NULL OR (char_length(email) <= 160 AND email ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$')),
  notes      TEXT        CHECK (notes IS NULL OR char_length(btrim(notes)) BETWEEN 1 AND 500),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX guests_name_idx ON guests (lower(full_name));
CREATE INDEX guests_phone_idx ON guests (phone);
CREATE TRIGGER guests_no_delete BEFORE DELETE ON guests FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER guests_no_truncate BEFORE TRUNCATE ON guests FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A night is booked from check_in up to, not including, check_out, so a departure and an arrival on the same day
-- do not clash. nightly_rate_cents is copied when the booking is made and never follows the room type afterwards.
CREATE TABLE reservations (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  reservation_no   BIGINT      NOT NULL, -- from document_counters 'reservation': gapless, never reused (A-05)
  guest_id         BIGINT      NOT NULL REFERENCES guests (id) ON DELETE RESTRICT,
  room_id          BIGINT      NOT NULL REFERENCES rooms (id) ON DELETE RESTRICT,
  check_in         DATE        NOT NULL,
  check_out        DATE        NOT NULL,
  status           TEXT        NOT NULL CHECK (status IN ('tentative', 'confirmed', 'checked_in', 'checked_out', 'cancelled', 'no_show')),
  adults           INTEGER     NOT NULL DEFAULT 1 CHECK (adults BETWEEN 1 AND 12),
  nightly_rate_cents BIGINT    NOT NULL CHECK (nightly_rate_cents BETWEEN 0 AND 10000000),
  notes            TEXT        CHECK (notes IS NULL OR char_length(btrim(notes)) BETWEEN 1 AND 500),
  cancelled_reason TEXT        CHECK (cancelled_reason IS NULL OR char_length(btrim(cancelled_reason)) BETWEEN 1 AND 200),
  cancelled_by     BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  cancelled_at     TIMESTAMPTZ,
  created_by       BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT reservations_no_key UNIQUE (reservation_no),
  CONSTRAINT reservations_dates_ok CHECK (check_out > check_in),
  CONSTRAINT reservations_max_stay CHECK (check_out - check_in <= 90),
  CONSTRAINT reservations_cancelled_shape CHECK ((status = 'cancelled') = (cancelled_reason IS NOT NULL AND cancelled_by IS NOT NULL AND cancelled_at IS NOT NULL)),
  CONSTRAINT reservations_no_overlap EXCLUDE USING gist (room_id WITH =, daterange(check_in, check_out, '[)') WITH &&)
    WHERE (status IN ('confirmed', 'checked_in'))
);
CREATE INDEX reservations_guest_idx ON reservations (guest_id);
CREATE INDEX reservations_room_dates_idx ON reservations (room_id, check_in);
CREATE INDEX reservations_status_idx ON reservations (status, check_in);

-- Moves the booking may make; ended bookings never change. A guest who is in the house keeps the room, the guest and
-- the arrival date; only the departure date (a stay extension) and the notes can still move.
CREATE FUNCTION guard_reservation_update() RETURNS trigger LANGUAGE plpgsql AS $$
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
  IF OLD.status = 'checked_in' AND (NEW.room_id <> OLD.room_id OR NEW.guest_id <> OLD.guest_id OR NEW.check_in <> OLD.check_in
       OR NEW.nightly_rate_cents <> OLD.nightly_rate_cents) THEN
    RAISE EXCEPTION 'a guest who is in the house keeps the room, the guest, the arrival date and the rate' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER reservations_guard BEFORE UPDATE ON reservations FOR EACH ROW EXECUTE FUNCTION guard_reservation_update();
CREATE TRIGGER reservations_no_delete BEFORE DELETE ON reservations FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER reservations_no_truncate BEFORE TRUNCATE ON reservations FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
