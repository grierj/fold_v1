package main

import (
	"time"

	"fold/models"
)

// Named shapes for JSON answers that were ad-hoc gin.H maps. Each is exactly
// the keys its handler always sent; web/src/types.ts names the same fields
// and TestWebTypesMatchGoJSON (tsContracts) keeps the two together.

// sessionJSON answers a sign-in and GET /auth/me for a signed-in person.
type sessionJSON struct {
	User       userJSON `json:"user"`
	CanCompose bool     `json:"canCompose"`
}

// sessionBootstrapJSON is GET /auth/bootstrap (M8.1): the viewer this
// browser's cookie authenticates (null when none), its CSRF token and that
// token's credential marker.
type sessionBootstrapJSON struct {
	User             *userJSON `json:"user"`
	CanCompose       bool      `json:"canCompose"`
	CSRFToken        string    `json:"csrfToken"`
	CredentialMarker string    `json:"credentialMarker"`
}

// siteJSON is GET /site.
type siteJSON struct {
	Name         string `json:"name"`
	Timezone     string `json:"timezone"`
	SelfSignup   string `json:"selfSignup"`
	RoomsEnabled bool   `json:"roomsEnabled"`
	// PushEnabled: the directions page (/notifications/setup) is public and
	// hides itself when the church turned notifications off (DEC-124).
	PushEnabled bool `json:"pushEnabled"`
}

// accountRequestConfirmJSON answers POST /account-requests/confirm.
type accountRequestConfirmJSON struct {
	User           userJSON `json:"user"`
	PendingConsent bool     `json:"pendingConsent"`
}

// accountRequestDecisionJSON answers an admin's approve or reject.
type accountRequestDecisionJSON struct {
	Request accountRequestJSON `json:"request"`
	UserID  uint               `json:"userId"`
}

// calendarResponseJSON is GET /calendar.
type calendarResponseJSON struct {
	Occurrences []calendarOccurrenceJSON `json:"occurrences"`
	Truncated   bool                     `json:"truncated"`
}

// guestConfirmationJSON answers POST /guest-registrations/confirm.
type guestConfirmationJSON struct {
	Registration guestRegistrationJSON `json:"registration"`
	ManageToken  string                `json:"manageToken"`
}

// profileUpdateJSON answers PUT /me. VerificationSent is set when an email
// change started.
type profileUpdateJSON struct {
	User             userJSON `json:"user"`
	VerificationSent bool     `json:"verificationSent,omitempty"`
}

// emailSettingsLinkJSON is GET /email-settings/:token.
type emailSettingsLinkJSON struct {
	EmailSettings emailSettingsJSON `json:"emailSettings"`
	Email         string            `json:"email"`
	FromCategory  string            `json:"fromCategory"`
	CanTurnOn     bool              `json:"canTurnOn"`
}

// teenConsentsJSON is GET /admin/users/:id/teen-consents.
type teenConsentsJSON struct {
	Consents                 []teenConsentJSON `json:"consents"`
	AdministrativelyDisabled bool              `json:"administrativelyDisabled"`
}

// guardianLinksResponseJSON is GET /admin/guardian-links.
type guardianLinksResponseJSON struct {
	GuardianLinks []guardianLinkJSON       `json:"guardianLinks"`
	Nominations   []guardianNominationJSON `json:"nominations"`
}

// adminJobsJSON is GET /admin/jobs.
type adminJobsJSON struct {
	Jobs       []adminJobJSON           `json:"jobs"`
	More       bool                     `json:"more"`
	Deliveries adminDeliverySummaryJSON `json:"deliveries"`
}

// backupStatusJSON is GET /admin/backup and `fold backup status --json`
// (backup_status.go). State is unwatched, missing, unreadable, stale or ok; a
// field the state or the files cannot vouch for is null, never zero.
type backupStatusJSON struct {
	State          string     `json:"state"`
	LastOkAt       *time.Time `json:"lastOkAt"`
	DumpBytes      *int64     `json:"dumpBytes"`
	UploadsBytes   *int64     `json:"uploadsBytes"`
	Offsite        *string    `json:"offsite"` // "off" or "uploaded"
	LastFailedAt   *time.Time `json:"lastFailedAt"`
	LastFailedStep *string    `json:"lastFailedStep"` // a step the server knows, else null
}

// adminRoomsJSON is GET /admin/rooms.
type adminRoomsJSON struct {
	Rooms        []roomJSON `json:"rooms"`
	NeedApproval bool       `json:"needApproval"`
}

