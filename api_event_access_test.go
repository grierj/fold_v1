package main

import (
	"encoding/json"
	"fmt"
	"fold/internal/audit"
	"fold/internal/notify"
	"fold/models"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestEventMorePrivateCancelsShutOutRegistrants(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	gid := createGroupAs(t, admin, "Garden Club", models.VisibilityPublic)
	mem := newTestUser(t, app, "mem", "password1234", models.RoleMember)
	addMemberEmail(t, admin, gid, mem.Email)
	memC := loginAPI(t, app, "mem", "password1234")
	out := newTestUser(t, app, "out", "password1234", models.RoleMember)
	outC := loginAPI(t, app, "out", "password1234")
	ev := scheduleGroupEvent(t, admin, gid, "Picnic")
	register(t, memC, ev.ID)
	register(t, outC, ev.ID)

	// Preview first, like a group change: the outsider is counted.
	var impact visibilityImpactJSON
	decodeJSON(t, admin.do("GET", fmt.Sprintf("/api/v1/events/%d/visibility-impact?visibility=invite_only", ev.ID), nil, false), &impact)
	if len(impact.Tightened) != 1 || impact.Tightened[0].Registrants != 1 || len(impact.LeaderOpened) != 0 {
		t.Fatalf("impact: %+v", impact)
	}
	apiDo(t, memC, "GET", fmt.Sprintf("/api/v1/events/%d/visibility-impact?visibility=invite_only", ev.ID), nil, http.StatusForbidden)
	// Without confirm nothing is saved.
	setVisibility(t, admin, ev.ID, models.VisibilityInviteOnly, "", http.StatusConflict)
	if e := eventRow(t, app, ev.ID); e.Visibility != models.VisibilityPublic || regCount(app, out, ev.ID) != 1 {
		t.Fatalf("saved without confirm: %+v", e)
	}
	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), confirmVisibility(t, admin, fmt.Sprintf("/api/v1/events/%d/visibility", ev.ID), map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	if regCount(app, out, ev.ID) != 1 || regCount(app, mem, ev.ID) != 1 {
		t.Fatalf("registrations: out %d, mem %d", regCount(app, out, ev.ID), regCount(app, mem, ev.ID))
	}
	found := false
	for _, n := range noticeIDs(t, outC) {
		if n.Kind == "registration.cancelled" {
			found = true
		}
	}
	if !found {
		t.Fatal("outsider was not told their registration was cancelled")
	}
}

// An opened invite-only group's event must not reveal the group's meeting
// place (the default event location) or, in a cancellation notice, the
// group's name, place, or page to someone outside the group.
func TestOpenedEventHidesGroupLocation(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	gid := createGroupAs(t, admin, "Hidden Care", models.VisibilityInviteOnly)
	if err := app.DB.Model(&models.Group{}).Where("id = ?", gid).Update("location", "12 Elm St").Error; err != nil {
		t.Fatal(err)
	}
	ev := scheduleGroupEvent(t, admin, gid, "Open House")
	setVisibility(t, admin, ev.ID, models.VisibilityPublic, "", http.StatusOK)

	newTestUser(t, app, "outsider", "password1234", models.RoleMember)
	outsider := loginAPI(t, app, "outsider", "password1234")
	_, raw := getEvent(t, outsider, ev.ID, http.StatusOK)
	if strings.Contains(raw, "12 Elm St") {
		t.Fatalf("outsider saw the group's location: %s", raw)
	}
	if list := outsider.do("GET", "/api/v1/events", nil, false).Body.String(); strings.Contains(list, "12 Elm St") {
		t.Fatalf("open list leaked the group's location: %s", list)
	}
	_, adminRaw := getEvent(t, admin, ev.ID, http.StatusOK)
	if !strings.Contains(adminRaw, "12 Elm St") {
		t.Fatalf("admin should still see the default location: %s", adminRaw)
	}

	apiDo(t, outsider, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", ev.ID), map[string]any{
		"status": "going", "date": "2026-12-01",
	}, http.StatusOK)
	if err := app.DB.Model(&models.Event{}).Where("id = ?", ev.ID).Update("title", "").Error; err != nil {
		t.Fatal(err)
	}
	apiDo(t, admin, "POST", fmt.Sprintf("/api/v1/events/%d/cancel", ev.ID), map[string]any{"date": "2026-12-01"}, http.StatusOK)

	var found bool
	for _, n := range noticeIDs(t, outsider) {
		if n.Kind != "event.cancelled" {
			continue
		}
		found = true
		b, _ := json.Marshal(n)
		s := string(b)
		if strings.Contains(s, "Hidden Care") || strings.Contains(s, "12 Elm St") || strings.Contains(s, "/groups/") {
			t.Fatalf("cancel notice revealed the group: %s", s)
		}
		if n.Link != fmt.Sprintf("/events/%d", ev.ID) {
			t.Fatalf("link: %s", n.Link)
		}
	}
	if !found {
		t.Fatal("outsider got no cancellation notice")
	}
}

