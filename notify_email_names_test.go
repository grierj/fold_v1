package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"fold/internal/mail"
	"fold/internal/notify"
	"fold/models"

	"gorm.io/gorm"
)

func queueNamedNotice(t *testing.T, app *App, m notify.Msg) models.Notification {
	t.Helper()
	var ids []uint
	if err := app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		ids, err = notify.Send(tx, m)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("%s: queued notices %v, want one", m.Kind, ids)
	}
	var n models.Notification
	gormMust(t, app.DB.First(&n, ids[0]))
	return n
}

func sendNamedNotice(t *testing.T, app *App, n models.Notification, u *models.User) mail.OutboxMessage {
	t.Helper()
	before := mailCount(t, app, u.Email)
	for range 2 {
		if err := app.deliverOneNotification(context.Background(), n.ID); err != nil {
			t.Fatalf("%s: %v", n.Kind, err)
		}
	}
	if d := emailDeliveryOf(t, app, n.ID); d.Status != models.DeliverySent || d.ProviderMessageID == nil {
		t.Fatalf("%s: delivery %+v", n.Kind, d)
	}
	if count := mailCount(t, app, u.Email); count != before+1 {
		t.Fatalf("%s: sent %d times, want one", n.Kind, count-before)
	}
	m, err := mail.Unwrap(app.Mailer).(*mail.DevOutbox).LatestTo(u.Email)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := notify.Lookup(n.Kind)
	if !strings.Contains(m.Body, app.emailLink(n.Link)) || (m.ListUnsubscribe != "") == k.Required {
		t.Errorf("%s: link or unsubscribe missing or unexpected: %+v", n.Kind, m)
	}
	return m
}

func assertNameFreeNotice(t *testing.T, n models.Notification, m mail.OutboxMessage, title string, names ...string) {
	t.Helper()
	if m.Subject != title {
		t.Errorf("%s: subject %q, want %q", n.Kind, m.Subject, title)
	}
	for _, name := range names {
		if strings.Contains(m.Subject+m.Body, name) {
			t.Errorf("%s: email still contains %q", n.Kind, name)
		}
	}
}

func TestNotifyDeliverChecksNamedAccountsAfterErasure(t *testing.T) {
	s := newCoreErasureScene(t)
	actor := newTestUser(t, s.app, "erased-actor", erasurePW, models.RoleMember)
	leader := newTestUser(t, s.app, "erased-leader", erasurePW, models.RoleMember)
	cases := []struct {
		kind, fallback string
		actor          *models.User
		person         *models.User
	}{
		{notify.GroupPending, "A new group is waiting for approval", actor, nil},
		{notify.GroupDeletion, "A request to delete a group is waiting", actor, nil},
		{notify.LeadershipChanged, "Group leadership changed", s.admin, leader},
		{notify.LeadershipChanged, "Group leadership changed", actor, s.admin},
		{notify.MembershipLeft, "A member left your group", actor, actor},
		{notify.VisibilityOverride, "An event was opened up", actor, nil},
		{notify.RoomRequested, "A room request is waiting for approval", actor, nil},
		{notify.RoomChanged, "An event's room plans changed", actor, nil},
	}
	var pending []models.Notification
	for _, tc := range cases {
		vars := notify.Vars{Actor: tc.actor.PublicName(), Group: "Choir", Event: "Practice", Change: "added"}
		data := map[string]any{}
		if tc.person != nil {
			vars.Person = tc.person.PublicName()
			data["userId"] = tc.person.ID
		}
		msg := notify.Msg{Kind: tc.kind, To: []uint{s.second.ID}, Actor: &tc.actor.ID, Link: "/groups/1", Vars: vars, Data: data}
		live := queueNamedNotice(t, s.app, msg)
		m := sendNamedNotice(t, s.app, live, s.second)
		if m.Subject != live.EmailTitle || !strings.Contains(m.Body, live.EmailBody) {
			t.Errorf("%s: live email lost its stored copy: %+v", live.Kind, m)
		}
		pending = append(pending, queueNamedNotice(t, s.app, msg))
	}
	for _, u := range []*models.User{actor, leader} {
		hrErase(t, s, u.ID)
	}
	for i, n := range pending {
		m := sendNamedNotice(t, s.app, n, s.second)
		assertNameFreeNotice(t, n, m, cases[i].fallback, actor.PublicName(), leader.PublicName())
		saved, ok := notify.Get(s.app.DB, s.second.ID, n.ID)
		if !ok || saved.Title != n.Title || saved.Body != n.Body || saved.EmailTitle != n.EmailTitle || saved.EmailBody != n.EmailBody {
			t.Errorf("%s: delivery or erasure changed the stored notice: %+v", n.Kind, saved)
		}
	}
}

