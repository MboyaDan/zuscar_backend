-- Stage 14: print jobs and print attempts (docs/behaviour.md section 8, AC-03, ADR-09).
-- Printing is separate from the record it prints: a job is created after the sale, ticket or bill exists, holds a frozen
-- copy of what was rendered, and only the job's own status ever moves. Nothing here can change a sale or a ticket.

CREATE TABLE print_jobs (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  doc_type      TEXT        NOT NULL CHECK (doc_type IN ('receipt', 'kitchen_ticket', 'bill', 'test_page')),
  sale_id       BIGINT      REFERENCES sales (id) ON DELETE RESTRICT,
  ticket_id     BIGINT      REFERENCES kitchen_tickets (id) ON DELETE RESTRICT,
  order_id      BIGINT      REFERENCES orders (id) ON DELETE RESTRICT,
  status        TEXT        NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'printed', 'failed', 'reprinted')),
  reprint_of    BIGINT      REFERENCES print_jobs (id) ON DELETE RESTRICT,
  copy_no       INTEGER     NOT NULL DEFAULT 0 CHECK (copy_no >= 0),
  document      JSONB       NOT NULL,
  created_by    BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- A job points at exactly the one record its type prints.
  CONSTRAINT print_jobs_target CHECK (
    (doc_type = 'receipt'        AND sale_id IS NOT NULL AND ticket_id IS NULL     AND order_id IS NULL) OR
    (doc_type = 'kitchen_ticket' AND sale_id IS NULL     AND ticket_id IS NOT NULL AND order_id IS NULL) OR
    (doc_type = 'bill'           AND sale_id IS NULL     AND ticket_id IS NULL     AND order_id IS NOT NULL) OR
    (doc_type = 'test_page'      AND sale_id IS NULL     AND ticket_id IS NULL     AND order_id IS NULL)),
  -- A reprint is a new job linked to the first one and numbered; an original has copy_no 0.
  CONSTRAINT print_jobs_reprint CHECK ((reprint_of IS NULL) = (copy_no = 0)),
  -- "printed" belongs to an original, "reprinted" to a copy.
  CONSTRAINT print_jobs_status_kind CHECK (NOT (status = 'printed' AND reprint_of IS NOT NULL) AND NOT (status = 'reprinted' AND reprint_of IS NULL))
);
-- One original job per receipt and per kitchen ticket: a double tap or a retry cannot print the first copy twice.
-- More paper needs a reprint, which is marked and audited. Bills may be printed again as the order changes.
CREATE UNIQUE INDEX print_jobs_one_receipt_key ON print_jobs (sale_id) WHERE doc_type = 'receipt' AND reprint_of IS NULL;
CREATE UNIQUE INDEX print_jobs_one_ticket_key  ON print_jobs (ticket_id) WHERE doc_type = 'kitchen_ticket' AND reprint_of IS NULL;
CREATE INDEX print_jobs_created_idx ON print_jobs (created_by, id DESC);
CREATE INDEX print_jobs_status_idx ON print_jobs (status, id DESC);

-- Only the status (and updated_at) of a job may change, and only along the allowed moves.
CREATE FUNCTION guard_print_job_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.doc_type <> OLD.doc_type OR NEW.sale_id IS DISTINCT FROM OLD.sale_id OR NEW.ticket_id IS DISTINCT FROM OLD.ticket_id
     OR NEW.order_id IS DISTINCT FROM OLD.order_id OR NEW.reprint_of IS DISTINCT FROM OLD.reprint_of OR NEW.copy_no <> OLD.copy_no
     OR NEW.document <> OLD.document OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a print job keeps what it prints; only its status moves' USING ERRCODE = '23514';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'queued' AND NEW.status IN ('printed', 'reprinted', 'failed')) OR
       (OLD.status = 'failed' AND NEW.status = 'queued')) THEN
    RAISE EXCEPTION 'a print job cannot move from % to %', OLD.status, NEW.status USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER print_jobs_guard BEFORE UPDATE ON print_jobs FOR EACH ROW EXECUTE FUNCTION guard_print_job_update();
CREATE TRIGGER print_jobs_no_delete BEFORE DELETE ON print_jobs FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER print_jobs_no_truncate BEFORE TRUNCATE ON print_jobs FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- Each time a person reports what happened at the printer. "printed" is their word: a browser cannot prove paper came out (DG-03).
CREATE TABLE print_attempts (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  job_id     BIGINT      NOT NULL REFERENCES print_jobs (id) ON DELETE RESTRICT,
  outcome    TEXT        NOT NULL CHECK (outcome IN ('printed', 'failed')),
  note       TEXT        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
  created_by BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX print_attempts_job_idx ON print_attempts (job_id, id);
CREATE TRIGGER print_attempts_immutable BEFORE UPDATE OR DELETE ON print_attempts FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER print_attempts_no_truncate BEFORE TRUNCATE ON print_attempts FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
