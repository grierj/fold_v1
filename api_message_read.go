package main

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"

	"fold/internal/content"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Reading an announcement in Fold (M3.13d2): the page an announcement's
// push opens. A recipient may read what they were sent (policy
// CanReadMessage), and nothing else about it: no audience, counts,
// history or other recipients. Its author and admins may read it too.

// messageReadJSON is GET /messages/:id/read: the message as its reader's
// email said it, merge fields filled with their own values. The event fields
// carry today's values only while, and only as far as, the reader may still
// see the event or date (readerEventValues); otherwise they read empty and a
// link reads as its text.
type messageReadJSON struct {
	ID      uint   `json:"id"`
	Subject string `json:"subject"`
	// From is "Pat Lee via First Church", or the church's name when the
	// author's account is gone (PLAN 7.8).
	From string `json:"from"`
	// SentAt is when it went out (when it started, while it is sending).
	SentAt   *time.Time `json:"sentAt"`
	BodyHTML string     `json:"bodyHtml"`
}

// errMessageNotShown answers a message the reader may not read, or that is
// not there: the same 404 either way.
var errMessageNotShown = errors.New("message not shown")

// isMessageRecipientIn: the person has a direct row (not a guardian copy) in
// any channel for an announcement that is sending or sent, read on db. For an
// event audience the person may read it for good (DEC-135) once a row shows
// they were let in: it was delivered (sent, delivered, bounced or complained),
// or a writer that had just made the audience's date check in one snapshot
// recorded its outcome (audience_admitted_at: preparation, a claimed email
// row, an announcement push), an email skip while they were in the audience
// included. That proof outlasts every later outcome, a later "can't see this
// date anymore" included. A row with no proof that was suppressed because its
// person could not see the date never counts; any other (still queued,
// skipped before or without that check, failed by the last-attempt cleanup or
// the reaper, or written before the marker existed) proves nothing, and they
// may read only while a date they may still see puts them in the audience
// (audienceSeers).
func (a *App) isMessageRecipientIn(db *gorm.DB, m *models.Message, au audienceJSON, userID uint, now time.Time) (bool, error) {
	if m.Status != models.MessageSending && m.Status != models.MessageSent {
		return false, nil
	}
	direct := func() *gorm.DB {
		return db.Model(&models.MessageRecipient{}).Where("message_id = ? AND user_id = ? AND copy_of_user_id IS NULL", m.ID, userID)
	}
	if eventAudience(au.Type) {
		var proven int64
		if err := direct().Where("audience_admitted_at IS NOT NULL OR status IN ?", deliveredStatuses).
			Limit(1).Count(&proven).Error; err != nil || proven > 0 {
			return proven > 0, err
		}
	}
	var rows int64
	if err := direct().Where("NOT (status = ? AND reason = ?)", models.RecipientSuppressed, skipCantSeeDate).
		Limit(1).Count(&rows).Error; err != nil || rows == 0 {
		return false, err
	}
	if !eventAudience(au.Type) {
		return true, nil
	}
	seers, err := a.audienceSeers(db, au, []uint{userID}, messageSince(m, now), now)
	return seers[userID], err
}

// deliveredStatuses are a recipient row's outcomes that mean it reached its
// person (a bounce and a complaint come after delivery).
var deliveredStatuses = []string{models.RecipientSent, models.RecipientDelivered, models.RecipientBounced, models.RecipientComplained}

// messageSince is when a message's series audience counts dates from: its
// scheduled time once it is going out, else now.
func messageSince(m *models.Message, now time.Time) time.Time {
	if m.ScheduledAt != nil && m.ScheduledAt.Before(now) {
		return *m.ScheduledAt
	}
	return now
}

