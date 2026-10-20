-- Stage 24: housekeeping tasks. A task is a job on one room: cleaning it after a guest, or a repair. Room status stays the
-- single truth about whether a room can be sold; a cleaning task follows it (dirty -> cleaning -> ready) and is opened for
-- every room that becomes dirty.

CREATE TABLE housekeeping_tasks (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  room_id     BIGINT      NOT NULL REFERENCES rooms (id) ON DELETE RESTRICT,
  kind        TEXT        NOT NULL CHECK (kind IN ('clean', 'maintenance')),
  status      TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'in_progress', 'done', 'cancelled')),
  note        TEXT        NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
  assigned_to BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  created_by  BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at  TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  finished_by BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
  CONSTRAINT housekeeping_started_shape CHECK (status NOT IN ('in_progress', 'done') OR started_at IS NOT NULL),
  CONSTRAINT housekeeping_finished_shape CHECK ((status IN ('done', 'cancelled')) = (finished_at IS NOT NULL AND finished_by IS NOT NULL))
);
CREATE INDEX housekeeping_tasks_room_idx ON housekeeping_tasks (room_id, id DESC);
CREATE INDEX housekeeping_tasks_open_idx ON housekeeping_tasks (status) WHERE status IN ('open', 'in_progress');
-- A room has at most one cleaning job going at a time.
CREATE UNIQUE INDEX housekeeping_one_active_clean ON housekeeping_tasks (room_id) WHERE kind = 'clean' AND status IN ('open', 'in_progress');

CREATE FUNCTION guard_housekeeping_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ROW(NEW.id, NEW.room_id, NEW.kind, NEW.created_by, NEW.created_at) IS DISTINCT FROM ROW(OLD.id, OLD.room_id, OLD.kind, OLD.created_by, OLD.created_at) THEN
    RAISE EXCEPTION 'a housekeeping task keeps its room, kind and creator' USING ERRCODE = '23514';
  END IF;
  IF OLD.status IN ('done', 'cancelled') THEN
    RAISE EXCEPTION 'a finished housekeeping task cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'open' AND NEW.status IN ('in_progress', 'done', 'cancelled'))
    OR (OLD.status = 'in_progress' AND NEW.status IN ('done', 'cancelled'))) THEN
    RAISE EXCEPTION 'a housekeeping task cannot go from % to %', OLD.status, NEW.status USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER housekeeping_guard BEFORE UPDATE ON housekeeping_tasks FOR EACH ROW EXECUTE FUNCTION guard_housekeeping_update();
CREATE TRIGGER housekeeping_no_delete BEFORE DELETE ON housekeeping_tasks FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER housekeeping_no_truncate BEFORE TRUNCATE ON housekeeping_tasks FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();