// matchLocationJSON answers POST /admin/rooms/locations/match.
type matchLocationJSON struct {
	Booked    int             `json:"booked"`
	Busy      []matchBusyJSON `json:"busy"`
	BusyTotal int             `json:"busyTotal"`
	Series    int             `json:"series"`
}

// roomRequestConfirmationJSON answers POST /admin/room-requests/:id/confirm.
type roomRequestConfirmationJSON struct {
	Confirmed  int      `json:"confirmed"`
	Overlapped int      `json:"overlapped"`
	Lost       []string `json:"lost"`
}

// dateRoomsJSON is GET /events/:id/dates/:date/rooms.
type dateRoomsJSON struct {
	Rooms       []roomPickerJSON    `json:"rooms"`
	Booking     *roomBookingJSON    `json:"booking"`
	DateRoom    revisionRefJSON     `json:"dateRoom"`
	Suggestions roomSuggestionsJSON `json:"suggestions"`
}

// revisionRefJSON names what a manager's pick was made from or stored: the
// thing's id and its token. A date's room (dateRoom) is named by its date's
// occurrence id and the booking's token (dateRoomRevisionIn).
type revisionRefJSON struct {
	ID       uint   `json:"id"`
	Revision string `json:"revision"`
}

// seriesRoomsJSON is GET /events/:id/rooms. Revision is the event's token
// (eventRevision) a series pick sends. Pending counts the series room's
// dates waiting for an admin; a date waiting for a room of its own is in
// OwnRooms.
type seriesRoomsJSON struct {
	Revision    string              `json:"revision"`
	Rooms       []seriesRoomJSON    `json:"rooms"`
	Dates       int                 `json:"dates"`
	DefaultRoom *occurrenceRoomJSON `json:"defaultRoom"`
	OwnRooms    []ownRoomJSON       `json:"ownRooms"`
	NeedsRoom   []needsRoomJSON     `json:"needsRoom"`
	Pending     int                 `json:"pending"`
	Suggestions roomSuggestionsJSON `json:"suggestions"`
}

// roomsCheckJSON answers POST /rooms/check.
type roomsCheckJSON struct {
	Rooms       []seriesRoomJSON    `json:"rooms"`
	Dates       int                 `json:"dates"`
	Suggestions roomSuggestionsJSON `json:"suggestions"`
}

// badgeJSON is GET /notifications/badge.
type badgeJSON struct {
	Unread  int64 `json:"unread"`
	Waiting int64 `json:"waiting"`
}

type userJSON struct {
	ID           uint    `json:"id"`
	Name         string  `json:"name"`
	DisplayName  string  `json:"displayName"`
	Email        string  `json:"email"`
	Phone        string  `json:"phone"`
	AvatarURL    string  `json:"avatarUrl"`
	AvatarFocalX float64 `json:"avatarFocalX"`
	AvatarFocalY float64 `json:"avatarFocalY"`
	Role         string  `json:"role"`
	Status       string  `json:"status"`
	HasPassword  bool    `json:"hasPassword"`
	AccountType  string  `json:"accountType"`
	BirthDate    string  `json:"birthDate,omitempty"`
	// Admin list only. Kept after age rollover so consent history is reachable.
	HasTeenConsentHistory bool `json:"hasTeenConsentHistory,omitempty"`
	// Effective preference: always true for a teen, regardless of stored value.
	HideContactFromLeaders bool `json:"hideContactFromLeaders"`
	// Timezone is the person's own zone for emails (DEC-58); "" means
	// the church's, until the browser fills it in.
	Timezone string `json:"timezone"`
	// EmailSuppression: Fold stopped mailing this person's address (admin
	// list only, M3.2c, DEC-113).
	EmailSuppression *emailSuppressionJSON `json:"emailSuppression,omitempty"`
}

// toUserJSON is unrestricted: only self/session and admin account endpoints
// may use it. Rosters and attendees use context-filtered contact presentation.
func (a *App) toUserJSON(u *models.User) userJSON {
	avatarURL := ""
	if u.AvatarKey != "" {
		avatarURL = a.Uploads.URL(u.AvatarKey)
	}
	accountType := u.AccountType
	if accountType == "" {
		accountType = models.AccountAdult
	}
	return userJSON{
		ID: u.ID, Name: u.Name,
		DisplayName: u.DisplayName, Email: u.Email, Phone: u.Phone,
		AvatarURL:    avatarURL,
		AvatarFocalX: u.AvatarFocalX, AvatarFocalY: u.AvatarFocalY,
		Role: u.Role, Status: u.Status, HasPassword: u.PasswordHash != nil,
		AccountType: accountType, BirthDate: formatBirthDate(u.BirthDate),
		Timezone:               u.Timezone,
		HideContactFromLeaders: u.AccountType == models.AccountTeen || u.HideContactFromLeaders,
	}
}

