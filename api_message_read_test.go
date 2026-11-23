package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/content"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/internal/providers"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// A recipient reads the announcement they got, as their email said it,
// at /messages/:id/read (the page its push opens), and nothing else about
// it: its history and its own page stay its author's. Someone it wasn't
// for, a guardian with only a copy, and anyone before it goes out hear
// that it isn't there (404, as the message's other routes answer).
func TestRecipientReadsTheAnnouncement(t *testing.T) {
	f := newComposeFixture(t)
	parent := newTestUser(t, f.app, "parent", "password1234", models.RoleMember)
	f.app.DB.Create(&models.GuardianLink{GuardianUserID: parent.ID, TeenUserID: &f.teen.ID, Status: models.GuardianConfirmed})
	f.app.DB.Create(&models.Setting{Key: settingGuardianCopies, Value: "true"})
	teen, guardian := loginAPI(t, f.app, "teen", "password1234"), loginAPI(t, f.app, "parent", "password1234")
	opted := loginAPI(t, f.app, "opted", "password1234")

	// A draft has no recipients yet: not even the people it will reach.
	var draft struct {
		Message messageJSON `json:"message"`
	}
	decodeJSON(t, f.leader.do("POST", "/api/v1/messages", map[string]any{"subject": "Draft", "bodyHtml": "<p>Hi</p>", "audience": groupAudience(f)}, true), &draft)
	if w := teen.do("GET", "/api/v1/messages/"+itoa(draft.Message.ID)+"/read", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("a draft: %d", w.Code)
	}

	id := f.scheduleNow(t, f.leader, "Snow day for {{first_name}}", `<p>Hi {{first_name}}, <strong>no meeting</strong>.</p>`, groupAudience(f))
	runMessageJobs(t, f.app)
	w := teen.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("recipient: %d %s", w.Code, w.Body.String())
	}
	var raw struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range raw.Message {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"bodyHtml", "from", "id", "sentAt", "subject"}) {
		t.Fatalf("a recipient gets more than the message: %v", keys)
	}
	var out struct {
		Message messageReadJSON `json:"message"`
	}
	decodeJSON(t, teen.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false), &out)
	first := firstName(f.teen.PublicName())
	if out.Message.Subject != "Snow day for "+first || out.Message.BodyHTML != "<p>Hi "+first+", <strong>no meeting</strong>.</p>" ||
		out.Message.From != f.leaderUser.PublicName()+" via "+f.app.siteName() || out.Message.SentAt == nil {
		t.Fatalf("read: %+v", out.Message)
	}
	// Skipped people were on the list too (they have a row): they may read it.
	if w := opted.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false); w.Code != http.StatusOK {
		t.Fatalf("a skipped recipient: %d", w.Code)
	}
	// Not for them, or only a copy: not there. Neither is the history.
	for name, c := range map[string]*apiClient{"member": f.member, "guardian": guardian, "host": f.host} {
		if w := c.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
	for _, path := range []string{"", "/recipients"} {
		if w := teen.do("GET", "/api/v1/messages/"+itoa(id)+path, nil, false); w.Code != http.StatusNotFound {
			t.Fatalf("a recipient opened %q: %d", path, w.Code)
		}
	}
	// The author and an admin read it too.
	for name, c := range map[string]*apiClient{"author": f.leader, "admin": f.admin} {
		if w := c.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false); w.Code != http.StatusOK {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
}

// A link to {{event_link}} or {{manage_link}} with no value for the reader
// (its event is gone) reads as its text, never an empty href.
func TestReadPageUnlinksMissingLinkFields(t *testing.T) {
	stored := content.SanitizeHTML(`<p>Hi. <a href="{{event_link}}">See the event</a> or <a href="{{ manage_link }}">your sign-up</a>, <a href="https://example.org">this</a>.</p>`)
	got := content.SanitizeHTML(content.Merge(unlinkMissingFields(stored, map[string]string{}), map[string]string{}, true))
	if strings.Contains(got, `href=""`) || !strings.Contains(got, "See the event") || !strings.Contains(got, "your sign-up") || !strings.Contains(got, `href="https://example.org"`) {
		t.Fatalf("no values: %s", got)
	}
	vals := map[string]string{content.FieldEventLink: "https://fold.example/events/3", content.FieldManageLink: "https://fold.example/events/3/dates/2026-11-05"}
	got = content.SanitizeHTML(content.Merge(unlinkMissingFields(stored, vals), vals, true))
	if !strings.Contains(got, `href="https://fold.example/events/3"`) || !strings.Contains(got, `href="https://fold.example/events/3/dates/2026-11-05"`) {
		t.Fatalf("with values: %s", got)
	}
}

// The read page fills a message's event fields only while, and only as far
// as, the reader may still see the event or date (readerEventValues). The
// tests seed the announcement and its recipient row directly (the send path
// is not what they pin), then change what the reader may see and rename the
// event, so a leaked title is told apart from the title they were sent.

const (
	readVisSubject = "{{event_title}} news for {{first_name}}"
	readVisBody    = `<p>{{event_title}} at {{event_time}}. <a href="{{event_link}}">See the event</a> <a href="{{manage_link}}">your sign-up</a></p>`
)

// seedAnnouncement is a sent announcement by author with a recipient row for
// each of to, and returns its id.
func seedAnnouncement(t *testing.T, app *App, author *models.User, au audienceJSON, to ...*models.User) uint {
	t.Helper()
	raw, err := json.Marshal(au)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	m := models.Message{AuthorID: &author.ID, Status: models.MessageSent, Subject: readVisSubject,
		BodyHTML: content.SanitizeHTML(readVisBody), Audience: string(raw), SentAt: &now}
	if err := app.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	for _, u := range to {
		row := models.MessageRecipient{MessageID: m.ID, UserID: u.ID, Channel: models.DeliveryEmail, Address: u.Email, Status: models.RecipientSent}
		if err := app.DB.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return m.ID
}

// readAs reads a message's page as c, expecting 200.
func readAs(t *testing.T, c *apiClient, id uint) messageReadJSON {
	t.Helper()
	w := c.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("read %d: %d %s", id, w.Code, w.Body.String())
	}
	var out struct {
		Message messageReadJSON `json:"message"`
	}
	decodeJSON(t, w, &out)
	return out.Message
}

// readWant is what one reader's page holds. An empty field is "nothing":
// no title, no time, and a link that reads as its text.
// readWant is what a reader is shown of each event field. notSent: an email or
// push of an event audience does not go at all, because no date the person may
// still see puts them in the audience (audienceSeers); offList: because no
// registration of theirs is in it any more.
type readWant struct {
	title, when, event, manage string
	notSent, offList           bool
}

// checkRead asserts each field on its own: the title (subject and body), the
// time, and each link's href, with the link text always kept.
func checkRead(t *testing.T, label string, got messageReadJSON, reader *models.User, w readWant) {
	t.Helper()
	first := firstName(reader.PublicName())
	if want := strings.TrimSpace(w.title + " news for " + first); got.Subject != want {
		t.Errorf("%s: subject %q, want %q", label, got.Subject, want)
	}
	if prefix := "<p>" + w.title + " at " + w.when + ". "; !strings.HasPrefix(got.BodyHTML, prefix) {
		t.Errorf("%s: body %q, want it to start %q", label, got.BodyHTML, prefix)
	}
	for text, url := range map[string]string{"See the event": w.event, "your sign-up": w.manage} {
		if !strings.Contains(got.BodyHTML, text) {
			t.Errorf("%s: the %q text is gone: %s", label, text, got.BodyHTML)
		}
		linked := regexp.MustCompile(`<a href="` + regexp.QuoteMeta(url) + `"[^>]*>` + regexp.QuoteMeta(text) + `</a>`)
		anyLink := regexp.MustCompile(`<a [^>]*>` + regexp.QuoteMeta(text) + `</a>`)
		switch {
		case url != "" && !linked.MatchString(got.BodyHTML):
			t.Errorf("%s: %q should link to %s: %s", label, text, url, got.BodyHTML)
		case url == "" && anyLink.MatchString(got.BodyHTML):
			t.Errorf("%s: %q should read as text: %s", label, text, got.BodyHTML)
		}
	}
}

type readVisFixture struct {
	app             *App
	admin, leader   *apiClient
	adminU, leaderU *models.User
	reader          *models.User
	readerC         *apiClient
	group           models.Group
	ev              eventJSON
	occs            []models.EventOccurrence
}

// newReadVisFixture: a leader's group of the given visibility with a weekly
// event of three dates, and a reader who is not a member (yet).
func newReadVisFixture(t *testing.T, groupVisibility string) readVisFixture {
	t.Helper()
	app := testApp(t)
	f := readVisFixture{app: app}
	f.adminU = newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	f.leaderU = newTestUser(t, app, "leader", "password1234", models.RoleMember)
	f.reader = newTestUser(t, app, "reader", "password1234", models.RoleMember)
	f.group = activeGroup(t, app, f.leaderU, "Vis group", groupVisibility, "")
	f.admin, f.leader = loginAPI(t, app, "admin", "password1234"), loginAPI(t, app, "leader", "password1234")
	f.readerC = loginAPI(t, app, "reader", "password1234")
	f.ev = createFeedEvent(t, f.leader, f.group.ID, "Vis night", feedStart(3), "FREQ=WEEKLY;COUNT=3")
	if err := app.DB.Where("event_id = ?", f.ev.ID).Order("starts_at").Find(&f.occs).Error; err != nil || len(f.occs) != 3 {
		t.Fatal(err, len(f.occs))
	}
	return f
}

func (f readVisFixture) eventURL() string {
	return f.app.emailLink("/events/" + itoa(f.ev.ID))
}

func (f readVisFixture) dateURL(o models.EventOccurrence) string {
	return f.app.emailLink(occurrenceDatePath(f.ev.ID, models.FormatCalendarDate(o.OriginalDate)))
}

func (f readVisFixture) when(o models.EventOccurrence) string {
	return formatWhen(o.StartsAt, eventZone("UTC"))
}

func (f readVisFixture) occAudience(o models.EventOccurrence) audienceJSON {
	return audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: o.ID}
}

