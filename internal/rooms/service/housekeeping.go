package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/rooms/repository"
)

// Housekeeping tasks (Stage 24, S-104). Room status stays the one truth about whether a room can be sold; a cleaning task
// follows it: a room that turns dirty gets a task, starting to clean moves dirty to cleaning, finishing moves cleaning to
// ready. Maintenance tasks are notes and do not change the room.

type TaskView struct {
	ID           int64      `json:"id"`
	RoomID       int64      `json:"room_id"`
	RoomNumber   string     `json:"room_number"`
	RoomStatus   string     `json:"room_status"`
	Kind         string     `json:"kind"`
	Status       string     `json:"status"`
	Note         string     `json:"note"`
	AssignedTo   *int64     `json:"assigned_to"`
	AssignedName string     `json:"assigned_name"`
	CreatedBy    int64      `json:"created_by"`
	CreatedName  string     `json:"created_name"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

func taskView(r repository.ListHousekeepingTasksRow) TaskView {
	return TaskView{ID: r.ID, RoomID: r.RoomID, RoomNumber: r.RoomNumber, RoomStatus: r.RoomStatus, Kind: r.Kind, Status: r.Status, Note: r.Note, AssignedTo: idPtr(r.AssignedTo),
		AssignedName: r.AssignedName, CreatedBy: r.CreatedBy, CreatedName: r.CreatedName, CreatedAt: r.CreatedAt.Time, StartedAt: tsPtr(r.StartedAt), FinishedAt: tsPtr(r.FinishedAt)}
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// syncTasks keeps the cleaning task in step with the room. The caller holds the room lock and has just moved the room.
func (s *Service) syncTasks(ctx context.Context, q *repository.Queries, actor *auth.Principal, roomID int64, to string) error {
	task, err := q.ActiveCleanTask(ctx, roomID)
	has := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	switch to {
	case "dirty":
		if !has {
			_, err = q.InsertHousekeepingTask(ctx, repository.InsertHousekeepingTaskParams{RoomID: roomID, Kind: "clean", Note: "Needs cleaning", CreatedBy: actor.UserID})
			return err
		}
	case "cleaning":
		if has && task.Status == "open" {
			return q.StartHousekeepingTask(ctx, repository.StartHousekeepingTaskParams{ID: task.ID, AssignedTo: pgtype.Int8{Int64: actor.UserID, Valid: true}})
		}
	case "vacant_ready":
		if has {
			return q.DoneHousekeepingTask(ctx, repository.DoneHousekeepingTaskParams{ID: task.ID, FinishedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}})
		}
	case "out_of_service", "occupied":
		if has {
			return q.CancelHousekeepingTask(ctx, repository.CancelHousekeepingTaskParams{ID: task.ID, FinishedBy: pgtype.Int8{Int64: actor.UserID, Valid: true}})
		}
	}
	return nil
}

func (s *Service) ListTasks(ctx context.Context, actor *auth.Principal, status string, roomID int64) ([]TaskView, error) {
	if !can(actor) {
		return nil, ErrForbidden
	}
	switch status {
	case "", "active", "open", "in_progress", "done", "cancelled":
	default:
		return nil, &ValidationError{Field: "status", Msg: "is not a recognised value"}
	}
	rows, err := repository.New(s.pool).ListHousekeepingTasks(ctx, repository.ListHousekeepingTasksParams{Status: status, RoomID: roomID, RowLimit: 200})
	if err != nil {
		return nil, err
	}
	out := make([]TaskView, 0, len(rows))
	for _, r := range rows {
		out = append(out, taskView(r))
	}
	return out, nil
}

type TaskInput struct {
	RoomID     int64  `json:"room_id"`
	Kind       string `json:"kind"`
	Note       string `json:"note"`
	AssignedTo int64  `json:"assigned_to"`
}

func (s *Service) assignee(ctx context.Context, q *repository.Queries, id int64) (pgtype.Int8, error) {
	if id == 0 {
		return pgtype.Int8{}, nil
	}
	ok, err := q.UserExistsActive(ctx, id)
	if err != nil {
		return pgtype.Int8{}, err
	}
	if !ok {
		return pgtype.Int8{}, &ValidationError{Field: "assigned_to", Msg: "is not an active user"}
	}
	return pgtype.Int8{Int64: id, Valid: true}, nil
}

func (s *Service) CreateTask(ctx context.Context, actor *auth.Principal, in TaskInput, m Meta) (TaskView, error) {
	if !can(actor) {
		return TaskView{}, ErrForbidden
	}
	if in.Kind != "clean" && in.Kind != "maintenance" {
		return TaskView{}, &ValidationError{Field: "kind", Msg: "must be clean or maintenance"}
	}
	note, err := optText(in.Note, 200, "note")
	if err != nil {
		return TaskView{}, err
	}
	if in.Kind == "maintenance" && !note.Valid {
		return TaskView{}, &ValidationError{Field: "note", Msg: "say what needs doing"}
	}
	var out TaskView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		room, err := q.LockRoom(ctx, in.RoomID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &ValidationError{Field: "room_id", Msg: "is not a room"}
		}
		if err != nil {
			return err
		}
		who, err := s.assignee(ctx, q, in.AssignedTo)
		if err != nil {
			return err
		}
		if in.Kind == "clean" {
			if _, err := q.ActiveCleanTask(ctx, room.ID); err == nil {
				return &ConflictError{Code: "task_exists", Field: "room_id", Msg: "Room " + room.Number + " already has a cleaning job going."}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		id, err := q.InsertHousekeepingTask(ctx, repository.InsertHousekeepingTaskParams{RoomID: room.ID, Kind: in.Kind, Note: note.String, AssignedTo: who, CreatedBy: actor.UserID})
		if err != nil {
			return err
		}
		if err := s.record(ctx, tx, actor, m, "rooms.task_created", "housekeeping_task", id, nil, map[string]any{"room_id": room.ID, "kind": in.Kind}); err != nil {
			return err
		}
		out, err = s.taskByID(ctx, q, id)
		return err
	})
	return out, err
}

func (s *Service) taskByID(ctx context.Context, q *repository.Queries, id int64) (TaskView, error) {
	rows, err := q.ListHousekeepingTasks(ctx, repository.ListHousekeepingTasksParams{OnlyID: id, RowLimit: 1})
	if err != nil {
		return TaskView{}, err
	}
	if len(rows) == 0 {
		return TaskView{}, &NotFoundError{What: "task"}
	}
	return taskView(rows[0]), nil
}

type TaskUpdate struct {
	Status     *string `json:"status"`
	Note       *string `json:"note"`
	AssignedTo *int64  `json:"assigned_to"` // 0 takes the name off
}

// UpdateTask moves a task forward (start, finish, cancel) or changes its note or who it is for. Finished tasks stay as they were.
func (s *Service) UpdateTask(ctx context.Context, actor *auth.Principal, id int64, in TaskUpdate, m Meta) (TaskView, error) {
	if !can(actor) {
		return TaskView{}, ErrForbidden
	}
	if in.Status != nil {
		switch *in.Status {
		case "in_progress", "done", "cancelled":
		default:
			return TaskView{}, &ValidationError{Field: "status", Msg: "must be in_progress, done or cancelled"}
		}
	}
	var out TaskView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		t, err := q.LockHousekeepingTask(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotFoundError{What: "task"}
		}
		if err != nil {
			return err
		}
		if t.Status == "done" || t.Status == "cancelled" {
			return &ConflictError{Code: "task_finished", Msg: "This job is already " + map[string]string{"done": "done", "cancelled": "cancelled"}[t.Status] + " and cannot change."}
		}
		room, err := q.LockRoom(ctx, t.RoomID)
		if err != nil {
			return err
		}
		if in.Note != nil || in.AssignedTo != nil {
			cur, err := s.taskByID(ctx, q, id)
			if err != nil {
				return err
			}
			note, who := cur.Note, pgtype.Int8{}
			if cur.AssignedTo != nil {
				who = pgtype.Int8{Int64: *cur.AssignedTo, Valid: true}
			}
			if in.Note != nil {
				n, err := optText(*in.Note, 200, "note")
				if err != nil {
					return err
				}
				note = n.String
			}
			if in.AssignedTo != nil {
				if who, err = s.assignee(ctx, q, *in.AssignedTo); err != nil {
					return err
				}
			}
			if err := q.UpdateHousekeepingTask(ctx, repository.UpdateHousekeepingTaskParams{ID: id, Note: note, AssignedTo: who}); err != nil {
				return err
			}
		}
		me := pgtype.Int8{Int64: actor.UserID, Valid: true}
		status := room.PhysicalStatus
		switch {
		case in.Status == nil:
		case *in.Status == "in_progress":
			if t.Status != "open" {
				return &ConflictError{Code: "invalid_transition", Msg: "This job is already started."}
			}
			if t.Kind == "clean" && status == "dirty" {
				if err := s.setStatus(ctx, tx, q, actor, room.ID, status, "cleaning", "Housekeeping started"); err != nil {
					return err
				}
			} else if err := q.StartHousekeepingTask(ctx, repository.StartHousekeepingTaskParams{ID: id, AssignedTo: me}); err != nil {
				return err
			}
		case *in.Status == "done":
			if t.Kind == "clean" && (status == "dirty" || status == "cleaning") {
				if status == "dirty" {
					if err := s.setStatus(ctx, tx, q, actor, room.ID, "dirty", "cleaning", "Housekeeping started"); err != nil {
						return err
					}
				}
				if err := s.setStatus(ctx, tx, q, actor, room.ID, "cleaning", "vacant_ready", "Housekeeping finished"); err != nil {
					return err
				}
			} else if err := q.DoneHousekeepingTask(ctx, repository.DoneHousekeepingTaskParams{ID: id, FinishedBy: me}); err != nil {
				return err
			}
		case *in.Status == "cancelled":
			if err := q.CancelHousekeepingTask(ctx, repository.CancelHousekeepingTaskParams{ID: id, FinishedBy: me}); err != nil {
				return err
			}
		}
		if err := s.record(ctx, tx, actor, m, "rooms.task_updated", "housekeeping_task", id, map[string]any{"status": t.Status}, map[string]any{"status": in.Status}); err != nil {
			return err
		}
		out, err = s.taskByID(ctx, q, id)
		return err
	})
	return out, err
}
