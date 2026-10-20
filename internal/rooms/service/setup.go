package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/rooms/repository"
)

// Room types, rooms and guests (ASSUMPTIONS.md S-93 to S-96).

type RoomTypeView struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	BaseRateCents int64  `json:"base_rate_cents"`
	Capacity      int    `json:"capacity"`
	TaxRateID     int64  `json:"tax_rate_id"`
	TaxName       string `json:"tax_name"`
	TaxRateBP     int    `json:"tax_rate_bp"`
	Active        bool   `json:"active"`
	RoomCount     int    `json:"room_count"`
}

type RoomTypePatch struct {
	Name          *string `json:"name"`
	BaseRateCents *int64  `json:"base_rate_cents"`
	Capacity      *int    `json:"capacity"`
	TaxRateID     *int64  `json:"tax_rate_id"`
	Active        *bool   `json:"active"`
}

func typeView(r repository.ListRoomTypesRow) RoomTypeView {
	return RoomTypeView{ID: r.ID, Name: r.Name, BaseRateCents: r.BaseRateCents, Capacity: int(r.Capacity), TaxRateID: r.TaxRateID,
		TaxName: r.TaxName, TaxRateBP: int(r.TaxRateBp), Active: r.Active, RoomCount: int(r.RoomCount)}
}

func (s *Service) ListRoomTypes(ctx context.Context, actor *auth.Principal, onlyActive bool) ([]RoomTypeView, error) {
	if !can(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListRoomTypes(ctx, repository.ListRoomTypesParams{OnlyActive: onlyActive})
	if err != nil {
		return nil, err
	}
	out := make([]RoomTypeView, 0, len(rows))
	for _, r := range rows {
		out = append(out, typeView(r))
	}
	return out, nil
}

func checkTypeFields(name string, rate int64, capacity int) (string, error) {
	n, err := required(name, "name", 60)
	if err != nil {
		return "", err
	}
	if rate < 0 || rate > maxRate {
		return "", &ValidationError{Field: "base_rate_cents", Msg: "must be from 0 to 10,000,000 cents"}
	}
	if capacity < 1 || capacity > 12 {
		return "", &ValidationError{Field: "capacity", Msg: "must be from 1 to 12 people"}
	}
	return n, nil
}

func checkTax(ctx context.Context, q *repository.Queries, id int64) error {
	t, err := q.GetTaxRateActive(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !t.Active) {
		return &ValidationError{Field: "tax_rate_id", Msg: "must be an active tax category"}
	}
	return err
}

func (s *Service) CreateRoomType(ctx context.Context, actor *auth.Principal, name string, rate int64, capacity int, taxID int64, m Meta) (RoomTypeView, error) {
	if !can(actor) {
		return RoomTypeView{}, ErrForbidden
	}
	n, err := checkTypeFields(name, rate, capacity)
	if err != nil {
		return RoomTypeView{}, err
	}
	var out RoomTypeView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if err := checkTax(ctx, q, taxID); err != nil {
			return err
		}
		id, err := q.InsertRoomType(ctx, repository.InsertRoomTypeParams{Name: n, BaseRateCents: rate, Capacity: int32(capacity), TaxRateID: taxID})
		if isUnique(err, "room_types_name_key") {
			return &ConflictError{Code: "duplicate_name", Field: "name", Msg: "A room type with that name already exists."}
		}
		if err != nil {
			return err
		}
		rows, err := q.ListRoomTypes(ctx, repository.ListRoomTypesParams{OnlyID: id})
		if err != nil {
			return err
		}
		out = typeView(rows[0])
		return s.record(ctx, tx, actor, m, "rooms.type_created", "room_type", id, nil, map[string]any{"name": n, "base_rate_cents": rate, "capacity": capacity, "tax_rate_id": taxID})
	})
	return out, err
}