func TestVisibilityOverrideNotice(t *testing.T) {
	app := testApp(t)
	adminUser := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	_ = adminUser
	gid := createGroupAs(t, admin, "Care", models.VisibilityInviteOnly)

	leaderUser := newTestUser(t, app, "leader2", "password1234", models.RoleMember)
	leaderID := addMemberEmail(t, admin, gid, leaderUser.Email)
	apiDo(t, admin, "PUT", fmt.Sprintf("/api/v1/groups/%d/members/%d/role", gid, leaderID), map[string]any{"role": "leader"}, http.StatusOK)
	leader := loginAPI(t, app, "leader2", "password1234")

	newTestUser(t, app, "otheradmin", "password1234", models.RoleAdmin)
	other := loginAPI(t, app, "otheradmin", "password1234")

	pat := newTestUser(t, app, "pat", "password1234", models.RoleMember)
	addMemberEmail(t, admin, gid, pat.Email)
	patC := loginAPI(t, app, "pat", "password1234")

	newTestUser(t, app, "rando", "password1234", models.RoleMember)
	randoC := loginAPI(t, app, "rando", "password1234")

	ev := scheduleGroupEvent(t, admin, gid, "Potluck")
	if ev.Visibility != models.VisibilityInviteOnly || ev.LeaderOpened || ev.ChurchWide {
		t.Fatalf("new group event: %+v", ev)
	}

	raw := setVisibility(t, leader, ev.ID, models.VisibilityPublic, "friends can come", http.StatusOK)
	if strings.Contains(raw, "@") {
		t.Fatalf("visibility response leaked contact: %s", raw)
	}

	var rows []models.AuditLog
	if err := app.DB.Where("action = ? AND target_id = ?", audit.VisibilityOverride, ev.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ActorID == nil || *rows[0].ActorID != leaderUser.ID {
		t.Fatalf("override audit: %+v", rows)
	}
	var meta struct {
		Reason          string `json:"reason"`
		GroupVisibility string `json:"groupVisibility"`
		NewVisibility   string `json:"newVisibility"`
		OldVisibility   string `json:"oldVisibility"`
	}
	if err := json.Unmarshal([]byte(rows[0].MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Reason != "friends can come" || meta.GroupVisibility != "invite_only" || meta.NewVisibility != "public" || meta.OldVisibility != "invite_only" {
		t.Fatalf("meta: %+v", meta)
	}

	for _, c := range []*apiClient{admin, other} {
		if !hasVisibilityNotice(noticeIDs(t, c), "Potluck", "friends can come") {
			t.Fatalf("missing notice: %+v", noticeIDs(t, c))
		}
	}
	for _, c := range []*apiClient{leader, patC, randoC} {
		if hasVisibilityNotice(noticeIDs(t, c), "Potluck", "friends can come") {
			t.Fatalf("unexpected notice: %+v", noticeIDs(t, c))
		}
	}
	adminNotes := noticeIDs(t, admin)
	for _, n := range adminNotes {
		if n.Kind == "event.visibility_override" && strings.Contains(n.Body, "@") {
			t.Fatalf("notice has contact: %+v", n)
		}
		if n.Kind == "event.visibility_override" && !strings.Contains(n.Body, "leader2") {
			t.Fatalf("notice should name the actor: %+v", n)
		}
	}

	if page, _ := getEvent(t, admin, ev.ID, http.StatusOK); !page.Event.LeaderOpened {
		t.Fatalf("opened wider should set the marker: %+v", page.Event)
	}

	// Going back to the group's value is audited, clears the marker, and
	// does not add another override notice.
	setVisibility(t, leader, ev.ID, models.VisibilityInviteOnly, "", http.StatusOK)
	var changed int64
	app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_id = ?", audit.VisibilityChanged, ev.ID).Count(&changed)
	if changed != 1 {
		t.Fatalf("changed rows: %d", changed)
	}
	var overrides int64
	app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_id = ?", audit.VisibilityOverride, ev.ID).Count(&overrides)
	if overrides != 1 {
		t.Fatalf("override rows: %d", overrides)
	}
	page, _ := getEvent(t, admin, ev.ID, http.StatusOK)
	if page.Event.Visibility != models.VisibilityInviteOnly || page.Event.LeaderOpened {
		t.Fatalf("back to group: %+v", page.Event)
	}
}

func TestOpenedEventHidesGroupAndNames(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	gid := createGroupAs(t, admin, "Hidden Care", models.VisibilityInviteOnly)
	sam := newTestUser(t, app, "secretsam", "password1234", models.RoleMember)
	if err := app.DB.Model(sam).Update("name", "Secret Sam").Error; err != nil {
		t.Fatal(err)
	}
	addMemberEmail(t, admin, gid, sam.Email)
	samC := loginAPI(t, app, "secretsam", "password1234")

	ev := scheduleGroupEvent(t, admin, gid, "Open House")
	apiDo(t, samC, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", ev.ID), map[string]any{
		"status": "going", "date": "2026-12-01",
	}, http.StatusOK)
	setVisibility(t, admin, ev.ID, models.VisibilityPublic, "", http.StatusOK)

	outsiderUser := newTestUser(t, app, "outsider", "password1234", models.RoleMember)
	outsider := loginAPI(t, app, "outsider", "password1234")
	page, raw := getEvent(t, outsider, ev.ID, http.StatusOK)
	if page.Event.HostedBy != "Fold" || page.Event.GroupID != nil || page.Event.GroupName != "" {
		t.Fatalf("event identity: %+v", page.Event)
	}
	if strings.Contains(raw, "Secret Sam") || strings.Contains(raw, "Hidden Care") || strings.Contains(raw, "@") {
		t.Fatalf("outsider payload named the group or a member: %s", raw)
	}
	if len(page.Occurrences) != 1 || page.Occurrences[0].AttendeesVisible || page.Occurrences[0].HostedBy != "Fold" {
		t.Fatalf("occurrence: %+v", page.Occurrences)
	}
	if page.Occurrences[0].Registrations["going"] != 1 {
		t.Fatalf("count: %+v", page.Occurrences[0].Registrations)
	}

	// The group itself stays hidden, and registering does not join it.
	apiDo(t, outsider, "GET", "/api/v1/groups/"+itoa(gid), nil, http.StatusNotFound)
	apiDo(t, outsider, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", ev.ID), map[string]any{
		"status": "maybe", "date": "2026-12-01",
	}, http.StatusOK)
	var n int64
	app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", outsiderUser.ID, gid).Count(&n)
	if n != 0 {
		t.Fatalf("registering created a membership")
	}

	memberPage, memberRaw := getEvent(t, samC, ev.ID, http.StatusOK)
	if memberPage.Event.GroupID == nil || memberPage.Event.GroupName != "Hidden Care" {
		t.Fatalf("member should see the group: %+v", memberPage.Event)
	}
	if len(memberPage.Occurrences) != 1 || !memberPage.Occurrences[0].AttendeesVisible || !strings.Contains(memberRaw, "Secret Sam") {
		t.Fatalf("member names: %s", memberRaw)
	}

	// A signed-in outsider of an ordinary public event may see adult names,
	// and that event is not duplicated on the home list.
	pub := createGroupAs(t, admin, "Public Choir", models.VisibilityPublic)
	ordinary := scheduleGroupEvent(t, admin, pub, "Sunday")
	apiDo(t, outsider, "PUT", fmt.Sprintf("/api/v1/events/%d/registrations", ordinary.ID), map[string]any{
		"status": "going", "date": "2026-12-01",
	}, http.StatusOK)
	ordinaryPage, ordinaryRaw := getEvent(t, samC, ordinary.ID, http.StatusOK)
	if !ordinaryPage.Occurrences[0].AttendeesVisible || !strings.Contains(ordinaryRaw, "outsider") {
		t.Fatalf("ordinary public names: %s", ordinaryRaw)
	}

	w := outsider.do("GET", "/api/v1/events", nil, false)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	list := w.Body.String()
	if !strings.Contains(list, "Open House") || strings.Contains(list, "Sunday") || strings.Contains(list, "Hidden Care") {
		t.Fatalf("open list: %s", list)
	}

	guest := newAPIClient(t, app)
	guestPage, guestRaw := getEvent(t, guest, ev.ID, http.StatusOK)
	if guestPage.Occurrences[0].AttendeesVisible || strings.Contains(guestRaw, "Secret Sam") || guestPage.Event.HostedBy != "Fold" {
		t.Fatalf("guest: %s", guestRaw)
	}
	setVisibility(t, admin, ev.ID, models.VisibilityPrivate, "", http.StatusOK)
	getEvent(t, guest, ev.ID, http.StatusNotFound)
	privatePage, privateRaw := getEvent(t, outsider, ev.ID, http.StatusOK)
	if privatePage.Occurrences[0].AttendeesVisible || strings.Contains(privateRaw, "Secret Sam") {
		t.Fatalf("opened private names: %s", privateRaw)
	}
}

// M1.2 acceptance: a visibility-override notice reaches the other leaders
// and the admins in the bell and by email, with one notification_deliveries
// row each.
func TestVisibilityOverrideEmailsAdminsAndLeaders(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "root", "password1234", models.RoleAdmin)
	lead1 := newTestUser(t, app, "lead1", "password1234", models.RoleMember)
	lead2 := newTestUser(t, app, "lead2", "password1234", models.RoleMember)
	lc1 := loginAPI(t, app, "lead1", "password1234")

	g := newTestGroup(t, app, lead1, "Youth", models.GroupActive)
	app.DB.Model(g).Update("visibility", models.VisibilityPrivate)
	addMemberEmail(t, lc1, g.ID, "lead2@example.com")
	apiDo(t, lc1, "PUT", "/api/v1/groups/"+itoa(g.ID)+"/members/"+itoa(lead2.ID)+"/role",
		map[string]string{"role": "leader"}, http.StatusOK)

	ev := scheduleGroupEvent(t, lc1, g.ID, "Lock-in")
	setVisibility(t, lc1, ev.ID, models.VisibilityPublic, "invite the neighbors", http.StatusOK)

	for _, who := range []struct{ name, email string }{
		{"root", "root@example.com"},
		{"lead2", "lead2@example.com"},
	} {
		c := loginAPI(t, app, who.name, "password1234")
		found := false
		for _, n := range feed(t, c, "") {
			if n.Kind == "event.visibility_override" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no bell item", who.name)
		}
		m := latestEmail(t, app, who.email)
		if !strings.Contains(m.Subject, "Event opened up") {
			t.Fatalf("%s: subject %q", who.name, m.Subject)
		}
		if !strings.Contains(m.Body, "/groups/") {
			t.Fatalf("%s: no group link in email", who.name)
		}
	}

	var n int64
	app.DB.Model(&models.NotificationDelivery{}).
		Joins("JOIN notifications ON notifications.id = notification_deliveries.notification_id").
		Where("notifications.kind = ? AND notification_deliveries.channel = ? AND notification_deliveries.status = ?",
			notify.VisibilityOverride, models.DeliveryEmail, models.DeliverySent).
		Count(&n)
	// The actor (lead1) is skipped: root and lead2 only.
	if n != 2 {
		t.Fatalf("email delivery rows: %d, want 2", n)
	}
}

