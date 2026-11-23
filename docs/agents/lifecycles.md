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
  booleans. A page that loads one resource uses `web/src/lib/useLoad.ts`. A server status is
  typed as a union in `web/src/types.ts` and gets its words from `web/src/lib/status.ts`.

## Accounts and identity

| Lifecycle | States | Where a move is decided | Writers | Enforced by |
|---|---|---|---|---|
| `users.status` | `active` `invited` `guest` `pending_consent` `disabled` | New rows: `account_status.go:newAccountStatus`. Changes: inline in `api_admin.go:apiAdminUpdateUser`, `api_teen_consent.go:changeTeenConsent` and `age_rollover.go:rolloverTeen` | `account_status.go:setAccountStatusIn`, `account_status.go:upgradeGuestIn`; outside them: `age_rollover.go:rolloverTeen`, `api_teen_conversion.go:completeTeenConversionIfReady`, `account_security.go:replacePasswordIn` (invited to active), `account_erasure.go:tombstoneUserIn` | `users_status_check`, `TestAdminEditCannotBypassTeenConsent`, `TestAgeRolloverStatusesAndConsentHistory` |
| `users.account_type` | `adult` `teen` | `age_rollover.go:rolloverTeen` (18 on the church calendar); a guest row takes its type in `account_status.go:upgradeGuestIn` | the same two | `users_account_type_check` |
| `household_accounts.relation` | `guardian` `adult` `teen` | teen to adult with the account, in `age_rollover.go:rolloverTeen` | `age_rollover.go:rolloverTeen` | `household_accounts_relation_check` |
| `household_people.kind` | `child` `teen` `adult` | `api_households.go:apiUpdateHouseholdPerson`; a staged teen turns adult in `age_rollover.go:rolloverTeen` | the same two | `household_people_kind_check`, `TestHouseholdTablesConstraints` |
| `users.role` (no CHECK) | `member` `admin` | `internal/policy/policy.go:CanDemoteOrDelete` | `api_admin.go:apiAdminUpdateUser` | nothing in the schema |
| `account_requests.status` | `pending` `approved` `rejected` `expired` `withdrawn` | `WHERE status = 'pending'` in each writer; a decision's UPDATE also needs `expires_at` still ahead (`decide_pending.go:decidePendingIn` with `stillLive`), so a request that lapses between the read and the write answers expired, and the sweep stamps it | `api_account_requests.go:decideAccountRequest` (through `decide_pending.go:decidePendingIn`), `api_account_requests.go:expirePendingRequests`, `account_erasure.go:eraseAccountRequestsIn` | `account_requests_status_check`, `TestDecisionRefusesRequestThatExpiresBeforeTheWrite` |
| `guardian_links.status` | `pending` `confirmed` `rejected` `revoked` | `WHERE` guards in each writer | made by `api_households.go:createGuardianLink`, `api_teen_guardians.go:requestTeenGuardian`; decided by `api_admin_guardian_links.go:decideGuardianLink`; ended by `api_households.go:revokeGuardianLinks`, `age_rollover.go:rolloverTeen`, `account_erasure_households.go:eraseHouseholdRecordsIn` | `guardian_links_status_check`, `TestAgeRolloverClosesOnlyPendingGuardianTasks` |
| `teen_consents.status` | `granted` `revoked` | Append only: the latest row per teen and scope is the state. `api_teen_consent.go:changeTeenConsent` | `api_teen_consent.go:changeTeenConsent` | `teen_consents_status_check`, trigger `teen_consents_append_only`, `TestTeenConsentAppendOnly` |
| Invitation (`accepted_at`, `revoked_at`, `expires_at`) | open, accepted, revoked, expired by the clock | the open predicate in `tokens.go:claimInvitation` and in each writer's `WHERE` | `tokens.go:claimInvitation`, `api_household_conversion.go:revokeConversionInvitation`, `api_household_conversion.go:issueHouseholdConversion`; no sweep | unique open conversion index |
| Account token (`used_at`, `expires_at`) | live, used, expired by the clock, deleted | `tokens.go:claimAccountToken` | `tokens.go:storeAccountToken`, `tokens.go:claimAccountToken`, `tokens.go:revokeAccountTokens` (a revoke is a delete); no sweep | `account_tokens_kind_check`, unique token hash |
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
| `groups.status` (no CHECK) | `pending` `active` `archived` | New groups: `internal/policy/policy.go:NewGroupStatus`. Edits: `internal/policy/policy.go:CanSetGroupStatus` judges the target status only. The approval queue: `decide_pending.go:decidePendingIn` | `api_groups.go:apiCreateGroup`, `api_admin_groups.go:decideGroup`, `api_groups.go:apiUpdateGroup` | `TestCanSetGroupStatus`, `TestGroupApprovalFlow` |
| Group deletion request (`deletion_requested`, `deleted_at`) | not asked, asked, deleted | `internal/policy/policy.go:CanRequestDeletion` | `api_groups.go:apiRequestDeletion`, `api_groups.go:clearDeletionRequestIn`, `api_groups.go:deleteGroup` | `TestDeletionRequestTask` |
| `memberships.status` | `active` `removed` `left` | `internal/policy/policy.go:CanJoinGroup`, `internal/policy/policy.go:CanLeaveGroup`, `internal/policy/policy.go:CanRemoveMember`; a disabled account's memberships stay `active` and are hidden by the join on `users.status` | `api_groups.go:changeRoleIn`, `api_departure.go:applyDeparture`, `api_handoff.go:handOffLeadership`, `api_handoff.go:restoreMemberships` | `memberships_status_check`, trigger `memberships_tenure_guard`, `TestMembershipTenureGuard` |
| `memberships.role` | `leader` `member` | `internal/policy/policy.go:CanChangeRole` (a group keeps a leader), `internal/policy/policy.go:CanBeSuccessor` | `api_groups.go:changeRoleIn`, `api_departure.go:applyDeparture`, `api_handoff.go:handOffLeadership`, `api_handoff.go:demoteEverywhere`, `api_handoff.go:restoreMemberships` | `memberships_role_check`, `TestCanChangeRole`, `TestSoleLeaderHandoff` |
| `event_occurrences.status` | `scheduled` `cancelled` | Series edits: `internal/events/series.go:ApplySeries`. One date: the `WHERE` in `api_events.go:apiCancelOccurrence`. No writer schedules a manually cancelled date again | `internal/eventdates/extend.go:Ensure`, `api_events.go:apiUpdateEvent`, `api_events.go:apiCancelOccurrence` | `event_occurrences_status_check`, `TestOccurrenceCancelSource` |
| `event_occurrences.cancel_source` | `''` `series` `manual` | with the status above; `series` can be scheduled again by a later series edit (DEC-91) | the same writers | `event_occurrences_cancel_source_check`, `TestSeriesEditRestoresSeriesCancelledDates` |
| `groups.visibility` | `public` `private` `invite_only` | Wider is free. Narrower previews, needs consent and cancels (`docs/agents/visibility.md`): `visibility_consent.go:checkConsentIn` | `api_groups.go:apiUpdateGroup` through `api_group_visibility.go:applyGroupTightening` | `groups_visibility_check`, `TestGroupMorePrivateTightensAndCancels` |
| `events.visibility` | `public` `private` `invite_only` | the same, with `internal/policy/policy.go:CanSetEventVisibility` | `api_event_access.go:apiSetEventVisibility`, `api_group_visibility.go:applyGroupTightening` | `events_visibility_check`, `TestEveryEventVisibilityWriterDeclaresConsent` |
| `event_occurrences.visibility_override` | `public` `private` `invite_only` | the same; null means the event's value (`internal/policy/policy.go:EffectiveVisibility`) | `api_occurrence_visibility.go:changeDateVisibilitySelected` | `event_occurrences_visibility_override_check`, `TestVisibilityConsentLifecycleMatrix` |
| `events.leader_opened` (a flag beside visibility) | false, true: the event was opened wider than its group on purpose | set from `internal/policy/policy.go:MoreOpen` when the event is made or its visibility set; a group tightening asks before narrowing an opened event | `api_events.go:apiCreateEvent`, `api_event_access.go:apiSetEventVisibility`, `api_group_visibility.go:applyGroupTightening` | `events_churchwide_check`, `TestLeaderOpenedEventsAskBeforeTightening` |
| A visibility consent (a plan per transaction, `settings.visibility_consent_epoch`) | an entry untaken, taken; the plan done; the epoch replaced at each boot | `internal/consent/consent.go:Check`; `visibility_consent.go:checkConsentIn` runs once per write | `visibility_consent.go:checkConsentIn`; the epoch only by `restore_fence.go:bootRestoreFence` | trigger `audit_logs_visibility_consent`, `TestConsentEpochHasOneWriter`, `TestVisibilityCancellationsConsumeACheckedPlan` |
| A date's phase (no column) | upcoming, underway, ended | each reader compares `starts_at` and `ends_at` with its own `now`; an end time cannot rise while someone keeps the date (`api_date_edits.go:refuseKeptDates`) | none; nothing stamps an ended date | trigger `event_occurrences_kept_date_end`, `TestEndsAtWritersCheckKeptDates` |
| Edit revisions (`edit_revision`, `edit_receipt`) | a counter and a receipt | database triggers (migration 00010); `api_date_edits.go:baseSuperseded` compares | the triggers | `TestDateEditRevisionsMigration` |
| Series clock repair (`events.series_clock_dirty`) | clean, dirty | a trigger sets it (migration 00009) | cleared by `internal/eventdates/extend.go:ClearDirty` | `TestOccurrenceSeriesStartsMigration` |
| Church-wide event hosts (`event_hosts`) | at least one live host | `internal/policy/policy.go:EventKeepsHost` | `api_event_access.go:apiAddEventHost`, `api_event_access.go:apiRemoveEventHost`, `api_host_handoff.go:handOffHosting` | `TestEventKeepsHost` |
| `room_bookings.status` | `confirmed` `requested` `rejected` | inline; `requested` is waiting only while `request_id` names a pending room request, otherwise the booking holds nothing | `api_room_bookings.go:placeBooking`, `api_room_bookings.go:moveRoomBookings`, `api_room_bookings.go:releaseRoomBookings`, `api_room_requests.go:apiConfirmRoomRequest`, `api_room_requests.go:apiRejectRoomRequest` | `room_bookings_status_check`, exclusion `room_bookings_no_overlap`, `TestRoomBookingConcurrentExactlyOneWins` |
| `room_requests.status` | `pending` `confirmed` `rejected` `withdrawn` | `api_room_requests.go:lockRequest` (pending only); `withdrawn` only as a side effect of `api_room_requests.go:closeEmptyRoomRequests`, or of a new series pick replacing the event's one pending series request (`api_room_requests.go:withdrawSeriesRequest`, called by `api_room_series.go:bookWholeSeries`) | `api_room_requests.go:openRoomRequest`, `api_room_requests.go:apiConfirmRoomRequest`, `api_room_requests.go:apiRejectRoomRequest`, `api_room_requests.go:closeEmptyRoomRequests`, `api_room_requests.go:withdrawSeriesRequest` | `room_requests_status_check`, `idx_room_requests_one_series`, `TestRoomApprovalSeries`, `TestRoomApprovalSeriesRepickWhilePending` |
| Series room (`default_room_id`, `default_room_pending`) | none, approved, awaiting approval | inline in `api_room_series.go:bookWholeSeries` | `api_room_series.go:bookWholeSeries`, `api_room_requests.go:apiConfirmRoomRequest`, `api_room_requests.go:apiRejectRoomRequest`, `api_room_locations.go:apiAdminMatchLocation` | `TestRoomApprovalSeriesRoomStaysUnapproved` |
| A calendar link (`calendar_tokens`, a row or none) | none, live; a new link replaces the old one, and turning it off deletes the row | inline | `api_feeds.go:apiCreateMyCalendarFeed`, `api_feeds.go:apiUpdateMyCalendarFeed` (scope), `api_feeds.go:revokeCalendarToken` | `TestPersonalFeedReadsItsLinkInTheFeedsSnapshot` |
| A room's feed link (`room_feed_tokens.revoked_at`) | none, live, revoked; a new link revokes the old one | the `WHERE revoked_at IS NULL` in each writer | `api_room_feeds.go:apiAdminCreateRoomFeed`, `api_room_feeds.go:apiAdminRevokeRoomFeed` | `TestRoomFeedReadsItsLinkAndSwitchInOneSnapshot` |

