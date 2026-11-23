package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/content"
	"fold/internal/mail"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/internal/providers"
	"fold/models"

	"gorm.io/gorm"
)

// An announcement's event fields are filled when each email row or push goes
// out, by what its reader may see of the event or date then
// (readerEventValues, message_event_visibility.go). These tests seed a
// message that is still sending with queued rows (what preparation leaves),
// change the event or the reader, and run the real batch or push job, so the
// fake provider tells a current title from the one they were prepared with.

// deliverySender is the email provider: it records each message and can fail
// the first few sends or run a step after each one.
type deliverySender struct {
	mu    sync.Mutex
	msgs  []mail.Message
	fail  int
	after func(n int)
}

func (s *deliverySender) SendEmail(_ context.Context, m mail.Message) (string, error) {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	n := len(s.msgs)
	s.mu.Unlock()
	if s.after != nil {
		s.after(n)
	}
	if n <= s.fail {
		return "", &providers.SendError{Provider: "fake", Code: 503, Message: "service unavailable"}
	}
	return "fake-" + itoa(uint(n)), nil
}

func (s *deliverySender) Capabilities() providers.EmailCaps { return providers.EmailCaps{Name: "fake"} }

// to is the last message sent to addr.
func (s *deliverySender) to(t *testing.T, addr string) mail.Message {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.msgs) - 1; i >= 0; i-- {
		if s.msgs[i].To == addr {
			return s.msgs[i]
		}
	}
	t.Fatalf("nothing was sent to %s (%d sent)", addr, len(s.msgs))
	return mail.Message{}
}

func (s *deliverySender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// deliveryBodyText puts each event field on its own place in the plain text,
// so a field that is filled is told from one that is blank.
const deliveryBodyText = "{{event_title}} at {{event_time}}. {{event_link}} {{manage_link}}"

func toRow(u *models.User) models.MessageRecipient {
	return models.MessageRecipient{UserID: u.ID, Address: u.Email}
}

// seedSending is a message that is going out, by author, with a queued email
// row for each of rows, as preparation leaves them. It returns the message and
// its rows' ids.
func seedSending(t *testing.T, app *App, author *models.User, au audienceJSON, rows ...models.MessageRecipient) (uint, []uint) {
	t.Helper()
	raw, err := json.Marshal(au)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	m := models.Message{AuthorID: &author.ID, Status: models.MessageSending, Subject: readVisSubject,
		BodyHTML: content.SanitizeHTML(readVisBody), BodyText: deliveryBodyText, Audience: string(raw), ScheduledAt: &now}
	if err := app.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	ids := []uint{}
	for i := range rows {
		rows[i].MessageID, rows[i].Channel, rows[i].Status = m.ID, models.DeliveryEmail, models.RecipientQueued
		if err := app.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rows[i].ID)
	}
	return m.ID, ids
}

func deliverRows(app *App, s *deliverySender, id uint, ids []uint) error {
	app.Mailer = s
	return app.deliverMessageBatchOnce(context.Background(), messageBatchJob{MessageID: id, RecipientIDs: ids})
}

// checkDelivered asserts what one email said: the subject and the plain-text
// line holding each event field exactly as w gives them (readWant: empty is
// "nothing"), and, for a blank, that none of forbid reached any part of it.
// copy is a guardian copy, whose subject and text carry the copy note.
func checkDelivered(t *testing.T, label string, msg mail.Message, first string, w readWant, copy bool, forbid ...string) {
	t.Helper()
	subject := w.title + " news for " + first
	body := msg.Text
	if copy {
		subject = "Copy: " + subject
		i := strings.Index(body, "\n\n")
		if i < 0 {
			t.Fatalf("%s: no copy note in %q", label, msg.Text)
		}
		body = body[i+2:]
	}
	if want := strings.TrimSpace(subject); msg.Subject != want {
		t.Errorf("%s: subject %q, want %q", label, msg.Subject, want)
	}
	if want := w.title + " at " + w.when + ". " + w.event + " " + w.manage; !strings.HasPrefix(body, want) {
		t.Errorf("%s: text %q, want it to start %q", label, body, want)
	}
	if w == (readWant{}) {
		for _, bad := range forbid {
			for part, s := range map[string]string{"subject": msg.Subject, "text": msg.Text, "html": msg.HTML} {
				if strings.Contains(s, bad) {
					t.Errorf("%s: %q reached the %s", label, bad, part)
				}
			}
		}
	}
}

// blankForbid is everything a reader who may see nothing of f's event must
// never be sent: the title it had, the one it is renamed to, the date's own
// title, the event's page (which every date link starts with) and the time.
func blankForbid(f readVisFixture) []string {
	return []string{"Vis night", "Renamed night", "Special night", f.eventURL(), f.when(f.occs[0])}
}

type liveCase struct {
	name     string
	vis      string
	member   bool
	change   func(t *testing.T, f readVisFixture)
	date, sr func(f readVisFixture) readWant
}

func liveSeen(f readVisFixture, title string) readWant {
	return readWant{title: title, when: f.when(f.occs[0]), event: f.eventURL(), manage: f.dateURL(f.occs[0])}
}

func seriesOnly(f readVisFixture) readWant {
	return readWant{title: "Vis night", event: f.eventURL(), manage: f.eventURL()}
}

func notSent(readVisFixture) readWant { return readWant{notSent: true} }

func offList(readVisFixture) readWant { return readWant{notSent: true, offList: true} }

// wantNotSent: the row of an event audience whose person no longer has a date
// they may see in it went nowhere and is suppressed with why.
func wantNotSent(t *testing.T, label string, app *App, s *deliverySender, to string, rowID uint, why string) {
	t.Helper()
	for _, m := range s.msgs {
		if m.To == to {
			t.Fatalf("%s: an email went to %s: %q", label, to, m.Subject)
		}
	}
	var row models.MessageRecipient
	gormMust(t, app.DB.First(&row, rowID))
	if row.Status != models.RecipientSuppressed || row.Reason != why {
		t.Fatalf("%s: row %+v, want suppressed %q", label, row, why)
	}
}

// A change after preparation (a rename, a tightened event or date, a lost
// membership, a cancelled or deleted event) reaches the email: the reader
// gets what the read page would give them now, per field, never the event as
// it was when the row was queued, and no email at all once no date they may
// still see puts them in the audience.
func TestEmailDeliveryUsesTheEventAsTheReaderMaySeeItNow(t *testing.T) {
	cases := []liveCase{
		{"a rename", models.VisibilityPublic, false,
			func(t *testing.T, f readVisFixture) { f.rename(t, "Renamed night") },
			func(f readVisFixture) readWant { return liveSeen(f, "Renamed night") },
			func(f readVisFixture) readWant { return liveSeen(f, "Renamed night") }},
		{"an event tightened to invite only", models.VisibilityPublic, false,
			func(t *testing.T, f readVisFixture) {
				f.rename(t, "Renamed night")
				if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
					t.Fatal(err)
				}
			}, notSent, notSent},
		{"a member who left", models.VisibilityInviteOnly, true,
			func(t *testing.T, f readVisFixture) {
				f.setMembership(t, f.reader, models.MembershipRemoved)
				f.rename(t, "Renamed night")
			}, notSent, notSent},
		// The reader's one date is closed to them, so neither audience has a
		// date they may see.
		{"one date closed by its override", models.VisibilityPublic, false,
			func(t *testing.T, f readVisFixture) {
				f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly)
			},
			notSent, notSent},
		// A Going row left on a cancelled date (a real cancellation cancels it)
		// still names the one-date audience's date, which the reader may see;
		// a series audience counts scheduled dates only, so it no longer names
		// the reader.
		{"a cancelled date", models.VisibilityPublic, false,
			func(t *testing.T, f readVisFixture) {
				f.setDate(t, f.occs[0], "title_override", "Special night")
				f.setDate(t, f.occs[0], "status", models.OccurrenceCancelled)
			},
			func(f readVisFixture) readWant { return seriesOnly(f) }, offList},
		{"a deleted event", models.VisibilityPublic, false,
			func(t *testing.T, f readVisFixture) {
				if err := f.app.DB.Exec("UPDATE events SET deleted_at = now() WHERE id = ?", f.ev.ID).Error; err != nil {
					t.Fatal(err)
				}
			}, notSent, notSent},
		{"a date with its own title", models.VisibilityPublic, false,
			func(t *testing.T, f readVisFixture) {
				f.setDate(t, f.occs[0], "title_override", "Special night")
				f.rename(t, "Renamed night")
			},
			func(f readVisFixture) readWant { return liveSeen(f, "Special night") },
			func(f readVisFixture) readWant { return liveSeen(f, "Renamed night") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadVisFixture(t, tc.vis)
			if tc.member {
				addMember(t, f.app, f.reader, &f.group, models.MembershipMember)
			}
			f.register(t, f.reader, f.occs[0])
			dateMsg, dateRows := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
			seriesMsg, seriesRows := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.reader))
			tc.change(t, f)
			first := firstName(f.reader.PublicName())
			for _, run := range []struct {
				label string
				id    uint
				ids   []uint
				want  readWant
			}{
				{"a one-date audience", dateMsg, dateRows, tc.date(f)},
				{"a series audience", seriesMsg, seriesRows, tc.sr(f)},
			} {
				s := &deliverySender{}
				if err := deliverRows(f.app, s, run.id, run.ids); err != nil {
					t.Fatalf("%s: %v", run.label, err)
				}
				if run.want.notSent {
					why := skipCantSeeDate
					if run.want.offList {
						why = skipNoLongerOnList
					}
					wantNotSent(t, run.label, f.app, s, f.reader.Email, run.ids[0], why)
					continue
				}
				checkDelivered(t, run.label, s.to(t, f.reader.Email), first, run.want, false, blankForbid(f)...)
			}
		})
	}
}

