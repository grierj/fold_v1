package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/mail"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type composeFixture struct {
	app                   *App
	admin, leader, host   *apiClient
	member                *apiClient
	leaderUser, hostUser  *models.User
	group, otherGroup     *models.Group
	groupEvent, otherEv   eventJSON
	churchEvent, otherCW  eventJSON
	groupOcc              models.EventOccurrence
	teen, opted, bouncing *models.User
}

// newComposeFixture: a leader's group (with a teen, someone who turned off
// the group's announcements and someone whose email bounces), another
// group, a group event of each, and two church-wide events, one hosted.
func newComposeFixture(t *testing.T) composeFixture {
	t.Helper()
	app := testApp(t)
	f := composeFixture{app: app}
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	f.leaderUser = newTestUser(t, app, "leader", "password1234", models.RoleMember)
	f.hostUser = newTestUser(t, app, "host", "password1234", models.RoleMember)
	newTestUser(t, app, "member", "password1234", models.RoleMember)
	f.admin, f.leader = loginAPI(t, app, "admin", "password1234"), loginAPI(t, app, "leader", "password1234")
	f.host, f.member = loginAPI(t, app, "host", "password1234"), loginAPI(t, app, "member", "password1234")
	g := activeGroup(t, app, f.leaderUser, "Book club", models.VisibilityPublic, "")
	f.group = &g
	other := activeGroup(t, app, f.hostUser, "Choir", models.VisibilityPublic, "")
	f.otherGroup = &other
	start := feedStart(3)
	f.groupEvent = createFeedEvent(t, f.leader, g.ID, "Book night", start, "")
	f.otherEv = createFeedEvent(t, f.host, other.ID, "Rehearsal", start, "")
	app.DB.Where("event_id = ?", f.groupEvent.ID).First(&f.groupOcc)
	church := func(title string) eventJSON {
		var out struct {
			Event eventJSON `json:"event"`
		}
		w := f.admin.do("POST", "/api/v1/events", map[string]any{"title": title, "startsLocal": start.Format("2006-01-02T15:04"), "durationMinutes": 60, "tzid": "UTC", "visibility": "public"}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("church event: %d %s", w.Code, w.Body.String())
		}
		decodeJSON(t, w, &out)
		return out.Event
	}
	f.churchEvent, f.otherCW = church("Picnic"), church("Concert")
	if err := app.DB.Create(&models.EventHost{EventID: f.churchEvent.ID, UserID: f.hostUser.ID, AddedBy: f.hostUser.ID}).Error; err != nil {
		t.Fatal(err)
	}
	f.teen = newTestUser(t, app, "teen", "password1234", models.RoleMember)
	app.DB.Model(f.teen).Update("account_type", models.AccountTeen)
	f.opted = newTestUser(t, app, "opted", "password1234", models.RoleMember)
	f.bouncing = newTestUser(t, app, "bouncing", "password1234", models.RoleMember)
	for _, u := range []*models.User{f.teen, f.opted, f.bouncing} {
		addMember(t, app, u, f.group, models.MembershipMember)
	}
	// Turning off messages from leaders is what stops announcements (DEC-115, DEC-119).
	if w := loginAPI(t, app, "opted", "password1234").do("PUT", "/api/v1/me/notification-settings", map[string]any{"category": "messages", "off": true}, true); w.Code != http.StatusOK {
		t.Fatalf("opt out: %d %s", w.Code, w.Body.String())
	}
	app.DB.Create(&models.Suppression{Channel: "email", AddressHash: mail.AddressHash(f.bouncing.Email), Reason: models.SuppressHardBounce})
	return f
}

type composeOptions = composeOptionsJSON

func (f composeFixture) options(t *testing.T, c *apiClient) composeOptions {
	t.Helper()
	var o composeOptions
	decodeJSON(t, c.do("GET", "/api/v1/messages/compose", nil, false), &o)
	return o
}

func eventTitles(o composeOptions) string {
	var s []string
	for _, e := range o.Events {
		s = append(s, e.Title)
	}
	return strings.Join(s, ",")
}

// The picker offers leaders their own groups and events, hosts only the
// events they host, admins everything; members can't compose (M3.4).
func TestComposerOffersOnlyWhatTheAuthorMayMessage(t *testing.T) {
	f := newComposeFixture(t)
	if o := f.options(t, f.leader); !o.CanCompose || o.CanMessageEveryone || len(o.Groups) != 1 || o.Groups[0].Name != "Book club" || eventTitles(o) != "Book night" {
		t.Fatalf("leader: %+v", o)
	}
	if o := f.options(t, f.host); !o.CanCompose || len(o.Groups) != 1 || eventTitles(o) != "Picnic,Rehearsal" {
		t.Fatalf("host (also leads the choir): %+v", o)
	}
	if o := f.options(t, f.admin); !o.CanMessageEveryone || len(o.Groups) != 2 || eventTitles(o) != "Book night,Concert,Picnic,Rehearsal" {
		t.Fatalf("admin: %+v", o)
	}
	if o := f.options(t, f.member); o.CanCompose || len(o.Groups) != 0 {
		t.Fatalf("member: %+v", o)
	}
	if len(f.options(t, f.leader).Events[0].Dates) != 1 {
		t.Fatal("events list their upcoming dates")
	}
	// A listed date is the plain YYYY-MM-DD; the fixture's event starts at
	// 19:00 UTC, so its start time names the date independently.
	if d := f.options(t, f.leader).Events[0].Dates[0]; d.OccurrenceID != f.groupOcc.ID || d.Date != f.groupOcc.StartsAt.UTC().Format("2006-01-02") {
		t.Fatalf("composer date = %+v, want occurrence %d on %s", d, f.groupOcc.ID, f.groupOcc.StartsAt.UTC().Format("2006-01-02"))
	}
}