func (f readVisFixture) seriesAudience() audienceJSON {
	return audienceJSON{Type: policy.AudienceEventSeries, EventID: f.ev.ID}
}

// register holds the reader's seat on a date without the registration API, so
// no visibility rule cancels it first.
func (f readVisFixture) register(t *testing.T, u *models.User, o models.EventOccurrence) {
	t.Helper()
	if err := f.app.DB.Create(&models.Registration{OccurrenceID: o.ID, UserID: &u.ID, Status: models.RegGoing}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f readVisFixture) rename(t *testing.T, title string) {
	t.Helper()
	if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("title", title).Error; err != nil {
		t.Fatal(err)
	}
}

func (f readVisFixture) setDate(t *testing.T, o models.EventOccurrence, column string, value any) {
	t.Helper()
	if err := f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", o.ID).Update(column, value).Error; err != nil {
		t.Fatal(err)
	}
}

func (f readVisFixture) setMembership(t *testing.T, u *models.User, status string) {
	t.Helper()
	if err := f.app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.group.ID, u.ID).Update("status", status).Error; err != nil {
		t.Fatal(err)
	}
}

// Readers who may see the series get what a send gives them, a live rename
// included: the recipient, the author and an admin. A group announcement
// gets the first name only.
func TestReadPageSeriesViewerSeesLiveValues(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	occ := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	series := seedAnnouncement(t, f.app, f.leaderU, f.seriesAudience(), f.reader)
	group := seedAnnouncement(t, f.app, f.leaderU, audienceJSON{Type: policy.AudienceGroup, GroupID: f.group.ID}, f.reader)
	live := readWant{title: "Vis night", when: f.when(f.occs[0]), event: f.eventURL(), manage: f.dateURL(f.occs[0])}
	for _, id := range []uint{occ, series} {
		checkRead(t, "before the rename", readAs(t, f.readerC, id), f.reader, live)
	}
	f.rename(t, "Renamed night")
	live.title = "Renamed night"
	for _, id := range []uint{occ, series} {
		checkRead(t, "after the rename", readAs(t, f.readerC, id), f.reader, live)
	}
	// The author and an admin hold no seat: no time, and the event's page
	// stands in for their date.
	for name, c := range map[string]*apiClient{"author": f.leader, "admin": f.admin} {
		u := f.leaderU
		if name == "admin" {
			u = f.adminU
		}
		checkRead(t, name, readAs(t, c, series), u, readWant{title: "Renamed night", event: f.eventURL(), manage: f.eventURL()})
	}
	checkRead(t, "group audience", readAs(t, f.readerC, group), f.reader, readWant{})
}

// Leaving an invite-only group ends the reader's sight of its event, and the
// page stops filling event fields; joining again restores them.
func TestReadPageLeavingTheGroupBlanksEventFields(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityInviteOnly)
	addMember(t, f.app, f.reader, &f.group, models.MembershipMember)
	f.register(t, f.reader, f.occs[0])
	ids := []uint{
		seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader),
		seedAnnouncement(t, f.app, f.leaderU, f.seriesAudience(), f.reader),
	}
	live := readWant{title: "Vis night", when: f.when(f.occs[0]), event: f.eventURL(), manage: f.dateURL(f.occs[0])}
	for _, id := range ids {
		checkRead(t, "a member", readAs(t, f.readerC, id), f.reader, live)
	}
	// A direct status write, as the roster's removal makes it with no date kept.
	f.setMembership(t, f.reader, models.MembershipRemoved)
	getEvent(t, f.readerC, f.ev.ID, http.StatusNotFound)
	f.rename(t, "Renamed night")
	for _, id := range ids {
		checkRead(t, "removed", readAs(t, f.readerC, id), f.reader, readWant{})
	}
	f.setMembership(t, f.reader, models.MembershipActive)
	live.title = "Renamed night"
	for _, id := range ids {
		checkRead(t, "added again", readAs(t, f.readerC, id), f.reader, live)
	}
}

