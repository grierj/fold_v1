package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/mail"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// suppressionFixtureUsers makes one account per name whose email is
// name@example.com.
func suppressionFixtureUsers(t *testing.T, app *App, names ...string) map[string]*models.User {
	t.Helper()
	out := map[string]*models.User{}
	for _, name := range names {
		out[name] = newTestUser(t, app, name, "password1234", models.RoleMember)
	}
	return out
}

// Preview and snapshot skip labels (recipientFacts) are the same for a hashed
// row and a legacy NULL-hash row, a cleared row blocks nothing, and any open
// complaint gives "marked our email as spam" whichever reason was written
// first.
func TestRecipientFactsReadHashedAndLegacySuppressions(t *testing.T) {
	app := testApp(t)
	users := suppressionFixtureUsers(t, app, "hashed", "legacy", "cleared", "complaintfirst", "bouncefirst",
		"legacycomplaintfirst", "legacybouncefirst", "complaintonly", "clean")
	// An account stored with capitals and padding still matches.
	app.DB.Model(users["hashed"]).Update("email", " Hashed@Example.COM ")
	suppress(t, app, "hashed@example.com", models.SuppressHardBounce)
	suppress(t, app, "legacy@example.com", models.SuppressHardBounce)
	now := time.Now()
	if err := app.DB.Create(&models.Suppression{Channel: "email", AddressHash: mail.AddressHash("cleared@example.com"),
		Reason: models.SuppressHardBounce, ClearedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	suppress(t, app, "complaintfirst@example.com", models.SuppressComplaint)
	suppress(t, app, "complaintfirst@example.com", models.SuppressHardBounce)
	suppress(t, app, "bouncefirst@example.com", models.SuppressHardBounce)
	suppress(t, app, "bouncefirst@example.com", models.SuppressComplaint)
	suppress(t, app, "legacycomplaintfirst@example.com", models.SuppressComplaint)
	suppress(t, app, "legacycomplaintfirst@example.com", models.SuppressHardBounce)
	suppress(t, app, "legacybouncefirst@example.com", models.SuppressHardBounce)
	suppress(t, app, "legacybouncefirst@example.com", models.SuppressComplaint)
	suppress(t, app, "complaintonly@example.com", models.SuppressComplaint)

	var ids []uint
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	facts, err := recipientFacts(app.DB, ids, notify.CatGroups)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range facts {
		got[f.User.Name] = f.Skip
	}
	want := map[string]string{
		"hashed": skipNotWorking, "legacy": skipNotWorking, "cleared": "", "clean": "",
		"complaintfirst": skipNotWorking, "bouncefirst": skipNotWorking,
		"legacycomplaintfirst": skipNotWorking, "legacybouncefirst": skipNotWorking,
		"complaintonly": skipComplained,
	}
	for name, skip := range want {
		if g, ok := got[name]; !ok || g != skip {
			t.Errorf("%s: skip = %q (present %v), want %q", name, g, ok, skip)
		}
	}
}

// suppressionDump renders every column of every row with the pointer fields
// (ClearedAt, ClearedBy) shown by value. Printing the structs with %+v shows
// pointer addresses, which differ between two reads of equal rows.
func suppressionDump(rows []models.Suppression) string {
	var out string
	for _, r := range rows {
		cleared, by := "<nil>", "<nil>"
		if r.ClearedAt != nil {
			cleared = r.ClearedAt.UTC().Format(time.RFC3339Nano)
		}
		if r.ClearedBy != nil {
			by = fmt.Sprint(*r.ClearedBy)
		}
		out += fmt.Sprintf("{ID:%d Channel:%q AddressHash:%s Reason:%q Provider:%q Detail:%q CreatedAt:%s ClearedAt:%s ClearedBy:%s}\n",
			r.ID, r.Channel, r.AddressHash, r.Reason, r.Provider, r.Detail,
			r.CreatedAt.UTC().Format(time.RFC3339Nano), cleared, by)
	}
	return out
}

// A suppression stays on the address: a later account with the same address
// in another case or padding is blocked, and erasing an account leaves every
// suppression row byte-identical.
func TestSuppressionsSurviveAddressReuseAndErasure(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, "admin", "password1234")
	old := newTestUser(t, app, "pat", "password1234", models.RoleMember)
	suppress(t, app, "pat@example.com", models.SuppressHardBounce)
	suppress(t, app, "sam@example.com", models.SuppressHardBounce)

	var before []models.Suppression
	app.DB.Order("id").Find(&before)
	if w := admin.do("DELETE", "/api/v1/admin/users/"+itoa(old.ID), nil, true); w.Code != 200 && w.Code != 204 {
		t.Fatalf("delete user: %d %s", w.Code, w.Body.String())
	}
	var after []models.Suppression
	app.DB.Order("id").Find(&after)
	if b, a := suppressionDump(before), suppressionDump(after); b != a || len(after) != 2 {
		t.Fatalf("erasure changed suppressions:\nbefore %s\nafter  %s", b, a)
	}
	// Someone signs up later with the same address in another case and padding.
	next := &models.User{Email: " PAT@Example.com ", Role: models.RoleMember, Name: "pat again", Status: models.UserActive}
	if err := app.DB.Create(next).Error; err != nil {
		t.Fatal(err)
	}
	rows, _, err := readEmailRecipients(app.DB, []uint{next.ID}, true)
	if err != nil || len(rows) != 1 || rows[0].EmailSuppressions != models.SuppressHardBounce {
		t.Fatalf("reused address: %+v, %v", rows, err)
	}
	facts, err := recipientFacts(app.DB, []uint{next.ID}, notify.CatGroups)
	if err != nil || len(facts) != 1 || facts[0].Skip != skipNotWorking {
		t.Fatalf("reused address facts: %+v, %v", facts, err)
	}
}

// snapshotSkip is the label preview and the send snapshot give someone with
// these rows (recipientFacts): any open row but a complaint gives "email not
// working" (it stops all mail), a complaint alone "marked our email as spam",
// cleared rows nothing.
func (c suppressionCase) snapshotSkip() string {
	skip := ""
	for _, sp := range c.specs {
		switch {
		case sp.cleared:
		case sp.reason == models.SuppressComplaint:
			if skip == "" {
				skip = skipComplained
			}
		default:
			skip = skipNotWorking
		}
	}
	return skip
}

// sendTimeSkip is the label announcement send gives when the rows appear after
// the snapshot: the reason optional mail is refused with, the strongest open
// one (mail.StrongestReason), whichever row is older.
func (c suppressionCase) sendTimeSkip() string {
	switch c.optional {
	case "":
		return ""
	case models.SuppressComplaint:
		return skipComplained
	}
	return skipNotWorking
}

// addSuppressionMembers adds one active member to the fixture's group per
// case, named sup00, sup01 and so on, and returns them by case name. It
// writes no suppression rows.
func (f composeFixture) addSuppressionMembers(t *testing.T, cases []suppressionCase) map[string]*models.User {
	t.Helper()
	out := map[string]*models.User{}
	for i, tc := range cases {
		u := newTestUser(t, f.app, fmt.Sprintf("sup%02d", i), "password1234", models.RoleMember)
		addMember(t, f.app, u, f.group, models.MembershipMember)
		out[tc.name] = u
	}
	return out
}

// TestEventAudienceLeavesOutADateTheyCannotSee: a departed member kept an
// invite-only date. Moving it later was refused while it was kept (DEC-156);
// it reached its old end, and the same guarded edit then ended the grant and
// moved the date into the future, leaving the row Going (A3-late). An announcement to that date's people is the author's own words
// about it, so it must not reach the departed person by any path, while an
// ordinary member of the group (the positive control) gets it every time:
// the composer's preview, preparation, a row an earlier preparation queued,
// Try again's recheck, and the read page.
func TestEventAudienceLeavesOutADateTheyCannotSee(t *testing.T) {
	const sentinel = "Literal sentinel: meet at the moved place"
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	member := goingMember(t, f, target.ID)
	moved := target.StartsAt.AddDate(0, 0, 30)
	changes := map[string]any{"startsLocal": moved.UTC().Format("2006-01-02T15:04"), "location": "Moved secret place"}
	// Before the old end the move is refused while the date is kept
	// (DEC-156); the manager skips Remove and saves again at the old end,
	// where the edit's own before pass ends the grant.
	clock := newMutableClock(f.app, target.EndsAt.Add(-time.Hour))
	base := editBaseOf(t, f.admin, f.event.ID, date)
	wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(base.Revision, changes), http.StatusConflict, "date_kept_after_leaving")
	clock.set(target.EndsAt)
	wantDateEdit(t, f.admin, f.event.ID, date, dateEditBody(base.Revision, changes), http.StatusOK, "")
	f.app.clock = nil
	var kept models.Registration
	gormMust(t, f.app.DB.Where("occurrence_id = ? AND user_id = ?", target.ID, f.recipient.ID).First(&kept))
	if kept.Status != models.RegGoing || kept.AccessEndedAt == nil {
		t.Fatalf("the fixture needs a Going row whose grant ended: %+v", kept)
	}
	aud := map[string]any{"type": policy.AudienceOccurrence, "occurrenceId": target.ID}

	// The preview counts the member alone.
	w := f.admin.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": aud}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var preview audienceSummaryResponseJSON
	decodeJSON(t, w, &preview)
	if preview.Summary.People != 1 || len(preview.Summary.Recipients) != 1 || preview.Summary.Recipients[0].Name != member.PublicName() {
		t.Fatalf("preview: %+v", preview.Summary)
	}

	// Preparation reaches the member and makes no row for the departed person.
	sender := &deliverySender{}
	f.app.Mailer = sender
	var draft struct {
		Message messageJSON `json:"message"`
	}
	w = f.admin.do("POST", "/api/v1/messages", map[string]any{"subject": "Date news", "bodyHtml": "<p>" + sentinel + "</p>", "audience": aud}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("draft: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &draft)
	if w := f.admin.do("POST", "/api/v1/messages/"+itoa(draft.Message.ID)+"/schedule", confirmedSend(t, f.admin, "/api/v1/messages/"+itoa(draft.Message.ID), map[string]any{}), true); w.Code != http.StatusOK {
		t.Fatalf("schedule: %d %s", w.Code, w.Body.String())
	}
	runMessageJobs(t, f.app)
	wantReachedOnly := func(label string, s *deliverySender) {
		t.Helper()
		got := map[string]bool{}
		for _, m := range s.msgs {
			got[m.To] = true
			if m.To == f.recipient.Email {
				t.Fatalf("%s: the departed person was sent %q", label, m.Subject)
			}
		}
		if !got[member.Email] {
			t.Fatalf("%s: the member got nothing: %+v", label, s.msgs)
		}
	}
	wantReachedOnly("preparation", sender)
	var departedRows int64
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ?", draft.Message.ID, f.recipient.ID).Count(&departedRows))
	if departedRows != 0 {
		t.Fatalf("preparation made %d rows for the departed person", departedRows)
	}

	// A row queued before the date moved is decided again when it goes.
	id, ids := seedSending(t, f.app, f.issuer, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: target.ID}, toRow(f.recipient), toRow(member))
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Updates(map[string]any{"subject": "Queued news", "body_html": "<p>" + sentinel + "</p>", "body_text": sentinel}))
	queued := &deliverySender{}
	if err := deliverRows(f.app, queued, id, ids); err != nil {
		t.Fatal(err)
	}
	wantReachedOnly("queued delivery", queued)
	var held models.MessageRecipient
	gormMust(t, f.app.DB.First(&held, ids[0]))
	if held.Status != models.RecipientSuppressed || held.Reason != skipCantSeeDate {
		t.Fatalf("the departed person's queued row: %+v", held)
	}

	// The read page: a suppressed row, and no row, read like no message.
	for _, msg := range []uint{draft.Message.ID, id} {
		if w := f.client.do("GET", "/api/v1/messages/"+itoa(msg)+"/read", nil, false); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), sentinel) {
			t.Fatalf("the departed person read message %d: %d %s", msg, w.Code, w.Body.String())
		}
		memberClient := loginAPI(t, f.app, member.Name, "password1234")
		if w := memberClient.do("GET", "/api/v1/messages/"+itoa(msg)+"/read", nil, false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), sentinel) {
			t.Fatalf("the member could not read message %d: %d %s", msg, w.Code, w.Body.String())
		}
	}
	// A queued row whose person can no longer see the date entitles nothing.
	id2, ids2 := seedSending(t, f.app, f.issuer, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: target.ID}, toRow(f.recipient))
	if w := f.client.do("GET", "/api/v1/messages/"+itoa(id2)+"/read", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("a queued row opened the read page: %d %s", w.Code, w.Body.String())
	}

	// Try again drops the departed person's failed row, in the batch's words.
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id IN ?", ids2).Updates(map[string]any{"status": models.RecipientFailed}))
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id2).Update("status", models.MessageSent))
	if w := f.admin.do("POST", "/api/v1/messages/"+itoa(id2)+"/retry", nil, true); w.Code != http.StatusOK {
		t.Fatalf("Try again: %d %s", w.Code, w.Body.String())
	}
	var again []uint
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND status = ?", id2, models.RecipientQueued).Pluck("id", &again))
	var retried models.MessageRecipient
	gormMust(t, f.app.DB.First(&retried, ids2[0]))
	if len(again) != 0 || retried.Status != models.RecipientSuppressed || retried.Reason != skipCantSeeDate {
		t.Fatalf("Try again kept the departed person's row: %v %+v", again, retried)
	}
}

