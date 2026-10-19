-- Stage 11a: sales, sale lines and payments (docs/schema-constraints.md section 4, behaviour.md 3-4).
-- Money is integer cents. With tax-inclusive prices (A-04) a sale's total is subtotal minus discount and the tax
-- is the part of that total that is tax; it is stored per line so a later rate change never alters a sale.
-- Not here yet: refunds and voids (Stage 13), shifts (Stage 12), folios, stock movements, receipts to print.

CREATE TABLE sales (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  receipt_no        BIGINT      NOT NULL, -- from document_counters 'receipt': gapless, never reused (A-05)
  order_id          BIGINT      REFERENCES orders (id) ON DELETE RESTRICT, -- null for outage entries
  business_date     DATE        NOT NULL,
  subtotal_cents    BIGINT      NOT NULL CHECK (subtotal_cents >= 0),
  discount_cents    BIGINT      NOT NULL DEFAULT 0 CHECK (discount_cents >= 0),
  tax_cents         BIGINT      NOT NULL CHECK (tax_cents >= 0),
  total_cents       BIGINT      NOT NULL CHECK (total_cents >= 0),
  status            TEXT        NOT NULL DEFAULT 'finalised' CHECK (status IN ('finalised', 'partially_refunded', 'refunded', 'voided')),
  payment_status    TEXT        NOT NULL DEFAULT 'unpaid' CHECK (payment_status IN ('unpaid', 'partially_paid', 'paid', 'charged_to_folio')),
  source            TEXT        NOT NULL DEFAULT 'pos' CHECK (source IN ('pos', 'outage_reconciliation')),
  manual_receipt_no TEXT,
  folio_id          BIGINT,     -- foreign key added with the folios tables
  cashier_id        BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  waiter_id         BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  shift_id          BIGINT,     -- foreign key added with the shifts table (Stage 12)
  tax_status        TEXT        NOT NULL CHECK (tax_status IN ('provisional_unverified', 'verified')), -- the tax configuration status when sold
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT sales_receipt_no_key UNIQUE (receipt_no),
  CONSTRAINT sales_order_id_key UNIQUE (order_id), -- an order is finalised at most once (AC-01)
  CONSTRAINT sales_manual_receipt_no_key UNIQUE (manual_receipt_no),
  CONSTRAINT sales_total_matches CHECK (total_cents = subtotal_cents - discount_cents),
  CONSTRAINT sales_outage_iff_manual CHECK ((source = 'outage_reconciliation') = (manual_receipt_no IS NOT NULL)),
  CONSTRAINT sales_folio_iff_charged CHECK ((payment_status = 'charged_to_folio') = (folio_id IS NOT NULL))
);
CREATE INDEX sales_business_date_idx ON sales (business_date);
CREATE INDEX sales_cashier_idx ON sales (cashier_id, business_date);

-- A finalised sale never changes except for its status and payment status (and the time that was set).
CREATE FUNCTION guard_sale_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ROW(NEW.id, NEW.receipt_no, NEW.order_id, NEW.business_date, NEW.subtotal_cents, NEW.discount_cents, NEW.tax_cents, NEW.total_cents,
         NEW.source, NEW.manual_receipt_no, NEW.folio_id, NEW.cashier_id, NEW.waiter_id, NEW.shift_id, NEW.tax_status, NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.receipt_no, OLD.order_id, OLD.business_date, OLD.subtotal_cents, OLD.discount_cents, OLD.tax_cents, OLD.total_cents,
         OLD.source, OLD.manual_receipt_no, OLD.folio_id, OLD.cashier_id, OLD.waiter_id, OLD.shift_id, OLD.tax_status, OLD.created_at) THEN
    RAISE EXCEPTION 'a sale keeps its receipt number, lines and amounts; only its status can change' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER sales_guard BEFORE UPDATE ON sales FOR EACH ROW EXECUTE FUNCTION guard_sale_update();
