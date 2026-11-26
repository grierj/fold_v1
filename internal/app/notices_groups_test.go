package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fold/internal/notify"
	"fold/internal/testdb"
	"fold/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// wantGroupDeleted checks that uid got exactly one group.deleted since mark,
// with this title, linking home, that their own feed shows it when client is
// set, and that it was mailed to address with the same text.
func wantGroupDeleted(t *testing.T, app *App, who string, uid uint, client *apiClient, address string, mark noticeMark, title string) models.Notification {
	t.Helper()
	const body = "Its events are off, and you're no longer a member."
	rows := noticesSince(t, app, uid, notify.GroupDeleted, mark.notice)
	if len(rows) != 1 {
		t.Fatalf("%s got %d group deletion notices, want 1: %+v", who, len(rows), rows)
	}
	n := rows[0]
	if n.Title != title || n.Body != body || n.Link != "/" || n.PushTitle != title || n.PushBody != "Its events are off." {
		t.Fatalf("%s's notice: %q / %q -> %q (push %q / %q), want %q / %q -> /", who, n.Title, n.Body, n.Link, n.PushTitle, n.PushBody, title, body)
	}
	if client != nil {
		if item := feedItem(t, client, n.ID); item["title"] != title || item["kind"] != notify.GroupDeleted || item["archived"] != false {
			t.Fatalf("%s's feed item: %v", who, item)
		}
	}
	mailed := false
	for _, m := range mailSince(t, app, address, mark) {
		mailed = mailed || (m.Subject == title && strings.Contains(m.Body, body))
	}
	if !mailed {
		t.Fatalf("%s was not mailed %q: %+v", who, title, mailSince(t, app, address, mark))
	}
	return n
}

// toldOf is every group.deleted and event.deleted written to uid since mark.
func toldOf(t *testing.T, app *App, uid uint, mark noticeMark) []models.Notification {
	t.Helper()
	var rows []models.Notification
	gormMust(t, app.DB.Where("user_id = ? AND id > ? AND kind IN ?", uid, mark.notice, []string{notify.GroupDeleted, notify.EventDeleted}).Find(&rows))
	return rows
}

// groupDeleteFixture is newDateEditFixture's invite-only "Supper club" (place
// "Group hall") with a second event of the group, "Open house", that is
// public, so people who are not members can be signed up for it.
type groupDeleteFixture struct {
	dateEditFixture
	open      eventJSON
	openDates []models.EventOccurrence
}

func newGroupDeleteFixture(t *testing.T) groupDeleteFixture {
	t.Helper()
	f := groupDeleteFixture{dateEditFixture: newDateEditFixture(t)}
	f.open = createFeedEvent(t, f.admin, f.group, "Open house", feedStart(9), "FREQ=WEEKLY;COUNT=3")
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.open.ID).Update("visibility", models.VisibilityPublic))
	gormMust(t, f.app.DB.Where("event_id = ?", f.open.ID).Order("starts_at").Find(&f.openDates))
	if len(f.openDates) != 3 {
		t.Fatalf("fixture: %d open dates", len(f.openDates))
	}
	return f
}

func (f groupDeleteFixture) deletePath() string { return "/api/v1/admin/groups/" + itoa(f.group) }