// TestReadPageKeepsDecidedRowsAndChecksUndecidedOnes: for an event audience a
// row the send decided with the person still in the audience (here skipped
// at the send for an email reason, with the check's mark) keeps letting them
// read the message in Fold after they change their answer (DEC-135); a row
// still queued, skipped as no longer on the list, or skipped before the mark
// existed lets them read only while a date they may see puts them in it.
func TestReadPageKeepsDecidedRowsAndChecksUndecidedOnes(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	read := func(id uint) int {
		return f.readerC.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false).Code
	}
	optedOut, rows := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
	gormMust(t, f.app.DB.Create(&models.Consent{UserID: f.reader.ID, Channel: models.ConsentEmail, Scope: categoryScope(notify.CatMessages),
		Status: models.ConsentOptedOut, Source: models.ConsentSourceWebForm}))
	if err := deliverRows(f.app, &deliverySender{}, optedOut, rows); err != nil {
		t.Fatal(err)
	}
	legacy, legacyRows := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", legacyRows[0]).Updates(map[string]any{"status": models.RecipientSuppressed, "reason": skipOptedOut}))
	queued, _ := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
	offList, offRows := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", offRows[0]).Updates(map[string]any{"status": models.RecipientSuppressed, "reason": skipNoLongerOnList}))
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id IN ?", []uint{optedOut, legacy}).Update("status", models.MessageSending))
	for _, id := range []uint{optedOut, legacy, queued, offList} {
		if code := read(id); code != http.StatusOK {
			t.Fatalf("still Going, message %d: %d", id, code)
		}
	}
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("user_id = ?", f.reader.ID).Update("status", models.RegDeclined))
	if code := read(optedOut); code != http.StatusOK {
		t.Fatalf("a decided row stopped opening the message: %d", code)
	}
	for _, id := range []uint{legacy, queued, offList} {
		if code := read(id); code != http.StatusNotFound {
			t.Fatalf("an undecided row opened message %d after the reader declined: %d", id, code)
		}
	}
	gormMust(t, f.app.DB.Create(&models.Consent{UserID: f.reader.ID, Channel: models.ConsentEmail, Scope: categoryScope(notify.CatMessages),
		Status: models.ConsentOptedIn, Source: models.ConsentSourceWebForm}))
	// Sending the queued row now: someone who only changed their answer is
	// no longer on the list, not shut out of the date.
	var queuedRow models.MessageRecipient
	gormMust(t, f.app.DB.Where("message_id = ?", queued).First(&queuedRow))
	s := &deliverySender{}
	if err := deliverRows(f.app, s, queued, []uint{queuedRow.ID}); err != nil {
		t.Fatal(err)
	}
	gormMust(t, f.app.DB.First(&queuedRow, queuedRow.ID))
	if len(s.msgs) != 0 || queuedRow.Status != models.RecipientSuppressed || queuedRow.Reason != skipNoLongerOnList {
		t.Fatalf("the declined reader's queued row: %+v, sent %d", queuedRow, len(s.msgs))
	}
}

