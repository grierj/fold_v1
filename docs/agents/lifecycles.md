# Lifecycles: rows that move through states

Read this before adding a status value, a new writer of a status column, a new status-like
column, a row whose state is a nullable timestamp, or a multi-step flow in the web client.

A lifecycle is anything whose next value depends on its current one: a `status` column, a pair
of decision columns, a set of nullable timestamps (`accepted_at`, `revoked_at`), a counter that
only goes up, or a state nobody stores and every reader derives from other rows and the clock.
This file is the map of them as they are built. It names where each move is decided and which
functions write it, so a change starts from the whole machine and not from one handler. The rows
describe today's code, including the lifecycles that do not follow the rule below yet.

## The rule for a new or changed lifecycle

- **Reuse the machine that is there.** A new move goes through the writers its row names. A
  writer owns the move's side effects (audit row, notice, session bump, token revoke, seat
  promotion), so a path that goes around it skips them.
- **One place decides a move.** "May this row go from A to B" is a pure function in
  `internal/policy` with a table test, or the `WHERE` of the one conditional `UPDATE`
  (`decide_pending.go:decidePendingIn`). New code in a handler loads facts and asks; it does not
  compare statuses itself.
- **A new status column** gets named constants (values owned by `internal/policy`, aliased by
  `models`), a CHECK that lists its values, one writer, and a row here in the same PR.
- **Locking and the order of side effects stay per machine.** The contact gate, the row locks and
  the conditional updates in the rows below are each there for a reason its code states; a shared
  rule does not replace them. There is no generic state-machine package.
- **A state the clock decides** (expired, ended, lapsed) is computed from one `now`, taken once
  per transaction. Its row says which sweep stamps it, or that nothing does.
- **Making a move stricter is a product decision.** Writing a machine down records what the code
  allows today, odd moves included. Refusing one of them is a separate change (AGENTS.md, merge
  rule 3).
- **Web:** one value per lifecycle, a union that cannot hold two states at once, over several
  booleans. A page that loads one resource uses `web/src/lib/useLoad.ts`; a form or button that
  changes something runs its submit through `web/src/lib/useAction.ts`. A server status is
  typed as a union in `web/src/types.ts` and gets its words from `web/src/lib/status.ts`.

## Accounts and identity

