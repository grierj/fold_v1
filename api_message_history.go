package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"fold/internal/audit"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A sent message's history and "Try again" (M3.5b, PLAN 7.1, DEC-131).
// The history is the message_recipients snapshot in counts and names, the
// same privacy rule as the composer's preview: counts by status and reason
// for everyone, each person's status in words for the author, each
// person's own reason only for an admin, and never an address (DEC-31).

// recipientCountsJSON counts the people a message was for, by status;
// guardian copies are counted apart (DEC-132). Skipped is the suppressed
// rows (by reason in skippedBy); Teens counts the teens it was for, not
// skipped (DEC-20). GuardianCopies counts the copies, and
// GuardianCopiesMissed the ones that didn't go out (skipped, bounced or
// failed); GuardianCopiesFailed are the failed ones, which Try again sends.
type recipientCountsJSON struct {
	Sent                 int `json:"sent"`
	Delivered            int `json:"delivered"`
	Bounced              int `json:"bounced"`
	Complained           int `json:"complained"`
	Failed               int `json:"failed"`
	Skipped              int `json:"skipped"`
	Queued               int `json:"queued"`
	Teens                int `json:"teens"`
	GuardianCopies       int `json:"guardianCopies"`
	GuardianCopiesMissed int `json:"guardianCopiesMissed"`
	GuardianCopiesFailed int `json:"guardianCopiesFailed"`
	// Push counts the companion pushes (M3.13d2), apart from the email:
	// they never change the counts above.
	Push pushCountsJSON `json:"push"`
	// Every email row and every failed one, copies included: what "Try
	// again" works from. A push is never sent again.
	rows, failedRows int
}

// pushCountsJSON counts an announcement's push rows (one per person
// however many devices): sent, still waiting, and the rest by reason
// (skipped or failed): "38 pushed · 2 waiting until 8:00 AM · 1 device
// gone". Waiting is by when quiet hours end on each person's clock ("" for
// pushes about to go).
type pushCountsJSON struct {
	Sent    int            `json:"sent"`
	Waiting []pushWaitJSON `json:"waiting"`
	Missed  map[string]int `json:"missed"`
}

type pushWaitJSON struct {
	// Until is "8:00 AM" on the recipient's clock, or "" for pushes about
	// to go out.
	Until string `json:"until"`
	N     int    `json:"n"`
}

// Each person's status as the history lists it. A leader or host can't tell
// a bounce, a complaint, a failure or a skip apart: all are "missed".
const (
	historySent      = "sent"
	historyDelivered = "delivered"
	historyMissed    = "missed"
	historyWaiting   = "waiting"
)