func (s *Service) UpdateRoomType(ctx context.Context, actor *auth.Principal, id int64, in RoomTypePatch, m Meta) (RoomTypeView, error) {
	if !can(actor) {
		return RoomTypeView{}, ErrForbidden
	}
	var out RoomTypeView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		rows, err := q.ListRoomTypes(ctx, repository.ListRoomTypesParams{OnlyID: id})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return &NotFoundError{What: "room type"}
		}
		cur := typeView(rows[0])
		next := cur
		if in.Name != nil {
			next.Name = *in.Name
		}
		if in.BaseRateCents != nil {
			next.BaseRateCents = *in.BaseRateCents
		}
		if in.Capacity != nil {
			next.Capacity = *in.Capacity
		}
		if in.TaxRateID != nil {
			next.TaxRateID = *in.TaxRateID
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		n, err := checkTypeFields(next.Name, next.BaseRateCents, next.Capacity)
		if err != nil {
			return err
		}
		if next.TaxRateID != cur.TaxRateID {
			if err := checkTax(ctx, q, next.TaxRateID); err != nil {
				return err
			}
		}
		if cur.Active && !next.Active {
			c, err := q.CountActiveRoomsOfType(ctx, id)
			if err != nil {
				return err
			}
			if c > 0 {
				return &ConflictError{Code: "room_type_in_use", Field: "active", Msg: "Move or switch off its rooms first."}
			}
		}
		err = q.UpdateRoomType(ctx, repository.UpdateRoomTypeParams{ID: id, Name: n, BaseRateCents: next.BaseRateCents, Capacity: int32(next.Capacity), TaxRateID: next.TaxRateID, Active: next.Active})
		if isUnique(err, "room_types_name_key") {
			return &ConflictError{Code: "duplicate_name", Field: "name", Msg: "A room type with that name already exists."}
		}
		if err != nil {
			return err
		}
		rows, err = q.ListRoomTypes(ctx, repository.ListRoomTypesParams{OnlyID: id})
		if err != nil {
			return err
		}
		out = typeView(rows[0])
		return s.record(ctx, tx, actor, m, "rooms.type_updated", "room_type", id,
			map[string]any{"name": cur.Name, "base_rate_cents": cur.BaseRateCents, "capacity": cur.Capacity, "tax_rate_id": cur.TaxRateID, "active": cur.Active},
			map[string]any{"name": n, "base_rate_cents": next.BaseRateCents, "capacity": next.Capacity, "tax_rate_id": next.TaxRateID, "active": next.Active})
	})
	return out, err
}

// Rooms

type RoomView struct {
	ID             int64  `json:"id"`
	Number         string `json:"number"`
	RoomTypeID     int64  `json:"room_type_id"`
	RoomType       string `json:"room_type"`
	BaseRateCents  int64  `json:"base_rate_cents"`
	Capacity       int    `json:"capacity"`
	Floor          *int   `json:"floor"`
	PhysicalStatus string `json:"physical_status"`
	Active         bool   `json:"active"`
}

type RoomPatch struct {
	Number     *string `json:"number"`
	RoomTypeID *int64  `json:"room_type_id"`
	Floor      *int    `json:"floor"`
	ClearFloor bool    `json:"-"`
	Active     *bool   `json:"active"`
}

func roomView(r repository.ListRoomsRow) RoomView {
	return RoomView{ID: r.ID, Number: r.Number, RoomTypeID: r.RoomTypeID, RoomType: r.RoomType, BaseRateCents: r.BaseRateCents, Capacity: int(r.Capacity),
		Floor: int4Ptr(r.Floor), PhysicalStatus: r.PhysicalStatus, Active: r.Active}
}