// Deleting a group tells each person it affects once, in its transaction:
// every member (the leader, a member signed up for four dates of two events,
// a teen) one group.deleted that names the group they may see, and nothing
// else; everyone else signed up for an upcoming date (a waitlisted outsider, a
// guest, a household person's holder, someone who left and kept a date) one
// event.deleted for the event they are on, as an event's own delete words it.
// A guardian gets no copy of the member notice. The admin who deleted it,
// people only on a cancelled or past date and former members are told
// nothing. Every notice is in its recipient's feed and mailed after the group
// row is gone.
func TestDeletingAGroupTellsEachPersonOnce(t *testing.T) {
	f := newGroupDeleteFixture(t)
	app := f.app
	gormMust(t, app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[3].ID).Updates(map[string]any{"status": models.OccurrenceCancelled, "cancel_source": models.CancelSourceManual}))
	past := time.Now().UTC().Add(-48 * time.Hour)
	gormMust(t, app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.openDates[2].ID).Updates(map[string]any{"starts_at": past, "ends_at": past.Add(time.Hour)}))

	for _, i := range []int{0, 1, 2} {
		signUp(t, app, f.memberUser, f.dates[i].ID, models.RegGoing)
	}
	signUp(t, app, f.memberUser, f.openDates[1].ID, models.RegGoing)
	signUp(t, app, f.adminUser, f.dates[0].ID, models.RegGoing)
	signUp(t, app, f.adminUser, f.openDates[0].ID, models.RegGoing)

	waitlisted, cancelledOnly := f.outsiders[0], f.outsiders[1]
	signUp(t, app, waitlisted, f.openDates[0].ID, models.RegWaitlisted)
	signUp(t, app, cancelledOnly, f.dates[3].ID, models.RegGoing)
	pastOnly := newTestUser(t, app, "past only", "password1234", models.RoleMember)
	signUp(t, app, pastOnly, f.openDates[2].ID, models.RegGoing)

	guest := newTestUser(t, app, "open guest", "password1234", models.RoleMember)
	gormMust(t, app.DB.Model(guest).Updates(map[string]any{"status": models.UserGuest, "password_hash": nil}))
	signUp(t, app, guest, f.openDates[1].ID, models.RegGoing)

	holder, holderClient, _, child := householdFamily(t, app, f.admin, "open holder")
	gormMust(t, app.DB.Create(&models.Registration{HouseholdPersonID: &child.ID, RegisteredBy: &holder.ID, OccurrenceID: f.openDates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))

	teen := activeTeen(t, app, "club teen", f.adminUser.ID)
	gormMust(t, app.DB.Create(&models.Membership{UserID: teen.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	signUp(t, app, teen, f.dates[1].ID, models.RegGoing)
	guardian := newTestUser(t, app, "club guardian", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}))
	gormMust(t, app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))

	kept := departedWithGrants(t, f.dateEditFixture, "kept a date", f.dates[2].ID)
	formerLeft := newTestUser(t, app, "former left", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.Membership{UserID: formerLeft.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipLeft}))
	formerRemoved := newTestUser(t, app, "former removed", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.Membership{UserID: formerRemoved.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipRemoved}))

	if got := groupDeleteCount(t, f.admin, f.group); got != 7 {
		t.Fatalf("the count is %d, want 7 (leader, member, teen, waitlisted, guest, holder, kept)", got)
	}
	mark := markNotices(t, app)
	apiDo(t, f.admin, "DELETE", f.deletePath(), nil, http.StatusOK)
	if err := app.DB.First(&models.Group{}, f.group).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("the group is still there: %v", err)
	}
	drainNotices(t, app)
	drainNotices(t, app)

	const club = "Supper club was deleted"
	for _, m := range []struct {
		who    string
		u      *models.User
		client *apiClient
	}{{"the leader", f.leaderUser, f.leader}, {"the member", f.memberUser, f.member}, {"the teen", teen, loginAPI(t, app, teen.Name, "password1234")}} {
		n := wantGroupDeleted(t, app, m.who, m.u.ID, m.client, m.u.Email, mark, club)
		if n.SubjectType != "group" || n.SubjectID == nil || *n.SubjectID != f.group {
			t.Fatalf("%s's notice: subject %q %v", m.who, n.SubjectType, n.SubjectID)
		}
		if got := noticesSince(t, app, m.u.ID, notify.EventDeleted, mark.notice); len(got) != 0 {
			t.Fatalf("%s, a member, was also told of an event: %+v", m.who, got)
		}
	}
	if got := copiesTo(t, app, guardian.Email); len(got) != 0 {
		t.Fatalf("the guardian got copies %v", got)
	}

	zone := churchZoneOrUTC(app.DB)
	starting := func(o models.EventOccurrence) string {
		return "This event was deleted, so its upcoming dates are off, starting " + formatWhen(o.StartsAt, zone) + "."
	}
	wantEventDeleted(t, app, "the waitlisted outsider", waitlisted.ID, loginAPI(t, app, waitlisted.Name, "password1234"), waitlisted.Email, mark, "Cancelled: Open house", starting(f.openDates[0]))
	wantEventDeleted(t, app, "the holder", holder.ID, holderClient, holder.Email, mark, "Cancelled: Open house", starting(f.openDates[0]))
	guestRows := noticesSince(t, app, guest.ID, notify.EventDeleted, mark.notice)
	if len(guestRows) != 1 || guestRows[0].Title != "Cancelled: Open house" {
		t.Fatalf("the guest's notices: %+v", guestRows)
	}
	keptRows := noticesSince(t, app, kept.ID, notify.EventDeleted, mark.notice)
	if len(keptRows) != 1 {
		t.Fatalf("the person who kept a date got %d notices, want 1: %+v", len(keptRows), keptRows)
	}
	for _, who := range []*models.User{waitlisted, holder, guest, kept} {
		if got := noticesSince(t, app, who.ID, notify.GroupDeleted, mark.notice); len(got) != 0 {
			t.Fatalf("%s, not a member, got the member notice: %+v", who.Name, got)
		}
	}
	for _, u := range []*models.User{f.adminUser, cancelledOnly, pastOnly, formerLeft, formerRemoved} {
		if got := toldOf(t, app, u.ID, mark); len(got) != 0 {
			t.Fatalf("%s was told: %+v", u.Name, got)
		}
	}
	if told := toldSince(t, app, notify.GroupDeleted, mark.notice) + toldSince(t, app, notify.EventDeleted, mark.notice); told != 7 {
		t.Fatalf("%d people told, the count said 7", told)
	}
}

