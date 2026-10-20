-- Stage 17: the stock a sale line took (needs 0018 and 0012).
-- What a sale line took from stock, as it was when sold, so a void or refund puts back exactly that (like the tax snapshot).
ALTER TABLE sale_items
  ADD COLUMN inventory_item_id    BIGINT REFERENCES inventory_items (id) ON DELETE RESTRICT,
  ADD COLUMN stock_units_per_sale BIGINT,
  ADD COLUMN stock_location_id    BIGINT REFERENCES stock_locations (id) ON DELETE RESTRICT,
  ADD CONSTRAINT sale_items_stock_snapshot CHECK ((inventory_item_id IS NULL) = (stock_units_per_sale IS NULL) AND (inventory_item_id IS NULL) = (stock_location_id IS NULL));