// ?event=<id> answers only that event, by the same rules, for the date
// page's "Email people going" (M3.4c).
func TestComposeOptionsForOneEvent(t *testing.T) {
	f := newComposeFixture(t)
	one := func(c *apiClient, id uint) composeOptions {
		t.Helper()
		var o composeOptions
		decodeJSON(t, c.do("GET", "/api/v1/messages/compose?event="+itoa(id), nil, false), &o)
		return o
	}
	if o := one(f.leader, f.groupEvent.ID); len(o.Groups) != 0 || eventTitles(o) != "Book night" || len(o.Events[0].Dates) != 1 {
		t.Fatalf("leader, own event: %+v", o)
	}
	for name, tc := range map[string]struct {
		c  *apiClient
		id uint
	}{"leader, another's event": {f.leader, f.otherEv.ID}, "host, unhosted church event": {f.host, f.otherCW.ID}, "member": {f.member, f.groupEvent.ID}, "no such event": {f.admin, 99999}} {
		if o := one(tc.c, tc.id); len(o.Events) != 0 || len(o.Groups) != 0 {
			t.Errorf("%s: %+v", name, o)
		}
	}
	if o := one(f.host, f.churchEvent.ID); eventTitles(o) != "Picnic" {
		t.Fatalf("host, hosted event: %+v", o)
	}
	if o := one(f.admin, f.otherEv.ID); eventTitles(o) != "Rehearsal" {
		t.Fatalf("admin: %+v", o)
	}
	if w := f.leader.do("GET", "/api/v1/messages/compose?event=nope", nil, false); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", w.Code)
	}
}

// The session says whether to offer Messages in the nav (M3.4c): leaders,
// hosts and admins, not plain members.
func TestSessionSaysWhoCanCompose(t *testing.T) {
	f := newComposeFixture(t)
	for name, tc := range map[string]struct {
		c    *apiClient
		want bool
	}{"admin": {f.admin, true}, "leader": {f.leader, true}, "host": {f.host, true}, "member": {f.member, false}} {
		var me struct {
			CanCompose bool `json:"canCompose"`
		}
		decodeJSON(t, tc.c.do("GET", "/api/v1/auth/me", nil, false), &me)
		if me.CanCompose != tc.want {
			t.Errorf("%s: canCompose %v, want %v", name, me.CanCompose, tc.want)
		}
	}
	// Signing in answers it too, so the nav is right before the next read.
	c := newAPIClient(t, f.app)
	c.fetchCSRF()
	var login struct {
		CanCompose bool `json:"canCompose"`
	}
	decodeJSON(t, c.do("POST", "/api/v1/auth/login", map[string]string{"email": testEmail("leader"), "password": "password1234"}, true), &login)
	if !login.CanCompose {
		t.Fatal("login: a leader can compose")
	}
}

// The API refuses targets the author may not message (M3.4 acceptance).
func TestComposerRefusesOtherTargets(t *testing.T) {
	f := newComposeFixture(t)
	draft := func(c *apiClient, audience map[string]any) int {
		return c.do("POST", "/api/v1/messages", map[string]any{"subject": "Hi", "bodyHtml": "<p>Hi</p>", "audience": audience}, true).Code
	}
	for name, tc := range map[string]struct {
		c    *apiClient
		aud  map[string]any
		want int
	}{
		"leader, own group":            {f.leader, map[string]any{"type": "group", "groupId": f.group.ID}, http.StatusCreated},
		"leader, another group":        {f.leader, map[string]any{"type": "group", "groupId": f.otherGroup.ID}, http.StatusForbidden},
		"leader, own event's date":     {f.leader, map[string]any{"type": "occurrence", "occurrenceId": f.groupOcc.ID}, http.StatusCreated},
		"leader, another's event":      {f.leader, map[string]any{"type": "event_series", "eventId": f.otherEv.ID}, http.StatusForbidden},
		"leader, everyone":             {f.leader, map[string]any{"type": "all_members"}, http.StatusForbidden},
		"leader, chosen people":        {f.leader, map[string]any{"type": "users", "userIds": []uint{f.teen.ID}}, http.StatusForbidden},
		"host, hosted event":           {f.host, map[string]any{"type": "events", "eventIds": []uint{f.churchEvent.ID}}, http.StatusCreated},
		"host, unhosted church event":  {f.host, map[string]any{"type": "events", "eventIds": []uint{f.churchEvent.ID, f.otherCW.ID}}, http.StatusForbidden},
		"member, a group they're in":   {f.member, map[string]any{"type": "group", "groupId": f.group.ID}, http.StatusForbidden},
		"admin, everyone":              {f.admin, map[string]any{"type": "all_members"}, http.StatusCreated},
		"no audience":                  {f.admin, map[string]any{}, http.StatusBadRequest},
		"a group that isn't there":     {f.admin, map[string]any{"type": "group", "groupId": 99999}, http.StatusNotFound},
		"leader, no such group":        {f.leader, map[string]any{"type": "group", "groupId": 99999}, http.StatusForbidden},
		"an event list that's too big": {f.admin, map[string]any{"type": "events", "eventIds": make([]uint, 51)}, http.StatusBadRequest},
	} {
		if got := draft(tc.c, tc.aud); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
	if w := f.leader.do("POST", "/api/v1/messages", map[string]any{"channel": "sms", "subject": "Hi", "audience": map[string]any{"type": "group", "groupId": f.group.ID}}, true); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "message_channel") {
		t.Fatalf("texting isn't set up: %d %s", w.Code, w.Body.String())
	}
}