// Nobody outside the invite-only group learns its name, place or id: not in
// the bell row, its data, its push text or its email, and not a guardian of a
// teen member. An untitled event of the group is told to an outsider with no
// title at all, never the group's name in its place.
func TestGroupDeleteNamesNoHiddenGroupToOutsiders(t *testing.T) {
	f := newGroupDeleteFixture(t)
	app := f.app
	gormMust(t, app.DB.Model(&models.Event{}).Where("id = ?", f.open.ID).Update("title", ""))
	outsider := f.outsiders[0]
	signUp(t, app, outsider, f.openDates[0].ID, models.RegGoing)
	kept := departedWithGrants(t, f.dateEditFixture, "kept outsider", f.dates[1].ID)
	teen := activeTeen(t, app, "hidden teen", f.adminUser.ID)
	gormMust(t, app.DB.Create(&models.Membership{UserID: teen.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	guardian := newTestUser(t, app, "hidden guardian", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}))
	gormMust(t, app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
	mark := markNotices(t, app)

	apiDo(t, f.admin, "DELETE", f.deletePath(), nil, http.StatusOK)
	drainNotices(t, app)
	drainNotices(t, app)

	secrets := []string{"Supper club", "Group hall", "/groups/", `"groupId"`}
	n := wantEventDeleted(t, app, "the outsider", outsider.ID, loginAPI(t, app, outsider.Name, "password1234"), outsider.Email, mark,
		"Cancelled: Church event", "This event was deleted, so its upcoming dates are off, starting "+formatWhen(f.openDates[0].StartsAt, churchZoneOrUTC(app.DB))+".")
	if n.PushTitle != "Cancelled: Church event" {
		t.Fatalf("the outsider's push: %q", n.PushTitle)
	}
	for _, p := range []struct {
		who string
		u   *models.User
	}{{"the outsider", outsider}, {"the person who kept a date", kept}, {"the guardian", guardian}} {
		noSecrets(t, app, p.who, p.u.ID, p.u.Email, mark, secrets)
	}
	if got := copiesTo(t, app, guardian.Email); len(got) != 0 {
		t.Fatalf("the guardian got copies %v", got)
	}
	wantGroupDeleted(t, app, "the teen", teen.ID, nil, teen.Email, mark, "Supper club was deleted")
}

// A member is told the group's name only when they may see it: an archived
// group is its leaders' alone (policy.CanSeeGroup), so a member of one is
// told "One of your groups" with no name, subject or link, and the leader is
// told its name.
func TestGroupDeleteNamesAnArchivedGroupOnlyToItsLeaders(t *testing.T) {
	f := newGroupDeleteFixture(t)
	gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("status", models.GroupArchived))
	mark := markNotices(t, f.app)
	apiDo(t, f.admin, "DELETE", f.deletePath(), nil, http.StatusOK)
	drainNotices(t, f.app)
	n := wantGroupDeleted(t, f.app, "the member", f.memberUser.ID, f.member, f.memberUser.Email, mark, "One of your groups was deleted")
	if n.SubjectType != "" || n.SubjectID != nil {
		t.Fatalf("the member's notice follows the group: subject %q %v", n.SubjectType, n.SubjectID)
	}
	noSecrets(t, f.app, "the member", f.memberUser.ID, f.memberUser.Email, mark, []string{"Supper club", "Group hall", "/groups/"})
	wantGroupDeleted(t, f.app, "the leader", f.leaderUser.ID, f.leader, f.leaderUser.Email, mark, "Supper club was deleted")
}

// A guardian is never copied on a teen member's group.deleted, by the kind
// (NoGuardianCopy) and not because the group happens to be gone when the copy
// job runs: with the group's row made live again before the teen's email
// goes out, no copy job is queued, and a copy job queued for the notice anyway
// (as an older process would have) sends nothing.
func TestGroupDeletedIsNeverCopiedEvenWithTheGroupLiveAgain(t *testing.T) {
	f := newGroupDeleteFixture(t)
	app := f.app
	teen := activeTeen(t, app, "copied teen", f.adminUser.ID)
	gormMust(t, app.DB.Create(&models.Membership{UserID: teen.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	guardian := newTestUser(t, app, "copied guardian", "password1234", models.RoleMember)
	gormMust(t, app.DB.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}))
	gormMust(t, app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
	mark := markNotices(t, app)

	apiDo(t, f.admin, "DELETE", f.deletePath(), nil, http.StatusOK)
	gormMust(t, app.DB.Unscoped().Model(&models.Group{}).Where("id = ?", f.group).Update("deleted_at", nil))
	gormMust(t, app.DB.Create(&models.Membership{UserID: teen.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	drainNotices(t, app)
	n := wantGroupDeleted(t, app, "the teen", teen.ID, nil, teen.Email, mark, "Supper club was deleted")
	var queued int64
	gormMust(t, app.DB.Model(&models.Job{}).Where("kind = ?", guardianCopyType.Name).Count(&queued))
	if queued != 0 {
		t.Fatalf("%d copy jobs were queued for a kind that is never copied", queued)
	}
	if _, err := guardianCopyType.Enqueue(app.DB, guardianCopyJob{NotificationID: n.ID, GuardianID: guardian.ID}, time.Now().UTC(), guardianCopyKey(n.ID, guardian.ID)); err != nil {
		t.Fatal(err)
	}
	drainNotices(t, app)
	if got := copiesTo(t, app, guardian.Email); len(got) != 0 {
		t.Fatalf("the guardian got copies %v", got)
	}
	noSecrets(t, app, "the guardian", guardian.ID, guardian.Email, mark, []string{"Supper club"})
}

// A refused delete tells nobody and keeps every row: a member's own try (the
// admin routes refuse it) and an admin's on a group that is already gone.
func TestRefusedGroupDeleteTellsNobody(t *testing.T) {
	f := newGroupDeleteFixture(t)
	signUp(t, f.app, f.outsiders[0], f.openDates[0].ID, models.RegGoing)
	before := groupDeleteState(t, f.app, f.group)
	mark := markNotices(t, f.app)
	if w := f.member.do("DELETE", f.deletePath(), nil, true); w.Code != http.StatusForbidden {
		t.Fatalf("a member's delete: %d %s", w.Code, w.Body.String())
	}
	if w := f.admin.do("DELETE", "/api/v1/admin/groups/999999", nil, true); w.Code != http.StatusNotFound {
		t.Fatalf("a missing group's delete: %d %s", w.Code, w.Body.String())
	}
	if got := groupDeleteState(t, f.app, f.group); got != before {
		t.Fatalf("a refused delete changed rows:\n%s\nwant\n%s", got, before)
	}
	if told := toldSince(t, f.app, notify.GroupDeleted, mark.notice) + toldSince(t, f.app, notify.EventDeleted, mark.notice); told != 0 {
		t.Fatalf("a refused delete told %d people", told)
	}
}

// A sign-up committed while the delete waits for the contact gate is told:
// the delete reads who to tell after its locks.
func TestGroupDeleteTellsASignUpCommittedWhileItWaited(t *testing.T) {
	f := newGroupDeleteFixture(t)
	outsider := loginAPI(t, f.app, f.outsiders[0].Name, "password1234")
	mark := markNotices(t, f.app)
	gate := holdContactGate(t, f.app)
	deletion := queueContact(func() *httptest.ResponseRecorder { return f.admin.do("DELETE", f.deletePath(), nil, true) })
	waitContactWaiters(t, f.app, 1)
	apiDo(t, outsider, "PUT", "/api/v1/events/"+itoa(f.open.ID)+"/registrations", map[string]any{"date": models.FormatCalendarDate(f.openDates[0].OriginalDate), "status": models.RegGoing}, http.StatusOK)
	if err := gate.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if w := awaitContactResponse(t, deletion); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if got := noticesSince(t, f.app, f.outsiders[0].ID, notify.EventDeleted, mark.notice); len(got) != 1 {
		t.Fatalf("the sign-up that committed first got %d notices, want 1", len(got))
	}
}

// Every writer that can wait behind a group delete gets a real refusal once
// its lock is granted, never a 500, and leaves no live event under the
// deleted group: a join, a leave, a role change and an admin's add (behind
// the contact gate and the group row), Keep group (behind the group row) and
// a leader's new event find the group gone (404); a leader's add by email
// answers as every unavailable add by email does (400, so it never says
// whether the address has an account); a sign-up and a withdrawal (behind
// their date) find the date gone (404). The hook starts each at the point
// the notices are written, where one that slipped past the read would be lost
// untold or misreported; a withdrawal there was going at the read, so it is
// told.
func TestGroupDeleteSettlesWritersInFlight(t *testing.T) {
	type writer func() *httptest.ResponseRecorder
	answer := func(f groupDeleteFixture, c *apiClient, status string) writer {
		return func() *httptest.ResponseRecorder {
			return c.do("PUT", "/api/v1/events/"+itoa(f.open.ID)+"/registrations",
				map[string]any{"date": models.FormatCalendarDate(f.openDates[0].OriginalDate), "status": status}, true)
		}
	}
	cases := []struct {
		name string
		code int
		told bool
		prep func(f groupDeleteFixture, outsider, joiner *apiClient) writer
	}{
		{"join", http.StatusNotFound, false, func(f groupDeleteFixture, _, joiner *apiClient) writer {
			return func() *httptest.ResponseRecorder {
				return joiner.do("POST", "/api/v1/groups/"+itoa(f.group)+"/join", nil, true)
			}
		}},
		{"leave", http.StatusNotFound, false, func(f groupDeleteFixture, _, _ *apiClient) writer {
			body := departureConfirmBody(t, f.member, "/api/v1/groups/"+itoa(f.group)+"/departure-preview")
			return func() *httptest.ResponseRecorder {
				return f.member.do("POST", "/api/v1/groups/"+itoa(f.group)+"/leave", body, true)
			}
		}},
		{"role change", http.StatusNotFound, false, func(f groupDeleteFixture, _, _ *apiClient) writer {
			return func() *httptest.ResponseRecorder {
				return f.leader.do("PUT", "/api/v1/groups/"+itoa(f.group)+"/members/"+itoa(f.memberUser.ID)+"/role", map[string]any{"role": models.MembershipLeader}, true)
			}
		}},
		{"leader add by email", http.StatusBadRequest, false, func(f groupDeleteFixture, _, _ *apiClient) writer {
			return func() *httptest.ResponseRecorder {
				return f.leader.do("POST", "/api/v1/groups/"+itoa(f.group)+"/members", map[string]any{"email": f.outsiders[1].Email}, true)
			}
		}},
		{"admin add", http.StatusNotFound, false, func(f groupDeleteFixture, _, _ *apiClient) writer {
			return func() *httptest.ResponseRecorder {
				return f.admin.do("POST", "/api/v1/admin/groups/"+itoa(f.group)+"/members", map[string]any{"userId": f.outsiders[1].ID}, true)
			}
		}},
		{"keep", http.StatusNotFound, false, func(f groupDeleteFixture, _, _ *apiClient) writer {
			return func() *httptest.ResponseRecorder {
				return f.admin.do("POST", "/api/v1/admin/groups/"+itoa(f.group)+"/reject-deletion", nil, true)
			}
		}},
		{"event creation", http.StatusNotFound, false, func(f groupDeleteFixture, _, _ *apiClient) writer {
			return func() *httptest.ResponseRecorder {
				return f.leader.do("POST", "/api/v1/groups/"+itoa(f.group)+"/events", map[string]any{"title": "Late", "startsLocal": feedStart(4).Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC"}, true)
			}
		}},
		{"sign-up", http.StatusNotFound, false, func(f groupDeleteFixture, outsider, _ *apiClient) writer {
			return answer(f, outsider, models.RegGoing)
		}},
		{"withdrawal", http.StatusNotFound, true, func(f groupDeleteFixture, outsider, _ *apiClient) writer {
			return answer(f, outsider, models.RegDeclined)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupDeleteFixture(t)
			gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Updates(map[string]any{"visibility": models.VisibilityPublic, "deletion_requested": true}))
			if tc.told {
				signUp(t, f.app, f.outsiders[0], f.openDates[0].ID, models.RegGoing)
			}
			run := tc.prep(f, loginAPI(t, f.app, f.outsiders[0].Name, "password1234"), loginAPI(t, f.app, f.outsiders[1].Name, "password1234"))
			mark := markNotices(t, f.app)
			var done <-chan *httptest.ResponseRecorder
			f.app.hooks.deleteNoticed = func(*gorm.DB) {
				done = queueContact(run)
				testdb.WaitUntil(t, "the writer to wait on a lock or return", 5*time.Second, func() bool {
					var n int64
					gormMust(t, f.app.DB.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n))
					return n > 0 || len(done) > 0
				})
			}
			apiDo(t, f.admin, "DELETE", f.deletePath(), nil, http.StatusOK)
			f.app.hooks.deleteNoticed = nil
			w := awaitContactResponse(t, done)
			if w.Code != tc.code {
				t.Fatalf("the %s returned %d once the group was deleted, want %d: %s", tc.name, w.Code, tc.code, w.Body.String())
			}
			var live int64
			gormMust(t, f.app.DB.Model(&models.Event{}).Where("group_id = ?", f.group).Count(&live))
			if live != 0 {
				t.Fatalf("%d live events are left under the deleted group", live)
			}
			if told := len(noticesSince(t, f.app, f.outsiders[0].ID, notify.EventDeleted, mark.notice)) == 1; told != tc.told {
				t.Fatalf("the outsider was told %v, want %v", told, tc.told)
			}
		})
	}
}

// The delete takes its events' rows before their dates', the order an
// ungated room pick takes (the event, then the date): a pick holding an event
// finds the delete waiting on that event and holding none of its dates, so
// the pick can take its date and finish, and neither waits on the other.
func TestGroupDeleteTakesItsEventsBeforeTheirDates(t *testing.T) {
	f := newGroupDeleteFixture(t)
	pick := f.app.DB.Begin()
	if pick.Error != nil {
		t.Fatal(pick.Error)
	}
	defer pick.Rollback()
	var ev models.Event
	gormMust(t, pick.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ev, f.open.ID))
	deletion := queueContact(func() *httptest.ResponseRecorder { return f.admin.do("DELETE", f.deletePath(), nil, true) })
	waitLockWaiters(t, f.app)
	var date models.EventOccurrence
	if err := pick.Raw("SELECT * FROM event_occurrences WHERE id = ? FOR UPDATE NOWAIT", f.openDates[0].ID).Scan(&date).Error; err != nil {
		t.Fatalf("the delete holds a date while it waits for its event: %v", err)
	}
	if err := pick.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if w := awaitContactResponse(t, deletion); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
}

// groupDeleteCount is what GET /admin/groups/:id/delete-count answers c.
func groupDeleteCount(t *testing.T, c *apiClient, groupID uint) int {
	t.Helper()
	var out signedUpCountJSON
	gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(apiDo(t, c, "GET", "/api/v1/admin/groups/"+itoa(groupID)+"/delete-count", nil, http.StatusOK)), &out)})
	return out.People
}

// The confirmation's count is the people the delete then tells, for none, one
// and many. It leaves out the admin who presses, their own membership and the
// sign-ups they made for their household, and counts a member who is also
// signed up once.
func TestGroupDeleteCountIsWhoTheDeleteTells(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{{"only the admin", 0}, {"one member", 1}, {"many", 4}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupDeleteFixture(t)
			app := f.app
			spouse := addMyPerson(t, f.admin, map[string]any{"firstName": "Mary", "kind": "adult", "birthDate": "1990-01-01"})
			batchAnswer(t, f.admin, f.open.ID, models.FormatCalendarDate(f.openDates[0].OriginalDate), map[string]any{"people": []map[string]any{{"personId": spouse.ID, "status": models.RegGoing}}}, http.StatusOK)
			signUp(t, app, f.adminUser, f.dates[0].ID, models.RegGoing)
			switch tc.name {
			case "only the admin":
				gormMust(t, app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id <> ?", f.group, f.adminUser.ID).Update("status", models.MembershipLeft))
			case "one member":
				gormMust(t, app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.group, f.leaderUser.ID).Update("status", models.MembershipLeft))
				signUp(t, app, f.memberUser, f.dates[0].ID, models.RegGoing)
				signUp(t, app, f.memberUser, f.openDates[1].ID, models.RegGoing)
			case "many":
				signUp(t, app, f.memberUser, f.dates[0].ID, models.RegGoing)
				signUp(t, app, f.outsiders[0], f.openDates[0].ID, models.RegGoing)
				signUp(t, app, f.outsiders[0], f.openDates[1].ID, models.RegWaitlisted)
				departedWithGrants(t, f.dateEditFixture, "kept count", f.dates[1].ID)
			}
			if got := groupDeleteCount(t, f.admin, f.group); got != tc.want {
				t.Fatalf("the count is %d, want %d", got, tc.want)
			}
			mark := markNotices(t, app)
			apiDo(t, f.admin, "DELETE", f.deletePath(), nil, http.StatusOK)
			if told := toldSince(t, app, notify.GroupDeleted, mark.notice) + toldSince(t, app, notify.EventDeleted, mark.notice); told != tc.want {
				t.Fatalf("the delete told %d people, its count said %d", told, tc.want)
			}
			if got := toldOf(t, app, f.adminUser.ID, mark); len(got) != 0 {
				t.Fatalf("the admin who deleted it was told: %+v", got)
			}
		})
	}
}