func TestReadPageAfterSendAndLostAccess(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityInviteOnly)
	addMember(t, f.app, f.reader, &f.group, models.MembershipMember)
	f.register(t, f.reader, f.occs[0])
	compose := composeFixture{app: f.app}
	id := compose.scheduleNow(t, f.leader, readVisSubject, readVisBody, map[string]any{"type": "occurrence", "occurrenceId": f.occs[0].ID})
	runMessageJobs(t, f.app)
	var row models.MessageRecipient
	if err := f.app.DB.Where("message_id = ? AND user_id = ? AND channel = ?", id, f.reader.ID, models.DeliveryEmail).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != models.RecipientSent {
		t.Fatalf("recipient was not sent the announcement: %+v", row)
	}
	f.setMembership(t, f.reader, models.MembershipRemoved)
	f.rename(t, "Renamed night")
	checkRead(t, "after delivery and removal", readAs(t, f.readerC, id), f.reader, readWant{})
}

// A private event is open to any signed-in person, so leaving its group is not
// losing sight of it: the gate is the event's visibility, not membership.
func TestReadPagePrivateEventStaysSeenAfterLeaving(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPrivate)
	addMember(t, f.app, f.reader, &f.group, models.MembershipMember)
	f.register(t, f.reader, f.occs[0])
	id := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	f.setMembership(t, f.reader, models.MembershipRemoved)
	f.rename(t, "Renamed night")
	checkRead(t, "removed from a private group", readAs(t, f.readerC, id), f.reader,
		readWant{title: "Renamed night", when: f.when(f.occs[0]), event: f.eventURL(), manage: f.dateURL(f.occs[0])})
}

// Making the event alone invite-only (the group stays public) cancels a
// non-member's seat; the announcements sent before read blank.
func TestReadPageEventTightenedToInviteOnlyBlanks(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	ids := []uint{
		seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader),
		seedAnnouncement(t, f.app, f.leaderU, f.seriesAudience(), f.reader),
	}
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", f.ev.ID),
		confirmVisibility(t, f.admin, fmt.Sprintf("/api/v1/events/%d/visibility", f.ev.ID), map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	f.rename(t, "Renamed night")
	for _, id := range ids {
		checkRead(t, "tightened", readAs(t, f.readerC, id), f.reader, readWant{})
	}
}

// A date opened by its own override on a hidden series is seen on its own:
// its title, its time and its page, never the series link.
func TestReadPageOpenedDateOfHiddenSeries(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityInviteOnly)
	f.setDate(t, f.occs[0], "visibility_override", models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	occ := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	series := seedAnnouncement(t, f.app, f.leaderU, f.seriesAudience(), f.reader)
	f.rename(t, "Renamed night")
	checkRead(t, "the opened date", readAs(t, f.readerC, occ), f.reader,
		readWant{title: "Renamed night", when: f.when(f.occs[0]), manage: f.dateURL(f.occs[0])})
	// Its title is the series' too, while the date has none of its own.
	checkRead(t, "the series, by an untitled date", readAs(t, f.readerC, series), f.reader, readWant{title: "Renamed night"})

	// A date's own title, even an empty one, never lends the series title.
	f.setDate(t, f.occs[0], "title_override", "")
	checkRead(t, "an empty date title", readAs(t, f.readerC, occ), f.reader, readWant{when: f.when(f.occs[0]), manage: f.dateURL(f.occs[0])})
	checkRead(t, "the series, by an empty date title", readAs(t, f.readerC, series), f.reader, readWant{})
	f.setDate(t, f.occs[0], "title_override", "Special night")
	checkRead(t, "a titled date", readAs(t, f.readerC, occ), f.reader, readWant{title: "Special night", when: f.when(f.occs[0]), manage: f.dateURL(f.occs[0])})
	checkRead(t, "the series, by a titled date", readAs(t, f.readerC, series), f.reader, readWant{})
}

// Closing one date of a public series shuts the reader out of that date only.
func TestReadPageOverrideTighteningBlanksOneDate(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly)
	f.register(t, f.reader, f.occs[0])
	occ := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	series := seedAnnouncement(t, f.app, f.leaderU, f.seriesAudience(), f.reader)
	checkRead(t, "the closed date", readAs(t, f.readerC, occ), f.reader, readWant{})
	// The series is still seen; the closed date supplies no time, and the
	// event's page stands in for the date's.
	checkRead(t, "the series", readAs(t, f.readerC, series), f.reader, readWant{title: "Vis night", event: f.eventURL(), manage: f.eventURL()})
}

// An invitation to a closed date of a public series lets its time and page
// through on a series announcement (the scoped fallback).
func TestReadPageSeriesViewerWithAnInvitedDate(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly)
	seedReadInvitation(t, f.app, f.adminU, f.reader, f.ev, f.occs[0])
	series := seedAnnouncement(t, f.app, f.leaderU, f.seriesAudience(), f.reader)
	occ := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	checkRead(t, "the series", readAs(t, f.readerC, series), f.reader,
		readWant{title: "Vis night", when: f.when(f.occs[0]), event: f.eventURL(), manage: f.dateURL(f.occs[0])})
	// Seen on its own, the invited date has no series link.
	checkRead(t, "the invited date", readAs(t, f.readerC, occ), f.reader,
		readWant{title: "Vis night", when: f.when(f.occs[0]), manage: f.dateURL(f.occs[0])})
}