// A transient email failure leaves the row queued; the retry rechecks, so a
// rename and a tightening made after the failed attempt are what it sees: the
// reader, shut out of the series' one date they hold, is sent nothing.
func TestEmailRetryRechecksEventFields(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	id, ids := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.reader))
	first := firstName(f.reader.PublicName())

	failing := &deliverySender{fail: 1}
	if err := deliverRows(f.app, failing, id, ids); err == nil {
		t.Fatal("a transient failure did not stop the batch")
	}
	checkDelivered(t, "the failed attempt", failing.to(t, f.reader.Email), first, liveSeen(f, "Vis night"), false)

	f.rename(t, "Renamed night")
	if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
		t.Fatal(err)
	}
	retry := &deliverySender{}
	if err := deliverRows(f.app, retry, id, ids); err != nil {
		t.Fatal(err)
	}
	wantNotSent(t, "the retry", f.app, retry, f.reader.Email, ids[0], skipCantSeeDate)
}

// A change between two rows of one batch reaches the later row.
func TestEmailLaterRowSeesAChangeMadeAfterTheEarlierOne(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.leaderU, f.occs[0])
	f.register(t, f.reader, f.occs[0])
	id, ids := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.leaderU), toRow(f.reader))
	s := &deliverySender{after: func(n int) {
		if n == 1 {
			f.app.DB.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("title", "Renamed night")
		}
	}}
	if err := deliverRows(f.app, s, id, ids); err != nil {
		t.Fatal(err)
	}
	seen := func(title string) readWant {
		return readWant{title: title, when: f.when(f.occs[0]), event: f.eventURL(), manage: f.dateURL(f.occs[0])}
	}
	checkDelivered(t, "the earlier row", s.to(t, f.leaderU.Email), firstName(f.leaderU.PublicName()), seen("Vis night"), false)
	checkDelivered(t, "the later row", s.to(t, f.reader.Email), firstName(f.reader.PublicName()), seen("Renamed night"), false)
}

// An administrator sees any event, but only while they are one: demoted after
// the batch began and before their own row is decided, the later row, whose
// one date is in a group they are not in, is not sent.
func TestEmailRowDoesNotUseAnAdministratorsCachedPrivilege(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityInviteOnly)
	if err := f.app.DB.Model(&models.User{}).Where("id = ?", f.reader.ID).Update("role", models.RoleAdmin).Error; err != nil {
		t.Fatal(err)
	}
	f.register(t, f.leaderU, f.occs[0])
	f.register(t, f.reader, f.occs[0])
	first := firstName(f.reader.PublicName())
	seen := readWant{title: "Vis night", when: f.when(f.occs[0]), event: f.eventURL(), manage: f.dateURL(f.occs[0])}

	// Control: still an administrator, the reader sees the hidden event.
	cid, cids := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.reader))
	control := &deliverySender{}
	if err := deliverRows(f.app, control, cid, cids); err != nil {
		t.Fatal(err)
	}
	checkDelivered(t, "an administrator", control.to(t, f.reader.Email), first, seen, false)

	id, ids := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.leaderU), toRow(f.reader))
	s := &deliverySender{after: func(n int) {
		if n == 1 {
			f.app.DB.Model(&models.User{}).Where("id = ?", f.reader.ID).Update("role", models.RoleMember)
		}
	}}
	if err := deliverRows(f.app, s, id, ids); err != nil {
		t.Fatal(err)
	}
	checkDelivered(t, "the leader's row", s.to(t, f.leaderU.Email), firstName(f.leaderU.PublicName()), seen, false)
	wantNotSent(t, "the demoted administrator's row", f.app, s, f.reader.Email, ids[1], skipCantSeeDate)
}

// A guardian copy carries the teen's own message with the teen's values, so
// its event fields follow what the teen may see now, not what the guardian,
// here an administrator, may.
func TestGuardianCopyEventFieldsFollowTheTeensAccess(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityInviteOnly)
	teen := f.reader
	addMember(t, f.app, teen, &f.group, models.MembershipMember)
	f.register(t, teen, f.occs[0])
	parent := newTestUser(t, f.app, "parent", "password1234", models.RoleAdmin)
	row := func() models.MessageRecipient {
		r := toRow(parent)
		r.CopyOfUserID = &teen.ID
		return r
	}
	first := firstName(teen.PublicName())

	id, ids := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), row())
	s := &deliverySender{}
	if err := deliverRows(f.app, s, id, ids); err != nil {
		t.Fatal(err)
	}
	checkDelivered(t, "the teen can see the event", s.to(t, parent.Email), first, liveSeen(f, "Vis night"), true)

	// The teen lost the date: the copy, like the teen's own email, is not
	// sent at all (audienceSeers), so nothing of the author's words reaches
	// the guardian either.
	f.setMembership(t, teen, models.MembershipRemoved)
	f.rename(t, "Renamed night")
	id, ids = seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), row())
	s = &deliverySender{}
	if err := deliverRows(f.app, s, id, ids); err != nil {
		t.Fatal(err)
	}
	if len(s.msgs) != 0 {
		t.Fatalf("a copy of a date the teen lost went out: %+v", s.msgs)
	}
	var held models.MessageRecipient
	gormMust(t, f.app.DB.First(&held, ids[0]))
	if held.Status != models.RecipientSuppressed || held.Reason != skipCantSeeDate {
		t.Fatalf("the copy row: %+v", held)
	}
}

// failDateFacts makes the claimed row's one facts statement (loadDateFacts)
// fail until the test ends, on the handle every transaction shares.
func failDateFacts(t *testing.T, db *gorm.DB) {
	t.Helper()
	name := "test:fail-date-facts"
	if err := db.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "named_occ AS") {
			_ = tx.AddError(errors.New("lookup failed"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Row().Remove(name) })
}

// A failed read fails the delivery: nothing is sent and the row stays queued
// for the retry, never read as "may not see" or "may see". The protected
// facts come from one statement, so it is that statement that fails, for a
// one-date and a series audience, a reminder and a guardian copy; on the
// last attempt the row fails with the error and the message settles.
func TestEmailLookupFailureFailsTheDelivery(t *testing.T) {
	for _, series := range []bool{false, true} {
		t.Run(fmt.Sprintf("series=%v", series), func(t *testing.T) {
			f := newReadVisFixture(t, models.VisibilityPublic)
			addMember(t, f.app, f.reader, &f.group, models.MembershipMember)
			f.register(t, f.reader, f.occs[0])
			au := f.occAudience(f.occs[0])
			if series {
				au = f.seriesAudience()
			}
			id, ids := seedSending(t, f.app, f.leaderU, au, toRow(f.reader))
			failDateFacts(t, f.app.DB)
			s := &deliverySender{}
			if err := deliverRows(f.app, s, id, ids); err == nil {
				t.Fatal("a failed lookup was not an error")
			}
			if s.count() != 0 {
				t.Fatalf("%d emails went out after a failed lookup", s.count())
			}
			var row models.MessageRecipient
			if err := f.app.DB.First(&row, ids[0]).Error; err != nil || row.Status != models.RecipientQueued {
				t.Fatalf("the row after a failed lookup: %+v %v", row, err)
			}
			// Through the runner to the last attempt.
			if err := f.app.DB.Transaction(func(tx *gorm.DB) error { return enqueueBatches(tx, id, ids, "") }); err != nil {
				t.Fatal(err)
			}
			runMessageJobs(t, f.app)
			if err := f.app.DB.First(&row, ids[0]).Error; err != nil || row.Status != models.RecipientFailed || s.count() != 0 {
				t.Fatalf("after the last attempt: %+v %v, %d sent", row, err, s.count())
			}
			var m models.Message
			if err := f.app.DB.First(&m, id).Error; err != nil || m.Status != models.MessageFailed {
				t.Fatalf("the message after the last attempt: %s %v", m.Status, err)
			}
		})
	}
}

