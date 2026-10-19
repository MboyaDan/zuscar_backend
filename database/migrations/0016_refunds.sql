-- Stage 15a: voids and refunds (ASSUMPTIONS.md S-18 and S-63).
-- A sale and its lines are never edited. A correction is a new, immutable record: a refund (some lines, any day) or a
-- void (the whole sale, same business date, no refund yet). Both are rows in `refunds`; a void is kind 'void' and lists
-- every line. The money that went back to the customer is in `refund_payouts`, one row per method, so a cash payout
-- lands on the drawer (shift) of the person who handed the cash over, and expected cash stays derived from rows (AC-09).
-- Payment rows are left as they were: what was received and what was returned are two separate ledgers.

CREATE TABLE refunds (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  refund_no     BIGINT      NOT NULL, -- from document_counters 'refund': gapless, never reused
  sale_id       BIGINT      NOT NULL REFERENCES sales (id) ON DELETE RESTRICT,
  kind          TEXT        NOT NULL CHECK (kind IN ('refund', 'void')),
  reason        TEXT        NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 200),
  approved_by   BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT, -- the person holding the permission who did it (as for discounts)
  created_by    BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  business_date DATE        NOT NULL,
  amount_cents  BIGINT      NOT NULL CHECK (amount_cents >= 0), -- value of the lines taken back, tax included
  tax_cents     BIGINT      NOT NULL CHECK (tax_cents >= 0),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT refunds_refund_no_key UNIQUE (refund_no),
  CONSTRAINT refunds_refund_has_value CHECK (kind = 'void' OR amount_cents > 0)
);
CREATE INDEX refunds_sale_idx ON refunds (sale_id, id);
CREATE UNIQUE INDEX refunds_one_void_key ON refunds (sale_id) WHERE kind = 'void';

CREATE TABLE refund_items (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  refund_id    BIGINT NOT NULL REFERENCES refunds (id) ON DELETE RESTRICT,
  sale_item_id BIGINT NOT NULL REFERENCES sale_items (id) ON DELETE RESTRICT,
  qty          INTEGER NOT NULL CHECK (qty > 0),
  amount_cents BIGINT NOT NULL CHECK (amount_cents >= 0),
  tax_cents    BIGINT NOT NULL CHECK (tax_cents >= 0),
  CONSTRAINT refund_items_once_per_refund UNIQUE (refund_id, sale_item_id)
);
CREATE INDEX refund_items_sale_item_idx ON refund_items (sale_item_id);

CREATE TABLE refund_payouts (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  refund_id    BIGINT NOT NULL REFERENCES refunds (id) ON DELETE RESTRICT,
  method       TEXT   NOT NULL CHECK (method IN ('cash', 'mpesa_manual', 'card_manual', 'bank_manual')),
  amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
  reference    TEXT   CHECK (reference IS NULL OR char_length(btrim(reference)) BETWEEN 1 AND 64),
  shift_id     BIGINT REFERENCES cashier_shifts (id) ON DELETE RESTRICT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT refund_payouts_once_per_method UNIQUE (refund_id, method),
  CONSTRAINT refund_payouts_cash_has_shift CHECK (method <> 'cash' OR shift_id IS NOT NULL) -- cash leaves an open drawer
);
CREATE INDEX refund_payouts_shift_idx ON refund_payouts (shift_id);

CREATE TRIGGER refunds_immutable BEFORE UPDATE OR DELETE ON refunds FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER refunds_no_truncate BEFORE TRUNCATE ON refunds FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER refund_items_immutable BEFORE UPDATE OR DELETE ON refund_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER refund_items_no_truncate BEFORE TRUNCATE ON refund_items FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER refund_payouts_immutable BEFORE UPDATE OR DELETE ON refund_payouts FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER refund_payouts_no_truncate BEFORE TRUNCATE ON refund_payouts FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Cash can only go out of a drawer that is open (same rule as money coming in).
CREATE TRIGGER refund_payouts_open_shift BEFORE INSERT ON refund_payouts FOR EACH ROW EXECUTE FUNCTION require_open_shift();

