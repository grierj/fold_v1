package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/eventdates"
	"fold/internal/events"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/internal/push"
	"fold/models"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
)

type occurrenceReadFixture struct {
	app                      *App
	issuer, recipient, other *models.User
	admin, client, stranger  *apiClient
	group                    uint
	event                    eventJSON
	dates                    []models.EventOccurrence
	access                   accessRow
}

// Seed successful invitation provenance using the real schema, not a route
// which would prematurely enable the deferred occurrence-add mutation.
func seedReadInvitation(t *testing.T, app *App, issuer, subject *models.User, ev eventJSON, occ models.EventOccurrence) accessRow {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	request := models.ContactRequest{RequesterID: issuer.ID, SubjectUserID: subject.ID,
		Purpose: "occurrence_add", ContextType: "event", ContextID: ev.ID, OccurrenceID: &occ.ID,
		Fields: "", AddAttendanceMode: models.AttendanceInPerson, NeedsAdmin: subject.AccountType == models.AccountTeen,
		SubjectDecision: "pending", AdminDecision: "not_needed", Status: "pending", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.AddDate(0, 0, 14)}
	if request.NeedsAdmin {
		request.AdminDecision = "pending"
	}
	var access accessRow
	err := app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&request).Error; err != nil {
			return err
		}
		reg := models.Registration{UserID: &subject.ID, OccurrenceID: occ.ID, Status: models.RegGoing, Source: "admin", AttendanceMode: models.AttendanceInPerson}
		if err := tx.Create(&reg).Error; err != nil {
			return err
		}
		update := map[string]any{"subject_decision": "approved", "subject_decided_by": subject.ID, "subject_decided_at": now,
			"status": "granted", "completed_at": now, "completed_target_id": reg.ID}
		if request.NeedsAdmin {
			update["admin_decision"] = "approved"
			update["admin_decided_by"] = issuer.ID
			update["admin_decided_at"] = now
		}
		if err := tx.Model(&request).Updates(update).Error; err != nil {
			return err
		}
		issuerID := issuer.ID
		var err error
		access, err = seedAccessGrantIn(tx, reg.ID, accessGrant{Origin: policy.OccurrenceOriginManagerAdd, IssuerID: &issuerID, RequestID: &request.ID}, &issuerID, now)
		return err
	})
	if err != nil {
		t.Fatal("seed valid invitation:", err)
	}
	return access
}

func newOccurrenceReadFixture(t *testing.T) occurrenceReadFixture {
	t.Helper()
	app := testApp(t)
	issuer := newTestUser(t, app, "invitation issuer", "password1234", models.RoleAdmin)
	recipient := newTestUser(t, app, "invitation recipient", "password1234", models.RoleMember)
	other := newTestUser(t, app, "other recipient", "password1234", models.RoleMember)
	admin := loginAPI(t, app, issuer.Name, "password1234")
	group := createFeedGroup(t, admin, "Hidden group identity", models.VisibilityInviteOnly, "Hidden group fallback")
	ev := createFeedEvent(t, admin, group, "Hidden series title", feedStart(7), "FREQ=WEEKLY;COUNT=3")
	var dates []models.EventOccurrence
	if err := app.DB.Where("event_id = ?", ev.ID).Order("starts_at").Find(&dates).Error; err != nil || len(dates) != 3 {
		t.Fatal(err, len(dates))
	}
	title, location := "Invited date title", "Invited date place"
	if err := app.DB.Model(&dates[1]).Updates(map[string]any{"title_override": title, "location_override": location}).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Model(&models.Event{}).Where("id = ?", ev.ID).Updates(map[string]any{"video_provider": "link", "video_url": "https://example.com/private-video", "video_reveal": "registered_only"}).Error; err != nil {
		t.Fatal(err)
	}
	access := seedReadInvitation(t, app, issuer, recipient, ev, dates[1])
	return occurrenceReadFixture{app: app, issuer: issuer, recipient: recipient, other: other, admin: admin,
		client: loginAPI(t, app, recipient.Name, "password1234"), stranger: loginAPI(t, app, other.Name, "password1234"), group: group, event: ev, dates: dates, access: access}
}

// grants is the fixture's registration as a one-element grant batch.
func (f occurrenceReadFixture) grants(t testing.TB) []models.Registration {
	t.Helper()
	var r models.Registration
	if err := f.app.DB.First(&r, f.access.ID).Error; err != nil {
		t.Fatal(err)
	}
	return []models.Registration{r}
}

func dateReadPath(ev uint, date string) string { return "/api/v1" + occurrenceDatePath(ev, date) }

// newDepartureReadFixture inserts departure provenance while the subject's
// membership is active (the T2 database contract), then removes only that
// membership. It deliberately does not use a departure route, which belongs to
// T4, and therefore exercises the retained reader/lifecycle seam directly.
func newDepartureReadFixture(t *testing.T, visibility, status, origin string) occurrenceReadFixture {
	t.Helper()
	app := testApp(t)
	issuer := newTestUser(t, app, "departure actor", "password1234", models.RoleAdmin)
	recipient := newTestUser(t, app, "departed recipient", "password1234", models.RoleMember)
	other := newTestUser(t, app, "departure outsider", "password1234", models.RoleMember)
	admin := loginAPI(t, app, issuer.Name, "password1234")
	group := createFeedGroup(t, admin, "Retained hidden group", visibility, "Retained group place")
	ev := createFeedEvent(t, admin, group, "Retained hidden series", feedStart(7), "FREQ=WEEKLY;COUNT=3")
	// Private groups remain discoverable to signed-in people. Make the event
	// itself invite-only so the exact-date reader, not ordinary series policy,
	// is what authorizes the retained private-group fixture.
	if visibility == models.VisibilityPrivate {
		if err := app.DB.Model(&models.Event{}).Where("id = ?", ev.ID).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
			t.Fatal(err)
		}
		ev.Visibility = models.VisibilityInviteOnly
	}
	var dates []models.EventOccurrence
	if err := app.DB.Where("event_id = ?", ev.ID).Order("starts_at").Find(&dates).Error; err != nil || len(dates) != 3 {
		t.Fatal(err, len(dates))
	}
	title, location := "Retained date title", "Retained date place"
	if err := app.DB.Model(&dates[1]).Updates(map[string]any{"title_override": title, "location_override": location}).Error; err != nil {
		t.Fatal(err)
	}
	membership := models.Membership{UserID: recipient.ID, GroupID: group, Role: models.MembershipMember, Status: "active"}
	var access accessRow
	if err := app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&membership).Error; err != nil {
			return err
		}
		reg := models.Registration{UserID: &recipient.ID, OccurrenceID: dates[1].ID, Status: status, Source: models.RegSourceAdmin, AttendanceMode: models.AttendanceInPerson}
		if status == models.RegWaitlisted {
			at := time.Now().UTC().Add(-time.Minute)
			reg.WaitlistedAt = &at
		}
		if err := tx.Create(&reg).Error; err != nil {
			return err
		}
		actorID := recipient.ID
		if origin == policy.OccurrenceOriginDepartureRemoval {
			actorID = issuer.ID
		}
		var err error
		access, err = seedAccessGrantIn(tx, reg.ID, accessGrant{Origin: origin, DepartureMembershipID: &membership.ID, DepartureActorID: &actorID}, &actorID, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal("seed validated departure access:", err)
	}
	if err := app.contactMutation(contactChange{UserID: recipient.ID}, func(tx *gorm.DB) error {
		return tx.Model(&models.Membership{}).Where("id = ? AND status = ?", membership.ID, "active").Update("status", "removed").Error
	}); err != nil {
		t.Fatal("remove retained membership:", err)
	}
	return occurrenceReadFixture{app: app, issuer: issuer, recipient: recipient, other: other, admin: admin,
		client: loginAPI(t, app, recipient.Name, "password1234"), stranger: loginAPI(t, app, other.Name, "password1234"), group: group, event: ev, dates: dates, access: access}
}

func TestOccurrenceDateReadExactAuthorityAndAllowlist(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	path := dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate))
	w := f.client.do("GET", path, nil, false)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var body struct {
		Occurrence map[string]json.RawMessage `json:"occurrence"`
	}
	decodeJSON(t, w, &body)
	keys := []string{}
	for key := range body.Occurrence {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := strings.Fields("canBlock canDecline canSeeAttendees canSeeVideo date dateUrl durationMinutes endsAt endsLocal eventId guestsAllowed id location myAttendanceMode myGuestCount myRegistration remoteAvailable startsAt startsLocal title tzid videoHidden videoProvider videoUrl")
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("exact key allowlist: %v, want %v", keys, want)
	}
	raw := w.Body.String()
	for _, forbidden := range []string{"Hidden group", "Hidden series", "issuer", "@", "recurrenceRule", "series", "hosts", "attendees\"", "contact", "groupId", "canManage"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("forbidden %q: %s", forbidden, raw)
		}
	}
	for _, expected := range []string{`"title":"Invited date title"`, `"location":"Invited date place"`, `"canDecline":true`, `"canBlock":true`, `"canSeeAttendees":false`, `"canSeeVideo":true`, `"myRegistration":"going"`, `private-video`} {
		if !strings.Contains(raw, expected) {
			t.Fatalf("missing %s: %s", expected, raw)
		}
	}
	apiDo(t, f.stranger, "GET", path, nil, 404)
	apiDo(t, newAPIClient(t, f.app), "GET", path, nil, 404)
	for _, date := range []string{models.FormatCalendarDate(f.dates[0].OriginalDate), models.FormatCalendarDate(f.dates[2].OriginalDate), "not-a-date"} {
		apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date), nil, 404)
	}
	for _, path := range []string{fmt.Sprintf("/api/v1/events/%d", f.event.ID), fmt.Sprintf("/api/v1/groups/%d", f.group), fmt.Sprintf("/api/v1/groups/%d/members", f.group), fmt.Sprintf("/api/v1/groups/%d/events", f.group)} {
		apiDo(t, f.client, "GET", path, nil, 404)
	}
	apiDo(t, f.client, "PUT", fmt.Sprintf("/api/v1/events/%d", f.event.ID), map[string]any{"title": "No authority"}, 404)
	apiDo(t, f.client, "POST", fmt.Sprintf("/api/v1/events/%d/registrations/add", f.event.ID), map[string]any{"date": models.FormatCalendarDate(f.dates[1].OriginalDate), "userId": f.other.ID}, 404)
	// Rich lists remain ordinary-only, even when a caller guesses the parent ID.
	if rows := f.app.occurrencesForEvents([]uint{f.event.ID}, models.FormatCalendarDate(f.dates[0].OriginalDate), models.FormatCalendarDate(f.dates[2].OriginalDate), nil, f.recipient); len(rows) != 0 {
		t.Fatal("ordinary list widened", rows)
	}
	seedReadInvitation(t, f.app, f.issuer, f.other, f.event, f.dates[2])
	apiDo(t, f.stranger, "GET", dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[2].OriginalDate)), nil, 200)
	apiDo(t, f.stranger, "GET", path, nil, 404)
	apiDo(t, f.client, "GET", dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[2].OriginalDate)), nil, 404)
	// Independent membership preserves the existing rich, series-level view.
	member := newTestUser(t, f.app, "independent member", "password1234", models.RoleMember)
	if err := f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	rows := f.app.occurrencesForEvents([]uint{f.event.ID}, models.FormatCalendarDate(f.dates[0].OriginalDate), models.FormatCalendarDate(f.dates[2].OriginalDate), nil, member)
	if len(rows) != 3 || rows[0].GroupID == nil || rows[0].RecurrenceRule == "" {
		t.Fatalf("ordinary view changed: %+v", rows)
	}
}

func TestOccurrenceDateReadLiveFactsFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(occurrenceReadFixture) error
	}{
		{"disabled", func(f occurrenceReadFixture) error {
			return f.app.DB.Model(f.recipient).Update("status", models.UserDisabled).Error
		}},
		{"account deleted", func(f occurrenceReadFixture) error { return f.app.DB.Delete(f.recipient).Error }},
		{"organizer changed", func(f occurrenceReadFixture) error {
			// The organizer is derived from the event, so moving a date to a
			// group that is not active is what closes the invitation.
			group := createFeedGroup(t, f.admin, "Replacement organizer", models.VisibilityInviteOnly, "")
			if err := f.app.DB.Model(&models.Group{}).Where("id = ?", group).Update("status", models.GroupArchived).Error; err != nil {
				return err
			}
			return f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("group_id", group).Error
		}},
		{"cancelled", func(f occurrenceReadFixture) error {
			return f.app.DB.Model(&f.dates[1]).Update("status", models.OccurrenceCancelled).Error
		}},
		{"occurrence deleted", func(f occurrenceReadFixture) error {
			// A live grant cannot be deleted with its row: end it first.
			endSeededAccess(t, f.app.DB, f.access.ID, policy.OccurrenceDeleted, nil, time.Now().UTC())
			if err := f.app.DB.Where("occurrence_id = ?", f.dates[1].ID).Delete(&models.Registration{}).Error; err != nil {
				return err
			}
			return f.app.DB.Delete(&f.dates[1]).Error
		}},
		{"event deleted", func(f occurrenceReadFixture) error { return f.app.DB.Delete(&models.Event{}, f.event.ID).Error }},
		{"group deleted", func(f occurrenceReadFixture) error { return f.app.DB.Delete(&models.Group{}, f.group).Error }},
		{"group archived", func(f occurrenceReadFixture) error {
			return f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("status", models.GroupArchived).Error
		}},
		{"registration removed", func(f occurrenceReadFixture) error {
			return f.app.DB.Model(&models.Registration{}).Where("id = ?", f.access.RegistrationID).Update("status", models.RegRemoved).Error
		}},
		{"revoked", func(f occurrenceReadFixture) error {
			endSeededAccess(t, f.app.DB, f.access.ID, policy.OccurrenceRemoved, &f.issuer.ID, time.Now().UTC())
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOccurrenceReadFixture(t)
			if err := tc.mutate(f); err != nil {
				t.Fatal(err)
			}
			date := models.FormatCalendarDate(f.dates[1].OriginalDate)
			w := f.client.do("GET", dateReadPath(f.event.ID, date), nil, false)
			if tc.name == "cancelled" {
				// A live grant on a date cancelled before its cleanup shows the
				// tombstone (M8.1 brief 2.2), and nothing else: no download,
				// and nothing for anyone without the grant.
				var got map[string]map[string]any
				want := map[string]map[string]any{"occurrence": {"title": "Invited date title", "date": date, "cancelled": true}}
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, want) {
					t.Fatal(w.Code, w.Body.String())
				}
				apiDo(t, f.client, "GET", dateReadPath(f.event.ID, date)+"/calendar.ics", nil, http.StatusNotFound)
				apiDo(t, f.stranger, "GET", dateReadPath(f.event.ID, date), nil, http.StatusNotFound)
			} else if w.Code != 404 && !((tc.name == "disabled" || tc.name == "account deleted") && w.Code == 401) {
				t.Fatal(w.Code, w.Body.String())
			}
			reads, err := f.app.scopedOccurrenceReads(f.recipient, f.app.DB.Where("event_id = ?", f.event.ID), time.Now().UTC())
			if err != nil || len(reads) != 0 {
				t.Fatalf("live facts did not close: %v %+v", err, reads)
			}
			page, raw := getCalendar(t, f.client, "from="+models.FormatCalendarDate(f.dates[0].OriginalDate)+"&to="+models.FormatCalendarDate(f.dates[2].OriginalDate)+"&tz=UTC&mine=1", 200)
			if len(page.Occurrences) != 0 {
				t.Fatal("calendar retained invalid invitation", raw)
			}
			feed := mustBuildFeed(t, f.app, f.recipient, feedOptions{personal: true, scope: models.CalendarFeedAll}, time.Now().UTC())
			if len(feed.Singles) != 0 {
				t.Fatal("feed retained invalid invitation", feed)
			}
		})
	}
}

func TestOccurrenceDateReadStableRescheduleAndIssuerIndependence(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	path := dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate))
	next := f.dates[1].StartsAt.Add(3 * time.Hour)
	if err := f.app.DB.Model(&f.dates[1]).Updates(map[string]any{"starts_at": next, "ends_at": next.Add(time.Hour), "starts_at_override": next}).Error; err != nil {
		t.Fatal(err)
	}
	// Accepted invitations are not contact grants: issuer loss does not end them.
	if err := f.app.DB.Model(f.issuer).Updates(map[string]any{"role": models.RoleMember, "status": models.UserDisabled}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.app.DB.Model(&models.Membership{}).Where("user_id = ?", f.issuer.ID).Update("role", models.MembershipMember).Error; err != nil {
		t.Fatal(err)
	}
	w := f.client.do("GET", path, nil, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), next.UTC().Format(time.RFC3339)) {
		t.Fatal(w.Code, w.Body.String())
	}
	added := models.EventOccurrence{EventID: f.event.ID, OriginalDate: calDate(next.AddDate(0, 0, 1).Format("2006-01-02")), StartsAt: next.AddDate(0, 0, 1), EndsAt: next.AddDate(0, 0, 1).Add(time.Hour), Status: models.OccurrenceScheduled}
	if err := f.app.DB.Create(&added).Error; err != nil {
		t.Fatal(err)
	}
	apiDo(t, f.client, "GET", dateReadPath(f.event.ID, models.FormatCalendarDate(added.OriginalDate)), nil, 404)
	// A block does not cancel a different already accepted date.
	if err := f.app.DB.Create(&models.ManagerAddBlock{SubjectUserID: f.recipient.ID, OrganizerType: "group", OrganizerID: f.group}).Error; err != nil {
		t.Fatal(err)
	}
	apiDo(t, f.client, "GET", path, nil, http.StatusOK)
	// Regenerating the same date key creates a different occurrence, not a copy
	// of the old invitation; retained access still names the deleted stable ID.
	// The grant ends with its date before the row can go: a live grant is never deleted.
	endSeededAccess(t, f.app.DB, f.access.ID, policy.OccurrenceDeleted, nil, time.Now().UTC())
	if err := f.app.DB.Where("occurrence_id = ?", f.dates[1].ID).Delete(&models.Registration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.app.DB.Delete(&f.dates[1]).Error; err != nil {
		t.Fatal(err)
	}
	replacement := models.EventOccurrence{EventID: f.event.ID, OriginalDate: f.dates[1].OriginalDate, StartsAt: next, EndsAt: next.Add(time.Hour), Status: models.OccurrenceScheduled}
	if err := f.app.DB.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	apiDo(t, f.client, "GET", path, nil, 404)
}

func TestOccurrenceDateReadTeenConsentAndVideoAreCurrent(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	teen := newTestUser(t, f.app, "invited teen", "password1234", models.RoleMember)
	birth := time.Now().UTC().AddDate(-15, 0, 0)
	if err := f.app.DB.Model(teen).Updates(map[string]any{"account_type": models.AccountTeen, "birth_date": birth}).Error; err != nil {
		t.Fatal(err)
	}
	granted := models.TeenConsent{TeenUserID: teen.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: f.issuer.ID, GranterRole: models.ConsentAdmin, EvidenceText: "Written guardian consent", At: time.Now().Add(-time.Hour)}
	if err := f.app.DB.Create(&granted).Error; err != nil {
		t.Fatal(err)
	}
	seedReadInvitation(t, f.app, f.issuer, teen, f.event, f.dates[0])
	client := loginAPI(t, f.app, teen.Name, "password1234")
	path := dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[0].OriginalDate))
	apiDo(t, client, "GET", path, nil, 200)
	// Fail closed even if status cleanup has not yet changed the active user.
	revoked := models.TeenConsent{TeenUserID: teen.ID, Scope: models.TeenConsentAccount, Status: models.ConsentRevoked, GrantedBy: f.issuer.ID, GranterRole: models.ConsentAdmin, EvidenceText: "Written guardian consent", At: time.Now()}
	if err := f.app.DB.Create(&revoked).Error; err != nil {
		t.Fatal(err)
	}
	apiDo(t, client, "GET", path, nil, 404)
	reads, err := f.app.scopedOccurrenceReads(teen, f.app.DB.Where("event_id = ?", f.event.ID), time.Now().UTC())
	if err != nil || len(reads) != 0 {
		t.Fatal("revoked consent", err, reads)
	}
	// Shared group membership or an original manager is not inherited video
	// authority for an override-only recipient; only this date's Going reveals.
	if err := f.app.DB.Model(&models.Registration{}).Where("id = ?", f.access.RegistrationID).Update("status", models.RegMaybe).Error; err != nil {
		t.Fatal(err)
	}
	w := f.client.do("GET", dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate)), nil, false)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-video") || !strings.Contains(w.Body.String(), `"canSeeVideo":false`) || !strings.Contains(w.Body.String(), `"videoHidden":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	raw := string(events.RenderFeed(mustBuildFeed(t, f.app, f.recipient, feedOptions{personal: true, scope: models.CalendarFeedAll}, time.Now().UTC())))
	if strings.Contains(raw, "private-video") {
		t.Fatal("feed inherited video authority", raw)
	}
	if !strings.Contains(raw, "STATUS:TENTATIVE") || strings.Contains(raw, "STATUS:CONFIRMED") {
		t.Fatal("Maybe invitation must remain tentative in the personal feed")
	}
}