| Lifecycle | States | Where a move is decided | Writers | Enforced by |
|---|---|---|---|---|
| `users.status` | `active` `invited` `guest` `pending_consent` `disabled` | New rows: `account_status.go:newAccountStatus`. Changes: inline in `api_admin.go:apiAdminUpdateUser`, `api_teen_consent.go:changeTeenConsent`, `api_teen_conversion.go:completeTeenConversionIfReady` and `age_rollover.go:rolloverTeen` | `account_status.go:setAccountStatusIn` (every change: the session bump, seats, token revoke; the caller writes the audit row), `account_status.go:upgradeGuestIn` (a guest becomes an account); in their own statement only `account_security.go:replacePasswordIn` (`account_status.go:invitedBecomesActive`, with the password) and the erasure, `account_erasure.go:tombstoneUserIn` and `account_erasure_tombstones.go:scrubTombstoneRowIn` | `users_status_check`, `TestAccountStatusHasOneWriter`, `TestAdminEditCannotBypassTeenConsent`, `TestAgeRolloverStatusesAndConsentHistory`, `TestAgeRolloverStatusChangesEndSessions`, `TestTeenConversionStatusMovesEndSessionsAndAreAudited` |
| `users.account_type` | `adult` `teen` | `age_rollover.go:rolloverTeen` (18 on the church calendar); a guest row takes its type in `account_status.go:upgradeGuestIn` | the same two | `users_account_type_check` |
| `household_accounts.relation` | `guardian` `adult` `teen` | teen to adult with the account, in `age_rollover.go:rolloverTeen` | `age_rollover.go:rolloverTeen` | `household_accounts_relation_check` |
| `household_people.kind` | `child` `teen` `adult` | `api_households.go:apiUpdateHouseholdPerson`; a staged teen turns adult in `age_rollover.go:rolloverTeen` | the same two | `household_people_kind_check`, `TestHouseholdTablesConstraints` |
| `users.role` (no CHECK) | `member` `admin` | `internal/policy/policy.go:CanDemoteOrDelete` | `api_admin.go:apiAdminUpdateUser` | nothing in the schema |
| `account_requests.status` | `pending` `approved` `rejected` `expired` `withdrawn` | `WHERE status = 'pending'` in each writer; a decision's UPDATE also needs `expires_at` still ahead (`decide_pending.go:decidePendingIn` with `stillLive`), so a request that lapses between the read and the write answers expired, and the sweep stamps it | `api_account_requests.go:decideAccountRequest` (through `decide_pending.go:decidePendingIn`), `api_account_requests.go:expirePendingRequests`, `account_erasure.go:eraseAccountRequestsIn` | `account_requests_status_check`, `TestDecisionRefusesRequestThatExpiresBeforeTheWrite` |
| `guardian_links.status` | `pending` `confirmed` `rejected` `revoked` | `WHERE` guards in each writer | made by `api_households.go:createGuardianLink`, `api_teen_guardians.go:requestTeenGuardian`; decided by `api_admin_guardian_links.go:decideGuardianLink`; ended by `api_households.go:revokeGuardianLinks`, `age_rollover.go:rolloverTeen`, `account_erasure_households.go:eraseHouseholdRecordsIn` | `guardian_links_status_check`, `TestAgeRolloverClosesOnlyPendingGuardianTasks` |
| `teen_consents.status` | `granted` `revoked` | Append only: the latest row per teen and scope is the state. `api_teen_consent.go:changeTeenConsent`; on the admin panel and the guardian card a grant names the read it was made from (`api_teen_consent.go:teenConsentRefusal` first, then `edit_revision.go:checkConsentRevision`, DEC-176) | `api_teen_consent.go:changeTeenConsent` | `teen_consents_status_check`, trigger `teen_consents_append_only`, `TestTeenConsentAppendOnly` |
| Invitation (`accepted_at`, `revoked_at`, `expires_at`) | open, accepted, revoked, expired by the clock, deleted | the open predicate in `tokens.go:claimInvitation` and in each writer's `WHERE` | `tokens.go:claimInvitation`, `api_household_conversion.go:revokeConversionInvitation`, `api_household_conversion.go:issueHouseholdConversion`, `link_cleanup.go:pruneExpiredLinks` (PLAN 10.1) | unique open conversion index |
| Account token (`used_at`, `expires_at`) | live, used, expired by the clock, deleted | `tokens.go:claimAccountToken` | `tokens.go:storeAccountToken`, `tokens.go:claimAccountToken`, `tokens.go:revokeAccountTokens` (a revoke is a delete), `link_cleanup.go:pruneExpiredLinks` (PLAN 10.1) | `account_tokens_kind_check`, unique token hash |
| Email change (two tokens, no column) | none, pending, verified, cancelled | which unused tokens exist | `api_profile.go:apiRequestEmailChange`, `api_profile.go:apiVerifyEmailChange`, `api_email_change_cancel.go:apiCancelEmailChange` | `TestEmailChangeVerifyAndCancelHaveOnlyOneWinner` |
| Household conversion (three tables, no column) | none, invited, requested, staged (teen), converted | inline, with `internal/policy/policy.go:CanConvertHouseholdPerson` | `api_household_conversion.go:issueHouseholdConversion`, `api_household_conversion.go:acceptHouseholdConversion`, `api_household_conversion.go:approveHouseholdConversion`, `api_teen_conversion.go:completeTeenConversionIfReady` | `TestHouseholdConversionInvitationConstraints` |
| Session epochs (`session_version`, `logout_version`) | counters that only go up | read only by `session_state.go:sessionCurrent` | `session_state.go:sessionVersionBump`, `session_state.go:endAllSessionsIn`, `api_auth.go:endSessionsAtLogout` | `TestSessionEpochsAreComparedOnlyBySessionCurrent` |
| Sign-in lockout (`login_throttles`) | absent, counting, locked until a time | SQL in `throttle.go:reserveLoginAttempt` | `throttle.go:reserveLoginAttempt`, `throttle.go:clearLoginKey`, `throttle.go:pruneLoginThrottles` | `TestThrottleSQLStaysInItsHelpers` |
| Account erasure | live, erased; an older tombstone is swept to the current version | `account_erasure.go:eraseAccountIn` (`docs/agents/erasure.md`) | `account_erasure.go:eraseAccountIn`, `account_erasure_tombstones.go:eraseTombstoneOnce` | `TestAuditErasureWriterCensus` |
| Erased reason item (derived from audit rows) | live, unreviewed tombstone, reviewed tombstone | `internal/policy/policy.go:ErasureReasonItemState` | `account_erasure_reasons.go:eraseContactRequestReasonsIn` | `TestErasureReasonItemState` |
| Admin operation attempt (in memory, plus a receipt row) | absent, running, applied, rolled back, unknown, fenced | methods of `admin_operation_attempts.go:attemptLedger` | the same | `TestAttemptLedgerRules` |
| Restore fence (`settings.restore_fence`) | a counter, a floor, a detected latch | `restore_fence.go:takeRestoreTicket` | `restore_fence.go:bootRestoreFence`, `restore_fence.go:takeRestoreTicket` | `TestRestoreFenceHasTwoWriters` |
| A claim on an email address (derived, no table) | free, existing account, pending conversion, pending invitation | `email_holds.go:emailHeldBy`, computed from users, account requests and invitations | none | advisory lock `email_holds.go:lockInvitationEmail` |
| A profile photo (`users.avatar_key`, files, a cleanup job) | none, current; old files wait for cleanup | `avatar_cleanup.go:cleanAvatarAccount` decides what a folder keeps | `avatar_cleanup.go:setAvatarKey`, `avatar_cleanup.go:queueAvatarCleanup`, `avatar_cleanup.go:cleanAvatarAccount` | `TestAvatarCleanupTombstoneKeepsNothing` |
| First-run setup (no column) | not set up, set up | a count of users in `setup.go:createFirstAdmin` | `setup.go:createFirstAdmin` | `TestConcurrentSetupCreatesOneAdmin` |

## Groups, events and rooms