## Registrations and requests

| Lifecycle | States | Where a move is decided | Writers | Enforced by |
|---|---|---|---|---|
| `registrations.status` | `going` `maybe` `declined` `cancelled` `waitlisted` `removed` | A person's answer: `api_registrations.go:prepareRegistration` and `registration_seats.go:seatChange`. Removal, guest cancel, manager add and series undo each decide inline. A disabled holder's row keeps its status and stops counting (`registration_subjects.go:listedRegistrations`) | `registration_rows.go:saveRegistration`, `registration_rows.go:promoteRegistration` | `registrations_status_check`, `TestRegistrationStatusVocabulary`, `TestSeatSQLMatchesGoRules`, `TestConcurrentGoingRespectsCapacity` |
| Date access on a registration (`access_origin`, `access_ended_at`, `access_end_reason`) | none, active, ended | trigger `registration_access_provenance`; `internal/policy/occurrence_access.go:OccurrenceAccessCurrent`. A kept date that has ended stays unstamped until a reconcile or the daily sweep | `registration_access.go:grantRegistrationAccess`, `registration_access.go:endAccess` | `registrations_access_end_state`, `TestHardDeletePathsEndLiveGrantsFirst`, `TestKeptDateSQLMatchesPolicy` |
| A guest's manage link (`registrations.manage_token_hash`) | none, live; replaced on promotion | cleared by status in `registration_rows.go:saveRegistration`; judged live at read in `api_guest.go:liveTokenManage` | `api_guest.go:apiConfirmGuestRegistration`, `registration_rows.go:saveRegistration`, `registration_rows.go:promoteRegistration`, `api_occurrence_mutations.go:recordManagerOccurrenceAdd` | `TestManageTokenIndex`, `TestGuestManageReauthorizesCancelledLinks` |
| `contact_requests.status` | `pending` `granted` `denied` `withdrawn` `expired` `ended` | `internal/policy/contact.go:ContactRequestStatusAfterDecision`, `internal/policy/occurrence_access.go:AddRequestStatusAfterDecision`, `internal/policy/contact.go:ContactRequestExpired`; what a purpose decides is `request_kinds.go:requestKind` | `request_lifecycle.go:openRequest`, `request_lifecycle.go:recordDecision`, `request_lifecycle.go:withdrawRequest`, `contact_lifecycle.go:finishContactRequest`, `api_contact.go:completeGroupAsk`, `api_occurrence_mutations.go:completeAddAsk` | `contact_requests_status_check`, `contact_requests_outcome`, trigger `add_request_identity` (add purposes), `TestContactRequestsConstraints`, `TestContactTerminalWinsApproval` |
| `contact_requests.subject_decision` | `pending` `approved` `denied` | `internal/policy/contact.go:ContactRequestStatusAfterDecision` (a party decides once) | `api_contact.go:setRequestDecision` | `contact_requests_subject_decision_check`, `TestContactParallelApprovalsOneGrant` |
| `contact_requests.admin_decision` | `pending` `approved` `denied` `not_needed` | the same; `not_needed` is fixed when the request opens | `request_lifecycle.go:openRequest`, `api_contact.go:setRequestDecision` | `contact_requests_admin_decision_check` |
| Contact grant (`ended_at`, `end_reason`) | live, ended | `internal/policy/contact.go:ContactGrantRevocationReason`, `internal/policy/contact.go:ContactIneligibility` | `api_contact.go:apiDecideContactRequest`, `contact_lifecycle.go:endContactGrant` | `contact_grants_check`, `TestContactRestoredRoleCannotResurrect` |
| Manager-add block (`manager_add_blocks`, a row or none) | absent, present | `internal/policy/occurrence_access.go:CanBlockManagerAdds`; only erasure removes one | `api_occurrence_mutations.go:mutateOwnOccurrence` | unique key |
| Guest signup (`confirmed_at`, `expires_at`) | pending, claimed, expired by the clock | the `WHERE` in `api_guest.go:claimGuestSignup` | `api_guest.go:apiCreateGuestRegistration`, `api_guest.go:claimGuestSignup`; no sweep | `TestGuestRedeemOnFullEventWithoutWaitlistCreatesNothing` |
| Series undo receipt (`used_at`, `expires_at`) | live, used, expired by the clock | inline in `api_series_registrations.go:apiUndoSeriesRegistration` | `api_series_registrations.go:apiRegisterSeries`, `api_series_registrations.go:apiUndoSeriesRegistration` | `TestSeriesUndoReceiptConstraints` |