CREATE TRIGGER sales_no_delete BEFORE DELETE ON sales FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER sales_no_truncate BEFORE TRUNCATE ON sales FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- What was sold, as it was at the time (AC-01). Fully immutable.
CREATE TABLE sale_items (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  sale_id              BIGINT NOT NULL REFERENCES sales (id) ON DELETE RESTRICT,
  order_item_id        BIGINT REFERENCES order_items (id) ON DELETE RESTRICT,
  product_id           BIGINT NOT NULL REFERENCES products (id) ON DELETE RESTRICT,
  name_snapshot        TEXT   NOT NULL,
  module               TEXT   NOT NULL CHECK (module IN ('restaurant', 'bar')),
  qty                  INTEGER NOT NULL CHECK (qty > 0),
  unit_price_cents     BIGINT NOT NULL CHECK (unit_price_cents >= 0),
  modifiers_text       TEXT   NOT NULL DEFAULT '',
  modifiers_cents      BIGINT NOT NULL DEFAULT 0, -- per unit, the sum of the chosen modifiers' price changes
  discount_cents       BIGINT NOT NULL DEFAULT 0,
  discount_reason      TEXT,
  discount_approved_by BIGINT REFERENCES users (id) ON DELETE RESTRICT,
  tax_code             TEXT   NOT NULL,
  tax_treatment        TEXT   NOT NULL CHECK (tax_treatment IN ('standard', 'zero_rated', 'exempt')),
  tax_rate_bp          INTEGER NOT NULL CHECK (tax_rate_bp BETWEEN 0 AND 10000),
  tax_cents            BIGINT NOT NULL CHECK (tax_cents >= 0),
  line_total_cents     BIGINT NOT NULL CHECK (line_total_cents >= 0),
  CONSTRAINT sale_items_discount_range CHECK (discount_cents BETWEEN 0 AND (unit_price_cents + modifiers_cents) * qty),
  CONSTRAINT sale_items_line_total CHECK (line_total_cents = (unit_price_cents + modifiers_cents) * qty - discount_cents),
  CONSTRAINT sale_items_discount_documented CHECK (discount_cents = 0 OR (discount_reason IS NOT NULL AND char_length(btrim(discount_reason)) > 0 AND discount_approved_by IS NOT NULL))
);
CREATE INDEX sale_items_sale_idx ON sale_items (sale_id);
CREATE TRIGGER sale_items_immutable BEFORE UPDATE OR DELETE ON sale_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER sale_items_no_truncate BEFORE TRUNCATE ON sale_items FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- One table of money received (S-09). Room charge is never a payment method (NN-10).
CREATE TABLE payments (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  sale_id      BIGINT      REFERENCES sales (id) ON DELETE RESTRICT,
  folio_id     BIGINT,     -- foreign key added with the folios tables
  purpose      TEXT        NOT NULL CHECK (purpose IN ('sale_payment', 'deposit', 'folio_settlement')),
  method       TEXT        NOT NULL CHECK (method IN ('cash', 'mpesa_manual', 'card_manual', 'bank_manual')),
  status       TEXT        NOT NULL CHECK (status IN ('pending_confirmation', 'confirmed', 'failed', 'reversed')),
  amount_cents BIGINT      NOT NULL CHECK (amount_cents > 0),
  reference    TEXT        CHECK (reference IS NULL OR char_length(btrim(reference)) BETWEEN 1 AND 64),
  shift_id     BIGINT,     -- foreign key added with the shifts table (Stage 12)
  created_by   BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  confirmed_by BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  confirmed_at TIMESTAMPTZ,
  failed_by    BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  failed_at    TIMESTAMPTZ,
  note         TEXT        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT payments_one_target CHECK ((sale_id IS NULL) <> (folio_id IS NULL)),
  CONSTRAINT payments_confirmed_fields CHECK ((status IN ('confirmed', 'reversed')) = (confirmed_at IS NOT NULL)),
  CONSTRAINT payments_failed_fields CHECK ((status = 'failed') = (failed_at IS NOT NULL))
);
-- A typed M-Pesa, card or bank reference can be recorded once while it is pending or confirmed (AC-02, R-05).
CREATE UNIQUE INDEX payments_method_reference_key ON payments (method, reference)
  WHERE reference IS NOT NULL AND status IN ('pending_confirmation', 'confirmed');
CREATE INDEX payments_sale_idx ON payments (sale_id);
CREATE INDEX payments_status_idx ON payments (status);

-- Amount, method and target never change; the status only moves pending -> confirmed or failed, confirmed -> reversed (S-10).
CREATE FUNCTION guard_payment_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.sale_id IS DISTINCT FROM OLD.sale_id OR NEW.folio_id IS DISTINCT FROM OLD.folio_id OR NEW.purpose <> OLD.purpose OR NEW.method <> OLD.method
     OR NEW.amount_cents <> OLD.amount_cents OR NEW.reference IS DISTINCT FROM OLD.reference OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a payment keeps its amount, method, reference and target' USING ERRCODE = '23514';
  END IF;
  IF NEW.status <> OLD.status AND NOT ((OLD.status = 'pending_confirmation' AND NEW.status IN ('confirmed', 'failed')) OR (OLD.status = 'confirmed' AND NEW.status = 'reversed')) THEN
    RAISE EXCEPTION 'a payment cannot move from % to %', OLD.status, NEW.status USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payments_guard BEFORE UPDATE ON payments FOR EACH ROW EXECUTE FUNCTION guard_payment_update();
CREATE TRIGGER payments_no_delete BEFORE DELETE ON payments FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER payments_no_truncate BEFORE TRUNCATE ON payments FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
