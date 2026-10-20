# Requirements traceability (Stage 3)

Maps every acceptance criterion and non-negotiable rule to planned verification. Nothing in the Status column is "passed": tests do not exist yet. "Spiked" means the underlying database constraint was exercised on a scratch database at Stage 2 (`docs/spikes/stage2-constraints-spike.sql`), which is evidence for the design only, not for the application. Test names are fixed here so later stages and commit messages can cite them. DB = needs PostgreSQL; those tests skip loudly when `TEST_DATABASE_URL` is unset and are listed as "not run" until they have run somewhere (locally: `make test-db`).

## Acceptance criteria
| ID | Planned verification | Type | DB | Stage | Status |
|---|---|---|---|---|---|
| AC-01 | `TestAC01_FinaliseSnapshotsCorrect`, `TestAC01_RetrySameKeyReturnsStoredSale`, `TestAC01_ConcurrentSameKeyCreatesOneSale`, `TestAC01_SecondFinaliseOfSameOrderRejected` | Go integration | Y | 11 | Stage 11a: all four tests pass against PostgreSQL; `TestAC01_FinaliseSnapshotsCorrect` also covers the bill, tax, discounts, scope, immutability, duplicate reference, a forced failure (no receipt-number gap) and payment independence of the kitchen ticket |
| AC-02 | `TestAC02_RetryPaymentNotDoubleRecorded`, `TestAC02_DuplicateReferenceRejected`, `TestAC02_DuplicateProviderEventIgnored` (schema level; callback handler gated by DG-02) | Go integration | Y | 11, 24 | Stage 11b: all three pass against PostgreSQL; the callback handler stays gated by DG-02 (Stage 24) |
| AC-03 | `TestAC03_PrintFailureKeepsOneSale`, `TestAC03_RetryNeverDuplicatesTicket`, `TestAC03_ReprintLinkedAndMarked` (+ frontend template test that REPRINT renders) | Go integration, frontend unit | Y | 14 | Stage 14a: the three Go tests pass against PostgreSQL (plus permission, visibility and database-protection tests); the frontend check that REPRINT renders is in 14b |
| AC-04 | `TestAC04_TicketPersistedBeforePrint`, `TestAC04_TicketStatusIndependentOfPayment`, `TestAC04_ChangesCreateLinkedUpdateTickets`, `TestAC04_CancelAfterSendCreatesCancelTicket` | Go integration | Y | 10 | Stage 10b-1: all four tests pass against PostgreSQL; send-to-kitchen idempotency tested in 10b-2. Payment independence is re-run against a finalised and a paid sale in Stage 11; the print half of "persisted before print" is covered in Stage 14 |
| AC-05 | `TestAC05_DiscountNeedsPermissionAndReason`, `TestAC05_VoidNeedsPermissionAndReason`, `TestAC05_RefundNeedsPermissionAndReason`, each asserting an audit row | Go integration | Y | 11, 15 | Stage 15a: all three pass against PostgreSQL, plus `TestS18_VoidRules`, `TestRefundPiecesAddUpExactly`, `TestRefundPayoutsAreCapped`, `TestRefundConcurrentOnlyOneWins` (8 at once, one wins), `TestRefundsAreProtectedInTheDatabase` and `TestRefundQuotePermissionAndShape`; mutation checks (sale lock removed, same-day rule removed) made the race and void tests fail |
| AC-06 | `TestAC06_BalanceEqualsSumOfMovements`, `TestAC06_LedgerRejectsUpdateDeleteTruncate`, `TestAC06_TransferPairsNetToZero`, `TestAC06_ConversionsBottleCrateShot`, `TestAC06_SaleDeductionNotRepeatedOnRetry`, `TestAC06_ReversalIsCompensating`, `TestAC06_PurchaseReceiveBooksReceiptsWithCost` | Go integration | Y | 17, 18, 19, 20 | Stage 17 RAN all but the transfer test: also `TestAC06_NegativeStockNeedsOverride`, `TestAC06_OpeningBalanceOncePerPlace`, `TestAC06_BarProductsComeFromTheBar`, `TestAC06_SaleNeverBlockedByEmptyShelfUnlessPolicyBlocks`, `TestAC06_VoidRestocksRefundOnlyWhenAsked`, `TestAC06_OutageEntryTakesStock`, `TestAC06_ConcurrentAdjustmentsNeverGoBelowZero`, `TestAC06_PermissionsAndIdempotency`; Stage 18 RAN `TestAC06_TransferPairsNetToZero`, `TestAC06_TransferRules`, `TestAC06_ConcurrentTransfersNeverOverdraw`, `TestAC06_WastageBreakageComplimentary`; Stage 19 RAN `TestAC06_PurchaseReceiveBooksReceiptsWithCost`, `TestAC06_PurchaseRules`, `TestAC06_ConcurrentReceivesBookOnce`; Stage 20 RAN `TestAC06_ReceiveRecordsWhatActuallyArrived`; AC-06 is complete |
| AC-07 | `TestAC07_ThirteenthAndFourteenthRoomViaAPI` (no code change), `TestAC07_ConcurrentBookingsOnlyOneWins`, `TestAC07_BackToBackAllowed`, `TestAC07_TentativeDoesNotBlock` | Go integration | Y | 20 | exclusion constraint spiked |
| AC-08 | `TestAC08_FolioBalanceFromItemsPaymentsAdjustments`, `TestAC08_RoomChargeIsNotPayment`, `TestAC08_PosChargePostsOnce`, `TestAC08_CloseNeedsZeroBalanceOrWriteOff` | Go integration | Y | 21 | planned |
| AC-09 | `TestAC09_ExpectedCashFromFixtures`, `TestAC09_VarianceStoredNotAbsorbed`, `TestAC09_PendingMpesaExcludedFromCash` | Go integration | Y | 12 | Stage 12: all three pass against PostgreSQL, plus `TestAC09_CloseRacingPaymentsStaysConsistent` and `TestStage12_ShiftRules` |
| AC-10 | `TestAC10_EveryRouteDeniesWithoutPermission` (reads `docs/api/routes.csv`: 401 unauthenticated, 403 for each role lacking the key), `TestAC10_RouteTableMatchesRegisteredRoutes`, cases for sales.discount, inventory.override_negative, reports.export | Go integration | Y | 7, extended each stage | groundwork RAN at Stage 6: authentication (401 without a session) on every /auth route, permissions read from the database per request (`TestPermissionChangesApplyOnTheNextRequestWithoutRelogin`); user routes covered at Stage 7a by `TestAC10_UserAdminRoutesDenyWithoutPermission` (401 signed out, 403 without the key, 403 `password_change_required`; RAN); the route-table test `TestAC10_EveryProtectedRouteDeniesWithoutPermission` RAN at Stage 7b-2 over every stage 7 and permission-named row of routes.csv for all nine default roles (signed out 401; without the key 403 forbidden; with the key never forbidden); later stages' routes are checked by the same test as they are added |
| AC-11 | `TestAC11_ManagerHasNoRoleAdminPermissions`, `TestAC11_RoleAdminWithoutSystemAdminCannotGrantIt`, `TestAC11_DBRejectsSystemAdminOnNonProtectedRole` | Go integration | Y | 5 (database), 7 (service) | database half RAN at Stage 5 (`TestAC11_DBRejectsSystemAdminOnNonProtectedRole`, `TestAC11_BusinessManagerTemplateHasNoRoleAdministration`, `TestProtectedRulesInTemplates`); service-level user-side tests RAN at Stage 7a (`TestAC11_ManagerHasNoRoleAdminPermissions`, `TestAC11_ManagerCannotGrantOrTouchSuperAdmin`, `TestAC11_RoleGrantNeedsHeldPermissions`; three mutation checks made them fail); `TestAC11_RoleAdminWithoutSystemAdminCannotGrantIt` RAN at Stage 7b-1 (a custom role with roles.manage and permissions.assign cannot grant system.admin, touch Super Admin, or grant permissions it lacks; mutation checks made it fail); `TestRoleAdminLifecycle` covers role CRUD, audit before/after and affected users |
| AC-12 | `TestAC12_LastSuperAdminCannotBeRemoved` (deactivate, role removal, delete), `TestAC12_ConcurrentDeactivationLeavesOne`, `TestAC12_DeactivationKeepsHistory` | Go integration | Y | 5 (database), 7 (service) | database half RAN at Stage 5, all three passing; a mutation check (lock removed) made the race test fail; service-level tests RAN at Stage 7a: all three passing (`TestAC12_LastSuperAdminCannotBeRemoved` covers deactivate, role removal, role replacement and delete; `TestAC12_ConcurrentDeactivationLeavesOne` runs 5 rounds of two admins deactivating each other; `TestAC12_DeactivationKeepsHistory`) |
| AC-13 | `TestAC13_RealMigrationsApplyToEmptyDatabaseAndRerunIsNoOp`, `TestAPINeverImportsSetupOrSeedCode` (API binary cannot reach migrate/seed code), CI job, deployment doc review | Go integration, CI, doc review | Y | 5, 26 | tests RAN at Stage 5; CI job written, not run |
| AC-14 | Secrets scan script over the repo and the built frontend bundle; `.env.example` placeholders only; HTTPS check after deploy | Script, manual | N | 25, 26 | partly: `.env.example` has placeholders; API refuses insecure production config (`TestProductionRules`, ran); log redaction (`TestSensitiveAttributesRedacted`, ran) |
| AC-15 | Backup configuration review and a real restore drill into a fresh database, recorded in `docs/operations-and-recovery.md` | Manual (owner, Supabase) | n/a | 26 | planned; cannot run in sandbox |
| AC-16 | `docs/printer-test-sheet.md` executed on the real printer, browser and Windows setup | Manual (on site) | n/a | 14 (sheet), owner executes | planned; needs hardware |
| AC-17 | `TestAC17_DuplicateManualReceiptRejected`, runbook review, staff walkthrough | Go integration, manual | Y | 16, 26 | Stage 16: `TestAC17_DuplicateManualReceiptRejected` (spacing and case variants, no receipt number used up, no money recorded), `TestAC17_TwoClerksEnteringTheSameReceiptAtOnce` (6 at once, one wins), `TestAC17_RetryWithSameKeyReplays`, `TestAC17_OnlyReconcilersMayEnterOrList`, `TestAC17_EntryIsValidated`, `TestAC17_OutageSaleBehavesLikeASale`, `TestAC17_ListFindsEntriesByNumber`, `TestAC17_DatabaseRefusesHalfEntries`; mutation checks (case folding removed; duplicate check and its constraint mapping removed) made the duplicate and race tests fail. Runbook written (`docs/runbooks/outage.md`); the staff walkthrough is still to do |
| AC-18 | Gated by DG-01. States and boundary documented at Stage 24; compliance testing only after the route is verified | Manual, gated | n/a | 24+ | not started, gated |
| AC-19 | `TestAC19_TypedReferenceNeverMarksPaid`, `TestAC19_CollectionsCountConfirmedOnly`; provider-verified flow gated by DG-02 | Go integration, gated | Y | 11 | planned (manual path); provider gated |
| AC-20 | `TestAC20_ReportsReconcileToFixtures` (sales, collections, balances due, expenses, estimated profit shown separately and labelled) | Go integration | Y | 23 | planned |
| AC-21 | Handover checklist, training guide, limitations list | Doc review | n/a | 26 | planned |