// eventCandidates is the shared candidate loader (#75): a soft-deleted group's
// events are never listed, not even for an admin, and a church-wide
// public event is listed for a signed-out visitor.
func TestEventCandidatesSkipsDeletedGroups(t *testing.T) {
	app := testApp(t)
	admin := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	g := models.Group{Name: "Gone", Status: models.GroupActive, Visibility: models.VisibilityPublic, LeaderID: admin.ID}
	if err := app.DB.Create(&g).Error; err != nil {
		t.Fatal(err)
	}
	ev, _ := seedEvent(t, app, &g, admin.ID, time.Now().Add(48*time.Hour))
	cw := models.Event{Visibility: models.VisibilityPublic, StartsLocal: "2030-01-01T10:00", DurationMinutes: 60, TZID: "UTC", CreatedBy: admin.ID, Title: "All church"}
	if err := app.DB.Create(&cw).Error; err != nil {
		t.Fatal(err)
	}
	if vs, _ := app.eventCandidates(admin, app.DB); vs.ByID[ev.ID] == nil || vs.ByID[cw.ID] == nil {
		t.Fatalf("admin should see both before the delete: %v", vs.IDs)
	}
	if err := app.DB.Delete(&g).Error; err != nil {
		t.Fatal(err)
	}
	vs, err := app.eventCandidates(admin, app.DB)
	if err != nil || vs.ByID[ev.ID] != nil {
		t.Fatalf("deleted group's event listed for an admin: %v %v", vs.IDs, err)
	}
	if anon, _ := app.eventCandidates(nil, app.DB); anon.ByID[cw.ID] == nil {
		t.Fatalf("public church-wide event missing for a visitor: %v", anon.IDs)
	}
	// The admin "show deleted events" switch lists the deleted group's event
	// for an admin and for nobody else (DEC-68).
	if withDeleted, err := deletedEventCandidatesIn(app.DB, admin, app.DB); err != nil || withDeleted.ByID[ev.ID] == nil {
		t.Fatalf("admin with the deleted switch should see the deleted group's event: %v %v", withDeleted.IDs, err)
	}
	member := newTestUser(t, app, "member", "password1234", models.RoleMember)
	if other, err := deletedEventCandidatesIn(app.DB, member, app.DB); err != nil || other.ByID[ev.ID] != nil {
		t.Fatalf("a non-admin must not get deleted groups' events: %v %v", other.IDs, err)
	}
}