// apiReadMessage answers GET /messages/:id/read. Who may read is
// policy.CanReadMessage; what the event fields hold is readerEventValues.
// Both, with the message, the reader and the sender's name, are read in one
// read-only Repeatable Read snapshot: an entitlement (author, admin, a
// reminder's manager, a recipient) is never paired with event values or a
// grant committed after it was judged.
func (a *App) apiReadMessage(c *gin.Context) {
	id, ok := messageIDParam(c)
	if !ok {
		return
	}
	var out messageReadJSON
	err := a.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = a.readMessageIn(tx, apiUser(c), id)
		return err
	}, readSnapshot)
	switch {
	case errors.Is(err, errSecuritySession):
		fail(c, errSecuritySession)
	case errors.Is(err, errMessageNotShown):
		jsonError(c, http.StatusNotFound, "message not found")
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not load the message")
	default:
		c.JSON(http.StatusOK, messageReadResponseJSON{Message: out})
	}
}

func (a *App) readMessageIn(tx *gorm.DB, session *models.User, id uint) (messageReadJSON, error) {
	me, err := snapshotViewerIn(tx, session)
	if err != nil {
		return messageReadJSON{}, err
	}
	if me == nil {
		return messageReadJSON{}, errSecuritySession
	}
	var m models.Message
	if err := tx.Where("id = ?", id).Limit(1).Find(&m).Error; err != nil {
		return messageReadJSON{}, err
	}
	if m.ID == 0 {
		return messageReadJSON{}, errMessageNotShown
	}
	now := a.now().UTC()
	var au audienceJSON
	_ = json.Unmarshal([]byte(m.Audience), &au)
	p := asPerson(me)
	isAuthor := m.AuthorID != nil && *m.AuthorID == me.ID
	reminder := m.Category == models.MessageCategoryReminder
	manages, recipient := false, false
	if reminder && !p.Admin && m.OccurrenceID != nil {
		if manages, err = managesOccurrenceEventIn(tx, me, *m.OccurrenceID); err != nil {
			return messageReadJSON{}, err
		}
	}
	if !reminder && !p.Admin && !isAuthor {
		if recipient, err = a.isMessageRecipientIn(tx, &m, au, me.ID, now); err != nil {
			return messageReadJSON{}, err
		}
	}
	// Anyone else hears what a message that isn't there answers.
	if !policy.CanReadMessage(p, isAuthor, reminder, manages, recipient) {
		return messageReadJSON{}, errMessageNotShown
	}
	// The event fields are filled per field, by what this reader may see of
	// the event or date today. Nobody is exempt: the author, an admin and a
	// reminder's manager get them only as far as they can see the event.
	vals, err := a.readerEventValues(tx, me, au, now)
	if err != nil {
		return messageReadJSON{}, err
	}
	site := defaultSiteName
	lenientReadIn(tx, func(sp *gorm.DB) {
		if name, err := siteNameIn(sp); err == nil {
			site = name
		}
	})
	from, err := messageFromName(tx, &m, site)
	if err != nil {
		return messageReadJSON{}, err
	}
	sentAt := m.SentAt
	if sentAt == nil {
		sentAt = m.ScheduledAt
	}
	// The stored body is sanitized; it is sanitized again once the
	// reader's values are in, so the page gets only the composer's
	// formatting and its own uploaded pictures (no script, style or frame).
	return messageReadJSON{
		ID:       m.ID,
		Subject:  strings.TrimSpace(content.Merge(m.Subject, vals, false)),
		From:     from,
		SentAt:   sentAt,
		BodyHTML: content.SanitizeHTML(content.Merge(unlinkMissingFields(m.BodyHTML, vals), vals, true)),
	}, nil
}