func TestOccurrenceDateReadPendingAskAndRegistrationAreNotAuthority(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	now := time.Now().UTC()
	req := models.ContactRequest{RequesterID: f.issuer.ID, SubjectUserID: f.other.ID, Purpose: "occurrence_add", ContextType: "event", ContextID: f.event.ID, OccurrenceID: &f.dates[0].ID, Fields: "", AddAttendanceMode: models.AttendanceInPerson, SubjectDecision: "pending", AdminDecision: "not_needed", Status: "pending", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 14)}
	if err := f.app.DB.Create(&req).Error; err != nil {
		t.Fatal(err)
	}
	reg := models.Registration{UserID: &f.other.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, Source: models.RegSourceAdmin, AttendanceMode: models.AttendanceInPerson}
	if err := f.app.DB.Create(&reg).Error; err != nil {
		t.Fatal(err)
	}
	apiDo(t, f.stranger, "GET", dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[0].OriginalDate)), nil, 404)
	page, raw := getCalendar(t, f.stranger, "from="+models.FormatCalendarDate(f.dates[0].OriginalDate)+"&to="+models.FormatCalendarDate(f.dates[2].OriginalDate)+"&mine=1", 200)
	if len(page.Occurrences) != 0 {
		t.Fatal("pending ask or registration widened calendar", raw)
	}
	feed := mustBuildFeed(t, f.app, f.other, feedOptions{personal: true, scope: models.CalendarFeedAll}, now)
	if len(feed.Singles) != 0 {
		t.Fatal("pending ask or registration widened feed", feed)
	}
}

func TestOccurrenceInvitationBatchEndReasonsAndFailsClosed(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	now := time.Now().UTC()
	// The grant is a property of the registration, so a synthetic variant is a
	// registration whose subject or date no longer matches the live schema.
	variants := func() []models.Registration {
		var base models.Registration
		if err := f.app.DB.First(&base, f.access.ID).Error; err != nil {
			t.Fatal(err)
		}
		rows := []models.Registration{base}
		subject, missing := uint(999999), uint(999999)
		bySubject, byDate := base, base
		bySubject.UserID = &subject
		byDate.OccurrenceID = missing
		return append(rows, bySubject, byDate)
	}
	rows := variants()
	// compare checks what the loader says ends each variant: the live grant
	// (want0), a grant whose subject account is gone, and one whose date is
	// gone (want2). Each is also the answer liveOccurrenceInvitationIn gives
	// for the same row, one at a time.
	compare := func(want0, want2 string) {
		t.Helper()
		rows = variants()
		facts, err := loadOccurrenceInvitationFacts(f.app.DB, rows, now)
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range []string{want0, policy.ContactAccountDisabled, want2} {
			if got := facts.endReason(rows[i], now); got != want {
				t.Fatalf("variant %d ends for %q, want %q", i, got, want)
			}
		}
		for _, row := range rows {
			live, err := liveOccurrenceInvitationIn(f.app.DB, row, now)
			if err != nil {
				t.Fatal(err)
			} // duplicate IDs in synthetic variants are checked individually.
			single, err := occurrenceInvitationReadsIn(f.app.DB, []models.Registration{row}, now)
			if err != nil || (single[row.ID] != nil) != live {
				t.Fatalf("invitation parity %v %v", live, err)
			}
		}
	}
	compare("", policy.OccurrenceDeleted)
	birth := now.AddDate(-15, 0, 0)
	if err := f.app.DB.Model(f.recipient).Updates(map[string]any{"account_type": models.AccountTeen, "birth_date": birth}).Error; err != nil {
		t.Fatal(err)
	}
	compare(policy.OccurrenceConsentLost, policy.OccurrenceConsentLost) // no current account consent: the teen is excluded
	if err := f.app.DB.Create(&models.TeenConsent{TeenUserID: f.recipient.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: f.issuer.ID, GranterRole: "admin", EvidenceText: "parent consent", At: now.Add(-time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	compare("", policy.OccurrenceDeleted)
	n := collectionReadCounter(t, f.app.DB)
	*n = 0
	if _, err := loadOccurrenceInvitationFacts(f.app.DB, rows[:1], now); err != nil {
		t.Fatal(err)
	}
	small := *n
	many := make([]models.Registration, 100)
	for i := range many {
		many[i] = rows[0]
	}
	*n = 0
	if _, err := loadOccurrenceInvitationFacts(f.app.DB, many, now); err != nil {
		t.Fatal(err)
	}
	if *n != small || *n > 6 {
		t.Fatalf("fact reads %d->%d budget6", small, *n)
	}
	t.Logf("teen lifecycle fact reads 1=%d 100=%d", small, *n)
	if err := f.app.DB.Model(&f.dates[1]).Update("status", models.OccurrenceCancelled).Error; err != nil {
		t.Fatal(err)
	}
	cancelled := ""
	if rows[0].OccurrenceID == f.dates[1].ID {
		cancelled = policy.OccurrenceCancelled
	}
	compare(cancelled, policy.OccurrenceDeleted)
	if err := f.app.DB.Model(f.recipient).Update("status", models.UserDisabled).Error; err != nil {
		t.Fatal(err)
	}
	compare(policy.ContactAccountDisabled, policy.ContactAccountDisabled)
	// The batch reads the dates, events and groups; the lifecycle sweep also
	// reads the live grants. Either failing is an error, never an empty answer.
	for _, tc := range []struct {
		table string
		run   func() error
	}{
		{"event_occurrences", func() error { _, err := occurrenceInvitationReadsIn(f.app.DB, rows, now); return err }},
		{"registrations", func() error {
			err := f.app.reconcileOccurrenceAccesses(f.app.DB, now, contactChange{EventID: f.event.ID})
			return err
		}},
	} {
		name := "invitation:fail:" + tc.table
		_ = f.app.DB.Callback().Query().Before("gorm:query").Register(name, func(db *gorm.DB) {
			if db.Statement.Table == tc.table {
				db.AddError(fmt.Errorf("injected provenance read failure"))
			}
		})
		err := tc.run()
		f.app.DB.Callback().Query().Remove(name)
		if err == nil {
			t.Fatalf("%s read error ignored", tc.table)
		}
	}
}

func TestDepartureOccurrenceAccessRetainsOnlyTheSanitizedDate(t *testing.T) {
	for _, tc := range []struct {
		visibility string
		status     string
		origin     string
	}{
		{models.VisibilityPrivate, models.RegGoing, policy.OccurrenceOriginDepartureSelf},
		{models.VisibilityInviteOnly, models.RegMaybe, policy.OccurrenceOriginDepartureRemoval},
		{models.VisibilityInviteOnly, models.RegWaitlisted, policy.OccurrenceOriginDepartureSelf},
	} {
		t.Run(tc.visibility+"/"+tc.status+"/"+tc.origin, func(t *testing.T) {
			f := newDepartureReadFixture(t, tc.visibility, tc.status, tc.origin)
			if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
				t.Fatal("membership departure ended retained access")
			}
			if err := f.app.DB.Create(&models.ManagerAddBlock{SubjectUserID: f.recipient.ID, OrganizerType: policy.ContactGroup, OrganizerID: f.group}).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.app.sweepContacts(t.Context(), time.Now().UTC()); err != nil || savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
				t.Fatal("standing departure access was ended by a manager-add block", err)
			}
			path := dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate))
			w := f.client.do("GET", path, nil, false)
			if w.Code != http.StatusOK {
				t.Fatal(w.Code, w.Body.String())
			}
			raw := w.Body.String()
			for _, forbidden := range []string{"Retained hidden group", "Retained hidden series", "departure actor", "issuer", "departureActor", "groupId", "recurrenceRule", "attendees\"", "canManage"} {
				if strings.Contains(raw, forbidden) {
					t.Fatalf("retained date leaked %q: %s", forbidden, raw)
				}
			}
			if !strings.Contains(raw, `"title":"Retained date title"`) || !strings.Contains(raw, `"location":"Retained date place"`) || !strings.Contains(raw, `"myRegistration":"`+tc.status+`"`) {
				t.Fatal("sanitized retained date incomplete", raw)
			}
			for _, hidden := range []string{models.FormatCalendarDate(f.dates[0].OriginalDate), models.FormatCalendarDate(f.dates[2].OriginalDate)} {
				apiDo(t, f.client, "GET", dateReadPath(f.event.ID, hidden), nil, http.StatusNotFound)
			}
			apiDo(t, f.client, "GET", fmt.Sprintf("/api/v1/events/%d", f.event.ID), nil, http.StatusNotFound)
			if tc.visibility == models.VisibilityInviteOnly {
				for _, parent := range []string{fmt.Sprintf("/api/v1/groups/%d", f.group), fmt.Sprintf("/api/v1/groups/%d/members", f.group), fmt.Sprintf("/api/v1/groups/%d/events", f.group)} {
					apiDo(t, f.client, "GET", parent, nil, http.StatusNotFound)
				}
			}

			page, calendarRaw := getCalendar(t, f.client, "from="+models.FormatCalendarDate(f.dates[0].OriginalDate)+"&to="+models.FormatCalendarDate(f.dates[2].OriginalDate)+"&tz=UTC&mine=1", http.StatusOK)
			if len(page.Occurrences) != 1 || page.Occurrences[0].Date != models.FormatCalendarDate(f.dates[1].OriginalDate) || page.Occurrences[0].MyStatus == nil || *page.Occurrences[0].MyStatus != tc.status {
				t.Fatalf("retained calendar=%+v raw=%s", page.Occurrences, calendarRaw)
			}
			if strings.Contains(calendarRaw, "Retained hidden group") || strings.Contains(calendarRaw, "Retained hidden series") || strings.Contains(calendarRaw, "groupId") {
				t.Fatal("calendar leaked retained parent", calendarRaw)
			}
			feed := mustBuildFeed(t, f.app, f.recipient, feedOptions{personal: true, scope: models.CalendarFeedMine}, time.Now().UTC())
			feedRaw := string(events.RenderFeed(feed))
			if len(feed.Singles) != 1 || !strings.Contains(feedRaw, "Retained date title") || strings.Contains(feedRaw, "Retained hidden group") || strings.Contains(feedRaw, "Retained hidden series") {
				t.Fatalf("retained personal feed widened: %s", feedRaw)
			}
			publicRaw := string(events.RenderFeed(mustBuildFeed(t, f.app, nil, feedOptions{name: "Public"}, time.Now().UTC())))
			gid := f.group
			groupRaw := string(events.RenderFeed(mustBuildFeed(t, f.app, nil, feedOptions{name: "Group", onlyGroupID: &gid}, time.Now().UTC())))
			if strings.Contains(publicRaw+groupRaw, "Retained date title") {
				t.Fatal("exact-date authority changed a public/group feed")
			}

			apiDo(t, f.client, "PUT", path+"/registration", map[string]any{"status": models.RegGoing}, http.StatusOK)
			if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive || regStatus(t, f.app, f.recipient.ID, f.dates[1].ID) != models.RegGoing {
				t.Fatal("own RSVP did not retain departure authority")
			}
		})
	}
}

func TestDepartureOccurrenceAccessExactEndAndMoveCannotResurrect(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	boundary := f.dates[1].EndsAt
	before, err := occurrenceInvitationReadsIn(f.app.DB, f.grants(t), boundary.Add(-time.Nanosecond))
	if err != nil || before[f.access.ID] == nil {
		t.Fatal("departure access ended before boundary", err)
	}
	at, err := occurrenceInvitationReadsIn(f.app.DB, f.grants(t), boundary)
	if err != nil || at[f.access.ID] != nil {
		t.Fatal("departure access survived exact end", err, at)
	}
	if err := f.app.sweepContacts(t.Context(), boundary); err != nil {
		t.Fatal(err)
	}
	ended := savedOccurrenceAccess(t, f)
	if ended.Status != policy.OccurrenceAccessEnded || ended.EndReason != policy.OccurrenceEnded || ended.EndedAt == nil || !ended.EndedAt.Equal(boundary) {
		t.Fatalf("expiry not persisted at boundary: %+v", ended)
	}
	future := boundary.AddDate(0, 0, 30)
	if err := f.app.contactMutation(contactChange{EventID: f.event.ID}, func(tx *gorm.DB) error {
		return tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Updates(map[string]any{"starts_at": future, "ends_at": future.Add(time.Hour)}).Error
	}); err != nil {
		t.Fatal(err)
	}
	apiDo(t, f.client, "GET", dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate)), nil, http.StatusNotFound)
	if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessEnded {
		t.Fatal("reschedule resurrected retained access")
	}

	// Model a process that missed the daily sweep: reads close immediately at
	// the old end, and the event-edit transaction persists that expiry before
	// moving the occurrence into the future.
	missed := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureRemoval)
	past := time.Now().UTC().Add(-time.Hour)
	if err := missed.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", missed.dates[1].ID).Updates(map[string]any{"starts_at": past.Add(-time.Hour), "ends_at": past}).Error; err != nil {
		t.Fatal(err)
	}
	reads, err := missed.app.scopedOccurrenceReads(missed.recipient, missed.app.DB.Where("id = ?", missed.dates[1].ID), time.Now().UTC())
	if err != nil || len(reads) != 0 {
		t.Fatal("read-time expiry stayed open before persistence", err, reads)
	}
	future = time.Now().UTC().AddDate(0, 0, 30)
	if err := missed.app.contactMutation(contactChange{EventID: missed.event.ID}, func(tx *gorm.DB) error {
		return tx.Model(&models.EventOccurrence{}).Where("id = ?", missed.dates[1].ID).Updates(map[string]any{"starts_at": future, "ends_at": future.Add(time.Hour)}).Error
	}); err != nil {
		t.Fatal(err)
	}
	moved := savedOccurrenceAccess(t, missed)
	if moved.Status != policy.OccurrenceAccessEnded || moved.EndReason != policy.OccurrenceEnded {
		t.Fatalf("move resurrected unswept expiry: %+v", moved)
	}
}

func TestManagerOccurrenceAccessKeepsEstablishedPostDateSemantics(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	if err := f.app.sweepContacts(t.Context(), f.dates[1].EndsAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if savedOccurrenceAccess(t, f).Status != policy.OccurrenceAccessActive {
		t.Fatal("M1.14 retroactively expired a manager invitation")
	}
}

func TestOccurrenceDateOrganizerFollowsOrdinaryGroupVisibility(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	read := func() dateOccurrenceJSON {
		t.Helper()
		w := f.client.do("GET", dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate)), nil, false)
		if w.Code != http.StatusOK {
			t.Fatalf("read date: %d %s", w.Code, w.Body.String())
		}
		var body struct {
			Occurrence dateOccurrenceJSON `json:"occurrence"`
		}
		decodeJSON(t, w, &body)
		return body.Occurrence
	}
	// An exact-date invitation alone still carries no parent-group metadata.
	invited := read()
	if invited.Visibility != "" || invited.GroupID != nil || invited.GroupName != "" || invited.HostedBy != "" {
		t.Fatalf("invitation widened organizer disclosures: %+v", invited)
	}
	// Opening this date does not open the group or the rest of the series.
	if err := f.app.DB.Model(&f.dates[1]).Update("visibility_override", models.VisibilityPublic).Error; err != nil {
		t.Fatal(err)
	}
	opened := read()
	if opened.Visibility != models.VisibilityPublic || opened.GroupID != nil || opened.GroupName != "" || opened.HostedBy != f.app.siteName() {
		t.Fatalf("hidden group must retain the church fallback: %+v", opened)
	}
	apiDo(t, f.client, "GET", fmt.Sprintf("/api/v1/groups/%d", f.group), nil, http.StatusNotFound)
	apiDo(t, f.client, "GET", fmt.Sprintf("/api/v1/events/%d", f.event.ID), nil, http.StatusNotFound)
	// Independent group membership permits attribution, even while the same
	// person also has an exact-date invitation. The invite itself grants none.
	membership := models.Membership{UserID: f.recipient.ID, GroupID: f.group, Role: models.MembershipMember, Status: "active"}
	if err := f.app.DB.Create(&membership).Error; err != nil {
		t.Fatal(err)
	}
	member := read()
	if member.GroupID == nil || *member.GroupID != f.group || member.GroupName != "Hidden group identity" || member.HostedBy != "" || !member.CanDecline {
		t.Fatalf("ordinary group member organizer: %+v", member)
	}
}

func TestOccurrenceDateChurchWideOrganizer(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	w := f.admin.do("POST", "/api/v1/events", map[string]any{
		"title": "Church gathering", "startsLocal": feedStart(10).Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create church event: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	var date models.EventOccurrence
	if err := f.app.DB.Where("event_id = ?", created.Event.ID).Order("starts_at").First(&date).Error; err != nil {
		t.Fatal(err)
	}
	w = newAPIClient(t, f.app).do("GET", dateReadPath(created.Event.ID, models.FormatCalendarDate(date.OriginalDate)), nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("public church date: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Occurrence dateOccurrenceJSON `json:"occurrence"`
	}
	raw := w.Body.String()
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if row := body.Occurrence; row.HostedBy != f.app.siteName() || row.GroupID != nil || row.GroupName != "" {
		t.Fatalf("church-wide organizer: %+v", row)
	}
	// The ordinary reader gets the date's own facts under the church label:
	// no cover and no named host (M8.1 brief 1, 2.3).
	if row := body.Occurrence; row.GroupHeader != nil || row.Attendance == nil || row.Seats == nil || row.Series == nil {
		t.Fatalf("church-wide date facts: %s", raw)
	}
	for _, forbidden := range []string{"groupHeader", f.issuer.Name, "hosts"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("church-wide date carries %q: %s", forbidden, raw)
		}
	}
}

// The one-date download (DEC-93) is the date page in another format: the same
// 404s, the same video rule, and the feeds' UID.
func TestOccurrenceCalendarFollowsTheDatePage(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	path := dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[1].OriginalDate)) + "/calendar.ics"
	for name, client := range map[string]*apiClient{"stranger": f.stranger, "signed out": newAPIClient(t, f.app)} {
		if w := client.do("GET", path, nil, false); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "VCALENDAR") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := f.client.do("GET", dateReadPath(f.event.ID, models.FormatCalendarDate(f.dates[0].OriginalDate))+"/calendar.ics", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("another date of the hidden series: %d", w.Code)
	}

	w := f.client.do("GET", path, nil, false)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/calendar") ||
		w.Header().Get("Content-Disposition") != `attachment; filename="event-`+models.FormatCalendarDate(f.dates[1].OriginalDate)+`.ics"` {
		t.Fatalf("download: %d %v", w.Code, w.Header())
	}
	for _, want := range []string{"BEGIN:VEVENT", "SUMMARY:Invited date title", "LOCATION:Invited date place", "private-video", "STATUS:CONFIRMED"} {
		if !strings.Contains(body, want) {
			t.Fatalf("going download lacks %q:\n%s", want, body)
		}
	}
	for _, leak := range []string{"Hidden series title", "Hidden group identity", "X-WR-CALNAME"} {
		if strings.Contains(body, leak) {
			t.Fatalf("download leaks %q:\n%s", leak, body)
		}
	}
	feed := string(events.RenderFeed(mustBuildFeed(t, f.app, f.recipient, feedOptions{personal: true, scope: models.CalendarFeedAll}, time.Now().UTC())))
	uid := regexp.MustCompile(`UID:[^\r\n]+`).FindString(body)
	if uid == "" || !strings.Contains(feed, uid) {
		t.Fatalf("UID %q differs from the personal feed:\n%s", uid, feed)
	}

	if err := f.app.DB.Model(&models.Registration{}).Where("id = ?", f.access.RegistrationID).Update("status", models.RegMaybe).Error; err != nil {
		t.Fatal(err)
	}
	body = f.client.do("GET", path, nil, false).Body.String()
	if strings.Contains(body, "private-video") || !strings.Contains(body, "STATUS:TENTATIVE") {
		t.Fatalf("maybe download:\n%s", body)
	}
}

