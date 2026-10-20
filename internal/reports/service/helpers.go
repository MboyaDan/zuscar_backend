package service

import (
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func dt(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }

func sortRowsByFirst(rows [][]any) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i][0].(string) < rows[j][0].(string) })
}