// The preview counts who gets it and who is skipped and why, with a teen
// count and Teen badges (DEC-20), names only (DEC-31).
func TestAudiencePreview(t *testing.T) {
	f := newComposeFixture(t)
	w := f.leader.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": map[string]any{"type": "group", "groupId": f.group.ID}}, true)
	var out struct {
		Summary audienceSummaryJSON `json:"summary"`
	}
	decodeJSON(t, w, &out)
	s := out.Summary
	if s.People != 4 || s.Eligible != 2 || s.Teens != 1 || s.Skipped[skipOptedOut] != 1 || s.Skipped[skipNotWorking] != 1 {
		t.Fatalf("summary: %+v", s)
	}
	teens := 0
	for _, r := range s.Recipients {
		if r.Teen {
			teens++
		}
	}
	if teens != 1 || len(s.Recipients) != 4 {
		t.Fatalf("recipients: %+v", s.Recipients)
	}
	// A leader sees that someone won't get it, not why; an admin sees why.
	for _, r := range s.Recipients {
		if r.Skipped != "" && r.Skipped != skipHidden {
			t.Fatalf("a leader sees a person's reason: %+v", r)
		}
	}
	decodeJSON(t, f.admin.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": map[string]any{"type": "group", "groupId": f.group.ID}}, true), &out)
	reasons := map[string]bool{}
	for _, r := range out.Summary.Recipients {
		reasons[r.Skipped] = true
	}
	if !reasons[skipOptedOut] || !reasons[skipNotWorking] {
		t.Fatalf("admin reasons: %+v", out.Summary.Recipients)
	}
	if strings.Contains(w.Body.String(), "@") {
		t.Fatal("the preview must never show an address")
	}
	// Leaders only by role.
	w = f.leader.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": map[string]any{"type": "group", "groupId": f.group.ID, "roles": []string{"leader"}}}, true)
	decodeJSON(t, w, &out)
	if out.Summary.People != 1 {
		t.Fatalf("leaders only: %+v", out.Summary)
	}
}

// The preview skips someone whose address has an open row, hashed or legacy,
// in either order, with the same words; a cleared row skips nobody. A complaint
// alone says "marked our email as spam"; beside a bounce the bounce wins.
func TestAudiencePreviewHashedAndLegacySuppressions(t *testing.T) {
	f := newComposeFixture(t)
	cases := suppressionCases()
	users := f.addSuppressionMembers(t, cases)
	wantEligible, wantSkipped := 2, map[string]int{skipOptedOut: 1, skipNotWorking: 1}
	for _, tc := range cases {
		suppressSpecs(t, f.app, strings.ToLower(users[tc.name].Email), tc.specs...)
		if skip := tc.snapshotSkip(); skip == "" {
			wantEligible++
		} else {
			wantSkipped[skip]++
		}
	}
	var out struct {
		Summary audienceSummaryJSON `json:"summary"`
	}
	decodeJSON(t, f.admin.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": map[string]any{"type": "group", "groupId": f.group.ID}}, true), &out)
	s := out.Summary
	if s.People != 4+len(cases) || s.Eligible != wantEligible || !reflect.DeepEqual(s.Skipped, wantSkipped) {
		t.Fatalf("summary: %+v, want %d people, %d eligible, skipped %v", s, 4+len(cases), wantEligible, wantSkipped)
	}
	byName := map[string]string{}
	for _, r := range s.Recipients {
		byName[r.Name] = r.Skipped
	}
	for _, tc := range cases {
		if got, ok := byName[users[tc.name].Name]; !ok || got != tc.snapshotSkip() {
			t.Errorf("%s: preview skipped %q (listed %v), want %q", tc.name, got, ok, tc.snapshotSkip())
		}
	}
}

// A household person's registration reaches the account holder who
// registered them (DEC-8), once.
func TestOccurrenceAudienceReachesTheAccountHolder(t *testing.T) {
	f := newComposeFixture(t)
	parent := newTestUser(t, f.app, "parent", "password1234", models.RoleMember)
	hh := models.Household{Name: "Parents", CreatedBy: parent.ID}
	f.app.DB.Create(&hh)
	kid := models.HouseholdPerson{HouseholdID: hh.ID, FirstName: "Kid", Kind: "child", CreatedBy: parent.ID}
	f.app.DB.Create(&kid)
	f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, HouseholdPersonID: &kid.ID, RegisteredBy: &parent.ID, Status: models.RegGoing})
	f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &parent.ID, Status: models.RegGoing})
	f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &f.teen.ID, Status: models.RegDeclined})
	// A guest registrant is named, but gets only transactional mail and
	// reminders until they ask for updates (PLAN 7.4).
	guest := newTestUser(t, f.app, "zguest", "password1234", models.RoleMember)
	f.app.DB.Model(guest).Update("status", models.UserGuest)
	f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &guest.ID, Status: models.RegGoing})
	var out struct {
		Summary audienceSummaryJSON `json:"summary"`
	}
	decodeJSON(t, f.admin.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": map[string]any{"type": "occurrence", "occurrenceId": f.groupOcc.ID}}, true), &out)
	if out.Summary.People != 2 || out.Summary.Eligible != 1 || out.Summary.Skipped[skipGuest] != 1 || out.Summary.Recipients[0].Name != parent.PublicName() {
		t.Fatalf("household and declined registrants: %+v", out.Summary)
	}
}

