-- Stage 9a: catalogue. Money is BIGINT cents.

-- name: ListCategories :many
SELECT id, module, name, sort_order, active, created_at FROM categories ORDER BY module, sort_order, name, id;

-- name: GetCategory :one
SELECT id, module, name, sort_order, active, created_at FROM categories WHERE id = $1;

-- name: CreateCategory :one
INSERT INTO categories (module, name, sort_order) VALUES ($1, $2, $3) RETURNING id;

-- name: UpdateCategory :exec
UPDATE categories SET name = $2, sort_order = $3, active = $4 WHERE id = $1;

-- name: ListProducts :many
SELECT p.id, p.category_id, p.module, p.name, p.sku, p.price_cents, p.tax_rate_id, p.is_available, p.kitchen_routed,
       p.created_at, p.updated_at, c.name AS category_name,
       t.code AS tax_code, t.name AS tax_name, t.rate_bp AS tax_rate_bp, t.treatment AS tax_treatment
FROM products p
JOIN categories c ON c.id = p.category_id
JOIN tax_rates t ON t.id = p.tax_rate_id
WHERE (sqlc.narg('category_id')::bigint IS NULL OR p.category_id = sqlc.narg('category_id')::bigint)
  AND (sqlc.narg('module')::text IS NULL OR p.module = sqlc.narg('module')::text)
  AND (sqlc.narg('available')::boolean IS NULL OR p.is_available = sqlc.narg('available')::boolean)
  AND (sqlc.narg('q')::text IS NULL OR p.name ILIKE '%' || sqlc.narg('q')::text || '%' OR p.sku ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY c.module, c.sort_order, c.name, p.name, p.id;

-- name: GetProduct :one
SELECT p.id, p.category_id, p.module, p.name, p.sku, p.price_cents, p.tax_rate_id, p.is_available, p.kitchen_routed,
       p.created_at, p.updated_at, c.name AS category_name,
       t.code AS tax_code, t.name AS tax_name, t.rate_bp AS tax_rate_bp, t.treatment AS tax_treatment
FROM products p
JOIN categories c ON c.id = p.category_id
JOIN tax_rates t ON t.id = p.tax_rate_id
WHERE p.id = $1;

-- name: CreateProduct :one
INSERT INTO products (category_id, module, name, sku, price_cents, tax_rate_id, kitchen_routed)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id;

-- name: UpdateProduct :exec
UPDATE products SET category_id = $2, module = $3, name = $4, sku = $5, tax_rate_id = $6, kitchen_routed = $7, updated_at = now()
WHERE id = $1;

-- name: SetProductPrice :exec
UPDATE products SET price_cents = $2, updated_at = now() WHERE id = $1;

-- name: InsertPriceHistory :exec
INSERT INTO price_history (product_id, price_cents, changed_by) VALUES ($1, $2, $3);

-- name: ListPriceHistory :many
SELECT id, product_id, price_cents, effective_from, changed_by FROM price_history WHERE product_id = $1 ORDER BY effective_from DESC, id DESC;

-- name: SetProductAvailability :exec
UPDATE products SET is_available = $2, updated_at = now() WHERE id = $1;

-- name: GetTaxRateForProduct :one
SELECT id, active FROM tax_rates WHERE id = $1;

-- name: ListModifiers :many
SELECT id, name, price_delta_cents, active, created_at FROM modifiers ORDER BY name, id;

-- name: GetModifier :one
SELECT id, name, price_delta_cents, active, created_at FROM modifiers WHERE id = $1;

-- name: CreateModifier :one
INSERT INTO modifiers (name, price_delta_cents) VALUES ($1, $2) RETURNING id;

-- name: UpdateModifier :exec
UPDATE modifiers SET name = $2, price_delta_cents = $3, active = $4 WHERE id = $1;

-- name: ListProductModifierLinks :many
SELECT pm.product_id, pm.modifier_id FROM product_modifiers pm WHERE pm.product_id = ANY($1::bigint[]) ORDER BY pm.product_id, pm.modifier_id;

-- name: CountModifiersByID :one
SELECT count(*) FROM modifiers WHERE id = ANY($1::bigint[]);

-- name: DeleteProductModifiers :exec
DELETE FROM product_modifiers WHERE product_id = $1;

-- name: InsertProductModifier :exec
INSERT INTO product_modifiers (product_id, modifier_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: GetTaxRateIDByCode :one
SELECT id FROM tax_rates WHERE code = $1;

-- name: CountCategories :one
SELECT count(*) FROM categories;
