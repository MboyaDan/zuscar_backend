-- Stage 10b-1: kitchen tickets (docs/schema-constraints.md section 3, docs/behaviour.md section 2).
-- A ticket is written in the same transaction as the order change that raises it, before anything is printed
-- (AC-04, NN-05). Ticket status has no link to payment. The idempotency records table arrives in 10b-2.

CREATE TABLE kitchen_tickets (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  order_id         BIGINT      NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
  business_date    DATE        NOT NULL,
  ticket_no        BIGINT      NOT NULL CHECK (ticket_no >= 1), -- restarts each business day (S-05)
  kind             TEXT        NOT NULL CHECK (kind IN ('original', 'update')),
  parent_ticket_id BIGINT,
  status           TEXT        NOT NULL DEFAULT 'submitted' CHECK (status IN ('submitted', 'preparing', 'ready', 'served', 'cancelled')),
  created_by       BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT kitchen_tickets_number_key UNIQUE (business_date, ticket_no),
  CONSTRAINT kitchen_tickets_id_order_key UNIQUE (id, order_id),
  -- the parent must be a ticket of the same order
  CONSTRAINT kitchen_tickets_parent_fkey FOREIGN KEY (parent_ticket_id, order_id) REFERENCES kitchen_tickets (id, order_id) ON DELETE RESTRICT,
  CONSTRAINT kitchen_tickets_parent_iff_update CHECK ((kind = 'update') = (parent_ticket_id IS NOT NULL))
);
CREATE UNIQUE INDEX kitchen_tickets_one_original_idx ON kitchen_tickets (order_id) WHERE kind = 'original';
CREATE INDEX kitchen_tickets_order_idx ON kitchen_tickets (order_id, id);
CREATE INDEX kitchen_tickets_active_idx ON kitchen_tickets (id) WHERE status IN ('submitted', 'preparing', 'ready');

-- Only the status moves, only forward, and a ticket is never deleted. The parent and the lines are never edited:
-- a change goes out as a new update ticket.
CREATE FUNCTION guard_kitchen_ticket() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.order_id <> OLD.order_id OR NEW.business_date <> OLD.business_date OR NEW.ticket_no <> OLD.ticket_no
     OR NEW.kind <> OLD.kind OR NEW.parent_ticket_id IS DISTINCT FROM OLD.parent_ticket_id OR NEW.created_by <> OLD.created_by
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'kitchen ticket % keeps its order, number, kind and parent', OLD.id USING ERRCODE = '23514';
  END IF;
  IF NEW.status = OLD.status THEN
    RAISE EXCEPTION 'kitchen ticket % is already %', OLD.id, OLD.status USING ERRCODE = '23514';
  END IF;
  IF NOT (
       (OLD.status = 'submitted' AND NEW.status IN ('preparing', 'ready', 'served', 'cancelled'))
    OR (OLD.status = 'preparing' AND NEW.status IN ('ready', 'served', 'cancelled'))
    OR (OLD.status = 'ready'     AND NEW.status IN ('served', 'cancelled'))
  ) THEN
    RAISE EXCEPTION 'kitchen ticket % cannot go from % to %', OLD.id, OLD.status, NEW.status USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER kitchen_tickets_guard BEFORE UPDATE ON kitchen_tickets FOR EACH ROW EXECUTE FUNCTION guard_kitchen_ticket();
CREATE TRIGGER kitchen_tickets_no_delete BEFORE DELETE ON kitchen_tickets FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER kitchen_tickets_no_truncate BEFORE TRUNCATE ON kitchen_tickets FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- What the kitchen is told. No prices. name_snapshot and modifiers_text are copied when the ticket is raised.
CREATE TABLE kitchen_ticket_items (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ticket_id      BIGINT NOT NULL REFERENCES kitchen_tickets (id) ON DELETE RESTRICT,
  order_item_id  BIGINT NOT NULL REFERENCES order_items (id) ON DELETE RESTRICT,
  name_snapshot  TEXT   NOT NULL,
  qty            INTEGER NOT NULL CHECK (qty BETWEEN 1 AND 999),
  change_type    TEXT   NOT NULL CHECK (change_type IN ('add', 'cancel', 'modify')),
  modifiers_text TEXT   NOT NULL DEFAULT '',
  note           TEXT   NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
  CONSTRAINT kitchen_ticket_items_line_key UNIQUE (ticket_id, order_item_id)
);
-- A line goes to the kitchen once, and is cancelled there once: a second send or a second cancel cannot slip in.
CREATE UNIQUE INDEX kitchen_ticket_items_one_add_idx ON kitchen_ticket_items (order_item_id) WHERE change_type = 'add';
CREATE UNIQUE INDEX kitchen_ticket_items_one_cancel_idx ON kitchen_ticket_items (order_item_id) WHERE change_type = 'cancel';
CREATE TRIGGER kitchen_ticket_items_immutable BEFORE UPDATE OR DELETE ON kitchen_ticket_items FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER kitchen_ticket_items_no_truncate BEFORE TRUNCATE ON kitchen_ticket_items FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TABLE ticket_status_history (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ticket_id   BIGINT      NOT NULL REFERENCES kitchen_tickets (id) ON DELETE RESTRICT,
  from_status TEXT        CHECK (from_status IN ('submitted', 'preparing', 'ready', 'served', 'cancelled')), -- null for the first row
  to_status   TEXT        NOT NULL CHECK (to_status IN ('submitted', 'preparing', 'ready', 'served', 'cancelled')),
  changed_by  BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  changed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ticket_status_history_ticket_idx ON ticket_status_history (ticket_id, id);
CREATE TRIGGER ticket_status_history_immutable BEFORE UPDATE OR DELETE ON ticket_status_history FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER ticket_status_history_no_truncate BEFORE TRUNCATE ON ticket_status_history FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
