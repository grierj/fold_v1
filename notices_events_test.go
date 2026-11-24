package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/mail"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// noticesSince is every row of kind for uid written after mark.
func noticesSince(t *testing.T, app *App, uid uint, kind string, mark uint) []models.Notification {
	t.Helper()
	var rows []models.Notification
	gormMust(t, app.DB.Where("user_id = ? AND id > ? AND kind = ?", uid, mark, kind).Order("id").Find(&rows))
	return rows
}

// wantWithheld checks that n follows nothing about the date occID it was
// written for: no subject, expiry or data and a link nowhere. The recipient's
// feed shows no subjectId; moving the date to start within the hour, or to
// have ended, neither makes its push urgent or late nor archives it. client
// is the recipient's session.
func wantWithheld(t *testing.T, app *App, who string, n models.Notification, client *apiClient, occID uint) {
	t.Helper()
	if n.SubjectType != "" || n.SubjectID != nil || n.ExpiresAt != nil || n.Data != "{}" || n.Link != "/" {
		t.Fatalf("%s's notice follows the date: subject %q %v expires %v data %s link %q", who, n.SubjectType, n.SubjectID, n.ExpiresAt, n.Data, n.Link)
	}
	k, ok := notify.Lookup(n.Kind)
	if !ok {
		t.Fatalf("unknown kind %q", n.Kind)
	}
	orig := occurrenceRow(t, app, occID)
	t.Cleanup(func() {
		gormMust(t, app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occID).Updates(map[string]any{"starts_at": orig.StartsAt, "ends_at": orig.EndsAt}))
	})
	now := time.Now().UTC()
	for _, clock := range [][2]time.Time{{now.Add(30 * time.Minute), now.Add(90 * time.Minute)}, {now.Add(-2 * time.Hour), now.Add(-time.Hour)}} {
		gormMust(t, app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occID).Updates(map[string]any{"starts_at": clock[0], "ends_at": clock[1]}))
		soon, over, err := app.startsSoon(context.Background(), k, &n, now)
		gormMust(t, &gorm.DB{Error: err})
		if soon || over {
			t.Fatalf("%s's push follows the hidden date's clock: soon %v over %v", who, soon, over)
		}
		var page struct {
			Notifications []map[string]any `json:"notifications"`
		}
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(apiDo(t, client, "GET", "/api/v1/notifications?filter=all&limit=50", nil, http.StatusOK)), &page)})
		found := false
		for _, item := range page.Notifications {
			if id, _ := item["id"].(float64); uint(id) != n.ID {
				continue
			}
			found = true
			if _, has := item["subjectId"]; has || item["archived"] != false {
				t.Fatalf("%s's feed item follows the date: %v", who, item)
			}
		}
		if !found {
			t.Fatalf("%s's feed has no current notice %d", who, n.ID)
		}
	}
}

// noticeMark is the newest notification and outbox mail before a change,
// with every queued notice delivered first.
type noticeMark struct{ notice, mail uint }

func markNotices(t *testing.T, app *App) noticeMark {
	t.Helper()
	drainNotices(t, app)
	var m noticeMark
	gormMust(t, app.DB.Model(&models.Notification{}).Select("COALESCE(MAX(id), 0)").Scan(&m.notice))
	gormMust(t, app.DB.Model(&models.MailOutbox{}).Select("COALESCE(MAX(id), 0)").Scan(&m.mail))
	return m
}

// mailSince is every outbox mail to address after the mark.
func mailSince(t *testing.T, app *App, address string, mark noticeMark) []mail.OutboxMessage {
	t.Helper()
	msgs, err := devOutbox(t, app).List(200)
	gormMust(t, &gorm.DB{Error: err})
	var out []mail.OutboxMessage
	for _, m := range msgs {
		if m.To == address && m.ID > mark.mail {
			out = append(out, m)
		}
	}
	return out
}

