-- name: UpsertSetting :exec
INSERT INTO system_settings (key, value, updated_by) VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now();

-- name: GetSetting :one
SELECT value FROM system_settings WHERE key = $1;

-- name: UpsertTaxRate :one
INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ($1, $2, $3, $4)
ON CONFLICT (code) DO UPDATE SET name = EXCLUDED.name, rate_bp = EXCLUDED.rate_bp, treatment = EXCLUDED.treatment
RETURNING id;

-- A counter that already exists is never reset: that would break gapless numbering (A-05).
-- name: CreateCounterIfMissing :exec
INSERT INTO document_counters (doc_type, scope, prefix, next_value) VALUES ($1, '', $2, $3)
ON CONFLICT (doc_type, scope) DO NOTHING;

-- ---- Stage 7b: settings and tax category administration ----

-- name: ListSettings :many
SELECT key, value, updated_at FROM system_settings ORDER BY key;

-- name: ListTaxRates :many
SELECT id, code, name, rate_bp, treatment, active, created_at FROM tax_rates ORDER BY id;

-- name: GetTaxRate :one
SELECT id, code, name, rate_bp, treatment, active, created_at FROM tax_rates WHERE id = $1;

-- name: CreateTaxRate :one
INSERT INTO tax_rates (code, name, rate_bp, treatment) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: UpdateTaxRate :exec
UPDATE tax_rates SET name = $2, rate_bp = $3, active = $4 WHERE id = $1;

-- A gapless document number: the row lock serialises callers and a rolled-back transaction gives the number back (A-05).
-- name: NextDocumentNumber :one
UPDATE document_counters SET next_value = next_value + 1, updated_at = now()
WHERE doc_type = $1 AND scope = '' RETURNING (next_value - 1)::bigint AS value;