// TestAudiencePreviewNeverStraddlesAnEdit: the composer's preview names who
// an event audience reaches from one snapshot. The departed person's kept
// date reaches its old end and the real guarded edit ends the grant and moves
// the date in one commit, at the "grants" seam of the preview's decision.
// Before the edit the grant had lapsed and after it the grant is stamped, so
// no committed state admits them: the preview must not name them, while the
// ordinary member (the positive control) is named. A decision read over
// separate statements would pair the live grant with the moved date and name
// the departed person, which no later delivery check could take back.
func TestAudiencePreviewNeverStraddlesAnEdit(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	date := models.FormatCalendarDate(target.OriginalDate)
	member := goingMember(t, f, target.ID)
	base := editBaseOf(t, f.admin, f.event.ID, date)
	newMutableClock(f.app, target.EndsAt)
	var fired atomic.Bool
	f.app.hooks.invitationReadStep = func(step string) {
		if step != "grants" || !fired.CompareAndSwap(false, true) {
			return
		}
		w := postDateEdit(f.admin, f.event.ID, date, dateEditBody(base.Revision,
			map[string]any{"startsLocal": target.StartsAt.Add(2 * time.Hour).UTC().Format("2006-01-02T15:04"), "title": "Moved secret title"}))
		if w.Code != http.StatusOK {
			t.Errorf("the edit at the seam: %d %s", w.Code, w.Body.String())
		}
	}
	aud := map[string]any{"type": policy.AudienceOccurrence, "occurrenceId": target.ID}
	w := f.admin.do("POST", "/api/v1/messages/audience-preview", map[string]any{"audience": aud}, true)
	if !fired.Load() {
		t.Fatal("the preview never reached the grants step: no straddle was tried")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var preview audienceSummaryResponseJSON
	decodeJSON(t, w, &preview)
	if preview.Summary.People != 1 || len(preview.Summary.Recipients) != 1 || preview.Summary.Recipients[0].Name != member.PublicName() {
		t.Fatalf("the preview named someone no committed state admits: %+v", preview.Summary)
	}
}

// TestPreparationDecidesItsAudienceInOneSnapshot: message.send reads the
// author's rights, the audience's targets and the dates and grants that put
// each registrant in it through its one snapshot. On a one-connection pool a
// read outside that snapshot while it is open would wait for the connection
// it holds and fail; the departed person's kept date admits them, the member
// is the control, and both get a queued row. Its data never changes, so it
// proves no second connection, not isolation:
// TestPreparationChoosesItsRowsInTheAudiencesSnapshot commits at the seam.
func TestPreparationDecidesItsAudienceInOneSnapshot(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	target := f.dates[1]
	member := goingMember(t, f, target.ID)
	raw, _ := json.Marshal(audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: target.ID})
	at := time.Now().UTC().Add(-time.Second)
	m := models.Message{AuthorID: &f.issuer.ID, Status: models.MessageScheduled, Category: models.MessageCategoryAnnouncement,
		Subject: "Date news", BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: string(raw), ScheduledAt: &at}
	gormMust(t, f.app.DB.Create(&m))
	one := oneConnectionPool(t, f.app)
	ctx := oneConnection(t, f.app, one)
	if err := f.app.prepareMessageOnce(ctx, m.ID); err != nil {
		t.Fatalf("on one connection: %v", err)
	}
	f.app.DB = one
	var got []uint
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND channel = ? AND status = ?", m.ID, models.DeliveryEmail, models.RecipientQueued).
		Order("user_id").Pluck("user_id", &got))
	if len(got) != 2 || !slices.Contains(got, f.recipient.ID) || !slices.Contains(got, member.ID) {
		t.Fatalf("queued rows for %v, want the departed person and the member", got)
	}
}