// One-key (and a few two-key) answers the SPA wraps in an envelope. Each is
// exactly the keys its handlers always sent; web/src/types.ts names the same
// fields (the *Response interfaces) and tsContracts keeps the two together.

// noticeResponseJSON is a plain acknowledgement line (a request received, a mail sent).
type noticeResponseJSON struct {
	Message string `json:"message"`
}

// requestResponseJSON and requestsResponseJSON answer one contact or add
// request and a page of them; T is that kind's presented request.
type requestResponseJSON[T any] struct {
	Request T `json:"request"`
}
type requestsResponseJSON[T any] struct {
	Requests []T `json:"requests"`
}

type messageResponseJSON struct {
	Message messageJSON `json:"message"`
}

// messageScheduledResponseJSON answers scheduling: the message and who it will reach.
type messageScheduledResponseJSON struct {
	Message messageJSON         `json:"message"`
	Summary audienceSummaryJSON `json:"summary"`
}

type messageReadResponseJSON struct {
	Message messageReadJSON `json:"message"`
}

type messagePreviewResponseJSON struct {
	Preview renderedMessage `json:"preview"`
}

type messageTestResponseJSON struct {
	OK bool `json:"ok"`
}

type audienceSummaryResponseJSON struct {
	Summary audienceSummaryJSON `json:"summary"`
}

type groupListResponseJSON struct {
	Groups []groupSummaryJSON `json:"groups"`
}

type groupResponseJSON struct {
	Group groupSummaryJSON `json:"group"`
}

type membersResponseJSON struct {
	Members []memberJSON `json:"members"`
}

type soleLedGroupsResponseJSON struct {
	Groups []soleLedGroupJSON `json:"groups"`
}

type departurePreviewResponseJSON struct {
	Departure departurePreviewJSON `json:"departure"`
}

type departureConfirmationResponseJSON struct {
	Departure departureConfirmationJSON `json:"departure"`
}

type coverUploadResponseJSON struct {
	ImageURL string `json:"imageUrl"`
}

// messageImageResponseJSON answers a picture upload for a message: the
// storage key and the address to put in the body (DEC-118).
type messageImageResponseJSON struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

type avatarUploadResponseJSON struct {
	AvatarURL string `json:"avatarUrl"`
}

type userResponseJSON struct {
	User userJSON `json:"user"`
}

type usersResponseJSON struct {
	Users []userJSON `json:"users"`
}

type eventSeriesResponseJSON struct {
	Event eventJSON `json:"event"`
}

type openEventsResponseJSON struct {
	Events []occurrenceJSON `json:"events"`
}

type occurrenceResponseJSON struct {
	Occurrence occurrenceJSON `json:"occurrence"`
}

// dateOwnJSON is what a date holds of its own, as the guarded edit writes it
// (the names are its changes'), each absent while the date falls back to the
// event's value (or the group's place). Only the manager's read of the date
// carries it (managedDateIn): the editor compares these after a refused save
// (DEC-164). The start is the wall clock in the event's zone, as the save sends it.
type dateOwnJSON struct {
	StartsLocal            *string `json:"startsLocal,omitempty"`
	DurationMinutes        *int    `json:"durationMinutes,omitempty"`
	Title                  *string `json:"title,omitempty"`
	Location               *string `json:"location,omitempty"`
	CapacityOverride       *int    `json:"capacityOverride,omitempty"`
	RemoteCapacityOverride *int    `json:"remoteCapacityOverride,omitempty"`
	Visibility             *string `json:"visibility,omitempty"`
}

// impactResetDateJSON is one date an event reset would change, as its review
// shows it: the date's own visibility before the reset (DEC-158).
type impactResetDateJSON struct {
	OccurrenceID uint   `json:"occurrenceId"`
	Date         string `json:"date"`
	Title        string `json:"title"`
	Visibility   string `json:"visibility"`
}

// dateNeedsConsentJSON is what a 409 date_needs_consent carries beside error
// and code: the people saving the date would cancel (dateNeedsConsent), with
// the impact's consent token.
type dateNeedsConsentJSON struct {
	Impact visibilityImpactJSON `json:"impact"`
}

// keptDatesConflictJSON is what a 409 date_kept_after_leaving carries beside
// error and code: each refused date (its original date, sorted) and how many
// people kept it. Counts only, never names, ids or links (DEC-156).
type keptDatesConflictJSON struct {
	KeptDates []keptDateCountJSON `json:"keptDates"`
}

type keptDateCountJSON struct {
	Date      string `json:"date"`
	KeptCount int    `json:"keptCount"`
}

