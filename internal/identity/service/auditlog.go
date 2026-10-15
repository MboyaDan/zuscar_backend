package service

// Audit log reader (Stage 7b, GET /audit-logs). Scope (docs/rbac.md note 6): operational events are visible to
// anyone holding audit.view; security events (users, roles, permissions, sessions, settings, tax categories) only
// to a system administrator or an Accountant / Auditor (S-40).

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/identity/repository"
	"zuscar/backend/internal/platform/auth"
)

var securityEntityTypes = []string{"user", "session", "role", "permission", "setting", "tax_rate"}

func canSeeSecurityEvents(actor *auth.Principal) bool {
	if actor.Has("system.admin") {
		return true
	}
	for _, r := range actor.Roles {
		if r == "accountant_auditor" {
			return true
		}
	}
	return false
}

type AuditFilter struct {
	Action, EntityType, Result string
	ActorID, BeforeID          int64
	From, To                   *time.Time
	Limit                      int
}

type AuditView struct {
	ID         int64           `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	ActorID    *int64          `json:"actor_user_id"`
	ActorLabel string          `json:"actor_label"`
	Action     string          `json:"action"`
	EntityType string          `json:"entity_type"`
	EntityID   string          `json:"entity_id"`
	Reason     string          `json:"reason"`
	Result     string          `json:"result"`
	Before     json.RawMessage `json:"before"`
	After      json.RawMessage `json:"after"`
	RequestID  string          `json:"request_id"`
	IP         string          `json:"ip"`
}

func tsParam(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// ListAuditLogs returns newest first. The next page starts after the returned cursor (the id of the last row, 0 when done).
func (s *Service) ListAuditLogs(ctx context.Context, actor *auth.Principal, f AuditFilter) ([]AuditView, int64, error) {
	if err := require(actor, "audit.view"); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	rows, err := repository.New(s.pool).ListAuditLogs(ctx, repository.ListAuditLogsParams{
		BeforeID: f.BeforeID, SecurityVisible: canSeeSecurityEvents(actor), SecurityTypes: securityEntityTypes,
		ActionFilter: strings.TrimSpace(f.Action), EntityFilter: strings.TrimSpace(f.EntityType), ResultFilter: strings.TrimSpace(f.Result),
		ActorFilter: f.ActorID, FromTs: tsParam(f.From), ToTs: tsParam(f.To), RowLimit: int32(f.Limit + 1),
	})
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		next = rows[len(rows)-1].ID
	}
	out := make([]AuditView, 0, len(rows))
	for _, r := range rows {
		v := AuditView{ID: r.ID, OccurredAt: r.OccurredAt.Time, ActorLabel: r.ActorLabel, Action: r.Action, EntityType: r.EntityType,
			EntityID: r.EntityID.String, Reason: r.Reason.String, Result: r.Result, Before: r.BeforeJson, After: r.AfterJson, RequestID: r.RequestID.String}
		if r.ActorUserID.Valid {
			id := r.ActorUserID.Int64
			v.ActorID = &id
		}
		if r.Ip != nil {
			v.IP = r.Ip.String()
		}
		out = append(out, v)
	}
	return out, next, nil
}
