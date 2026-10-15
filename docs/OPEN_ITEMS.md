# Open items and tracked decisions

Updated at Stage 7a. S-34 was approved by the owner; S-35 to S-37 are defaults in use. "Default in use" means the build proceeds on it; nothing here is confirmed until the Status says so.

## Explicitly tracked by the owner
| ID | Item | Default in use | Status |
|---|---|---|---|
| S-17 | Kitchen ticket statuses may skip forward (for example submitted straight to served) because the default kitchen has no screen | Skipping allowed | **OPEN**, to be confirmed by the owner before Stage 10 |
| S-18 | A sale can be voided only from finalised with no refunds; later corrections are refunds | Void only before any refund | **OPEN**, owner and accountant to confirm before Stage 15 |
| S-21 | A shift can be closed while non-cash payments (for example manual M-Pesa) are still pending; they are listed separately on the cash-up | Close allowed | **OPEN**, owner and accountant to confirm before Stage 12 |
| S-15 | Extra permission keys beyond the brief: sales.reprint, orders.manage, kitchen.view, kitchen.update, catalogue.manage, shifts.manage, inventory.wastage, inventory.override_negative, rooms.view, folios.view, folios.post, audit.view | **Retained as seeded** | Retained at the owner's instruction. Conflict check (Stage 6): no conflict found. Every key is in the catalog, matches `docs/rbac.md` (`TestCatalogMatchesRBACDocument`), and is used by the planned routes (`TestRoutePermissionsExistInCatalog`); the owner decision that Business Manager lacks `roles.manage` and `permissions.assign` is enforced (`TestProtectedRulesInTemplates`). Re-check if the brief changes. |

## Tax (owner instruction, Stage 6)
| ID | Item | Status |
|---|---|---|
| T-01 | Standard rate 1600 basis points is a **provisional development default**; production tax configuration must be verified against VAT registration and KRA requirements before go-live (`docs/tax-configuration.md`) | **OPEN, go-live blocker** |
| T-02 | Tax category per product and room type (standard, zero-rated, exempt) must be set explicitly; no implicit default | OPEN, enforced from Stages 9 and 20 |
| T-03 | eTIMS integration route (DG-01) | OPEN, gated; no compliance claim until verified and tested |

## Other open decisions from the brief
DG-02 (M-Pesa API route, gated), DG-03 (printer model and connection, tested with the printer test sheet), DG-04 (hosting plans and budget).

## Assumptions awaiting confirmation (defaults in use)
S-01 to S-14, S-16, S-19, S-20, S-22 to S-33 are listed in `ASSUMPTIONS.md` with who must confirm each. S-13 is confirmed.