// wantGenericCancellation checks that uid got exactly one event.cancelled
// about occID since mark, and its email, that both name nothing about the
// date, and that no mail to address since mark carries one of the secrets.
func wantGenericCancellation(t *testing.T, app *App, who string, uid, occID uint, mark noticeMark, address string, client *apiClient, secrets []string) {
	t.Helper()
	rows := noticesSince(t, app, uid, notify.OccurrenceCancelled, mark.notice)
	if len(rows) != 1 {
		t.Fatalf("%s got %d cancellation notices: %+v", who, len(rows), rows)
	}
	n := rows[0]
	if n.Title != "Cancelled: Church event" || n.Body != "" || n.Link != "/" {
		t.Fatalf("%s's cancellation is not the generic one: title %q body %q link %q", who, n.Title, n.Body, n.Link)
	}
	wantWithheld(t, app, who, n, client, occID)
	raw, err := json.Marshal(n)
	gormMust(t, &gorm.DB{Error: err})
	msgs := mailSince(t, app, address, mark)
	mailed := false
	for _, m := range msgs {
		mailed = mailed || strings.Contains(m.Subject, "Cancelled: Church event")
	}
	if !mailed {
		t.Fatalf("%s was not mailed the cancellation: %+v", who, msgs)
	}
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Fatalf("%s's cancellation carries %q: %s", who, s, raw)
		}
		for _, m := range msgs {
			if strings.Contains(m.Subject+m.Body, s) {
				t.Fatalf("%s was mailed %q: %q / %q", who, s, m.Subject, m.Body)
			}
		}
	}
}

// wantFullCancellation checks that uid got exactly one event.cancelled about
// occID since mark naming the title and the place.
func wantFullCancellation(t *testing.T, app *App, who string, uid, occID uint, mark noticeMark, title, place string) {
	t.Helper()
	rows := noticesSince(t, app, uid, notify.OccurrenceCancelled, mark.notice)
	if len(rows) != 1 || !strings.Contains(rows[0].Title, title) || !strings.Contains(rows[0].Body, place) ||
		rows[0].SubjectID == nil || *rows[0].SubjectID != occID || rows[0].ExpiresAt == nil {
		t.Fatalf("%s should be told %q at %q: %+v", who, title, place, rows)
	}
}

// A series edit that drops a date again, after its departure grant ended,
// tells the departed person only that something they signed up for was
// cancelled: not the title or place the same edit set, nor the group's. An
// active member still going is told everything.
func TestSeriesCancelWithholdsTheDateFromAnEndedGrant(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{"title_override": nil, "location_override": nil}))
	member := newTestUser(t, f.app, "still going member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	eid := itoa(f.event.ID)

	apiDo(t, f.admin, "PUT", "/api/v1/events/"+eid, seriesBody(t, f.admin, "/api/v1/events/"+eid, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=1"}), http.StatusOK)
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessEnded {
		t.Fatalf("fixture: dropping the date left the grant %+v", got)
	}
	apiDo(t, f.admin, "PUT", "/api/v1/events/"+eid, seriesBody(t, f.admin, "/api/v1/events/"+eid, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=3",
		"title": "NEWSECRETTITLE", "location": "NEWSECRETPLACE"}), http.StatusOK)
	if s := regStatus(t, f.app, f.recipient.ID, target.ID); s != models.RegGoing {
		t.Fatalf("fixture: the departed row is %s", s)
	}
	mark := markNotices(t, f.app)

	apiDo(t, f.admin, "PUT", "/api/v1/events/"+eid, seriesBody(t, f.admin, "/api/v1/events/"+eid, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=1",
		"title": "SECONDSECRET", "location": "SECONDPLACE"}), http.StatusOK)
	if o := occurrenceRow(t, f.app, target.ID); o.Status != models.OccurrenceCancelled || o.CancelSource != models.CancelSourceSeries {
		t.Fatalf("the date was not dropped again: %+v", o)
	}
	wantGenericCancellation(t, f.app, "the departed person", f.recipient.ID, target.ID, mark, f.recipient.Email, f.client,
		[]string{"NEWSECRETTITLE", "NEWSECRETPLACE", "SECONDSECRET", "SECONDPLACE", "Retained hidden group", "Retained group place", "/events/" + eid})
	wantFullCancellation(t, f.app, "the member still going", member.ID, target.ID, mark, "SECONDSECRET", "SECONDPLACE")
}