// TestPreparationChoosesItsRowsInTheAudiencesSnapshot: message.send decides
// the audience and builds every row it will write (each person's email facts,
// the guardian copies and push choices) in one snapshot. Right after the
// audience is decided ("preparation"), the teen leaves the audience (their
// membership ends, or they decline their only date in a date, series or
// two-event audience) and a guardian link is
// confirmed, in one commit. Before that commit the teen was in the audience
// with no guardian; after it they are out. No committed state gives the teen a
// row and the new guardian a copy, which a link read after the snapshot would.
// The control confirms the link before preparation and gets the copy.
func TestPreparationChoosesItsRowsInTheAudiencesSnapshot(t *testing.T) {
	for _, audience := range []string{"group", "date", "series", "events"} {
		for _, straddle := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s straddle=%v", audience, straddle), func(t *testing.T) {
				f := newComposeFixture(t)
				gormMust(t, f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
				guardian := newTestUser(t, f.app, "guardian", "password1234", models.RoleMember)
				au := audienceJSON{Type: policy.AudienceGroup, GroupID: f.group.ID}
				leave := func(tx *gorm.DB) error {
					return tx.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.group.ID, f.teen.ID).Update("status", models.MembershipRemoved).Error
				}
				if audience != "group" {
					switch audience {
					case "date":
						au = audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: f.groupOcc.ID}
					case "series":
						au = audienceJSON{Type: policy.AudienceEventSeries, EventID: f.groupEvent.ID}
					case "events":
						second := createFeedEvent(t, f.leader, f.group.ID, "Second night", feedStart(5), "")
						au = audienceJSON{Type: policy.AudienceEvents, EventIDs: []uint{f.groupEvent.ID, second.ID}}
					}
					gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.teen.ID, OccurrenceID: f.groupOcc.ID, Status: models.RegGoing}))
					leave = func(tx *gorm.DB) error {
						return tx.Model(&models.Registration{}).Where("occurrence_id = ? AND user_id = ?", f.groupOcc.ID, f.teen.ID).Update("status", models.RegDeclined).Error
					}
				}
				link := func(tx *gorm.DB) error {
					return tx.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &f.teen.ID, Status: models.GuardianConfirmed}).Error
				}
				var fired atomic.Bool
				if straddle {
					f.app.hooks.readerAuthorized = func(surface string) {
						if surface != "preparation" || !fired.CompareAndSwap(false, true) {
							return
						}
						if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
							if err := leave(tx); err != nil {
								return err
							}
							return link(tx)
						}); err != nil {
							t.Error(err)
						}
					}
				} else {
					if err := f.app.DB.Transaction(link); err != nil {
						t.Fatal(err)
					}
				}
				raw, _ := json.Marshal(au)
				at := time.Now().UTC().Add(-time.Second)
				m := models.Message{AuthorID: &f.leaderUser.ID, Status: models.MessageScheduled, Category: models.MessageCategoryAnnouncement,
					Subject: "News", BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: string(raw), ScheduledAt: &at}
				gormMust(t, f.app.DB.Create(&m))
				if err := f.app.prepareMessageOnce(context.Background(), m.ID); err != nil {
					t.Fatal(err)
				}
				if straddle && !fired.Load() {
					t.Fatal("preparation never reached its seam: no straddle was tried")
				}
				var teenRows, copies int64
				gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND copy_of_user_id IS NULL AND channel = ?", m.ID, f.teen.ID, models.DeliveryEmail).Count(&teenRows))
				gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND copy_of_user_id = ?", m.ID, guardian.ID, f.teen.ID).Count(&copies))
				if teenRows != 1 {
					t.Fatalf("the teen has %d rows; preparation's audience included them", teenRows)
				}
				if straddle {
					if copies != 0 {
						t.Fatalf("the guardian got a copy from a link confirmed after the teen left: %d", copies)
					}
				} else if copies != 1 {
					t.Fatalf("the control's guardian has %d copies, want 1", copies)
				}
			})
		}
	}
}