func (s *Service) ListRooms(ctx context.Context, actor *auth.Principal, onlyActive bool) ([]RoomView, error) {
	if !can(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListRooms(ctx, repository.ListRoomsParams{OnlyActive: onlyActive})
	if err != nil {
		return nil, err
	}
	out := make([]RoomView, 0, len(rows))
	for _, r := range rows {
		out = append(out, roomView(r))
	}
	return out, nil
}

func floorVal(f *int) (pgtype.Int4, error) {
	if f == nil {
		return pgtype.Int4{}, nil
	}
	if *f < -3 || *f > 60 {
		return pgtype.Int4{}, &ValidationError{Field: "floor", Msg: "must be from -3 to 60"}
	}
	return pgtype.Int4{Int32: int32(*f), Valid: true}, nil
}

func checkRoomType(ctx context.Context, q *repository.Queries, id int64) error {
	rows, err := q.ListRoomTypes(ctx, repository.ListRoomTypesParams{OnlyID: id})
	if err != nil {
		return err
	}
	if len(rows) == 0 || !rows[0].Active {
		return &ValidationError{Field: "room_type_id", Msg: "must be an active room type"}
	}
	return nil
}

func (s *Service) CreateRoom(ctx context.Context, actor *auth.Principal, number string, typeID int64, floor *int, m Meta) (RoomView, error) {
	if !can(actor) {
		return RoomView{}, ErrForbidden
	}
	n, err := required(number, "number", 10)
	if err != nil {
		return RoomView{}, err
	}
	fl, err := floorVal(floor)
	if err != nil {
		return RoomView{}, err
	}
	var out RoomView
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		if err := checkRoomType(ctx, q, typeID); err != nil {
			return err
		}
		id, err := q.InsertRoom(ctx, repository.InsertRoomParams{Number: n, RoomTypeID: typeID, Floor: fl})
		if isUnique(err, "rooms_number_key") {
			return &ConflictError{Code: "duplicate_number", Field: "number", Msg: "A room with that number already exists."}
		}
		if err != nil {
			return err
		}
		rows, err := q.ListRooms(ctx, repository.ListRoomsParams{OnlyID: id})
		if err != nil {
			return err
		}
		out = roomView(rows[0])
		return s.record(ctx, tx, actor, m, "rooms.room_created", "room", id, nil, map[string]any{"number": n, "room_type_id": typeID})
	})
	return out, err
}

func (s *Service) UpdateRoom(ctx context.Context, actor *auth.Principal, id int64, in RoomPatch, m Meta) (RoomView, error) {
	if !can(actor) {
		return RoomView{}, ErrForbidden
	}
	var out RoomView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		rows, err := q.ListRooms(ctx, repository.ListRoomsParams{OnlyID: id})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return &NotFoundError{What: "room"}
		}
		cur := roomView(rows[0])
		next := cur
		if in.Number != nil {
			next.Number = *in.Number
		}
		if in.RoomTypeID != nil {
			next.RoomTypeID = *in.RoomTypeID
		}
		if in.Floor != nil {
			next.Floor = in.Floor
		}
		if in.ClearFloor {
			next.Floor = nil
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		n, err := required(next.Number, "number", 10)
		if err != nil {
			return err
		}
		fl, err := floorVal(next.Floor)
		if err != nil {
			return err
		}
		if next.RoomTypeID != cur.RoomTypeID {
			if err := checkRoomType(ctx, q, next.RoomTypeID); err != nil {
				return err
			}
		}
		if cur.Active && !next.Active {
			today, err := s.today(ctx, q)
			if err != nil {
				return err
			}
			c, err := q.CountUpcomingBlocks(ctx, repository.CountUpcomingBlocksParams{RoomID: id, Column2: pgtype.Date{Time: today, Valid: true}})
			if err != nil {
				return err
			}
			if c > 0 {
				return &ConflictError{Code: "room_in_use", Field: "active", Msg: "This room has bookings or a guest in it. Move or cancel them first."}
			}
		}
		err = q.UpdateRoom(ctx, repository.UpdateRoomParams{ID: id, Number: n, RoomTypeID: next.RoomTypeID, Floor: fl, Active: next.Active})
		if isUnique(err, "rooms_number_key") {
			return &ConflictError{Code: "duplicate_number", Field: "number", Msg: "A room with that number already exists."}
		}
		if err != nil {
			return err
		}
		rows, err = q.ListRooms(ctx, repository.ListRoomsParams{OnlyID: id})
		if err != nil {
			return err
		}
		out = roomView(rows[0])
		return s.record(ctx, tx, actor, m, "rooms.room_updated", "room", id,
			map[string]any{"number": cur.Number, "room_type_id": cur.RoomTypeID, "active": cur.Active},
			map[string]any{"number": n, "room_type_id": next.RoomTypeID, "active": next.Active})
	})
	return out, err
}

// Guests

type GuestView struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
	Notes    string `json:"notes"`
}

