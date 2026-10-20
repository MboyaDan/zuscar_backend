-- Stage 17: link products to stock (needs 0018_inventory.sql).
-- A product takes stock when it is sold: stock_units_per_sale base units of its item per unit sold
-- (a bottle sold is 1 piece; a 25 ml tot of a 750 ml bottle is 25 ml). No recipes (DQ-08).
ALTER TABLE products
  ADD COLUMN track_stock          BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN inventory_item_id    BIGINT  REFERENCES inventory_items (id) ON DELETE RESTRICT,
  ADD COLUMN stock_units_per_sale BIGINT  NOT NULL DEFAULT 1 CHECK (stock_units_per_sale > 0 AND stock_units_per_sale <= 1000000),
  ADD CONSTRAINT products_track_stock_item CHECK (track_stock = (inventory_item_id IS NOT NULL));