// A one-connection pool cannot serve the event fields from a second
// connection while the claimed row's transaction holds the only one: the
// fields are read through that transaction, so the email is sent. A deadline
// stands in for the hang a regression would be.
func TestEmailEventFieldsNeedOnlyTheClaimedConnection(t *testing.T) {
	oneConnection := func(t *testing.T, app *App) {
		t.Helper()
		sqlDB, err := app.DB.DB()
		if err != nil {
			t.Fatal(err)
		}
		before := sqlDB.Stats().MaxOpenConnections
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { sqlDB.SetMaxOpenConns(before) })
	}
	deliver := func(t *testing.T, app *App, s *deliverySender, id uint, ids []uint) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- deliverRows(app, s, id, ids) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("delivery is waiting for a second connection")
		}
	}
	t.Run("a series the reader can see", func(t *testing.T) {
		f := newReadVisFixture(t, models.VisibilityPublic)
		f.register(t, f.reader, f.occs[0])
		dateID, dateIDs := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
		seriesID, seriesIDs := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.reader))
		oneConnection(t, f.app)
		s := &deliverySender{}
		first := firstName(f.reader.PublicName())
		deliver(t, f.app, s, dateID, dateIDs)
		checkDelivered(t, "a one-date audience", s.to(t, f.reader.Email), first, liveSeen(f, "Vis night"), false)
		deliver(t, f.app, s, seriesID, seriesIDs)
		checkDelivered(t, "a series audience", s.to(t, f.reader.Email), first, liveSeen(f, "Vis night"), false)
	})
	t.Run("an exact-date invitation to a hidden series", func(t *testing.T) {
		f := newOccurrenceReadFixture(t)
		date := f.dates[1]
		dateID, dateIDs := seedSending(t, f.app, f.issuer, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: date.ID}, toRow(f.recipient))
		seriesID, seriesIDs := seedSending(t, f.app, f.issuer, audienceJSON{Type: policy.AudienceEventSeries, EventID: f.event.ID}, toRow(f.recipient))
		oneConnection(t, f.app)
		s := &deliverySender{}
		deliver(t, f.app, s, dateID, dateIDs)
		if got := s.to(t, f.recipient.Email); !strings.Contains(got.Subject, "Invited date title") {
			t.Errorf("the invited date: %q", got.Subject)
		}
		deliver(t, f.app, s, seriesID, seriesIDs)
		if got := s.to(t, f.recipient.Email); strings.Contains(got.Subject+got.Text+got.HTML, "Invited date title") {
			t.Errorf("a series announcement borrowed the date's title: %q", got.Subject)
		}
	})
}

// reminderPlace stands for the date's own place. A reminder carries it as
// literal text written when the reminder was prepared, not as a merge field,
// so it is what someone who can no longer see the date must never be sent.
const reminderPlace = "12 Hidden Lane"

// sentTo is how many emails went to addr.
func (s *deliverySender) sentTo(addr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.msgs {
		if m.To == addr {
			n++
		}
	}
	return n
}

// prepareReminder gives the group event a place, registers going, runs the
// real reminder job while the date is still public and returns the reminder
// with the queued email row of each of rowUsers by user id (a guardian copy's
// row is its guardian's).
func prepareReminder(t *testing.T, f composeFixture, going []*models.User, rowUsers ...*models.User) (uint, map[uint]models.MessageRecipient) {
	t.Helper()
	if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.groupEvent.ID).Update("location", reminderPlace).Error; err != nil {
		t.Fatal(err)
	}
	for _, u := range going {
		if err := f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &u.ID, Status: models.RegGoing}).Error; err != nil {
			t.Fatal(err)
		}
	}
	at := f.groupOcc.StartsAt.Add(-24 * time.Hour)
	if err := f.app.sendReminder(context.Background(), reminderJob{OccurrenceID: f.groupOcc.ID, StartsAt: f.groupOcc.StartsAt.Unix(), Minutes: 1440}, at); err != nil {
		t.Fatal(err)
	}
	var m models.Message
	if err := f.app.DB.Where("category = ? AND occurrence_id = ?", models.MessageCategoryReminder, f.groupOcc.ID).First(&m).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.BodyHTML, reminderPlace) {
		t.Fatalf("the prepared reminder lost its place: %q", m.BodyHTML)
	}
	ids := make([]uint, 0, len(rowUsers))
	for _, u := range rowUsers {
		ids = append(ids, u.ID)
	}
	var rows []models.MessageRecipient
	if err := f.app.DB.Where("message_id = ? AND channel = ? AND user_id IN ?", m.ID, models.DeliveryEmail, ids).Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]models.MessageRecipient{}
	for _, r := range rows {
		if r.Status != models.RecipientQueued {
			t.Fatalf("a prepared reminder row: %+v", r)
		}
		out[r.UserID] = r
	}
	if len(out) != len(rowUsers) {
		t.Fatalf("the prepared reminder rows: %+v", rows)
	}
	return m.ID, out
}

// reminderTightener narrows the date to a visibility after its reminder was
// prepared, and renames it so a reminder that borrowed the live title would
// show it.
type reminderTightener struct {
	name  string
	apply func(t *testing.T, f composeFixture)
	title string
}

// reminderTighteners narrow the whole event, and then one date alone, to
// visibility. Private closes a date to a signed-out visitor only: every
// signed-in person, a former member included, still sees it. Only invite-only
// shuts out a person who is not in the group (policy.visibleTo).
func reminderTighteners(visibility string) []reminderTightener {
	return []reminderTightener{
		{"the event made " + visibility, func(t *testing.T, f composeFixture) {
			if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.groupEvent.ID).
				Updates(map[string]any{"title": "Renamed night", "visibility": visibility}).Error; err != nil {
				t.Fatal(err)
			}
		}, "Renamed night"},
		{"one date made " + visibility, func(t *testing.T, f composeFixture) {
			if err := f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.groupOcc.ID).
				Updates(map[string]any{"title_override": "Special night", "visibility_override": visibility}).Error; err != nil {
				t.Fatal(err)
			}
		}, "Special night"},
	}
}

func leaveGroup(t *testing.T, f composeFixture, u *models.User) {
	t.Helper()
	if err := f.app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.group.ID, u.ID).Update("status", models.MembershipLeft).Error; err != nil {
		t.Fatal(err)
	}
}

// requireSkippedCantSee asserts a reminder row was never sent: it is
// suppressed with the can't-see-the-date reason, and nothing went to addr.
func requireSkippedCantSee(t *testing.T, f composeFixture, s *deliverySender, label string, row models.MessageRecipient) {
	t.Helper()
	if n := s.sentTo(row.Address); n != 0 {
		t.Errorf("%s: %d emails went to someone who may not see the date", label, n)
	}
	var got models.MessageRecipient
	if err := f.app.DB.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != models.RecipientSuppressed || got.Reason != skipCantSeeDate {
		t.Errorf("%s: the row is %q (%q), want it suppressed as %q", label, got.Status, got.Reason, skipCantSeeDate)
	}
}

// requireReminderSeen asserts the reminder reached addr with the live title
// and the date's place, the control that tells delivery from suppression.
func requireReminderSeen(t *testing.T, s *deliverySender, label, addr, title string) {
	t.Helper()
	got := s.to(t, addr)
	if !strings.Contains(got.Subject, title) {
		t.Errorf("%s: subject %q lacks %q", label, got.Subject, title)
	}
	for part, text := range map[string]string{"text": got.Text, "html": got.HTML} {
		if !strings.Contains(text, reminderPlace) {
			t.Errorf("%s: the %s lost the place a reader who may see the date is given: %q", label, part, text)
		}
	}
}