// An exact-date invitation to a hidden series gives the date alone, and ends
// with the invitation or the date.
func TestReadPageExactDateInvitation(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	app, date := f.app, f.dates[1]
	url := func(o models.EventOccurrence) string {
		return app.emailLink(occurrenceDatePath(f.event.ID, models.FormatCalendarDate(o.OriginalDate)))
	}
	when := formatWhen(date.StartsAt, eventZone("UTC"))
	occ := seedAnnouncement(t, app, f.issuer, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: date.ID}, f.recipient)
	series := seedAnnouncement(t, app, f.issuer, audienceJSON{Type: policy.AudienceEventSeries, EventID: f.event.ID}, f.recipient)

	checkRead(t, "the invited date", readAs(t, f.client, occ), f.recipient,
		readWant{title: "Invited date title", when: when, manage: url(date)})
	// The date has a title of its own, so the series title stays blank.
	checkRead(t, "the series, by a titled date", readAs(t, f.client, series), f.recipient, readWant{})

	// Without its own title the date shows the event's, and lends it to the
	// series announcement: no time, no link, even when the invitee's next
	// series registration is on another date they cannot see.
	if err := app.DB.Model(&models.EventOccurrence{}).Where("id = ?", date.ID).Update("title_override", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Create(&models.Registration{OccurrenceID: f.dates[0].ID, UserID: &f.recipient.ID, Status: models.RegGoing}).Error; err != nil {
		t.Fatal(err)
	}
	checkRead(t, "the untitled date", readAs(t, f.client, occ), f.recipient,
		readWant{title: "Hidden series title", when: when, manage: url(date)})
	checkRead(t, "the series, by an untitled date", readAs(t, f.client, series), f.recipient, readWant{title: "Hidden series title"})

	// A cancelled date is blank to someone who cannot see the series.
	if err := app.DB.Model(&models.EventOccurrence{}).Where("id = ?", date.ID).Update("status", models.OccurrenceCancelled).Error; err != nil {
		t.Fatal(err)
	}
	checkRead(t, "the cancelled date", readAs(t, f.client, occ), f.recipient, readWant{})
	if err := app.DB.Model(&models.EventOccurrence{}).Where("id = ?", date.ID).Update("status", models.OccurrenceScheduled).Error; err != nil {
		t.Fatal(err)
	}
	endSeededAccess(t, app.DB, f.access.RegistrationID, policy.OccurrenceDeclined, &f.recipient.ID, time.Now().UTC())
	checkRead(t, "the ended invitation", readAs(t, f.client, occ), f.recipient, readWant{})
	checkRead(t, "the series, after the invitation", readAs(t, f.client, series), f.recipient, readWant{})
}

// A date kept on leaving a group is seen on its own until its access ends.
func TestReadPageKeptDepartureDate(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	date := f.dates[1]
	occ := seedAnnouncement(t, f.app, f.issuer, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: date.ID}, f.recipient)
	checkRead(t, "the kept date", readAs(t, f.client, occ), f.recipient, readWant{
		title:  "Retained date title",
		when:   formatWhen(date.StartsAt, eventZone("UTC")),
		manage: f.app.emailLink(occurrenceDatePath(f.event.ID, models.FormatCalendarDate(date.OriginalDate)))})
	endSeededAccess(t, f.app.DB, f.access.RegistrationID, policy.OccurrenceDeclined, &f.recipient.ID, time.Now().UTC())
	checkRead(t, "the ended date", readAs(t, f.client, occ), f.recipient, readWant{})
}

// A cancelled date shows its series title and link to someone who sees the
// series, never the date's own title or time.
func TestReadPageCancelledDate(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.setDate(t, f.occs[0], "title_override", "Special night")
	f.register(t, f.reader, f.occs[0])
	occ := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	f.setDate(t, f.occs[0], "status", models.OccurrenceCancelled)
	checkRead(t, "a series viewer", readAs(t, f.readerC, occ), f.reader, readWant{title: "Vis night", event: f.eventURL(), manage: f.eventURL()})
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d/visibility", f.ev.ID),
		confirmVisibility(t, f.admin, fmt.Sprintf("/api/v1/events/%d/visibility", f.ev.ID), map[string]any{"visibility": models.VisibilityInviteOnly}), http.StatusOK)
	checkRead(t, "someone who cannot see the series", readAs(t, f.readerC, occ), f.reader, readWant{})
}

// Nobody is exempt: an author who lost the event reads blank, an admin and a
// current manager see it, and a reminder is read only by its managers.
func TestReadPageAuthorAdminAndReminderManager(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityInviteOnly)
	f.register(t, f.reader, f.occs[0])
	authored := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	checkRead(t, "the author, a leader", readAs(t, f.leader, authored), f.leaderU,
		readWant{title: "Vis night", event: f.eventURL(), manage: f.eventURL()})

	reminder := seedAnnouncement(t, f.app, f.adminU, f.occAudience(f.occs[0]))
	if err := f.app.DB.Model(&models.Message{}).Where("id = ?", reminder).
		Updates(map[string]any{"category": models.MessageCategoryReminder, "occurrence_id": f.occs[0].ID}).Error; err != nil {
		t.Fatal(err)
	}
	checkRead(t, "a managing leader's reminder", readAs(t, f.leader, reminder), f.leaderU,
		readWant{title: "Vis night", event: f.eventURL(), manage: f.eventURL()})
	if w := f.readerC.do("GET", "/api/v1/messages/"+itoa(reminder)+"/read", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("a reminder for a non-manager: %d", w.Code)
	}

	// The leader steps down with no date kept: still the author, and blind.
	if err := f.app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.group.ID, f.leaderU.ID).
		Update("status", models.MembershipRemoved).Error; err != nil {
		t.Fatal(err)
	}
	f.rename(t, "Renamed night")
	checkRead(t, "the removed author", readAs(t, f.leader, authored), f.leaderU, readWant{})
	// An admin sees every event, even one in an archived group.
	if err := f.app.DB.Model(&models.Group{}).Where("id = ?", f.group.ID).Update("status", models.GroupArchived).Error; err != nil {
		t.Fatal(err)
	}
	checkRead(t, "an admin", readAs(t, f.admin, authored), f.adminU, readWant{title: "Renamed night", event: f.eventURL(), manage: f.eventURL()})
}

// Church-wide events: a public one reads as ever; a date closed by an
// invite-only override reads blank for its registrant and its host, who see
// no more than anyone, but not for an admin or someone invited to it.
func TestReadPageChurchWideEvent(t *testing.T) {
	f := newComposeFixture(t)
	var occ models.EventOccurrence
	if err := f.app.DB.Where("event_id = ?", f.churchEvent.ID).First(&occ).Error; err != nil {
		t.Fatal(err)
	}
	memberU, adminU := &models.User{}, &models.User{}
	if err := f.app.DB.Where("name = ?", "member").First(memberU).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.app.DB.Where("name = ?", "admin").First(adminU).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.app.DB.Create(&models.Registration{OccurrenceID: occ.ID, UserID: &memberU.ID, Status: models.RegGoing}).Error; err != nil {
		t.Fatal(err)
	}
	when := formatWhen(occ.StartsAt, eventZone("UTC"))
	eventURL := f.app.emailLink("/events/" + itoa(f.churchEvent.ID))
	dateURL := f.app.emailLink(occurrenceDatePath(f.churchEvent.ID, models.FormatCalendarDate(occ.OriginalDate)))
	id := seedAnnouncement(t, f.app, adminU, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: occ.ID}, memberU)
	checkRead(t, "a public church-wide date", readAs(t, f.member, id), memberU, readWant{title: "Picnic", when: when, event: eventURL, manage: dateURL})

	if err := f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occ.ID).Update("visibility_override", models.VisibilityInviteOnly).Error; err != nil {
		t.Fatal(err)
	}
	checkRead(t, "the registrant of a closed date", readAs(t, f.member, id), memberU, readWant{})
	// A host who manages a reminder for the date gets 200 and no event fields.
	reminder := seedAnnouncement(t, f.app, adminU, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: occ.ID})
	if err := f.app.DB.Model(&models.Message{}).Where("id = ?", reminder).
		Updates(map[string]any{"category": models.MessageCategoryReminder, "occurrence_id": occ.ID}).Error; err != nil {
		t.Fatal(err)
	}
	checkRead(t, "the host", readAs(t, f.host, reminder), f.hostUser, readWant{})
	// An admin keeps them (no seat, so the event's page stands in).
	checkRead(t, "an admin", readAs(t, f.admin, id), adminU, readWant{title: "Picnic", event: eventURL, manage: eventURL})
	// Someone invited to the date sees the date alone.
	invitee := newTestUser(t, f.app, "invitee", "password1234", models.RoleMember)
	seedReadInvitation(t, f.app, adminU, invitee, f.churchEvent, occ)
	inviteeID := seedAnnouncement(t, f.app, adminU, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: occ.ID}, invitee)
	checkRead(t, "an invitee", readAs(t, loginAPI(t, f.app, "invitee", "password1234"), inviteeID), invitee,
		readWant{title: "Picnic", when: when, manage: dateURL})
}

