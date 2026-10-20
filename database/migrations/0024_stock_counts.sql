-- Stage 21: stock counts (docs/schema-constraints.md section 5, AC-06).
-- A count is a sheet for one place. Each line holds what was counted and what the books said at the moment it was
-- entered, so sales and deliveries that happen while people are counting are kept. Approving the count posts the
-- difference of each line as a count_adjustment movement; nothing is edited or deleted in the ledger (NN-08).

CREATE TABLE stock_counts (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  count_no     BIGINT      NOT NULL, -- from document_counters 'count': gapless, never reused (A-05)
  location_id  BIGINT      NOT NULL REFERENCES stock_locations (id) ON DELETE RESTRICT,
  status       TEXT        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'approved', 'cancelled')),
  note         TEXT        CHECK (note IS NULL OR char_length(btrim(note)) BETWEEN 1 AND 200),
  created_by   BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  approved_by  BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  approved_at  TIMESTAMPTZ,
  cancelled_by BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  cancelled_at TIMESTAMPTZ,
  CONSTRAINT stock_counts_no_key UNIQUE (count_no),
  CONSTRAINT stock_counts_approved_shape CHECK ((status = 'approved') = (approved_at IS NOT NULL AND approved_by IS NOT NULL)),
  CONSTRAINT stock_counts_cancelled_shape CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL))
);
CREATE INDEX stock_counts_status_idx ON stock_counts (status, id DESC);

CREATE FUNCTION guard_stock_count_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' THEN
    RAISE EXCEPTION 'a count that is approved or cancelled cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.count_no <> OLD.count_no OR NEW.location_id <> OLD.location_id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a count keeps its number, place and author' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER stock_counts_guard BEFORE UPDATE ON stock_counts FOR EACH ROW EXECUTE FUNCTION guard_stock_count_update();
CREATE TRIGGER stock_counts_no_delete BEFORE DELETE ON stock_counts FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER stock_counts_no_truncate BEFORE TRUNCATE ON stock_counts FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- counted_base is what was on the shelf; expected_base is what the books said when the line was entered.
CREATE TABLE stock_count_lines (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  count_id        BIGINT      NOT NULL REFERENCES stock_counts (id) ON DELETE RESTRICT,
  item_id         BIGINT      NOT NULL REFERENCES inventory_items (id) ON DELETE RESTRICT,
  counted_base    BIGINT      NOT NULL CHECK (counted_base >= 0),
  entered_qty     BIGINT      NOT NULL CHECK (entered_qty >= 0),
  entered_unit_id BIGINT      NOT NULL REFERENCES units_of_measure (id) ON DELETE RESTRICT,
  expected_base   BIGINT      NOT NULL,
  saved_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT stock_count_lines_once UNIQUE (count_id, item_id)
);

-- Lines can be changed or removed only while the count is a draft; after approval they are the record.
CREATE FUNCTION guard_stock_count_line() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE cid BIGINT;
BEGIN
  cid := CASE WHEN TG_OP = 'INSERT' THEN NEW.count_id ELSE OLD.count_id END;
  IF (SELECT status FROM stock_counts WHERE id = cid) <> 'draft' THEN
    RAISE EXCEPTION 'the lines of an approved or cancelled count cannot change' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER stock_count_lines_guard BEFORE INSERT OR UPDATE OR DELETE ON stock_count_lines FOR EACH ROW EXECUTE FUNCTION guard_stock_count_line();
CREATE TRIGGER stock_count_lines_no_truncate BEFORE TRUNCATE ON stock_count_lines FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A count adjusts an item in a place once, and always names its count.
CREATE UNIQUE INDEX inventory_movements_one_count_adjustment ON inventory_movements (ref_id, item_id, location_id)
  WHERE movement_type = 'count_adjustment' AND ref_type = 'stock_count';
ALTER TABLE inventory_movements ADD CONSTRAINT inventory_movements_count_ref
  CHECK (movement_type <> 'count_adjustment' OR (ref_type = 'stock_count' AND ref_id IS NOT NULL));