// historyRecipientJSON is one row: a name and a Teen badge, the status,
// and for an admin why they missed it. CopyOf is the teen's name on a
// guardian copy (DEC-19).
type historyRecipientJSON struct {
	Name   string `json:"name"`
	Teen   bool   `json:"teen"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	CopyOf string `json:"copyOf,omitempty"`
	// Push is the person's companion push (M3.13d2), when they had one:
	// shown on their row, so the list counts people, not rows.
	Push *historyPushJSON `json:"push,omitempty"`
}

// historyPushJSON is one person's push: sent, waiting (until when quiet
// hours end on their clock, for everyone who may see the history, as the
// counts say it) or missed, with the reason for an admin.
type historyPushJSON struct {
	Status string `json:"status"`
	Until  string `json:"until,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// messageRecipientsJSON is GET /messages/:id/recipients.
type messageRecipientsJSON struct {
	Counts    recipientCountsJSON `json:"counts"`
	SkippedBy map[string]int      `json:"skippedBy"`
	// Recipients are the first 200 by name.
	Recipients []historyRecipientJSON `json:"recipients"`
	More       bool                   `json:"more"`
	// CanRetry shows "Try again": some sends failed and the author may
	// still write to these people.
	CanRetry bool `json:"canRetry"`
}

const maxHistoryRecipients = 200

var (
	errMessageNothingToRetry = apiErr(http.StatusConflict, "message_nothing_to_retry", "there's nothing to send again; everyone it could reach got it")
	errMessageRetryRefused   = apiErr(http.StatusForbidden, "message_retry_refused", "its author can no longer write to these people, so it can't be sent again")
	errMessageRetrySending   = apiErr(http.StatusConflict, "message_retry_sending", "it's already sending again")
)

func (a *App) recipientCounts(id uint) (recipientCountsJSON, map[string]int, error) {
	return recipientCountsIn(a.DB, id)
}

// recipientCountsIn is recipientCounts read on db.
func recipientCountsIn(db *gorm.DB, id uint) (recipientCountsJSON, map[string]int, error) {
	var groups []struct {
		Push   bool
		Status string
		Reason string
		Teen   bool
		Copy   bool
		N      int
	}
	// A push row's reason is kept whatever its status (why it missed, or
	// when it waits until); an email row's only when it was skipped.
	err := db.Raw(`SELECT r.channel = ? AS push, r.status,
		CASE WHEN r.channel = ? OR (r.status = ? AND r.copy_of_user_id IS NULL) THEN r.reason ELSE '' END AS reason,
		(u.account_type = ? AND r.copy_of_user_id IS NULL) AS teen, r.copy_of_user_id IS NOT NULL AS copy, count(*) AS n
		FROM message_recipients r JOIN users u ON u.id = r.user_id
		WHERE r.message_id = ? GROUP BY 1, 2, 3, 4, 5`, models.DeliveryPush, models.DeliveryPush, models.RecipientSuppressed, models.AccountTeen, id).Scan(&groups).Error
	c := recipientCountsJSON{Push: pushCountsJSON{Waiting: []pushWaitJSON{}, Missed: map[string]int{}}}
	by := map[string]int{}
	waiting := map[string]int{}
	for _, g := range groups {
		if g.Push {
			switch g.Status {
			case models.RecipientSent:
				c.Push.Sent += g.N
			case models.RecipientQueued:
				waiting[pushWaitUntil(g.Reason)] += g.N
			default:
				c.Push.Missed[g.Reason] += g.N
			}
			continue
		}
		c.rows += g.N
		if g.Status == models.RecipientFailed {
			c.failedRows += g.N
		}
		if g.Copy {
			c.GuardianCopies += g.N
			switch g.Status {
			case models.RecipientSuppressed, models.RecipientBounced, models.RecipientFailed:
				c.GuardianCopiesMissed += g.N
			}
			if g.Status == models.RecipientFailed {
				c.GuardianCopiesFailed += g.N
			}
			continue
		}
		switch g.Status {
		case models.RecipientSent:
			c.Sent += g.N
		case models.RecipientDelivered:
			c.Delivered += g.N
		case models.RecipientBounced:
			c.Bounced += g.N
		case models.RecipientComplained:
			c.Complained += g.N
		case models.RecipientFailed:
			c.Failed += g.N
		case models.RecipientSuppressed:
			c.Skipped += g.N
			by[g.Reason] += g.N
		default: // queued, and deferred texts
			c.Queued += g.N
		}
		if g.Teen && g.Status != models.RecipientSuppressed {
			c.Teens += g.N
		}
	}
	for until, n := range waiting {
		c.Push.Waiting = append(c.Push.Waiting, pushWaitJSON{Until: until, N: n})
	}
	sort.Slice(c.Push.Waiting, func(i, j int) bool { return c.Push.Waiting[i].Until < c.Push.Waiting[j].Until })
	return c, by, err
}

// historyStatus is a row's status in the history's terms, and for an
// admin why they missed it, in words.
func historyStatus(status, reason, errText string, admin bool) (string, string) {
	switch status {
	case models.RecipientSent:
		return historySent, ""
	case models.RecipientDelivered:
		return historyDelivered, ""
	case models.RecipientQueued, models.DeliveryDeferred:
		return historyWaiting, ""
	}
	if !admin {
		return historyMissed, ""
	}
	return historyMissed, whyFor(status, reason, errText)
}

// pushHistoryStatus is a person's push in the history's terms: sent,
// waiting (until when), or missed with its reason (admins only).
func pushHistoryStatus(status, reason string, admin bool) *historyPushJSON {
	switch status {
	case models.RecipientSent:
		return &historyPushJSON{Status: historySent}
	case models.RecipientQueued:
		// When it goes is no one's private reason: the counts say it too.
		return &historyPushJSON{Status: historyWaiting, Until: pushWaitUntil(reason)}
	}
	h := &historyPushJSON{Status: historyMissed}
	if admin {
		h.Reason = reason
	}
	return h
}

// historyRecipients lists the first rows by name in one query, and says
// whether there are more. Only an admin's list has guardian copies, each
// with its teen; a leader or host never sees a guardian's name (DEC-132).
func historyRecipientsIn(db *gorm.DB, id uint, admin bool) ([]historyRecipientJSON, bool, error) {
	var rows []struct {
		Status          string
		Reason          string
		Error           string
		Copy            bool
		Name            string
		DisplayName     string
		AccountType     string
		TeenName        string
		TeenDisplayName string
		PushStatus      *string
		PushReason      *string
	}
	// One row a person (or guardian copy): the email row, with the
	// person's push row joined on (never a copy's). A push row with no
	// email row beside it (a push reminder's, M3.13d3) is the person's row
	// itself.
	err := db.Raw(`SELECT r.status, r.reason, r.error, r.copy_of_user_id IS NOT NULL AS copy,
		u.name, u.display_name, u.account_type, COALESCE(t.name, '') AS teen_name, COALESCE(t.display_name, '') AS teen_display_name,
		p.status AS push_status, p.reason AS push_reason
		FROM message_recipients r JOIN users u ON u.id = r.user_id LEFT JOIN users t ON t.id = r.copy_of_user_id
		LEFT JOIN message_recipients p ON r.channel <> ? AND p.message_id = r.message_id AND p.user_id = r.user_id AND p.channel = ? AND r.copy_of_user_id IS NULL
		WHERE r.message_id = ? AND (? OR r.copy_of_user_id IS NULL)
		  AND (r.channel <> ? OR NOT EXISTS (SELECT 1 FROM message_recipients e
			WHERE e.message_id = r.message_id AND e.user_id = r.user_id AND e.channel <> ? AND e.copy_of_user_id IS NULL))
		ORDER BY lower(`+models.PublicNameSQL("u")+`), r.id
		LIMIT ?`, models.DeliveryPush, models.DeliveryPush, id, admin, models.DeliveryPush, models.DeliveryPush, maxHistoryRecipients+1).Scan(&rows).Error
	more := len(rows) > maxHistoryRecipients
	if more {
		rows = rows[:maxHistoryRecipients]
	}
	out := make([]historyRecipientJSON, 0, len(rows))
	for _, r := range rows {
		u := models.User{Name: r.Name, DisplayName: r.DisplayName}
		h := historyRecipientJSON{Name: u.PublicName(), Teen: r.AccountType == models.AccountTeen && !r.Copy}
		h.Status, h.Reason = historyStatus(r.Status, r.Reason, r.Error, admin)
		if r.PushStatus != nil {
			reason := ""
			if r.PushReason != nil {
				reason = *r.PushReason
			}
			h.Push = pushHistoryStatus(*r.PushStatus, reason, admin)
		}
		if r.Copy {
			teen := models.User{Name: r.TeenName, DisplayName: r.TeenDisplayName}
			h.CopyOf = teen.PublicName()
		}
		out = append(out, h)
	}
	return out, more, err
}

// retryable: m has people to try again (failed rows on a settled send), or
// failed before anyone was chosen because it couldn't be got ready.
func retryable(m *models.Message, c recipientCountsJSON) bool {
	switch {
	case m.Status != models.MessageSent && m.Status != models.MessageFailed:
		return false
	case c.failedRows > 0:
		return true
	}
	return m.Status == models.MessageFailed && c.rows == 0 && neverPrepared(m.Error)
}

// goingOut: m is sending, so a Try again has already been pressed (or its
// first send is still going). A scheduled message, due or not, has nothing
// to retry: it was never sent, or a retry of one that never got ready is
// about to prepare it.
func goingOut(m *models.Message) bool { return m.Status == models.MessageSending }

// authorMayResend checks again, as message.send does, that m's author is
// active and may still write to its audience: a "Try again" goes out in
// the author's name, whoever presses it. It reads on a.DB, outside the
// retry transaction, so it is only the quick refusal and the source of
// canRetry; apiRetryMessage decides again inside its transaction
// (decideRetryIn), where each failed person is checked against today's
// audience too.
func (a *App) authorMayResend(db *gorm.DB, m *models.Message) (bool, error) {
	if m.AuthorID == nil {
		return false, nil
	}
	var author models.User
	if err := db.First(&author, *m.AuthorID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if author.Status != models.UserActive {
		return false, nil
	}
	var au audienceJSON
	_ = json.Unmarshal([]byte(m.Audience), &au)
	_, err := a.resolveAudienceIn(db, &author, &au, time.Now())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errAudienceRefused), errors.Is(err, errAudienceGone), errors.Is(err, errAudienceInvalid):
		return false, nil
	}
	return false, err
}