// Cancelling a date of an archived group by hand tells the member and the
// outsider still holding a Going row only that something they signed up for
// was cancelled, not the title or place set while the group was archived;
// the teen's guardians get no copy. The leader, who still sees the date, is
// told everything.
func TestManualCancelWithholdsAnArchivedGroupsDate(t *testing.T) {
	f := newDateEditFixture(t)
	target := f.dates[1]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{"visibility_override": models.VisibilityPublic, "title_override": "Open supper"}))
	teen := f.outsiders[0]
	gormMust(t, f.app.DB.Model(teen).Update("account_type", models.AccountTeen))
	var guardians []*models.User
	for _, name := range []string{"cancel teen mom", "cancel teen dad"} {
		g := newTestUser(t, f.app, name, "password1234", models.RoleMember)
		gormMust(t, f.app.DB.Model(g).Update("account_type", models.AccountAdult))
		gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: g.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}))
		guardians = append(guardians, g)
	}
	gormMust(t, f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
	for _, u := range []*models.User{teen, f.memberUser, f.leaderUser} {
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &u.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	}

	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+itoa(f.group), groupBody(t, f.admin, "/api/v1/groups/"+itoa(f.group), map[string]any{"status": models.GroupArchived}), http.StatusOK)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(1))
	wantDateEdit(t, f.admin, f.event.ID, f.date(1), dateEditBody(base.Revision,
		map[string]any{"title": "HIDDENTITLE", "location": "HIDDENPLACE"}), http.StatusOK, "")
	for _, u := range []*models.User{teen, f.memberUser, f.leaderUser} {
		if s := regStatus(t, f.app, u.ID, target.ID); s != models.RegGoing {
			t.Fatalf("fixture: %s is %s", u.Name, s)
		}
	}
	mark := markNotices(t, f.app)

	apiDo(t, f.admin, "POST", "/api/v1/events/"+itoa(f.event.ID)+"/cancel", map[string]any{"date": f.date(1)}, http.StatusOK)
	secrets := []string{"HIDDENTITLE", "HIDDENPLACE", "Open supper", "Supper club", "Group hall"}
	wantGenericCancellation(t, f.app, "the teen", teen.ID, target.ID, mark, teen.Email, loginAPI(t, f.app, teen.Name, "password1234"), secrets)
	wantGenericCancellation(t, f.app, "the member", f.memberUser.ID, target.ID, mark, f.memberUser.Email, f.member, secrets)
	wantFullCancellation(t, f.app, "the leader", f.leaderUser.ID, target.ID, mark, "HIDDENTITLE", "HIDDENPLACE")
	for _, g := range guardians {
		if got := copiesTo(t, f.app, g.Email); len(got) != 0 {
			t.Fatalf("%s got copies %v of a notice about a date the teen can't see", g.Email, got)
		}
		for _, m := range mailSince(t, f.app, g.Email, noticeMark{}) {
			for _, s := range secrets {
				if strings.Contains(m.Subject+m.Body, s) {
					t.Fatalf("%s was mailed %q: %q / %q", g.Email, s, m.Subject, m.Body)
				}
			}
		}
	}
}

// regOf is uid's own registration on occID.
func regOf(t *testing.T, app *App, uid, occID uint) models.Registration {
	t.Helper()
	var r models.Registration
	gormMust(t, app.DB.Where("user_id = ? AND occurrence_id = ?", uid, occID).First(&r))
	return r
}