## Messaging, notifications and operations

| Lifecycle | States | Where a move is decided | Writers | Enforced by |
|---|---|---|---|---|
| `jobs.status` | `queued` `running` `done` `failed` `dead` `dismissed` | `internal/jobs`, model-checked (`.cursor/skills/verify-job-queue`) | `internal/jobs/jobs.go:RunOnce`, `internal/jobs/jobs.go:LaterIn`, `internal/jobs/dead.go:Retry`, `internal/jobs/dead.go:Dismiss` | `jobs_status_check`, the TLA+ model, `TestTwoWorkersNeverRunAJobTwice` |
| `messages.status` | `draft` `scheduled` `sending` `sent` `cancelled` `failed` | the move table in `internal/policy/message_lifecycle.go:MessageMove`; a retry reopens `sent` and `failed`, an email reminder is created `scheduled` and a push reminder `sent` | changes: `message_status.go:moveMessage`, `message_status.go:moveMessagesWhere` (the author's erasure); new rows: `api_messages.go:apiCreateMessage`, `reminders.go:writeReminder`, `reminders_push.go:writePushReminder` | `messages_status_check`, `TestMessageMoves`, `TestMessageStatusesMatchTheSchema`, `TestMoveMessageSQLMatchesTheRule`, `TestMoveMessagesWhereSQLMatchesTheRule`, `TestMessageStatusHasOneWriter`, `TestMessageDraftScheduleCancel`, `TestMessagePipelineHonorsCancel` |
| `message_recipients.status` | `queued` `suppressed` `deferred` `sent` `delivered` `bounced` `complained` `failed` | Email and push share the column. Email: `messages_send.go:markRecipient`, then provider reports in `api_email_webhooks.go:applyEmailEvent`. Push: `messages_push.go:messagePushRecorder`. No writer sets `deferred` | those three, `api_message_history.go:apiRetryMessage`, `messages_send.go:reapMessages`, `messages_push.go:reapMessagePushes`, `account_erasure.go:eraseMessagesIn` | `message_recipients_status_check`, `TestPostmarkReportsUpdateDeliveriesAndSuppress`, `TestRetryNeverPushesAgain` |
| `notification_deliveries.status` | `queued` `suppressed` `deferred` `sent` `delivered` `bounced` `failed` | Email: `notify_deliver.go:updateEmailDelivery`, then `api_email_webhooks.go:applyEmailEvent`. Push: `push_deliver.go:recordPushIn` | those three, `internal/notify/notify.go:Send` | `notification_deliveries_status_check`, `TestNotifyDeliverSendsOnce` |
| A notification (`read_at`, `archived_at`, `resolved_at`, `expires_at`) | unread or read; live or archived; a task open or resolved | `internal/notify` owns every write | `internal/notify/notify.go:Send`, `internal/notify/notify.go:SetRead`, `internal/notify/notify.go:SetArchived`, `internal/notify/notify.go:Resolve`, `internal/notify/notify.go:Prune` | `TestStatesBadgeAndReadAll`, `TestStoredNotificationsAcceptance` |
| A push device (`push_subscriptions`, a row or none) | absent, bound, failing, expired by the clock | `api_push.go:apiPushSubscribe`; `binding_generation` moves only in a trigger (migration 00011); a session end deletes devices in trigger `push_devices_end_with_sessions` (`docs/agents/push.md`) | `api_push.go:apiPushSubscribe`, `api_push.go:apiPushUnsubscribe`, `push_deliver.go:markPushFailed`, `push_deliver.go:prunePushDevices` | `TestPushBindingGenerationMigration`, `TestPushDevicesEndWithSessions` |
| Push keys (`settings`, the VAPID pair) | absent, present, unreadable after a secret change | `push_keys.go:ensureVAPIDKeys` at boot, `push_keys.go:rotateVAPIDKeys` from the CLI; a new pair deletes every device | `push_keys.go:writeVAPIDKeys` | `TestVAPIDKeysAreMadeOnceOnBoot` |
| Set-once and count-up columns | `jobs.claim_seq` (a new number per claim), `notifications.batch_count` (up on a merge), `message_recipients.audience_admitted_at` (set once) | each in its one writer | `internal/jobs/jobs.go:RunOnce`, `internal/notify/notify.go:merge`, `messages_send.go:markRecipient`, `messages_push.go:markPushAdmitted` | `TestJobClaimSeqMigration`, `TestMessageAudienceAdmittedMigration` |
| `consents.status` | `opted_in` `opted_out` | Append only: the latest row per person, channel and scope is the state; with no row, the caller's default | `email_unsubscribe.go:setConsent` | `consents_status_check`, trigger `consents_append_only`, `TestConsentTriggerAllowsOnlyTheErasureScrub` |
| An address's own opt-out and suppression | present or absent; open or cleared | `email_optouts.go:readEmailOff`; `docs/agents/mail.md` | `email_optouts.go:exportEmailOptOutsIn`, `email_optouts.go:clearOptOutOnTurnOn`, `api_email_webhooks.go:applyEmailEvent`, `api_admin_suppressions.go:apiAdminClearSuppression` | `TestEmailChoiceSQLStaysInOneHelper`, `TestSuppressionSQLStaysInItsHelpers` |
| A reminder sent once | unwritten, written | unique keys from `reminder_key.go:reminderKey` | `reminders.go:writeReminder`, `reminders_push.go:writePushReminder` | `TestReminderSweepQueuesOncePerDate` |
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

- The shapes to copy, each one value that names the state:
  `web/src/lib/useLoad.ts` (a load), `web/src/lib/dateWrites.tsx` (`Phase`, a guarded date edit),
  `web/src/components/DeleteUserPanel.tsx` and `web/src/pages/ReasonReviewLegacy.tsx` (`Phase`, a
  delete and its review), `web/src/lib/pushState.ts` (`DeviceState`, `PushCardState`) and the
  `busy` union in `web/src/pages/MessageComposer.tsx`.
- `web/src/components/ui/Form.tsx` validates and has no submit lifecycle, so each form keeps its
  own `busy`, `error` and done values.
- A making-more-private review is preview, review, confirm in two places with the same rule:
  `web/src/components/EventVisibility.tsx` and `web/src/pages/GroupDetail.tsx`, around
  `web/src/components/GroupVisibilityReview.tsx`.
- Server statuses are typed in `web/src/types.ts`, most as unions and a few (a group's status, a
  guest registration's) as plain `string`; their words are in `web/src/lib/status.ts`
  (`accountStatusLabel`, `groupStatusLabel`, `registrationStatusLabel`,
  `contactRequestStatusLabel`) and `web/src/lib/messages.ts` for messages.

## Rules and their tests

| Rule | Enforced by |
|------|-------------|
| Every column whose CHECK lists its values has a row here with exactly those values, or a line under "Enumerated columns that are not lifecycles"; every Go name written with its file, every Go test and every `web/` path this map names exists | `TestLifecycleMapMatchesSchemaAndSource`, `TestLifecycleMapGuardCatchesMistakes` |
