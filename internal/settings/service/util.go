package service

import (
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
