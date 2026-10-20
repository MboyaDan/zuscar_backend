-- Stage 20: what actually arrived. A purchase line keeps what was written down (the order); when the purchase is
-- received, each line also records what really came in and at what price, once. Short deliveries and price
-- differences are therefore visible, and the receipt movement books what arrived, not what was ordered.

DROP TRIGGER purchase_items_immutable ON purchase_items;

ALTER TABLE purchase_items
  ADD COLUMN received_qty             BIGINT CHECK (received_qty IS NULL OR received_qty >= 0),            -- in the entered unit
  ADD COLUMN received_qty_base        BIGINT CHECK (received_qty_base IS NULL OR received_qty_base >= 0),
  ADD COLUMN received_unit_cost_cents BIGINT CHECK (received_unit_cost_cents IS NULL OR received_unit_cost_cents >= 0),
  ADD CONSTRAINT purchase_items_received_all_or_none CHECK ((received_qty IS NULL) = (received_qty_base IS NULL) AND (received_qty IS NULL) = (received_unit_cost_cents IS NULL)),
  ADD CONSTRAINT purchase_items_received_zero_pair CHECK (received_qty IS NULL OR ((received_qty = 0) = (received_qty_base = 0)));

-- Purchases received in Stage 19 arrived exactly as written.
UPDATE purchase_items pi SET received_qty = pi.entered_qty, received_qty_base = pi.qty_base, received_unit_cost_cents = pi.unit_cost_cents
FROM purchases p WHERE p.id = pi.purchase_id AND p.status = 'received';

-- The order never changes. The received figures are written once, while the purchase is still a draft.
CREATE FUNCTION guard_purchase_item_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'a purchase line is never deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.purchase_id <> OLD.purchase_id OR NEW.item_id <> OLD.item_id OR NEW.entered_qty <> OLD.entered_qty OR NEW.entered_unit_id <> OLD.entered_unit_id
     OR NEW.qty_base <> OLD.qty_base OR NEW.unit_cost_cents <> OLD.unit_cost_cents THEN
    RAISE EXCEPTION 'what was ordered on a purchase line never changes' USING ERRCODE = '23514';
  END IF;
  IF OLD.received_qty IS NOT NULL THEN
    RAISE EXCEPTION 'what arrived is recorded once' USING ERRCODE = '23514';
  END IF;
  IF (SELECT status FROM purchases WHERE id = NEW.purchase_id) <> 'draft' THEN
    RAISE EXCEPTION 'only a draft purchase can be received' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER purchase_items_guard BEFORE UPDATE OR DELETE ON purchase_items FOR EACH ROW EXECUTE FUNCTION guard_purchase_item_update();

-- A purchase is marked received only when every line says what arrived.
CREATE OR REPLACE FUNCTION guard_purchase_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' THEN
    RAISE EXCEPTION 'a purchase that is received or cancelled cannot change' USING ERRCODE = '23514';
  END IF;
  IF NEW.purchase_no <> OLD.purchase_no OR NEW.supplier_id <> OLD.supplier_id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'a purchase keeps its number, supplier and author' USING ERRCODE = '23514';
  END IF;
  IF NEW.status = 'received' AND EXISTS (SELECT 1 FROM purchase_items WHERE purchase_id = NEW.id AND received_qty IS NULL) THEN
    RAISE EXCEPTION 'every line must say what arrived' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$;
