# RBAC plan (Stage 2)

Design only. Permission keys come from brief section 4; keys marked `*` are additions proposed here because the brief's list cannot express a listed role duty (S-15). Roles are starter templates: every cell is editable by an authorised administrator except the protected rules in section 3. The backend decides on every request from database state (ADR-04); frontend visibility is cosmetic (AC-10).

Implementation: `internal/identity/catalog/catalog.go` holds these templates and is checked against this document's matrix by `catalog_test.go` on every test run (a drift fails the test). Role templates are seeded by the bootstrap and seed-dev commands.

## 1. Roles
- **SA** = Super Admin
- **BM** = Business Manager
- **CA** = Cashier
- **WA** = Waiter
- **BA** = Bartender
- **RE** = Receptionist
- **ST** = Storekeeper
- **KI** = Kitchen staff
- **AU** = Accountant / Auditor

Role codes: `super_admin`, `business_manager`, `cashier`, `waiter`, `bartender`, `receptionist`, `storekeeper`, `kitchen_staff`, `accountant_auditor`. A user may hold several roles; effective permissions are the union. Super Admin is a non-operational owner/recovery role; the venue owner is expected to hold Super Admin plus Business Manager (S-14, DQ-13).

## 2. Matrix (default templates)
● full   ◐ limited (see scope notes)   · none

| Permission | SA | BM | CA | WA | BA | RE | ST | KI | AU |
|---|---|---|---|---|---|---|---|---|---|
| `sales.view` | · | ● | ● | ◐1 | ◐1 | · | · | · | ● |
| `sales.create` | · | ● | ● | · | ◐2 | · | · | · | · |
| `sales.discount` | · | ● | · | · | · | · | · | · | · |
| `sales.void` | · | ● | · | · | · | · | · | · | · |
| `sales.refund` | · | ● | · | · | · | · | · | · | · |
| `sales.reprint*` | · | ● | ● | · | ◐1 | · | · | · | · |
| `orders.manage*` | · | ● | ● | ● | ● | · | · | · | · |
| `kitchen.view*` | · | ● | ● | ● | · | · | · | ● | · |
| `kitchen.update*` | · | ● | · | ● | · | · | · | ● | · |
| `catalogue.manage*` | · | ● | · | · | · | · | · | · | · |
| `shifts.manage*` | · | ● | ● | · | ◐1 | · | · | · | · |
| `shifts.close` | · | ● | ◐1 | · | ◐1 | · | · | · | · |
| `payments.reconcile` | · | ● | · | · | · | · | · | · | · |
| `inventory.view` | · | ● | · | · | ● | · | ● | · | ● |
| `inventory.adjust` | · | ● | · | · | · | · | ● | · | · |
| `inventory.transfer` | · | ● | · | · | ◐3 | · | ● | · | · |
| `inventory.wastage*` | · | ● | · | · | ◐3 | · | ● | ◐3 | · |
| `inventory.override_negative*` | · | ● | · | · | · | · | · | · | · |
| `purchasing.manage` | · | ● | · | · | · | · | ● | · | · |
| `rooms.view*` | · | ● | ◐4 | ◐4 | ◐4 | ● | · | · | ● |
| `rooms.manage` | · | ● | · | · | · | ● | · | · | · |
| `folios.view*` | · | ● | · | · | · | ● | · | · | ● |
| `folios.post*` | · | ● | ● | ● | ● | ● | · | · | · |
| `folios.manage` | · | ● | · | · | · | ◐5 | · | · | · |
| `reports.view` | · | ● | ◐1 | · | · | · | · | · | ● |
| `reports.export` | · | ● | · | · | · | · | · | · | ● |
| `audit.view*` | ● | ◐6 | · | · | · | · | · | · | ● |
| `users.create` | ● | ◐7 | · | · | · | · | · | · | · |
| `users.update` | ● | ◐7 | · | · | · | · | · | · | · |
| `users.deactivate` | ● | ◐7 | · | · | · | · | · | · | · |
| `users.delete` | ● | · | · | · | · | · | · | · | · |
| `roles.manage` | ● | · | · | · | · | · | · | · | · |
| `permissions.assign` | ● | · | · | · | · | · | · | · | · |
| `system.admin` | ● | · | · | · | · | · | · | · | · |

