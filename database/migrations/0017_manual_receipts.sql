-- Stage 16: outage reconciliation (docs/schema-constraints.md section 4, AC-17).
-- When the internet or the system is down, sales are written on a paper receipt book. Afterwards a manager enters each
-- paper receipt once. The entry is an ordinary sale (source 'outage_reconciliation', no order) plus one row here that
-- remembers the paper receipt number, when the sale really happened, and who entered it. The paper number is unique on
-- the sale and on this table, so the same paper receipt can never be entered twice.

CREATE TABLE manual_receipt_reconciliations (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  manual_receipt_no    TEXT        NOT NULL CHECK (char_length(manual_receipt_no) BETWEEN 1 AND 32),
  sale_id              BIGINT      NOT NULL REFERENCES sales (id) ON DELETE RESTRICT,
  original_occurred_at TIMESTAMPTZ NOT NULL,
  paper_total_cents    BIGINT      NOT NULL CHECK (paper_total_cents >= 0), -- the total written on the paper receipt
  entered_by           BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  discrepancy_note     TEXT        CHECK (discrepancy_note IS NULL OR char_length(btrim(discrepancy_note)) BETWEEN 1 AND 200),
  source_note          TEXT        NOT NULL DEFAULT '' CHECK (char_length(source_note) <= 200),
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT manual_receipt_reconciliations_no_key UNIQUE (manual_receipt_no),
  CONSTRAINT manual_receipt_reconciliations_sale_key UNIQUE (sale_id)
);

-- The row must describe its sale: an outage sale with the same paper number, a paper total that either matches the
-- sale or is explained, and a time that is not in the future (a few minutes of clock difference are allowed).
CREATE FUNCTION guard_manual_receipt_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s sales%ROWTYPE;
BEGIN
  SELECT * INTO s FROM sales WHERE id = NEW.sale_id;
  IF NOT FOUND OR s.source <> 'outage_reconciliation' OR s.manual_receipt_no IS DISTINCT FROM NEW.manual_receipt_no THEN
    RAISE EXCEPTION 'a manual receipt entry must belong to an outage sale with the same paper receipt number' USING ERRCODE = '23514';
  END IF;
  IF (NEW.paper_total_cents <> s.total_cents) AND NEW.discrepancy_note IS NULL THEN
    RAISE EXCEPTION 'the paper total differs from the sale total, so a discrepancy note is required' USING ERRCODE = '23514';
  END IF;
  IF NEW.original_occurred_at > now() + interval '5 minutes' THEN
    RAISE EXCEPTION 'a paper receipt cannot be dated in the future' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER manual_receipt_reconciliations_guard BEFORE INSERT ON manual_receipt_reconciliations FOR EACH ROW EXECUTE FUNCTION guard_manual_receipt_insert();
CREATE TRIGGER manual_receipt_reconciliations_immutable BEFORE UPDATE OR DELETE ON manual_receipt_reconciliations FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER manual_receipt_reconciliations_no_truncate BEFORE TRUNCATE ON manual_receipt_reconciliations FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- The other direction: an outage sale cannot exist without its entry. Checked when the transaction commits, so the
-- sale and the entry are written together or not at all.
CREATE FUNCTION require_manual_receipt_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM manual_receipt_reconciliations WHERE sale_id = NEW.id) THEN
    RAISE EXCEPTION 'an outage sale needs its manual receipt entry' USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER sales_outage_has_entry AFTER INSERT ON sales DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW WHEN (NEW.source = 'outage_reconciliation') EXECUTE FUNCTION require_manual_receipt_entry();