## Non-negotiables
| ID | Enforced by | Check |
|---|---|---|
| NN-01 | No tenant column anywhere | Schema test: no column named like tenant or org id (Stage 25) |
| NN-02 | No service worker or offline queue | Repo grep in CI (Stage 25) |
| NN-03 | Kitchen ticket print on the 80 mm printer | AC-04, AC-16 |
| NN-04 | Atomic idempotent finalisation | AC-01 tests |
| NN-05 | Print separate from persistence | AC-03 tests |
| NN-06 | Service-level authorisation | AC-10 tests |
| NN-07 | Protected system.admin | AC-11 tests, DB trigger |
| NN-08 | Immutability triggers on audit_logs, sale_items, folio_items, inventory_movements, cash_movements, refunds | `TestNN08_AuditLogsRejectUpdateDeleteTruncate` RAN at Stage 5 (audit_logs only); other tables as they are created |
| NN-09 | BIGINT cents and base-unit integers | CHECK constraints; lint test forbidding float types in money packages; money rounding table test |
| NN-10 | Folio postings are not payments | AC-08 tests, schema CHECKs |
| NN-11 | Pending until confirmed | AC-19 tests |
| NN-12 | No compliance claim | Review plus grep for tax-invoice wording in UI and print templates (Stage 24, 25) |
| NN-13 | Production database not in Docker | Compose file contains only a local db service (Stage 4, file review only, Compose not run); deployment doc review (Stage 26) |
| NN-14 | No fixed timeline or exact hardware price | Doc review (Stage 26) |

