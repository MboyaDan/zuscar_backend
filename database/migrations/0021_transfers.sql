-- Stage 18: transfers between stock places (docs/schema-constraints.md section 5, AC-06).
-- A transfer is two ledger rows that share a transfer_group: stock out of one place and the same amount into another.
-- Written together or not at all, and for each group and item the amounts must add up to zero.

ALTER TABLE inventory_movements
  ADD CONSTRAINT inventory_movements_transfer_group CHECK ((movement_type IN ('transfer_out', 'transfer_in')) = (transfer_group IS NOT NULL));
CREATE INDEX inventory_movements_transfer_group_idx ON inventory_movements (transfer_group) WHERE transfer_group IS NOT NULL;

CREATE FUNCTION check_transfer_group() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s BIGINT; places INT; items INT;
BEGIN
  SELECT COALESCE(sum(qty_delta), 0), count(DISTINCT location_id), count(DISTINCT item_id)
    INTO s, places, items FROM inventory_movements WHERE transfer_group = NEW.transfer_group;
  IF s <> 0 OR places <> 2 OR items <> 1 THEN
    RAISE EXCEPTION 'a transfer is one item leaving one place and arriving in another, for the same amount' USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER inventory_movements_transfer_nets_to_zero AFTER INSERT ON inventory_movements DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW WHEN (NEW.transfer_group IS NOT NULL) EXECUTE FUNCTION check_transfer_group();