// reminderMayResend: an automatic reminder has no author, so a "Try again"
// needs only what reminder.send needs: its date still scheduled and not
// yet started, and its event's reminders still on (who pressed it was
// checked by loadMessage: an admin or a manager of the event). Each person
// is checked again by decideRetryIn, with the date, the switch and who
// pressed it. A push reminder (M3.13d3) is never sent again, as no push is.
func reminderMayResend(db *gorm.DB, m *models.Message) (bool, error) {
	if m.OccurrenceID == nil || reminderKind(m) == reminderPushKind {
		return false, nil
	}
	// An audience that no longer reads as one (a gone audience, as for an
	// announcement) can't say who is still going: nothing goes again.
	var au audienceJSON
	if json.Unmarshal([]byte(m.Audience), &au) != nil || au.normalize() != nil {
		return false, nil
	}
	var occ models.EventOccurrence
	if err := db.First(&occ, *m.OccurrenceID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var ev models.Event
	if err := db.Select("id, reminder_email_minutes").First(&ev, occ.EventID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return ev.ReminderEmailMinutes > 0 && occ.Status == models.OccurrenceScheduled && occ.StartsAt.After(time.Now()), nil
}

// Why a reminder's failed row isn't sent again: its person's answer
// changed since (they cancelled, declined, or moved off or onto the
// waitlist).
const (
	skipNoLongerGoing    = "no longer going"
	skipNoLongerWaitlist = "no longer on the waitlist"
)

// Why an announcement's failed row isn't sent again: its person has left
// the audience message.send would build today, or a guardian copy no
// longer follows from its teen. A copy's reason reaches only an admin's
// per-person history (DEC-132).
const (
	skipNoLongerOnList = "no longer on the list"
	skipNoGuardianCopy = "guardian copy no longer sent"
)

// mayResend is whether m can go out again, and if not, why: an
// announcement's author must still be able to write to its people; a
// reminder's date must still be ahead.
func (a *App) mayResend(db *gorm.DB, m *models.Message) (bool, *apiError, error) {
	if m.Category == models.MessageCategoryReminder {
		ok, err := reminderMayResend(db, m)
		return ok, errReminderRetryRefused, err
	}
	ok, err := a.authorMayResend(db, m)
	return ok, errMessageRetryRefused, err
}

// apiMessageRecipients is a message's history, for whoever may read it
// (loadMessage): its author, an admin, or a reminder's event managers. The
// session's user, their right to the history (and whether they are an admin,
// which shows guardian copies and private reasons), the counts, the names and
// canRetry are read in one read-only snapshot, so a manager who loses the
// event never sees a name written after. The message's existence is judged
// there too: a session revoked after the middleware gets 401 for any id.
func (a *App) apiMessageRecipients(c *gin.Context) {
	id, ok := messageIDParam(c)
	if !ok {
		return
	}
	var out messageRecipientsJSON
	var me *models.User
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if me, err = snapshotViewerIn(tx, apiUser(c)); err != nil || me == nil {
			return err
		}
		var m models.Message
		if err := tx.First(&m, id).Error; err != nil {
			return err
		}
		if allowed, err := messageHistoryAllowedIn(tx, me, &m); err != nil {
			return err
		} else if !allowed {
			return gorm.ErrRecordNotFound
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("message recipients")
		}
		admin := policy.IsAdmin(asPerson(me))
		counts, by, err := recipientCountsIn(tx, m.ID)
		if err != nil {
			return err
		}
		list, more, err := historyRecipientsIn(tx, m.ID, admin)
		if err != nil {
			return err
		}
		out = messageRecipientsJSON{Counts: counts, SkippedBy: by, Recipients: list, More: more}
		if retryable(&m, counts) {
			out.CanRetry, _, err = a.mayResend(tx, &m)
		}
		return err
	}, readSnapshot)
	switch {
	case me == nil && err == nil:
		fail(c, errSecuritySession)
	case errors.Is(err, gorm.ErrRecordNotFound):
		jsonError(c, http.StatusNotFound, "message not found")
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not load who got the message")
	default:
		c.JSON(http.StatusOK, out)
	}
}

