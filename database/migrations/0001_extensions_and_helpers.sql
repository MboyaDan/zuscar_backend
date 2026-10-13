-- Extensions and shared helpers. Applied by `make migrate`, never at app start (AC-13).
-- btree_gist is required by the room-overlap exclusion constraint (ADR-06, Stage 20).
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Used by immutable tables: audit_logs now; ledger, sale lines, folio items later (NN-08).
CREATE FUNCTION reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on % is not allowed: records are append-only (use a compensating record)', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'integrity_constraint_violation';
END $$;
