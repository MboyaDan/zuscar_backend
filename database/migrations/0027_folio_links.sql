-- Stage 23: the links from payments and sales to the guest folio, kept apart from 0026 so the rooms module's generated
-- code reads only its own tables.
CREATE TRIGGER payments_open_folio BEFORE INSERT ON payments FOR EACH ROW EXECUTE FUNCTION require_open_folio();
-- The foreign key Stage 11 left open, and: a sale payment belongs to a sale, a deposit or settlement to a folio.
ALTER TABLE payments ADD CONSTRAINT payments_folio_fk FOREIGN KEY (folio_id) REFERENCES guest_folios (id) ON DELETE RESTRICT;
ALTER TABLE payments ADD CONSTRAINT payments_purpose_target CHECK ((purpose = 'sale_payment') = (sale_id IS NOT NULL));
CREATE INDEX payments_folio_idx ON payments (folio_id);
ALTER TABLE sales ADD CONSTRAINT sales_folio_fk FOREIGN KEY (folio_id) REFERENCES guest_folios (id) ON DELETE RESTRICT;