// noSecrets fails when a notice written to uid after mark, or a mail to
// address after mark, carries one of the secrets.
func noSecrets(t *testing.T, app *App, who string, uid uint, address string, mark noticeMark, secrets []string) {
	t.Helper()
	drainNotices(t, app)
	var rows []models.Notification
	gormMust(t, app.DB.Where("user_id = ? AND id > ?", uid, mark.notice).Find(&rows))
	for _, n := range rows {
		raw, err := json.Marshal(n)
		gormMust(t, &gorm.DB{Error: err})
		for _, s := range secrets {
			if strings.Contains(string(raw), s) {
				t.Fatalf("%s was told %q: %s", who, s, raw)
			}
		}
	}
	for _, m := range mailSince(t, app, address, mark) {
		for _, s := range secrets {
			if strings.Contains(m.Subject+m.Body, s) {
				t.Fatalf("%s was mailed %q: %q / %q", who, s, m.Subject, m.Body)
			}
		}
	}
}

// A leader's removal of a row kept after its grant ended tells the departed
// person they were removed, with the note written to them, but not the title,
// time or link of the invite-only date they can no longer see. A member still
// going who is removed is told which date.
func TestRemovalWithholdsTheDateFromAnEndedGrant(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{"title_override": nil, "location_override": nil}))
	member := newTestUser(t, f.app, "removed member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: member.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	eid := itoa(f.event.ID)
	apiDo(t, f.admin, "PUT", "/api/v1/events/"+eid, seriesBody(t, f.admin, "/api/v1/events/"+eid, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=1"}), http.StatusOK)
	apiDo(t, f.admin, "PUT", "/api/v1/events/"+eid, seriesBody(t, f.admin, "/api/v1/events/"+eid, map[string]any{"recurrenceRule": "FREQ=WEEKLY;COUNT=3",
		"title": "NEWSECRETTITLE", "location": "NEWSECRETPLACE"}), http.StatusOK)
	if got := savedOccurrenceAccess(t, f); got.Status != policy.OccurrenceAccessEnded || regStatus(t, f.app, f.recipient.ID, target.ID) != models.RegGoing {
		t.Fatalf("fixture: the grant %+v", got)
	}
	date := models.FormatCalendarDate(target.OriginalDate)
	mark := markNotices(t, f.app)

	apiDo(t, f.admin, "POST", "/api/v1/events/"+eid+"/registrations/remove", map[string]any{
		"registrationId": regOf(t, f.app, f.recipient.ID, target.ID).ID, "date": date, "note": "Bring a coat next time"}, http.StatusOK)
	apiDo(t, f.admin, "POST", "/api/v1/events/"+eid+"/registrations/remove", map[string]any{
		"registrationId": regOf(t, f.app, member.ID, target.ID).ID, "date": date}, http.StatusOK)
	got := noticesSince(t, f.app, f.recipient.ID, notify.RegistrationRemoved, mark.notice)
	if len(got) != 1 || got[0].Title != "Registration removed: an event" ||
		got[0].Body != "A leader or host removed your registration for an event. Note: Bring a coat next time" {
		t.Fatalf("the departed person's removal notices: %+v", got)
	}
	wantWithheld(t, f.app, "the departed person", got[0], f.client, target.ID)
	noSecrets(t, f.app, "the departed person", f.recipient.ID, f.recipient.Email, mark,
		[]string{"NEWSECRETTITLE", "NEWSECRETPLACE", "Retained hidden group", "Retained group place", "/events/" + eid, "/groups/"})
	told := noticesSince(t, f.app, member.ID, notify.RegistrationRemoved, mark.notice)
	if len(told) != 1 || told[0].Title != "Registration removed: NEWSECRETTITLE" || told[0].SubjectID == nil || *told[0].SubjectID != target.ID {
		t.Fatalf("the member removed: %+v", told)
	}
}

// Re-enabling an account tells its holder about each answer that did not come
// back (DEC-47). A date that was full is named with the reason; a date the
// person can no longer see (its group was archived, then the date renamed
// while they were away) names nothing, and says plainly why rather than
// claiming it was full.
func TestReenableWithholdsAHiddenDate(t *testing.T) {
	f := newDateEditFixture(t)
	full, hidden := f.dates[1], f.dates[2]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", full.ID).Updates(map[string]any{"visibility_override": models.VisibilityPublic, "capacity_override": 1}))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", hidden.ID).Update("visibility_override", models.VisibilityPublic))
	waiter, away := f.outsiders[1], f.outsiders[0]
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &waiter.ID, OccurrenceID: full.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &away.ID, OccurrenceID: hidden.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	for _, u := range []*models.User{waiter, away} {
		apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(u.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
	}
	// While the first is away the member takes the only seat.
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: full.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	mark := markNotices(t, f.app)
	apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(waiter.ID), map[string]any{"status": models.UserActive}, http.StatusOK)
	got := noticesSince(t, f.app, waiter.ID, notify.RegistrationNotRestored, mark.notice)
	if len(got) != 1 || got[0].Title != "You're not back in Supper yet" ||
		got[0].Body != "The event was full when your account was turned back on. Ask a leader or host to add you." ||
		got[0].SubjectID == nil || *got[0].SubjectID != full.ID {
		t.Fatalf("the person left out of a full date: %+v", got)
	}

	// While the second is away the group is archived and the date renamed.
	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+itoa(f.group), groupBody(t, f.admin, "/api/v1/groups/"+itoa(f.group), map[string]any{"status": models.GroupArchived}), http.StatusOK)
	base := editBaseOf(t, f.admin, f.event.ID, f.date(2))
	wantDateEdit(t, f.admin, f.event.ID, f.date(2), dateEditBody(base.Revision,
		map[string]any{"title": "HIDDENTITLE", "location": "HIDDENPLACE"}), http.StatusOK, "")
	mark = markNotices(t, f.app)
	apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(away.ID), map[string]any{"status": models.UserActive}, http.StatusOK)
	var kept int64
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("user_id = ? AND occurrence_id = ?", away.ID, hidden.ID).Count(&kept))
	if kept != 0 {
		t.Fatalf("fixture: the hidden date's answer came back (%d rows)", kept)
	}
	got = noticesSince(t, f.app, away.ID, notify.RegistrationNotRestored, mark.notice)
	if len(got) != 1 || got[0].Title != "You're not back in one of your events yet" ||
		got[0].Body != "Your sign-up couldn't come back when your account was turned back on. Ask a leader or host to add you." {
		t.Fatalf("the person whose date is hidden: %+v", got)
	}
	wantWithheld(t, f.app, "the person whose date is hidden", got[0], loginAPI(t, f.app, away.Name, "password1234"), hidden.ID)
	noSecrets(t, f.app, "the person whose date is hidden", away.ID, away.Email, mark,
		[]string{"HIDDENTITLE", "HIDDENPLACE", "Supper", "Group hall", "/events/", "/groups/"})
}