// A reminder's text carries the date's place, so someone who can no longer
// see the date is sent no reminder at all, not one with its fields blanked,
// while a member of the group beside them still gets the live title and the
// place (C5, M1.13). Who loses the date follows the visibility: private shuts
// out only a guest, and a former member, still signed in, is the control that
// keeps getting it; invite-only shuts out the former member too.
func TestReminderEmailSkipsWhoCanNoLongerSeeTheDate(t *testing.T) {
	for _, vis := range []struct {
		name          string
		visibility    string
		formerStaysIn bool
	}{
		{"private", models.VisibilityPrivate, true},
		{"invite only", models.VisibilityInviteOnly, false},
	} {
		for _, tc := range reminderTighteners(vis.visibility) {
			t.Run(vis.name+"/"+tc.name, func(t *testing.T) {
				f := newComposeFixture(t)
				f.admin.holdNotices, f.leader.holdNotices = true, true
				guest := newTestUser(t, f.app, "reminder-guest", "password1234", models.RoleMember)
				if err := f.app.DB.Model(guest).Update("status", models.UserGuest).Error; err != nil {
					t.Fatal(err)
				}
				former := newTestUser(t, f.app, "reminder-former", "password1234", models.RoleMember)
				addMember(t, f.app, former, f.group, models.MembershipMember)
				id, rows := prepareReminder(t, f, []*models.User{guest, former, f.teen}, guest, former, f.teen)
				tc.apply(t, f)
				leaveGroup(t, f, former)
				s := &deliverySender{}
				if err := deliverRows(f.app, s, id, []uint{rows[guest.ID].ID, rows[former.ID].ID, rows[f.teen.ID].ID}); err != nil {
					t.Fatal(err)
				}
				requireSkippedCantSee(t, f, s, "a guest", rows[guest.ID])
				want := 1
				if vis.formerStaysIn {
					want++
					requireReminderSeen(t, s, "a former member who is still signed in", rows[former.ID].Address, tc.title)
				} else {
					requireSkippedCantSee(t, f, s, "a former member", rows[former.ID])
				}
				requireReminderSeen(t, s, "a member of the group", rows[f.teen.ID].Address, tc.title)
				if s.count() != want {
					t.Errorf("%d emails went out, want %d", s.count(), want)
				}
			})
		}
	}
}

// A delivery that failed for a transient reason and runs again after the date
// was made private is rechecked at that send: the retry sends the person who
// lost the date nothing.
func TestReminderEmailRetryRechecksTheDate(t *testing.T) {
	for _, tc := range reminderTighteners(models.VisibilityPrivate) {
		t.Run(tc.name, func(t *testing.T) {
			f := newComposeFixture(t)
			f.admin.holdNotices, f.leader.holdNotices = true, true
			guest := newTestUser(t, f.app, "reminder-guest", "password1234", models.RoleMember)
			if err := f.app.DB.Model(guest).Update("status", models.UserGuest).Error; err != nil {
				t.Fatal(err)
			}
			id, rows := prepareReminder(t, f, []*models.User{guest, f.teen}, guest, f.teen)
			ids := []uint{rows[guest.ID].ID, rows[f.teen.ID].ID}
			failing := &deliverySender{fail: 1}
			if err := deliverRows(f.app, failing, id, ids); !errors.Is(err, errStopBatch) {
				t.Fatalf("the first attempt: %v, want it to stop for a retry", err)
			}
			tc.apply(t, f)
			s := &deliverySender{}
			if err := deliverRows(f.app, s, id, ids); err != nil {
				t.Fatal(err)
			}
			requireSkippedCantSee(t, f, s, "a guest on the retry", rows[guest.ID])
			requireReminderSeen(t, s, "a member of the group on the retry", rows[f.teen.ID].Address, tc.title)
			if s.count() != 1 {
				t.Errorf("%d emails went out on the retry, want the member's alone", s.count())
			}
		})
	}
}

// A guardian copy of a reminder is the teen's own: it follows what the teen
// may see of the date, not the guardian, so a copy to an administrator who
// could still see everything is not sent once the teen lost the date, and a
// copy to a guardian who could not is, while the teen still sees it. A teen
// who left but is still signed in keeps a private date, so both are sent.
func TestReminderGuardianCopyFollowsTheTeensDate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		guardianRole string
		visibility   string
		teenLeaves   bool
		sent         bool
	}{
		{"the teen can still see the date", models.RoleMember, models.VisibilityInviteOnly, false, true},
		{"the teen lost the date", models.RoleAdmin, models.VisibilityInviteOnly, true, false},
		{"the teen left but the date is private", models.RoleAdmin, models.VisibilityPrivate, true, true},
	} {
		for _, tight := range reminderTighteners(tc.visibility) {
			t.Run(tc.name+"/"+tight.name, func(t *testing.T) {
				f := newComposeFixture(t)
				f.admin.holdNotices, f.leader.holdNotices = true, true
				guardian := newTestUser(t, f.app, "reminder-guardian", "password1234", tc.guardianRole)
				if err := f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.app.DB.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &f.teen.ID, Status: models.GuardianConfirmed}).Error; err != nil {
					t.Fatal(err)
				}
				id, rows := prepareReminder(t, f, []*models.User{f.teen}, f.teen, guardian)
				copyRow := rows[guardian.ID]
				if copyRow.CopyOfUserID == nil || *copyRow.CopyOfUserID != f.teen.ID {
					t.Fatalf("the guardian's row is not a copy of the teen's: %+v", copyRow)
				}
				tight.apply(t, f)
				if tc.teenLeaves {
					leaveGroup(t, f, f.teen)
				}
				s := &deliverySender{}
				if err := deliverRows(f.app, s, id, []uint{rows[f.teen.ID].ID, copyRow.ID}); err != nil {
					t.Fatal(err)
				}
				if !tc.sent {
					requireSkippedCantSee(t, f, s, "the teen", rows[f.teen.ID])
					requireSkippedCantSee(t, f, s, "the guardian's copy", copyRow)
					if s.count() != 0 {
						t.Errorf("%d emails went out, want none", s.count())
					}
					return
				}
				requireReminderSeen(t, s, "the teen", rows[f.teen.ID].Address, tight.title)
				requireReminderSeen(t, s, "the guardian's copy", copyRow.Address, "Copy: Reminder: "+tight.title)
			})
		}
	}
}

// An exact-date invitation shows the invited date alone, and once it ends the
// date's announcement no longer goes to them; a series announcement never
// borrows the date's title.
func TestEmailExactDateInvitation(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	date := f.dates[1]
	first := firstName(f.recipient.PublicName())
	dateURL := f.app.emailLink(occurrenceDatePath(f.event.ID, models.FormatCalendarDate(date.OriginalDate)))
	when := formatWhen(date.StartsAt, eventZone("UTC"))
	seeOne := readWant{title: "Invited date title", when: when, manage: dateURL}
	forbid := []string{"Hidden series title", "Invited date title", when, f.app.emailLink("/events/" + itoa(f.event.ID))}
	run := func(label string, au audienceJSON, want readWant) {
		t.Helper()
		id, ids := seedSending(t, f.app, f.issuer, au, toRow(f.recipient))
		s := &deliverySender{}
		if err := deliverRows(f.app, s, id, ids); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if want.notSent {
			wantNotSent(t, label, f.app, s, f.recipient.Email, ids[0], skipCantSeeDate)
			return
		}
		checkDelivered(t, label, s.to(t, f.recipient.Email), first, want, false, forbid...)
	}
	occ := audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: date.ID}
	series := audienceJSON{Type: policy.AudienceEventSeries, EventID: f.event.ID}
	run("the invited date", occ, seeOne)
	run("the series, by a titled date", series, readWant{})
	endSeededAccess(t, f.app.DB, f.access.RegistrationID, policy.OccurrenceDeclined, &f.recipient.ID, time.Now().UTC())
	run("the ended invitation", occ, readWant{notSent: true})
}