// Only an admin may read the count, in one snapshot that acknowledges the
// viewer it judged; a group that is not there is missing, and an admin whose
// session ended before the snapshot is refused.
func TestGroupDeleteCountIsAnAdminsRead(t *testing.T) {
	f := newGroupDeleteFixture(t)
	admin := pinnedAs(t, f.admin)
	path := "/api/v1/admin/groups/" + itoa(f.group) + "/delete-count"
	w := admin.do("GET", path, nil, false)
	if w.Code != http.StatusOK || w.Body.String() != `{"people":2}` {
		t.Fatalf("the admin's count: %d %s", w.Code, w.Body.String())
	}
	wantAck(t, "the admin's count", w, admin, f.adminUser)
	for _, c := range []*apiClient{f.leader, f.member} {
		if w := c.do("GET", path, nil, false); w.Code != http.StatusForbidden {
			t.Fatalf("a non-admin's count: %d %s", w.Code, w.Body.String())
		}
	}
	if w := f.admin.do("GET", "/api/v1/admin/groups/999999/delete-count", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("a missing group's count: %d %s", w.Code, w.Body.String())
	}
	other := newTestUser(t, f.app, "other count admin", "password1234", models.RoleAdmin)
	otherClient := loginAPI(t, f.app, other.Name, "password1234")
	var fired bool
	f.app.hooks.readerBegin = func(surface string) {
		if surface == "group delete count" && !fired {
			fired = true
			apiDo(t, otherClient, "POST", "/api/v1/admin/users/"+itoa(f.adminUser.ID)+"/password", map[string]string{"password": "replacement password"}, http.StatusOK)
		}
	}
	defer func() { f.app.hooks.readerBegin = nil }()
	w = admin.do("GET", path, nil, false)
	if !fired {
		t.Fatal("the snapshot never started")
	}
	wantCode(t, "a session ended before the snapshot", w, http.StatusUnauthorized, "signed_out")
	wantNoAck(t, "a session ended before the snapshot", w)

	// A role read in the snapshot decides, not the middleware's: an admin
	// demoted before it (here without the session bump a demotion makes) is
	// refused too.
	demoted := newTestUser(t, f.app, "demoted count admin", "password1234", models.RoleAdmin)
	demotedClient := loginAPI(t, f.app, demoted.Name, "password1234")
	f.app.hooks.readerBegin = func(surface string) {
		if surface == "group delete count" {
			gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", demoted.ID).Update("role", models.RoleMember))
		}
	}
	wantCode(t, "an admin demoted before the snapshot", demotedClient.do("GET", path, nil, false), http.StatusUnauthorized, "signed_out")
}
