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