type eventDateResponseJSON struct {
	Occurrence dateOccurrenceJSON `json:"occurrence"`
}

// cancelledDateJSON is the cancelled-date tombstone (M8.1 brief 2.2): exactly
// the date's effective title, its original date and that it is cancelled.
type cancelledDateJSON struct {
	Title     string `json:"title"`
	Date      string `json:"date"`
	Cancelled bool   `json:"cancelled"`
}

type eventDateCancelledResponseJSON struct {
	Occurrence cancelledDateJSON `json:"occurrence"`
}

// The scheduled date page's own facts for an ordinary reader (M8.1 brief
// sections 4 and 6, policy.DatePage). An invitation-only read carries none of
// them.

// dateGroupHeaderJSON is the cover and display-safe leader name of a group the
// viewer may see (groupShownTo). imageUrl is "" for a group with no photo.
type dateGroupHeaderJSON struct {
	ImageURL    string  `json:"imageUrl"`
	CoverFocalX float64 `json:"coverFocalX"`
	CoverFocalY float64 `json:"coverFocalY"`
	LeaderName  string  `json:"leaderName"`
}

// dateAttendanceJSON is the date's row counts by answer (registrationCounts:
// declined only for its managers) and, when canSeeAttendees, the Going and
// Maybe names the existing subject rules allow (left out when there are none).
type dateAttendanceJSON struct {
	Registrations map[string]int64   `json:"registrations"`
	Attendees     []dateAttendeeJSON `json:"attendees,omitempty"`
}

type dateAttendeeJSON struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	GuestCount int    `json:"guestCount"`
}

// datePoolJSON is one pool of places: capacity and left are null when the pool
// is unlimited; left never goes below zero. used counts places, not rows.
type datePoolJSON struct {
	Capacity *int `json:"capacity"`
	Used     int  `json:"used"`
	Left     *int `json:"left"`
}

// dateSeatsJSON keeps each pool apart: remote only when the event takes
// remote answers, childcare only when it offers childcare.
type dateSeatsJSON struct {
	InPerson        datePoolJSON       `json:"inPerson"`
	Remote          *datePoolJSON      `json:"remote,omitempty"`
	WaitlistEnabled bool               `json:"waitlistEnabled"`
	Childcare       *dateChildcareJSON `json:"childcare,omitempty"`
}

// dateChildcareJSON: full is whether a limited childcare pool has no places
// left; totals are the exact numbers, only under policy.CanSeeChildcare.
type dateChildcareJSON struct {
	Full   bool          `json:"full"`
	Totals *datePoolJSON `json:"totals,omitempty"`
}

// dateSeriesJSON is the series as this date's reader may see it
// (dateProjection.ShowsSeries). The coverage counts are for a signed-in
// reader of a repeating event; usual is absent for a one-time event, which
// has nothing to differ from or answer for.
type dateSeriesJSON struct {
	RecurrenceRule  string         `json:"recurrenceRule"`
	UpcomingDates   *int           `json:"upcomingDates,omitempty"`
	CoveredDates    *int           `json:"coveredDates,omitempty"`
	CanAnswerSeries bool           `json:"canAnswerSeries"`
	Usual           *dateUsualJSON `json:"usual,omitempty"`
	Changes         []string       `json:"changes"`
}

// dateUsualJSON is what the series' dates usually are, on this date's original
// day: the series clock (the writer's stored evidence, DEC-155; startsLocal
// and endsLocal "" when it is unknown), duration, place, room, both caps and
// visibility.
type dateUsualJSON struct {
	StartsLocal     string              `json:"startsLocal"`
	EndsLocal       string              `json:"endsLocal"`
	DurationMinutes int                 `json:"durationMinutes"`
	Location        string              `json:"location"`
	Room            *occurrenceRoomJSON `json:"room"`
	Capacity        *int                `json:"capacity"`
	RemoteCapacity  *int                `json:"remoteCapacity"`
	Visibility      string              `json:"visibility"`
}

type eventHostsResponseJSON struct {
	Hosts []hostJSON `json:"hosts"`
}

type jitsiLinkResponseJSON struct {
	URL string `json:"url"`
}

type timezoneResponseJSON struct {
	Timezone string `json:"timezone"`
}

type calendarFeedResponseJSON struct {
	CalendarFeed calendarFeedJSON `json:"calendarFeed"`
}

// calendarFeedRevokedResponseJSON: an admin turned a person's link off; HadLink
// says whether there was one to turn off.
type calendarFeedRevokedResponseJSON struct {
	CalendarFeed calendarFeedJSON `json:"calendarFeed"`
	HadLink      bool             `json:"hadLink"`
}