-- A void and a refund cannot share a sale: a void is only for a sale with no refunds, and nothing follows a void.
CREATE FUNCTION guard_refund_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind = 'void' AND EXISTS (SELECT 1 FROM refunds WHERE sale_id = NEW.sale_id) THEN
    RAISE EXCEPTION 'a sale that already has a refund cannot be voided' USING ERRCODE = '23514';
  END IF;
  IF NEW.kind = 'refund' AND EXISTS (SELECT 1 FROM refunds WHERE sale_id = NEW.sale_id AND kind = 'void') THEN
    RAISE EXCEPTION 'a voided sale cannot be refunded' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER refunds_guard BEFORE INSERT ON refunds FOR EACH ROW EXECUTE FUNCTION guard_refund_insert();

-- Quantity taken back for a line can never pass what was sold, however many refunds there are (checked again here
-- because the service check is the first line of defence, not the only one).
CREATE FUNCTION guard_refund_item_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE sold INTEGER; line_sale BIGINT; refund_sale BIGINT; already INTEGER;
BEGIN
  SELECT qty, sale_id INTO sold, line_sale FROM sale_items WHERE id = NEW.sale_item_id;
  SELECT sale_id INTO refund_sale FROM refunds WHERE id = NEW.refund_id;
  IF line_sale IS DISTINCT FROM refund_sale THEN
    RAISE EXCEPTION 'that line belongs to another sale' USING ERRCODE = '23514';
  END IF;
  SELECT COALESCE(sum(qty), 0) INTO already FROM refund_items WHERE sale_item_id = NEW.sale_item_id;
  IF already + NEW.qty > sold THEN
    RAISE EXCEPTION 'more of a line is being taken back (%) than was sold (%)', already + NEW.qty, sold USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER refund_items_guard BEFORE INSERT ON refund_items FOR EACH ROW EXECUTE FUNCTION guard_refund_item_insert();

-- Money returned by one method can never pass the confirmed money received by that method on the sale.
CREATE FUNCTION guard_refund_payout_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE sid BIGINT; received BIGINT; returned BIGINT;
BEGIN
  SELECT sale_id INTO sid FROM refunds WHERE id = NEW.refund_id;
  SELECT COALESCE(sum(amount_cents), 0) INTO received FROM payments WHERE sale_id = sid AND method = NEW.method AND status = 'confirmed';
  SELECT COALESCE(sum(p.amount_cents), 0) INTO returned FROM refund_payouts p JOIN refunds r ON r.id = p.refund_id WHERE r.sale_id = sid AND p.method = NEW.method;
  IF returned + NEW.amount_cents > received THEN
    RAISE EXCEPTION 'more % is being returned (%) than was received (%)', NEW.method, returned + NEW.amount_cents, received USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER refund_payouts_guard BEFORE INSERT ON refund_payouts FOR EACH ROW EXECUTE FUNCTION guard_refund_payout_insert();

-- A sale's status moves one way, and only with the record that explains it.
CREATE FUNCTION guard_sale_status() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status = OLD.status THEN RETURN NEW; END IF;
  IF NOT ((OLD.status = 'finalised' AND NEW.status IN ('partially_refunded', 'refunded', 'voided'))
       OR (OLD.status = 'partially_refunded' AND NEW.status = 'refunded')) THEN
    RAISE EXCEPTION 'a sale cannot move from % to %', OLD.status, NEW.status USING ERRCODE = '23514';
  END IF;
  IF NEW.status = 'voided' AND NOT EXISTS (SELECT 1 FROM refunds WHERE sale_id = NEW.id AND kind = 'void') THEN
    RAISE EXCEPTION 'a sale is voided by a void record' USING ERRCODE = '23514';
  END IF;
  IF NEW.status IN ('partially_refunded', 'refunded') AND NOT EXISTS (SELECT 1 FROM refunds WHERE sale_id = NEW.id AND kind = 'refund') THEN
    RAISE EXCEPTION 'a sale is refunded by a refund record' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER sales_status_guard BEFORE UPDATE ON sales FOR EACH ROW EXECUTE FUNCTION guard_sale_status();