// TestPreparationSnapshotsAHouseholdHolder: a parent reaches a series
// audience only through the registration they made for their child (no row
// of their own, DEC-8). Right after the audience is decided ("preparation"),
// that registration is declined and, in the same commit, the parent turns
// push on with a device. Before the commit the parent was in the audience
// with push off; after it they are out. The parent gets the email row, and no
// committed state gives them a push row as well. The control turns push on
// before preparation and gets both.
func TestPreparationSnapshotsAHouseholdHolder(t *testing.T) {
	for _, straddle := range []bool{false, true} {
		t.Run(fmt.Sprintf("straddle=%v", straddle), func(t *testing.T) {
			f := newComposeFixture(t)
			parent := newTestUser(t, f.app, "parent", "password1234", models.RoleMember)
			household := models.Household{Name: "Parent family", CreatedBy: parent.ID}
			gormMust(t, f.app.DB.Create(&household))
			child := models.HouseholdPerson{HouseholdID: household.ID, FirstName: "Kid", Kind: models.HouseholdChild, CreatedBy: parent.ID}
			gormMust(t, f.app.DB.Create(&child))
			gormMust(t, f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, HouseholdPersonID: &child.ID, RegisteredBy: &parent.ID, Status: models.RegGoing}))
			pushOn := func(tx *gorm.DB) error {
				if err := tx.Create(&models.Consent{UserID: parent.ID, Channel: models.ConsentPush, Scope: "all", Status: models.ConsentOptedIn, Source: models.ConsentSourceWebForm}).Error; err != nil {
					return err
				}
				return tx.Create(&models.PushSubscription{UserID: parent.ID, Endpoint: "https://fcm.googleapis.com/fcm/send/parent-secret", P256dh: "p256dh", Auth: "auth"}).Error
			}
			var fired atomic.Bool
			if straddle {
				f.app.hooks.readerAuthorized = func(surface string) {
					if surface != "preparation" || !fired.CompareAndSwap(false, true) {
						return
					}
					if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
						if err := tx.Model(&models.Registration{}).Where("household_person_id = ?", child.ID).Update("status", models.RegDeclined).Error; err != nil {
							return err
						}
						return pushOn(tx)
					}); err != nil {
						t.Error(err)
					}
				}
			} else if err := f.app.DB.Transaction(pushOn); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(audienceJSON{Type: policy.AudienceEventSeries, EventID: f.groupEvent.ID})
			at := time.Now().UTC().Add(-time.Second)
			m := models.Message{AuthorID: &f.leaderUser.ID, Status: models.MessageScheduled, Category: models.MessageCategoryAnnouncement,
				Subject: "News", BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: string(raw), ScheduledAt: &at, PushAlert: true}
			gormMust(t, f.app.DB.Create(&m))
			if err := f.app.prepareMessageOnce(context.Background(), m.ID); err != nil {
				t.Fatal(err)
			}
			if straddle && !fired.Load() {
				t.Fatal("preparation never reached its seam: no straddle was tried")
			}
			var email, pushes int64
			gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND channel = ? AND status = ? AND copy_of_user_id IS NULL", m.ID, parent.ID, models.DeliveryEmail, models.RecipientQueued).Count(&email))
			gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND channel = ?", m.ID, parent.ID, models.DeliveryPush).Count(&pushes))
			if email != 1 {
				t.Fatalf("the household holder has %d queued email rows; their child's registration puts them in the audience", email)
			}
			if straddle && pushes != 0 {
				t.Fatalf("the holder got a push row from push turned on after their registration ended: %d", pushes)
			}
			if !straddle && pushes != 1 {
				t.Fatalf("the control's holder has %d push rows, want 1", pushes)
			}
		})
	}
}

