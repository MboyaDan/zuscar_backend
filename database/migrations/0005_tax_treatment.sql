-- Tax categories (owner instruction, Stage 6). A tax category is a named treatment plus a rate, held as
-- configuration. Nothing in the application assumes one rate: every product and room type must name its
-- category explicitly (enforced when those tables are created in Stages 9 and 20).
--   standard    : VAT charged at rate_bp
--   zero_rated  : taxable supply charged at 0 percent
--   exempt      : outside VAT, nothing charged
-- Whether, where and at what rate a business must charge VAT depends on its VAT registration and KRA
-- rules: see docs/tax-configuration.md. This migration only provides the structure.
ALTER TABLE tax_rates
  ADD COLUMN treatment TEXT NOT NULL DEFAULT 'standard'
    CHECK (treatment IN ('standard', 'zero_rated', 'exempt'));
ALTER TABLE tax_rates ALTER COLUMN treatment DROP DEFAULT;
ALTER TABLE tax_rates
  ADD CONSTRAINT tax_rates_zero_for_nonstandard CHECK (treatment = 'standard' OR rate_bp = 0);