func TestOccurrenceCalendarPublicDateSignedOut(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	w := f.admin.do("POST", "/api/v1/events", map[string]any{
		"title": "Church picnic", "startsLocal": feedStart(10).Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create church event: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	decodeJSON(t, w, &created)
	var date models.EventOccurrence
	if err := f.app.DB.Where("event_id = ?", created.Event.ID).First(&date).Error; err != nil {
		t.Fatal(err)
	}
	w = newAPIClient(t, f.app).do("GET", dateReadPath(created.Event.ID, models.FormatCalendarDate(date.OriginalDate))+"/calendar.ics", nil, false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "SUMMARY:Church picnic") || strings.Contains(w.Body.String(), "STATUS:") {
		t.Fatalf("public date: %d %s", w.Code, w.Body.String())
	}
}

// One table runs the shared corruption predicate through the Go loader and
// through the direct-SQL trigger: a malformed access snapshot anywhere in the
// two actions and the registration target fails the grant closed, even when its
// subject and date cannot match; valid rows are filtered normally; rows outside
// the scope do not take part.
func TestAccessSnapshotCorruptionPredicateGoAndSQLAgree(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	reg := models.Registration{UserID: &f.other.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, Source: models.RegSourceAdmin, AttendanceMode: models.AttendanceInPerson}
	if err := f.app.DB.Create(&reg).Error; err != nil {
		t.Fatal(err)
	}
	const (
		added, ended = "occurrence_access.added", "occurrence_access.ended"
		big          = "9223372036854775807"
		over         = "9223372036854775808"
	)
	valid := `"subjectUserId":5,"occurrenceId":6,"grantedAtMicro":1788000000000000`
	other := fmt.Sprintf(`{%s,"endedAtMicro":1788000000000001}`, valid)
	for _, tc := range []struct {
		name, action, target, meta string
		corrupt, repeat            bool
	}{
		{"missing subject", added, "registration", `{"occurrenceId":6,"grantedAtMicro":1}`, true, false},
		{"missing occurrence", added, "registration", `{"subjectUserId":5,"grantedAtMicro":1}`, true, false},
		{"missing grant stamp", added, "registration", `{"subjectUserId":5,"occurrenceId":6}`, true, false},
		{"ended without an end stamp", ended, "registration", `{` + valid + `}`, true, false},
		{"null subject", added, "registration", `{"subjectUserId":null,"occurrenceId":6,"grantedAtMicro":1}`, true, false},
		{"string subject", added, "registration", `{"subjectUserId":"5","occurrenceId":6,"grantedAtMicro":1}`, true, false},
		{"boolean occurrence", added, "registration", `{"subjectUserId":5,"occurrenceId":true,"grantedAtMicro":1}`, true, false},
		{"object stamp", added, "registration", `{"subjectUserId":5,"occurrenceId":6,"grantedAtMicro":{"n":1}}`, true, false},
		{"array subject", added, "registration", `{"subjectUserId":[5],"occurrenceId":6,"grantedAtMicro":1}`, true, false},
		{"fractional subject", added, "registration", `{"subjectUserId":1.5,"occurrenceId":6,"grantedAtMicro":1}`, true, false},
		{"decimal-point integer", added, "registration", `{"subjectUserId":5.0,"occurrenceId":6,"grantedAtMicro":1}`, true, false},
		{"zero occurrence", added, "registration", `{"subjectUserId":5,"occurrenceId":0,"grantedAtMicro":1}`, true, false},
		{"negative stamp", added, "registration", `{"subjectUserId":5,"occurrenceId":6,"grantedAtMicro":-1}`, true, false},
		{"overflowing subject", added, "registration", `{"subjectUserId":` + over + `,"occurrenceId":6,"grantedAtMicro":1}`, true, false},
		{"huge end stamp", ended, "registration", `{` + valid + `,"endedAtMicro":99999999999999999999}`, true, false},
		{"array metadata", added, "registration", `[]`, true, false},
		{"scalar metadata", ended, "registration", `"text"`, true, false},
		{"boundary integers are valid", ended, "registration", `{"subjectUserId":` + big + `,"occurrenceId":` + big + `,"grantedAtMicro":` + big + `,"endedAtMicro":` + big + `}`, false, false},
		{"unrelated valid rows are filtered", added, "registration", `{` + valid + `}`, false, false},
		{"unrelated valid ended row", ended, "registration", other, false, false},
		{"another action is not access history", "registration.changed", "registration", `{"subjectUserId":"x"}`, false, false},
		{"a block action is not authority", "occurrence.blocked", "registration", `{"subjectUserId":"x"}`, false, false},
		{"another target type does not participate", added, "event", `{"subjectUserId":"x"}`, false, false},
		{"the old target label does not participate", added, "occurrence_access", `{"subjectUserId":"x"}`, false, false},
		{"a matching valid row makes the add a repeat", added, "registration", fmt.Sprintf(`{"subjectUserId":%d,"occurrenceId":%d,"grantedAtMicro":1788000000000000}`, f.other.ID, f.dates[0].ID), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := errors.New("roll back the probe")
			var goErr, sqlErr error
			var history map[accessKey]accessHistory
			_ = f.app.DB.Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec(`INSERT INTO audit_logs(action, target_type, target_id, meta_json) VALUES (?, ?, 1, ?::jsonb)`, tc.action, tc.target, tc.meta).Error; err != nil {
					t.Fatal(err)
				}
				history, goErr = loadAccessHistory(tx, []uint{f.other.ID}, []uint{f.dates[0].ID})
				sqlErr = tx.Exec(`UPDATE registrations SET access_origin = 'manager_add', access_issuer_id = ?, access_granted_at = now() WHERE id = ?`, f.issuer.ID, reg.ID).Error
				return stop
			})
			switch {
			case tc.corrupt:
				if !errors.Is(goErr, errAccessHistoryCorrupt) {
					t.Fatalf("Go loader accepted corrupt history: %v", goErr)
				}
				if sqlErr == nil || !strings.Contains(sqlErr.Error(), "history is corrupt") {
					t.Fatalf("trigger accepted corrupt history: %v", sqlErr)
				}
			case tc.repeat:
				if goErr != nil || !history[accessKey{f.other.ID, f.dates[0].ID}].Granted {
					t.Fatalf("Go loader missed the matching row: %v %+v", goErr, history)
				}
				if sqlErr == nil || !strings.Contains(sqlErr.Error(), "repeat manager add") {
					t.Fatalf("trigger did not treat the audited grant as a repeat: %v", sqlErr)
				}
			default:
				if goErr != nil || sqlErr != nil {
					t.Fatalf("valid or out-of-scope rows blocked the grant: go=%v sql=%v", goErr, sqlErr)
				}
			}
		})
	}
}

// The history loader is one scan: a corrupt snapshot fails closed even with no
// subject or date to match, and a valid snapshot for someone else is neither
// returned nor an error.
func TestLoadAccessHistoryOneScanFailsClosedAndFilters(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	stop := errors.New("roll back the probe")
	probe := func(meta string, subjects, occurrences []uint) (map[accessKey]accessHistory, error) {
		var history map[accessKey]accessHistory
		var err error
		_ = f.app.DB.Transaction(func(tx *gorm.DB) error {
			if meta != "" {
				if e := tx.Exec(`INSERT INTO audit_logs(action, target_type, target_id, meta_json) VALUES ('occurrence_access.added', 'registration', 1, ?::jsonb)`, meta).Error; e != nil {
					t.Fatal(e)
				}
			}
			history, err = loadAccessHistory(tx, subjects, occurrences)
			return stop
		})
		return history, err
	}
	if _, err := probe(`{"subjectUserId":"x","occurrenceId":6,"grantedAtMicro":1}`, nil, nil); !errors.Is(err, errAccessHistoryCorrupt) {
		t.Fatalf("an unrelated corrupt row with nothing to match: %v", err)
	}
	if got, err := probe(`{"subjectUserId":5,"occurrenceId":6,"grantedAtMicro":1}`, []uint{f.other.ID}, []uint{f.dates[0].ID}); err != nil || len(got) != 0 {
		t.Fatalf("a valid row for someone else: %v %+v", err, got)
	}
	if got, err := probe(`{"subjectUserId":5,"occurrenceId":6,"grantedAtMicro":1}`, nil, nil); err != nil || len(got) != 0 {
		t.Fatalf("a valid row with nothing to match: %v %+v", err, got)
	}
}

// tombstoneFixture is an invite-only group's weekly series. Its second date
// has its own title; dates holds the first three dates by start.
type tombstoneFixture struct {
	app       *App
	adminUser *models.User
	admin     *apiClient
	group     uint
	event     eventJSON
	dates     []models.EventOccurrence
}

const tombstoneTitle = "Cancelled date title"

func newTombstoneFixture(t *testing.T) tombstoneFixture {
	t.Helper()
	app := testApp(t)
	adminUser := newTestUser(t, app, "tombstone admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, adminUser.Name, "password1234")
	group := createFeedGroup(t, admin, "Tombstone hidden group", models.VisibilityInviteOnly, "Tombstone hidden place")
	start := feedStart(7)
	ev := createFeedEvent(t, admin, group, "Tombstone hidden series", start, weeklyRule(start))
	var dates []models.EventOccurrence
	if err := app.DB.Where("event_id = ?", ev.ID).Order("starts_at").Limit(3).Find(&dates).Error; err != nil || len(dates) != 3 {
		t.Fatal(err, len(dates))
	}
	gormMust(t, app.DB.Model(&dates[1]).Update("title_override", tombstoneTitle))
	return tombstoneFixture{app: app, adminUser: adminUser, admin: admin, group: group, event: ev, dates: dates}
}

func tombstoneDate(occ models.EventOccurrence) string {
	return models.FormatCalendarDate(occ.OriginalDate)
}

// member is a signed-in member of the group: ordinary access to every date.
func (f tombstoneFixture) member(t *testing.T, name string) (*models.User, *apiClient) {
	t.Helper()
	u := newTestUser(t, f.app, name, "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: u.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	return u, loginAPI(t, f.app, name, "password1234")
}

// invitee holds a manager-add grant on occ alone.
func (f tombstoneFixture) invitee(t *testing.T, name string, occ models.EventOccurrence) (*models.User, accessRow, *apiClient) {
	t.Helper()
	u := newTestUser(t, f.app, name, "password1234", models.RoleMember)
	access := seedReadInvitation(t, f.app, f.adminUser, u, f.event, occ)
	return u, access, loginAPI(t, f.app, name, "password1234")
}

// departed is a member who answered Going on occ and then left the group
// through the real departure preview and confirmation, keeping occ, so the
// departure grant and its provenance are the production ones (DEC-74). An
// invite-only date can be kept only inside the RSVP cutoff, so occ is first
// moved to start startIn from now (under events.CutoffWindow); the moved row
// is returned.
func (f tombstoneFixture) departed(t *testing.T, name string, occ models.EventOccurrence, startIn time.Duration) (accessRow, models.EventOccurrence, *apiClient) {
	t.Helper()
	if startIn <= 0 || startIn >= events.CutoffWindow {
		t.Fatalf("fixture: %s is outside the keep cutoff", startIn)
	}
	u, c := f.member(t, name)
	apiDo(t, c, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.event.ID), map[string]any{"date": tombstoneDate(occ), "status": models.RegGoing}, http.StatusOK)
	start := time.Now().UTC().Add(startIn).Truncate(time.Second)
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occ.ID).Updates(map[string]any{"starts_at": start, "ends_at": start.Add(time.Hour), "starts_at_override": start}))
	gormMust(t, f.app.DB.First(&occ, occ.ID))
	groupPath := fmt.Sprintf("/api/v1/groups/%d", f.group)
	preview := getDeparturePreview(t, c, groupPath+"/departure-preview", http.StatusOK)
	if w := confirmDeparturePreview(t, c, "POST", groupPath+"/leave", preview, []uint{occ.ID}, ""); w.Code != http.StatusOK {
		t.Fatalf("leave keeping %s: %d %s", tombstoneDate(occ), w.Code, w.Body.String())
	}
	var regs []models.Registration
	gormMust(t, f.app.DB.Where("user_id = ? AND occurrence_id = ?", u.ID, occ.ID).Find(&regs))
	if len(regs) != 1 || regs[0].Status != models.RegGoing {
		t.Fatalf("the departure changed the kept row: %+v", regs)
	}
	access := loadAccessRow(t, f.app.DB, regs[0].ID)
	if access.Origin != policy.OccurrenceOriginDepartureSelf || access.Status != policy.OccurrenceAccessActive || access.RegistrationID != regs[0].ID ||
		access.DepartureActorID == nil || *access.DepartureActorID != u.ID || access.DepartureMembershipID == nil {
		t.Fatalf("the departure did not keep %s: %+v", tombstoneDate(occ), access)
	}
	var membership models.Membership
	gormMust(t, f.app.DB.First(&membership, *access.DepartureMembershipID))
	if membership.Status != models.MembershipLeft || membership.UserID != u.ID {
		t.Fatalf("the departure did not mark the membership left: %+v", membership)
	}
	return access, occ, c
}

func (f tombstoneFixture) cancel(t *testing.T, occ models.EventOccurrence) {
	t.Helper()
	apiDo(t, f.admin, "POST", fmt.Sprintf("/api/v1/events/%d/cancel", f.event.ID), map[string]any{"date": tombstoneDate(occ)}, http.StatusOK)
}

// liveGrants counts grants still live on occ: a committed cancellation leaves none.
func (f tombstoneFixture) liveGrants(t *testing.T, occ models.EventOccurrence) int64 {
	t.Helper()
	var n int64
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("registrations.occurrence_id = ? AND "+registrationAccessLiveSQL, occ.ID).Count(&n))
	return n
}

// wantTombstone compares the whole answer: exactly the three facts.
func (f tombstoneFixture) wantTombstone(t *testing.T, c *apiClient, occ models.EventOccurrence, title string) {
	t.Helper()
	raw := apiDo(t, c, "GET", dateReadPath(f.event.ID, tombstoneDate(occ)), nil, http.StatusOK)
	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err, raw)
	}
	want := map[string]map[string]any{"occurrence": {"title": title, "date": tombstoneDate(occ), "cancelled": true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tombstone %s, want %v", raw, want)
	}
}

// wantUnavailable is the existing unavailable answer, byte for byte the one a
// date that never existed gets, and never the title or the cancellation.
func (f tombstoneFixture) wantUnavailable(t *testing.T, c *apiClient, date string) {
	t.Helper()
	missing := apiDo(t, c, "GET", dateReadPath(f.event.ID, "1999-01-01"), nil, http.StatusNotFound)
	raw := apiDo(t, c, "GET", dateReadPath(f.event.ID, date), nil, http.StatusNotFound)
	if raw != missing || strings.Contains(raw, tombstoneTitle) || strings.Contains(raw, "Tombstone hidden") || strings.Contains(raw, "cancelled") {
		t.Fatalf("unavailable answer %s differs from %s", raw, missing)
	}
}

func TestCancelledDateTombstoneQualifiedReadersAndDenials(t *testing.T) {
	f := newTombstoneFixture(t)
	target := f.dates[1]
	d := tombstoneDate(target)
	_, member := f.member(t, "tombstone member")
	apiDo(t, member, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.event.ID), map[string]any{"date": d, "status": models.RegGoing}, http.StatusOK)
	_, _, invitee := f.invitee(t, "tombstone invitee", target)
	_, _, departed := f.departed(t, "tombstone departed", target, 30*time.Minute)
	_, outsider := f.member(t, "tombstone unrelated member")
	newTestUser(t, f.app, "tombstone stranger", "password1234", models.RoleMember)
	stranger := loginAPI(t, f.app, "tombstone stranger", "password1234")
	anon := newAPIClient(t, f.app)

	// A retained row whose only authority was a grant ended for another
	// reason keeps its Going status and still shows nothing.
	ended, endedAccess, endedClient := f.invitee(t, "tombstone removed invitee", target)
	if err := f.app.contactMutation(contactChange{EventID: f.event.ID}, func(tx *gorm.DB) error {
		var row models.Registration
		if err := tx.First(&row, endedAccess.ID).Error; err != nil {
			return err
		}
		return endRegistrationAccessQuietly(tx, &row, accessScopeOf(&models.Event{ID: f.event.ID, GroupID: &f.group}), policy.OccurrenceRemoved, &f.adminUser.ID, time.Now().UTC(), "")
	}); err != nil {
		t.Fatal(err)
	}
	// A viewer whose only link is a household person's registration they hold.
	holder := newTestUser(t, f.app, "tombstone household holder", "password1234", models.RoleMember)
	household := models.Household{Name: "Tombstone household", CreatedBy: holder.ID}
	gormMust(t, f.app.DB.Create(&household))
	gormMust(t, f.app.DB.Create(&models.HouseholdAccount{HouseholdID: household.ID, UserID: holder.ID, Relation: models.HouseholdRelationAdult}))
	birth := time.Now().UTC().AddDate(-30, 0, 0)
	person := models.HouseholdPerson{HouseholdID: household.ID, FirstName: "Tombstone proxy", Kind: models.HouseholdAdult, BirthDate: &birth, CreatedBy: holder.ID}
	gormMust(t, f.app.DB.Create(&person))
	gormMust(t, f.app.DB.Create(&models.Registration{HouseholdPersonID: &person.ID, RegisteredBy: &holder.ID, OccurrenceID: target.ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson}))
	householdClient := loginAPI(t, f.app, holder.Name, "password1234")
	asked := newTestUser(t, f.app, "tombstone asked", "password1234", models.RoleMember)
	askedClient := loginAPI(t, f.app, asked.Name, "password1234")

	for _, c := range []*apiClient{member, invitee, departed} {
		apiDo(t, c, "GET", dateReadPath(f.event.ID, d), nil, http.StatusOK)
	}
	f.cancel(t, target)
	f.cancel(t, f.dates[2])
	if n := f.liveGrants(t, target); n != 0 {
		t.Fatalf("the date cancel left %d live grants", n)
	}
	if got := loadAccessRow(t, f.app.DB, endedAccess.ID); got.EndReason != policy.OccurrenceRemoved {
		t.Fatalf("an independently ended grant changed: %+v", got)
	}
	var reg models.Registration
	gormMust(t, f.app.DB.First(&reg, endedAccess.ID))
	if reg.Status != models.RegGoing || reg.UserID == nil || *reg.UserID != ended.ID {
		t.Fatalf("the retained row lost its status: %+v", reg)
	}
	// A pending ask alone is no authority. It is added after the cancel,
	// whose reconcile would otherwise end it, and removed before the delete.
	at := time.Now().UTC()
	ask := models.ContactRequest{RequesterID: f.adminUser.ID, SubjectUserID: asked.ID, Purpose: "occurrence_add", ContextType: "event", ContextID: f.event.ID, OccurrenceID: &target.ID,
		Fields: "", AddAttendanceMode: models.AttendanceInPerson, SubjectDecision: "pending", AdminDecision: "not_needed", Status: "pending", CreatedAt: at, ExpiresAt: at.AddDate(0, 0, 14)}
	gormMust(t, f.app.DB.Create(&ask))

	// Qualified: a registered reader, an invitee after cleanup and a
	// departure grant the cancel ended. The calendar download stays 404.
	for _, c := range []*apiClient{member, invitee, departed} {
		f.wantTombstone(t, c, target, tombstoneTitle)
		apiDo(t, c, "GET", dateReadPath(f.event.ID, d)+"/calendar.ics", nil, http.StatusNotFound)
	}
	// Denied: no own row (an unrelated member, a stranger, signed out, a
	// role alone), a pending ask, a retained row with an independently ended
	// grant and a household person's row. A row a visibility loss cancelled
	// has its own test.
	for _, c := range []*apiClient{outsider, stranger, anon, f.admin, askedClient, endedClient, householdClient} {
		f.wantUnavailable(t, c, d)
	}
	// Denied: sibling dates (scheduled and cancelled), an impossible date, a
	// date that never existed and a malformed one.
	for _, date := range []string{tombstoneDate(f.dates[0]), tombstoneDate(f.dates[2]), "2026-02-30", target.OriginalDate.AddDate(0, 0, 1).Format("2006-01-02")} {
		f.wantUnavailable(t, invitee, date)
	}
	apiDo(t, member, "GET", dateReadPath(f.event.ID, "not-a-date"), nil, http.StatusNotFound)
	// Discovery and the series stay on their scheduled-date authority.
	apiDo(t, invitee, "GET", fmt.Sprintf("/api/v1/events/%d", f.event.ID), nil, http.StatusNotFound)

	// Deleted dates: deleting the event removes them, and the tombstone with them.
	gormMust(t, f.app.DB.Delete(&models.ContactRequest{}, ask.ID))
	apiDo(t, f.admin, "DELETE", fmt.Sprintf("/api/v1/events/%d", f.event.ID), nil, http.StatusOK)
	for _, c := range []*apiClient{member, invitee, departed} {
		f.wantUnavailable(t, c, d)
	}
}

