// Package idempotency makes retries of a request safe (ADR-08, S-06). A route that needs it carries the Required or
// Optional middleware, which checks the Idempotency-Key header and hashes the request. The service then calls Begin
// as the first step of its own database transaction and Complete as the last, so the claim, the work and the stored
// response commit together or not at all. Clients generate one key per user intent, not per HTTP attempt.
//
// Outcomes for a key on an endpoint:
//   - first use: Begin returns nil and the service does the work;
//   - same key, same user and same request after the first finished: Begin returns the stored response;
//   - same key at the same moment as the first: the second call waits for the first transaction, then replays it
//     (after ten seconds it gives up with ErrInFlight, HTTP 409 idempotency_conflict);
//   - same key with a different user or request: ErrKeyReused (HTTP 422 idempotency_key_reused);
//   - the first attempt failed: its transaction rolled back, so the key is free and a retry runs normally.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
)

const Header = "Idempotency-Key"

// ReplayHeader is set to "true" on a response that was stored by an earlier request with the same key.
const ReplayHeader = "Idempotent-Replayed"

var (
	ErrKeyReused = errors.New("this Idempotency-Key was already used for a different request")
	ErrInFlight  = errors.New("a request with this Idempotency-Key is still being processed")
)

// LockTimeout is how long a request waits for another request that is using the same key before it answers 409
// idempotency_conflict. It is a variable only so a test can shorten it.
var LockTimeout = "10s"

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// Key is one request's claim. It is built by the middleware and handed to the service.
type Key struct {
	Value    string
	Endpoint string
	UserID   int64
	Hash     [32]byte
}

// Stored is a response saved by an earlier request with the same key.
type Stored struct {
	Status int
	Body   []byte
}

const ctxKey = "idempotency.key"

// Required rejects a request without the header with 400. Optional lets it through without a key.
func Required() gin.HandlerFunc { return middleware(true) }
func Optional() gin.HandlerFunc { return middleware(false) }

func middleware(required bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		v := strings.TrimSpace(c.GetHeader(Header))
		if v == "" {
			if required {
				zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: Header, Message: "header is required"})
				return
			}
			c.Next()
			return
		}
		if !keyPattern.MatchString(v) {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: Header, Message: "must be 8 to 128 letters, digits, dots, dashes, underscores or colons"})
			return
		}
		p := auth.From(c)
		if p == nil {
			zhttp.WriteError(c, http.StatusUnauthorized, "unauthenticated", "Please sign in.")
			return
		}
		var body []byte
		if c.Request.Body != nil {
			var err error
			if body, err = io.ReadAll(c.Request.Body); err != nil {
				zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "The request body could not be read.")
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		h := sha256.New()
		for _, part := range [][]byte{[]byte(c.Request.Method), []byte(c.Request.URL.Path), []byte(c.Request.URL.RawQuery), body} {
			h.Write(part)
			h.Write([]byte{0})
		}
		k := &Key{Value: v, Endpoint: c.Request.Method + " " + c.FullPath(), UserID: p.UserID}
		copy(k.Hash[:], h.Sum(nil))
		c.Set(ctxKey, k)
		c.Next()
	}
}

// From returns the key the middleware stored for this request, or nil when none was sent.
func From(c *gin.Context) *Key {
	if v, ok := c.Get(ctxKey); ok {
		if k, ok := v.(*Key); ok {
			return k
		}
	}
	return nil
}

// Begin claims the key inside tx, which must be the transaction that does the work. It returns the stored response
// to replay, or nil when the caller should do the work and then call Complete. The key's life is the
// idempotency_ttl_hours setting (default 24), never less than 24 hours (S-06).
func Begin(ctx context.Context, tx pgx.Tx, k *Key) (*Stored, error) {
	// Wait at most LockTimeout for a concurrent request with the same key, then report it as in flight.
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, LockTimeout); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM idempotency_records WHERE idem_key = $1 AND endpoint = $2 AND expires_at <= now()`, k.Value, k.Endpoint); err != nil {
		return nil, mapLock(err)
	}
	var id int64
	err := tx.QueryRow(ctx, `
INSERT INTO idempotency_records (idem_key, endpoint, user_id, request_hash, expires_at)
VALUES ($1, $2, $3, $4, now() + make_interval(hours => GREATEST(24, COALESCE((SELECT (value #>> '{}')::int FROM system_settings WHERE key = 'idempotency_ttl_hours'), 24))))
ON CONFLICT (idem_key, endpoint) DO NOTHING RETURNING id`, k.Value, k.Endpoint, k.UserID, k.Hash[:]).Scan(&id)
	if err == nil {
		_, err = tx.Exec(ctx, `SELECT set_config('lock_timeout', '0', true)`)
		return nil, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, mapLock(err)
	}
	// Someone else holds a committed record for this key.
	var (
		user   int64
		hash   []byte
		status int
		body   []byte
	)
	err = tx.QueryRow(ctx, `SELECT user_id, request_hash, COALESCE(response_status, 0), COALESCE(response_body::text, '') FROM idempotency_records WHERE idem_key = $1 AND endpoint = $2`,
		k.Value, k.Endpoint).Scan(&user, &hash, &status, &body)
	if err != nil {
		return nil, err
	}
	if user != k.UserID || !bytes.Equal(hash, k.Hash[:]) {
		return nil, ErrKeyReused
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', '0', true)`); err != nil {
		return nil, err
	}
	return &Stored{Status: status, Body: body}, nil
}

// Complete stores the response for the key claimed by Begin, in the same transaction. Only successful (2xx)
// responses are stored: a failed request rolls back, which frees the key for a corrected retry.
func Complete(ctx context.Context, tx pgx.Tx, k *Key, status int, body []byte) error {
	_, err := tx.Exec(ctx, `UPDATE idempotency_records SET status = 'completed', response_status = $3, response_body = $4::jsonb WHERE idem_key = $1 AND endpoint = $2`,
		k.Value, k.Endpoint, status, string(body))
	return err
}

// Purge removes expired records and returns how many it removed. Nothing newer than the retention period is ever
// removed (the table forbids it). Run it from a scheduled job.
func Purge(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM idempotency_records WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func mapLock(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "55P03" { // lock_not_available
		return ErrInFlight
	}
	return err
}