// A reminder email, first or retried, is filled by the same rules: the
// registrant whose date was renamed or cancelled gets only what the read page
// would give them, and one who lost the date gets no reminder.
func TestReminderEmailRechecksEventFields(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "first send"
		if retry {
			name = "Try again"
		}
		t.Run(name+"/access lost and renamed", func(t *testing.T) {
			f, id, rows, _ := prepareChoiceBatch(t, true, retry, false)
			var ev models.Event
			if err := f.app.DB.First(&ev, f.groupEvent.ID).Error; err != nil {
				t.Fatal(err)
			}
			when := formatWhen(f.groupOcc.StartsAt, eventZone(ev.TZID))
			if err := f.app.DB.Model(&models.Event{}).Where("id = ?", ev.ID).Updates(map[string]any{"title": "Renamed night", "visibility": models.VisibilityInviteOnly}).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.group.ID, f.teen.ID).Update("status", models.MembershipLeft).Error; err != nil {
				t.Fatal(err)
			}
			s := &deliverySender{}
			if err := deliverRows(f.app, s, id, []uint{rows[0].ID, rows[1].ID}); err != nil {
				t.Fatal(err)
			}
			if got := s.to(t, rows[0].Address); !strings.Contains(got.Subject, "Renamed night") || !strings.Contains(got.Subject, when) {
				t.Errorf("the leader still sees the date: %q", got.Subject)
			}
			requireSkippedCantSee(t, f, s, "a registrant who lost the date", rows[1])
		})
		t.Run(name+"/cancelled", func(t *testing.T) {
			f, id, rows, _ := prepareChoiceBatch(t, true, retry, false)
			var ev models.Event
			if err := f.app.DB.First(&ev, f.groupEvent.ID).Error; err != nil {
				t.Fatal(err)
			}
			when := formatWhen(f.groupOcc.StartsAt, eventZone(ev.TZID))
			f.app.DB.Model(&models.Event{}).Where("id = ?", ev.ID).Update("title", "Renamed night")
			if err := f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.groupOcc.ID).Update("status", models.OccurrenceCancelled).Error; err != nil {
				t.Fatal(err)
			}
			s := &deliverySender{}
			if err := deliverRows(f.app, s, id, []uint{rows[0].ID, rows[1].ID}); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				got := s.to(t, row.Address)
				if !strings.Contains(got.Subject, "Renamed night") || strings.Contains(got.Subject+got.Text+got.HTML, when) {
					t.Errorf("a cancelled date gives the series title and no time: %q / %q", got.Subject, got.Text)
				}
			}
		})
	}
}

// A push held for quiet hours is decided and filled when it goes, after the
// morning opens: a rename shows, and a person whose date is no longer one they
// may see gets no push at all (audienceSeers), not one with the title blanked.
func TestAnnouncementPushRechecksEventFieldsAfterQuietHours(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f composeFixture)
		title  string // "" when the push is suppressed
	}{
		{"renamed", func(t *testing.T, f composeFixture) {
			if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.groupEvent.ID).Update("title", "Renamed night").Error; err != nil {
				t.Fatal(err)
			}
		}, "Renamed night news"},
		{"tightened and left the group", func(t *testing.T, f composeFixture) {
			if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.groupEvent.ID).Updates(map[string]any{"title": "Renamed night", "visibility": models.VisibilityInviteOnly}).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.group.ID, f.teen.ID).Update("status", models.MembershipLeft).Error; err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"deleted", func(t *testing.T, f composeFixture) {
			if err := f.app.DB.Exec("UPDATE events SET deleted_at = now() WHERE id = ?", f.groupEvent.ID).Error; err != nil {
				t.Fatal(err)
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComposeFixture(t)
			fp := useFakePush(t, f.app)
			clock := quietHours(f.app)
			pushConsentRow(t, f.app, f.teen, "all", models.ConsentOptedIn)
			f.app.DB.Model(f.teen).Update("timezone", "America/Denver")
			phone := newPushDevice(t, f.app, f.teen, "phone")
			if err := f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &f.teen.ID, Status: models.RegGoing}).Error; err != nil {
				t.Fatal(err)
			}
			id := f.scheduleNow(t, f.leader, "{{event_title}} news", "<p>{{event_title}} at {{event_time}}</p>",
				map[string]any{"type": "occurrence", "occurrenceId": f.groupOcc.ID})
			runMessageJobs(t, f.app)
			if row := pushRowsOf(t, f.app, id)[f.teen.Email]; row.Status != models.RecipientQueued || fp.total() != 0 {
				t.Fatalf("the push should be waiting for the morning: %+v, pushes %v", row, fp.sent)
			}
			tc.change(t, f)
			clock.set(8, 0, 16)
			makeDue(f.app)
			drainJobs(t, f.app)
			if tc.title == "" {
				if row := pushRowsOf(t, f.app, id)[f.teen.Email]; row.Status != models.RecipientSuppressed || row.Reason != skipCantSeeDate || fp.total() != 0 {
					t.Fatalf("in the morning: %+v, pushes %v; want no push", row, fp.sent)
				}
				return
			}
			if row := pushRowsOf(t, f.app, id)[f.teen.Email]; row.Status != models.RecipientSent || fp.count(phone.path) != 1 {
				t.Fatalf("in the morning: %+v, pushes %v", row, fp.sent)
			}
			note := pushPayload(t, fp, phone)
			if note.Title != tc.title {
				t.Errorf("lock-screen title %q, want %q", note.Title, tc.title)
			}
		})
	}
}