type settingOnResponseJSON struct {
	On bool `json:"on"`
}

type settingAddressResponseJSON struct {
	Address string `json:"address"`
}

type selfSignupResponseJSON struct {
	SelfSignup string `json:"selfSignup"`
}

type adminJobStatusResponseJSON struct {
	Status string `json:"status"`
}

type notificationResponseJSON struct {
	Notification notificationJSON `json:"notification"`
}

type notificationSettingsResponseJSON struct {
	Settings notificationSettingsJSON `json:"settings"`
}

type emailSettingsChangeResponseJSON struct {
	EmailSettings emailSettingsJSON `json:"emailSettings"`
}

type pushSettingsResponseJSON struct {
	Push pushSettingsJSON `json:"push"`
}

type emailSuppressionClearedResponseJSON struct {
	Cleared int64 `json:"cleared"`
}

type emailChangeCancelledResponseJSON struct {
	Cancelled bool `json:"cancelled"`
}

type roomResponseJSON struct {
	Room roomJSON `json:"room"`
}

type roomsEnabledResponseJSON struct {
	RoomsEnabled bool `json:"roomsEnabled"`
}

type roomApprovalResponseJSON struct {
	NeedApproval bool `json:"needApproval"`
}

type roomLocationsResponseJSON struct {
	Locations []typedLocationJSON `json:"locations"`
}

type roomFeedResponseJSON struct {
	Feed roomFeedJSON `json:"feed"`
}

type roomRequestsResponseJSON struct {
	Requests []roomRequestJSON `json:"requests"`
}

// roomBookingResponseJSON answers PUT /events/:id/dates/:date/room: the
// booking it stored and the token the next pick sends.
type roomBookingResponseJSON struct {
	Booking  *roomBookingJSON `json:"booking"`
	DateRoom revisionRefJSON  `json:"dateRoom"`
}

type guardianLinkResponseJSON struct {
	GuardianLink guardianLinkJSON `json:"guardianLink"`
}

type guardianTeensResponseJSON struct {
	Teens []guardianTeenJSON `json:"teens"`
}

type guestRegistrationResponseJSON struct {
	Registration guestRegistrationJSON `json:"registration"`
}

type dateRegistrationResponseJSON struct {
	Registration registrationJSON `json:"registration"`
}

type householdResponseJSON struct {
	Household householdJSON `json:"household"`
}

type myHouseholdResponseJSON struct {
	Household *householdJSON `json:"household"`
}

type householdPersonResponseJSON struct {
	Person householdPersonJSON `json:"person"`
}

type householdConversionResponseJSON struct {
	Conversion conversionJSON `json:"conversion"`
}

type invitationResponseJSON struct {
	Invitation invitationJSON `json:"invitation"`
}

type contactGrantsResponseJSON struct {
	Grants []contactGrantJSON `json:"grants"`
}

type accountRequestsResponseJSON struct {
	Requests []accountRequestJSON `json:"requests"`
}

// legacyDiscoveryItemJSON is one undecided reason in the legacy review list:
// only the request, as a decimal string (a request id is signed 64-bit and must
// survive JavaScript). The text, the hash and the other person arrive only with
// an audited per-account page.
type legacyDiscoveryItemJSON struct {
	RequestID string `json:"requestId"`
}

// legacyDiscoveryAccountJSON groups a page's items by deleted account. userId
// is the account's users id as a decimal string and is the only reference a
// person is shown beside "Deleted account"; the client merges accounts across
// pages by it.
type legacyDiscoveryAccountJSON struct {
	UserID string                    `json:"userId"`
	Name   string                    `json:"name"`
	Items  []legacyDiscoveryItemJSON `json:"items"`
}

// legacyReasonDiscoveryJSON answers GET /admin/erasure-reasons/legacy: one
// request-atomic unit, at most 100 requests and 200 items, both parties of a
// request together. Accounts are in first-seen order and may continue on the
// next page; no total is claimed. next is the cursor, null when the table ended.
type legacyReasonDiscoveryJSON struct {
	Accounts []legacyDiscoveryAccountJSON `json:"accounts"`
	Next     *string                      `json:"next"`
}

// legacyReasonProbeJSON answers GET /admin/erasure-reasons/legacy/probe. found
// false proves only that the first unit had none.
type legacyReasonProbeJSON struct {
	Found bool `json:"found"`
}

// legacyReasonSaveJSON answers PUT /admin/erasure-reasons/legacy/:userId: how
// many of the sent decisions kept and how many removed their reason. Counts
// only, no ids and no text.
type legacyReasonSaveJSON struct {
	Kept    int `json:"kept"`
	Removed int `json:"removed"`
}
