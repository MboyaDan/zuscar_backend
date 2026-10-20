-- Stage 19: suppliers, purchases and receiving (docs/schema-constraints.md section 5, AC-06).
-- A purchase is written as a draft, then received once: receiving writes one 'receipt' movement per line in the same
-- transaction. A received purchase never changes; a wrong receipt is undone by reversing its movement (NN-08).

CREATE TABLE suppliers (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name       TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
  contact    TEXT        CHECK (contact IS NULL OR char_length(btrim(contact)) BETWEEN 1 AND 200),
  active     BOOLEAN     NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX suppliers_name_key ON suppliers (lower(name));
CREATE TRIGGER suppliers_no_delete BEFORE DELETE ON suppliers FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER suppliers_no_truncate BEFORE TRUNCATE ON suppliers FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TABLE purchases (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  purchase_no  BIGINT      NOT NULL, -- from document_counters 'purchase': gapless, never reused (A-05)
  supplier_id  BIGINT      NOT NULL REFERENCES suppliers (id) ON DELETE RESTRICT,
  status       TEXT        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'received', 'cancelled')),
  invoice_ref  TEXT        CHECK (invoice_ref IS NULL OR char_length(btrim(invoice_ref)) BETWEEN 1 AND 60),
  note         TEXT        CHECK (note IS NULL OR char_length(btrim(note)) BETWEEN 1 AND 200),
  location_id  BIGINT      REFERENCES stock_locations (id) ON DELETE RESTRICT, -- where it arrived; set when received
  created_by   BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  received_by  BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  received_at  TIMESTAMPTZ,
  cancelled_by BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  cancelled_at TIMESTAMPTZ,
  CONSTRAINT purchases_no_key UNIQUE (purchase_no),
  CONSTRAINT purchases_received_shape CHECK ((status = 'received') = (received_at IS NOT NULL AND received_by IS NOT NULL AND location_id IS NOT NULL)),
  CONSTRAINT purchases_cancelled_shape CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL))
);
CREATE INDEX purchases_supplier_idx ON purchases (supplier_id);

-- A draft may be received or cancelled, once. Nothing else about a purchase changes, and it is never deleted.
CREATE FUNCTION guard_purchase_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' THEN
    RAISE EXCEPTION 'a purchase that is received or cancelled cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.purchase_no <> OLD.purchase_no OR NEW.supplier_id <> OLD.supplier_id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a purchase keeps its number, supplier and author' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER purchases_guard BEFORE UPDATE ON purchases FOR EACH ROW EXECUTE FUNCTION guard_purchase_update();
CREATE TRIGGER purchases_no_delete BEFORE DELETE ON purchases FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER purchases_no_truncate BEFORE TRUNCATE ON purchases FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- unit_cost_cents is the price of one entered unit (one crate, one bottle), as the supplier's invoice shows it.
CREATE TABLE purchase_items (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  purchase_id     BIGINT NOT NULL REFERENCES purchases (id) ON DELETE RESTRICT,
  item_id         BIGINT NOT NULL REFERENCES inventory_items (id) ON DELETE RESTRICT,
  entered_qty     BIGINT NOT NULL CHECK (entered_qty > 0),
  entered_unit_id BIGINT NOT NULL REFERENCES units_of_measure (id) ON DELETE RESTRICT,
  qty_base        BIGINT NOT NULL CHECK (qty_base > 0),
  unit_cost_cents BIGINT NOT NULL CHECK (unit_cost_cents >= 0),
  CONSTRAINT purchase_items_once UNIQUE (purchase_id, item_id)
);
CREATE TRIGGER purchase_items_immutable BEFORE UPDATE OR DELETE ON purchase_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER purchase_items_no_truncate BEFORE TRUNCATE ON purchase_items FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A purchase puts each item into a place once, so a retry can never receive twice.
CREATE UNIQUE INDEX inventory_movements_one_receipt ON inventory_movements (ref_id, item_id, location_id)
  WHERE movement_type = 'receipt' AND ref_type = 'purchase';
-- A receipt always names its purchase and carries its cost.
ALTER TABLE inventory_movements ADD CONSTRAINT inventory_movements_receipt_ref
  CHECK (movement_type <> 'receipt' OR (ref_type = 'purchase' AND ref_id IS NOT NULL AND unit_cost_cents IS NOT NULL));