// apiRetryMessage is "Try again" (DEC-131): the people a send failed for
// are queued again and sent in fresh batches. Only failed rows go back, so
// nobody it reached gets it twice. A message that failed before anyone
// was chosen is prepared again from the start.
//
// The pre-transaction loadMessage and mayResend are the quick refusal and
// the source of canRetry. The transaction is pinned to Read Committed and
// locks the presser's users row, then the messages row, then takes the
// daily-limit count; everything that then decides (who pressed it, the
// author's right to the audience or the reminder's date and switch, and
// which failed people message.send would still reach) comes from one later
// statement (decideRetryIn), so none of it straddles a commit. A change
// committed after that statement may not stop this retry's batches; each
// batch row decides its date again, and the next retry checks everything.
func (a *App) apiRetryMessage(c *gin.Context) {
	m, ok := a.loadMessage(c)
	if !ok {
		return
	}
	counts, _, err := a.recipientCounts(m.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not send the message again")
		return
	}
	if goingOut(m) {
		fail(c, errMessageRetrySending)
		return
	}
	if !retryable(m, counts) {
		fail(c, errMessageNothingToRetry)
		return
	}
	if may, why, err := a.mayResend(a.DB, m); err != nil {
		jsonError(c, http.StatusInternalServerError, "could not send the message again")
		return
	} else if !may {
		fail(c, why)
		return
	}
	me := apiUser(c)
	now := time.Now()
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		// The actor's users row first: for a non-admin announcement press it is
		// the author's, so the daily-limit count below cannot interleave with
		// another schedule or retry by the same author (#260).
		freshActor, err := lockActorIn(tx, me, actorNoKeyUpdate)
		if err != nil {
			return err
		}
		var cur models.Message
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id, status, updated_at").First(&cur, m.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errLocked
			}
			return err
		}
		// The counts and the branch choice were read from this snapshot, so any
		// change since, by another retry or by message.send, sends the press
		// down the existing errLocked path.
		if cur.Status != m.Status || !cur.UpdatedAt.Equal(m.UpdatedAt) {
			return errLocked
		}
		if m.Category != models.MessageCategoryReminder {
			var au audienceJSON
			_ = json.Unmarshal([]byte(m.Audience), &au)
			if full, err := a.dailyLimitReached(tx, freshActor, m.ID, au, now); err != nil {
				return err
			} else if full {
				return errMessageRetryDailyLimit
			}
			if a.hooks.messageLimitAfterCount != nil {
				a.hooks.messageLimitAfterCount(tx)
			}
		}
		if a.hooks.retryLocked != nil {
			a.hooks.retryLocked(tx)
		}
		// The failed rows' ids and people (fixed once written), then one
		// statement for everything that decides (decideRetryIn).
		var failed []models.MessageRecipient
		if counts.failedRows > 0 {
			if err := tx.Select("id, user_id, copy_of_user_id").Where("message_id = ? AND status = ? AND channel = ?", m.ID, models.RecipientFailed, models.DeliveryEmail).
				Order("id").Find(&failed).Error; err != nil {
				return err
			}
		}
		decision, err := a.decideRetryIn(tx, freshActor, m, failed, a.now().UTC())
		if err != nil {
			return err
		}
		// The claim holds only if nobody else retried it meanwhile.
		claim := func(fields map[string]any) error {
			res := tx.Model(&models.Message{}).Where("id = ? AND status = ? AND updated_at = ?", m.ID, m.Status, m.UpdatedAt).Updates(fields)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return errLocked
			}
			return nil
		}
		ids := decision.keep
		if counts.failedRows == 0 {
			if err := claim(map[string]any{"status": models.MessageScheduled, "scheduled_at": now, "error": "", "updated_at": now}); err != nil {
				return err
			}
			if err := enqueueMessageSend(tx, m.ID, now); err != nil {
				return err
			}
		} else {
			// An announcement's retry is charged to the church day it goes out
			// on, as the never-prepared branch above already does.
			fields := map[string]any{"status": models.MessageSending, "error": "", "updated_at": now}
			if m.Category != models.MessageCategoryReminder {
				fields["scheduled_at"] = now
			}
			if err := claim(fields); err != nil {
				return err
			}
			mark := func(list []uint, fields map[string]any) error {
				return eachIDChunk(list, func(chunk []uint) error {
					return tx.Model(&models.MessageRecipient{}).Where("id IN ? AND status = ?", chunk, models.RecipientFailed).Updates(fields).Error
				})
			}
			for why, list := range decision.drop {
				if err := mark(list, map[string]any{"status": models.RecipientSuppressed, "reason": why, "error": "", "updated_at": now}); err != nil {
					return err
				}
			}
			// Email only: a push is never sent again (M3.13d2).
			if err := mark(ids, map[string]any{"status": models.RecipientQueued, "error": "", "updated_at": now}); err != nil {
				return err
			}
			if len(ids) == 0 {
				// Nobody left to send to: it settles as it now stands.
				if err := a.settleMessage(tx, m.ID); err != nil {
					return err
				}
			} else if err := enqueueBatches(tx, m.ID, ids, "retry"+strconv.FormatInt(now.UnixNano(), 36)); err != nil {
				return err
			}
		}
		return audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.MessageRetried, TargetType: "message", TargetID: m.ID,
			Meta: map[string]any{"people": len(ids), "byOther": m.AuthorID == nil || *m.AuthorID != me.ID}, IP: c.ClientIP()})
	}, readCommitted)
	if errors.Is(err, errRetryNotShown) {
		jsonError(c, http.StatusNotFound, "message not found")
		return
	}
	if errors.Is(err, errLocked) {
		// Someone else pressed it first.
		if a.DB.First(m, m.ID).Error == nil && goingOut(m) {
			fail(c, errMessageRetrySending)
		} else {
			fail(c, errMessageNothingToRetry)
		}
		return
	}
	var ae *apiError
	if errors.As(err, &ae) {
		fail(c, err)
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not send the message again")
		return
	}
	a.respondMessage(c, http.StatusOK, m)
}
