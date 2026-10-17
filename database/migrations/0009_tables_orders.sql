-- Stage 10a: dining tables and orders (docs/schema-constraints.md section 3). Kitchen tickets and the idempotency
-- records arrive in Stage 10b. Money is integer cents.

CREATE TABLE dining_tables (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  label      TEXT        NOT NULL CHECK (char_length(btrim(label)) BETWEEN 1 AND 40),
  seats      INTEGER     NOT NULL CHECK (seats BETWEEN 1 AND 100),
  active     BOOLEAN     NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT dining_tables_label_key UNIQUE (label)
);

CREATE TABLE orders (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_no      BIGINT      NOT NULL, -- from document_counters 'order': gapless, never reused
  order_type    TEXT        NOT NULL CHECK (order_type IN ('dine_in', 'takeaway')),
  table_id      BIGINT      REFERENCES dining_tables (id) ON DELETE RESTRICT,
  status        TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'sent', 'finalised', 'cancelled')),
  opened_by     BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  waiter_id     BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  note          TEXT        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
  cancel_reason TEXT        CHECK (cancel_reason IS NULL OR char_length(btrim(cancel_reason)) BETWEEN 1 AND 200),
  cancelled_by  BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  cancelled_at  TIMESTAMPTZ,
  version       INTEGER     NOT NULL DEFAULT 1,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT orders_order_no_key UNIQUE (order_no),
  CONSTRAINT orders_table_matches_type CHECK ((order_type = 'dine_in') = (table_id IS NOT NULL)),
  CONSTRAINT orders_cancel_fields CHECK ((status = 'cancelled') = (cancel_reason IS NOT NULL AND cancelled_by IS NOT NULL AND cancelled_at IS NOT NULL))
);
CREATE INDEX orders_status_idx ON orders (status);
CREATE INDEX orders_open_table_idx ON orders (table_id) WHERE status IN ('open', 'sent');
CREATE INDEX orders_opened_by_idx ON orders (opened_by, id DESC);

-- A finalised or cancelled order never changes again, and no order is ever deleted (S-07).
CREATE FUNCTION guard_closed_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IN ('finalised', 'cancelled') THEN
    RAISE EXCEPTION 'order % is % and cannot be changed', OLD.id, OLD.status USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER orders_closed_guard BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION guard_closed_order();
CREATE TRIGGER orders_no_delete BEFORE DELETE ON orders FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER orders_no_truncate BEFORE TRUNCATE ON orders FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TABLE order_items (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id         BIGINT      NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
  product_id       BIGINT      NOT NULL REFERENCES products (id) ON DELETE RESTRICT,
  qty              INTEGER     NOT NULL CHECK (qty BETWEEN 1 AND 999),
  unit_price_cents BIGINT      NOT NULL CHECK (unit_price_cents >= 0), -- set by the server from products at add time
  status           TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
  note             TEXT        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
  cancel_reason    TEXT        CHECK (cancel_reason IS NULL OR char_length(btrim(cancel_reason)) BETWEEN 1 AND 200),
  cancelled_by     BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  cancelled_at     TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT order_items_cancel_fields CHECK ((status = 'cancelled') = (cancel_reason IS NOT NULL AND cancelled_by IS NOT NULL AND cancelled_at IS NOT NULL))
);
CREATE INDEX order_items_order_idx ON order_items (order_id);

-- Items can only be added or changed while their order is open or sent, and what was sold never changes.
CREATE FUNCTION guard_order_item() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE parent TEXT;
BEGIN
  SELECT status INTO parent FROM orders WHERE id = NEW.order_id;
  IF parent IS NULL OR parent NOT IN ('open', 'sent') THEN
    RAISE EXCEPTION 'order % is not open', NEW.order_id USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' AND (NEW.order_id <> OLD.order_id OR NEW.product_id <> OLD.product_id OR NEW.unit_price_cents <> OLD.unit_price_cents) THEN
    RAISE EXCEPTION 'order item % keeps its order, product and price', OLD.id USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER order_items_guard BEFORE INSERT OR UPDATE ON order_items FOR EACH ROW EXECUTE FUNCTION guard_order_item();
CREATE TRIGGER order_items_no_delete BEFORE DELETE ON order_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER order_items_no_truncate BEFORE TRUNCATE ON order_items FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- The modifiers chosen for a line, with the price change as it was at the time.
CREATE TABLE order_item_modifiers (
  order_item_id     BIGINT NOT NULL REFERENCES order_items (id) ON DELETE RESTRICT,
  modifier_id       BIGINT NOT NULL REFERENCES modifiers (id) ON DELETE RESTRICT,
  price_delta_cents BIGINT NOT NULL,
  PRIMARY KEY (order_item_id, modifier_id)
);
CREATE TRIGGER order_item_modifiers_immutable BEFORE UPDATE OR DELETE ON order_item_modifiers FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER order_item_modifiers_no_truncate BEFORE TRUNCATE ON order_item_modifiers FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