// A guardian who holds their own row on a date they can't see, beside their
// teen who can, is a direct recipient of the cancellation: they get the
// nameless notice, and no copy of the teen's, which names the date.
func TestCancelGuardianWithOwnRowGetsNoCopyOfTheTeensNotice(t *testing.T) {
	f := newDateEditFixture(t)
	target := f.dates[1]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("title_override", "TEENONLYTITLE"))
	teen := newTestUser(t, f.app, "member teen", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(teen).Update("account_type", models.AccountTeen))
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: teen.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	guardian := f.outsiders[0]
	gormMust(t, f.app.DB.Model(guardian).Update("account_type", models.AccountAdult))
	gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}))
	gormMust(t, f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
	for _, u := range []*models.User{teen, guardian} {
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &u.ID, OccurrenceID: target.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	}
	mark := markNotices(t, f.app)

	apiDo(t, f.admin, "POST", "/api/v1/events/"+itoa(f.event.ID)+"/cancel", map[string]any{"date": f.date(1)}, http.StatusOK)
	wantFullCancellation(t, f.app, "the teen", teen.ID, target.ID, mark, "TEENONLYTITLE", "Group hall")
	wantGenericCancellation(t, f.app, "the guardian", guardian.ID, target.ID, mark, guardian.Email, loginAPI(t, f.app, guardian.Name, "password1234"),
		[]string{"TEENONLYTITLE", "Group hall", "Supper club"})
	noSecrets(t, f.app, "the guardian", guardian.ID, guardian.Email, mark, []string{"TEENONLYTITLE", "Group hall", "Supper club"})
	if got := copiesTo(t, f.app, guardian.Email); len(got) != 0 {
		t.Fatalf("the guardian got copies %v of the teen's notice", got)
	}
}

