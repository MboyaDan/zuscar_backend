-- Stage 17: inventory (docs/schema-constraints.md section 5, ADR-07, AC-06).
-- Stock is an append-only ledger. A balance is never stored; it is the sum of an item's movements at a location.
-- A mistake is corrected by a compensating movement, never by editing or deleting one (NN-08).
-- Quantities are whole numbers in each item's base unit (ml, g, piece, ...), never decimals (S-03, NN-09).

CREATE TABLE units_of_measure (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  code       TEXT        NOT NULL CHECK (code ~ '^[a-z0-9_]{1,16}$'),
  name       TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 40),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT units_of_measure_code_key UNIQUE (code)
);
-- The three base units every venue needs. Bottles, crates, shots and so on are conversions per item, not code (brief 7.2).
INSERT INTO units_of_measure (code, name) VALUES ('pc', 'Piece'), ('ml', 'Millilitre'), ('g', 'Gram');

CREATE TABLE stock_locations (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  code       TEXT        NOT NULL CHECK (code ~ '^[a-z0-9_]{1,24}$'),
  name       TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 60),
  active     BOOLEAN     NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT stock_locations_code_key UNIQUE (code)
);
-- Where a sale takes stock from depends on the product's module: bar products from 'bar', restaurant products from 'kitchen' (S-72).
INSERT INTO stock_locations (code, name) VALUES ('main_store', 'Main store'), ('bar', 'Bar'), ('kitchen', 'Kitchen');