// A registration a visibility tightening cancelled (cancelDateLosers) stays
// cancelled history: it shows nothing once the date is cancelled, even after
// the person has ordinary access again.
func TestCancelledDateTombstoneDeniesARowAVisibilityLossCancelled(t *testing.T) {
	f := newTombstoneFixture(t)
	target := f.dates[0]
	d := tombstoneDate(target)
	datePath := fmt.Sprintf("/api/v1/events/%d/occurrences/%s", f.event.ID, d)
	apiDo(t, f.admin, "PUT", datePath, map[string]any{"visibility": models.VisibilityPublic}, http.StatusOK)
	loser := newTestUser(t, f.app, "tombstone visibility loser", "password1234", models.RoleMember)
	client := loginAPI(t, f.app, loser.Name, "password1234")
	apiDo(t, client, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", f.event.ID), map[string]any{"date": d, "status": models.RegGoing}, http.StatusOK)
	var registered models.Registration
	gormMust(t, f.app.DB.Where("user_id = ? AND occurrence_id = ?", loser.ID, target.ID).First(&registered))
	if registered.Status != models.RegGoing {
		t.Fatalf("fixture: %+v", registered)
	}
	// The confirmed switch back to invite-only shuts the person out and
	// cancels this same row.
	apiDo(t, f.admin, "PUT", datePath, map[string]any{"visibility": models.VisibilityInviteOnly, "confirm": true}, http.StatusOK)
	var lost models.Registration
	gormMust(t, f.app.DB.First(&lost, registered.ID))
	if lost.Status != models.RegCancelled || lost.UserID == nil || *lost.UserID != loser.ID {
		t.Fatalf("the tightening did not cancel the row: %+v", lost)
	}
	apiDo(t, f.admin, "PUT", datePath, map[string]any{"title": "Tombstone hidden private title"}, http.StatusOK)
	f.cancel(t, target)
	f.wantUnavailable(t, client, d)
	// Ordinary access again, the row still cancelled: the retained row is
	// not enough.
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: loser.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	f.wantUnavailable(t, client, d)
	gormMust(t, f.app.DB.First(&lost, registered.ID))
	if lost.Status != models.RegCancelled {
		t.Fatalf("the read changed the row: %+v", lost)
	}
}

func TestCancelledDateTombstoneLiveGrantBeforeCleanup(t *testing.T) {
	f := newTombstoneFixture(t)
	_, _, invitee := f.invitee(t, "unstamped invitee", f.dates[1])
	_, target, departed := f.departed(t, "unstamped departed", f.dates[1], 30*time.Minute)
	// No writer commits a cancelled date with a live grant: flip the status
	// alone to stand for the moment before the cancel's cleanup.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{"status": models.OccurrenceCancelled, "cancel_source": models.CancelSourceManual}))
	if n := f.liveGrants(t, target); n != 2 {
		t.Fatalf("fixture: %d live grants", n)
	}
	f.wantTombstone(t, invitee, target, tombstoneTitle)
	f.wantTombstone(t, departed, target, tombstoneTitle)
	// After the date's end the records cannot tell whether a departure grant
	// expired before the cancellation; a manager add has no expiry.
	after := target.EndsAt.Add(time.Hour)
	f.app.clock = func() time.Time { return after }
	f.wantUnavailable(t, departed, tombstoneDate(target))
	f.wantTombstone(t, invitee, target, tombstoneTitle)
}

func TestCancelledDateTombstoneOutlivesTheDateAndARetime(t *testing.T) {
	f := newTombstoneFixture(t)
	access, target, departed := f.departed(t, "retimed departed", f.dates[1], 30*time.Minute)
	f.cancel(t, target)
	stamped := loadAccessRow(t, f.app.DB, access.ID)
	if stamped.EndReason != policy.OccurrenceCancelled || stamped.EndedAt == nil || stamped.Origin != policy.OccurrenceOriginDepartureSelf || stamped.RegistrationID != access.RegistrationID {
		t.Fatalf("the cancel did not stamp the grant: %+v", stamped)
	}
	f.app.clock = func() time.Time { return target.EndsAt.Add(time.Hour) }
	f.wantTombstone(t, departed, target, tombstoneTitle)
	f.app.clock = nil
	// The per-date editor moves the cancelled date to end before the stamp.
	earlier := stamped.EndedAt.Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04")
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d/occurrences/%s", f.event.ID, tombstoneDate(target)), map[string]any{"startsLocal": earlier}, http.StatusOK)
	var moved models.EventOccurrence
	gormMust(t, f.app.DB.First(&moved, target.ID))
	if !moved.EndsAt.Before(*stamped.EndedAt) || moved.Status != models.OccurrenceCancelled {
		t.Fatalf("fixture: retime %+v", moved)
	}
	f.wantTombstone(t, departed, target, tombstoneTitle)
}

func TestCancelledDateTombstoneClockBoundary(t *testing.T) {
	f := newTombstoneFixture(t)
	early, before, earlyClient := f.departed(t, "boundary before", f.dates[1], 30*time.Minute)
	late, at, lateClient := f.departed(t, "boundary at", f.dates[2], time.Hour)
	f.app.clock = func() time.Time { return before.EndsAt.Add(-time.Microsecond) }
	f.cancel(t, before)
	if got := loadAccessRow(t, f.app.DB, early.ID); got.EndReason != policy.OccurrenceCancelled {
		t.Fatalf("a cancel just before the end: %+v", got)
	}
	f.wantTombstone(t, earlyClient, before, tombstoneTitle)

	// At the end the manual cancel is not refused: its first reconcile, with
	// the date still scheduled, ends the departure grant as expired, and the
	// cancel then commits with no grant of its own to stamp.
	f.app.clock = func() time.Time { return at.EndsAt }
	f.cancel(t, at)
	if got := loadAccessRow(t, f.app.DB, late.ID); got.EndReason != policy.OccurrenceEnded || got.EndedAt == nil {
		t.Fatalf("a cancel at the end: %+v", got)
	}
	var cancelled models.EventOccurrence
	gormMust(t, f.app.DB.First(&cancelled, at.ID))
	if cancelled.Status != models.OccurrenceCancelled {
		t.Fatalf("the cancel at the end did not commit: %+v", cancelled)
	}
	f.wantUnavailable(t, lateClient, tombstoneDate(at))
}

func TestCancelledDateTombstoneFollowsTheCancellationThatEndedTheGrant(t *testing.T) {
	f := newTombstoneFixture(t)
	target := f.dates[1]
	d := tombstoneDate(target)
	// A series edit keeps a date with an override of its own, so this one
	// has none: the series cancels and restores it, under the series title.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("title_override", nil))
	const title = "Tombstone hidden series"
	restorerUser := newTestUser(t, f.app, "tombstone restorer", "password1234", models.RoleAdmin)
	restorer := loginAPI(t, f.app, restorerUser.Name, "password1234")
	_, first, firstClient := f.invitee(t, "restored invitee", target)
	regranted, again, againClient := f.invitee(t, "regranted invitee", target)
	eventPath := fmt.Sprintf("/api/v1/events/%d", f.event.ID)
	// The series edit cancels the date and stamps both grants.
	apiDo(t, f.admin, "PUT", eventPath, map[string]any{"rruleUntil": tombstoneDate(f.dates[0])}, http.StatusOK)
	if n := f.liveGrants(t, target); n != 0 {
		t.Fatalf("the series cancel left %d live grants", n)
	}
	f.wantTombstone(t, firstClient, target, title)
	f.wantTombstone(t, againClient, target, title)
	// Another admin's real series restore writes event.dates_restored.
	apiDo(t, restorer, "PUT", eventPath, map[string]any{"rruleUntil": ""}, http.StatusOK)
	var restore models.AuditLog
	gormMust(t, f.app.DB.Where("action = ? AND target_type = 'event' AND target_id = ?", audit.EventDatesRestored, f.event.ID).First(&restore))
	var back models.EventOccurrence
	gormMust(t, f.app.DB.First(&back, target.ID))
	if back.Status != models.OccurrenceScheduled || restore.ActorID == nil || *restore.ActorID != restorerUser.ID || !strings.Contains(restore.MetaJSON, itoa(target.ID)) {
		t.Fatalf("fixture: the restore did not bring the date back: %+v %+v", back, restore)
	}
	// The second recipient is invited again on the same registration: a
	// fresh request, approved by them. The first gets no new grant.
	ask := createOccurrenceAsk(t, occurrenceReadFixture{app: f.app, issuer: f.adminUser, admin: f.admin, event: f.event}, regranted, target, nil)
	apiDo(t, againClient, "POST", "/api/v1/add-requests/"+itoa(ask.ID)+"/decision", map[string]string{"party": "subject", "decision": "approved"}, http.StatusOK)
	var rows []models.Registration
	gormMust(t, f.app.DB.Where("user_id = ? AND occurrence_id = ?", regranted.ID, target.ID).Find(&rows))
	if live := loadAccessRow(t, f.app.DB, again.ID); len(rows) != 1 || rows[0].ID != again.ID ||
		live.Status != policy.OccurrenceAccessActive || live.RequestID == nil || *live.RequestID != ask.ID {
		t.Fatalf("the reinvitation did not regrant the same row: %+v %+v", rows, live)
	}
	f.cancel(t, target)
	// That registration now has two end snapshots, one each side of the
	// restore: the reader has to pick the one matching the current grant.
	var ends []accessSnapshot
	for _, s := range accessSnapshotsFor(t, f.app.DB, again.ID) {
		if s.Action == audit.OccurrenceAccessEnded {
			ends = append(ends, s)
		}
	}
	if len(ends) != 2 || ends[0].ID >= restore.ID || ends[1].ID <= restore.ID ||
		metaUint(ends[0].Meta["grantedAtMicro"]) == metaUint(ends[1].Meta["grantedAtMicro"]) || ends[1].Meta["endReason"] != policy.OccurrenceCancelled {
		t.Fatalf("end snapshots around restore %d: %+v", restore.ID, ends)
	}
	// The first grant ended under the cancellation the restore undid; the
	// regranted one ended under this one.
	check := func(stage string) {
		t.Helper()
		f.wantUnavailable(t, firstClient, d)
		f.wantTombstone(t, againClient, target, title)
		if got := loadAccessRow(t, f.app.DB, first.ID); got.EndReason != policy.OccurrenceCancelled {
			t.Fatalf("%s: first grant %+v", stage, got)
		}
	}
	check("recancelled")
	// The restore's at moves before the first grant's end (a restore that
	// queued behind the cancellation): ids decide, so nothing changes.
	firstEnd := loadAccessRow(t, f.app.DB, first.ID).EndedAt
	gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("id = ?", restore.ID).Update("at", firstEnd.Add(-time.Hour)))
	check("restore at moved earlier")
	// Erasing the restorer through the real admin delete keeps the restore
	// record's id and dates.
	var idsBefore string
	gormMust(t, f.app.DB.Raw("SELECT (meta_json->'occurrenceIds')::text FROM audit_logs WHERE id = ?", restore.ID).Scan(&idsBefore))
	apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(restorerUser.ID), nil, http.StatusOK)
	var erased models.User
	gormMust(t, f.app.DB.Unscoped().First(&erased, restorerUser.ID))
	var kept struct {
		Action   string
		TargetID uint
		IDs      *string
	}
	gormMust(t, f.app.DB.Raw("SELECT action, target_id, (meta_json->'occurrenceIds')::text AS ids FROM audit_logs WHERE id = ?", restore.ID).Scan(&kept))
	ids, ok := restoredOccurrenceIDs(kept.IDs)
	if !erased.DeletedAt.Valid || kept.Action != audit.EventDatesRestored || kept.TargetID != f.event.ID || kept.IDs == nil || *kept.IDs != idsBefore ||
		!ok || !slices.Contains(ids, target.ID) {
		t.Fatalf("restore record after erasing its actor: %+v %v (deleted %v)", kept, idsBefore, erased.DeletedAt.Valid)
	}
	check("restorer erased")
	// A restore record whose list cannot be read denies every ended grant on
	// the event: read as a zero, the null would hide this date's restore and
	// pass the first grant.
	gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("id = ?", restore.ID).Update("meta_json", gorm.Expr(`jsonb_set(meta_json, '{occurrenceIds}', '[null]')`)))
	f.wantUnavailable(t, firstClient, d)
	f.wantUnavailable(t, againClient, d)
}

func TestCancelledDateTombstoneEvidenceReader(t *testing.T) {
	f := newTombstoneFixture(t)
	target := f.dates[1]
	d := tombstoneDate(target)
	_, dup, dupClient := f.invitee(t, "duplicated snapshot", target)
	_, corrupt, corruptClient := f.invitee(t, "corrupt snapshot", target)
	_, missing, missingClient := f.invitee(t, "missing snapshot", target)
	_, healthy, healthyClient := f.invitee(t, "healthy snapshot", target)
	f.cancel(t, target)
	for _, c := range []*apiClient{dupClient, corruptClient, missingClient, healthyClient} {
		f.wantTombstone(t, c, target, tombstoneTitle)
	}

	// A failed query at either evidence lookup, with the other one and every
	// earlier read succeeding, answers the error, never the unavailable
	// response; so does one earlier in the snapshot. The failure is added to
	// that one statement alone and leaves the transaction healthy, so a reader
	// that dropped it would commit and answer 404 or the tombstone instead.
	var armed atomic.Pointer[string]
	var failed string
	inject := func(kind string) func(*gorm.DB) {
		return func(db *gorm.DB) {
			want := armed.Load()
			if want == nil {
				return
			}
			statement := kind + ":" + db.Statement.Table + ":" + db.Statement.SQL.String()
			if strings.Contains(statement, *want) && armed.CompareAndSwap(want, nil) {
				failed = statement
				db.AddError(errors.New("injected tombstone lookup failure"))
			}
		}
	}
	if err := f.app.DB.Callback().Row().Before("gorm:row").Register("tombstone_lookup_failure_row", inject("row")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.app.DB.Callback().Row().Remove("tombstone_lookup_failure_row") })
	if err := f.app.DB.Callback().Query().Before("gorm:query").Register("tombstone_lookup_failure_query", inject("query")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove("tombstone_lookup_failure_query") })
	for _, c := range []struct{ step, statement string }{
		{"snapshots", "endReason"},
		{"restores", "occurrenceIds"},
		{"date", "query:registrations:"},
	} {
		failed = ""
		f.app.hooks.cancelledDateStep = func(at string) {
			if at == c.step {
				armed.Store(&c.statement)
			}
		}
		apiDo(t, healthyClient, "GET", dateReadPath(f.event.ID, d), nil, http.StatusInternalServerError)
		armed.Store(nil)
		if failed == "" {
			t.Fatalf("the read never ran its %s lookup after reaching that step", c.step)
		}
	}
	f.app.hooks.cancelledDateStep = nil
	f.wantTombstone(t, healthyClient, target, tombstoneTitle)

	ended := func(registrationID uint) models.AuditLog {
		t.Helper()
		var row models.AuditLog
		gormMust(t, f.app.DB.Where("action = ? AND target_type = ? AND target_id = ?", audit.OccurrenceAccessEnded, accessTargetType, registrationID).First(&row))
		return row
	}
	copyOf := func(row models.AuditLog, meta string) {
		t.Helper()
		gormMust(t, f.app.DB.Exec("INSERT INTO audit_logs (actor_id, action, target_type, target_id, meta_json, ip, at) SELECT actor_id, action, target_type, target_id, "+meta+", ip, at FROM audit_logs WHERE id = ?", row.ID))
	}
	copyOf(ended(dup.ID), "meta_json")
	copyOf(ended(corrupt.ID), "meta_json - 'grantedAtMicro'")
	gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("id = ?", ended(missing.ID).ID).Update("meta_json", gorm.Expr("jsonb_set(meta_json, '{endReason}', '\"declined\"')")))
	// A corrupt snapshot on another registration leaves this read alone.
	gormMust(t, f.app.DB.Create(&models.AuditLog{Action: audit.OccurrenceAccessEnded, TargetType: accessTargetType, TargetID: healthy.ID + 1000, MetaJSON: `{"occurrenceId":"corrupt"}`}))
	for _, c := range []*apiClient{dupClient, corruptClient, missingClient} {
		f.wantUnavailable(t, c, d)
	}
	f.wantTombstone(t, healthyClient, target, tombstoneTitle)

	// A restore record for the event whose dates cannot be read is a denial,
	// whatever it names; one that reads as a list of other dates is not.
	restore := models.AuditLog{Action: audit.EventDatesRestored, TargetType: "event", TargetID: f.event.ID, MetaJSON: `{"dates":[]}`}
	gormMust(t, f.app.DB.Create(&restore))
	id := itoa(target.ID)
	for _, ids := range []string{"", `"not a list"`, `null`, `[]`, `[null]`, `[0]`, `[-1]`, `[1.5]`, `["` + id + `"]`, `[` + id + `, null]`, `{"id":` + id + `}`} {
		meta := `{"dates":[]}`
		if ids != "" {
			meta = `{"dates":[],"occurrenceIds":` + ids + `}`
		}
		gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("id = ?", restore.ID).Update("meta_json", gorm.Expr("?::jsonb", meta)))
		f.wantUnavailable(t, healthyClient, d)
	}
	gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("id = ?", restore.ID).Update("meta_json", gorm.Expr("?::jsonb", `{"dates":[],"occurrenceIds":[`+itoa(f.dates[2].ID)+`]}`)))
	f.wantTombstone(t, healthyClient, target, tombstoneTitle)
}