| Lifecycle | States | Where a move is decided | Writers | Enforced by |
|---|---|---|---|---|
| `groups.status` | `pending` `active` `archived` | New groups: `internal/policy/policy.go:NewGroupStatus`. Edits: `internal/policy/policy.go:DecideGroupStatus` judges the move from the locked status (nothing goes back to pending). The approval queue: `decide_pending.go:decidePendingIn` (pending only, the approve and reject moves) | `api_groups.go:apiCreateGroup`, `api_admin_groups.go:decideGroup`, `api_groups.go:apiUpdateGroup` | `groups_status_check`, `TestDecideGroupStatus`, `TestUpdateGroupNeverReturnsToPending`, `TestUpdateGroupBackToPendingIsJudgedOnTheLockedStatus`, `TestUpdateGroupArchiveAndUnarchiveAreAudited`, `TestApprovalQueueAgreesWithDecideGroupStatus`, `TestGroupApprovalFlow` |
| Group deletion request (`deletion_requested`, `deleted_at`) | not asked, asked, deleted | `internal/policy/policy.go:CanRequestDeletion` | `api_groups.go:apiRequestDeletion`, `api_groups.go:clearDeletionRequestIn`, `api_groups.go:deleteGroup` | `TestDeletionRequestTask` |
| `memberships.status` | `active` `removed` `left` | `internal/policy/policy.go:CanJoinGroup`, `internal/policy/policy.go:CanLeaveGroup`, `internal/policy/policy.go:CanRemoveMember`; a disabled account's memberships stay `active` and are hidden by the join on `users.status` | `api_groups.go:changeRoleIn`, `api_departure.go:applyDeparture`, `api_handoff.go:handOffLeadership`, `api_handoff.go:restoreMemberships` | `memberships_status_check`, trigger `memberships_tenure_guard`, `TestMembershipTenureGuard` |
| `memberships.role` | `leader` `member` | `internal/policy/policy.go:CanChangeRole` (a group keeps a leader), `internal/policy/policy.go:CanBeSuccessor` | `api_groups.go:changeRoleIn`, `api_departure.go:applyDeparture`, `api_handoff.go:handOffLeadership`, `api_handoff.go:demoteEverywhere`, `api_handoff.go:restoreMemberships` | `memberships_role_check`, `TestCanChangeRole`, `TestSoleLeaderHandoff` |
| `event_occurrences.status` | `scheduled` `cancelled` | Series edits: `internal/events/series.go:ApplySeries`. One date: `api_events.go:apiCancelOccurrence` judges the end under the date lock. Manual and series cancellations use `api_events.go:auditDateCancellation`. No writer schedules a manually cancelled date again | `internal/eventdates/extend.go:Ensure`, `api_events.go:apiUpdateEvent`, `api_events.go:apiCancelOccurrence` | `event_occurrences_status_check`, `TestOccurrenceCancelSource` |
| `event_occurrences.cancel_source` | `''` `series` `manual` | with the status above; `series` can be scheduled again by a later series edit (DEC-91) | the same writers | `event_occurrences_cancel_source_check`, `TestSeriesEditRestoresSeriesCancelledDates` |
| `groups.visibility` | `public` `private` `invite_only` | Wider is free. Narrower previews, needs consent and cancels (`docs/agents/visibility.md`): `visibility_consent.go:checkConsentIn` | `api_groups.go:apiUpdateGroup` through `api_group_visibility.go:applyGroupTightening` | `groups_visibility_check`, `TestGroupMorePrivateTightensAndCancels` |
| `events.visibility` | `public` `private` `invite_only` | the same, with `internal/policy/policy.go:CanSetEventVisibility` | `api_event_access.go:apiSetEventVisibility`, `api_group_visibility.go:applyGroupTightening` | `events_visibility_check`, `TestEveryEventVisibilityWriterDeclaresConsent` |
| `event_occurrences.visibility_override` | `public` `private` `invite_only` | the same; null means the event's value (`internal/policy/policy.go:EffectiveVisibility`) | `api_occurrence_visibility.go:changeDateVisibilitySelected` | `event_occurrences_visibility_override_check`, `TestVisibilityConsentLifecycleMatrix` |
| `events.leader_opened` (a flag beside visibility) | false, true: the event was opened wider than its group on purpose | set from `internal/policy/policy.go:MoreOpen` when the event is made or its visibility set; a group tightening asks before narrowing an opened event | `api_events.go:apiCreateEvent`, `api_event_access.go:apiSetEventVisibility`, `api_group_visibility.go:applyGroupTightening` | `TestLeaderOpenedEventsAskBeforeTightening`; the CHECK `events_churchwide_check` forbids the flag on a church-wide event |
| A visibility consent (a plan per transaction, `settings.visibility_consent_epoch`) | an entry untaken, taken; the plan done; the epoch replaced at each boot | `internal/consent/consent.go:Check`; `visibility_consent.go:checkConsentIn` runs once per write | `visibility_consent.go:checkConsentIn`; the epoch only by `restore_fence.go:bootRestoreFence` | trigger `audit_logs_visibility_consent`, `TestConsentEpochHasOneWriter`, `TestVisibilityCancellationsConsumeACheckedPlan` |
| A date's phase (no column) | upcoming, underway, ended | each reader compares `starts_at` and `ends_at` with its own `now`; an end time cannot rise while someone keeps the date (`api_date_edits.go:refuseKeptDates`) | none; nothing stamps an ended date | trigger `event_occurrences_kept_date_end`, `TestEndsAtWritersCheckKeptDates` |
| Edit revisions (`edit_revision` on events and dates; a group's, a date's booking's, a message's, a room's, a household's and a household person's are digests of their rows, `edit_revision.go:rowRevisionIn`) | a counter; for a group or a booking, the row's digest | database triggers (migration 00010) for the counters; the digests are computed on each read and save, never stored; `api_date_edits.go:editDateIn` refuses a date edit whose base is not the date's current token (DEC-164); `edit_revision.go:checkEventRevision` refuses a series save or an event visibility save whose revision is not the event's current token (DEC-162, DEC-163), `edit_revision.go:checkGroupRevision` a group form save whose revision is not the group's (DEC-166), `edit_revision.go:checkDateRoomRevision` a pick of a date's room whose revision is not its booking's, and `checkEventRevision` a series room pick (DEC-167), `edit_revision.go:checkMessageRevision` a message's edit or send whose revision is not the message's (DEC-169), `edit_revision.go:checkRoomRevision` an admin's room edit whose revision is not the room's (DEC-171), `edit_revision.go:checkHouseholdRevision` and `checkHouseholdPersonRevision` a household's details save or a person's edit whose revision is not that row's (DEC-174), `edit_revision.go:checkConsentRevision` an admin's or a guardian's grant of teen consent whose revision is not the teen's consent log's (its highest row in every scope, sealed; a revoke is not bound, DEC-176), and the web client's reader is `web/src/lib/useRevisionSave.ts` | the triggers; nothing writes a digest | `TestDateEditRevisionsMigration`, `TestGroupRevisionMovesWithEveryWrite`, `TestRowRevisionCoversTheWholeRow`, `TestEmptyDateTokenIsScopedToTheDate`, `TestDatePickFromAnOlderReadIsRefused`, `TestRoomPickByAnAdminRevokedWhileItWaitsWritesNothing`, `TestSeriesPickFromAnOlderReadIsRefused`, `TestGroupSaveResentAfterItWasUndoneIsRefused`, `TestSeriesSaveNamesTheReadItWasMadeFrom`, `TestSeriesRevisionFollowsTheEventsOtherWriters`, `TestVisibilitySaveNamesTheReadItWasMadeFrom`, `TestGroupAudienceChangeMakesTheEventTokenStale`, `TestGroupSaveNamesTheReadItWasMadeFrom`, `TestMessageEditNamesItsRead`, `TestMessageSendNamesItsText`, `TestRoomEditNamesItsRead`, `TestRoomRevisionCoversTheWholeRow`, `TestRoomEditByAnAdminRevokedWhileItWaitsWritesNothing`, `TestHouseholdPersonEditNamesItsRead`, `TestHouseholdDetailsSaveNamesItsRead`, `TestHouseholdRevisionsFollowTheirRows`, `TestAdminGrantFromAReadOlderThanAGuardiansRevokeIsRefused`, `TestGuardianGrantFromAReadOlderThanAnAdminsRevokeIsRefused`, `TestConsentRefusalsComeBeforeTheStaleRead`, `TestConsentReadsTakeTheirTokenInTheirRowsSnapshot`, `TestSavesCheckTheirRevisionInTheirTransaction` |
| Series clock repair (`events.series_clock_dirty`) | clean, dirty | a trigger sets it (migration 00009) | cleared by `internal/eventdates/extend.go:ClearDirty` | `TestOccurrenceSeriesStartsMigration` |
| Church-wide event hosts (`event_hosts`) | at least one live host | `internal/policy/policy.go:EventKeepsHost` | `api_event_access.go:apiAddEventHost`, `api_event_access.go:apiRemoveEventHost`, `api_host_handoff.go:handOffHosting` | `TestEventKeepsHost` |
| `room_bookings.status` | `confirmed` `requested` `rejected`, read as five states: confirmed, double-booked (confirmed with an allowed overlap), waiting (requested, its request pending), lost (requested, no pending request) and rejected | The state is `internal/policy/room_lifecycle.go:RoomBookingStateOf`, from the status, whether its request is pending and `allow_overlap`. The moves are data: `internal/policy/room_lifecycle.go:RoomBookingMove`, each with the states it starts from and where it ends. A change's `WHERE` carries the move's statuses; which of a status's states it starts from (waiting, not lost) is the caller's rows and locks (the event's, then `api_room_requests.go:lockRoomDecisionIn`). A waiting booking also turns lost with no write to it, when its request is withdrawn or deleted (the foreign key clears `request_id`). A date that moves keeps its state; only a confirmed one can lose its room | every change of a row: `room_status.go:changeBookings`, called by `api_room_bookings.go:placeBooking`, `api_room_bookings.go:moveRoomBookings`, `api_room_requests.go:apiConfirmRoomRequest`, `api_room_requests.go:apiRejectRoomRequest`. Inserts: `api_room_bookings.go:placeBooking`. Deletes (release): `api_room_bookings.go:releaseRoomBookings`, `api_room_bookings.go:pickDateRoom`, `api_room_series.go:bookWholeSeries` | `room_bookings_status_check`, exclusion `room_bookings_no_overlap`, `TestRoomBookingStateOf`, `TestRoomBookingMoves`, `TestChangeBookingsSQLMatchesTheRule`, `TestRoomBookingStateSQLMatchesGo`, `TestRoomStatusHasOneWriter`, `TestRoomBookingConcurrentExactlyOneWins` |
| `room_requests.status` | `pending` `confirmed` `rejected` `withdrawn` | `internal/policy/room_lifecycle.go:RoomRequestMove`: every move of a row starts from pending (`internal/policy/room_lifecycle.go:RoomRequestUndecided`, which `api_room_requests.go:lockRoomDecisionIn` asks). `withdrawn` only as a side effect of `api_room_requests.go:closeEmptyRoomRequests`, or of a new series pick of another room replacing the event's one pending series request (`api_room_requests.go:withdrawSeriesRequest`, called by `api_room_series.go:bookWholeSeries`); a leader's or host's pick of the room that request is for keeps it, and the dates it sends join it (DEC-173). A request no date ever waited on is deleted in the transaction that opened it | `room_status.go:moveRoomRequest` (a decision: `api_room_requests.go:apiConfirmRoomRequest`, `api_room_requests.go:apiRejectRoomRequest`), `room_status.go:moveRoomRequestsWhere` (the two withdrawals); inserted pending by `api_room_requests.go:openRoomRequest`; deleted by `api_room_series.go:bookWholeSeries` and `api_room_series.go:bookSeriesRoom` | `room_requests_status_check`, `idx_room_requests_one_series`, `TestRoomRequestMoves`, `TestRoomRequestWritersMatchTheRule`, `TestRoomStatusHasOneWriter`, `TestRoomApprovalSeries`, `TestRoomApprovalSeriesRepickWhilePending` |
| Series room (`default_room_id`, `default_room_pending`) | none, approved, awaiting approval | inline in `api_room_series.go:bookWholeSeries`; what a change does to each date's booking is `internal/policy/series_room.go:SeriesRoomChange` (DEC-168: a date with a room of its own, booked or waiting, keeps it unless the pick names it in `includeDates` and its new room is free then; the control's list is `SeriesDateBooking.Listed`; a leader's pick that would replace the series request a kept date waits on is refused, and one of that request's room keeps it, leaving its waiting dates to it, `SeriesDateBooking.OnKeptRequest`); its new dates are booked by `api_room_series.go:bookSeriesRoom`, which asks for approval into the event's pending series request (or a new one) while the room awaits approval and approval is on, and touches no date that has a booking | `api_room_series.go:bookWholeSeries`, `api_room_requests.go:apiConfirmRoomRequest`, `api_room_requests.go:apiRejectRoomRequest`, `api_room_locations.go:apiAdminMatchLocation` | `TestRoomApprovalSeriesRoomStaysUnapproved`, `TestSeriesRoomChange`, `TestSeriesRoomListIsWhatAChangeKeeps`, `TestSeriesPickLeavesDatesWithTheirOwnRoom`, `TestSeriesPickChangesOnlyTheNamedDates`, `TestSeriesPickIncludesAnAdminOnlyBookingOnlyForAnAdmin`, `TestStoppingTheSeriesRoomKeepsOwnRoomsUnlessNamed`, `TestSeriesPickAnswersWithTheDatesAsTheyAreNow`, `TestSeriesPickKeepsARequestOnlyAnAdminCanChange`, `TestSeriesPickKeepsANamedDateWhoseNewRoomIsInUse`, `TestSeriesPickNamingAnotherEventsDateLeavesItAlone`, `TestSeriesRepickOfTheWaitingRoomKeepsItsRequest`, `TestSeriesPickOfTheKeptRequestsRoomGoesAhead` |
| A calendar link (`calendar_tokens`, a row or none) | none, live; a new link replaces the old one, and turning it off deletes the row | inline | `api_feeds.go:apiCreateMyCalendarFeed`, `api_feeds.go:apiUpdateMyCalendarFeed` (scope), `api_feeds.go:revokeCalendarToken` | `TestPersonalFeedReadsItsLinkInTheFeedsSnapshot` |
| A room's feed link (`room_feed_tokens.revoked_at`) | none, live, revoked; a new link revokes the old one | the `WHERE revoked_at IS NULL` in each writer | `api_room_feeds.go:apiAdminCreateRoomFeed`, `api_room_feeds.go:apiAdminRevokeRoomFeed` | `TestRoomFeedReadsItsLinkAndSwitchInOneSnapshot` |

## Registrations and requests

| Lifecycle | States | Where a move is decided | Writers | Enforced by |
|---|---|---|---|---|
| `registrations.status` | `going` `maybe` `declined` `cancelled` `waitlisted` `removed` | `internal/policy/registration_lifecycle.go:RegistrationMove`: each cause (an answer, a series undo, a guest's confirm and link cancel, promotion, a manager's add and removal, leaving or removal from a group, a privacy cancel, a household person changed) names the statuses it starts from, none where it makes the row, and those it may end in or pass through; where a move may end going or waitlisted, `registration_seats.go:seatChange` picks. A disabled holder's row keeps its status and stops counting (`registration_subjects.go:listedRegistrations`). Three statements delete rows instead of moving them: `api_registrations.go:restoreRegistrations` (a re-enabled account's answer no longer fits or is no longer allowed), `api_series_registrations.go:apiUndoSeriesRegistration` (the undone series answer made the row) and `api_events.go:deleteEventRows` (with its event or group) | The status: `registration_rows.go:saveRegistration` (`registration_rows.go:saveRegistrationStep` for a batch's seat release), each checking the caller's move, and `registration_rows.go:promoteRegistration`, whose `WHERE` is the promotion's from-set (`registration_rows.go:registrationStartsFrom`). Other columns only: `registration_access.go:grantRegistrationAccess` and `registration_access.go:endAccess` (access), `api_household_conversion.go:transferHouseholdConversion` (identity), `api_occurrence_mutations.go:recordManagerOccurrenceAdd` (manage token), `account_erasure.go:eraseActivityRowsIn` and `account_erasure_households.go:eraseHouseholdRecordsIn` (erasure scrub) | `registrations_status_check`, `TestRegistrationStatusVocabulary`, `TestRegistrationMoves`, `TestRegistrationStatusesMatchTheSchema`, `TestSaveRegistrationRefusesMovesTheRuleDoesNotHold`, `TestRegistrationStartsFromSQLMatchesTheRule`, `TestPromoteRegistrationSQLMatchesTheRule`, `TestRegistrationStatusHasTwoWriters`, `TestSeatSQLMatchesGoRules`, `TestConcurrentGoingRespectsCapacity` |
| Date access on a registration (`access_origin`, `access_ended_at`, `access_end_reason`) | none, active, ended | trigger `registration_access_provenance`; `internal/policy/occurrence_access.go:OccurrenceAccessCurrent`. A kept date that has ended stays unstamped until a reconcile or the daily sweep | `registration_access.go:grantRegistrationAccess`, `registration_access.go:endAccess` | `registrations_access_end_state`, `TestHardDeletePathsEndLiveGrantsFirst`, `TestKeptDateSQLMatchesPolicy` |
| A guest's manage link (`registrations.manage_token_hash`) | none, live; replaced on promotion | cleared by status in `registration_rows.go:saveRegistration`; judged live at read in `api_guest.go:liveTokenManage` | `api_guest.go:apiConfirmGuestRegistration`, `registration_rows.go:saveRegistration`, `registration_rows.go:promoteRegistration`, `api_occurrence_mutations.go:recordManagerOccurrenceAdd` | `TestManageTokenIndex`, `TestGuestManageReauthorizesCancelledLinks` |
| `contact_requests.status` | `pending` `granted` `denied` `withdrawn` `expired` `ended` | `internal/policy/contact.go:ContactRequestStatusAfterDecision`, `internal/policy/occurrence_access.go:AddRequestStatusAfterDecision`, `internal/policy/contact.go:ContactRequestExpired`; what a purpose decides is `request_kinds.go:requestKind` | `request_lifecycle.go:openRequest`, `request_lifecycle.go:recordDecision`, `request_lifecycle.go:withdrawRequest`, `contact_lifecycle.go:finishContactRequest`, `api_contact.go:completeGroupAsk`, `api_occurrence_mutations.go:completeAddAsk` | `contact_requests_status_check`, `contact_requests_outcome`, trigger `add_request_identity` (add purposes), `TestContactRequestsConstraints`, `TestContactTerminalWinsApproval`, trigger `contact_request_terminal`, `TestContactFinalityMigration` |
| `contact_requests.subject_decision` | `pending` `approved` `denied` | `internal/policy/contact.go:ContactRequestStatusAfterDecision` (a party decides once) | `api_contact.go:setRequestDecision` | `contact_requests_subject_decision_check`, `TestContactParallelApprovalsOneGrant` |
| `contact_requests.admin_decision` | `pending` `approved` `denied` `not_needed` | the same; `not_needed` is fixed when the request opens | `request_lifecycle.go:openRequest`, `api_contact.go:setRequestDecision` | `contact_requests_admin_decision_check` |
| Contact grant (`ended_at`, `end_reason`) | live, ended | `internal/policy/contact.go:ContactGrantRevocationReason`, `internal/policy/contact.go:ContactIneligibility` | `api_contact.go:apiDecideContactRequest`, `contact_lifecycle.go:endContactGrant` | `contact_grants_check`, trigger `contact_grant_terminal`, `TestContactFinalityMigration`, `TestContactRestoredRoleCannotResurrect` |
| Manager-add block (`manager_add_blocks`, a row or none) | absent, present | `internal/policy/occurrence_access.go:CanBlockManagerAdds`; only erasure removes one | `api_occurrence_mutations.go:mutateOwnOccurrence` | unique key |
| Guest signup (`confirmed_at`, `expires_at`) | pending, claimed, expired by the clock, deleted | the `WHERE` in `api_guest.go:claimGuestSignup` | `api_guest.go:apiCreateGuestRegistration`, `api_guest.go:claimGuestSignup`, `link_cleanup.go:pruneExpiredLinks` (PLAN 10.1) | `TestGuestRedeemOnFullEventWithoutWaitlistCreatesNothing` |
| Series undo receipt (`used_at`, `expires_at`) | live, used, expired by the clock | inline in `api_series_registrations.go:apiUndoSeriesRegistration` | `api_series_registrations.go:apiRegisterSeries`, `api_series_registrations.go:apiUndoSeriesRegistration` | `TestSeriesUndoReceiptConstraints` |

## Messaging, notifications and operations

| Lifecycle | States | Where a move is decided | Writers | Enforced by |
|---|---|---|---|---|
| `jobs.status` | `queued` `running` `done` `failed` `dead` `dismissed` | `internal/jobs`, model-checked (`.cursor/skills/verify-job-queue`) | `internal/jobs/jobs.go:RunOnce`, `internal/jobs/jobs.go:LaterIn`, `internal/jobs/dead.go:Retry`, `internal/jobs/dead.go:Dismiss` | `jobs_status_check`, the TLA+ model, `TestTwoWorkersNeverRunAJobTwice` |
| `messages.status` | `draft` `scheduled` `sending` `sent` `cancelled` `failed` | the move table in `internal/policy/message_lifecycle.go:MessageMove`; a retry reopens `sent` and `failed`, an email reminder is created `scheduled` and a push reminder `sent` | changes: `message_status.go:moveMessage`, `message_status.go:moveMessagesWhere` (the author's erasure); new rows: `api_messages.go:apiCreateMessage`, `reminders.go:writeReminder`, `reminders_push.go:writePushReminder` | `messages_status_check`, `TestMessageMoves`, `TestMessageStatusesMatchTheSchema`, `TestMoveMessageSQLMatchesTheRule`, `TestMoveMessagesWhereSQLMatchesTheRule`, `TestMessageStatusHasOneWriter`, `TestMessageDraftScheduleCancel`, `TestMessagePipelineHonorsCancel` |
| `message_recipients.status` | `queued` `suppressed` `deferred` `sent` `delivered` `bounced` `complained` `failed` | Email and push share the column, each with its own machine: the move table in `internal/policy/recipient_lifecycle.go:RecipientMove`, which names each move's channels. Email: queued to sent, suppressed or failed at the send (`messages_send.go:markRecipient`), or failed when a batch gives up or the reaper stops it; sent to delivered, and sent or delivered to bounced or complained, by a provider report (`api_email_webhooks.go:applyEmailEvent`); failed back to queued or suppressed by Try again (`api_message_history.go:apiRetryMessage`). Push: queued to suppressed or failed, and anything but sent to sent, by the recorder (`messages_push.go:messagePushRecorder`, through `internal/policy/recipient_lifecycle.go:RecipientPushOutcome`); back to queued to wait for quiet hours, from a device's miss only for a person still active (`messages_push.go:pushWaitIn`); suppressed with no device or by the reaper. The erasure suppresses queued rows of both. No move reaches `deferred` | changes: `recipient_status.go:moveRecipient`, `recipient_status.go:moveRecipientsWhere`; in its own statement, its statuses from the rule, only `account_erasure.go:eraseMessagesIn` (it also scrubs rows it does not move, under one timeout); new rows: `messages_send.go:prepareMessageOnce`, `reminders_push.go:writePushReminderPlans` | `message_recipients_status_check`, `TestRecipientMoves`, `TestRecipientStatusesMatchTheSchema`, `TestRecipientPushOutcomesMatchDeliveryStatuses`, `TestMoveRecipientSQLMatchesTheRule`, `TestMoveRecipientsWhereSQLMatchesTheRule`, `TestRecipientStatusHasOneWriter`, `TestPostmarkReportsUpdateDeliveriesAndSuppress`, `TestRetryNeverPushesAgain` |
| `notification_deliveries.status` | `queued` `suppressed` `deferred` `sent` `delivered` `bounced` `failed` | Email: `notify_deliver.go:updateEmailDelivery`, then `api_email_webhooks.go:applyEmailEvent`. Push: `push_deliver.go:recordPushIn` | those three, `internal/notify/notify.go:Send` | `notification_deliveries_status_check`, `TestNotifyDeliverSendsOnce` |
| A notification (`read_at`, `archived_at`, `resolved_at`, `expires_at`) | unread or read; live or archived; a task open or resolved | `internal/notify` owns every write | `internal/notify/notify.go:Send`, `internal/notify/notify.go:SetRead`, `internal/notify/notify.go:SetArchived`, `internal/notify/notify.go:Resolve`, `internal/notify/notify.go:Prune` | `TestStatesBadgeAndReadAll`, `TestStoredNotificationsAcceptance` |
| A push device (`push_subscriptions`, a row or none) | absent, bound, failing, expired by the clock | `api_push.go:apiPushSubscribe`; `binding_generation` moves only in a trigger (migration 00011); a session end deletes devices in trigger `push_devices_end_with_sessions` (`docs/agents/push.md`) | `api_push.go:apiPushSubscribe`, `api_push.go:apiPushUnsubscribe`, `push_deliver.go:markPushFailed`, `push_deliver.go:prunePushDevices` | `TestPushBindingGenerationMigration`, `TestPushDevicesEndWithSessions` |
| Push keys (`settings`, the VAPID pair) | absent, present, unreadable after a secret change | `push_keys.go:ensureVAPIDKeys` at boot, `push_keys.go:rotateVAPIDKeys` from the CLI; a new pair deletes every device | `push_keys.go:writeVAPIDKeys` | `TestVAPIDKeysAreMadeOnceOnBoot` |
| Set-once and count-up columns | `jobs.claim_seq` (a new number per claim), `notifications.batch_count` (up on a merge), `message_recipients.audience_admitted_at` (set once) | each in its one writer | `internal/jobs/jobs.go:RunOnce`, `internal/notify/notify.go:merge`, `messages_send.go:markRecipient`, `messages_push.go:markPushAdmitted` | `TestJobClaimSeqMigration`, `TestMessageAudienceAdmittedMigration` |
| `consents.status` | `opted_in` `opted_out` | Append only: the latest row per person, channel and scope is the state; with no row, the caller's default | `email_unsubscribe.go:setConsent` | `consents_status_check`, trigger `consents_append_only`, `TestConsentTriggerAllowsOnlyTheErasureScrub` |
| An address's own opt-out and suppression | present or absent; open or cleared | `email_optouts.go:readEmailOff`; `docs/agents/mail.md` | `email_optouts.go:exportEmailOptOutsIn`, `email_optouts.go:clearOptOutOnTurnOn`, `api_email_webhooks.go:applyEmailEvent`, `api_admin_suppressions.go:apiAdminClearSuppression` | `TestEmailChoiceSQLStaysInOneHelper`, `TestSuppressionSQLStaysInItsHelpers` |
| A reminder sent once | unwritten, written | unique keys built by `reminder_key.go:reminderKeySQL` in SQL and by the `String` method of `reminder_key.go:reminderKey` in Go | `reminders.go:writeReminder`, `reminders_push.go:writePushReminder` | `TestReminderSweepQueuesOncePerDate` |
| Backup status (derived from files) | unwatched, missing, unreadable, stale, ok | `backup_status.go:readBackupStatus` | none | `TestReadBackupStatusStates` |

## Enumerated columns that are not lifecycles

These have a CHECK that lists their values, and no value depends on the one before: each is
chosen when the row is made, or is a setting that may change to any value its own rules allow.

`account_requests.account_type`, `account_requests.source`, `account_tokens.kind`,
`calendar_tokens.scope`, `consents.channel`, `consents.source`, `contact_requests.context_type`,
`email_address_optouts.scope`, `events.video_provider`, `events.video_reveal`,
`invitations.account_type`, `invitations.role`, `manager_add_blocks.organizer_type`,
`message_recipients.channel`, `messages.category`, `messages.channel`,
`notification_deliveries.channel`, `notifications.category`, `registrations.attendance_mode`,
`registrations.source`, `suppressions.channel`, `suppressions.reason`,
`teen_consents.granter_role`, `teen_consents.scope`.

## Web client

- The shapes to copy, each one value that names the state: `web/src/lib/useLoad.ts` (a load),
  `web/src/components/DeleteUserPanel.tsx` and `web/src/pages/ReasonReviewLegacy.tsx` (`Phase`, a
  delete and its review) and `web/src/lib/pushState.ts` (`DeviceState`, `PushCardState`).
- The group, series, event visibility and single-date forms use `ManagerSaveStatus` to
  announce “Saving…” and show and announce “Updated” when the save succeeds (DEC-163).
- A manager's save that names the read it was made from (the series editor, an event's
  visibility, one date's edit, the group form, the two room controls, the message composer, the admin room editor
  and its Deactivate and Reactivate, the household's details and person forms, a teen consent change on the admin
  panel and the guardian card; DEC-162 to DEC-164, DEC-166, DEC-167, DEC-169, DEC-171, DEC-174, DEC-176) goes through
  `web/src/lib/useRevisionSave.ts`:
  sent, then on a refusal read once and done, sent again or handed to the page as a conflict,
  which `web/src/lib/refusedSave.tsx` tells. A form that shows the stored row after an outcome uses the same file's
  `useManagedSave` (the room editor and its Deactivate and Reactivate, the two room controls, the household's details
  form, the two teen consent controls), which
  returns that row from the write's answer or from the read the protocol made, and gives a conflict the row the read
  found. A save counts only when its answer names what it
  saved (`answerNames`); after a send with no answer, a refusal is never sent again under a newer
  revision until a save lands. Nothing about a save outlives the draft that sent it (the date
  editor keeps that no-answer mark with its held draft). A control that stays on screen and is
  used again (the room controls) passes `stays`: each outcome moves the base its next save names
  (a landed save to what its answer says was stored, a read to what it found), and a send with no
  answer moves nothing; a refusal that arrives after such a control left the screen hands nothing
  to its host, whether it was the send's or the read's. A room control compares what its pick sets: the room alone for a pick that leaves
  the padding (or which busy dates it books over) to the server, the padding for "Save times",
  the busy dates for "Book anyway"; a date with no booking and one whose booking lost its room
  compare apart. A "Book anyway" whose answer was lost keeps its prompt and reason, so the next
  press settles it. The series pick writes more than its read shows (every date's booking,
  `writesMore`): a read showing its room counts as this pick only after a send of it got no
  answer, and otherwise the pick goes again under the new revision, so what the control says
  comes from the write's own answer (its counts, `doubleBooked` included) or, after a lost
  answer, from what the read shows. The next pick is chosen from the dates as the answer read them after the write
  (`after`), never from its counts. Its list of dates with a room of their own is never compared: the pick names the
  dates it changes (`includeDates`, DEC-168), and the server changes only those; the dates named go with the hook's
  `unseen`, so a pick whose answer was lost counts as this one only when it named the same dates. After a conflict, as after a landed pick, it asks its host to read again
  quietly (`reload`), so the page's place and lists show the room while the editor keeps its
  draft. "Dates that need a room" says what a row's own pick left when the row leaves the list
  (booked, waiting for an admin, released), and after a refused pick only that the choice could
  not be saved and the date is no longer listed.
- The three admin settings editors (#482) share `RevisedSetting`. A save returns the value and its
  new revision. A conflict discards the draft and reads the setting again. The admin account
  editor uses the same protocol, then closes and reloads the users list after a conflict.
- The teen consent controls (DEC-176) send a grant and a revoke through `useManagedSave` with `stays`: a revoke is
  never refused as stale, but its answer moves the base the next grant names. They compare how many changes the
  teen's log holds and who made the newest (the admin panel: its scope, status, granter and evidence; the guardian
  card: each scope's status and whether this guardian made it), never a status alone, so a grant and a revoke since the
  read is a conflict and never a quiet resend. The admin panel is an editor: a conflict closes it and the users list
  reads again and says so. The guardian card stays and says so at the control (`choosing`).
- The message composer (DEC-169) mounts its editor once per message it opens and stays open after
  its saves (`stays`); a new draft's first save creates it through the same hook. The page shows
  an editor only while the address names its message, so an editor the person moved away from
  can't act. Its send is not a save of the hook's: the confirmation names the save's revision and
  the review it showed, a `message_list_changed` answer replaces the confirmation with the change,
  and a press whose answer was lost is settled by the next one, judged by what a send changes (the
  status and the scheduled time, `wentOut`), never by the text, which a send leaves alone. A
  `message_changed` answer to a send is someone else's change when the status or the scheduled
  time moved since the question was asked, and the page then says the message was not sent
  (`RefusedSaveLine`'s send wording). A save of a scheduled message that changes its list is
  answered with a review of the proposal; the confirmation sends the save again with its token and
  the press that asked goes on. A Schedule press ends there when the form's time is the one the
  message has; a new time is the send's own question, on the token the save's answer gives, so a
  refused schedule never leaves a list beside a time nobody was asked about.
- `web/src/components/ui/Form.tsx` validates; the submit lifecycle is `web/src/lib/useAction.ts`
  (idle, running, failed, done). It refuses a second run while one is on its way, always leaves
  running, words a throw with `errorText`, and keeps a follow-up step (a reload after the save)
  apart from the save, so a failed reload is never told as a failed save. A `reset` while a run is
  on its way keeps it running and lands its answer as idle, so a cancelled draft's answer never
  shows in the next one. The teen consent controls (`web/src/components/GuardianConsentCard.tsx`,
  `web/src/components/TeenConsentPanel.tsx`) need no reload after a change: its answer is the
  history or card row it left (DEC-176). Most older forms still keep their own `busy`,
  `error` and done values; the `busyFlags` ratchet only lets that count fall. It counts
  single-line `useState` declarations by name, so a flag named otherwise (`pending`) or a
  declaration split across lines is not counted.
- A making-more-private review is preview, review, confirm in two places with the same rule:
  `web/src/components/EventVisibility.tsx` and `web/src/pages/GroupDetail.tsx`, around
  `web/src/components/GroupVisibilityReview.tsx`. Each runs its Save (the preview included) and the
  review's confirm in one `useAction`, so neither starts while the other is on its way; the group
  page hands its run to `web/src/components/GroupForm.tsx` (`action`).
- Server statuses are typed in `web/src/types.ts`, most as unions and a few (a guest
  registration's) as plain `string`; their words are in `web/src/lib/status.ts`
  (`accountStatusLabel`, `groupStatusLabel`, `registrationStatusLabel`,
  `contactRequestStatusLabel`) and `web/src/lib/messages.ts` for messages.

## Rules and their tests

| Rule | Enforced by |
|------|-------------|
| Writes of account, room booking, room request, message, recipient, and registration statuses stay at the named sites. Other updates must identify the columns they write. | `TestAccountStatusHasOneWriter`, `TestRoomStatusHasOneWriter`, `TestMessageStatusHasOneWriter`, `TestRecipientStatusHasOneWriter`, `TestRegistrationStatusHasTwoWriters` |
| A new hand-rolled busy flag (`useState` named `busy`, `saving`, `submitting`, `sending` or the like) in `web/src` | `TestUIRatchets` (`busyFlags` in `ui-ratchet.json`), `TestUIRatchetsCatchPastMistakes` |
| Every column whose CHECK lists its values has a row here with exactly those values, or a line under "Enumerated columns that are not lifecycles"; every Go name written with its file, every Go test and every `web/` path this map names exists | `TestLifecycleMapMatchesSchemaAndSource`, `TestLifecycleMapGuardCatchesMistakes` |