Scope notes (enforced in the service layer with the permission check, not in the UI):
1. Own records only (own sales, own shift, own tickets/receipts); the Cashier's `reports.view` is their own shift summary.
2. Bar-module products only.
3. Own location only (bar for Bartender, kitchen for Kitchen staff); transfers limited to receiving into their location.
4. In-house room list (room number and guest display name) only, to authorise a room charge. No rates, no guest contact data.
5. Check-out and settlement of a folio; adjustments, write-offs and closing with a balance need a Business Manager.
6. Operational events only; security events (role, permission, user, session changes) are visible to Super Admin and Auditor.
7. Non-privileged targets only; applies to `users.create`, `users.update` and `users.deactivate`; see section 3.

Discounts, voids, refunds and reconciliation are off for Cashier by default and can be granted explicitly (brief 4). Every grant of these writes an audit event.

## 3. Protected rules (AC-10, AC-11, AC-12, NN-07)
1. `system.admin` and the `super_admin` role can only be assigned, changed or removed by a user who holds `system.admin`. Business Manager cannot, even if given `roles.manage` or `permissions.assign`. Enforced in the service and, for the role-permission link, by a database trigger (SPIKE: `system.admin` rejected on a non-protected role).
2. Role and permission administration (`roles.manage`, `permissions.assign`) is Super Admin only by default: Business Manager does not hold them (decision confirmed by owner, S-13). If an administrator later grants these keys to another role, the service still applies the no-escalation guard: an actor can grant a permission or role only if they already hold it, can only edit non-protected roles, and can never touch `system.admin` or the protected Super Admin role. Business Manager may assign existing non-protected roles to non-privileged users through `users.update`, limited to roles whose permissions the manager already holds.
3. The last active Super Admin cannot be deactivated, deleted or have the role removed. Enforced in the service and by a lock-and-count constraint trigger on users and user_roles (SPIKE: passes single and concurrent attempts; a race of two admins deactivating each other left one active).
4. Deactivation, not deletion: `users.delete` is Super Admin only and only for accounts with no history (foreign keys RESTRICT). Deactivating revokes all sessions in the same transaction and keeps attribution.
5. A role cannot be deleted while assigned (FK RESTRICT); protected roles cannot be deleted at all.
6. Sensitive changes (role, permission, user status, password reset, system settings) require the actor's password re-entered within the last 5 minutes (S-16) and write an audit event with before and after values.
7. Role and permission changes apply on the next request because permissions are read from the database each time; the response to the changing actor lists affected users.
8. PIN fast-switch (A-07, Should) is available only to users holding only floor roles (Cashier, Waiter, Bartender); never to Business Manager, Super Admin or Accountant.
9. Super Admin recovery is an offline CLI run by someone with server access (documented at Stage 26): it resets a named Super Admin's password, forces a password change, revokes sessions, and writes an audit row with actor `cli`. No emailed back door (brief 6).

## 4. Tests planned (written at Stage 7, names fixed here)
- `TestAC10_EveryProtectedRouteDeniesWithoutPermission`: table-driven over the route list from Stage 3 for each role.
- `TestAC11_ManagerHasNoRoleAdminPermissions` (default template), `TestAC11_RoleAdminWithoutSystemAdminCannotGrantIt` (a custom role given roles.manage still cannot grant system.admin or Super Admin), and `TestAC11_DBRejectsSystemAdminOnNonProtectedRole` (database).
- `TestAC12_LastSuperAdminCannotBeRemoved` (deactivate, role removal, delete) and `TestAC12_ConcurrentDeactivationLeavesOne` (two transactions).
- `TestAC12_DeactivationKeepsHistory`.
Database tests skip loudly when `TEST_DATABASE_URL` is unset and will be reported under "Not run" until they have run.