// Drafts are sanitized; scheduling needs a time ahead and someone to get
// it; scheduled messages can change or be cancelled until they send.
func TestMessageDraftScheduleCancel(t *testing.T) {
	f := newComposeFixture(t)
	aud := map[string]any{"type": "group", "groupId": f.group.ID}
	var out struct {
		Message messageJSON `json:"message"`
	}
	w := f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Next week", "bodyHtml": `<p onclick="x()">See <a href="javascript:alert(1)">you</a></p><script>bad()</script>`, "audience": aud}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &out)
	m := out.Message
	if m.Status != models.MessageDraft || m.BodyHTML != "<p>See you</p>" || !m.Editable || m.AuthorName == "" {
		t.Fatalf("draft: %+v", m)
	}
	var row models.Message
	f.app.DB.First(&row, m.ID)
	if row.BodyText != "See you" {
		t.Fatalf("plain text: %q", row.BodyText)
	}
	path := "/api/v1/messages/" + itoa(m.ID)
	if w := f.host.do("GET", path, nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("someone else's message: %d", w.Code)
	}
	if w := f.admin.do("GET", path, nil, false); w.Code != http.StatusOK {
		t.Fatalf("admin: %d", w.Code)
	}
	for _, when := range []time.Time{time.Now().Add(-time.Hour), time.Now().AddDate(2, 0, 0)} {
		if w := f.leader.do("POST", path+"/schedule", confirmedSend(f.leader, path, map[string]any{"scheduledAt": when}), true); w.Code != http.StatusBadRequest {
			t.Fatalf("schedule at %v: %d", when, w.Code)
		}
	}
	when := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	w = f.leader.do("POST", path+"/schedule", confirmedSend(f.leader, path, map[string]any{"scheduledAt": when}), true)
	decodeJSON(t, w, &out)
	if w.Code != http.StatusOK || out.Message.Status != models.MessageScheduled || !out.Message.ScheduledAt.Equal(when) {
		t.Fatalf("schedule: %d %s", w.Code, w.Body.String())
	}
	f.app.DB.First(&row, m.ID)
	if row.RecipientCount != 2 || auditCount(t, f.app, audit.MessageScheduled) != 1 {
		t.Fatalf("recipients %d, audited %d", row.RecipientCount, auditCount(t, f.app, audit.MessageScheduled))
	}
	// Still editable while scheduled, but only into something that could
	// still go out; the count follows the audience, and the edit is audited.
	if w := f.leader.do("PUT", path, namedEdit(f.leader, path, map[string]any{"subject": "", "bodyHtml": "", "audience": aud}), true); w.Code != http.StatusBadRequest {
		t.Fatalf("emptying a scheduled message: %d %s", w.Code, w.Body.String())
	}
	if w := f.leader.do("PUT", path, namedEdit(f.leader, path, map[string]any{"subject": "Next week, again", "bodyHtml": "<p>Changed</p>", "audience": map[string]any{"type": "group", "groupId": f.group.ID, "roles": []string{"leader"}}}), true); w.Code != http.StatusOK {
		t.Fatalf("edit scheduled: %d %s", w.Code, w.Body.String())
	}
	f.app.DB.First(&row, m.ID)
	if row.Status != models.MessageScheduled || row.Subject != "Next week, again" || row.RecipientCount != 1 || auditCount(t, f.app, audit.MessageEdited) != 1 {
		t.Fatalf("scheduled edit: %+v, audited %d", row, auditCount(t, f.app, audit.MessageEdited))
	}
	// What is stored is bounded, not just what was sent: escaping grows it.
	if w := f.leader.do("PUT", path, namedEdit(f.leader, path, map[string]any{"subject": "x", "bodyHtml": "<p>" + strings.Repeat("&", 60000) + "</p>", "audience": aud}), true); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "message_body") {
		t.Fatalf("too long once escaped: %d %s", w.Code, w.Body.String())
	}
	// Once it is sending, nothing changes.
	f.app.DB.Model(&models.Message{}).Where("id = ?", m.ID).Update("status", models.MessageSending)
	for _, c := range []struct{ method, path string }{{"PUT", path}, {"POST", path + "/schedule"}, {"POST", path + "/cancel"}} {
		if w := f.leader.do(c.method, c.path, map[string]any{"subject": "x", "bodyHtml": "<p>x</p>", "audience": aud}, true); w.Code != http.StatusConflict {
			t.Fatalf("%s %s while sending: %d", c.method, c.path, w.Code)
		}
	}
	f.app.DB.Model(&models.Message{}).Where("id = ?", m.ID).Update("status", models.MessageScheduled)
	if w := f.leader.do("POST", path+"/cancel", nil, true); w.Code != http.StatusOK || auditCount(t, f.app, audit.MessageCancelled) != 1 {
		t.Fatalf("cancel: %d", w.Code)
	}
	// A message nobody can get can't be scheduled.
	solo := activeGroup(t, f.app, f.leaderUser, "Just me", models.VisibilityPublic, "")
	f.app.DB.Model(&models.User{}).Where("id = ?", f.leaderUser.ID).Update("email", "")
	decodeJSON(t, f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Hi", "bodyHtml": "<p>Hi</p>", "audience": map[string]any{"type": "group", "groupId": solo.ID}}, true), &out)
	if w := f.leader.do("POST", "/api/v1/messages/"+itoa(out.Message.ID)+"/schedule", confirmedSend(f.leader, "/api/v1/messages/"+itoa(out.Message.ID), map[string]any{}), true); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "message_nobody") {
		t.Fatalf("nobody: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Messages []messageJSON `json:"messages"`
	}
	decodeJSON(t, f.leader.do("GET", "/api/v1/messages", nil, false), &list)
	if len(list.Messages) != 2 {
		t.Fatalf("my messages: %d", len(list.Messages))
	}
}

