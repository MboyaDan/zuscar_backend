// Package audit is the contract other modules use to write audit rows. The identity module owns the audit_logs
// table (docs/architecture.md section 3) and implements Recorder; callers pass their own open transaction so the
// audit row commits or rolls back together with the change it describes (NN-08).
package audit

import (
	"context"
	"net/netip"

	"github.com/jackc/pgx/v5"
)

// Entry describes one event. Before and After must never hold secrets.
type Entry struct {
	ActorID                      int64 // 0 for none
	ActorLabel                   string
	Action, EntityType, EntityID string
	Reason                       string
	Before, After                map[string]any
	RequestID                    string
	IP                           *netip.Addr
}

type Recorder interface {
	Record(ctx context.Context, tx pgx.Tx, e Entry) error
}