func TestNotifyDeliverChecksHouseholdNamesAfterErasure(t *testing.T) {
	s := newCoreErasureScene(t)
	member := newTestUser(t, s.app, "later-converted", erasurePW, models.RoleMember)
	hid := hrHouseholdOf(t, s, "Taylor household", "")
	hrSeat(t, s, hid, s.second.ID, models.HouseholdRelationGuardian)
	personID := hrPerson(t, s, hid, "Original name", models.HouseholdAdult, nil, false)
	person := hrPersonRow(t, s.app.DB, personID)
	g := newTestGroup(t, s.app, s.second, "Choir", models.GroupActive)
	e, o := seedEvent(t, s.app, g, s.second.ID, time.Now().Add(72*time.Hour))
	reg := models.Registration{HouseholdPersonID: &personID, RegisteredBy: &s.second.ID}
	cases := []struct{ kind, fallback string }{
		{notify.RegistrationPromoted, "A waitlist spot opened"},
		{notify.RegistrationRemoved, "A registration was removed"},
		{notify.RegistrationNotRestored, "A registration couldn't be restored"},
	}
	var pending []models.Notification
	for _, tc := range cases {
		msg, err := registrationNoticeIn(s.app.DB, tc.kind, &e, &o, reg, nil)
		if err != nil {
			t.Fatal(err)
		}
		msg.Actor = &s.admin.ID
		msg.Vars.Note = "Note about " + person.FullName()
		msg.Vars.ManageURL = "https://fold.example/guest/manage/held-registration"
		live := queueNamedNotice(t, s.app, msg)
		m := sendNamedNotice(t, s.app, live, s.second)
		if !strings.Contains(m.Subject+m.Body, person.FullName()) || (tc.kind == notify.RegistrationPromoted && !strings.Contains(m.Body, msg.Vars.ManageURL)) {
			t.Errorf("%s: living person's email lost its name or manage link: %+v", tc.kind, m)
		}
		legacy := queueNamedNotice(t, s.app, msg)
		data, err := json.Marshal(msg.Data)
		if err != nil {
			t.Fatal(err)
		}
		gormMust(t, s.app.DB.Model(&legacy).UpdateColumn("data", string(data)))
		m = sendNamedNotice(t, s.app, legacy, s.second)
		assertNameFreeNotice(t, legacy, m, tc.fallback, person.FullName(), msg.Vars.ManageURL)
		pending = append(pending, queueNamedNotice(t, s.app, msg))
	}
	gormMust(t, s.app.DB.Model(&person).UpdateColumn("converted_user_id", member.ID))
	hrErase(t, s, member.ID)
	for i, n := range pending {
		assertNameFreeNotice(t, n, sendNamedNotice(t, s.app, n, s.second), cases[i].fallback, person.FullName(), "held-registration")
		var saved models.Notification
		gormMust(t, s.app.DB.First(&saved, n.ID))
		if !strings.Contains(saved.EmailTitle+saved.EmailBody, person.FullName()) || strings.Contains(saved.Data, person.FullName()) {
			t.Errorf("%s: unexpected stored text or data after erasure: %+v", n.Kind, saved)
		}
	}
}