// A soft-deleted event (or its group) and a date that is gone read blank, an
// admin included, and never answer 500.
func TestReadPageGoneEventsReadBlank(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	occ := seedAnnouncement(t, f.app, f.leaderU, f.occAudience(f.occs[0]), f.reader)
	series := seedAnnouncement(t, f.app, f.leaderU, f.seriesAudience(), f.reader)
	missing := seedAnnouncement(t, f.app, f.leaderU, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: f.occs[2].ID + 1000}, f.reader)
	checkRead(t, "a missing date", readAs(t, f.readerC, missing), f.reader, readWant{})

	if err := f.app.DB.Exec("UPDATE events SET deleted_at = now() WHERE id = ?", f.ev.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint{occ, series} {
		checkRead(t, "a deleted event", readAs(t, f.readerC, id), f.reader, readWant{})
		checkRead(t, "a deleted event, as admin", readAs(t, f.admin, id), f.adminU, readWant{})
	}
	if err := f.app.DB.Exec("UPDATE events SET deleted_at = NULL WHERE id = ?", f.ev.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.app.DB.Exec("UPDATE groups SET deleted_at = now() WHERE id = ?", f.group.ID).Error; err != nil {
		t.Fatal(err)
	}
	checkRead(t, "a deleted group, as admin", readAs(t, f.admin, series), f.adminU, readWant{})
}

// A failed read inside the check is returned, never read as "not visible".
func TestReaderEventValuesReturnsReadErrors(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	original := f.app.DB
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.app.DB = original.WithContext(ctx)
	t.Cleanup(func() { f.app.DB = original })
	for name, au := range map[string]audienceJSON{"occurrence": f.occAudience(f.occs[0]), "series": f.seriesAudience()} {
		if vals, err := f.app.readerEventValues(f.app.DB, f.reader, au, time.Now().UTC()); err == nil {
			t.Errorf("%s: a failed read answered %v, no error", name, vals)
		}
	}
}

// The hidden-series fallback proves a visible untitled date exists without
// materializing every grant at once: the probe pages the event's untitled
// scheduled dates and validates each page with the date page's own grant
// check. Ended earlier grants never hide a later live one, and with no live
// grant the series title stays blank. The page shows the title alone in both
// cases, never a time or an /events/ href.
func TestReadPageHiddenSeriesManyGrantsInvalidBeforeValid(t *testing.T) {
	app := testApp(t)
	issuer := newTestUser(t, app, "issuer", "password1234", models.RoleAdmin)
	reader := newTestUser(t, app, "reader", "password1234", models.RoleMember)
	admin := loginAPI(t, app, issuer.Name, "password1234")
	readerC := loginAPI(t, app, reader.Name, "password1234")
	group := createFeedGroup(t, admin, "Hidden group many", models.VisibilityInviteOnly, "")
	ev := createFeedEvent(t, admin, group, "Hidden series many", feedStart(7), "FREQ=DAILY;COUNT=55")
	var dates []models.EventOccurrence
	if err := app.DB.Where("event_id = ?", ev.ID).Order("id").Find(&dates).Error; err != nil || len(dates) != 55 {
		t.Fatal(err, len(dates))
	}
	rows := make([]accessRow, 0, len(dates))
	for _, d := range dates {
		rows = append(rows, seedReadInvitation(t, app, issuer, reader, ev, d))
	}
	// Earlier grants remain live but their cancelled registrations fail policy.
	// This forces validation across a full page before the final valid grant.
	for _, r := range rows[:len(rows)-1] {
		if err := app.DB.Model(&models.Registration{}).Where("id = ?", r.RegistrationID).Update("status", models.RegCancelled).Error; err != nil {
			t.Fatal(err)
		}
	}
	series := seedAnnouncement(t, app, issuer, audienceJSON{Type: policy.AudienceEventSeries, EventID: ev.ID}, reader)
	checkRead(t, "invalid grants before the valid one", readAs(t, readerC, series), reader, readWant{title: "Hidden series many"})
	// With no live grant left, the series title goes too: nothing leaks.
	endSeededAccess(t, app.DB, rows[len(rows)-1].RegistrationID, policy.OccurrenceDeclined, &reader.ID, time.Now().UTC())
	checkRead(t, "no valid grant", readAs(t, readerC, series), reader, readWant{})
}

// The read page shows the message's own pictures, by their same-origin
// address, and nothing else an image tag could load (DEC-118).
func TestReadPageShowsOwnPictures(t *testing.T) {
	f := newMessageImageFixture(t)
	id, url := f.pictureDraft(t, "Our picnic table")
	if w := f.leader.do("POST", "/api/v1/messages/"+itoa(id)+"/schedule", map[string]any{}, true); w.Code != http.StatusOK {
		t.Fatalf("schedule: %d %s", w.Code, w.Body.String())
	}
	runMessageJobs(t, f.app)
	teen := loginAPI(t, f.app, "teen", "password1234")
	var out struct {
		Message messageReadJSON `json:"message"`
	}
	w := teen.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false)
	decodeJSON(t, w, &out)
	if w.Code != http.StatusOK || !strings.Contains(out.Message.BodyHTML, `<img src="`+url+`" alt="Our picnic table">`) {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	// Whatever the stored body holds, an outside or blank picture never reaches the page.
	f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("body_html",
		`<p>Hi</p><img src="https://evil.example/x.gif" alt="x"><img src="`+url+`" alt=" ">`)
	decodeJSON(t, teen.do("GET", "/api/v1/messages/"+itoa(id)+"/read", nil, false), &out)
	if strings.Contains(out.Message.BodyHTML, "<img") {
		t.Fatalf("read page body: %q", out.Message.BodyHTML)
	}
}

// beforeFirstStatement runs fn once, before the next query or raw statement
// on db, once armed: for a snapshot, after BEGIN and before the statement that
// takes the snapshot. A change fn commits on another connection is in it.
func beforeFirstStatement(t *testing.T, db *gorm.DB, fn func()) (arm func()) {
	t.Helper()
	var armed atomic.Bool
	name := fmt.Sprintf("test:first_statement:%d", queryCounterSeq.Add(1))
	run := func(*gorm.DB) {
		if armed.CompareAndSwap(true, false) {
			fn()
		}
	}
	if err := db.Callback().Query().Before("gorm:query").Register(name, run); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Raw().Before("gorm:raw").Register(name, run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove(name)
		_ = db.Callback().Raw().Remove(name)
	})
	return func() { armed.Store(true) }
}

