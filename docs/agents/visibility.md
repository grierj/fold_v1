# Visibility, consent and kept dates

Read this before touching who can see an event or a date, making a group or an event more
private, a visibility reset, a date someone kept after leaving, the date page, or a roster on the
event, group or date pages. Background: DEC-48, DEC-156, DEC-157, DEC-158, PLAN section 6.

## Who can see a date

- An event's `visibility` is its own value, copied from the group at creation (DEC-48). A nullable
  occurrence `visibility_override` wins for that date (`policy.EffectiveVisibility`); date
  candidates never authorize the series.
- Visibility is `visibleTo` in `internal/policy`; its SQL forms (`directoryQuery`,
  `losersForDate`) are kept equal by `TestDirectoryMatchesPolicy` and `TestLosersMatchPolicy`.
- Making a group or one event more private previews the cancellations, needs the preview's
  `consent` (`internal/consent`, `visibility_consent.go`), and cancels and tells the people it
  shuts out (`cancelLosers`, `visibility-impact`, `api_group_visibility.go`). Group and
  single-event tightening share `visibility-impact`, `cancelLosers` and `GroupVisibilityReview`.
- An event's visibility save names its read (DEC-163): it sends the event page's `revision`, which
  `checkEventRevision` (`edit_revision.go`) compares under the event's row lock, after the authority
  recheck and before the no-change answer and the consent check. The token covers the group's
  visibility too (it decides leader-opened), read after the contact gate that every group visibility
  write also holds. The control sends it through `useRevisionSave`. A date's own visibility edit
  names its read through the date's revision (the guarded edit, `api_date_edits.go`); the group form
  does not name its read yet (#454).

## Consent (DEC-158)

- A tightening's or reset's consent is the token `internal/consent` computes over the displayed
  impact (the current visibility and the reset dates included) and the locked selections; a
  change that cancels anyone, and every reset request, needs it. Every visibility write runs
  `checkConsentIn` once, before `changeDateVisibilitySelected`. A cancellation runs only from the
  checked plan's `Take`, and `cancelSelectedLosers` verifies the rows against it.
- Every writer of an `event.visibility_*` audit row runs `checkConsentIn` first, group-event
  creation included (`TestEventVisibilityAuditWritersDeclareConsent`). `00015` refuses a
  visibility cancellation without a passed check, any event visibility audit row without the
  consent rule, and an earlier release's start.
- The token is bound to `visibility_consent_epoch`, which only the boot step (`bootRestoreFence`)
  writes; `setSettingIn` refuses it. A token check reads the epoch with `lockConsentEpochIn`
  (`FOR SHARE`), so a boot's rotation waits for a confirmation in flight; previews read it plainly.

## Rules and their tests

| Rule | Enforced by |
|------|-------------|
| Making a group or one event more private previews, confirms, cancels and tells people | `TestGroupMorePrivateTightensAndCancels`, `TestEventMorePrivateCancelsShutOutRegistrants`, `e2e/event-visibility.spec.ts` |
| A tightening cancels only what its reviewer was shown, a reset resets only the dates and saves only the series value its reviewer was shown (even cancelling nobody), and a release without that check never starts on a database this release has run, nor changes who can see an event or a date while still running (DEC-158) | `TestConsentKeyIsStableAcrossAppValues`, `TestVisibilityConsentAcrossServeRestart`, `TestServeBootStepComesFirst`, `TestVisibilityConsentAcrossSecrets`, `TestNoConsentWhereNothingIsReviewRequired`, `TestStaleTabBodiesAreRefused`, `TestVisibilityCancellationsCancelExactlyTheCheckedSet`, `TestVisibilityConsentFenceMigration`, `TestVisibilityConsentFenceInFlight`, `TestPriorReleaseRefusesToStart`, `TestRealServeRestartRotatesTheEpoch`, `TestDatabaseRecoveryFloorIsNotAboveTheBinary`, `TestVisibilityCancellationsConsumeACheckedPlan`, `TestCancellationRefusesRowsOutsideItsPlan`, `TestDateLosersCloneIsDeep`, `TestEventResetIsReviewedWithNoLosers`, `TestEventResetBindsTheCurrentSeries`, `TestDateEditReasonNamedLikeAResetSaves`, `TestVisibilityConsentReadsTheInjectedClock`, `TestConsentEpochHasOneWriter`, `TestRecoveryFloorSurvivesTheWholeChainDown`, `TestRecoveryFloorIsDroppedOnlyByTheTestCleanup`, `TestRecoveryFloorCleanupRefusesForeignDatabases`, `TestPriorProcessRunningAcrossTheUpgrade`, `TestEveryEventVisibilityWriterDeclaresConsent`, `TestEventVisibilityAuditWritersDeclareConsent`, `TestConsentEpochCheckIsOrderedWithTheBoot`, `TestRealServeBootWaitsForAnOpenConfirmation`, `TestConsentEpochIsLockedOnlyWhereATokenIsChecked`, `TestVisibilityConsentLifecycleMatrix`, `TestVisibilityConsentSerializesWithLifecycleWriters`, `TestConsentEpochLockAddsNoDeadlockWithTheGates`, `TestVisibilityConsentAcrossARestore`, `TestVisibilityConsentPreviewMatchesWrite`, `TestVisibilityConsentAuthority`, `TestConsentTokenDefinition`, `TestDateEditConsentRefusalsRunOnce`, `internal/consent` tests |
| An event's visibility save made from an older read is refused with nothing written; the control saves again by itself when the audience is as it opened, and otherwise closes with the panel and any review while the page says the changes weren't saved (DEC-163) | `TestVisibilitySaveNamesTheReadItWasMadeFrom`, `TestStaleWideningAfterAnotherManagersTighteningIsRefused`, `TestConcurrentVisibilitySavesFromOneReadExactlyOneWins`, `TestVisibilityRevisionIsJudgedOnlyForAManager`, `TestVisibilityRevisionNoChangeAndDecision`, `EventVisibility.test.tsx`, `EventDetail.test.tsx`, `useRevisionSave.test.ts`, `web/e2e/visibility-revision.spec.ts` |
| A visibility review and the date question state the current and the new value as their accessible description (`changeSentence`), and Cancel returns focus to the Save button that opened the review | `visibilityChange.test.ts`, `GroupVisibilityReview.test.tsx`, `DateEditor.test.tsx`, `EventVisibility.test.tsx`, `GroupDetail.test.tsx` |
| A date someone kept after leaving never ends later while kept (DEC-156): every writer of a date's end and the date preview refuse it, and the `event_occurrences_kept_date_end` trigger (`00014`) refuses a raise in the database for issuers that lock the date first | `TestLaterEndRefusedWhileADateIsKept`, `TestSeriesEditRefusesMovingAKeptDateLater`, `TestDatePreviewRefusesALaterEndOnAKeptDate`, `TestKeptDateSQLMatchesPolicy`, `TestEndsAtWritersCheckKeptDates`, `TestDepartureGrantsAreIssuedUnderTheDateLock`, `TestKeptDateEndMigration`, `TestKeptDateRaiseBehindTheIssuerIsRefused` |
| The manager's exact-date read carries a roster only for a scheduled date that has not ended, and decides household visibility and ages on its snapshot's one clock and church zone | `TestManagedDateCarriesItsRoster`, `TestManagedDateHouseholdAgeFollowsTheSnapshotClock`, `TestManagedDateRefusesABadChurchZone` |
| A manager's calendar links each date the event page doesn't list to its date page, and keeps a date someone kept after leaving in Everything (not Just mine or the feeds) until Remove or its end, including an underway one that began before the first day shown, which Week and Month show under "Already underway" (DEC-157) | `TestManagerCalendarLinkMatchesTheEventPageList`, `TestManagersReachKeptDatesOnTheirCalendar`, `TestDecideCalendarManagerKept`, `Calendar.test.tsx`, `web/e2e/kept-calendar.spec.ts`, `web/e2e/kept-dates.spec.ts` |
| The date page shows manager content only while its latest answer grants it, and its roster never stays on screen while a refresh after a recheck or a roster write is pending, nor remounts while a roster write on that date is in flight after a recheck; every roster write on the date, the editor's kept-dates panel's included, rereads the managed date fail-closed, so a question asked before it never submits; a draft is held, not shown, while authority is checked | `EventDate.test.tsx`, `ManageDate.test.tsx`, `dateWrites.test.tsx` |
| On the event and group pages, a roster write on a date that may have changed it (any end but "answered": a date row's, or a kept-dates panel's in a date editor or, on the event page, the series editor) holds that date like an update and asks for the page again; a date save started before it never writes (an open question closes as no, the count is read again), and the editor keeps its draft and focus. On every host a roster write that begins on a date closes its open consent question and disables Save until it ends (`useRosterActivity`); the event and group visibility reviews open (from a preview or a 409) and commit only when no matching roster write began, ended or is pending since their mark (`useRosterActivityOf`), with confirm waiting while one is pending | `EventDetail.test.tsx`, `GroupDetail.test.tsx`, `ManageDate.test.tsx`, `dateWrites.test.tsx`, `DateEditor.test.tsx`, `EventVisibility.test.tsx`, `GroupVisibilityReview.test.tsx` |
| A roster's focus or care-boundary recheck, and the refresh after a roster write, never shows the old roster while it waits, and no roster remounts or starts a second write while a roster write on that date is in flight after a recheck. The kept-dates panel shows nothing kept-specific until a fresh read of the selected date answers, and never says a set of dates is clear | `ManageRoster.test.tsx`, `dateWrites.test.tsx`, `KeptDatesPanel.test.tsx`, `SeriesEditor.test.tsx`, `DateEditor.test.tsx`, `EventDetail.test.tsx`, `GroupDetail.test.tsx` |