// N-17 (inventory 9.2.2, a gated entry): making an event more private cancels
// the sign-ups of people who could not see it any more and tells each of them.
// The real operation is PUT /events/:id/visibility {visibility: invite_only,
// confirm} -> apiSetEventVisibility -> contactMutation (gate first, with the
// event id) -> the event row FOR UPDATE -> cancelLosers: the losing registrants
// are found (a non-member of a group whose event is now invite-only), their
// registrations cancelled with an audit row, and notifyRegistrationCancelled
// tells each one, preceded by a read of the person that skips someone who no
// longer exists or is disabled. Tightening with a confirm answers 200.
//
// The two outsiders (the victim and the kept one) lose; the group member does
// not. Sender first: the tightening stops after it wrote the victim's notice;
// the real delete waits on the gate and removes it. Deletion first: losersForDate
// joins users with a raw JOIN (no deleted_at filter), so the erased holder's
// sign-up can still be listed, cancelled and audited; notifyRegistrationCancelled
// then reads the holder with userIn, which returns nil for a soft-deleted user,
// and returns before Send. This order therefore proves the outcome (200, the
// event tightened, the kept outsider told, no notice recorded for the victim)
// but not the recipient lock inside Send, which the sender-first order and the
// plain-entry tests exercise. Not covered: the group-level tightening
// (apiUpdateGroup) and the guarded date edit (editDateIn), which reach the
// same cancellation of the checked losers.
func TestRegistrationCancelledNoticeSerializesWithRealAdminDelete(t *testing.T) {
	runGatedErasureBarriers(t, 7402125, 7402126, func(t *testing.T) gatedErasureScene {
		f := newRegistrantRace(t)
		return gatedErasureScene{app: f.app, victim: f.victim, sends: []string{notify.RegistrationCancelled},
			run: func() error {
				path := "/api/v1/events/" + itoa(f.eventID) + "/visibility"
				return doExpect(f.admin, "PUT", path,
					confirmVisibility(t, f.admin, path, map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
			},
			erase: eraseVia(f.eraser, f.victim),
			told: func(t *testing.T) {
				if e := eventRow(t, f.app, f.eventID); e.Visibility != models.VisibilityInviteOnly {
					t.Fatalf("event visibility %q, want invite_only", e.Visibility)
				}
				wantNotices(t, f.app, notify.RegistrationCancelled, map[*models.User]int64{f.kept: 1, f.member: 0})
				if regCount(f.app, f.member, f.eventID) != 1 {
					t.Fatal("the group member's sign-up should have stayed")
				}
			},
		}
	})
}

// eventWriterRace keeps notice jobs queued so refusal snapshots cover every
// effect of the writer, without a delivery worker changing rows between reads.
type eventWriterRace struct {
	app          *App
	admin, actor *apiClient
	user         *models.User
	group        *models.Group
	event        models.Event
	date         string
}

func newEventWriterRace(t *testing.T, adminActor bool) eventWriterRace {
	t.Helper()
	app := testApp(t)
	owner := newTestUser(t, app, "owner", "password1234", models.RoleAdmin)
	admin := loginHeld(t, app, owner.Email, "password1234")
	g := activeGroup(t, app, owner, "Choir", models.VisibilityPublic, "")
	role := models.RoleMember
	if adminActor {
		role = models.RoleAdmin
	}
	actor := newTestUser(t, app, "writer", "password1234", role)
	if !adminActor {
		addMember(t, app, actor, &g, models.MembershipLeader)
	}
	client := loginHeld(t, app, actor.Email, "password1234")
	ev := requireCreatedEvent(t, app, admin.do("POST", "/api/v1/groups/"+itoa(g.ID)+"/events", createEventBodyForTest(), true))
	var occ models.EventOccurrence
	if err := app.DB.Where("event_id = ?", ev.ID).First(&occ).Error; err != nil {
		t.Fatal(err)
	}
	return eventWriterRace{app: app, admin: admin, actor: client, user: actor, group: &g, event: ev,
		date: models.FormatCalendarDate(occ.OriginalDate)}
}

func (f eventWriterRace) revoke(t *testing.T, change string) {
	t.Helper()
	switch change {
	case "demoted admin":
		apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(f.user.ID), adminEditBody(f.admin, f.user.ID, map[string]any{"role": models.RoleMember}), http.StatusOK)
	case "disabled account":
		apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(f.user.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
	case "removed leader":
		path := "/api/v1/groups/" + itoa(f.group.ID) + "/members/" + itoa(f.user.ID) + "/departure"
		preview := getDeparturePreview(t, f.admin, path+"-preview", http.StatusOK)
		apiDo(t, f.admin, "POST", path, map[string]any{"snapshot": preview.Snapshot, "keepOccurrenceIds": []uint{}}, http.StatusOK)
	default:
		t.Fatalf("unknown event writer revocation %q", change)
	}
}

func eventWriterState(t *testing.T, app *App, id uint) string {
	t.Helper()
	var state struct {
		Event         models.Event
		Dates         []models.EventOccurrence
		Registrations []models.Registration
		Bookings      []models.RoomBooking
		Counts        map[string]int64
	}
	if err := app.DB.Unscoped().First(&state.Event, id).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Where("event_id = ?", id).Order("id").Find(&state.Dates).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Where("occurrence_id IN (SELECT id FROM event_occurrences WHERE event_id = ?)", id).
		Order("id").Find(&state.Registrations).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Order("id").Find(&state.Bookings).Error; err != nil {
		t.Fatal(err)
	}
	state.Counts = createEventWriteCounts(t, app)
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEventVisibilityRechecksAfterHeldBody(t *testing.T) {
	for _, change := range []string{"demoted admin", "removed leader", "disabled account"} {
		for _, noOp := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-op=%v", change, noOp), func(t *testing.T) {
				f := newEventWriterRace(t, change == "demoted admin")
				path := "/api/v1/events/" + itoa(f.event.ID) + "/visibility"
				apiDo(t, f.admin, "PUT", path, visibilityBody(t, f.admin, path, map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
				vis := models.VisibilityPublic
				if noOp {
					vis = models.VisibilityInviteOnly
				}
				held := f.actor.doHeld("PUT", path, visibilityBody(t, f.actor, path, map[string]any{"visibility": vis, "consent": ""}))
				f.revoke(t, change)
				before := eventWriterState(t, f.app, f.event.ID)
				held.release()
				w := held.wait(t)
				want := http.StatusUnauthorized
				if change == "removed leader" {
					want = http.StatusForbidden
				}
				if w.Code != want {
					t.Fatalf("visibility after %s: %d %s, want %d", change, w.Code, w.Body.String(), want)
				}
				if after := eventWriterState(t, f.app, f.event.ID); before != after {
					t.Fatalf("refused visibility write changed rows:\nbefore %s\nafter %s", before, after)
				}
			})
		}
	}
}

func TestEventVisibilityUsesGroupAfterHeldBody(t *testing.T) {
	f := newEventWriterRace(t, false)
	path := "/api/v1/events/" + itoa(f.event.ID) + "/visibility"
	// Already invite only, so the group's tightening leaves the event row as
	// it was. The token covers the group's audience (DEC-163), so the held body
	// carries the one the page reads after the tightening: the handler loads
	// its first facts before the group changes and only the locked ones are
	// fresh.
	apiDo(t, f.admin, "PUT", path, confirmVisibility(t, f.admin, path, map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	stored := eventRow(t, f.app, f.event.ID)
	after := f.app.eventRevision(&stored, &models.Group{Visibility: models.VisibilityInviteOnly})
	held := f.actor.doHeld("PUT", path, map[string]any{"visibility": models.VisibilityPrivate, "consent": "", "reason": "Welcome friends", "revision": after})
	groupPath := "/api/v1/groups/" + itoa(f.group.ID)
	apiDo(t, f.admin, "PUT", groupPath, confirmVisibility(t, f.admin, groupPath, map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	if read := seriesRevisionOf(t, f.actor, f.event.ID); read != after {
		t.Fatalf("the page reads %q after the tightening, the held body carries %q", read, after)
	}
	held.release()
	w := held.wait(t)
	if w.Code != http.StatusOK {
		t.Fatalf("visibility after group tightening: %d %s", w.Code, w.Body.String())
	}
	var answer eventPage
	decodeJSON(t, w, &answer)
	ev := eventRow(t, f.app, f.event.ID)
	if ev.Visibility != models.VisibilityPrivate || !ev.LeaderOpened || answer.Event.Visibility != ev.Visibility || !answer.Event.LeaderOpened {
		t.Fatalf("fresh group visibility not used in saved row or response: %+v %+v", ev, answer.Event)
	}
	var row models.AuditLog
	if err := f.app.DB.Where("target_type = 'event' AND target_id = ? AND action = ?", ev.ID, audit.VisibilityOverride).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(row.MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["groupVisibility"] != models.VisibilityInviteOnly || meta["leaderOpened"] != true || row.ActorID == nil || *row.ActorID != f.user.ID {
		t.Fatalf("override audit used stale facts: %+v %v", row, meta)
	}
	if n := contactCount(t, f.app, &models.Notification{}, "user_id = ? AND kind = ? AND subject_id = ?", f.group.LeaderID, notify.VisibilityOverride, ev.ID); n != 1 {
		t.Fatalf("override notices to the other leader = %d, want 1", n)
	}
}

func TestEventVisibilityImpactIncludesHeldBodyRegistrants(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirm=%v", confirm), func(t *testing.T) {
			f := newEventWriterRace(t, false)
			var people []*models.User
			for _, name := range []string{"first", "second"} {
				people = append(people, newTestUser(t, f.app, name, "password1234", models.RoleMember))
			}
			registerPerson := func(u *models.User) {
				t.Helper()
				c := loginHeld(t, f.app, u.Email, "password1234")
				if code, body := registerAs(c, f.event.ID, f.date, models.RegGoing, nil); code != http.StatusOK {
					t.Fatalf("register: %d %s", code, body)
				}
			}
			registerPerson(people[0])
			path := "/api/v1/events/" + itoa(f.event.ID) + "/visibility"
			// confirm: the person reviewed the one registrant there was; a
			// review is never stretched over the second (DEC-158).
			body := visibilityBody(t, f.actor, path, map[string]any{"visibility": models.VisibilityInviteOnly, "consent": ""})
			if confirm {
				body = confirmVisibility(t, f.actor, path, body)
			}
			held := f.actor.doHeld("PUT", path, body)
			registerPerson(people[1])
			before := eventWriterState(t, f.app, f.event.ID)
			held.release()
			w := held.wait(t)
			var answer struct {
				NeedsDecision bool                 `json:"needsDecision"`
				Impact        visibilityImpactJSON `json:"impact"`
			}
			decodeJSON(t, w, &answer)
			if w.Code != http.StatusConflict || !answer.NeedsDecision || len(answer.Impact.Tightened) != 1 || answer.Impact.Tightened[0].Registrants != 2 || answer.Impact.Consent == "" {
				t.Fatalf("the write missed a concurrent registrant: %d %s", w.Code, w.Body.String())
			}
			if after := eventWriterState(t, f.app, f.event.ID); before != after {
				t.Fatal("an unreviewed tightening changed rows")
			}
			if !confirm {
				return
			}
			w = f.actor.do("PUT", path, visibilityBody(t, f.actor, path, map[string]any{"visibility": models.VisibilityInviteOnly, "consent": answer.Impact.Consent}), true)
			if w.Code != http.StatusOK {
				t.Fatalf("confirmed tightening: %d %s", w.Code, w.Body.String())
			}
			var occ models.EventOccurrence
			if err := f.app.DB.Where("event_id = ?", f.event.ID).First(&occ).Error; err != nil {
				t.Fatal(err)
			}
			for _, u := range people {
				if got := regStatus(t, f.app, u.ID, occ.ID); got != models.RegCancelled {
					t.Fatalf("registration of %s = %s, want cancelled", u.Name, got)
				}
				if n := contactCount(t, f.app, &models.Notification{}, "user_id = ? AND kind = ?", u.ID, notify.RegistrationCancelled); n != 1 {
					t.Fatalf("cancellation notices for %s = %d, want 1", u.Name, n)
				}
			}
		})
	}
}

func TestChurchEventVisibilityRechecksRemovedHostAfterHeldBody(t *testing.T) {
	app := testApp(t)
	owner := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	host := newTestUser(t, app, "host", "password1234", models.RoleMember)
	admin := loginHeld(t, app, owner.Email, "password1234")
	client := loginHeld(t, app, host.Email, "password1234")
	body := createEventBodyForTest()
	body["hosts"] = []string{owner.Email, host.Email}
	ev := requireCreatedEvent(t, app, admin.do("POST", "/api/v1/events", body, true))
	path := "/api/v1/events/" + itoa(ev.ID) + "/visibility"
	held := client.doHeld("PUT", path, visibilityBody(t, client, path, map[string]any{"visibility": models.VisibilityPrivate, "consent": ""}))
	apiDo(t, admin, "DELETE", "/api/v1/events/"+itoa(ev.ID)+"/hosts/"+itoa(host.ID), nil, http.StatusOK)
	before := eventWriterState(t, app, ev.ID)
	held.release()
	w := held.wait(t)
	if w.Code != http.StatusForbidden {
		t.Fatalf("removed host visibility: %d %s", w.Code, w.Body.String())
	}
	if after := eventWriterState(t, app, ev.ID); before != after {
		t.Fatal("removed host changed event rows")
	}
}

// TestEventPageReadsOneSnapshotAndAcknowledgesIt: the event page reads the
// viewer, the event and its facts, its dates, the group label, the site name
// and an admin's church-wide hosts in one read-only snapshot on one
// connection, and a participating read (M8.1) is acknowledged for the viewer
// that snapshot judged, with the body a plain read gets. A host removed
// through the real route and a leader demoted after the judgment leave the
// answer the snapshot's. A missing event answers exactly like a hidden one,
// acknowledgment included; a session ended before the snapshot is answered
// and acknowledged as the signed-out visitor it reads as; a failed read and a
// failed COMMIT answer the generic 500 with no acknowledgment. A plain read
// carries none.
func TestEventPageReadsOneSnapshotAndAcknowledgesIt(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	hostUser := newTestUser(t, f.app, "page host", "password1234", models.RoleMember)
	church := churchEvent(t, f.admin, "Snapshot church day", hostUser.Email)
	churchPath := fmt.Sprintf("/api/v1/events/%d", church.ID)
	groupPath := fmt.Sprintf("/api/v1/events/%d", f.event.ID)
	admin, leader := pinnedAs(t, f.admin), pinnedAs(t, f.leader)
	outsiderUser := newTestUser(t, f.app, "page outsider", "password1234", models.RoleMember)
	outsider := pinnedAs(t, loginAPI(t, f.app, outsiderUser.Name, "password1234"))
	anonPlain := newAPIClient(t, f.app)
	anonPlain.fetchCSRF()
	anon := pinnedAs(t, anonPlain)

	t.Run("answers", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			plain  *apiClient
			viewer *models.User
			path   string
			shows  []string
		}{
			{"admin, church-wide", f.admin, f.adminUser, churchPath, []string{hostUser.Email, `"hosts":[`}},
			{"leader, group", f.leader, f.leaderUser, groupPath, []string{`"canManage":true`, f.memberUser.PublicName(), groupReadPlace}},
			{"member, group", f.member, f.memberUser, groupPath, []string{`"canManage":false`, groupReadTitle}},
			{"signed out, church-wide", anonPlain, nil, churchPath, []string{"Snapshot church day"}},
		} {
			plain := c.plain.do("GET", c.path, nil, false)
			wantNoAck(t, "a plain read, "+c.name, plain)
			p := pinnedAs(t, c.plain)
			w := p.do("GET", c.path, nil, false)
			if w.Code != http.StatusOK || w.Body.String() != plain.Body.String() {
				t.Fatalf("%s: %d, the same body as a plain read %t: %s", c.name, w.Code, w.Body.String() == plain.Body.String(), w.Body.String())
			}
			wantIn(t, c.name, w.Body.String(), c.shows...)
			wantAck(t, c.name, w, p, c.viewer)
		}
		wantOut(t, "a signed-out church page", anon.do("GET", churchPath, nil, false).Body.String(), hostUser.Email)
	})

	t.Run("refusals", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			client *apiClient
			viewer *models.User
		}{{"signed out", anon, nil}, {"an outsider", outsider, outsiderUser}} {
			hidden := c.client.do("GET", groupPath, nil, false)
			missing := c.client.do("GET", fmt.Sprintf("/api/v1/events/%d", church.ID+100000), nil, false)
			if hidden.Code != http.StatusNotFound || missing.Code != hidden.Code || missing.Body.String() != hidden.Body.String() {
				t.Fatalf("%s: hidden %d %s, missing %d %s", c.name, hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
			}
			wantAck(t, c.name+", hidden", hidden, c.client, c.viewer)
			wantAck(t, c.name+", missing", missing, c.client, c.viewer)
		}
	})

	t.Run("a session ended before the snapshot", func(t *testing.T) {
		ended := groupReadMember(t, f, "page ended")
		c := pinnedAs(t, loginAPI(t, f.app, ended.Name, "password1234"))
		for _, path := range []string{churchPath, groupPath} {
			var fired atomic.Bool
			f.app.hooks.readerBegin = func(surface string) {
				if surface == "event detail" && fired.CompareAndSwap(false, true) {
					gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", ended.ID).Update("session_version", gorm.Expr("session_version + 1")))
				}
			}
			w := c.do("GET", path, nil, false)
			f.app.hooks.readerBegin = nil
			signedOut := anonPlain.do("GET", path, nil, false)
			if !fired.Load() || w.Code != signedOut.Code || w.Body.String() != signedOut.Body.String() {
				t.Fatalf("%s for an ended session (fired %t): %d %s, signed out %d %s", path, fired.Load(), w.Code, w.Body.String(), signedOut.Code, signedOut.Body.String())
			}
			wantAck(t, path+" for an ended session", w, c, nil)
			gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", ended.ID).Update("session_version", gorm.Expr("session_version - 1")))
		}
	})

	t.Run("changes after the judgment", func(t *testing.T) {
		const retitled = "Retitled at the page seam"
		before := apiDo(t, admin, "GET", churchPath, nil, http.StatusOK)
		fired := atReaderSeam(f.app, "authorized", "event detail", func() {
			apiDo(t, f.admin, "DELETE", fmt.Sprintf("%s/hosts/%d", churchPath, hostUser.ID), nil, http.StatusOK)
			gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", church.ID).Update("title", retitled))
		})
		w := admin.do("GET", churchPath, nil, false)
		f.app.hooks.readerAuthorized = nil
		if !fired.Load() || w.Body.String() != before {
			t.Fatalf("the page straddled the host removal (fired %t):\nbefore %s\ngot    %s", fired.Load(), before, w.Body.String())
		}
		wantAck(t, "the snapshot's church page", w, admin, f.adminUser)
		after := apiDo(t, admin, "GET", churchPath, nil, http.StatusOK)
		wantOut(t, "the next church page", after, hostUser.Email)
		wantIn(t, "the next church page", after, retitled)

		before = apiDo(t, leader, "GET", groupPath, nil, http.StatusOK)
		fired = atReaderSeam(f.app, "authorized", "event detail", func() {
			gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipMember))
		})
		w = leader.do("GET", groupPath, nil, false)
		f.app.hooks.readerAuthorized = nil
		if !fired.Load() || w.Body.String() != before {
			t.Fatalf("the page straddled the demotion (fired %t):\nbefore %s\ngot    %s", fired.Load(), before, w.Body.String())
		}
		wantAck(t, "the snapshot's group page", w, leader, f.leaderUser)
		wantIn(t, "the demoted leader's next page", apiDo(t, leader, "GET", groupPath, nil, http.StatusOK), `"canManage":false`)
		gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipLeader))

		going := groupReadMember(t, f, "page erasable attendee")
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &going.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
		before = apiDo(t, leader, "GET", groupPath, nil, http.StatusOK)
		wantIn(t, "the leader's page", before, going.PublicName())
		fired = atReaderSeam(f.app, "authorized", "event detail", func() {
			apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(going.ID), nil, http.StatusOK)
		})
		w = leader.do("GET", groupPath, nil, false)
		f.app.hooks.readerAuthorized = nil
		if !fired.Load() || w.Body.String() != before {
			t.Fatalf("the page straddled the erasure (fired %t):\nbefore %s\ngot    %s", fired.Load(), before, w.Body.String())
		}
		wantAck(t, "the snapshot's page before the erasure", w, leader, f.leaderUser)
		after = apiDo(t, leader, "GET", groupPath, nil, http.StatusOK)
		wantOut(t, "the page after the erasure", after, going.PublicName(), going.Email, fmt.Sprintf(`"userId":%d,`, going.ID))
		wantIn(t, "the page after the erasure", after, f.memberUser.PublicName())
	})

	t.Run("failures", func(t *testing.T) {
		second := newTestUser(t, f.app, "page second host", "password1234", models.RoleMember)
		apiDo(t, f.admin, "POST", churchPath+"/hosts", map[string]any{"email": second.Email}, http.StatusOK)
		n := failStatement(t, f.app, func(tx *gorm.DB) bool { return tx.Statement.Table == "event_hosts" })
		w := admin.do("GET", churchPath, nil, false)
		if n.Load() == 0 || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
			t.Fatalf("a failed host read (failed %d): %d %s", n.Load(), w.Code, w.Body.String())
		}
		wantNoAck(t, "a failed host read", w)
	})

	t.Run("a failed commit", func(t *testing.T) {
		var fired atomic.Bool
		f.app.hooks.readerEnd = func(surface string, tx *gorm.DB) {
			if surface == "event detail" && fired.CompareAndSwap(false, true) {
				endBackend(t, f.app, tx)
			}
		}
		w := leader.do("GET", groupPath, nil, false)
		f.app.hooks.readerEnd = nil
		if !fired.Load() || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
			t.Fatalf("a failed commit (fired %t): %d %s", fired.Load(), w.Code, w.Body.String())
		}
		wantNoAck(t, "a failed commit", w)
	})

	t.Run("one connection", func(t *testing.T) {
		second := newTestUser(t, f.app, "page connection host", "password1234", models.RoleMember)
		apiDo(t, f.admin, "POST", churchPath+"/hosts", map[string]any{"email": second.Email}, http.StatusOK)
		orig := f.app.DB
		one := oneConnectionPool(t, f.app)
		defer func() { f.app.DB = orig }()
		for _, c := range []struct {
			client *apiClient
			path   string
			status int
			shows  []string
		}{
			{admin, churchPath, http.StatusOK, []string{second.Email, `"hosts":[`}},
			{leader, groupPath, http.StatusOK, []string{`"canManage":true`, f.memberUser.PublicName(), "Snapshot circle", groupReadPlace}},
			{outsider, groupPath, http.StatusNotFound, nil},
		} {
			oneConnection(t, f.app, one)
			w := c.client.do("GET", c.path, nil, false)
			if w.Code != c.status {
				t.Fatalf("%s: %d %s (a read outside the snapshot waits for the one connection)", c.path, w.Code, w.Body.String())
			}
			wantIn(t, c.path, w.Body.String(), c.shows...)
			if len(w.Header().Values(sessionCheckedHeader)) != 1 {
				t.Fatalf("%s on one connection: no acknowledgment", c.path)
			}
		}
	})
}