// TestReadPageCountsFromTheCutoffTryAgainSet: the read page's clock is read in
// its snapshot, after the message. Try again commits a later cutoff after the
// handler began but before its snapshot's first statement, while the app's
// clock still reads an earlier time (a request that waited for a connection).
// The reader's row is undecided (retried, then left off), and their only date
// starts between that earlier clock and the cutoff, so they may not read it.
// With the earlier clock the page would count from the clock and admit the
// date. The control, with no Try again, reads from the original cutoff.
func TestReadPageCountsFromTheCutoffTryAgainSet(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "between clocks"}[retry], func(t *testing.T) {
			f := newReadVisFixture(t, models.VisibilityPublic)
			date := f.occs[0]
			now := time.Now().UTC()
			early := now.Add(-10 * time.Minute)
			startsAt := early.Add(5 * time.Minute)
			f.setDate(t, date, "starts_at", startsAt)
			f.setDate(t, date, "ends_at", now.Add(30*time.Minute))
			f.register(t, f.reader, date)
			id, emailRows := seedSending(t, f.app, f.leaderU, f.seriesAudience(), toRow(f.reader))
			gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("scheduled_at", now.Add(-2*time.Hour)))
			if retry {
				gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("status", models.MessageSent))
				gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", emailRows[0]).
					Updates(map[string]any{"status": models.RecipientFailed, "error": whyProvider}))
			}
			var pressed atomic.Bool
			f.app.clock = func() time.Time {
				if pressed.Load() {
					return time.Now()
				}
				return early
			}
			arm := beforeFirstStatement(t, f.app.DB, func() {
				if !retry {
					return
				}
				pressed.Store(true)
				if w := f.leader.do("POST", "/api/v1/messages/"+itoa(id)+"/retry", nil, true); w.Code != http.StatusOK {
					t.Errorf("Try again before the read's snapshot: %d %s", w.Code, w.Body.String())
				}
			})
			var reader models.User
			gormMust(t, f.app.DB.First(&reader, f.reader.ID))
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("GET", "/api/v1/messages/"+itoa(id)+"/read", nil)
			c.Params = gin.Params{{Key: "id", Value: itoa(id)}}
			c.Set("apiUser", &reader)
			arm()
			f.app.apiReadMessage(c)
			if !retry {
				if rec.Code != http.StatusOK {
					t.Fatalf("the control's read from the original cutoff: %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if !pressed.Load() {
				t.Fatal("the read never reached its first statement: no Try again was pressed")
			}
			var msg models.Message
			gormMust(t, f.app.DB.First(&msg, id))
			var email models.MessageRecipient
			gormMust(t, f.app.DB.First(&email, emailRows[0]))
			if msg.ScheduledAt == nil || !msg.ScheduledAt.After(startsAt) ||
				email.Status == models.RecipientSent || email.Status == models.RecipientFailed {
				t.Fatalf("Try again's cutoff %v (date %v), email row %+v", msg.ScheduledAt, startsAt, email)
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("an undecided reader whose only date is before Try again's cutoff read it: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// entitlementFixture is a reader Going to a public date and an announcement
// to that date with a literal body, going out with the reader's email row
// queued as preparation leaves it.
type entitlementFixture struct {
	readVisFixture
	id    uint
	email uint
}

const entitlementLiteral = "Only for the people this date let in"

func newEntitlementFixture(t *testing.T) entitlementFixture {
	t.Helper()
	f := entitlementFixture{readVisFixture: newReadVisFixture(t, models.VisibilityPublic)}
	f.register(t, f.reader, f.occs[0])
	var ids []uint
	f.id, ids = seedSending(t, f.app, f.leaderU, f.occAudience(f.occs[0]), toRow(f.reader))
	f.email = ids[0]
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.id).Updates(map[string]any{
		"body_html": "<p>" + entitlementLiteral + "</p>", "body_text": entitlementLiteral}))
	return f
}

// read is the reader's GET of the read page: its status, and whether the
// literal body came back.
func (f entitlementFixture) read(t *testing.T) (int, bool) {
	t.Helper()
	w := f.readerC.do("GET", "/api/v1/messages/"+itoa(f.id)+"/read", nil, false)
	return w.Code, strings.Contains(w.Body.String(), entitlementLiteral)
}

func (f entitlementFixture) optOut(t *testing.T) {
	t.Helper()
	gormMust(t, f.app.DB.Create(&models.Consent{UserID: f.reader.ID, Channel: models.ConsentEmail, Scope: categoryScope(notify.CatMessages),
		Status: models.ConsentOptedOut, Source: models.ConsentSourceWebForm}))
}

func (f entitlementFixture) decline(t *testing.T) {
	t.Helper()
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("user_id = ?", f.reader.ID).Update("status", models.RegDeclined))
}

func (f entitlementFixture) deliverEmail(t *testing.T) models.MessageRecipient {
	t.Helper()
	if err := deliverRows(f.app, &deliverySender{}, f.id, []uint{f.email}); err != nil {
		t.Fatal(err)
	}
	var row models.MessageRecipient
	gormMust(t, f.app.DB.First(&row, f.email))
	return row
}

// wantRead asserts the read page's answer, and that a 200 carries the
// literal body while a 404 carries none of it.
func (f entitlementFixture) wantRead(t *testing.T, label string, want int) {
	t.Helper()
	code, literal := f.read(t)
	if code != want || literal != (want == http.StatusOK) {
		t.Fatalf("%s: read page %d (literal %v), want %d", label, code, literal, want)
	}
}

// TestReadEntitlementNeedsProofTheCheckLetThemIn (DEC-135): for an event
// audience a person may read an announcement for good only once a row shows
// they were delivered it or a writer's date check let them in. Every row here
// is written by the real writer. An eligible opt-out (at the send or at
// preparation), a delivery, and an admitted push keep the message readable
// after the reader declines; an opt-out after the reader lost the date, an
// early push refusal beside an email that could not see the date, the
// reaper's and the last attempt's failures, and a legacy skip or failure
// prove nothing, so the reader reads only while the date still puts them in.
// A legacy delivery stays readable.
func TestReadEntitlementNeedsProofTheCheckLetThemIn(t *testing.T) {
	t.Run("an eligible opt-out at the send", func(t *testing.T) {
		f := newEntitlementFixture(t)
		f.optOut(t)
		if row := f.deliverEmail(t); row.Status != models.RecipientSuppressed || row.Reason != skipOptedOut || row.AudienceAdmittedAt == nil {
			t.Fatalf("the row: %+v", row)
		}
		f.decline(t)
		f.wantRead(t, "after declining", http.StatusOK)
	})
	t.Run("an opt-out after losing the date", func(t *testing.T) {
		f := newEntitlementFixture(t)
		f.optOut(t)
		f.decline(t)
		if row := f.deliverEmail(t); row.Status != models.RecipientSuppressed || row.Reason != skipOptedOut || row.AudienceAdmittedAt != nil {
			t.Fatalf("the row: %+v", row)
		}
		f.wantRead(t, "after declining", http.StatusNotFound)
	})
	t.Run("a delivery", func(t *testing.T) {
		f := newEntitlementFixture(t)
		if row := f.deliverEmail(t); row.Status != models.RecipientSent || row.AudienceAdmittedAt == nil {
			t.Fatalf("the row: %+v", row)
		}
		f.decline(t)
		f.wantRead(t, "after declining", http.StatusOK)
	})
	t.Run("an eligible opt-out at preparation", func(t *testing.T) {
		f := newEntitlementFixture(t)
		f.optOut(t)
		raw, _ := json.Marshal(f.occAudience(f.occs[0]))
		at := time.Now().UTC().Add(-time.Second)
		m := models.Message{AuthorID: &f.leaderU.ID, Status: models.MessageScheduled, Category: models.MessageCategoryAnnouncement,
			Subject: "News", BodyHTML: "<p>" + entitlementLiteral + "</p>", BodyText: entitlementLiteral, Audience: string(raw), ScheduledAt: &at}
		gormMust(t, f.app.DB.Create(&m))
		if err := f.app.prepareMessageOnce(context.Background(), m.ID); err != nil {
			t.Fatal(err)
		}
		var row models.MessageRecipient
		gormMust(t, f.app.DB.Where("message_id = ? AND user_id = ? AND channel = ?", m.ID, f.reader.ID, models.DeliveryEmail).First(&row))
		if row.Status != models.RecipientSuppressed || row.Reason != skipOptedOut || row.AudienceAdmittedAt == nil {
			t.Fatalf("the prepared row: %+v", row)
		}
		f.id = m.ID
		f.decline(t)
		f.wantRead(t, "after declining", http.StatusOK)
	})
	t.Run("an early push refusal beside an email that could not see the date", func(t *testing.T) {
		f := newEntitlementFixture(t)
		useFakePush(t, f.app)
		dev := newPushDevice(t, f.app, f.reader, "phone") // push consent never given
		push := models.MessageRecipient{MessageID: f.id, UserID: f.reader.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
		gormMust(t, f.app.DB.Create(&push))
		f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly)
		if row := f.deliverEmail(t); row.Reason != skipCantSeeDate {
			t.Fatalf("the email row: %+v", row)
		}
		if err := f.app.deliverMessagePush(context.Background(), push.ID, dev.ID, 0); err != nil {
			t.Fatal(err)
		}
		gormMust(t, f.app.DB.First(&push, push.ID))
		if push.Status != models.RecipientSuppressed || push.AudienceAdmittedAt != nil {
			t.Fatalf("the push row: %+v", push)
		}
		f.wantRead(t, "the push refusal proves nothing", http.StatusNotFound)
		f.setDate(t, f.occs[0], "visibility_override", nil)
		f.wantRead(t, "read live once the date shows again", http.StatusOK)
	})
	t.Run("an admitted push", func(t *testing.T) {
		f := newEntitlementFixture(t)
		useFakePush(t, f.app)
		pushConsentRow(t, f.app, f.reader, "all", models.ConsentOptedIn)
		dev := newPushDevice(t, f.app, f.reader, "phone")
		push := models.MessageRecipient{MessageID: f.id, UserID: f.reader.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
		gormMust(t, f.app.DB.Create(&push))
		if err := f.app.deliverMessagePush(context.Background(), push.ID, dev.ID, 0); err != nil {
			t.Fatal(err)
		}
		gormMust(t, f.app.DB.First(&push, push.ID))
		if push.Status != models.RecipientSent || push.AudienceAdmittedAt == nil {
			t.Fatalf("the push row: %+v", push)
		}
		// The email row is still queued: the push alone keeps it readable.
		f.decline(t)
		f.wantRead(t, "after declining", http.StatusOK)
	})
	// A row someone else was already sent keeps the message sent once the
	// reader's row fails.
	sentToOther := func(t *testing.T, f entitlementFixture) {
		now := time.Now().UTC()
		gormMust(t, f.app.DB.Create(&models.MessageRecipient{MessageID: f.id, UserID: f.leaderU.ID, Channel: models.DeliveryEmail,
			Status: models.RecipientSent, Address: f.leaderU.Email, SentAt: &now}))
	}
	t.Run("the reaper's failure", func(t *testing.T) {
		f := newEntitlementFixture(t)
		sentToOther(t, f)
		old := time.Now().UTC().Add(-2 * time.Hour)
		gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.id).Update("updated_at", old))
		if err := f.app.reapMessages(context.Background(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var row models.MessageRecipient
		gormMust(t, f.app.DB.First(&row, f.email))
		if row.Status != models.RecipientFailed || row.AudienceAdmittedAt != nil {
			t.Fatalf("the reaped row: %+v", row)
		}
		f.wantRead(t, "while Going", http.StatusOK)
		f.decline(t)
		f.wantRead(t, "after declining", http.StatusNotFound)
	})
	t.Run("the last attempt's failure", func(t *testing.T) {
		f := newEntitlementFixture(t)
		sentToOther(t, f)
		failDateFacts(t, f.app.DB)
		f.app.Mailer = &deliverySender{}
		if err := f.app.DB.Transaction(func(tx *gorm.DB) error { return enqueueBatches(tx, f.id, []uint{f.email}, "") }); err != nil {
			t.Fatal(err)
		}
		runMessageJobs(t, f.app)
		var row models.MessageRecipient
		gormMust(t, f.app.DB.First(&row, f.email))
		if row.Status != models.RecipientFailed || row.AudienceAdmittedAt != nil {
			t.Fatalf("the failed row: %+v", row)
		}
		f.wantRead(t, "while Going", http.StatusOK)
		f.decline(t)
		f.wantRead(t, "after declining", http.StatusNotFound)
	})
	t.Run("legacy rows", func(t *testing.T) {
		for _, status := range []string{models.RecipientSuppressed, models.RecipientFailed, models.RecipientSent, models.RecipientBounced} {
			f := newEntitlementFixture(t)
			gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", f.email).Updates(map[string]any{"status": status, "reason": skipOptedOut}))
			gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.id).Update("status", models.MessageSent))
			f.decline(t)
			want := http.StatusNotFound
			if status == models.RecipientSent || status == models.RecipientBounced {
				want = http.StatusOK
			}
			f.wantRead(t, "a legacy "+status+" row after declining", want)
		}
	})
}