// The tombstone's facts come from one snapshot: the account and the series
// title changed together by another connection after the snapshot's first
// read reach neither this answer nor its decision.
func TestCancelledDateTombstoneIsOneSnapshot(t *testing.T) {
	f := newTombstoneFixture(t)
	target := f.dates[1]
	// An inherited title, so the title is read after the snapshot is fixed.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("title_override", nil))
	const series = "Tombstone hidden series"
	invitee, _, client := f.invitee(t, "snapshot invitee", target)
	f.cancel(t, target)
	f.wantTombstone(t, client, target, series)
	fired := false
	f.app.hooks.cancelledDateStep = func(step string) {
		if step != "date" || fired {
			return
		}
		fired = true
		if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.User{}).Where("id = ?", invitee.ID).Update("status", models.UserDisabled).Error; err != nil {
				return err
			}
			return tx.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("title", "Tombstone hidden series renamed mid-read").Error
		}); err != nil {
			t.Error("commit during the read:", err)
		}
	}
	f.wantTombstone(t, client, target, series)
	f.app.hooks.cancelledDateStep = nil
	if !fired {
		t.Fatal("the read never reached its snapshot")
	}
	f.wantUnavailable(t, client, tombstoneDate(target))
}

// A session ended after the middleware read the account but before the
// snapshot reads like a signed-out visitor, with the account still active.
func TestCancelledDateTombstoneRechecksTheSessionInItsSnapshot(t *testing.T) {
	f := newTombstoneFixture(t)
	target := f.dates[1]
	invitee, _, client := f.invitee(t, "session invitee", target)
	elsewhere := loginAPI(t, f.app, invitee.Name, "password1234")
	f.cancel(t, target)
	f.wantTombstone(t, client, target, tombstoneTitle)
	missing := apiDo(t, client, "GET", dateReadPath(f.event.ID, "1999-01-01"), nil, http.StatusNotFound)
	fired := false
	f.app.hooks.cancelledDateStep = func(step string) {
		if step != "begin" || fired {
			return
		}
		fired = true
		// Signing out on another device ends every session of the account.
		apiDo(t, elsewhere, "POST", "/api/v1/auth/logout", nil, http.StatusOK)
	}
	raw := apiDo(t, client, "GET", dateReadPath(f.event.ID, tombstoneDate(target)), nil, http.StatusNotFound)
	f.app.hooks.cancelledDateStep = nil
	if !fired || raw != missing {
		t.Fatalf("a session ended before the snapshot got %s (hook ran: %v), want %s", raw, fired, missing)
	}
	var fresh models.User
	gormMust(t, f.app.DB.First(&fresh, invitee.ID))
	if fresh.Status != models.UserActive || !fresh.CanSignIn() || fresh.LogoutVersion == invitee.LogoutVersion {
		t.Fatalf("fixture: the account should be active with a new sign-out epoch: %+v", fresh)
	}
}

func TestCancelledDateTombstoneRechecksConsentAndContext(t *testing.T) {
	f := newTombstoneFixture(t)
	target := f.dates[1]
	d := tombstoneDate(target)
	teen := newTestUser(t, f.app, "tombstone teen", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(teen).Updates(map[string]any{"account_type": models.AccountTeen, "birth_date": time.Now().UTC().AddDate(-15, 0, 0)}))
	gormMust(t, f.app.DB.Create(&models.TeenConsent{TeenUserID: teen.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: f.adminUser.ID, GranterRole: models.ConsentAdmin, EvidenceText: "Written guardian consent", At: time.Now().Add(-time.Hour)}))
	seedReadInvitation(t, f.app, f.adminUser, teen, f.event, target)
	teenClient := loginAPI(t, f.app, teen.Name, "password1234")
	_, _, adult := f.invitee(t, "tombstone context invitee", target)
	f.cancel(t, target)
	f.wantTombstone(t, teenClient, target, tombstoneTitle)
	f.wantTombstone(t, adult, target, tombstoneTitle)
	// The end reason stays occurrence_cancelled; the current checks still apply.
	gormMust(t, f.app.DB.Create(&models.TeenConsent{TeenUserID: teen.ID, Scope: models.TeenConsentAccount, Status: models.ConsentRevoked, GrantedBy: f.adminUser.ID, GranterRole: models.ConsentAdmin, At: time.Now()}))
	f.wantUnavailable(t, teenClient, d)
	gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("status", models.GroupArchived))
	f.wantUnavailable(t, adult, d)
}

// datePageFixture is one public group's weekly event with every kind of date
// page fact (M8.1 brief sections 4 and 8): limited in-person, remote and
// childcare pools, a waitlist, guests, a registered-only link, and a third date
// that is invite-only (a hidden sibling for anyone outside the group) with
// remote answers closed, where an outsider holds only an exact-date invitation.
type datePageFixture struct {
	app                                             *App
	admin, leader, member, outsider, waiter, remote *models.User
	invitee                                         *models.User
	group                                           models.Group
	ev                                              models.Event
	dates                                           []models.EventOccurrence
	as                                              map[string]*apiClient
}

func newDatePageFixture(t *testing.T) *datePageFixture {
	t.Helper()
	app := testApp(t)
	f := &datePageFixture{app: app, as: map[string]*apiClient{}}
	user := func(name, role string) *models.User {
		u := newTestUser(t, app, name, "password1234", role)
		f.as[name] = loginAPI(t, app, name, "password1234")
		return u
	}
	f.admin = user("Dana Admin", models.RoleAdmin)
	f.leader = user("Lou Leader", models.RoleMember)
	f.member = user("Max Member", models.RoleMember)
	f.outsider = user("Olive Outsider", models.RoleMember)
	f.waiter = user("Walt Waiter", models.RoleMember)
	f.remote = user("Rae Remote", models.RoleMember)
	f.invitee = user("Ina Invitee", models.RoleMember)
	decliner := user("Del Decliner", models.RoleMember)
	f.as["signed out"] = anonClient(t, app, "")

	f.group = activeGroup(t, app, f.leader, "Visible Circle", models.VisibilityPublic, "")
	gormMust(t, app.DB.Model(&f.group).Updates(map[string]any{"location": "Circle Hall", "cover_focal_x": 30, "cover_focal_y": 70}))
	addMember(t, app, f.member, &f.group, models.MembershipMember)

	capacity, remote, care := 3, 2, 1
	start := feedStart(7)
	f.ev = models.Event{GroupID: &f.group.ID, Title: "Circle Night", Visibility: models.VisibilityPublic,
		StartsLocal: start.Format(events.WallLayout), DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY;COUNT=4", CreatedBy: f.admin.ID,
		Capacity: &capacity, WaitlistEnabled: true, GuestsAllowed: 2, RemoteAvailable: true, RemoteCapacity: &remote,
		ChildcareAvailable: true, ChildcareCapacity: &care, VideoProvider: models.VideoProviderLink,
		VideoURL: "https://example.com/circle-call", VideoReveal: models.VideoRevealRegisteredOnly}
	gormMust(t, app.DB.Create(&f.ev))
	for i := 0; i < 4; i++ {
		s := start.AddDate(0, 0, 7*i)
		occ := models.EventOccurrence{EventID: f.ev.ID, OriginalDate: calDate(s.Format("2006-01-02")), StartsAt: s, EndsAt: s.Add(time.Hour), Status: models.OccurrenceScheduled}
		gormMust(t, app.DB.Create(&occ))
		f.dates = append(f.dates, occ)
	}
	recordClocks(t, app.DB, f.ev, f.dates...)
	gormMust(t, app.DB.Model(&f.dates[2]).Updates(map[string]any{"visibility_override": models.VisibilityInviteOnly, "remote_capacity_override": 0}))

	reg := func(u *models.User, occ models.EventOccurrence, status, mode string, guests, children int) {
		r := models.Registration{UserID: &u.ID, OccurrenceID: occ.ID, Status: status, GuestCount: guests, ChildcareCount: children, Source: models.RegSourceWeb, AttendanceMode: mode}
		if status == models.RegWaitlisted {
			at := time.Now().UTC().Add(-time.Hour)
			r.WaitlistedAt = &at
		}
		gormMust(t, app.DB.Create(&r))
	}
	// The first date: three Going rows using 3 of 3 in-person seats and 1 of 2
	// remote seats, the one childcare place taken, one Maybe, one waiting,
	// one declined.
	reg(f.member, f.dates[0], models.RegGoing, models.AttendanceInPerson, 1, 1)
	reg(f.outsider, f.dates[0], models.RegGoing, models.AttendanceInPerson, 0, 0)
	reg(f.remote, f.dates[0], models.RegGoing, models.AttendanceRemote, 0, 0)
	reg(f.leader, f.dates[0], models.RegMaybe, models.AttendanceInPerson, 0, 0)
	reg(f.waiter, f.dates[0], models.RegWaitlisted, models.AttendanceInPerson, 0, 0)
	reg(decliner, f.dates[0], models.RegDeclined, models.AttendanceInPerson, 0, 0)
	// Coverage: the outsider's answer on the hidden sibling must count for
	// nothing; the member is Going on the closed-remote third date.
	reg(f.outsider, f.dates[1], models.RegMaybe, models.AttendanceInPerson, 0, 0)
	reg(f.outsider, f.dates[2], models.RegGoing, models.AttendanceInPerson, 0, 0)
	reg(f.member, f.dates[2], models.RegGoing, models.AttendanceInPerson, 0, 0)
	seedReadInvitation(t, app, f.admin, f.invitee, eventJSON{ID: f.ev.ID}, f.dates[2])
	return f
}

// read is one viewer's date page, typed and as raw keys.
func (f *datePageFixture) read(t *testing.T, viewer string, occ models.EventOccurrence) (dateOccurrenceJSON, map[string]json.RawMessage, string) {
	t.Helper()
	raw := apiDo(t, f.as[viewer], "GET", dateReadPath(f.ev.ID, tombstoneDate(occ)), nil, http.StatusOK)
	var typed struct {
		Occurrence dateOccurrenceJSON `json:"occurrence"`
	}
	var keys struct {
		Occurrence map[string]json.RawMessage `json:"occurrence"`
	}
	if err := json.Unmarshal([]byte(raw), &typed); err != nil {
		t.Fatal(err, raw)
	}
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		t.Fatal(err, raw)
	}
	return typed.Occurrence, keys.Occurrence, raw
}

func attendeeSet(list []dateAttendeeJSON) map[string]string {
	out := map[string]string{}
	for _, a := range list {
		out[a.Name] = a.Status
	}
	return out
}

func wantPool(t *testing.T, label string, got *datePoolJSON, capacity *int, used int, left *int) {
	t.Helper()
	if got == nil || !intPtrEqual(got.Capacity, capacity) || got.Used != used || !intPtrEqual(got.Left, left) {
		t.Fatalf("%s pool %+v, want capacity %v used %d left %v", label, got, capacity, used, left)
	}
}

var datePageKeys = []string{"groupHeader", "attendance", "seats", "remoteRsvp", "myWaitlistPosition", "series"}

func TestDatePageFactsForEachReader(t *testing.T) {
	f := newDatePageFixture(t)
	first := f.dates[0]

	t.Run("ordinary member", func(t *testing.T) {
		d, keys, raw := f.read(t, "Max Member", first)
		if d.GroupHeader == nil || *d.GroupHeader != (dateGroupHeaderJSON{ImageURL: "", CoverFocalX: 30, CoverFocalY: 70, LeaderName: "Lou Leader"}) || d.GroupID == nil || d.HostedBy != "" {
			t.Fatalf("a shown group's header: %+v %s", d.GroupHeader, raw)
		}
		if !reflect.DeepEqual(d.Attendance.Registrations, map[string]int64{models.RegGoing: 3, models.RegMaybe: 1, models.RegWaitlisted: 1}) {
			t.Fatalf("row counts, no declined: %v", d.Attendance.Registrations)
		}
		if got := attendeeSet(d.Attendance.Attendees); !reflect.DeepEqual(got, map[string]string{"Max Member": models.RegGoing, "Olive Outsider": models.RegGoing, "Rae Remote": models.RegGoing, "Lou Leader": models.RegMaybe}) {
			t.Fatalf("Going and Maybe names: %v", got)
		}
		for _, forbidden := range []string{"Walt Waiter", "Del Decliner", "@", "email", "phone", "careNotes", "registrationId", "childcareCount"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("the name summary carries %q: %s", forbidden, raw)
			}
		}
		// Seats are not rows: three Going rows hold 3 in-person seats (one
		// guest) and 1 remote seat.
		wantPool(t, "in person", &d.Seats.InPerson, intPtr(3), 3, intPtr(0))
		wantPool(t, "remote", d.Seats.Remote, intPtr(2), 1, intPtr(1))
		if !d.Seats.WaitlistEnabled || d.Seats.Childcare == nil || !d.Seats.Childcare.Full || d.Seats.Childcare.Totals != nil {
			t.Fatalf("childcare full without the manager's totals: %+v", d.Seats.Childcare)
		}
		if d.RemoteRSVP != remoteRSVPOffered || d.MyWaitlistPosition != nil || d.VideoURL != "https://example.com/circle-call" {
			t.Fatalf("remote offered, and a Going member's link: %s", raw)
		}
		s := d.Series
		if s == nil || s.RecurrenceRule == "" || s.UpcomingDates == nil || *s.UpcomingDates != 4 || s.CoveredDates == nil || *s.CoveredDates != 2 || !s.CanAnswerSeries {
			t.Fatalf("series coverage over all four dates the member may answer: %+v", s)
		}
		if s.Usual == nil || len(s.Changes) != 0 || s.Usual.Location != "Circle Hall" || s.Usual.StartsLocal != d.StartsLocal {
			t.Fatalf("an unchanged date: %+v", s)
		}
		if _, ok := keys["myWaitlistPosition"]; ok {
			t.Fatal("a waitlist position without a waitlisted answer")
		}
	})

	t.Run("managers", func(t *testing.T) {
		for _, viewer := range []string{"Dana Admin", "Lou Leader"} {
			d, _, raw := f.read(t, viewer, first)
			if d.Attendance.Registrations[models.RegDeclined] != 1 || len(d.Attendance.Registrations) != 4 {
				t.Fatalf("%s: declined count for managers: %v", viewer, d.Attendance.Registrations)
			}
			if strings.Contains(raw, "Del Decliner") {
				t.Fatalf("%s: the summary names only Going and Maybe: %s", viewer, raw)
			}
			if d.Seats.Childcare == nil {
				t.Fatalf("%s: no childcare: %s", viewer, raw)
			}
			wantPool(t, viewer+" childcare", d.Seats.Childcare.Totals, intPtr(1), 1, intPtr(0))
		}
		// An ordinary manager sees a registered-only link with no answer.
		if d, _, _ := f.read(t, "Dana Admin", first); d.VideoURL != "https://example.com/circle-call" || !d.CanSeeVideo {
			t.Fatalf("manager link: %+v", d)
		}
	})

	t.Run("waitlisted reader gets their own position", func(t *testing.T) {
		d, _, _ := f.read(t, "Walt Waiter", first)
		if d.MyRegistration == nil || *d.MyRegistration != models.RegWaitlisted || d.MyWaitlistPosition == nil || *d.MyWaitlistPosition != 1 {
			t.Fatalf("own waitlist position: %+v", d)
		}
		if *d.Series.CoveredDates != 0 {
			t.Fatalf("waiting is not coverage: %+v", d.Series)
		}
	})

	t.Run("a hidden sibling moves neither coverage count", func(t *testing.T) {
		d, _, _ := f.read(t, "Olive Outsider", first)
		if *d.Series.UpcomingDates != 3 || *d.Series.CoveredDates != 2 {
			t.Fatalf("outsider coverage %d of %d, want 2 of 3 (the invite-only date and its Going answer left out)", *d.Series.CoveredDates, *d.Series.UpcomingDates)
		}
		apiDo(t, f.as["Olive Outsider"], "GET", dateReadPath(f.ev.ID, tombstoneDate(f.dates[2])), nil, http.StatusNotFound)
	})

	t.Run("signed out", func(t *testing.T) {
		d, keys, raw := f.read(t, "signed out", first)
		if d.CanSeeAttendees || d.Attendance == nil || d.Attendance.Attendees != nil || strings.Contains(raw, "Max Member") || strings.Contains(raw, "Del Decliner") {
			t.Fatalf("counts without names: %s", raw)
		}
		if _, ok := d.Attendance.Registrations[models.RegDeclined]; ok || d.Seats.Childcare.Totals != nil || d.MyRegistration != nil {
			t.Fatalf("no manager data or answer: %s", raw)
		}
		if d.Series == nil || d.Series.UpcomingDates != nil || d.Series.CoveredDates != nil || d.Series.CanAnswerSeries {
			t.Fatalf("no coverage or series action signed out: %s", raw)
		}
		if _, ok := keys["myWaitlistPosition"]; ok || d.VideoURL != "" || !d.VideoHidden || d.RemoteRSVP != remoteRSVPOffered {
			t.Fatalf("a registered-only link waits for Going: %s", raw)
		}
	})

	t.Run("remote closed for the date while the link stays visible", func(t *testing.T) {
		d, _, raw := f.read(t, "Max Member", f.dates[2])
		if d.RemoteRSVP != remoteRSVPClosed || d.RemoteAvailable || d.VideoURL != "https://example.com/circle-call" {
			t.Fatalf("closed remote with a Going member's link: %s", raw)
		}
		wantPool(t, "closed remote", d.Seats.Remote, intPtr(0), 0, intPtr(0))
		if !reflect.DeepEqual(d.Series.Changes, []string{changeRemoteCapacity, changeVisibility}) {
			t.Fatalf("closed, invite-only date changes: %v", d.Series.Changes)
		}
	})

	t.Run("invitation only keeps today's payload", func(t *testing.T) {
		_, keys, raw := f.read(t, "Ina Invitee", f.dates[2])
		got := []string{}
		for key := range keys {
			got = append(got, key)
		}
		sort.Strings(got)
		want := strings.Fields("canBlock canDecline canSeeAttendees canSeeVideo date dateUrl durationMinutes endsAt endsLocal eventId guestsAllowed id location myAttendanceMode myGuestCount myRegistration remoteAvailable startsAt startsLocal title tzid videoHidden videoProvider videoUrl")
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("invitation keys %v, want %v", got, want)
		}
		for _, forbidden := range append([]string{"Visible Circle", "Lou Leader", "Max Member", "Circle Hall", "hostedBy", "closed"}, datePageKeys...) {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("invitation-only read carries %q: %s", forbidden, raw)
			}
		}
		if !strings.Contains(raw, `"remoteAvailable":false`) || !strings.Contains(raw, "circle-call") {
			t.Fatalf("invitation's remote and video fields: %s", raw)
		}
	})

	t.Run("the calendar download carries none of it", func(t *testing.T) {
		raw := apiDo(t, f.as["Max Member"], "GET", dateReadPath(f.ev.ID, tombstoneDate(first))+"/calendar.ics", nil, http.StatusOK)
		for _, forbidden := range []string{"Olive Outsider", "Lou Leader", "FREQ", "Rae Remote"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("calendar.ics carries %q: %s", forbidden, raw)
			}
		}
	})
}