// eventChanged requires w to be the event_changed refusal (DEC-163).
func eventChanged(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"event_changed"`) || strings.Contains(w.Body.String(), "needsDecision") {
		t.Fatalf("%s: %d %s, want event_changed", what, w.Code, w.Body.String())
	}
}

// TestVisibilitySaveNamesTheReadItWasMadeFrom (DEC-163): a visibility save,
// a plain change and a reset alike, carries the event page's revision; any
// other token is refused with nothing written, even with the right consent.
func TestVisibilitySaveNamesTheReadItWasMadeFrom(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	f.outsider(t, "outsider", 0)
	f.setOverride(t, 1, strp(models.VisibilityPrivate))
	path, series := f.eventPath(), "/api/v1/events/"+itoa(f.ev.ID)
	stale := seriesRevisionOf(t, f.admin, f.ev.ID)
	apiDo(t, f.admin, "PUT", series, seriesBody(t, f.admin, series, map[string]any{"title": "A newer title"}), http.StatusOK)
	current := seriesRevisionOf(t, f.admin, f.ev.ID)
	if stale == current {
		t.Fatal("a series save left the revision as it was")
	}
	other := scheduleGroupEvent(t, f.admin, f.gid, "Another evening")
	parts := strings.Split(current, ".")
	parts[1] = strings.Repeat("f", 16)
	missing := struct{}{}
	tokens := map[string]any{
		"stale": stale, "missing": missing, "null": nil, "empty": "", "garbled": "garbled",
		"another event's": seriesRevisionOf(t, f.admin, other.ID), "another generation's": strings.Join(parts, "."),
	}
	changes := map[string]map[string]any{
		"a tightening": {"visibility": models.VisibilityInviteOnly},
		"a reset":      {"visibility": models.VisibilityPublic, "resetOverrides": true},
	}
	for change, body := range changes {
		for name, token := range tokens {
			t.Run(change+"/"+name, func(t *testing.T) {
				sent := with(body, "consent", f.preview(t, path, body).Consent)
				if token == missing {
					delete(sent, "revision")
				} else {
					sent["revision"] = token
				}
				before := f.everything(t)
				eventChanged(t, f.admin.do("PUT", path, sent, true), name)
				if after := f.everything(t); after != before {
					t.Fatalf("a refused save wrote:\n%s\nwant\n%s", after, before)
				}
			})
		}
	}
	apiDo(t, f.admin, "PUT", path, map[string]any{"visibility": models.VisibilityPrivate, "consent": "", "revision": 7}, http.StatusBadRequest)
	// The current token saves, and the answer carries the next one.
	body := map[string]any{"visibility": models.VisibilityInviteOnly}
	w := f.admin.do("PUT", path, with(with(body, "consent", f.preview(t, path, body).Consent), "revision", current), true)
	if w.Code != http.StatusOK {
		t.Fatalf("the current token: %d %s", w.Code, w.Body.String())
	}
	var answer eventPage
	decodeJSON(t, w, &answer)
	if next := seriesRevisionOf(t, f.admin, f.ev.ID); answer.Event.Revision != next || next == current {
		t.Fatalf("the answer's revision %q, the page's %q, the sent one %q", answer.Event.Revision, next, current)
	}
	eventChanged(t, f.admin.do("PUT", path, map[string]any{"visibility": models.VisibilityPrivate, "consent": "", "revision": current}, true), "the token a save used")
}

// TestStaleWideningAfterAnotherManagersTighteningIsRefused (#454): a manager
// whose page was read before another manager tightened the event cannot save
// an older, wider audience over it.
func TestStaleWideningAfterAnotherManagersTighteningIsRefused(t *testing.T) {
	f := newEventWriterRace(t, false)
	path := "/api/v1/events/" + itoa(f.event.ID) + "/visibility"
	opened := seriesRevisionOf(t, f.actor, f.event.ID)
	apiDo(t, f.admin, "PUT", path, confirmVisibility(t, f.admin, path, map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	before := eventWriterState(t, f.app, f.event.ID)
	for _, vis := range []string{models.VisibilityPublic, models.VisibilityPrivate} {
		eventChanged(t, f.actor.do("PUT", path, map[string]any{"visibility": vis, "reason": "Open house", "consent": "", "revision": opened}, true), "a stale widening to "+vis)
	}
	if after := eventWriterState(t, f.app, f.event.ID); after != before {
		t.Fatalf("a stale widening wrote:\n%s\nwant\n%s", after, before)
	}
	if ev := eventRow(t, f.app, f.event.ID); ev.Visibility != models.VisibilityInviteOnly {
		t.Fatalf("the event is %s, want invite_only", ev.Visibility)
	}
	// Read again, the same widening saves.
	apiDo(t, f.actor, "PUT", path, visibilityBody(t, f.actor, path, map[string]any{"visibility": models.VisibilityPublic, "reason": "Open house", "consent": ""}), http.StatusOK)
}

// The event's revision covers its group's audience (DEC-163): with the event
// row untouched, a group change makes an earlier token stale for both saves,
// so a widening chosen against the old group is never saved without the
// leader-opened review. A church-wide event's token follows no group.
func TestGroupAudienceChangeMakesTheEventTokenStale(t *testing.T) {
	f := newEventWriterRace(t, false)
	path := "/api/v1/events/" + itoa(f.event.ID) + "/visibility"
	series := "/api/v1/events/" + itoa(f.event.ID)
	group := "/api/v1/groups/" + itoa(f.group.ID)
	apiDo(t, f.admin, "PUT", path, confirmVisibility(t, f.admin, path, map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	opened := seriesRevisionOf(t, f.actor, f.event.ID)
	row := eventRow(t, f.app, f.event.ID).EditRevision
	apiDo(t, f.admin, "PUT", group, groupBody(t, f.admin, group, map[string]any{"visibility": models.VisibilityPrivate}), http.StatusOK)
	if got := eventRow(t, f.app, f.event.ID).EditRevision; got != row {
		t.Fatalf("the group change wrote the event row: revision %d, was %d", got, row)
	}
	before := eventWriterState(t, f.app, f.event.ID)
	eventChanged(t, f.actor.do("PUT", path, map[string]any{"visibility": models.VisibilityPublic, "consent": "", "revision": opened}, true), "a widening chosen against the public group")
	eventChanged(t, f.actor.do("PUT", series, map[string]any{"title": "Renamed", "revision": opened}, true), "a series save read before the group change")
	if after := eventWriterState(t, f.app, f.event.ID); after != before {
		t.Fatalf("a refused save wrote:\n%s\nwant\n%s", after, before)
	}
	if ev := eventRow(t, f.app, f.event.ID); ev.Visibility != models.VisibilityInviteOnly || ev.LeaderOpened {
		t.Fatalf("the event is %s (leader-opened %t), want invite_only", ev.Visibility, ev.LeaderOpened)
	}
	// Read again, the same widening saves as the leader-opened override it now is.
	apiDo(t, f.actor, "PUT", path, visibilityBody(t, f.actor, path, map[string]any{"visibility": models.VisibilityPublic, "consent": ""}), http.StatusOK)
	if ev := eventRow(t, f.app, f.event.ID); ev.Visibility != models.VisibilityPublic || !ev.LeaderOpened {
		t.Fatalf("the event is %s (leader-opened %t), want a leader-opened public event", ev.Visibility, ev.LeaderOpened)
	}

	church := churchEvent(t, f.admin, "Church supper")
	token := seriesRevisionOf(t, f.admin, church.ID)
	apiDo(t, f.admin, "PUT", group, groupBody(t, f.admin, group, map[string]any{"visibility": models.VisibilityPublic}), http.StatusOK)
	if got := seriesRevisionOf(t, f.admin, church.ID); got != token {
		t.Fatalf("a group change moved a church-wide event's token: %q, was %q", got, token)
	}
	apiDo(t, f.admin, "PUT", "/api/v1/events/"+itoa(church.ID), map[string]any{"title": "Church supper renamed", "revision": token}, http.StatusOK)
}

func TestConcurrentVisibilitySavesFromOneReadExactlyOneWins(t *testing.T) {
	f := newEventWriterRace(t, false)
	path := "/api/v1/events/" + itoa(f.event.ID) + "/visibility"
	opened := seriesRevisionOf(t, f.actor, f.event.ID)
	type answer struct {
		code      int
		body, vis string
	}
	answers := make(chan answer, 2)
	start := make(chan struct{})
	for _, save := range []struct {
		c   *apiClient
		vis string
	}{{f.actor, models.VisibilityPrivate}, {f.admin, models.VisibilityInviteOnly}} {
		go func() {
			<-start
			w := save.c.do("PUT", path, map[string]any{"visibility": save.vis, "consent": "", "revision": opened}, true)
			answers <- answer{w.Code, w.Body.String(), save.vis}
		}()
	}
	close(start)
	var won []string
	for range 2 {
		a := <-answers
		switch {
		case a.code == http.StatusOK:
			won = append(won, a.vis)
		case a.code != http.StatusConflict || !strings.Contains(a.body, `"event_changed"`):
			t.Fatalf("a save answered %d %s", a.code, a.body)
		}
	}
	if ev := eventRow(t, f.app, f.event.ID); len(won) != 1 || ev.Visibility != won[0] {
		t.Fatalf("stored %q; saves that won: %v", ev.Visibility, won)
	}
	var audits int64
	gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("target_type = 'event' AND target_id = ? AND action LIKE 'event.visibility%'", f.event.ID).Count(&audits))
	if audits != 1 {
		t.Fatalf("visibility audit rows = %d, want 1", audits)
	}
}

// A person who may not manage the event gets the answer they always got,
// whatever revision they send; event_changed is only for a manager.
func TestVisibilityRevisionIsJudgedOnlyForAManager(t *testing.T) {
	f := newEventWriterRace(t, false)
	path := "/api/v1/events/" + itoa(f.event.ID) + "/visibility"
	member := newTestUser(t, f.app, "member", "password1234", models.RoleMember)
	addMember(t, f.app, member, f.group, models.MembershipMember)
	mc := loginHeld(t, f.app, member.Email, "password1234")
	outsider := newTestUser(t, f.app, "outsider", "password1234", models.RoleMember)
	oc := loginHeld(t, f.app, outsider.Email, "password1234")
	current := seriesRevisionOf(t, f.admin, f.event.ID)
	apiDo(t, f.admin, "PUT", path, confirmVisibility(t, f.admin, path, map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	before := eventWriterState(t, f.app, f.event.ID)
	for _, rev := range []any{nil, "", "garbled", current, seriesRevisionOf(t, f.admin, f.event.ID)} {
		body := map[string]any{"visibility": models.VisibilityPublic, "consent": ""}
		if rev != nil {
			body["revision"] = rev
		}
		if w := mc.do("PUT", path, body, true); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "event_changed") {
			t.Fatalf("a member with revision %v: %d %s, want 403", rev, w.Code, w.Body.String())
		}
		if w := oc.do("PUT", path, body, true); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "event_changed") {
			t.Fatalf("someone who cannot see it, with revision %v: %d %s, want 404", rev, w.Code, w.Body.String())
		}
	}
	if after := eventWriterState(t, f.app, f.event.ID); after != before {
		t.Fatal("a refused save wrote")
	}
}

// A save that changes nothing stays a no-op under a current token and is
// refused under a stale one; a current token whose consent is out of date
// still gets the decision, not event_changed.
func TestVisibilityRevisionNoChangeAndDecision(t *testing.T) {
	f := newVCFixture(t, models.VisibilityPublic)
	f.outsider(t, "outsider", 0)
	path, series := f.eventPath(), "/api/v1/events/"+itoa(f.ev.ID)
	stale := seriesRevisionOf(t, f.admin, f.ev.ID)
	apiDo(t, f.admin, "PUT", series, seriesBody(t, f.admin, series, map[string]any{"title": "A newer title"}), http.StatusOK)
	current := seriesRevisionOf(t, f.admin, f.ev.ID)
	same := map[string]any{"visibility": models.VisibilityPublic, "consent": ""}
	before := f.everything(t)
	eventChanged(t, f.admin.do("PUT", path, with(same, "revision", stale), true), "a stale no-change save")
	w := f.admin.do("PUT", path, with(same, "revision", current), true)
	if w.Code != http.StatusOK {
		t.Fatalf("a current no-change save: %d %s", w.Code, w.Body.String())
	}
	var answer eventPage
	decodeJSON(t, w, &answer)
	if after := f.everything(t); after != before || answer.Event.Revision != current {
		t.Fatalf("a no-change save wrote or moved the revision (%q, was %q)", answer.Event.Revision, current)
	}
	// A reset that leaves the visibility as it is changes only the dates, so
	// the event's revision stays.
	f.setOverride(t, 1, strp(models.VisibilityPrivate))
	reset := map[string]any{"visibility": models.VisibilityPublic, "resetOverrides": true}
	apiDo(t, f.admin, "PUT", path, with(with(reset, "consent", f.preview(t, path, reset).Consent), "revision", current), http.StatusOK)
	if f.override(t, 1) != "series" || seriesRevisionOf(t, f.admin, f.ev.ID) != current {
		t.Fatalf("a reset-only save: override %q, revision %q, want series and %q", f.override(t, 1), seriesRevisionOf(t, f.admin, f.ev.ID), current)
	}
	before = f.everything(t)
	tighten := map[string]any{"visibility": models.VisibilityInviteOnly, "consent": "", "revision": current}
	impact := wantDecision(t, f.admin.do("PUT", path, tighten, true))
	if after := f.everything(t); after != before {
		t.Fatal("an unreviewed tightening wrote")
	}
	apiDo(t, f.admin, "PUT", path, with(tighten, "consent", impact.Consent), http.StatusOK)
}