// failAdmitted leaves f's email row failed after a send whose date check let
// the reader in (the last attempt records the provider's failure after the
// check), and the message failed, ready for Try again. It returns the mark.
func (f entitlementFixture) failAdmitted(t *testing.T) time.Time {
	t.Helper()
	f.leader.holdNotices = true
	if err := deliverRows(f.app, &deliverySender{fail: 1}, f.id, []uint{f.email}); !errors.Is(err, errStopBatch) {
		t.Fatalf("the outage: %v", err)
	}
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", f.email).Updates(map[string]any{"status": models.RecipientFailed, "error": whyProvider}))
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.id).Update("status", models.MessageFailed))
	var row models.MessageRecipient
	gormMust(t, f.app.DB.First(&row, f.email))
	if row.AudienceAdmittedAt != nil {
		t.Fatalf("a stopped attempt recorded an outcome: %+v", row)
	}
	next := f.app.Mailer
	f.app.Mailer = failingSender{}
	gormMust(t, f.app.DB.Model(&models.MessageRecipient{}).Where("id = ?", f.email).Update("status", models.RecipientQueued))
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.id).Update("status", models.MessageSending))
	if err := f.app.DB.Transaction(func(tx *gorm.DB) error { return enqueueBatches(tx, f.id, []uint{f.email}, "") }); err != nil {
		t.Fatal(err)
	}
	runMessageJobs(t, f.app)
	f.app.Mailer = next
	gormMust(t, f.app.DB.First(&row, f.email))
	if row.Status != models.RecipientFailed || row.AudienceAdmittedAt == nil {
		t.Fatalf("the failed row: %+v", row)
	}
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", f.id).Update("status", models.MessageFailed))
	return *row.AudienceAdmittedAt
}

