-- Stage 10b-2: idempotency records (docs/adr/ADR-08, docs/schema-constraints.md section 2, S-06).
-- The service claims a key inside the same transaction as the work it protects and stores the response in that
-- transaction too, so a crash leaves either both the work and the stored response or neither. Other callers see
-- only committed rows, so a record that is visible is always completed; a concurrent duplicate waits on the unique
-- index until the first transaction ends.

CREATE TABLE idempotency_records (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  idem_key        TEXT        NOT NULL CHECK (char_length(idem_key) BETWEEN 8 AND 128),
  endpoint        TEXT        NOT NULL, -- method and route pattern, for example "POST /api/v1/orders/:id/send-to-kitchen"
  user_id         BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  request_hash    BYTEA       NOT NULL CHECK (octet_length(request_hash) = 32), -- sha256 of method, path, query and body
  status          TEXT        NOT NULL DEFAULT 'processing' CHECK (status IN ('processing', 'completed')),
  response_status INTEGER     CHECK (response_status BETWEEN 200 AND 299),
  response_body   JSONB,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at      TIMESTAMPTZ NOT NULL,
  CONSTRAINT idempotency_records_key UNIQUE (idem_key, endpoint),
  CONSTRAINT idempotency_completed_has_response CHECK ((status = 'completed') = (response_status IS NOT NULL AND response_body IS NOT NULL)),
  -- S-06: a key is never forgotten within 24 hours, whatever the setting says.
  CONSTRAINT idempotency_min_retention CHECK (expires_at >= created_at + interval '24 hours')
);
CREATE INDEX idempotency_records_expires_idx ON idempotency_records (expires_at);

-- A completed record is the proof that the work was done once: it is only ever removed, after it expires.
CREATE FUNCTION guard_idempotency_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'completed' THEN
    RAISE EXCEPTION 'a completed idempotency record cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.idem_key <> OLD.idem_key OR NEW.endpoint <> OLD.endpoint OR NEW.user_id <> OLD.user_id OR NEW.request_hash <> OLD.request_hash
     OR NEW.created_at <> OLD.created_at OR NEW.expires_at <> OLD.expires_at THEN
    RAISE EXCEPTION 'only the status and the response of an idempotency record can be set' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER idempotency_records_guard BEFORE UPDATE ON idempotency_records FOR EACH ROW EXECUTE FUNCTION guard_idempotency_record();
CREATE TRIGGER idempotency_records_no_truncate BEFORE TRUNCATE ON idempotency_records FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
