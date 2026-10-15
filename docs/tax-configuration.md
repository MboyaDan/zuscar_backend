# Tax configuration (owner instruction, Stage 6)

**Status: PROVISIONAL. This is a development configuration, not a verified production tax setup.**
The production tax configuration must be verified against the business's VAT registration and the applicable KRA requirements before go-live. Nothing in this document is legal or tax advice, and the system does not claim eTIMS compliance.

## What exists now
- **Tax categories are configuration** in the `tax_rates` table (migration 0005), not code. Each has a `treatment`:
  - `standard`: tax charged at `rate_bp` (basis points; 1600 = 16.00%).
  - `zero_rated`: taxable supply charged at 0%. `rate_bp` must be 0 (database CHECK).
  - `exempt`: outside the tax. `rate_bp` must be 0 (database CHECK).
  More than one `standard`-treatment category can exist (so a different rate is possible without code changes).
- **Provisional default.** `bootstrap` and `seed-dev` create three categories: `standard` at **1600 basis points**, `zero_rated`, `exempt`. 1600 is a provisional development default supplied by the owner for the commonly quoted standard Kenyan VAT rate. It is the default of the bootstrap flag `--tax-rate-bp` and is defined once, as `provision.ProvisionalStandardRateBP`. A test (`TestTaxRateIsNotHardCoded`) fails if the value appears in any other production Go file.
- **Status flag.** `system_settings.tax_config_status` is set to `provisional_unverified` at bootstrap. The bootstrap command prints a loud notice, and its audit record includes the provisional status.
- **No implicit category.** Products (Stage 9) and room types (Stage 20) must reference a tax category explicitly (NOT NULL, no default). The system never assumes that every restaurant, bar or accommodation item is standard-rated.
- **Snapshots.** From Stage 11, each sale line stores the category's `treatment` and `rate_bp` at the time of sale, and reports break tax down by treatment, so later rate changes never rewrite history (see `schema-constraints.md`).
- **Prices.** Tax-inclusive pricing is the working assumption (A-04) and is itself configuration (`tax_inclusive`), to be confirmed.

## Not done and not claimed
- **eTIMS:** not integrated. Receipts from this system are receipts, not KRA tax invoices (NN-12, DG-01). Compliance will not be claimed until the integration route has been verified and tested (Stage 24 and after).
- **Other taxes or levies** that may apply to food, drinks or accommodation are not modelled. Confirm with the accountant before go-live.
- **Marking the configuration verified:** the admin endpoint that sets `tax_config_status` to `verified` (system.admin, password re-entry, audited) is built in Stage 7. Until then the status stays provisional, and the Stage 26 go-live checklist requires it to be `verified`.

## Go-live verification checklist (all items required)
1. Confirm whether the business is VAT-registered, with its registration details. If it is not, the system must not be configured to add VAT.
2. Confirm the correct treatment for each product category (food, soft drinks, alcohol, accommodation, services) and set each product and room type to it explicitly.
3. Confirm the standard rate and any other rates, and update `tax_rates`.
4. Confirm whether prices are tax-inclusive or tax-exclusive, and the rounding rule on receipts.
5. Confirm any other applicable taxes or levies and whether they must appear on receipts.
6. Confirm the eTIMS or other invoicing obligation and the integration route (DG-01).
7. Record who verified it and when, then set `tax_config_status` to `verified`.

## Questions for the accountant
VAT registration status; treatment per item category; other levies; inclusive or exclusive pricing; invoice and credit-note obligations; whether manual receipts during outages need special handling.