// An opened date of a hidden series, and an invitation to a hidden group's
// date with remote answers closed (M8.1 brief 4, 8).
func TestDatePageHiddenGroupDates(t *testing.T) {
	app := testApp(t)
	admin := newTestUser(t, app, "Hidden admin", "password1234", models.RoleAdmin)
	leader := newTestUser(t, app, "Quiet Leader", "password1234", models.RoleMember)
	member := newTestUser(t, app, "Quiet Member", "password1234", models.RoleMember)
	reader := newTestUser(t, app, "Open Reader", "password1234", models.RoleMember)
	invitee := newTestUser(t, app, "Quiet Invitee", "password1234", models.RoleMember)
	group := activeGroup(t, app, leader, "Quiet Circle", models.VisibilityInviteOnly, "")
	gormMust(t, app.DB.Model(&group).Updates(map[string]any{"location": "Secret Loft", "image_url": "https://example.com/quiet.jpg"}))
	addMember(t, app, member, &group, models.MembershipMember)
	remote := 5
	start := feedStart(7)
	ev := models.Event{GroupID: &group.ID, Title: "Quiet Night", Visibility: models.VisibilityInviteOnly, StartsLocal: start.Format(events.WallLayout),
		DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY;COUNT=3", CreatedBy: admin.ID, RemoteAvailable: true, RemoteCapacity: &remote,
		ChildcareAvailable: true, VideoProvider: models.VideoProviderLink, VideoURL: "https://example.com/quiet-call", VideoReveal: models.VideoRevealRegisteredOnly}
	gormMust(t, app.DB.Create(&ev))
	var dates []models.EventOccurrence
	for i := 0; i < 3; i++ {
		s := start.AddDate(0, 0, 7*i)
		occ := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate(s.Format("2006-01-02")), StartsAt: s, EndsAt: s.Add(time.Hour), Status: models.OccurrenceScheduled}
		gormMust(t, app.DB.Create(&occ))
		dates = append(dates, occ)
	}
	gormMust(t, app.DB.Model(&dates[0]).Update("visibility_override", models.VisibilityPublic))
	gormMust(t, app.DB.Model(&dates[1]).Update("remote_capacity_override", 0))
	gormMust(t, app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: dates[0].ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson}))
	seedReadInvitation(t, app, admin, invitee, eventJSON{ID: ev.ID}, dates[1])
	secrets := []string{"Quiet Circle", "Secret Loft", "quiet.jpg", "Quiet Leader", "Quiet Member", "groupId", "groupHeader", "series", "recurrenceRule", "totals"}

	for _, viewer := range []string{"Open Reader", "signed out"} {
		c := anonClient(t, app, "")
		if viewer != "signed out" {
			c = loginAPI(t, app, reader.Name, "password1234")
		}
		raw := apiDo(t, c, "GET", dateReadPath(ev.ID, tombstoneDate(dates[0])), nil, http.StatusOK)
		var body struct {
			Occurrence dateOccurrenceJSON `json:"occurrence"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		d := body.Occurrence
		for _, forbidden := range secrets {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("%s: the opened date carries %q: %s", viewer, forbidden, raw)
			}
		}
		if d.HostedBy == "" || d.CanSeeAttendees || d.Attendance == nil || d.Attendance.Registrations[models.RegGoing] != 1 || d.Seats == nil || d.RemoteRSVP != remoteRSVPOffered {
			t.Fatalf("%s: the opened date's own facts under the church label: %s", viewer, raw)
		}
		if d.Seats.Childcare == nil || d.Seats.Childcare.Full {
			t.Fatalf("%s: unlimited childcare is available, never full: %s", viewer, raw)
		}
		// Unlimited is null, never full: one Going seat in an uncapped pool.
		wantPool(t, viewer+" unlimited in person", &d.Seats.InPerson, nil, 1, nil)
	}

	c := loginAPI(t, app, invitee.Name, "password1234")
	raw := apiDo(t, c, "GET", dateReadPath(ev.ID, tombstoneDate(dates[1])), nil, http.StatusOK)
	for _, forbidden := range append(append([]string{"hostedBy", "closed"}, secrets...), datePageKeys...) {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("invitation to a hidden group's date carries %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, `"remoteAvailable":false`) || !strings.Contains(raw, "quiet-call") || !strings.Contains(raw, `"myRegistration":"going"`) {
		t.Fatalf("today's remote and video fields: %s", raw)
	}
	// The group's own member reads the closed date with the series.
	m := loginAPI(t, app, member.Name, "password1234")
	raw = apiDo(t, m, "GET", dateReadPath(ev.ID, tombstoneDate(dates[1])), nil, http.StatusOK)
	if !strings.Contains(raw, `"remoteRsvp":"closed"`) || !strings.Contains(raw, `"imageUrl":"https://example.com/quiet.jpg"`) || !strings.Contains(raw, `"leaderName":"Quiet Leader"`) {
		t.Fatalf("member view of the closed date: %s", raw)
	}
}

// A failed count or seat read answers 500, never a date with zero usage, and
// the calendar download, which reads neither, still works.
func TestDatePageCountErrorsFail(t *testing.T) {
	f := newDatePageFixture(t)
	path := dateReadPath(f.ev.ID, tombstoneDate(f.dates[0]))
	for name, marker := range map[string]string{"seats": "SUM(1 + registrations.guest_count)", "rows": "count(*) as n", "childcare": "SUM(registrations.childcare_count"} {
		t.Run(name, func(t *testing.T) {
			failed := 0
			hook := "test:fail_date_page_" + name
			if err := f.app.DB.Callback().Row().Before("gorm:row").Register(hook, func(tx *gorm.DB) {
				if strings.Contains(strings.Join(tx.Statement.Selects, " "), marker) {
					failed++
					_ = tx.AddError(errors.New("count unavailable"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.app.DB.Callback().Row().Remove(hook) })
			for _, viewer := range []string{"Max Member", "Dana Admin", "signed out"} {
				w := f.as[viewer].do("GET", path, nil, false)
				if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"used"`) {
					t.Fatalf("%s: a failed %s read answered %d %s", viewer, name, w.Code, w.Body.String())
				}
			}
			if failed == 0 {
				t.Fatalf("the %s read was not exercised", name)
			}
			apiDo(t, f.as["Max Member"], "GET", path+"/calendar.ics", nil, http.StatusOK)
		})
	}
}

// The scheduled date page reads from one snapshot: the member's removal and
// the event made invite-only, committed with a new Going answer after the read
// authorized the date, reach neither the read's decisions nor its names or
// counts, so no answer pairs the old permissions with the new roster. The next
// read sees all of it.
func TestDatePageIsOneSnapshot(t *testing.T) {
	f := newDatePageFixture(t)
	first := f.dates[0]
	late := newTestUser(t, f.app, "Lena Late", "password1234", models.RoleMember)
	fired := false
	f.app.hooks.scheduledDateStep = func(step string) {
		if step != "authorized" || fired {
			return
		}
		fired = true
		if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.member.ID, f.group.ID).Update("status", models.MembershipRemoved).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
				return err
			}
			return tx.Create(&models.Registration{UserID: &late.ID, OccurrenceID: first.ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson}).Error
		}); err != nil {
			t.Error("commit during the read:", err)
		}
	}
	d, _, raw := f.read(t, "Max Member", first)
	f.app.hooks.scheduledDateStep = nil
	if !fired {
		t.Fatal("the read never reached its authorization")
	}
	if strings.Contains(raw, "Lena Late") || d.Attendance == nil || d.Attendance.Registrations[models.RegGoing] != 3 || d.Seats == nil || d.Seats.InPerson.Used != 3 {
		t.Fatalf("a roster committed after the read's authorization reached it: %s", raw)
	}
	if got := attendeeSet(d.Attendance.Attendees); got["Max Member"] != models.RegGoing || len(got) != 4 || d.Visibility != models.VisibilityPublic {
		t.Fatalf("the read's own snapshot: %s", raw)
	}
	apiDo(t, f.as["Max Member"], "GET", dateReadPath(f.ev.ID, tombstoneDate(first)), nil, http.StatusNotFound)
}

// A session ended after the middleware read the account but before the
// snapshot reads like a signed-out visitor, byte for byte.
func TestDatePageRechecksTheSessionInItsSnapshot(t *testing.T) {
	f := newDatePageFixture(t)
	first := f.dates[0]
	elsewhere := loginAPI(t, f.app, f.member.Name, "password1234")
	_, _, signedOut := f.read(t, "signed out", first)
	fired := false
	f.app.hooks.scheduledDateStep = func(step string) {
		if step != "begin" || fired {
			return
		}
		fired = true
		// Signing out on another device ends every session of the account.
		apiDo(t, elsewhere, "POST", "/api/v1/auth/logout", nil, http.StatusOK)
	}
	_, _, raw := f.read(t, "Max Member", first)
	f.app.hooks.scheduledDateStep = nil
	if !fired || raw != signedOut {
		t.Fatalf("a session ended before the snapshot got %s (hook ran: %v), want %s", raw, fired, signedOut)
	}
}

// A failed room read leaves the room out of the calendar download and of an
// invitation's date, as on the event pages; only an ordinary read whose series
// comparison needs the true room fails, never claiming a room change it could
// not see. The room statement fails inside PostgreSQL (an unknown table), so
// an ordinary one-time date's attendance and seats, read after it in the same
// transaction, load only because the savepoint rolled the failure back.
func TestDatePageRoomReadFailure(t *testing.T) {
	f := newDatePageFixture(t)
	capacity := 2
	start := feedStart(9)
	once := models.Event{GroupID: &f.group.ID, Title: "One Night", Visibility: models.VisibilityPublic, StartsLocal: start.Format(events.WallLayout),
		DurationMinutes: 60, TZID: "UTC", Capacity: &capacity, CreatedBy: f.admin.ID}
	gormMust(t, f.app.DB.Create(&once))
	onceDate := models.EventOccurrence{EventID: once.ID, OriginalDate: calDate(start.Format("2006-01-02")), StartsAt: start, EndsAt: start.Add(time.Hour), Status: models.OccurrenceScheduled}
	gormMust(t, f.app.DB.Create(&onceDate))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.member.ID, OccurrenceID: onceDate.ID, Status: models.RegGoing, Source: models.RegSourceWeb,
		AttendanceMode: models.AttendanceInPerson}))

	const missing = "rooms_unavailable_in_test"
	var failed, pgFailed atomic.Int64
	inject := func(db *gorm.DB) {
		if q := db.Statement.SQL.String(); strings.Contains(q, "FROM room_bookings b JOIN rooms r") {
			failed.Add(1)
			db.Statement.SQL.Reset()
			db.Statement.SQL.WriteString(strings.Replace(q, "JOIN rooms r", "JOIN "+missing+" r", 1))
		}
	}
	// 42P01 is undefined_table: PostgreSQL itself refused the statement.
	observe := func(db *gorm.DB) {
		if strings.Contains(db.Statement.SQL.String(), missing) && pgCode(db.Error) == "42P01" {
			pgFailed.Add(1)
		}
	}
	rows, queries := f.app.DB.Callback().Row(), f.app.DB.Callback().Query()
	for _, err := range []error{
		rows.Before("gorm:row").Register("test:fail_date_room_row", inject),
		queries.Before("gorm:query").Register("test:fail_date_room_query", inject),
		rows.After("gorm:row").Register("test:see_date_room_row", observe),
		queries.After("gorm:query").Register("test:see_date_room_query", observe),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = rows.Remove("test:fail_date_room_row")
		_ = queries.Remove("test:fail_date_room_query")
		_ = rows.Remove("test:see_date_room_row")
		_ = queries.Remove("test:see_date_room_query")
	})
	for _, c := range []struct {
		name, viewer, suffix string
		ev                   uint
		occ                  models.EventOccurrence
		code                 int
	}{
		{"calendar download", "Max Member", "/calendar.ics", f.ev.ID, f.dates[0], http.StatusOK},
		{"invitation only", "Ina Invitee", "", f.ev.ID, f.dates[2], http.StatusOK},
		{"ordinary one-time date", "Max Member", "", once.ID, onceDate, http.StatusOK},
		{"ordinary member, series compared", "Max Member", "", f.ev.ID, f.dates[0], http.StatusInternalServerError},
		{"signed out, series compared", "signed out", "", f.ev.ID, f.dates[0], http.StatusInternalServerError},
	} {
		before, pgBefore := failed.Load(), pgFailed.Load()
		w := f.as[c.viewer].do("GET", dateReadPath(c.ev, tombstoneDate(c.occ))+c.suffix, nil, false)
		if w.Code != c.code || failed.Load() == before || pgFailed.Load()-pgBefore != failed.Load()-before {
			t.Fatalf("%s: a failed room read answered %d (room reads %d, refused by PostgreSQL %d) %s, want %d", c.name, w.Code,
				failed.Load()-before, pgFailed.Load()-pgBefore, w.Body.String(), c.code)
		}
		if c.code == http.StatusOK && strings.Contains(w.Body.String(), `"room"`) {
			t.Fatalf("%s: a room from a failed read: %s", c.name, w.Body.String())
		}
		if c.ev != once.ID {
			continue
		}
		var body struct {
			Occurrence dateOccurrenceJSON `json:"occurrence"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err, w.Body.String())
		}
		d := body.Occurrence
		if d.Attendance == nil || d.Attendance.Registrations[models.RegGoing] != 1 || d.Seats == nil || d.Seats.InPerson.Used != 1 ||
			d.Seats.InPerson.Left == nil || *d.Seats.InPerson.Left != 1 {
			t.Fatalf("%s: the facts read after the failed room read: %s", c.name, w.Body.String())
		}
	}
}

// The date page's lenient reads (the church name, the push switch, the church
// zone and whether the viewer may answer) keep their defaults when PostgreSQL
// refuses them inside the page's snapshot, and every other fact still loads:
// each refused statement is rolled back to its savepoint (lenientReadIn), so
// the reads after it in the same transaction run. The statements fail inside
// PostgreSQL itself (an unknown table), only while the page's snapshot runs.
func TestDatePageLenientReadsSurviveAFailedStatement(t *testing.T) {
	f := newDatePageFixture(t)
	for key, value := range map[string]string{settingSiteName: "Grace Church", settingPushEnabled: "false", settingTimezone: "America/Chicago"} {
		if err := setSettingIn(f.app.DB, key, value); err != nil {
			t.Fatal(err)
		}
	}
	capacity := 2
	start := feedStart(10)
	picnic := models.Event{Title: "Church Picnic", Visibility: models.VisibilityPublic, StartsLocal: start.Format(events.WallLayout),
		DurationMinutes: 60, TZID: "UTC", Capacity: &capacity, ReminderPush: true, CreatedBy: f.admin.ID}
	gormMust(t, f.app.DB.Create(&picnic))
	picnicDate := models.EventOccurrence{EventID: picnic.ID, OriginalDate: calDate(start.Format("2006-01-02")), StartsAt: start, EndsAt: start.Add(time.Hour), Status: models.OccurrenceScheduled}
	gormMust(t, f.app.DB.Create(&picnicDate))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.member.ID, OccurrenceID: picnicDate.ID, Status: models.RegGoing, Source: models.RegSourceWeb,
		AttendanceMode: models.AttendanceInPerson}))

	read := func(viewer string, ev uint, occ models.EventOccurrence) dateOccurrenceJSON {
		t.Helper()
		raw := apiDo(t, f.as[viewer], "GET", dateReadPath(ev, tombstoneDate(occ)), nil, http.StatusOK)
		var body struct {
			Occurrence dateOccurrenceJSON `json:"occurrence"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err, raw)
		}
		return body.Occurrence
	}
	cases := []struct {
		name, viewer string
		ev           uint
		occ          models.EventOccurrence
		kinds        []string
	}{
		{"admin, church-wide date", "Dana Admin", picnic.ID, picnicDate, []string{"church name", "push switch", "church zone", "may answer"}},
		{"member, series date", "Max Member", f.ev.ID, f.dates[0], []string{"may answer"}},
	}
	baseline := make([]dateOccurrenceJSON, len(cases))
	for i, c := range cases {
		baseline[i] = read(c.viewer, c.ev, c.occ)
		if !baseline[i].CanRegister || baseline[i].Attendance == nil || baseline[i].Seats == nil {
			t.Fatalf("%s: the page without failures: %+v", c.name, baseline[i])
		}
	}
	if b := baseline[0]; b.HostedBy != "Grace Church" || b.Reminders == nil || b.Reminders.Push != nil {
		t.Fatalf("the admin's page without failures: %+v", b)
	}

	const missing = "lenient_reads_unavailable_in_test"
	keys := map[string]string{settingSiteName: "church name", settingPushEnabled: "push switch", settingTimezone: "church zone"}
	var armed atomic.Bool
	var mu sync.Mutex
	injected, refused := map[string]int{}, 0
	inject := func(db *gorm.DB) {
		if c, inTx := db.Statement.ConnPool.(gorm.TxCommitter); !armed.Load() || !inTx || c == nil || db.Error != nil ||
			(db.Statement.Table != "settings" && db.Statement.Table != "event_occurrences") {
			return
		}
		callbacks.BuildQuerySQL(db)
		q, kind := db.Statement.SQL.String(), ""
		switch {
		case strings.Contains(q, `FROM "settings" WHERE key = `) && len(db.Statement.Vars) > 0:
			key, _ := db.Statement.Vars[0].(string)
			kind = keys[key]
		case strings.Contains(q, `FROM "event_occurrences" WHERE id = $1 AND event_id = $2 AND status = $3`):
			kind = "may answer"
		}
		if kind == "" {
			return
		}
		mu.Lock()
		injected[kind]++
		mu.Unlock()
		db.Statement.SQL.Reset()
		db.Statement.SQL.WriteString(strings.NewReplacer(`"settings"`, `"`+missing+`"`, `"event_occurrences"`, `"`+missing+`"`).Replace(q))
	}
	// 42P01 is undefined_table: PostgreSQL itself refused the statement.
	observe := func(db *gorm.DB) {
		if strings.Contains(db.Statement.SQL.String(), missing) && pgCode(db.Error) == "42P01" {
			mu.Lock()
			refused++
			mu.Unlock()
		}
	}
	queries := f.app.DB.Callback().Query()
	for _, err := range []error{
		queries.Before("gorm:query").Register("test:fail_lenient_reads", inject),
		queries.After("gorm:query").Register("test:see_lenient_reads", observe),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.app.hooks.scheduledDateStep = func(step string) {
		if step == "begin" {
			armed.Store(true)
		}
	}
	// The push reminder is due at 17:00 UTC, inside a 4 PM to midnight window
	// in UTC and outside it on Chicago's clock, so its time shows which zone
	// the page used.
	window := push.Window{StartHour: 16, EndHour: 24}
	f.app.hooks.pushWindow = &window
	t.Cleanup(func() {
		f.app.hooks.pushWindow = nil
		f.app.hooks.scheduledDateStep = nil
		_ = queries.Remove("test:fail_lenient_reads")
		_ = queries.Remove("test:see_lenient_reads")
	})

	for i, c := range cases {
		mu.Lock()
		injected, refused = map[string]int{}, 0
		mu.Unlock()
		got := read(c.viewer, c.ev, c.occ)
		armed.Store(false)
		mu.Lock()
		total := 0
		for _, kind := range c.kinds {
			if injected[kind] == 0 {
				t.Errorf("%s: the %s read was not refused (refused: %v)", c.name, kind, injected)
			}
		}
		for _, n := range injected {
			total += n
		}
		if refused != total {
			t.Errorf("%s: %d reads failed, %d refused by PostgreSQL", c.name, total, refused)
		}
		mu.Unlock()
		if got.CanRegister {
			t.Fatalf("%s: a failed may-answer read still offers the answer: %+v", c.name, got)
		}
		want := baseline[i]
		want.CanRegister = false
		if c.ev == picnic.ID {
			if due := start.Add(-pushReminderLead); got.HostedBy != defaultSiteName || got.Reminders == nil || got.Reminders.Push == nil ||
				!*got.Reminders.Push || got.Reminders.PushDueAt == nil || !got.Reminders.PushDueAt.Equal(due) {
				t.Fatalf("%s: the defaults after failed setting reads (the church name, push on, the UTC reminder at %s): %+v %+v",
					c.name, due, got, got.Reminders)
			}
			want.HostedBy = defaultSiteName
			wantReminders := *want.Reminders
			wantReminders.Push, wantReminders.PushDueAt = got.Reminders.Push, got.Reminders.PushDueAt
			want.Reminders = &wantReminders
		}
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("%s: the facts read after the failed lenient reads:\n got %s\nwant %s", c.name, gotJSON, wantJSON)
		}
	}
}