// readerEventValues is the reader's whole merge map for the read page and an
// announcement push, on db, the caller's one handle (both read it inside a
// read-only snapshot); a claimed email row fills the same map from its one
// facts statement (readerEventValuesFrom over memDates). It applies the event
// and date pages' own rules to the reader and never authorizes the message
// (CanReadMessage does that). It takes no authority lock, and a failed read
// is returned, never treated as "not visible".
//
// The reader's identity personalizes the message and finds their
// registration; what they may see is decided for eventViewer(reader), so a
// guest sees what a signed-out visitor sees and holds no exact-date
// invitation.
//
// Access is decided per field:
//   - the series is visible: today's values, as a send gives them;
//   - one date seen on its own (an exact-date invitation, or an override that
//     opens a date of a hidden series): that date's title, its time when the
//     reader's registration is in the audience's statuses, and a link to the
//     date page, never the series link;
//   - a series audience the reader cannot see as a series: the series title
//     alone, through a scheduled date with no title of its own that they can
//     see;
//   - a cancelled date: the series title and link for series viewers only.
//
// Otherwise the event fields stay unset and a link reads as its text.
func (a *App) readerEventValues(db *gorm.DB, reader *models.User, au audienceJSON, now time.Time) (map[string]string, error) {
	return a.readerEventValuesFrom(a.dbDates(db), reader, au, now)
}

// readerEventValuesFrom is readerEventValues on any fact source
// (dateFactSource).
func (a *App) readerEventValuesFrom(src dateFactSource, reader *models.User, au audienceJSON, now time.Time) (map[string]string, error) {
	vals := map[string]string{content.FieldFirstName: firstName(reader.PublicName())}
	switch au.Type {
	case policy.AudienceOccurrence:
		if au.OccurrenceID == 0 {
			return vals, nil
		}
		return a.readerDateValues(src, reader, au, now, vals)
	case policy.AudienceEventSeries:
		if au.EventID == 0 {
			return vals, nil
		}
		return a.readerSeriesValues(src, reader, au, now, vals)
	}
	return vals, nil
}

// eventViewer is who an event or date is authorized for when reader is the
// recipient or reader: nil for a guest (AGENTS.md, C5: a guest is never loaded
// as an actor), who sees only what a signed-out visitor sees. reminderReaders
// decides the same way at preparation.
func eventViewer(reader *models.User) *models.User {
	if reader.Status == models.UserGuest {
		return nil
	}
	return reader
}

func (a *App) readerDateValues(src dateFactSource, reader *models.User, au audienceJSON, now time.Time, vals map[string]string) (map[string]string, error) {
	viewer := src.viewer(reader)
	occs, err := src.occurrences([]uint{au.OccurrenceID})
	if err != nil {
		return nil, err
	}
	if occs[au.OccurrenceID] == nil {
		return vals, nil // the date is gone: the fields stay unset
	}
	occ := *occs[au.OccurrenceID]
	vs, err := src.candidates(viewer, occ.EventID)
	if err != nil {
		return nil, err
	}
	ev := vs.ByID[occ.EventID] // nil when the event or its group is deleted
	if occ.Status != models.OccurrenceScheduled {
		// Neither the event page's dates nor the date page show a cancelled
		// date: series viewers get the series alone, never the date's title.
		if ev != nil && vs.canSeeSeries(viewer, ev) {
			maps.Copy(vals, a.eventFields(messageEvent{ID: ev.ID, Title: ev.Title, TZID: ev.TZID}, nil))
		}
		return vals, nil
	}
	scoped, err := src.scoped(viewer, []uint{occ.ID}, true, now)
	if err != nil {
		return nil, err
	}
	p, ok := a.projectDate(viewer, vs, &occ, scoped[occ.ID])
	if !ok {
		return vals, nil
	}
	d, registered, err := src.registeredDate(occ.EventID, au, reader.ID, now)
	if err != nil {
		return nil, err
	}
	if p.ShowsSeries() {
		f := newMessageEvent(*p.Event, au, occ.TitleOverride)
		maps.Copy(vals, a.eventFields(f, nil))
		if registered {
			vals[content.FieldEventTime] = f.timeText(d.StartsAt)
			vals[content.FieldManageLink] = a.emailLink(occurrenceDatePath(p.Event.ID, models.FormatCalendarDate(d.OriginalDate)))
		}
		return vals, nil
	}
	// The date alone: its own title and page, never the series link.
	vals[content.FieldEventTitle] = effectiveOccurrenceTitle(p.Event, &occ)
	vals[content.FieldManageLink] = a.emailLink(occurrenceDatePath(p.Event.ID, p.DateString()))
	if registered {
		vals[content.FieldEventTime] = newMessageEvent(*p.Event, au, nil).timeText(d.StartsAt)
	}
	return vals, nil
}