// A removal or a promotion that passed its scheduled check, then waited for
// the contact gate while the date was cancelled and renamed, changes nothing
// and tells nobody: under the lock the date is not on the event. The holder,
// an outsider of a public date, would otherwise be told the new title.
func TestRemovalAndPromotionRecheckTheDateUnderTheGate(t *testing.T) {
	for _, tc := range []struct {
		action, status, kind, audit string
	}{
		{"remove", models.RegGoing, notify.RegistrationRemoved, audit.RegistrationRemoved},
		{"promote", models.RegWaitlisted, notify.RegistrationPromoted, audit.RegistrationPromoted},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f := newDateEditFixture(t)
			target := f.dates[1]
			gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Update("visibility_override", models.VisibilityPublic))
			holder := f.outsiders[0]
			since := time.Now().UTC()
			reg := models.Registration{UserID: &holder.ID, OccurrenceID: target.ID, Status: tc.status, AttendanceMode: models.AttendanceInPerson, Source: "web"}
			if tc.status == models.RegWaitlisted {
				reg.WaitlistedAt = &since
			}
			gormMust(t, f.app.DB.Create(&reg))
			mark := markNotices(t, f.app)

			gate := holdContactGate(t, f.app)
			client := cloneContactClient(f.admin)
			path := "/api/v1/events/" + itoa(f.event.ID) + "/registrations/" + tc.action
			answer := queueContact(func() *httptest.ResponseRecorder {
				return client.do("POST", path, map[string]any{"registrationId": reg.ID, "date": f.date(1), "note": "See you"}, true)
			})
			waitContactWaiters(t, f.app, 1)
			gormMust(t, gate.Model(&models.EventOccurrence{}).Where("id = ?", target.ID).Updates(map[string]any{
				"status": models.OccurrenceCancelled, "cancel_source": models.CancelSourceManual, "title_override": "RENAMEDSECRET"}))
			gormMust(t, gate.Commit())
			if w := awaitContactResponse(t, answer); w.Code != http.StatusNotFound {
				t.Fatalf("%s into a date cancelled while it waited answered %d %s", tc.action, w.Code, w.Body.String())
			}
			if s := regStatus(t, f.app, holder.ID, target.ID); s != tc.status {
				t.Fatalf("the holder's row became %s", s)
			}
			var audits int64
			gormMust(t, f.app.DB.Model(&models.AuditLog{}).Where("action = ? AND target_id = ?", tc.audit, f.event.ID).Count(&audits))
			if audits != 0 {
				t.Fatalf("%d %s audit rows for a refused %s", audits, tc.audit, tc.action)
			}
			if got := noticesSince(t, f.app, holder.ID, tc.kind, mark.notice); len(got) != 0 {
				t.Fatalf("the holder was told about a cancelled date: %+v", got)
			}
			noSecrets(t, f.app, "the holder", holder.ID, holder.Email, mark, []string{"RENAMEDSECRET"})
		})
	}
}