// The date page's coverage counts exactly the dates the series writer answers
// (M8.1 brief 2.1), at one injected instant: a date starting exactly now, a
// cancelled date and a hidden sibling are in neither, and a household
// person's answer covers nothing for the account.
func TestDatePageCoverageMatchesTheSeriesWriter(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Cora Leader", "password1234", models.RoleMember)
	reader := newTestUser(t, app, "Rex Reader", "password1234", models.RoleMember)
	client := loginAPI(t, app, reader.Name, "password1234")
	now := time.Now().UTC().Truncate(time.Minute)
	app.clock = func() time.Time { return now }
	group := activeGroup(t, app, leader, "Coverage Circle", models.VisibilityPublic, "")
	start := now.AddDate(0, 0, -7)
	ev := models.Event{GroupID: &group.ID, Title: "Coverage Night", Visibility: models.VisibilityPublic, StartsLocal: start.Format(events.WallLayout),
		DurationMinutes: 60, TZID: "UTC", RRule: "FREQ=WEEKLY;COUNT=7", CreatedBy: leader.ID}
	gormMust(t, app.DB.Create(&ev))
	var dates []models.EventOccurrence
	for i := 0; i < 7; i++ {
		s := start.AddDate(0, 0, 7*i)
		occ := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate(s.Format("2006-01-02")), StartsAt: s, EndsAt: s.Add(time.Hour), Status: models.OccurrenceScheduled}
		gormMust(t, app.DB.Create(&occ))
		dates = append(dates, occ)
	}
	// dates[0] is past and dates[1] starts exactly now; dates[2] is cancelled,
	// dates[3] is hidden from a reader outside the group, dates[4] carries only
	// the reader's household person's answer, dates[6] the reader's own.
	gormMust(t, app.DB.Model(&dates[2]).Updates(map[string]any{"status": models.OccurrenceCancelled, "cancel_source": models.CancelSourceManual}))
	gormMust(t, app.DB.Model(&dates[3]).Update("visibility_override", models.VisibilityInviteOnly))
	household := models.Household{Name: "Reader household", CreatedBy: reader.ID}
	gormMust(t, app.DB.Create(&household))
	gormMust(t, app.DB.Create(&models.HouseholdAccount{HouseholdID: household.ID, UserID: reader.ID, Relation: models.HouseholdRelationAdult}))
	birth := now.AddDate(-30, 0, 0)
	person := models.HouseholdPerson{HouseholdID: household.ID, FirstName: "Rita", LastName: "Reader", Kind: models.HouseholdAdult, BirthDate: &birth, CreatedBy: reader.ID}
	gormMust(t, app.DB.Create(&person))
	gormMust(t, app.DB.Create(&models.Registration{HouseholdPersonID: &person.ID, RegisteredBy: &reader.ID, OccurrenceID: dates[4].ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson}))
	gormMust(t, app.DB.Create(&models.Registration{UserID: &reader.ID, OccurrenceID: dates[6].ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson}))

	series := func() dateSeriesJSON {
		t.Helper()
		raw := apiDo(t, client, "GET", dateReadPath(ev.ID, tombstoneDate(dates[5])), nil, http.StatusOK)
		var body struct {
			Occurrence dateOccurrenceJSON `json:"occurrence"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err, raw)
		}
		s := body.Occurrence.Series
		if s == nil || s.UpcomingDates == nil || s.CoveredDates == nil {
			t.Fatalf("no coverage: %s", raw)
		}
		return *s
	}
	own := func() []uint {
		t.Helper()
		var ids []uint
		gormMust(t, app.DB.Model(&models.Registration{}).Where("user_id = ? AND household_person_id IS NULL AND status = ?", reader.ID, models.RegGoing).
			Order("occurrence_id").Pluck("occurrence_id", &ids))
		return ids
	}
	before := series()
	if *before.UpcomingDates != 3 || *before.CoveredDates != 1 || !before.CanAnswerSeries {
		t.Fatalf("coverage %d of %d (series answer %v), want 1 of 3: dates[4], [5] and [6]", *before.CoveredDates, *before.UpcomingDates, before.CanAnswerSeries)
	}
	if postSeriesAnswer(client, ev.ID, map[string]any{"scope": seriesScopeAllFuture, "status": models.RegGoing}) == nil {
		t.Fatal("the series writer refused")
	}
	if got, want := own(), []uint{dates[4].ID, dates[5].ID, dates[6].ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the writer answered %v, want exactly the %d dates the page counted %v", got, *before.UpcomingDates, want)
	}
	if after := series(); *after.UpcomingDates != 3 || *after.CoveredDates != 3 {
		t.Fatalf("after the series answer: %d of %d, want 3 of 3", *after.CoveredDates, *after.UpcomingDates)
	}
}

// An accepted rule that makes two times on one day (BYHOUR=9,10): event
// creation writes the day's first time and a title-only series edit moves the
// date to its last (events.ApplySeries). Each is a time the series itself
// holds that day, so the date page reports no change either way.
func TestDatePageTwoTimesOnOneDay(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Morning Circle", models.VisibilityPublic, "")
	ev := createFeedEvent(t, client, group.ID, "Morning Prayer", feedStart(3), "FREQ=WEEKLY;BYHOUR=9,10;COUNT=4")
	var occ models.EventOccurrence
	gormMust(t, app.DB.Where("event_id = ?", ev.ID).Order("original_date").First(&occ))
	unchanged := func(clock string) {
		t.Helper()
		gormMust(t, app.DB.First(&occ, occ.ID))
		if got := occ.StartsAt.UTC().Format("15:04"); got != clock {
			t.Fatalf("fixture: the writer put %s at %s, want %s", tombstoneDate(occ), got, clock)
		}
		raw := apiDo(t, client, "GET", dateReadPath(ev.ID, tombstoneDate(occ)), nil, http.StatusOK)
		var body struct {
			Occurrence dateOccurrenceJSON `json:"occurrence"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err, raw)
		}
		s := body.Occurrence.Series
		if s == nil || s.Usual == nil || s.Usual.StartsLocal != body.Occurrence.StartsLocal || len(s.Changes) != 0 {
			t.Fatalf("a date at %s the writer assigned: %s", clock, raw)
		}
	}
	unchanged("09:00")
	apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), map[string]any{"title": "Morning Prayers"}, http.StatusOK)
	unchanged("10:00")
}

// The date page's name summary follows the attendee rules for every reader
// (subjectFacts.attendee): a Going teen awaiting consent shows only to the
// event's managers, a household child only to an admin and their own
// household adult (a leader needs a confirmed guardian link), and a verified
// guest to anyone who sees names. Counts include every listed row, each name
// carries exactly name, status and guestCount, and a signed-out reader gets
// counts alone.
func TestDatePageAttendeeSafeguarding(t *testing.T) {
	f := newDatePageFixture(t)
	occ := f.dates[3]
	teen := newTestUser(t, f.app, "Tess Pending", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(teen).Updates(map[string]any{"account_type": models.AccountTeen, "status": models.UserPendingConsent}))
	guest := newTestUser(t, f.app, "Gus Guest", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(guest).Update("status", models.UserGuest))
	guardian := newTestUser(t, f.app, "Gail Guardian", "password1234", models.RoleMember)
	f.as[guardian.Name] = loginAPI(t, f.app, guardian.Name, "password1234")
	household := models.Household{Name: "Guardian household", CreatedBy: guardian.ID}
	gormMust(t, f.app.DB.Create(&household))
	gormMust(t, f.app.DB.Create(&models.HouseholdAccount{HouseholdID: household.ID, UserID: guardian.ID, Relation: models.HouseholdRelationAdult}))
	birth := time.Now().UTC().AddDate(-8, 0, 0)
	child := models.HouseholdPerson{HouseholdID: household.ID, FirstName: "Kit", LastName: "Small", Kind: models.HouseholdChild, BirthDate: &birth,
		CareNotes: "peanut allergy", CreatedBy: guardian.ID}
	gormMust(t, f.app.DB.Create(&child))
	for _, r := range []models.Registration{
		{UserID: &teen.ID, OccurrenceID: occ.ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson},
		{HouseholdPersonID: &child.ID, RegisteredBy: &guardian.ID, OccurrenceID: occ.ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson},
		{UserID: &guest.ID, OccurrenceID: occ.ID, Status: models.RegGoing, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson, GuestCount: 1},
	} {
		gormMust(t, f.app.DB.Create(&r))
	}
	tess, kit, gus := teen.PublicName(), child.FullName(), guest.PublicName()
	counts := map[string]int64{models.RegGoing: 3, models.RegMaybe: 0, models.RegWaitlisted: 0}
	managerCounts := map[string]int64{models.RegGoing: 3, models.RegMaybe: 0, models.RegWaitlisted: 0, models.RegDeclined: 0}
	forbidden := []string{teen.Email, guest.Email, guardian.Email, "peanut allergy", "accountType", "householdPerson", "careNotes", "registeredBy",
		"registrationId", "childcareCount", "Guardian household"}
	for _, c := range []struct {
		viewer string
		counts map[string]int64
		names  map[string]string // nil: no names at all
	}{
		{"Max Member", counts, map[string]string{gus: models.RegGoing}},
		{"Lou Leader", managerCounts, map[string]string{tess: models.RegGoing, gus: models.RegGoing}},
		{"Dana Admin", managerCounts, map[string]string{tess: models.RegGoing, kit: models.RegGoing, gus: models.RegGoing}},
		{guardian.Name, counts, map[string]string{kit: models.RegGoing, gus: models.RegGoing}},
		{"signed out", counts, nil},
	} {
		d, _, raw := f.read(t, c.viewer, occ)
		if d.Attendance == nil || !reflect.DeepEqual(d.Attendance.Registrations, c.counts) {
			t.Fatalf("%s: counts %s, want %v", c.viewer, raw, c.counts)
		}
		for _, s := range forbidden {
			if strings.Contains(raw, s) {
				t.Fatalf("%s: the date page carries %q: %s", c.viewer, s, raw)
			}
		}
		var shape struct {
			Occurrence struct {
				Attendance map[string]json.RawMessage `json:"attendance"`
			} `json:"occurrence"`
		}
		if err := json.Unmarshal([]byte(raw), &shape); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{tess, kit, gus} {
			if _, shown := c.names[name]; !shown && strings.Contains(raw, name) {
				t.Fatalf("%s: a hidden name %q in the page: %s", c.viewer, name, raw)
			}
		}
		list, hasNames := shape.Occurrence.Attendance["attendees"]
		if c.names == nil {
			if hasNames || d.CanSeeAttendees || strings.Contains(raw, gus) {
				t.Fatalf("%s: names without the right: %s", c.viewer, raw)
			}
			continue
		}
		if got := attendeeSet(d.Attendance.Attendees); !reflect.DeepEqual(got, c.names) {
			t.Fatalf("%s: names %v, want %v", c.viewer, got, c.names)
		}
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(list, &entries); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			keys := []string{}
			for k := range e {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, []string{"guestCount", "name", "status"}) {
				t.Fatalf("%s: a name entry with keys %v: %s", c.viewer, keys, raw)
			}
		}
	}
	if err := setSettingIn(f.app.DB, settingTimezone, "not/a-zone"); err != nil {
		t.Fatal(err)
	}
	apiDo(t, f.as["Lou Leader"], "GET", dateReadPath(occ.EventID, tombstoneDate(occ)), nil, http.StatusInternalServerError)
}

// The series clock evidence on the date page (DEC-155). The writers record
// every time the rule makes on a date's original day; the page reads one row
// of it in a fixed number of indexed probes and never walks the rule.

// seriesRead is a date page's own start and series facts.
type seriesRead struct {
	StartsLocal string
	Series      *dateSeriesJSON
}

func readSeriesDate(t *testing.T, c *apiClient, eventID uint, day string) seriesRead {
	t.Helper()
	raw := apiDo(t, c, "GET", dateReadPath(eventID, day), nil, http.StatusOK)
	var body struct {
		Occurrence dateOccurrenceJSON `json:"occurrence"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err, raw)
	}
	if body.Occurrence.Series == nil || body.Occurrence.Series.Usual == nil {
		t.Fatalf("no series facts: %s", raw)
	}
	return seriesRead{StartsLocal: body.Occurrence.StartsLocal, Series: body.Occurrence.Series}
}

// wantUsual checks a date page's usual clock (HH:MM, "" for unknown) and its
// changes.
func wantUsual(t *testing.T, r seriesRead, clock string, changes ...string) {
	t.Helper()
	got := r.Series.Usual.StartsLocal
	if clock == "" && got != "" || clock != "" && (len(got) != len(events.WallLayout) || got[11:] != clock) {
		t.Fatalf("usual clock %q, want %q (date at %s)", got, clock, r.StartsLocal)
	}
	if changes == nil {
		changes = []string{}
	}
	if !reflect.DeepEqual(r.Series.Changes, changes) {
		t.Fatalf("changes %v, want %v (usual %+v, date at %s)", r.Series.Changes, changes, r.Series.Usual, r.StartsLocal)
	}
}

func moveDateTo(t *testing.T, c *apiClient, eventID uint, day, wall string) {
	t.Helper()
	apiDo(t, c, "PUT", "/api/v1/events/"+itoa(eventID)+"/occurrences/"+day, map[string]any{"startsLocal": wall}, http.StatusOK)
}

func seriesClockTestDates(t *testing.T, db *gorm.DB, eventID uint) []models.EventOccurrence {
	t.Helper()
	var occs []models.EventOccurrence
	gormMust(t, db.Where("event_id = ?", eventID).Order("original_date").Find(&occs))
	return occs
}

func clockRowsOf(t *testing.T, db *gorm.DB, occID uint) []models.OccurrenceSeriesStart {
	t.Helper()
	var rows []models.OccurrenceSeriesStart
	gormMust(t, db.Where("occurrence_id = ?", occID).Order("starts_at").Find(&rows))
	return rows
}

func clockCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	gormMust(t, db.Model(&models.OccurrenceSeriesStart{}).Count(&n))
	return n
}

func runExtend(t *testing.T, app *App) {
	t.Helper()
	if err := eventdates.Extend(app.DB, time.Now().UTC(), app.afterExtend()); err != nil {
		t.Fatal(err)
	}
}

// BYHOUR=9,10 through the API: creation puts a date at 09:00 and a title-only
// edit at 10:00, both the series' own; a date moved alone to the other valid
// time is no time change, and one moved to 11:00 is compared with the first
// time. Each answer holds immediately, after Extend and on another read.
func TestSeriesClockMultiTimeDayThroughTheAPI(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Morning Circle", models.VisibilityPublic, "")
	ev := createFeedEvent(t, client, group.ID, "Morning Prayer", feedStart(3), "FREQ=WEEKLY;BYHOUR=9,10;COUNT=6")
	occ := seriesClockTestDates(t, app.DB, ev.ID)[0]
	day := tombstoneDate(occ)
	if got := occ.StartsAt.UTC().Format("15:04"); got != "09:00" {
		t.Fatalf("fixture: created at %s", got)
	}
	if rows := clockRowsOf(t, app.DB, occ.ID); len(rows) != 2 || !rows[0].IsFirst || rows[1].IsFirst || rows[0].TZID != "UTC" {
		t.Fatalf("evidence %+v", rows)
	}
	stable := func(clock string, changes ...string) {
		t.Helper()
		wantUsual(t, readSeriesDate(t, client, ev.ID, day), clock, changes...)
		runExtend(t, app)
		wantUsual(t, readSeriesDate(t, client, ev.ID, day), clock, changes...)
		wantUsual(t, readSeriesDate(t, client, ev.ID, day), clock, changes...)
	}
	stable("09:00")
	apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), map[string]any{"title": "Morning Prayers"}, http.StatusOK)
	if got := seriesClockTestDates(t, app.DB, ev.ID)[0].StartsAt.UTC().Format("15:04"); got != "10:00" {
		t.Fatalf("fixture: the series edit put the date at %s", got)
	}
	stable("10:00")
	moveDateTo(t, client, ev.ID, day, day+"T09:00")
	stable("09:00")
	moveDateTo(t, client, ev.ID, day, day+"T10:00")
	stable("10:00")
	moveDateTo(t, client, ev.ID, day, day+"T11:00")
	stable("09:00", changeTime)
}

// Extending a weekly BYHOUR=9,10;BYSECOND=30 series from COUNT=2 to COUNT=4
// adds one date in the edit's transaction: one row, at the series edit's
// time (the last), with both times as evidence and one first. The new date is
// handed on once: the series room books it once, and no room is reported
// lost and no date restored. The date editor saves whole minutes, so 10:00
// matches 10:00:30 and is no change, while 11:00 falls back to 09:00:30 and
// is a time change.
func TestSeriesClockEditAddsAMultiTimeDayOnce(t *testing.T) {
	f := newBookingFixture(t)
	app, client := f.app, f.leader
	start := feedStart(3).Add(-11 * time.Hour) // 08:00, before the day's times
	ev := createFeedEvent(t, client, f.group, "Morning Prayer", start, "FREQ=WEEKLY;BYHOUR=9,10;BYSECOND=30;COUNT=2")
	if occs := seriesClockTestDates(t, app.DB, ev.ID); len(occs) != 1 {
		t.Fatalf("fixture: %d dates", len(occs))
	}
	if code, out := f.seriesRoom(t, client, ev, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK || out.Booked != 1 {
		t.Fatalf("fixture: series room %d %+v", code, out)
	}
	notices := func(kind string) int64 {
		t.Helper()
		return contactCount(t, app, &models.Notification{}, "kind = ?", kind)
	}
	restored := func() int64 {
		t.Helper()
		return contactCount(t, app, &models.AuditLog{}, "action = ? AND target_id = ?", audit.EventDatesRestored, ev.ID)
	}
	lostBefore, changedBefore, restoredBefore := notices(notify.RoomLost), notices(notify.EventChanged), restored()
	apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), map[string]any{"recurrenceRule": "FREQ=WEEKLY;BYHOUR=9,10;BYSECOND=30;COUNT=4"}, http.StatusOK)
	occs := seriesClockTestDates(t, app.DB, ev.ID)
	if len(occs) != 2 {
		t.Fatalf("dates after the edit: %d", len(occs))
	}
	added := occs[1]
	if got := added.StartsAt.UTC().Format("15:04:05"); got != "10:00:30" {
		t.Fatalf("the added date starts at %s, want the series edit's 10:00:30", got)
	}
	rows := clockRowsOf(t, app.DB, added.ID)
	if len(rows) != 2 || rows[0].StartsAt.UTC().Format("15:04:05") != "09:00:30" || !rows[0].IsFirst ||
		rows[1].StartsAt.UTC().Format("15:04:05") != "10:00:30" || rows[1].IsFirst {
		t.Fatalf("evidence %+v", rows)
	}
	if n := contactCount(t, app, &models.RoomBooking{}, "occurrence_id = ?", added.ID); n != 1 {
		t.Fatalf("the added date has %d bookings, want the series room once", n)
	}
	if n := seriesBookings(t, app, ev.ID, models.BookingConfirmed); n != 2 {
		t.Fatalf("%d confirmed series bookings, want 2", n)
	}
	if lost, changed, back := notices(notify.RoomLost), notices(notify.EventChanged), restored(); lost != lostBefore || changed != changedBefore || back != restoredBefore {
		t.Fatalf("room.lost %d -> %d, event.changed %d -> %d, dates_restored %d -> %d", lostBefore, lost, changedBefore, changed, restoredBefore, back)
	}
	day := tombstoneDate(added)
	wantUsual(t, readSeriesDate(t, client, ev.ID, day), "10:00")
	moveDateTo(t, client, ev.ID, day, day+"T10:00")
	for i := 0; i < 2; i++ {
		wantUsual(t, readSeriesDate(t, client, ev.ID, day), "10:00")
		runExtend(t, app)
	}
	moveDateTo(t, client, ev.ID, day, day+"T11:00")
	for i := 0; i < 2; i++ {
		wantUsual(t, readSeriesDate(t, client, ev.ID, day), "09:00", changeTime)
		runExtend(t, app)
	}
}

// An edit that makes a series one-time drops the old series' evidence from
// the dates that were upcoming: kept, the first date's 19:00 would come back
// as its usual clock once the date had passed and a later edit made the
// event repeat again, and its 20:00 would read as a time change. The date had
// no series clock when it happened, so it is unknown; dates still upcoming
// at the second edit get the new series' evidence.
func TestSeriesClockOneTimeEditDropsTheOldSeriesEvidence(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Evening Circle", models.VisibilityPublic, "")
	ev := createFeedEvent(t, client, group.ID, "Evening Prayer", feedStart(3), "FREQ=WEEKLY;COUNT=4")
	first := seriesClockTestDates(t, app.DB, ev.ID)[0]
	day := tombstoneDate(first)
	if rows := clockRowsOf(t, app.DB, first.ID); len(rows) != 1 || rows[0].StartsAt.UTC().Format("15:04") != "19:00" {
		t.Fatalf("fixture: evidence %+v", rows)
	}
	apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), map[string]any{"startsLocal": day + "T20:00", "recurrenceRule": ""}, http.StatusOK)
	occs := seriesClockTestDates(t, app.DB, ev.ID)
	if got := occs[0].StartsAt.UTC().Format("15:04"); occs[0].ID != first.ID || got != "20:00" {
		t.Fatalf("fixture: the one-time edit left the date at %s", got)
	}
	for _, o := range occs {
		if rows := clockRowsOf(t, app.DB, o.ID); len(rows) != 0 {
			t.Fatalf("date %s kept the old series' evidence: %+v", tombstoneDate(o), rows)
		}
	}
	// Two days on, the date is past, and the event repeats again from it.
	aged := first.StartsAt.Add(48 * time.Hour)
	app.clock = func() time.Time { return aged }
	apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=4"}, http.StatusOK)
	app.clock = nil
	if rows := clockRowsOf(t, app.DB, first.ID); len(rows) != 0 {
		t.Fatalf("the past date has evidence %+v", rows)
	}
	// Read before any Extend: runExtend walks from the real clock, to which
	// the date is still upcoming.
	wantUsual(t, readSeriesDate(t, client, ev.ID, day), "")
	checked := 0
	for _, o := range seriesClockTestDates(t, app.DB, ev.ID)[1:] {
		if !o.StartsAt.After(aged) || o.Status != models.OccurrenceScheduled {
			continue
		}
		checked++
		if rows := clockRowsOf(t, app.DB, o.ID); len(rows) != 1 || rows[0].StartsAt.UTC().Format("15:04") != "20:00" {
			t.Fatalf("upcoming date %s evidence %+v, want the new series' 20:00", tombstoneDate(o), rows)
		}
	}
	if checked != 3 {
		t.Fatalf("%d dates restored by the second edit, want 3", checked)
	}
}

// usualClockIn prefers the exact instant, then the earliest in the start's
// UTC minute, then the first; the two passes through a repeated hour are
// different minutes, so a date at the pass the writer did not generate falls
// back to the first and is a time change.
func TestUsualClockInPicks(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	ev := models.Event{Title: "Probe", Visibility: models.VisibilityPublic, StartsLocal: "2026-10-25T01:30", DurationMinutes: 60,
		TZID: "America/Boise", RRule: "FREQ=WEEKLY", CreatedBy: leader.ID}
	gormMust(t, app.DB.Create(&ev))
	occ := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate("2026-11-01"), StartsAt: time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC),
		EndsAt: time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC), Status: models.OccurrenceScheduled}
	gormMust(t, app.DB.Create(&occ))
	at := func(h, m, s int) time.Time { return time.Date(2026, 11, 1, h, m, s, 0, time.UTC) }
	for _, r := range []models.OccurrenceSeriesStart{
		{OccurrenceID: occ.ID, StartsAt: at(7, 30, 0), IsFirst: true, TZID: ev.TZID},
		{OccurrenceID: occ.ID, StartsAt: at(9, 0, 0), TZID: ev.TZID},
		{OccurrenceID: occ.ID, StartsAt: at(9, 0, 30), TZID: ev.TZID},
	} {
		gormMust(t, app.DB.Create(&r))
	}
	for _, c := range []struct {
		name        string
		start, want time.Time
	}{
		{"exact, seconds kept over the earlier time in the minute", at(9, 0, 30), at(9, 0, 30)},
		{"exact first", at(7, 30, 0), at(7, 30, 0)},
		{"exact whole minute", at(9, 0, 0), at(9, 0, 0)},
		{"no exact time: the earliest in the minute", at(9, 0, 15), at(9, 0, 0)},
		{"the other pass through the repeated hour", at(8, 30, 0), at(7, 30, 0)},
		{"nothing in the minute", at(11, 0, 0), at(7, 30, 0)},
	} {
		got, err := usualClockIn(app.DB, occ.ID, c.start)
		if err != nil || got == nil || !got.StartsAt.Equal(c.want) || got.TZID != ev.TZID || got.WrittenLocal != nil {
			t.Fatalf("%s: %+v %v, want %s", c.name, got, err, c.want)
		}
	}
	none := models.EventOccurrence{EventID: ev.ID, OriginalDate: calDate("2026-11-08"), StartsAt: at(7, 30, 0).AddDate(0, 0, 7),
		EndsAt: at(8, 30, 0).AddDate(0, 0, 7), Status: models.OccurrenceScheduled}
	gormMust(t, app.DB.Create(&none))
	if got, err := usualClockIn(app.DB, none.ID, none.StartsAt); err != nil || got != nil {
		t.Fatalf("a date with no evidence: %+v %v", got, err)
	}
}

// A series repeating from Go's zero instant, created through the API: an
// untouched date has no time change and a moved one has one, and Extend
// never replaces the clock its creation captured. A date with no evidence
// stays unknown however often Extend runs, until a real series edit writes
// new evidence with the dates it moves.
func TestSeriesClockZeroStartThroughTheAPI(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Year Circle", models.VisibilityPublic, "")
	w := client.do("POST", "/api/v1/groups/"+itoa(group.ID)+"/events", map[string]any{
		"title": "New Year Prayer", "startsLocal": "0001-01-01T00:00", "durationMinutes": 60, "tzid": "UTC",
		"recurrenceRule": "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;BYHOUR=9",
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event eventJSON `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	ev := created.Event
	occs := seriesClockTestDates(t, app.DB, ev.ID)
	if len(occs) == 0 {
		t.Fatal("fixture: no dates")
	}
	first, second := occs[0], occs[len(occs)-1]
	captured := clockRowsOf(t, app.DB, first.ID)
	if len(captured) != 1 || !captured[0].StartsAt.Equal(first.StartsAt) {
		t.Fatalf("captured %+v for a date at %s", captured, first.StartsAt)
	}
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(first)), first.StartsAt.UTC().Format("15:04"))
	time.Sleep(1100 * time.Millisecond)
	runExtend(t, app)
	if again := clockRowsOf(t, app.DB, first.ID); !reflect.DeepEqual(again, captured) {
		t.Fatalf("Extend replaced the captured clock: %+v -> %+v", captured, again)
	}
	day := tombstoneDate(first)
	moveDateTo(t, client, ev.ID, day, day+"T15:00")
	wantUsual(t, readSeriesDate(t, client, ev.ID, day), first.StartsAt.UTC().Format("15:04"), changeTime)

	if len(occs) > 1 {
		// A date written before the store: no evidence, and none rebuilt.
		gormMust(t, app.DB.Where("occurrence_id = ?", second.ID).Delete(&models.OccurrenceSeriesStart{}))
		for i := 0; i < 2; i++ {
			runExtend(t, app)
			wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(second)), "")
		}
		apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), map[string]any{"title": "New Year Prayers"}, http.StatusOK)
		var now models.EventOccurrence
		gormMust(t, app.DB.First(&now, second.ID))
		wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(second)), now.StartsAt.UTC().Format("15:04"))
	}
}