type GuestInput struct {
	FullName string `json:"full_name"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
	Notes    string `json:"notes"`
}

type GuestPatch struct {
	FullName *string `json:"full_name"`
	Phone    *string `json:"phone"`
	Email    *string `json:"email"`
	Notes    *string `json:"notes"`
}

func guestView(r repository.ListGuestsRow) GuestView {
	return GuestView{ID: r.ID, FullName: r.FullName, Phone: r.Phone, Email: r.Email, Notes: r.Notes}
}

type cleanGuest struct {
	name                string
	phone, email, notes pgtype.Text
}

func cleanGuestInput(in GuestInput) (cleanGuest, error) {
	n, err := required(in.FullName, "full_name", 120)
	if err != nil {
		return cleanGuest{}, err
	}
	phone, err := optText(in.Phone, 30, "phone")
	if err != nil {
		return cleanGuest{}, err
	}
	if phone.Valid && runeLen(phone.String) < 5 {
		return cleanGuest{}, &ValidationError{Field: "phone", Msg: "is too short"}
	}
	email, err := optText(in.Email, 160, "email")
	if err != nil {
		return cleanGuest{}, err
	}
	if email.Valid && !emailRe.MatchString(email.String) {
		return cleanGuest{}, &ValidationError{Field: "email", Msg: "is not a valid email address"}
	}
	notes, err := optText(in.Notes, 500, "notes")
	if err != nil {
		return cleanGuest{}, err
	}
	return cleanGuest{name: n, phone: phone, email: email, notes: notes}, nil
}

func (s *Service) ListGuests(ctx context.Context, actor *auth.Principal, search string, beforeID int64, limit int) ([]GuestView, error) {
	if !can(actor) {
		return nil, ErrForbidden
	}
	rows, err := repository.New(s.pool).ListGuests(ctx, repository.ListGuestsParams{Q: search, BeforeID: beforeID, RowLimit: clampLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]GuestView, 0, len(rows))
	for _, r := range rows {
		out = append(out, guestView(r))
	}
	return out, nil
}

func (s *Service) insertGuest(ctx context.Context, tx pgx.Tx, actor *auth.Principal, in GuestInput, m Meta) (int64, error) {
	g, err := cleanGuestInput(in)
	if err != nil {
		return 0, err
	}
	q := repository.New(tx)
	id, err := q.InsertGuest(ctx, repository.InsertGuestParams{FullName: g.name, Phone: g.phone, Email: g.email, Notes: g.notes})
	if err != nil {
		return 0, err
	}
	return id, s.record(ctx, tx, actor, m, "rooms.guest_created", "guest", id, nil, map[string]any{"full_name": g.name})
}

func (s *Service) CreateGuest(ctx context.Context, actor *auth.Principal, in GuestInput, m Meta) (GuestView, error) {
	if !can(actor) {
		return GuestView{}, ErrForbidden
	}
	var out GuestView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		id, err := s.insertGuest(ctx, tx, actor, in, m)
		if err != nil {
			return err
		}
		rows, err := repository.New(tx).ListGuests(ctx, repository.ListGuestsParams{OnlyID: id, RowLimit: 1})
		if err != nil {
			return err
		}
		out = guestView(rows[0])
		return nil
	})
	return out, err
}

func (s *Service) UpdateGuest(ctx context.Context, actor *auth.Principal, id int64, in GuestPatch, m Meta) (GuestView, error) {
	if !can(actor) {
		return GuestView{}, ErrForbidden
	}
	var out GuestView
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := repository.New(tx)
		rows, err := q.ListGuests(ctx, repository.ListGuestsParams{OnlyID: id, RowLimit: 1})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return &NotFoundError{What: "guest"}
		}
		cur := guestView(rows[0])
		next := GuestInput{FullName: cur.FullName, Phone: cur.Phone, Email: cur.Email, Notes: cur.Notes}
		if in.FullName != nil {
			next.FullName = *in.FullName
		}
		if in.Phone != nil {
			next.Phone = *in.Phone
		}
		if in.Email != nil {
			next.Email = *in.Email
		}
		if in.Notes != nil {
			next.Notes = *in.Notes
		}
		g, err := cleanGuestInput(next)
		if err != nil {
			return err
		}
		if err := q.UpdateGuest(ctx, repository.UpdateGuestParams{ID: id, FullName: g.name, Phone: g.phone, Email: g.email, Notes: g.notes}); err != nil {
			return err
		}
		out = GuestView{ID: id, FullName: g.name, Phone: g.phone.String, Email: g.email.String, Notes: g.notes.String}
		return s.record(ctx, tx, actor, m, "rooms.guest_updated", "guest", id,
			map[string]any{"full_name": cur.FullName}, map[string]any{"full_name": g.name})
	})
	return out, err
}