func TestMessageListPagesFiltersAndAuthorScope(t *testing.T) {
	f := newComposeFixture(t)
	create := func(client *apiClient, groupID uint, subject string) messageJSON {
		t.Helper()
		w := client.do("POST", "/api/v1/messages", map[string]any{"subject": subject, "bodyHtml": "<p>Draft only</p>", "audience": map[string]any{"type": "group", "groupId": groupID}}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Message messageJSON `json:"message"`
		}
		decodeJSON(t, w, &out)
		return out.Message
	}
	for i := 0; i < 205; i++ {
		create(f.leader, f.group.ID, "History")
	}
	create(f.host, f.otherGroup.ID, "Other author's history")
	// Equal timestamps exercise the ID tie-breaker, not just timestamp paging.
	if err := f.app.DB.Model(&models.Message{}).Where("author_id = ?", f.leaderUser.ID).UpdateColumn("updated_at", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Error; err != nil {
		t.Fatal(err)
	}
	get := func(client *apiClient, query string) messageListJSON {
		t.Helper()
		w := client.do("GET", "/api/v1/messages?"+query, nil, false)
		if w.Code != http.StatusOK {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		var out messageListJSON
		decodeJSON(t, w, &out)
		return out
	}
	first := get(f.leader, "limit=25")
	if len(first.Messages) != 25 || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	create(f.leader, f.group.ID, "New after page one") // must not shift the next page
	seen := map[uint]bool{}
	page := first
	for {
		for _, m := range page.Messages {
			if seen[m.ID] || m.Subject != "History" {
				t.Fatalf("duplicate or wrong author/page: %+v", m)
			}
			seen[m.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		page = get(f.leader, "limit=25&cursor="+url.QueryEscape(page.NextCursor))
	}
	if len(seen) != 205 {
		t.Fatalf("history lost: got %d", len(seen))
	}
	if got := get(f.leader, ""); len(got.Messages) != 200 || got.NextCursor == "" {
		t.Fatal("default limit changed or history has no continuation")
	}
	create(f.leader, f.group.ID, "Literal 100% match")
	create(f.leader, f.group.ID, "Literal 100X match")
	if got := get(f.leader, "search=100%25&status=draft"); len(got.Messages) != 1 || got.Messages[0].Subject != "Literal 100% match" {
		t.Fatalf("literal search: %+v", got)
	}
	if got := get(f.leader, "search=100%25&status=sent"); len(got.Messages) != 0 {
		t.Fatal("status filter ignored")
	}
	if got := get(f.admin, "search=Other+author"); len(got.Messages) != 1 {
		t.Fatal("admin cannot browse all authors")
	}
	if got := get(f.leader, "search=Other+author&cursor="+url.QueryEscape(first.NextCursor)); len(got.Messages) != 0 {
		t.Fatal("cursor widened author access")
	}
	if got := get(f.leader, "category=reminder&cursor="+url.QueryEscape(first.NextCursor)); len(got.Messages) != 0 {
		t.Fatal("cursor widened category access")
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=no", "cursor=bad", "cursor=e30", "status=unknown"} {
		if w := f.leader.do("GET", "/api/v1/messages?"+query, nil, false); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", query, w.Code)
		}
	}
}

// "Push notification too" (M3.13d2, DEC-123) is on for a new message
// that leaves it out; false is kept, in the 201 too; an update that leaves
// it out keeps the stored value; and the composer hears whether the
// church allows push.
func TestMessagePushAlertDefaultsOn(t *testing.T) {
	f := newComposeFixture(t)
	aud := map[string]any{"type": "group", "groupId": f.group.ID}
	var out struct {
		Message messageJSON `json:"message"`
	}
	w := f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Off", "bodyHtml": "<p>Hi</p>", "audience": aud, "pushAlert": false}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &out)
	var off models.Message
	if f.app.DB.First(&off, out.Message.ID); out.Message.PushAlert || off.PushAlert {
		t.Fatalf("created with pushAlert false: response %v, stored %v", out.Message.PushAlert, off.PushAlert)
	}
	decodeJSON(t, f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Hi", "bodyHtml": "<p>Hi</p>", "audience": aud}, true), &out)
	if !out.Message.PushAlert {
		t.Fatal("pushAlert left out isn't on")
	}
	id := itoa(out.Message.ID)
	decodeJSON(t, f.leader.do("PUT", "/api/v1/messages/"+id, namedEdit(f.leader, "/api/v1/messages/"+id, map[string]any{"subject": "Hi", "bodyHtml": "<p>Hi</p>", "audience": aud, "pushAlert": false}), true), &out)
	var m models.Message
	if f.app.DB.First(&m, out.Message.ID); out.Message.PushAlert || m.PushAlert {
		t.Fatalf("pushAlert false: %v %v", out.Message.PushAlert, m.PushAlert)
	}
	decodeJSON(t, f.leader.do("PUT", "/api/v1/messages/"+id, namedEdit(f.leader, "/api/v1/messages/"+id, map[string]any{"subject": "Hi again", "bodyHtml": "<p>Hi</p>", "audience": aud}), true), &out)
	if f.app.DB.First(&m, out.Message.ID); out.Message.PushAlert || m.PushAlert || m.Subject != "Hi again" {
		t.Fatalf("an update that leaves pushAlert out changed it: %v %v", out.Message.PushAlert, m.PushAlert)
	}
	var opts composeOptionsJSON
	decodeJSON(t, f.leader.do("GET", "/api/v1/messages/compose", nil, false), &opts)
	if !opts.PushEnabled {
		t.Fatal("pushEnabled off by default")
	}
	f.app.DB.Create(&models.Setting{Key: settingPushEnabled, Value: "false"})
	decodeJSON(t, f.leader.do("GET", "/api/v1/messages/compose", nil, false), &opts)
	if opts.PushEnabled {
		t.Fatal("pushEnabled with the church's switch off")
	}
}

// An announcement is never push-only (DEC-123): push is a switch beside
// the email, so asking for channel "push" is refused with its own code,
// on create and on update.
func TestMessagePushOnlyIsRefused(t *testing.T) {
	f := newComposeFixture(t)
	aud := map[string]any{"type": "group", "groupId": f.group.ID}
	for _, ch := range []string{"push", "Push"} {
		w := f.leader.do("POST", "/api/v1/messages", map[string]any{"channel": ch, "subject": "Hi", "bodyHtml": "<p>Hi</p>", "audience": aud, "pushAlert": true}, true)
		if w.Code != http.StatusBadRequest || errorCodeOf(w) != "message_push_only" {
			t.Fatalf("create %q: %d %s", ch, w.Code, w.Body.String())
		}
	}
	var out struct {
		Message messageJSON `json:"message"`
	}
	decodeJSON(t, f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Hi", "bodyHtml": "<p>Hi</p>", "audience": aud}, true), &out)
	w := f.leader.do("PUT", "/api/v1/messages/"+itoa(out.Message.ID), namedEdit(f.leader, "/api/v1/messages/"+itoa(out.Message.ID), map[string]any{"channel": "push", "subject": "Hi", "bodyHtml": "<p>Hi</p>", "audience": aud}), true)
	if w.Code != http.StatusBadRequest || errorCodeOf(w) != "message_push_only" {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	var m models.Message
	if f.app.DB.First(&m, out.Message.ID); m.Channel != "email" {
		t.Fatalf("channel %q", m.Channel)
	}
}

// The preview counts who also gets a push: push on for messages from
// leaders and a device. Someone with push on but no device isn't a skip;
// they still get the email. Push follows push consent, not the email
// (DEC-122): someone who turned off email from leaders but has push on is
// counted and marked. Off for messages from leaders, or the church switch
// off, counts nobody.
func TestAudiencePreviewCountsPush(t *testing.T) {
	f := newComposeFixture(t)
	pushConsentRow(t, f.app, f.teen, "all", models.ConsentOptedIn)
	newPushDevice(t, f.app, f.teen, "teen-phone")
	pushConsentRow(t, f.app, f.leaderUser, "all", models.ConsentOptedIn) // no device
	// Opted out of email from leaders: skipped for the email, still pushed.
	pushConsentRow(t, f.app, f.opted, "all", models.ConsentOptedIn)
	newPushDevice(t, f.app, f.opted, "opted-phone")
	preview := func() audienceSummaryJSON {
		var out struct {
			Summary audienceSummaryJSON `json:"summary"`
		}
		decodeJSON(t, f.leader.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": map[string]any{"type": "group", "groupId": f.group.ID}}, true), &out)
		return out.Summary
	}
	s := preview()
	if s.Push != 2 || s.Eligible != 2 || len(s.Skipped) != 2 {
		t.Fatalf("summary: %+v", s)
	}
	for _, r := range s.Recipients {
		if want := r.Name == f.teen.PublicName() || r.Name == f.opted.PublicName(); r.Push != want {
			t.Fatalf("%s: push %v", r.Name, r.Push)
		}
	}
	pushConsentRow(t, f.app, f.teen, "category:"+notify.CatMessages, models.ConsentOptedOut)
	if s := preview(); s.Push != 1 || s.Eligible != 2 {
		t.Fatalf("push off for messages from leaders: %+v", s)
	}
	f.app.DB.Create(&models.Setting{Key: settingPushEnabled, Value: "false"})
	if s := preview(); s.Push != 0 {
		t.Fatalf("church off: %+v", s)
	}
}

// TestMessageListAndWriteRepliesReadOneSnapshot: the message list and the
// reply to a write judge the session's user and read what they show in one
// snapshot, as the message page does. At the seam, right after the viewer is
// judged, the admin is demoted (role and session version, as the admin
// screen does it) and, in the same commit, the leader writes a new message
// or renames themselves. Before that commit the admin saw neither; after it
// they see nothing. Each request runs on a one-connection pool, so a read
// outside its snapshot fails it. A demotion committed after the session was
// checked and before the snapshot began is a 401, never the earlier role.
func TestMessageListAndWriteRepliesReadOneSnapshot(t *testing.T) {
	const secret = "Written after the admin stepped down"
	demoteAnd := func(f composeFixture, db *gorm.DB, then func(tx *gorm.DB) error) func() {
		return func() {
			if err := db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Model(&models.User{}).Where("name = ?", "admin").
					Updates(map[string]any{"role": models.RoleMember, "session_version": sessionVersionBump()}).Error; err != nil {
					return err
				}
				return then(tx)
			}); err != nil {
				panic(err)
			}
		}
	}
	draft := func(t *testing.T, f composeFixture) uint {
		var out struct {
			Message messageJSON `json:"message"`
		}
		w := f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Leader news", "bodyHtml": "<p>Hello</p>",
			"audience": map[string]any{"type": policy.AudienceGroup, "groupId": f.group.ID}}, true)
		if w.Code != http.StatusCreated {
			t.Fatalf("draft: %d %s", w.Code, w.Body.String())
		}
		decodeJSON(t, w, &out)
		return out.Message.ID
	}
	type view struct {
		surface string
		// setup returns the admin's request, the change committed with the
		// demotion, and the handler that builds the same answer.
		setup func(t *testing.T, f composeFixture) (req func() *httptest.ResponseRecorder, change func(tx *gorm.DB) error, reply func(a *App, c *gin.Context))
	}
	for _, v := range []view{
		{"message list", func(t *testing.T, f composeFixture) (func() *httptest.ResponseRecorder, func(tx *gorm.DB) error, func(*App, *gin.Context)) {
			draft(t, f)
			return func() *httptest.ResponseRecorder { return f.admin.do("GET", "/api/v1/messages", nil, false) },
				func(tx *gorm.DB) error {
					return tx.Create(&models.Message{AuthorID: &f.leaderUser.ID, Status: models.MessageDraft, Category: models.MessageCategoryAnnouncement,
						Subject: secret, BodyHTML: "<p>" + secret + "</p>", BodyText: secret, Audience: `{"type":"group","groupId":1}`}).Error
				},
				func(a *App, c *gin.Context) { a.apiListMessages(c) }
		}},
		{"message reply", func(t *testing.T, f composeFixture) (func() *httptest.ResponseRecorder, func(tx *gorm.DB) error, func(*App, *gin.Context)) {
			id := draft(t, f)
			return func() *httptest.ResponseRecorder {
					return f.admin.do("POST", "/api/v1/messages/"+itoa(id)+"/cancel", nil, true)
				},
				func(tx *gorm.DB) error {
					return tx.Model(&models.User{}).Where("id = ?", f.leaderUser.ID).Update("display_name", secret).Error
				},
				func(a *App, c *gin.Context) { a.respondMessage(c, http.StatusOK, &models.Message{ID: id}) }
		}},
	} {
		t.Run(v.surface, func(t *testing.T) {
			f := newComposeFixture(t)
			req, change, _ := v.setup(t, f)
			orig := f.app.DB
			fired := atReaderSeam(f.app, "authorized", v.surface, demoteAnd(f, orig, change))
			one := oneConnectionPool(t, f.app)
			oneConnection(t, f.app, one)
			w := req()
			f.app.DB = orig
			f.app.hooks.readerAuthorized = nil
			if !fired.Load() {
				t.Fatalf("%s never reached its seam", v.surface)
			}
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), secret) {
				t.Fatalf("%s paired the old role with a later change: %d %s", v.surface, w.Code, w.Body.String())
			}
			if after := req(); after.Code != http.StatusUnauthorized || strings.Contains(after.Body.String(), secret) {
				t.Fatalf("%s after the admin stepped down: %d %s", v.surface, after.Code, after.Body.String())
			}
		})
		t.Run(v.surface+" demoted before the snapshot", func(t *testing.T) {
			f := newComposeFixture(t)
			_, _, reply := v.setup(t, f)
			var checked models.User
			gormMust(t, f.app.DB.Where("name = ?", "admin").First(&checked))
			demoteAnd(f, f.app.DB, func(*gorm.DB) error { return nil })()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("GET", "/x", nil)
			c.Set("apiUser", &checked)
			reply(f.app, c)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s for a session demoted before its snapshot: %d %s", v.surface, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestWriteReplyAnswers404WhenTheViewerMayNotSeeTheMessage: a write's reply
// is presented by the viewer's right as the snapshot reads it, so a current
// session that may not see the message (not its author, not an admin) gets
// 404, never the message.
func TestWriteReplyAnswers404WhenTheViewerMayNotSeeTheMessage(t *testing.T) {
	f := newComposeFixture(t)
	m := models.Message{AuthorID: &f.leaderUser.ID, Status: models.MessageDraft, Category: models.MessageCategoryAnnouncement,
		Subject: "Leader only", BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: `{"type":"group","groupId":1}`}
	gormMust(t, f.app.DB.Create(&m))
	var member models.User
	gormMust(t, f.app.DB.Where("name = ?", "member").First(&member))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("GET", "/x", nil)
	c.Set("apiUser", &member)
	f.app.respondMessage(c, http.StatusOK, &models.Message{ID: m.ID})
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Leader only") {
		t.Fatalf("a reply to someone who may not see the message: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMessageViewsJudgeTheSessionInTheirSnapshot: a message's page, its
// history, its preview (JSON and HTML), its test send and its read page decide
// the session, whether the message is there and the right to it in one
// snapshot, in that order. A session revoked after the middleware checked it
// gets 401 for an existing announcement, an automatic reminder and an id that
// isn't there alike, never a 404 or the reminder's 409 decided from the
// earlier user. A current admin still gets 404 for a missing id and 409 for a
// reminder's preview and test; a member who may not see the reminder's
// history, or read a message not sent to them, gets 404, never the 409 that
// would say it exists.
func TestMessageViewsJudgeTheSessionInTheirSnapshot(t *testing.T) {
	f := newComposeFixture(t)
	var adminU, memberU models.User
	gormMust(t, f.app.DB.Where("name = ?", "admin").First(&adminU))
	gormMust(t, f.app.DB.Where("name = ?", "member").First(&memberU))
	announcement := models.Message{AuthorID: &adminU.ID, Status: models.MessageDraft, Category: models.MessageCategoryAnnouncement,
		Subject: "News", BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: `{"type":"group","groupId":` + itoa(f.group.ID) + `}`}
	gormMust(t, f.app.DB.Create(&announcement))
	soon := time.Now().Add(time.Hour)
	reminder := models.Message{Status: models.MessageScheduled, ScheduledAt: &soon, Category: models.MessageCategoryReminder, OccurrenceID: &f.groupOcc.ID,
		Subject: "Reminder", BodyHTML: "<p>Soon</p>", BodyText: "Soon", Audience: `{"type":"occurrence","occurrenceId":` + itoa(f.groupOcc.ID) + `}`}
	gormMust(t, f.app.DB.Create(&reminder))
	missing := reminder.ID + 1000
	surfaces := []struct {
		name    string
		method  string
		suffix  string
		handler func(*App, *gin.Context)
		edits   bool
	}{
		{"message", "GET", "", (*App).apiGetMessage, false},
		{"recipients", "GET", "/recipients", (*App).apiMessageRecipients, false},
		{"preview", "GET", "/preview", (*App).apiPreviewMessage, true},
		{"preview html", "GET", "/preview.html", (*App).apiPreviewMessageHTML, true},
		{"test", "POST", "/test", (*App).apiTestMessage, true},
		{"read", "GET", "/read", (*App).apiReadMessage, false},
	}
	call := func(s int, viewer *models.User, id uint) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(surfaces[s].method, "/api/v1/messages/"+itoa(id)+surfaces[s].suffix, nil)
		c.Params = gin.Params{{Key: "id", Value: itoa(id)}}
		c.Set("apiUser", viewer)
		surfaces[s].handler(f.app, c)
		return rec
	}
	for s, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			var admin, member models.User
			gormMust(t, f.app.DB.First(&admin, adminU.ID))
			gormMust(t, f.app.DB.First(&member, memberU.ID))
			if w := call(s, &admin, missing); w.Code != http.StatusNotFound {
				t.Fatalf("a current admin, missing id: %d %s", w.Code, w.Body.String())
			}
			want := http.StatusOK
			if surface.edits {
				want = http.StatusConflict
			}
			if w := call(s, &admin, reminder.ID); w.Code != want {
				t.Fatalf("a current admin, the reminder: %d %s, want %d", w.Code, w.Body.String(), want)
			}
			for _, id := range []uint{announcement.ID, reminder.ID} {
				if w := call(s, &member, id); w.Code != http.StatusNotFound {
					t.Fatalf("a member who may not see message %d: %d %s", id, w.Code, w.Body.String())
				}
			}
			// Revoked after the middleware passed this user.
			gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", admin.ID).Update("session_version", gorm.Expr("session_version + 1")))
			for _, id := range []uint{announcement.ID, reminder.ID, missing} {
				if w := call(s, &admin, id); w.Code != http.StatusUnauthorized {
					t.Fatalf("a session revoked before the snapshot, message %d: %d %s", id, w.Code, w.Body.String())
				}
			}
		})
	}
}