// The release before the store (schema 00008) kept running against the new
// schema with its own statements: a series move (a full save, then the
// upcoming dates retimed), a frozen date it left alone, an inserted date, a
// per-date move on another event, a zero-start parent save and a date that
// passed before the new binary came back. Before any worker runs no stale
// clock makes a time claim; the repair rebuilds the upcoming dates from the
// current rule and leaves past ones unknown; a later edit keeps it right.
func TestSeriesClockAfterAPreviousBinary(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Evening Circle", models.VisibilityPublic, "")
	ev := createFeedEvent(t, client, group.ID, "Evening Prayer", feedStart(-14), "FREQ=WEEKLY;COUNT=8")
	other := createFeedEvent(t, client, group.ID, "Other Prayer", feedStart(3), "FREQ=WEEKLY;COUNT=3")
	occs := seriesClockTestDates(t, app.DB, ev.ID)
	if len(occs) != 8 {
		t.Fatalf("fixture: %d dates", len(occs))
	}
	past, frozen, moved, last := occs[0], occs[3], occs[4], occs[7]
	frozenAt := frozen.StartsAt.Add(-time.Hour)
	moveDateTo(t, client, ev.ID, tombstoneDate(frozen), events.FormatWall(frozenAt, time.UTC))

	// The previous binary: a full save that moves the series to 20:00, then
	// the upcoming dates without a clock override retimed, an inserted date
	// with no evidence, and a per-date move on the other event.
	gormMust(t, app.DB.Exec(`UPDATE events SET title = 'Evening Prayers', starts_local = ?, tzid = 'UTC', rrule = 'FREQ=WEEKLY;COUNT=9', rrule_until = NULL, updated_at = now() WHERE id = ?`,
		feedStart(-14).Add(time.Hour).Format(events.WallLayout), ev.ID))
	gormMust(t, app.DB.Exec(`UPDATE event_occurrences SET starts_at = starts_at + interval '1 hour', ends_at = ends_at + interval '1 hour'
		WHERE event_id = ? AND starts_at > now() AND starts_at_override IS NULL`, ev.ID))
	extra := last.StartsAt.AddDate(0, 0, 7).Add(time.Hour)
	gormMust(t, app.DB.Exec(`INSERT INTO event_occurrences (event_id, original_date, starts_at, ends_at, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'scheduled', now(), now())`, ev.ID, extra.Format("2006-01-02"), extra, extra.Add(time.Hour)))
	otherDate := seriesClockTestDates(t, app.DB, other.ID)[0]
	gormMust(t, app.DB.Exec(`UPDATE event_occurrences SET starts_at = starts_at + interval '2 hours', ends_at = ends_at + interval '2 hours',
		starts_at_override = starts_at + interval '2 hours' WHERE id = ?`, otherDate.ID))

	// Before any worker: the dirty event claims no time anywhere.
	for _, o := range []models.EventOccurrence{frozen, moved, last} {
		wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(o)), "")
	}
	wantUsual(t, readSeriesDate(t, client, ev.ID, extra.Format("2006-01-02")), "")
	// The other event's parent never changed: its intact evidence compares.
	wantUsual(t, readSeriesDate(t, client, other.ID, tombstoneDate(otherDate)), "19:00", changeTime)

	// A failure between dropping and rebuilding rolls the repair back whole.
	hook := "test:fail_clock_insert"
	failures := 0
	if err := app.DB.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "occurrence_series_starts" {
			failures++
			_ = tx.AddError(errors.New("injected"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	before := clockCount(t, app.DB)
	if err := eventdates.Extend(app.DB, time.Now().UTC(), nil); err == nil || failures == 0 {
		t.Fatalf("the injected failure was not reached: %v", err)
	}
	_ = app.DB.Callback().Create().Remove(hook)
	var dirty bool
	gormMust(t, app.DB.Raw(`SELECT series_clock_dirty FROM events WHERE id = ?`, ev.ID).Scan(&dirty))
	if !dirty || clockCount(t, app.DB) != before {
		t.Fatalf("a failed repair committed part of its work: dirty %v", dirty)
	}
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(moved)), "")

	runExtend(t, app)
	gormMust(t, app.DB.Raw(`SELECT series_clock_dirty FROM events WHERE id = ?`, ev.ID).Scan(&dirty))
	if dirty {
		t.Fatal("the repair left the event dirty")
	}
	if rows := clockRowsOf(t, app.DB, past.ID); len(rows) != 0 {
		t.Fatalf("a past date kept evidence a previous binary may have made stale: %+v", rows)
	}
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(moved)), "20:00")
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(frozen)), "20:00", changeTime)
	wantUsual(t, readSeriesDate(t, client, ev.ID, extra.Format("2006-01-02")), "20:00")

	// A new edit after the repair keeps every answer.
	apiDo(t, client, "PUT", "/api/v1/events/"+itoa(ev.ID), map[string]any{"title": "Evening Prayer"}, http.StatusOK)
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(moved)), "20:00")
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(frozen)), "20:00", changeTime)

	// A zero-start parent saved by the previous binary reads unknown until
	// settled, and settling cannot rebuild its clocks.
	zw := client.do("POST", "/api/v1/groups/"+itoa(group.ID)+"/events", map[string]any{
		"title": "New Year", "startsLocal": "0001-01-01T00:00", "durationMinutes": 60, "tzid": "UTC",
		"recurrenceRule": "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;BYHOUR=9",
	}, true)
	if zw.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", zw.Code, zw.Body.String())
	}
	var zero struct {
		Event eventJSON `json:"event"`
	}
	if err := json.Unmarshal(zw.Body.Bytes(), &zero); err != nil {
		t.Fatal(err)
	}
	zdate := seriesClockTestDates(t, app.DB, zero.Event.ID)[0]
	gormMust(t, app.DB.Exec(`UPDATE events SET title = 'New Year Prayer', starts_local = starts_local, tzid = tzid, rrule = rrule, rrule_until = rrule_until WHERE id = ?`, zero.Event.ID))
	wantUsual(t, readSeriesDate(t, client, zero.Event.ID, tombstoneDate(zdate)), "")
	runExtend(t, app)
	wantUsual(t, readSeriesDate(t, client, zero.Event.ID, tombstoneDate(zdate)), "")
}

// The repair takes the event's lock and reads the event under it, so a
// series edit holding the lock when the repair starts is the one it settles.
func TestSeriesClockRepairWaitsForTheEventLock(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "Evening Circle", models.VisibilityPublic, "")
	ev := createFeedEvent(t, client, group.ID, "Evening Prayer", feedStart(3), "FREQ=WEEKLY;COUNT=4")
	occ := seriesClockTestDates(t, app.DB, ev.ID)[1]
	// A previous binary dirtied the event; its writer now holds the lock
	// while moving the series to 21:00.
	gormMust(t, app.DB.Exec(`UPDATE events SET rrule = rrule WHERE id = ?`, ev.ID))
	writer := app.DB.Begin()
	gormMust(t, writer.Exec(`SELECT id FROM events WHERE id = ? FOR UPDATE`, ev.ID))
	gormMust(t, writer.Exec(`UPDATE events SET starts_local = ? WHERE id = ?`, feedStart(3).Add(2*time.Hour).Format(events.WallLayout), ev.ID))
	gormMust(t, writer.Exec(`UPDATE event_occurrences SET starts_at = starts_at + interval '2 hours', ends_at = ends_at + interval '2 hours' WHERE event_id = ?`, ev.ID))
	done := make(chan error, 1)
	go func() { done <- eventdates.Extend(app.DB, time.Now().UTC(), nil) }()
	select {
	case err := <-done:
		writer.Rollback()
		t.Fatalf("the repair ran past the held lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	gormMust(t, writer.Commit())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wantUsual(t, readSeriesDate(t, client, ev.ID, tombstoneDate(occ)), "21:00")
}

// A failed read of the series clock fails the page; the calendar download,
// which never reads it, still answers.
func TestDatePageClockReadFailureFails(t *testing.T) {
	f := newDatePageFixture(t)
	path := dateReadPath(f.ev.ID, tombstoneDate(f.dates[0]))
	hook := "test:fail_usual_clock"
	failed := 0
	if err := f.app.DB.Callback().Row().Before("gorm:row").Register(hook, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "occurrence_series_starts") {
			failed++
			_ = tx.AddError(errors.New("clock unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.app.DB.Callback().Row().Remove(hook) })
	if w := f.as["Max Member"].do("GET", path, nil, false); w.Code != http.StatusInternalServerError || failed == 0 {
		t.Fatalf("a failed clock read answered %d (%d reads failed): %s", w.Code, failed, w.Body.String())
	}
	apiDo(t, f.as["Max Member"], "GET", path+"/calendar.ics", nil, http.StatusOK)
}

// The page's clock costs the same for a long, dense rule as for a short one:
// one statement on the evidence, three probes, and no walk of the rule. A
// rule the server could not even parse still projects from evidence written
// for it, which only holds because the page never reads the rule's times.
func TestSeriesClockReadCost(t *testing.T) {
	app := testApp(t)
	leader := newTestUser(t, app, "Lou Leader", "password1234", models.RoleMember)
	client := loginAPI(t, app, leader.Name, "password1234")
	group := activeGroup(t, app, leader, "History Circle", models.VisibilityPublic, "")
	rule := "FREQ=YEARLY;BYDAY=MO,TU,WE,TH,FR,SA,SU;BYSETPOS=" + strings.TrimSuffix(strings.Repeat("1,", 100), ",")
	long := models.Event{GroupID: &group.ID, Title: "Old Feast", Visibility: models.VisibilityPublic, StartsLocal: "1827-01-01T09:00",
		DurationMinutes: 60, TZID: "America/Lima", RRule: rule, CreatedBy: leader.ID}
	gormMust(t, app.DB.Create(&long))
	if err := eventdates.Ensure(app.DB, &long, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var occ models.EventOccurrence
	gormMust(t, app.DB.Where("event_id = ? AND starts_at > now()", long.ID).Order("original_date").First(&occ))
	day := tombstoneDate(occ)
	lima, _ := events.LoadZone("America/Lima")
	moveDateTo(t, client, long.ID, day, day+"T15:00")
	r := readSeriesDate(t, client, long.ID, day)
	wantUsual(t, r, events.FormatWall(clockRowsOf(t, app.DB, occ.ID)[0].StartsAt, lima)[11:], changeTime)

	var statements []string
	hook := "test:count_clock_reads"
	if err := app.DB.Callback().Row().After("gorm:row").Register(hook, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "occurrence_series_starts") {
			statements = append(statements, tx.Statement.SQL.String())
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "occurrence_series_starts") || tx.Statement.Table == "occurrence_series_starts" {
			statements = append(statements, tx.Statement.SQL.String())
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = app.DB.Callback().Row().Remove(hook)
		_ = app.DB.Callback().Query().Remove(hook)
	})
	readSeriesDate(t, client, long.ID, day)
	if len(statements) != 1 || strings.Count(statements[0], "FROM occurrence_series_starts") != 3 || !strings.Contains(statements[0], "LIMIT 1") {
		t.Fatalf("the page read the evidence with %d statements: %q", len(statements), statements)
	}
	for _, predicate := range []string{
		"WHERE occurrence_id = $1 AND starts_at = $2",
		"WHERE occurrence_id = $3 AND starts_at >= $4 AND starts_at < $5 ORDER BY starts_at LIMIT 1",
		"WHERE occurrence_id = $6 AND is_first",
	} {
		if !strings.Contains(statements[0], predicate) {
			t.Fatalf("clock lookup lacks bounded predicate %q: %s", predicate, statements[0])
		}
	}
	apiDo(t, client, "GET", dateReadPath(long.ID, day)+"/calendar.ics", nil, http.StatusOK)
	if len(statements) != 1 {
		t.Fatalf("the calendar download read the evidence: %q", statements[1:])
	}

	// The lookup allocates the same for this rule as for a one-row date.
	short := createFeedEvent(t, client, group.ID, "Short", feedStart(3), "FREQ=WEEKLY;COUNT=2")
	shortOcc := seriesClockTestDates(t, app.DB, short.ID)[0]
	measure := func(id uint, at time.Time) float64 {
		return testing.AllocsPerRun(20, func() {
			if _, err := usualClockIn(app.DB, id, at); err != nil {
				t.Fatal(err)
			}
		})
	}
	longAllocs, shortAllocs := measure(occ.ID, occ.StartsAt.Add(6*time.Hour)), measure(shortOcc.ID, shortOcc.StartsAt.Add(time.Hour))
	if longAllocs > shortAllocs*1.5+50 {
		t.Fatalf("the lookup allocates %.0f for the long rule, %.0f for a short one", longAllocs, shortAllocs)
	}

	// An unparseable rule with evidence written for it still projects.
	gormMust(t, app.DB.Exec(`UPDATE events SET rrule = 'FREQ=NOPE' WHERE id = ?`, short.ID))
	gormMust(t, app.DB.Exec(`UPDATE events SET series_clock_dirty = false WHERE id = ?`, short.ID))
	wantUsual(t, readSeriesDate(t, client, short.ID, tombstoneDate(shortOcc)), "19:00")
}