// TestPreparationSnapshotsAMixedVisibilitySeries: a departed teen holds two
// dates of an invite-only series: one hidden from them, and one they still
// see only through their kept exact-date grant. The grant is what keeps them
// in the audience. With the grant live, they get a row and their confirmed
// guardian a copy; with it ended before preparation, neither. Ended right
// after the audience is decided ("preparation"), together with a guardian
// link confirmed in the same commit, the teen keeps the row preparation's
// snapshot gave them and no committed state gives the new guardian a copy.
func TestPreparationSnapshotsAMixedVisibilitySeries(t *testing.T) {
	for _, arm := range []string{"grant live", "grant ended before", "grant ended at the seam"} {
		t.Run(arm, func(t *testing.T) {
			f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
			teen := f.recipient
			// A teen keeps a grant only with a known age and current account
			// consent.
			born := time.Now().UTC().AddDate(-15, 0, 0)
			gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", teen.ID).Updates(map[string]any{"account_type": models.AccountTeen, "birth_date": born}))
			parent := newTestUser(t, f.app, "consenting parent", "password1234", models.RoleMember)
			gormMust(t, f.app.DB.Create(&models.TeenConsent{TeenUserID: teen.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: parent.ID, GranterRole: "guardian"}))
			hidden := f.dates[2]
			gormMust(t, f.app.DB.Create(&models.Registration{UserID: &teen.ID, OccurrenceID: hidden.ID, Status: models.RegGoing}))
			gormMust(t, f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
			guardian := newTestUser(t, f.app, "guardian", "password1234", models.RoleMember)
			endGrant := func(tx *gorm.DB) error {
				return tx.Model(&models.Registration{}).Where("id = ?", f.access.RegistrationID).
					Updates(map[string]any{"access_ended_at": time.Now().UTC(), "access_end_reason": "context_changed"}).Error
			}
			link := func(tx *gorm.DB) error {
				return tx.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &teen.ID, Status: models.GuardianConfirmed}).Error
			}
			var fired atomic.Bool
			switch arm {
			case "grant live":
				if err := f.app.DB.Transaction(link); err != nil {
					t.Fatal(err)
				}
			case "grant ended before":
				if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
					if err := endGrant(tx); err != nil {
						return err
					}
					return link(tx)
				}); err != nil {
					t.Fatal(err)
				}
			case "grant ended at the seam":
				f.app.hooks.readerAuthorized = func(surface string) {
					if surface != "preparation" || !fired.CompareAndSwap(false, true) {
						return
					}
					if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
						if err := endGrant(tx); err != nil {
							return err
						}
						return link(tx)
					}); err != nil {
						t.Error(err)
					}
				}
			}
			raw, _ := json.Marshal(audienceJSON{Type: policy.AudienceEventSeries, EventID: f.event.ID})
			at := time.Now().UTC().Add(-time.Second)
			m := models.Message{AuthorID: &f.issuer.ID, Status: models.MessageScheduled, Category: models.MessageCategoryAnnouncement,
				Subject: "Series news", BodyHTML: "<p>Hello</p>", BodyText: "Hello", Audience: string(raw), ScheduledAt: &at}
			gormMust(t, f.app.DB.Create(&m))
			if err := f.app.prepareMessageOnce(context.Background(), m.ID); err != nil {
				t.Fatal(err)
			}
			if arm == "grant ended at the seam" && !fired.Load() {
				t.Fatal("preparation never reached its seam: no straddle was tried")
			}
			var teenRows, copies int64
			gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND copy_of_user_id IS NULL AND channel = ? AND status = ?", m.ID, teen.ID, models.DeliveryEmail, models.RecipientQueued).Count(&teenRows))
			gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND copy_of_user_id = ?", m.ID, guardian.ID, teen.ID).Count(&copies))
			want := map[string][2]int64{"grant live": {1, 1}, "grant ended before": {0, 0}, "grant ended at the seam": {1, 0}}[arm]
			if teenRows != want[0] || copies != want[1] {
				t.Fatalf("%s: the teen has %d queued rows and the guardian %d copies, want %d and %d", arm, teenRows, copies, want[0], want[1])
			}
		})
	}
}