// A promotion off the waitlist, automatic or by hand, tells an invited member
// and a pending-consent teen member that they are in but not which date of the
// invite-only group: neither account has a view of it, and the stored row is
// what they read once their account is active. Neither row carries the manage
// link or the video link. An active member, and an invited holder of a public
// date, which a signed-out visitor may see, are told the date as before.
func TestPromotionWithholdsAPrivateDateFromAnInactiveHolder(t *testing.T) {
	f := newDateEditFixture(t)
	const video = "https://video.example/VIDEOSECRET"
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Updates(map[string]any{
		"video_provider": models.VideoProviderLink, "video_url": video, "video_reveal": models.VideoRevealRegisteredOnly}))
	invited := newTestUser(t, f.app, "invited member", "password1234", models.RoleMember)
	teen := newTestUser(t, f.app, "consent teen member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(invited).Update("status", models.UserInvited))
	gormMust(t, f.app.DB.Model(teen).Updates(map[string]any{"status": models.UserPendingConsent, "account_type": models.AccountTeen}))
	publicInvitee := f.outsiders[0]
	gormMust(t, f.app.DB.Model(publicInvitee).Update("status", models.UserInvited))
	for _, u := range []*models.User{invited, teen} {
		gormMust(t, f.app.DB.Create(&models.Membership{UserID: u.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	}
	cases := []struct {
		who    string
		u      *models.User
		date   int
		manual bool
	}{
		{"the invited member", invited, 0, false},
		{"the pending-consent teen", teen, 1, true},
		{"the active member", f.memberUser, 2, false},
		{"the invited holder of a public date", publicInvitee, 3, false},
	}
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[3].ID).Update("visibility_override", models.VisibilityPublic))
	since := time.Now().UTC().Add(-time.Hour)
	eid := itoa(f.event.ID)
	for _, tc := range cases {
		occ := f.dates[tc.date]
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occ.ID).Updates(map[string]any{
			"capacity_override": 1, "title_override": "DATESECRET" + itoa(uint(tc.date)), "location_override": "PLACESECRET"}))
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.leaderUser.ID, OccurrenceID: occ.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &tc.u.ID, OccurrenceID: occ.ID, Status: models.RegWaitlisted, WaitlistedAt: &since, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	}
	mark := markNotices(t, f.app)
	for _, tc := range cases {
		occ := f.dates[tc.date]
		if tc.manual {
			gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occ.ID).Update("capacity_override", 2))
			apiDo(t, f.admin, "POST", "/api/v1/events/"+eid+"/registrations/promote", map[string]any{
				"registrationId": regOf(t, f.app, tc.u.ID, occ.ID).ID, "date": f.date(tc.date)}, http.StatusOK)
		} else {
			apiDo(t, f.admin, "POST", "/api/v1/events/"+eid+"/registrations/remove", map[string]any{
				"registrationId": regOf(t, f.app, f.leaderUser.ID, occ.ID).ID, "date": f.date(tc.date)}, http.StatusOK)
		}
		if s := regStatus(t, f.app, tc.u.ID, occ.ID); s != models.RegGoing {
			t.Fatalf("%s was not promoted: %s", tc.who, s)
		}
	}
	drainNotices(t, f.app)

	for _, tc := range cases[2:] {
		occ := f.dates[tc.date]
		got := noticesSince(t, f.app, tc.u.ID, notify.RegistrationPromoted, mark.notice)
		title := "You're in: DATESECRET" + itoa(uint(tc.date))
		if len(got) != 1 || got[0].Title != title || got[0].SubjectID == nil || *got[0].SubjectID != occ.ID || got[0].ExpiresAt == nil ||
			!strings.Contains(got[0].Body, "PLACESECRET") || !strings.Contains(got[0].EmailBody, "/guest/manage/") || !strings.Contains(got[0].EmailBody, video) {
			t.Errorf("%s should be told %q with the manage and video links: %+v", tc.who, title, got)
		}
	}
	secrets := []string{"DATESECRET", "PLACESECRET", "Supper", "Group hall", "/events/", "/groups/", "/guest/manage/", "VIDEOSECRET"}
	for _, tc := range cases[:2] {
		got := noticesSince(t, f.app, tc.u.ID, notify.RegistrationPromoted, mark.notice)
		if len(got) != 1 || got[0].Title != "You're in: an event" || got[0].Body != "" {
			t.Errorf("%s's promotion notices: %+v", tc.who, got)
			continue
		}
		noSecrets(t, f.app, tc.who, tc.u.ID, tc.u.Email, mark, secrets)
		gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", tc.u.ID).Update("status", models.UserActive))
		wantWithheld(t, f.app, tc.who+" once active", got[0], loginAPI(t, f.app, tc.u.Name, "password1234"), f.dates[tc.date].ID)
		noSecrets(t, f.app, tc.who+" once active", tc.u.ID, tc.u.Email, mark, secrets)
	}
}