// A reminder's guardian copy rows already follow the same switch and links as
// an announcement's: this pins that, since a notice's copy (notify.guardian_copy)
// follows the same rules and the two should not drift. No behavior is under
// test here that this change adds.
func TestReminderGuardianCopyMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		switchOn bool
		setup    func(t *testing.T, f composeFixture, mom, dad *models.User)
		wantMom  string // "" means no copy row
		wantDad  string
	}{
		{name: "two confirmed guardians", switchOn: true, wantMom: models.RecipientQueued, wantDad: models.RecipientQueued},
		{name: "switch off", switchOn: false},
		{name: "a pending link", switchOn: true, wantDad: models.RecipientQueued, setup: func(t *testing.T, f composeFixture, mom, _ *models.User) {
			if err := f.app.DB.Model(&models.GuardianLink{}).Where("guardian_user_id = ?", mom.ID).Update("status", models.GuardianPending).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a revoked link", switchOn: true, wantDad: models.RecipientQueued, setup: func(t *testing.T, f composeFixture, mom, _ *models.User) {
			if err := f.app.DB.Model(&models.GuardianLink{}).Where("guardian_user_id = ?", mom.ID).Update("status", models.GuardianRevoked).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a guardian with event updates off", switchOn: true, wantMom: models.RecipientSuppressed, wantDad: models.RecipientQueued, setup: func(t *testing.T, f composeFixture, mom, _ *models.User) {
			if err := f.app.DB.Create(&models.Consent{UserID: mom.ID, Channel: models.ConsentEmail, Scope: categoryScope(notify.CatEvents), Status: models.ConsentOptedOut, Source: models.ConsentSourceWebForm}).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a guardian whose address bounced", switchOn: true, wantMom: models.RecipientSuppressed, wantDad: models.RecipientQueued, setup: func(t *testing.T, f composeFixture, mom, _ *models.User) {
			suppressDeliverAddress(t, f.app, mom)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComposeFixture(t)
			f.admin.holdNotices, f.leader.holdNotices = true, true
			mom := newTestUser(t, f.app, "matrix-mom", "password1234", models.RoleMember)
			dad := newTestUser(t, f.app, "matrix-dad", "password1234", models.RoleMember)
			for _, g := range []*models.User{mom, dad} {
				if err := f.app.DB.Create(&models.GuardianLink{GuardianUserID: g.ID, TeenUserID: &f.teen.ID, Status: models.GuardianConfirmed}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.switchOn {
				if err := f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.setup != nil {
				tc.setup(t, f, mom, dad)
			}
			id, _ := prepareReminder(t, f, []*models.User{f.teen}, f.teen)
			var all []models.MessageRecipient
			if err := f.app.DB.Where("message_id = ? AND channel = ? AND copy_of_user_id = ?", id, models.DeliveryEmail, f.teen.ID).Find(&all).Error; err != nil {
				t.Fatal(err)
			}
			rows := map[uint]models.MessageRecipient{}
			for _, r := range all {
				rows[r.UserID] = r
			}
			for g, want := range map[*models.User]string{mom: tc.wantMom, dad: tc.wantDad} {
				row, has := rows[g.ID]
				if want == "" {
					if has {
						t.Errorf("%s got a copy row: %+v", g.Email, row)
					}
					continue
				}
				if !has || row.Status != want || row.CopyOfUserID == nil || *row.CopyOfUserID != f.teen.ID {
					t.Errorf("%s: row %+v, want a %s copy of the teen's", g.Email, row, want)
				}
			}
		})
	}
}

// TestBatchHonorsAChoiceCommittedRightAfterTheClaim: the app's pool defaults
// to Repeatable Read. Right after a row's claim, on an independent connection,
// its person turns the message's kind of email off. The row's transaction is
// pinned to Read Committed, so the choice read after the claim sees the
// change: nothing goes to that person and the row says why. On a Repeatable
// Read claim the choice would be read in the claim's snapshot and the email
// would go. The other row is the control.
func TestBatchHonorsAChoiceCommittedRightAfterTheClaim(t *testing.T) {
	for _, mode := range []struct {
		name              string
		reminder, copyRow bool
	}{{"announcement", false, false}, {"reminder", true, false}, {"guardian copy", false, true}} {
		t.Run(mode.name, func(t *testing.T) {
			f, id, rows, target := prepareChoiceBatch(t, mode.reminder, false, mode.copyRow)
			cat := notify.CatMessages
			if mode.reminder {
				cat = notify.CatEvents
			}
			orig := f.app.DB
			f.app.DB = repeatableReadPool(t, f.app)
			t.Cleanup(func() { f.app.DB = orig })
			fired := 0
			f.app.hooks.batchRowClaimed = func(tx *gorm.DB, rowID uint) {
				if rowID != rows[1].ID {
					return
				}
				fired++
				var level string
				if err := tx.Raw("SHOW transaction_isolation").Scan(&level).Error; err != nil || level != "read committed" {
					t.Errorf("the claim runs at %q (%v), want read committed", level, err)
				}
				if err := orig.Create(&models.Consent{UserID: target.ID, Channel: models.ConsentEmail, Scope: categoryScope(cat),
					Status: models.ConsentOptedOut, Source: models.ConsentSourceWebForm}).Error; err != nil {
					t.Error(err)
				}
			}
			s := &deliverySender{}
			f.app.Mailer = s
			if err := f.app.deliverMessageBatchOnce(context.Background(), messageBatchJob{MessageID: id, RecipientIDs: []uint{rows[0].ID, rows[1].ID}}); err != nil {
				t.Fatal(err)
			}
			if fired != 1 {
				t.Fatalf("the claim seam fired %d times", fired)
			}
			if n := s.sentTo(rows[1].Address); n != 0 {
				t.Fatalf("%d emails went after the choice committed", n)
			}
			var got models.MessageRecipient
			gormMust(t, orig.First(&got, rows[1].ID))
			if got.Status != models.RecipientSuppressed || got.Reason != skipOptedOutOf(cat) {
				t.Fatalf("the row: %q %q", got.Status, got.Reason)
			}
			if s.sentTo(rows[0].Address) != 1 {
				t.Fatalf("the control row was not sent: %d", s.sentTo(rows[0].Address))
			}
		})
	}
}

// Literal words an author wrote about a date, which reach only someone the
// date's check lets in.
const (
	batchSecretSubject = "Secret literal subject"
	batchSecretText    = "Secret literal body"
	batchMovedTitle    = "Moved secret title"
	batchMovedPlace    = "Moved secret place"
)

// seedDatedSending stores a message going out about date (an announcement
// with literal words and the date's fields, or a reminder whose text carries
// the stored place) with a queued row for each of rows, in order.
func seedDatedSending(t *testing.T, app *App, author *models.User, date models.EventOccurrence, reminder bool, rows ...models.MessageRecipient) (uint, []models.MessageRecipient) {
	t.Helper()
	raw, _ := json.Marshal(audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: date.ID, Statuses: []string{models.RegGoing}})
	now := time.Now().UTC().Add(-time.Minute)
	m := models.Message{AuthorID: &author.ID, Status: models.MessageSending, Category: models.MessageCategoryAnnouncement,
		Subject: batchSecretSubject + " {{event_title}}", BodyHTML: content.SanitizeHTML("<p>" + batchSecretText + " " + readVisBody[3:]),
		BodyText: batchSecretText + " " + deliveryBodyText, Audience: string(raw), ScheduledAt: &now}
	if reminder {
		m.AuthorID, m.Category, m.OccurrenceID = nil, models.MessageCategoryReminder, &date.ID
		m.Subject, m.BodyHTML, m.BodyText = "Reminder: {{event_title}}", "<p>Where: "+reminderPlace+"</p>", "Where: "+reminderPlace
	}
	gormMust(t, app.DB.Create(&m))
	for i := range rows {
		rows[i].MessageID, rows[i].Channel, rows[i].Status = m.ID, models.DeliveryEmail, models.RecipientQueued
		gormMust(t, app.DB.Create(&rows[i]))
	}
	return m.ID, rows
}

// datedBatchFixture is a departed person's kept, invite-only date at its old
// end (their grant lapses there), a member of the group going to it (the
// control), and for a guardian copy the departed person as a consenting teen
// with a confirmed guardian.
type datedBatchFixture struct {
	occurrenceReadFixture
	target   models.EventOccurrence
	control  *models.User
	guardian *models.User
}

func newDatedBatchFixture(t *testing.T, copyRow bool) datedBatchFixture {
	t.Helper()
	f := datedBatchFixture{occurrenceReadFixture: newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)}
	f.target = f.dates[1]
	// The edit's own notices are not what these tests count.
	f.admin.holdNotices = true
	f.control = newTestUser(t, f.app, "going member", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: f.control.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, f.app.DB.Create(&models.Registration{OccurrenceID: f.target.ID, UserID: &f.control.ID, Status: models.RegGoing}))
	if copyRow {
		gormMust(t, f.app.DB.Model(f.recipient).Update("account_type", models.AccountTeen))
		gormMust(t, f.app.DB.Create(&models.TeenConsent{TeenUserID: f.recipient.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted,
			GrantedBy: f.issuer.ID, GranterRole: models.ConsentAdmin, EvidenceText: "Checked with a parent"}))
		f.guardian = newTestUser(t, f.app, "teen's guardian", "password1234", models.RoleMember)
		gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: f.guardian.ID, TeenUserID: &f.recipient.ID, Status: models.GuardianConfirmed}))
	}
	return f
}

// subjectRow is the departed person's row: their own, or their guardian's copy.
func (f datedBatchFixture) subjectRow() models.MessageRecipient {
	if f.guardian != nil {
		return models.MessageRecipient{UserID: f.guardian.ID, CopyOfUserID: &f.recipient.ID, Address: f.guardian.Email}
	}
	return toRow(f.recipient)
}

// guardedEdit moves the target date two hours later, retitles it and gives it
// a new place through the real guarded edit, which stamps the lapsed grant
// ended in the same commit.
func (f datedBatchFixture) guardedEdit(t *testing.T, base occurrenceJSON) *httptest.ResponseRecorder {
	newStart := f.target.StartsAt.Add(2 * time.Hour)
	return postDateEdit(f.admin, f.event.ID, models.FormatCalendarDate(f.target.OriginalDate), dateEditBody(newDateAttempt(t), base.Revision,
		map[string]any{"startsLocal": newStart.UTC().Format("2006-01-02T15:04"), "title": batchMovedTitle, "location": batchMovedPlace}))
}