## Test infrastructure plan
- Go tests tagged by need: unit (no DB), integration (DB). `make test` runs unit; `make test-db` runs integration against `TEST_DATABASE_URL` (a throwaway database; tests refuse to run if `APP_ENV=production`).
- CI starts a PostgreSQL service container, applies migrations, then runs both.
- Concurrency tests use real goroutines and separate pooled connections, not mocks.
- Each stage report lists which tests ran in the sandbox and which did not, with the exact local command.

## Stage 5 additions
| Item | Evidence |
|---|---|
| Prompt section 8: seed-dev refuses in production | `TestGuardRefusesProduction`, `TestRunRefusesProductionBeforeAnyDatabaseAccess` (no DB), binary run with APP_ENV=production exited 3 |
| Prompt section 8: seed code never in the API | `TestAPINeverImportsSetupOrSeedCode`, `TestOnlySeedCommandImportsDevseed`; production Docker stage has no seed binary (Dockerfile, not built here) |
| Prompt section 8: bootstrap creates only the minimum | `TestBootstrapCreatesOnlyTheMinimum`, `TestBootstrapRefusesWhenUsersExist`, `TestFailedBootstrapLeavesNothingBehind`, `TestBootstrapValidatesBeforeTouchingDatabase`, `TestBootstrapRefusesSeededDatabase` |
| Counters never reset | `TestApplyBaseDataKeepsExistingCounters` |
| Role templates match docs/rbac.md | `TestCatalogMatchesRBACDocument`, `TestRoutePermissionsExistInCatalog` |
| Argon2id | `internal/identity/password` tests (round trip, unique salts, malformed hashes, rehash, policy) |