func TestNotifyDeliverRechecksNamesOnAdminRetry(t *testing.T) {
	s := newCoreErasureScene(t)
	actor := newTestUser(t, s.app, "retry-named-person", erasurePW, models.RoleMember)
	n := queueNamedNotice(t, s.app, notify.Msg{Kind: notify.MembershipLeft, To: []uint{s.second.ID}, Actor: &actor.ID,
		Vars: notify.Vars{Person: actor.PublicName(), Group: "Choir"}, Link: "/groups/1"})
	var job models.Job
	gormMust(t, s.app.DB.Where("unique_key = ?", notify.DeliverKey(n.ID)).First(&job))
	gormMust(t, s.app.DB.Model(&job).UpdateColumn("max_attempts", 1))
	mailer := s.app.Mailer
	failing := &notifyFailSender{err: errors.New("provider unavailable")}
	s.app.Mailer = failing
	runNotifyOnce(t, s.app)
	if failing.n() != 1 || emailDeliveryOf(t, s.app, n.ID).Status != models.DeliveryDeferred || reloadJob(t, s.app, job.ID).Status != models.JobDead {
		t.Fatal("first send did not leave a dead job and deferred delivery")
	}
	s.app.Mailer = mailer
	hrErase(t, s, actor.ID)
	if r := s.ac.do(http.MethodPost, fmt.Sprintf("/api/v1/admin/jobs/%d/retry", job.ID), nil, true); r.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", r.Code, r.Body)
	}
	for range 8 {
		if reloadJob(t, s.app, job.ID).Status == models.JobDone {
			break
		}
		runNotifyOnce(t, s.app)
	}
	if d := emailDeliveryOf(t, s.app, n.ID); d.Status != models.DeliverySent {
		t.Fatalf("retry delivery %+v", d)
	}
	if j := reloadJob(t, s.app, job.ID); j.Status != models.JobDone {
		t.Fatalf("retry job %+v", j)
	}
	m, err := mail.Unwrap(s.app.Mailer).(*mail.DevOutbox).LatestTo(s.second.Email)
	if err != nil {
		t.Fatal(err)
	}
	assertNameFreeNotice(t, n, m, "A member left your group", actor.PublicName())
}

func TestNoticeGuardianCopyRechecksNamesAfterTheDirectSend(t *testing.T) {
	f := newCopyFixture(t)
	admin := newTestUser(t, f.app, "erasure-admin", erasurePW, models.RoleAdmin)
	s := coreErasureScene{app: f.app, admin: admin, ac: loginAPI(t, f.app, "erasure-admin", erasurePW)}
	member := newTestUser(t, f.app, "converted-member", erasurePW, models.RoleMember)
	hid := hrHouseholdOf(t, s, "Taylor household", "")
	personID := hrPerson(t, s, hid, "Original name", models.HouseholdAdult, nil, false)
	person := hrPersonRow(t, f.app.DB, personID)
	const manage = "https://fold.example/guest/manage/held-registration"
	n := queueNamedNotice(t, f.app, notify.Msg{
		Kind: notify.RegistrationPromoted, To: []uint{f.teen.ID}, SubjectType: "occurrence", SubjectID: f.occ.ID,
		HouseholdPersonID: &personID, Link: "/events/1",
		Vars: notify.Vars{Person: person.FullName(), Event: f.event.Title, ManageURL: manage},
	})
	direct := sendNamedNotice(t, f.app, n, f.teen)
	if !strings.Contains(direct.Subject, person.FullName()) || !strings.Contains(direct.Body, manage) {
		t.Fatalf("direct email lost its name or manage link: %+v", direct)
	}
	gormMust(t, f.app.DB.Model(&person).UpdateColumn("converted_user_id", member.ID))
	hrErase(t, s, member.ID)
	for _, g := range f.guardians {
		copy := latestEmail(t, f.app, g.Email)
		assertNameFreeNotice(t, n, copy, "Copy: A waitlist spot opened", person.FullName(), manage, f.app.emailLink(n.Link))
		if copy.ListUnsubscribe == "" || !strings.Contains(copy.Body, "Copy: sent to "+f.teen.PublicName()) {
			t.Errorf("guardian email lost its marker or unsubscribe: %+v", copy)
		}
	}
}
