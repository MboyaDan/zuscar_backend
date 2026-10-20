package service

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"zuscar/backend/internal/platform/auth"
	"zuscar/backend/internal/sales/repository"
)

// Payments on a guest folio (Stage 23). The rooms module owns the folio and its balance and calls these inside its own
// transaction, after it has locked the folio, so two payments racing for the last of a balance are decided one by one.
// Cash counts at once and needs the actor's open shift; other methods wait for payments.reconcile (NN-11).

// FolioPayment is what the rooms module asks for. Purpose is deposit or folio_settlement.
type FolioPayment struct {
	FolioID int64
	Purpose string
	PaymentInput
	Note string
}

func (s *Service) RecordFolioPayment(ctx context.Context, tx pgx.Tx, actor *auth.Principal, in FolioPayment) (PaymentRecord, error) {
	if in.Purpose != "deposit" && in.Purpose != "folio_settlement" {
		return PaymentRecord{}, &ValidationError{Field: "purpose", Msg: "must be deposit or settlement"}
	}
	pays, err := checkPayments([]PaymentInput{in.PaymentInput})
	if err != nil {
		return PaymentRecord{}, err
	}
	p := pays[0]
	note := in.Note
	if runeLen(note) > 200 {
		return PaymentRecord{}, &ValidationError{Field: "note", Msg: "must be 200 characters or fewer"}
	}
	q := repository.New(tx)
	shift, err := s.shiftFor(ctx, q, actor, p.Method == "cash")
	if err != nil {
		return PaymentRecord{}, err
	}
	status, by, at := "pending_confirmation", pgtype.Int8{}, pgtype.Timestamptz{}
	if p.Method == "cash" {
		status, by, at = "confirmed", pgtype.Int8{Int64: actor.UserID, Valid: true}, pgtype.Timestamptz{Time: s.now(), Valid: true}
	}
	ref := pgtype.Text{}
	if p.Reference != "" {
		ref = pgtype.Text{String: p.Reference, Valid: true}
	}
	id, err := q.InsertFolioPayment(ctx, repository.InsertFolioPaymentParams{FolioID: pgtype.Int8{Int64: in.FolioID, Valid: true}, Purpose: in.Purpose, Method: p.Method,
		Status: status, AmountCents: p.AmountCents, Reference: ref, CreatedBy: actor.UserID, ConfirmedBy: by, ConfirmedAt: at, Note: note, ShiftID: shift})
	if isUniqueRef(err) {
		return PaymentRecord{}, &ConflictError{Code: "duplicate_reference", Field: "reference", Msg: "that " + p.Method + " reference was already recorded"}
	}
	if err != nil {
		return PaymentRecord{}, err
	}
	return s.paymentRecord(ctx, q, id)
}

// FolioPaymentTotals is the money on a folio: confirmed counts towards the balance, pending is promised and not yet counted.
func (s *Service) FolioPaymentTotals(ctx context.Context, tx pgx.Tx, folioID int64) (confirmed, pending int64, err error) {
	t, err := repository.New(tx).FolioPaymentTotals(ctx, pgtype.Int8{Int64: folioID, Valid: true})
	return t.ConfirmedCents, t.PendingCents, err
}

// FolioPayments lists the payments on a folio, oldest first. The caller has already checked who may see the folio.
func (s *Service) FolioPayments(ctx context.Context, tx pgx.Tx, folioID int64) ([]PaymentRecord, error) {
	rows, err := repository.New(tx).ListPayments(ctx, repository.ListPaymentsParams{FolioID: folioID, RowLimit: 500})
	if err != nil {
		return nil, err
	}
	out := make([]PaymentRecord, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		out = append(out, PaymentRecord{ID: r.ID, SaleID: optID(r.SaleID.Int64), FolioID: optID(r.FolioID.Int64), Purpose: r.Purpose, ReceiptNo: r.ReceiptNo, Method: r.Method, Status: r.Status,
			AmountCents: r.AmountCents, Reference: r.Reference, CreatedBy: r.CreatedBy, CreatedByName: r.CreatedByName, CreatedAt: r.CreatedAt.Time, ConfirmedBy: optID(r.ConfirmedBy),
			ConfirmedAt: optTime(r.ConfirmedAt), FailedBy: optID(r.FailedBy), FailedAt: optTime(r.FailedAt), Note: r.Note, ShiftID: optID(r.ShiftID)})
	}
	return out, nil
}