// TestBatchEmailNeverPairsALapsedGrantWithAMovedDate: a departed person's kept
// date reaches its old end, so their grant has lapsed. The real guarded edit
// then moves the date later, retitles it and stamps the grant ended, in one
// commit, right after the row's facts statement was read. Before the commit
// the grant had lapsed; after it, it is stamped: in no committed state may
// the person see the date, so nothing goes to them (or, for a copy, to their
// guardian), however the reads fall. The member going to the date is the
// control and is sent. The split readers, given the same clock and the same
// edit between their grant read and their date read, do let the person in:
// that pairing is what one statement rules out.
func TestBatchEmailNeverPairsALapsedGrantWithAMovedDate(t *testing.T) {
	for _, mode := range []struct {
		name                      string
		reminder, copyRow, series bool
	}{{"one-date announcement", false, false, false}, {"series announcement", false, false, true}, {"reminder", true, false, false}, {"guardian copy", false, true, false}} {
		t.Run(mode.name, func(t *testing.T) {
			f := newDatedBatchFixture(t, mode.copyRow)
			id, rows := seedDatedSending(t, f.app, f.issuer, f.target, mode.reminder, f.subjectRow(), toRow(f.control))
			if mode.series {
				raw, _ := json.Marshal(audienceJSON{Type: policy.AudienceEventSeries, EventID: f.event.ID, Statuses: []string{models.RegGoing}})
				gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("audience", string(raw)))
			}
			base := editBaseOf(t, f.admin, f.event.ID, models.FormatCalendarDate(f.target.OriginalDate))
			newMutableClock(f.app, f.target.EndsAt)
			fired := 0
			var edit *httptest.ResponseRecorder
			f.app.hooks.dateFactsRead = func() {
				if fired++; fired == 1 {
					edit = f.guardedEdit(t, base)
				}
			}
			s := &deliverySender{}
			statements := recordStatements(t, f.app)
			if err := deliverRows(f.app, s, id, []uint{rows[0].ID, rows[1].ID}); err != nil {
				t.Fatal(err)
			}
			wantOneFactsStatementPerRow(t, statements(), 2)
			if fired != 2 || edit == nil || edit.Code != http.StatusOK {
				t.Fatalf("the seam fired %d times; the edit: %v", fired, edit)
			}
			if o := occurrenceRow(t, f.app, f.target.ID); !o.StartsAt.Equal(f.target.StartsAt.Add(2 * time.Hour)) {
				t.Fatalf("the edit did not move the date: %v", o.StartsAt)
			}
			var grant models.Registration
			gormMust(t, f.app.DB.First(&grant, f.access.RegistrationID))
			if grant.AccessEndedAt == nil {
				t.Fatal("the edit did not stamp the lapsed grant ended")
			}
			wantNotSent(t, "the departed person", f.app, s, rows[0].Address, rows[0].ID, skipCantSeeDate)
			for _, m := range s.msgs {
				if m.To == rows[0].Address {
					continue
				}
				if m.To != f.control.Email {
					t.Fatalf("an email went to %s", m.To)
				}
			}
			if s.sentTo(f.control.Email) != 1 {
				t.Fatalf("the control was sent %d emails", s.sentTo(f.control.Email))
			}
		})
	}
	// The negative control: the split readers on a Read Committed transaction,
	// at the same clock, with the edit committed between their grant read and
	// their date read, let the departed person in.
	f := newDatedBatchFixture(t, false)
	base := editBaseOf(t, f.admin, f.event.ID, models.FormatCalendarDate(f.target.OriginalDate))
	newMutableClock(f.app, f.target.EndsAt)
	var fired atomic.Bool
	f.app.hooks.invitationReadStep = func(step string) {
		if step == "grants" && fired.CompareAndSwap(false, true) {
			if w := f.guardedEdit(t, base); w.Code != http.StatusOK {
				t.Errorf("the edit at the split seam: %d %s", w.Code, w.Body.String())
			}
		}
	}
	au := audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: f.target.ID, Statuses: []string{models.RegGoing}}
	now := f.app.now().UTC()
	var seers map[uint]bool
	if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		seers, err = f.app.audienceSeers(tx, au, []uint{f.recipient.ID}, now, now)
		return err
	}, readCommitted); err != nil {
		t.Fatal(err)
	}
	if !fired.Load() || !seers[f.recipient.ID] {
		t.Fatalf("the split readers did not pair the live grant with the moved date (fired %v, seers %v): the fixture proves nothing", fired.Load(), seers)
	}
}

// TestBatchEmailInFlightSendsOnlyTheOldWholePayload: the departed person may
// still see their kept date when the row's facts are read. Right after, one
// commit ends their grant and retitles the date. That one email may go, and
// it carries the old payload whole: the old title, time and link, never the
// new title. The next attempt for the same row is suppressed.
func TestBatchEmailInFlightSendsOnlyTheOldWholePayload(t *testing.T) {
	f := newDatedBatchFixture(t, false)
	id, rows := seedDatedSending(t, f.app, f.issuer, f.target, false, f.subjectRow())
	newMutableClock(f.app, f.target.StartsAt.Add(-time.Hour))
	var fired atomic.Bool
	f.app.hooks.dateFactsRead = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.EventOccurrence{}).Where("id = ?", f.target.ID).Update("title_override", batchMovedTitle).Error; err != nil {
				return err
			}
			endSeededAccess(t, tx, f.access.RegistrationID, policy.OccurrenceDeclined, &f.recipient.ID, f.app.now().UTC())
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	s := &deliverySender{}
	if err := deliverRows(f.app, s, id, []uint{rows[0].ID}); err != nil {
		t.Fatal(err)
	}
	got := s.to(t, f.recipient.Email)
	want := readWant{title: "Retained date title", when: formatWhen(f.target.StartsAt, eventZone("UTC")),
		manage: f.app.emailLink(occurrenceDatePath(f.event.ID, models.FormatCalendarDate(f.target.OriginalDate)))}
	if got.Subject != batchSecretSubject+" "+want.title {
		t.Fatalf("subject %q", got.Subject)
	}
	for part, text := range map[string]string{"text": got.Text, "html": got.HTML} {
		for _, w := range []string{batchSecretText, want.title, want.when, want.manage} {
			if !strings.Contains(text, w) {
				t.Errorf("the %s lacks %q: %s", part, w, text)
			}
		}
		if strings.Contains(text, batchMovedTitle) {
			t.Errorf("the %s carries the new title: %s", part, text)
		}
	}
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", rows[0].ID).Updates(map[string]any{"status": models.RecipientQueued, "sent_at": nil}))
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("status", models.MessageSending))
	again := &deliverySender{}
	if err := deliverRows(f.app, again, id, []uint{rows[0].ID}); err != nil {
		t.Fatal(err)
	}
	wantNotSent(t, "the next attempt", f.app, again, f.recipient.Email, rows[0].ID, skipCantSeeDate)
}

// TestGuardianCopyOfADisabledTeenSaysOnlyWhatASignedOutVisitorSees: a teen's
// reminder and its guardian copy are prepared for a private date (seen by any
// signed-in person, not a signed-out visitor). The teen's account consent is
// then revoked, which disables the account. The teen's row is not sent, and
// the guardian, active and opted in, is not sent the copy either: a teen who
// cannot sign in lends it no signed-in authority, so the copy says only what
// a signed-out visitor could see. On a public date the copy still goes.
func TestGuardianCopyOfADisabledTeenSaysOnlyWhatASignedOutVisitorSees(t *testing.T) {
	for _, visibility := range []string{models.VisibilityPrivate, models.VisibilityPublic} {
		t.Run(visibility, func(t *testing.T) {
			f := newComposeFixture(t)
			gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.groupEvent.ID).Update("visibility", visibility))
			guardian := newTestUser(t, f.app, "guardian", "password1234", models.RoleMember)
			gormMust(t, f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"}))
			gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: guardian.ID, TeenUserID: &f.teen.ID, Status: models.GuardianConfirmed}))
			id, rows := prepareReminder(t, f, []*models.User{f.teen}, f.teen, guardian)
			if w := f.admin.do("POST", "/api/v1/admin/users/"+itoa(f.teen.ID)+"/teen-consents", map[string]any{"scope": models.TeenConsentAccount, "status": models.ConsentRevoked}, true); w.Code != http.StatusOK {
				t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
			}
			s := &deliverySender{}
			if err := deliverRows(f.app, s, id, []uint{rows[f.teen.ID].ID, rows[guardian.ID].ID}); err != nil {
				t.Fatal(err)
			}
			wantNotSent(t, "the disabled teen", f.app, s, f.teen.Email, rows[f.teen.ID].ID, skipNotActive)
			if visibility == models.VisibilityPublic {
				requireReminderSeen(t, s, "the guardian's copy", guardian.Email, "Copy: Reminder: Book night")
				return
			}
			requireSkippedCantSee(t, f, s, "the guardian's copy", rows[guardian.ID])
			if s.count() != 0 {
				t.Fatalf("%d emails went out", s.count())
			}
		})
	}
}

// recordStatements logs, per claimed row, every read its claim transaction
// makes after the claim (the batchRowClaimed seam), until the test ends.
// Reads on any other connection (a concurrent edit's) are not logged.
func recordStatements(t *testing.T, app *App) func() map[uint][]string {
	t.Helper()
	var mu sync.Mutex
	rowOf := map[gorm.ConnPool]uint{}
	log := map[uint][]string{}
	app.hooks.batchRowClaimed = func(tx *gorm.DB, rowID uint) {
		mu.Lock()
		rowOf[tx.Statement.ConnPool] = rowID
		mu.Unlock()
	}
	rec := func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		if id, ok := rowOf[tx.Statement.ConnPool]; ok {
			log[id] = append(log[id], tx.Statement.SQL.String())
		}
	}
	name := "test:record-statements"
	gormMust(t, &gorm.DB{Error: app.DB.Callback().Query().After("gorm:query").Register(name, rec)})
	gormMust(t, &gorm.DB{Error: app.DB.Callback().Row().After("gorm:row").Register(name, rec)})
	t.Cleanup(func() {
		_ = app.DB.Callback().Query().Remove(name)
		_ = app.DB.Callback().Row().Remove(name)
	})
	return func() map[uint][]string {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(log)
	}
}