CREATE TABLE inventory_items (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name                TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
  sku                 TEXT        CHECK (sku IS NULL OR char_length(btrim(sku)) BETWEEN 1 AND 40),
  base_unit_id        BIGINT      NOT NULL REFERENCES units_of_measure (id) ON DELETE RESTRICT,
  low_stock_threshold BIGINT      CHECK (low_stock_threshold IS NULL OR low_stock_threshold >= 0), -- base units, across all locations
  active              BOOLEAN     NOT NULL DEFAULT true,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX inventory_items_name_key ON inventory_items (lower(name));
CREATE UNIQUE INDEX inventory_items_sku_key ON inventory_items (lower(sku)) WHERE sku IS NOT NULL;

-- An item's base unit is what its movements are counted in, so it can never change.
CREATE FUNCTION guard_inventory_item_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.base_unit_id <> OLD.base_unit_id THEN
    RAISE EXCEPTION 'an item keeps its base unit' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER inventory_items_guard BEFORE UPDATE ON inventory_items FOR EACH ROW EXECUTE FUNCTION guard_inventory_item_update();
CREATE TRIGGER inventory_items_no_delete BEFORE DELETE ON inventory_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER inventory_items_no_truncate BEFORE TRUNCATE ON inventory_items FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- "1 crate = 24 pieces": factor_num / factor_den base units per from_unit. The result must be whole when used (S-03).
CREATE TABLE unit_conversions (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  item_id      BIGINT NOT NULL REFERENCES inventory_items (id) ON DELETE RESTRICT,
  from_unit_id BIGINT NOT NULL REFERENCES units_of_measure (id) ON DELETE RESTRICT,
  to_unit_id   BIGINT NOT NULL REFERENCES units_of_measure (id) ON DELETE RESTRICT,
  factor_num   BIGINT NOT NULL CHECK (factor_num > 0 AND factor_num <= 1000000000),
  factor_den   BIGINT NOT NULL DEFAULT 1 CHECK (factor_den > 0 AND factor_den <= 1000000000),
  CONSTRAINT unit_conversions_key UNIQUE (item_id, from_unit_id, to_unit_id),
  CONSTRAINT unit_conversions_distinct CHECK (from_unit_id <> to_unit_id)
);
CREATE FUNCTION guard_unit_conversion() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE base BIGINT;
BEGIN
  SELECT base_unit_id INTO base FROM inventory_items WHERE id = NEW.item_id;
  IF NEW.to_unit_id <> base THEN
    RAISE EXCEPTION 'a conversion must end in the item''s base unit' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER unit_conversions_guard BEFORE INSERT OR UPDATE ON unit_conversions FOR EACH ROW EXECUTE FUNCTION guard_unit_conversion();

CREATE TABLE inventory_movements (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  item_id              BIGINT      NOT NULL REFERENCES inventory_items (id) ON DELETE RESTRICT,
  location_id          BIGINT      NOT NULL REFERENCES stock_locations (id) ON DELETE RESTRICT,
  qty_delta            BIGINT      NOT NULL CHECK (qty_delta <> 0), -- signed, base units
  movement_type        TEXT        NOT NULL CHECK (movement_type IN ('opening_balance', 'receipt', 'transfer_out', 'transfer_in', 'sale_consumption', 'sale_return',
                                                                     'recipe_consumption', 'wastage', 'breakage', 'complimentary', 'count_adjustment', 'adjustment', 'reversal')),
  reason               TEXT        CHECK (reason IS NULL OR char_length(btrim(reason)) BETWEEN 1 AND 200),
  entered_qty          BIGINT,     -- what the person typed, in the unit they typed it, for traceability
  entered_unit_id      BIGINT      REFERENCES units_of_measure (id) ON DELETE RESTRICT,
  unit_cost_cents      BIGINT      CHECK (unit_cost_cents IS NULL OR unit_cost_cents >= 0),
  ref_type             TEXT        CHECK (ref_type IS NULL OR ref_type IN ('sale', 'refund', 'purchase', 'stock_count')),
  ref_id               BIGINT,
  transfer_group       UUID,
  reverses_movement_id BIGINT      REFERENCES inventory_movements (id) ON DELETE RESTRICT,
  override_reason      TEXT        CHECK (override_reason IS NULL OR char_length(btrim(override_reason)) BETWEEN 1 AND 200),
  created_by           BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT inventory_movements_reverses_key UNIQUE (reverses_movement_id), -- a movement is reversed at most once
  CONSTRAINT inventory_movements_sign CHECK (
    (movement_type IN ('opening_balance', 'receipt', 'transfer_in', 'sale_return') AND qty_delta > 0)
    OR (movement_type IN ('transfer_out', 'sale_consumption', 'recipe_consumption', 'wastage', 'breakage', 'complimentary') AND qty_delta < 0)
    OR movement_type IN ('adjustment', 'count_adjustment', 'reversal')),
  CONSTRAINT inventory_movements_reason_needed CHECK (movement_type NOT IN ('wastage', 'breakage', 'complimentary', 'adjustment', 'count_adjustment', 'reversal') OR reason IS NOT NULL),
  CONSTRAINT inventory_movements_reversal_link CHECK ((movement_type = 'reversal') = (reverses_movement_id IS NOT NULL)),
  CONSTRAINT inventory_movements_ref_pair CHECK ((ref_type IS NULL) = (ref_id IS NULL)),
  CONSTRAINT inventory_movements_entered_pair CHECK ((entered_qty IS NULL) = (entered_unit_id IS NULL))
);
CREATE INDEX inventory_movements_item_location_idx ON inventory_movements (item_id, location_id);
CREATE INDEX inventory_movements_ref_idx ON inventory_movements (ref_type, ref_id);
-- A sale (or a refund putting stock back) takes or returns each item once per location, so a retry can never deduct twice (AC-06).
CREATE UNIQUE INDEX inventory_movements_once_per_ref ON inventory_movements (ref_type, ref_id, item_id, location_id, movement_type)
  WHERE movement_type IN ('sale_consumption', 'sale_return', 'recipe_consumption');
-- An opening balance is set once per item and location; after that, stock moves by receipts, counts and adjustments.
CREATE UNIQUE INDEX inventory_movements_one_opening ON inventory_movements (item_id, location_id) WHERE movement_type = 'opening_balance';

-- A reversal undoes exactly one earlier movement of a kind that can be undone this way: same item, same place, opposite amount.
CREATE FUNCTION guard_inventory_reversal() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE o inventory_movements%ROWTYPE;
BEGIN
  IF NEW.movement_type <> 'reversal' THEN RETURN NEW; END IF;
  SELECT * INTO o FROM inventory_movements WHERE id = NEW.reverses_movement_id;
  IF NOT FOUND OR o.movement_type IN ('reversal', 'transfer_in', 'transfer_out', 'sale_consumption', 'sale_return', 'recipe_consumption') THEN
    RAISE EXCEPTION 'this kind of movement is not undone by a reversal' USING ERRCODE = '23514';
  END IF;
  IF NEW.item_id <> o.item_id OR NEW.location_id <> o.location_id OR NEW.qty_delta <> -o.qty_delta THEN
    RAISE EXCEPTION 'a reversal must be the exact opposite of the movement it undoes' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER inventory_movements_reversal BEFORE INSERT ON inventory_movements FOR EACH ROW EXECUTE FUNCTION guard_inventory_reversal();
CREATE TRIGGER inventory_movements_immutable BEFORE UPDATE OR DELETE ON inventory_movements FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER inventory_movements_no_truncate BEFORE TRUNCATE ON inventory_movements FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- The balance of every item at every location, always worked out from the ledger (ADR-07, S-71).
CREATE VIEW stock_balances AS
  SELECT item_id, location_id, sum(qty_delta)::bigint AS qty_base, count(*)::bigint AS movements, max(created_at) AS last_movement_at
  FROM inventory_movements GROUP BY item_id, location_id;
