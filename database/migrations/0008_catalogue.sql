-- Stage 9a: catalogue (docs/schema-constraints.md section 3). Money is integer minor units (cents of KES).
-- Not here yet: products.track_stock and products.inventory_item_id arrive with the inventory tables (Stage 17).

CREATE TABLE categories (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  module     TEXT        NOT NULL CHECK (module IN ('restaurant', 'bar')),
  name       TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 80),
  sort_order INTEGER     NOT NULL DEFAULT 0,
  active     BOOLEAN     NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT categories_module_name_key UNIQUE (module, name),
  -- lets products prove that their module matches their category's module
  CONSTRAINT categories_id_module_key UNIQUE (id, module)
);

-- tax_rate_id is NOT NULL with no default on purpose: nothing is assumed to be standard-rated (docs/tax-configuration.md).
CREATE TABLE products (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  category_id    BIGINT      NOT NULL,
  module         TEXT        NOT NULL,
  name           TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
  sku            TEXT        CHECK (sku IS NULL OR char_length(sku) BETWEEN 1 AND 40),
  price_cents    BIGINT      NOT NULL CHECK (price_cents >= 0),
  tax_rate_id    BIGINT      NOT NULL REFERENCES tax_rates (id) ON DELETE RESTRICT,
  is_available   BOOLEAN     NOT NULL DEFAULT true,
  kitchen_routed BOOLEAN     NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT products_sku_key UNIQUE (sku),
  CONSTRAINT products_category_module_fkey FOREIGN KEY (category_id, module) REFERENCES categories (id, module) ON DELETE RESTRICT
);
CREATE INDEX products_category_idx ON products (category_id);

-- Every price a product has had. Immutable: a price change inserts a row and updates products.price_cents.
CREATE TABLE price_history (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  product_id     BIGINT      NOT NULL REFERENCES products (id) ON DELETE RESTRICT,
  price_cents    BIGINT      NOT NULL CHECK (price_cents >= 0),
  effective_from TIMESTAMPTZ NOT NULL DEFAULT now(),
  changed_by     BIGINT      REFERENCES users (id) ON DELETE RESTRICT
);
CREATE INDEX price_history_product_idx ON price_history (product_id, effective_from DESC);

CREATE TRIGGER price_history_immutable BEFORE UPDATE OR DELETE ON price_history
  FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER price_history_no_truncate BEFORE TRUNCATE ON price_history
  FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TABLE modifiers (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name              TEXT        NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 80),
  price_delta_cents BIGINT      NOT NULL DEFAULT 0, -- signed: a "no ice" modifier may be 0, a discount negative
  active            BOOLEAN     NOT NULL DEFAULT true,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT modifiers_name_key UNIQUE (name)
);

CREATE TABLE product_modifiers (
  product_id  BIGINT NOT NULL REFERENCES products (id) ON DELETE CASCADE,
  modifier_id BIGINT NOT NULL REFERENCES modifiers (id) ON DELETE RESTRICT,
  PRIMARY KEY (product_id, modifier_id)
);
CREATE INDEX product_modifiers_modifier_idx ON product_modifiers (modifier_id);