// wantOneFactsStatementPerRow: after each claimed row's choice read, its
// transaction's only read is the one facts statement. rows is how many
// claimed rows the log must show.
func wantOneFactsStatementPerRow(t *testing.T, log map[uint][]string, rows int) {
	t.Helper()
	if len(log) != rows {
		t.Fatalf("the log shows %d claimed rows, want %d", len(log), rows)
	}
	for id, stmts := range log {
		choice := slices.IndexFunc(stmts, func(s string) bool {
			return strings.Contains(s, "email_address_optouts") || strings.Contains(s, "FROM consents")
		})
		if choice < 0 {
			t.Fatalf("row %d read no choice: %q", id, stmts)
		}
		after := stmts[choice+1:]
		if len(after) != 1 || !strings.Contains(after[0], "named_occ AS") {
			t.Fatalf("row %d: after its choice it read %q, want only the facts statement", id, after)
		}
	}
}

// TestBatchEmailCountsFromTheCutoffAsItStandsNow: a series announcement went
// out at T0, and the reader's only date in it started after T0 but before
// now. A worker loads the message (cutoff T0), and before its row is decided
// a Try again moves the cutoff to now on the same rows. The row is decided
// from the message as the facts statement reads it, so that date no longer
// counts and nothing is sent; the control, on a later date, is sent.
func TestBatchEmailCountsFromTheCutoffAsItStandsNow(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	addMember(t, f.app, f.reader, &f.group, models.MembershipMember)
	now := time.Now().UTC()
	started := now.Add(-30 * time.Minute)
	f.setDate(t, f.occs[0], "starts_at", started)
	f.register(t, f.reader, f.occs[0])
	f.register(t, f.leaderU, f.occs[1])
	id, ids := seedSending(t, f.app, f.adminU, f.seriesAudience(), toRow(f.reader), toRow(f.leaderU))
	t0 := now.Add(-time.Hour)
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("scheduled_at", t0))
	var moved atomic.Bool
	f.app.hooks.batchRowClaimed = func(_ *gorm.DB, rowID uint) {
		if moved.CompareAndSwap(false, true) {
			gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("scheduled_at", time.Now().UTC()))
		}
	}
	s := &deliverySender{}
	if err := deliverRows(f.app, s, id, ids); err != nil {
		t.Fatal(err)
	}
	wantNotSent(t, "the reader whose date started before the new cutoff", f.app, s, f.reader.Email, ids[0], skipNoLongerOnList)
	if s.sentTo(f.leaderU.Email) != 1 {
		t.Fatalf("the control was sent %d emails", s.sentTo(f.leaderU.Email))
	}
}

// TestBatchEmailHoldsNoDateOrPersonLockOverTheProvider: while a dated row's
// email is held at the provider, the real guarded edit of its date and a
// change to its person's membership both complete, and a second worker on
// the same row skips it rather than waiting. Only the row's own claim is
// held across the call.
func TestBatchEmailHoldsNoDateOrPersonLockOverTheProvider(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.admin.holdNotices = true
	addMember(t, f.app, f.reader, &f.group, models.MembershipMember)
	f.register(t, f.reader, f.occs[0])
	id, ids := seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
	date := models.FormatCalendarDate(f.occs[0].OriginalDate)
	base := editBaseOf(t, f.admin, f.ev.ID, date)
	sender := newBlockingChoiceSender(t, nil)
	f.app.Mailer = sender
	done := make(chan error, 1)
	go func() {
		done <- f.app.deliverMessageBatchOnce(context.Background(), messageBatchJob{MessageID: id, RecipientIDs: ids})
	}()
	awaitDBSignal(t, sender.entered)
	within := func(label string, fn func()) {
		t.Helper()
		finished := make(chan struct{})
		go func() { fn(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s waited on the held send", label)
		}
	}
	within("the guarded date edit", func() {
		if w := postDateEdit(f.admin, f.ev.ID, date, dateEditBody(newDateAttempt(t), base.Revision, map[string]any{"title": "Renamed while held"})); w.Code != http.StatusOK {
			t.Errorf("the edit: %d %s", w.Code, w.Body.String())
		}
	})
	within("the membership change", func() { f.setMembership(t, f.reader, models.MembershipLeft) })
	within("a second worker", func() {
		if err := f.app.deliverMessageBatchOnce(context.Background(), messageBatchJob{MessageID: id, RecipientIDs: ids}); err != nil {
			t.Errorf("the second worker: %v", err)
		}
	})
	if n := len(sender.sent()); n != 1 {
		t.Fatalf("the provider was called %d times while the row was held", n)
	}
	sender.resume()
	if err := awaitDBResult(t, done); err != nil {
		t.Fatal(err)
	}
	var row models.MessageRecipient
	gormMust(t, f.app.DB.First(&row, ids[0]))
	if row.Status != models.RecipientSent {
		t.Fatalf("the held row: %+v", row)
	}
}

// TestBatchEmailHonorsARevocationBeforeItsFactsStatement: the departed person
// may still see their kept date when the row is claimed and the choice read.
// Their grant is ended in a commit between the choice and the facts
// statement: the statement sees it, so nothing goes to them.
func TestBatchEmailHonorsARevocationBeforeItsFactsStatement(t *testing.T) {
	f := newDatedBatchFixture(t, false)
	id, rows := seedDatedSending(t, f.app, f.issuer, f.target, false, f.subjectRow(), toRow(f.control))
	newMutableClock(f.app, f.target.StartsAt.Add(-time.Hour))
	fired := 0
	f.app.hooks.batchChoiceRead = func(rowID uint) {
		if rowID == rows[0].ID {
			fired++
			endSeededAccess(t, f.app.DB, f.access.RegistrationID, policy.OccurrenceDeclined, &f.recipient.ID, f.app.now().UTC())
		}
	}
	s := &deliverySender{}
	if err := deliverRows(f.app, s, id, []uint{rows[0].ID, rows[1].ID}); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("the seam fired %d times", fired)
	}
	wantNotSent(t, "the revoked person", f.app, s, rows[0].Address, rows[0].ID, skipCantSeeDate)
	if s.sentTo(f.control.Email) != 1 {
		t.Fatalf("the control was sent %d emails", s.sentTo(f.control.Email))
	}
}

// TestBatchEmailReachesAHouseholdHolderOnlyThroughADateTheyMaySee: a parent
// reaches a date's and a series' audience only through the registration they
// made for their child (DEC-8). Through the real batch each row is sent while
// the date is open to the parent, and suppressed once it is closed to them:
// the child's registration lends the parent no grant.
func TestBatchEmailReachesAHouseholdHolderOnlyThroughADateTheyMaySee(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	parent := newTestUser(t, f.app, "parent", "password1234", models.RoleMember)
	household := models.Household{Name: "Parent family", CreatedBy: parent.ID}
	gormMust(t, f.app.DB.Create(&household))
	child := models.HouseholdPerson{HouseholdID: household.ID, FirstName: "Kid", Kind: models.HouseholdChild, CreatedBy: parent.ID}
	gormMust(t, f.app.DB.Create(&child))
	gormMust(t, f.app.DB.Create(&models.Registration{OccurrenceID: f.occs[0].ID, HouseholdPersonID: &child.ID, RegisteredBy: &parent.ID, Status: models.RegGoing}))
	for _, closed := range []bool{false, true} {
		if closed {
			f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly)
		}
		for _, au := range []audienceJSON{f.occAudience(f.occs[0]), f.seriesAudience()} {
			label := fmt.Sprintf("closed=%v %s", closed, au.Type)
			id, ids := seedSending(t, f.app, f.leaderU, au, toRow(parent))
			s := &deliverySender{}
			if err := deliverRows(f.app, s, id, ids); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			if closed {
				wantNotSent(t, label, f.app, s, parent.Email, ids[0], skipCantSeeDate)
			} else if s.sentTo(parent.Email) != 1 {
				t.Fatalf("%s: the parent was sent %d emails", label, s.sentTo(parent.Email))
			}
		}
	}
}