func (a *App) readerSeriesValues(src dateFactSource, reader *models.User, au audienceJSON, now time.Time, vals map[string]string) (map[string]string, error) {
	viewer := src.viewer(reader)
	vs, err := src.candidates(viewer, au.EventID)
	if err != nil {
		return nil, err
	}
	ev := vs.ByID[au.EventID]
	if ev == nil {
		return vals, nil // deleted event or group: no candidate and no live invitation
	}
	if vs.canSeeSeries(viewer, ev) {
		f := newMessageEvent(*ev, au, nil)
		maps.Copy(vals, a.eventFields(f, nil))
		d, registered, err := src.registeredDate(ev.ID, au, reader.ID, now)
		if err != nil {
			return nil, err
		}
		if !registered {
			return vals, nil
		}
		// The next date supplies a time and a page only if the reader may see
		// that date, ordinarily or by invitation.
		seen := vs.canSeeDate(viewer, ev, &models.EventOccurrence{EventID: ev.ID, VisibilityOverride: d.VisibilityOverride})
		if !seen {
			scoped, err := src.scoped(viewer, []uint{d.OccurrenceID}, true, now)
			if err != nil {
				return nil, err
			}
			seen = scoped[d.OccurrenceID] != nil
		}
		if seen {
			vals[content.FieldEventTime] = f.timeText(d.StartsAt)
			vals[content.FieldManageLink] = a.emailLink(occurrenceDatePath(ev.ID, models.FormatCalendarDate(d.OriginalDate)))
		}
		return vals, nil
	}
	// A series the reader cannot see: the series title alone, through a
	// scheduled date with no title of its own that they can see. canSeeSeries
	// is canSeeDate with no override, so a hidden series' date is ordinarily
	// visible only through its override: the overrides that would open it for
	// this reader are known without a query, and one LIMIT 1 probe finds a
	// date that has one. Otherwise the invitation fallback pages the untitled
	// dates and validates every page. A date with any title_override, empty
	// included, never supplies the series title.
	open := []string{}
	for _, v := range []string{policy.Public, policy.Private, policy.InviteOnly} {
		if vs.canSeeDate(viewer, ev, &models.EventOccurrence{EventID: ev.ID, VisibilityOverride: &v}) {
			open = append(open, v)
		}
	}
	if len(open) > 0 {
		hit, err := src.untitledOpenDate(ev.ID, open)
		if err != nil {
			return nil, err
		}
		if hit {
			vals[content.FieldEventTitle] = ev.Title
			return vals, nil
		}
	}
	// The existence probe pages this reader's live grant candidates for
	// untitled scheduled dates and validates them with the date page's grant
	// check, so an invalid earlier grant never hides a valid later one.
	hit, err := src.untitledGrant(viewer, ev.ID, now)
	if err != nil {
		return nil, err
	}
	if hit {
		vals[content.FieldEventTitle] = ev.Title
	}
	return vals, nil
}

// linkFieldAnchorRe is a link the editor made to {{event_link}} or
// {{manage_link}}, as the sanitizer stores it.
var linkFieldAnchorRe = regexp.MustCompile(`(?is)<a\b[^>]*\bhref="\{\{\s*(event_link|manage_link)\s*\}\}"[^>]*>(.*?)</a>`)

// unlinkMissingFields keeps the text of a link to a link field that has no
// value for this reader (its event is gone, say), never an empty href.
func unlinkMissingFields(html string, vals map[string]string) string {
	return linkFieldAnchorRe.ReplaceAllStringFunc(html, func(a string) string {
		m := linkFieldAnchorRe.FindStringSubmatch(a)
		if vals[strings.ToLower(m[1])] != "" {
			return a
		}
		return m[2]
	})
}