## Stage 6 additions (all RAN against PostgreSQL 16.15 unless stated)
| Requirement | Evidence |
|---|---|
| Argon2id, opaque server-side sessions, only SHA-256 stored (ADR-04) | `TestSessionAndCSRFTokensAreStoredOnlyAsHashes`, `TestLoginSetsHardenedCookiesAndMeReturnsRolesAndPermissions` (HttpOnly, SameSite=Lax, Secure) |
| Permissions read from the database on each request | `TestPermissionChangesApplyOnTheNextRequestWithoutRelogin`, `TestInactiveUserCannotLoginAndExistingSessionStopsWorking` |
| Login rate limiting and lockout | `TestLockoutAfterMaxAttemptsThenReleasesAfterWindow`, `TestSuccessfulLoginResetsTheFailureCounter`, `TestUnknownUsernamesAreThrottledToo`, `TestPerIPRateLimitIgnoresSpoofedForwardedHeaders`, `TestWrongPasswordAndUnknownUserGiveIdenticalResponses` |
| Session expiry | `TestIdleAndAbsoluteExpiry` |
| CSRF (ADR-13) | `TestCSRFProtection` (missing, wrong, other-session, forged pair, foreign Origin, foreign Referer, valid, script client), `TestLoginRejectsForeignOrigin`; mutation check: weakening the session-hash comparison makes `TestCSRFProtection` fail |
| Forced password change at first login | `TestMustChangePasswordBlocksOrdinaryRoutesUntilChanged` |
| Change password, other sessions ended, dev password refused | `TestChangePassword`, `TestWrongCurrentPasswordOnChangeCountsTowardLockout` |
| Re-authentication window (S-16) | `TestReauthWindow` |
| PIN fast-switch (A-07) | `TestPINSwitchHandsTheTerminalToAnotherFloorUser`, `TestPINSwitchIsBoundByTheOriginalAbsoluteExpiry` (mutation check: removing the bound fails it), `TestPINIsRefusedForNonFloorRolesWithoutRevealingWhoTheyAre`, `TestPINFormatRules`, `TestPINLockoutAndRecovery`, `TestPINAuditTrail`, `TestFloorRolesAreExactlyCashierWaiterBartender` |
| Audit of security events, no secrets in logs | `TestAuditTrailRecordsSecurityEventsWithoutSecrets` (also checks ip and request id) |
| Tax categories and provisional status (owner instruction) | `TestBootstrapCreatesOnlyTheMinimum` (three categories, `provisional_unverified`), `TestTaxCategoryConstraints`, `TestProvisionalRateConstantIsOwnerValue`, `TestTaxRateIsNotHardCoded` |
| Bug found by tests | `TestGuardIgnoresUsersWhoAreNotSuperAdmins` (migration 0007) |
| Not verified | Cookie, Origin and `Set-Cookie` behaviour through Vercel's rewrite on a deployed project; per-IP limiting behind real proxies (TRUSTED_PROXIES) |