// Re-enabling an admin as a member in one edit judges their answers by the
// member they come back as. A date of an archived group, which only an admin
// may still see, is not kept even with a seat free, and the notices name
// neither date. An admin re-enabled as an admin, whose reach needs both the
// restored status and the role, is told which full date did not come back.
func TestReenableAsMemberJudgesTheRoleItComesBackTo(t *testing.T) {
	f := newDateEditFixture(t)
	roomy, full := f.dates[2], f.dates[3]
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", full.ID).Update("capacity_override", 1))
	demoted := newTestUser(t, f.app, "demoted admin", "password1234", models.RoleAdmin)
	kept := newTestUser(t, f.app, "kept admin", "password1234", models.RoleAdmin)
	for _, r := range []struct {
		u   *models.User
		occ uint
	}{{demoted, roomy.ID}, {demoted, full.ID}, {kept, full.ID}} {
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &r.u.ID, OccurrenceID: r.occ, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	}
	for _, u := range []*models.User{demoted, kept} {
		apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(u.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
	}
	// While both are away the member takes the full date's only seat, the
	// group is archived and both dates are renamed.
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: full.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	apiDo(t, f.admin, "PUT", "/api/v1/groups/"+itoa(f.group), groupBody(t, f.admin, "/api/v1/groups/"+itoa(f.group), map[string]any{"status": models.GroupArchived}), http.StatusOK)
	for i, title := range map[int]string{2: "ROOMYSECRET", 3: "FULLSECRET"} {
		base := editBaseOf(t, f.admin, f.event.ID, f.date(i))
		wantDateEdit(t, f.admin, f.event.ID, f.date(i), dateEditBody(base.Revision,
			map[string]any{"title": title, "location": "HIDDENPLACE"}), http.StatusOK, "")
	}

	mark := markNotices(t, f.app)
	apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(kept.ID), map[string]any{"status": models.UserActive}, http.StatusOK)
	got := noticesSince(t, f.app, kept.ID, notify.RegistrationNotRestored, mark.notice)
	if len(got) != 1 || got[0].Title != "You're not back in FULLSECRET yet" ||
		got[0].Body != "The event was full when your account was turned back on. Ask a leader or host to add you." ||
		got[0].SubjectID == nil || *got[0].SubjectID != full.ID {
		t.Fatalf("the admin re-enabled as an admin: %+v", got)
	}

	mark = markNotices(t, f.app)
	apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(demoted.ID), map[string]any{"status": models.UserActive, "role": models.RoleMember}, http.StatusOK)
	var back models.User
	gormMust(t, f.app.DB.First(&back, demoted.ID))
	if back.Role != models.RoleMember || back.Status != models.UserActive {
		t.Fatalf("fixture: the edit left %s %s", back.Role, back.Status)
	}
	var rows int64
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("user_id = ?", demoted.ID).Count(&rows))
	if rows != 0 {
		t.Errorf("%d of the demoted person's answers on the archived group's dates came back", rows)
	}
	got = noticesSince(t, f.app, demoted.ID, notify.RegistrationNotRestored, mark.notice)
	if len(got) != 2 {
		t.Fatalf("the person re-enabled as a member got %d not-restored notices: %+v", len(got), got)
	}
	client := loginAPI(t, f.app, demoted.Name, "password1234")
	for i, n := range got {
		if n.Title != "You're not back in one of your events yet" ||
			n.Body != "Your sign-up couldn't come back when your account was turned back on. Ask a leader or host to add you." {
			t.Fatalf("the person re-enabled as a member was told %q / %q", n.Title, n.Body)
		}
		wantWithheld(t, f.app, "the person re-enabled as a member", n, client, []uint{roomy.ID, full.ID}[i])
	}
	noSecrets(t, f.app, "the person re-enabled as a member", demoted.ID, demoted.Email, mark,
		[]string{"ROOMYSECRET", "FULLSECRET", "HIDDENPLACE", "Supper", "Group hall", "/events/", "/groups/"})
}
