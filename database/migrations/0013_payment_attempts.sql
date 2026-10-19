-- Stage 11b: payment_attempts (docs/schema-constraints.md section 4, AC-02). Gated by DG-02: the table exists so the
-- provider-event rule is fixed in the schema now, but nothing writes to it until the M-Pesa route and its callback
-- authenticity are verified (Stage 24). A provider event can be recorded once: the same event delivered twice is
-- the same row, so a callback that is retried cannot create a second payment.
CREATE TABLE payment_attempts (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  payment_id        BIGINT      REFERENCES payments (id) ON DELETE RESTRICT,
  provider          TEXT        NOT NULL CHECK (char_length(btrim(provider)) BETWEEN 1 AND 40),
  provider_event_id TEXT        NOT NULL CHECK (char_length(btrim(provider_event_id)) BETWEEN 1 AND 128),
  status            TEXT        NOT NULL DEFAULT 'received',
  raw_ref           TEXT        CHECK (raw_ref IS NULL OR char_length(raw_ref) <= 200),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT payment_attempts_provider_event_key UNIQUE (provider, provider_event_id)
);
CREATE INDEX payment_attempts_payment_idx ON payment_attempts (payment_id);
CREATE TRIGGER payment_attempts_no_delete BEFORE DELETE ON payment_attempts FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER payment_attempts_no_truncate BEFORE TRUNCATE ON payment_attempts FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