func (f entitlementFixture) tryAgain(t *testing.T) {
	t.Helper()
	if w := f.leader.do("POST", "/api/v1/messages/"+itoa(f.id)+"/retry", nil, true); w.Code != http.StatusOK {
		t.Fatalf("Try again: %d %s", w.Code, w.Body.String())
	}
}

// TestReadEntitlementEvidenceSurvivesLaterOutcomes: the mark a date check
// left on a row is kept by everything that comes later. An email row let in
// and then failed at the provider is queued again by Try again and, once the
// reader has declined, skipped as no longer on the list: the reader still
// reads. The same row skipped because the date was then made private, by the
// batch after Try again or by Try again itself, keeps it too. A row let in,
// sent and then reported bounced keeps it. A push let in and then refused by
// the push service keeps it, and the reader still reads after declining with
// that push row alone.
func TestReadEntitlementEvidenceSurvivesLaterOutcomes(t *testing.T) {
	t.Run("a failed email, Try again, then no longer on the list", func(t *testing.T) {
		f := newEntitlementFixture(t)
		mark := f.failAdmitted(t)
		f.tryAgain(t)
		f.decline(t)
		row := f.deliverEmail(t)
		if row.Status != models.RecipientSuppressed || row.Reason != skipNoLongerOnList || row.AudienceAdmittedAt == nil || !row.AudienceAdmittedAt.Equal(mark) {
			t.Fatalf("after Try again: %+v", row)
		}
		f.wantRead(t, "after declining", http.StatusOK)
	})
	t.Run("a failed email, Try again, then the date made private", func(t *testing.T) {
		f := newEntitlementFixture(t)
		mark := f.failAdmitted(t)
		f.tryAgain(t)
		f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly)
		row := f.deliverEmail(t)
		if row.Status != models.RecipientSuppressed || row.Reason != skipCantSeeDate || row.AudienceAdmittedAt == nil || !row.AudienceAdmittedAt.Equal(mark) {
			t.Fatalf("after Try again: %+v", row)
		}
		f.wantRead(t, "after the date was made private", http.StatusOK)
	})
	t.Run("a failed email whose date is made private before Try again", func(t *testing.T) {
		f := newEntitlementFixture(t)
		mark := f.failAdmitted(t)
		f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly)
		f.tryAgain(t)
		var row models.MessageRecipient
		gormMust(t, f.app.DB.First(&row, f.email))
		if row.Status != models.RecipientSuppressed || row.Reason != skipCantSeeDate || row.AudienceAdmittedAt == nil || !row.AudienceAdmittedAt.Equal(mark) {
			t.Fatalf("after Try again: %+v", row)
		}
		f.wantRead(t, "after the date was made private", http.StatusOK)
	})
	t.Run("a bounce report", func(t *testing.T) {
		f := newEntitlementFixture(t)
		row := f.deliverEmail(t)
		if err := f.app.applyEmailEvent(context.Background(), newEmailEventJob(providers.Event{Kind: providers.EventBounced, MessageID: row.ProviderMessageID, At: time.Now()})); err != nil {
			t.Fatal(err)
		}
		var after models.MessageRecipient
		gormMust(t, f.app.DB.First(&after, f.email))
		if after.Status != models.RecipientBounced || after.AudienceAdmittedAt == nil || !after.AudienceAdmittedAt.Equal(*row.AudienceAdmittedAt) {
			t.Fatalf("after the bounce: %+v", after)
		}
	})
	t.Run("a push refused by the push service", func(t *testing.T) {
		f := newEntitlementFixture(t)
		fp := useFakePush(t, f.app)
		pushConsentRow(t, f.app, f.reader, "all", models.ConsentOptedIn)
		dev := newPushDevice(t, f.app, f.reader, "phone")
		fp.status[dev.path] = http.StatusBadRequest
		push := models.MessageRecipient{MessageID: f.id, UserID: f.reader.ID, Channel: models.DeliveryPush, Status: models.RecipientQueued}
		gormMust(t, f.app.DB.Create(&push))
		_ = f.app.deliverMessagePush(context.Background(), push.ID, dev.ID, 0)
		gormMust(t, f.app.DB.First(&push, push.ID))
		if push.Status == models.RecipientSent || push.Status == models.RecipientQueued || push.AudienceAdmittedAt == nil {
			t.Fatalf("the refused push row: %+v", push)
		}
		// The email row goes nowhere: the reader declines, and it is skipped.
		f.decline(t)
		if row := f.deliverEmail(t); row.Reason != skipNoLongerOnList {
			t.Fatalf("the email row: %+v", row)
		}
		f.wantRead(t, "after declining, by the push's mark", http.StatusOK)
	})
}
