package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The send confirmation names the list the sender reviewed (DEC-169). Send
// now and Schedule first ask for a review: the summary the picker shows, and a
// sealed token of who it reaches (audienceReach), bound to the sender, the
// message and its revision. The confirmation sends the token back; under the
// message's lock the server works out who it reaches again and schedules only
// when that is the same set. Otherwise it answers what changed and a fresh
// review. A scheduled message is never checked again when it goes out: it
// goes to the list as it stands then.

var (
	// errMessageListChanged: who the message reaches changed between the review
	// and the confirmation. The answer carries the change and a fresh review.
	errMessageListChanged = apiErr(http.StatusConflict, "message_list_changed", "who gets this message changed since you looked")
	// errMessageReviewNeeded: the confirmation's review is not one this message,
	// this sender and this text can use. The answer carries a fresh review.
	errMessageReviewNeeded = apiErr(http.StatusConflict, "message_review_needed", "check who gets this message before it goes")
)

// messageReviewPurpose names the review token's key and binding (deriveKey).
const messageReviewPurpose = "message-review:v1"

// maxMessageReviewToken bounds a token before it is decoded: an id costs at
// most ten bytes of the sealed list.
const maxMessageReviewToken = 64 << 10

func (a *App) messageReviewCipher() (cipher.AEAD, error) {
	key, err := deriveKey(a.Config.SessionSecret, messageReviewPurpose)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// messageReviewBinding is what a token is good for: one sender, one message,
// one revision of it. A revision carries the process generation, so a token
// from before a restart never opens.
func messageReviewBinding(actorID, messageID uint, revision string) []byte {
	return []byte(messageReviewPurpose + "|" + strconv.FormatUint(uint64(actorID), 10) + "|" + strconv.FormatUint(uint64(messageID), 10) + "|" + revision)
}

// sealMessageReview seals reached (any order) for the binding. The plaintext
// is the ids ascending, each as the uvarint of its gap from the one before.
func (a *App) sealMessageReview(reached []uint, actorID, messageID uint, revision string) (string, error) {
	ids := slices.Clone(reached)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	plain := make([]byte, 0, len(ids)*3)
	var last uint64
	for _, id := range ids {
		plain = binary.AppendUvarint(plain, uint64(id)-last)
		last = uint64(id)
	}
	crypt, err := a.messageReviewCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, crypt.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(crypt.Seal(nonce, nonce, plain, messageReviewBinding(actorID, messageID, revision))), nil
}

// openMessageReview is the set a token sealed for this binding, ascending;
// false when it does not open for it (another sender, message or revision,
// another key, a garbled or empty token).
func (a *App) openMessageReview(token string, actorID, messageID uint, revision string) ([]uint, bool) {
	if token == "" || len(token) > maxMessageReviewToken {
		return nil, false
	}
	crypt, err := a.messageReviewCipher()
	if err != nil {
		return nil, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(sealed) < crypt.NonceSize()+crypt.Overhead() {
		return nil, false
	}
	plain, err := crypt.Open(nil, sealed[:crypt.NonceSize()], sealed[crypt.NonceSize():], messageReviewBinding(actorID, messageID, revision))
	if err != nil {
		return nil, false
	}
	ids := []uint{}
	var last uint64
	for len(plain) > 0 {
		gap, n := binary.Uvarint(plain)
		if n <= 0 {
			return nil, false
		}
		plain = plain[n:]
		last += gap
		ids = append(ids, uint(last))
	}
	return ids, true
}

// messageReachIn is who m reaches for me now, read on tx: its audience as me
// may message it (resolveAudienceIn) and the rows preparation would write for
// it (audienceReachIn). The errors are resolveAudienceIn's.
func (a *App) messageReachIn(tx *gorm.DB, me *models.User, m *models.Message) (audienceJSON, audienceSummaryJSON, audienceReach, error) {
	var au audienceJSON
	_ = json.Unmarshal([]byte(m.Audience), &au)
	ids, err := a.resolveAudienceIn(tx, me, &au, a.now())
	if err != nil {
		return au, audienceSummaryJSON{}, audienceReach{}, err
	}
	s, reach, err := a.audienceReachIn(tx, ids, policy.IsAdmin(asPerson(me)), m.PushAlert)
	return au, s, reach, err
}

// audienceError is the answer to a resolveAudienceIn error for me
// (answerAudience): only an admin learns a target is gone, and anyone else
// hears what a target that isn't theirs answers, so ids can't be probed. Any
// other error is returned as it is.
func audienceError(me *models.User, err error) error {
	switch {
	case errors.Is(err, errAudienceGone) && policy.IsAdmin(asPerson(me)):
		return errAudienceGone
	case errors.Is(err, errAudienceGone), errors.Is(err, errAudienceRefused):
		return errMessageNotYours
	}
	return err
}

// errMessageNotFound is a message the session may not see, or one that isn't
// there: 404 alike.
var errMessageNotFound = errors.New("message not found")

// ownMessageIn judges m for me as loadOwnMessage does, on facts read on db:
// a message whose history me may not see is not found; one me may see but not
// change is not found, or message_reminder for an automatic reminder. move's
// rule then decides whether its status still allows the action (errLocked).
func ownMessageIn(db *gorm.DB, me *models.User, m *models.Message, move policy.MessageMove) error {
	allowed, err := messageHistoryAllowedIn(db, me, m)
	if err != nil {
		return err
	}
	if !allowed {
		return errMessageNotFound
	}
	reminder := m.Category == models.MessageCategoryReminder
	if !policy.CanEditMessage(asPerson(me), m.AuthorID != nil && *m.AuthorID == me.ID, reminder) {
		if reminder {
			return errMessageReminder
		}
		return errMessageNotFound
	}
	if !move.Allows(m.Status) {
		return errLocked
	}
	return nil
}

// messageReviewIn is a review of message id for the session's user, in one
// read-only snapshot tx: the user, the message, the right to send it, its
// revision, who it reaches and their names, and the token of that set.
func (a *App) messageReviewIn(tx *gorm.DB, session *models.User, id uint) (messageReviewJSON, audienceReach, error) {
	me, err := snapshotViewerIn(tx, session)
	if err != nil {
		return messageReviewJSON{}, audienceReach{}, err
	}
	if me == nil {
		return messageReviewJSON{}, audienceReach{}, errSecuritySession
	}
	var m models.Message
	if err := tx.First(&m, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return messageReviewJSON{}, audienceReach{}, errMessageNotFound
	} else if err != nil {
		return messageReviewJSON{}, audienceReach{}, err
	}
	if err := ownMessageIn(tx, me, &m, policy.MessageSchedule); err != nil {
		return messageReviewJSON{}, audienceReach{}, err
	}
	revision, err := a.messageRevisionIn(tx, m.ID)
	if err != nil {
		return messageReviewJSON{}, audienceReach{}, err
	}
	_, s, reach, err := a.messageReachIn(tx, me, &m)
	if err != nil {
		return messageReviewJSON{}, audienceReach{}, audienceError(me, err)
	}
	token, err := a.sealMessageReview(reach.reached, me.ID, m.ID, revision)
	return messageReviewJSON{Summary: s, Token: token}, reach, err
}

// answerMessageError answers an error of the composer's own routes: a stale
// session, a message that isn't there or isn't the viewer's, one already
// sending or sent, a refusal in words, or a 500 with fallback.
func answerMessageError(c *gin.Context, err error, fallback string) {
	switch {
	case answerStaleSession(c, err):
	case errors.Is(err, errMessageNotFound):
		jsonError(c, http.StatusNotFound, "message not found")
	case errors.Is(err, errLocked):
		fail(c, errMessageLocked)
	case isAPIError(err):
		fail(c, err)
	default:
		jsonError(c, http.StatusInternalServerError, fallback)
	}
}

// apiReviewMessage is the review the send confirmation shows and names
// (DEC-169): who the message reaches, as the picker counts them, and the
// token the confirmation sends back.
func (a *App) apiReviewMessage(c *gin.Context) {
	id, ok := messageIDParam(c)
	if !ok {
		return
	}
	var out messageReviewJSON
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		out, _, err = a.messageReviewIn(tx, apiUser(c), id)
		return err
	}, readSnapshot)
	if err != nil {
		answerMessageError(c, err, "could not check who gets this message")
		return
	}
	c.JSON(http.StatusOK, messageReviewResponseJSON{Review: out})
}

// messageReviewRefusal is a confirmation that did not match: the fresh
// review, and what changed since the one the sender confirmed (nil when the
// token did not open, or the list is back as it was).
type messageReviewRefusal struct {
	review messageReviewJSON
	change *audienceChangeJSON
}

func (r messageReviewRefusal) Error() string { return r.code().Message }

func (r messageReviewRefusal) code() *apiError {
	if r.change == nil {
		return errMessageReviewNeeded
	}
	return errMessageListChanged
}

func (r messageReviewRefusal) apiAnswer() *apiError {
	extra := gin.H{"review": r.review}
	if r.change != nil {
		extra["change"] = r.change
	}
	return r.code().with(extra)
}

// answerReviewRefusal answers a confirmation whose review did not hold, from
// a fresh review in its own snapshot. confirmed is the set the sender
// confirmed, nil when their token did not open.
func (a *App) answerReviewRefusal(c *gin.Context, id uint, confirmed []uint) {
	var refusal messageReviewRefusal
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		review, reach, err := a.messageReviewIn(tx, apiUser(c), id)
		if err != nil {
			return err
		}
		refusal = messageReviewRefusal{review: review}
		if confirmed != nil {
			refusal.change = audienceChange(confirmed, reach)
		}
		return nil
	}, readSnapshot)
	if err != nil {
		answerMessageError(c, err, "could not check who gets this message")
		return
	}
	fail(c, refusal)
}

// audienceChange is who reach adds to confirmed and who it drops, nil when
// neither. A person added is in the audience now, so the summary names them.
// A person dropped is named only while the summary still lists them (still in
// the audience, now skipped); anyone else (gone from the audience, erased, or
// no longer someone the sender reaches by name) is only counted. No reason is
// given for anyone. Each list holds the first maxAudiencePreview names by
// name, and the rest are counted.
func audienceChange(confirmed []uint, reach audienceReach) *audienceChangeJSON {
	was := map[uint]bool{}
	for _, id := range confirmed {
		was[id] = true
	}
	now := map[uint]bool{}
	out := audienceChangeJSON{Added: []string{}, Dropped: []string{}}
	for _, id := range reach.reached {
		now[id] = true
		if !was[id] {
			out.Added = append(out.Added, reach.named[id])
		}
	}
	for _, id := range confirmed {
		if now[id] {
			continue
		}
		if name, ok := reach.named[id]; ok {
			out.Dropped = append(out.Dropped, name)
		} else {
			out.DroppedUnnamed++
		}
	}
	if len(out.Added) == 0 && len(out.Dropped) == 0 && out.DroppedUnnamed == 0 {
		return nil
	}
	out.Added, out.AddedMore = firstNames(out.Added)
	var more int
	out.Dropped, more = firstNames(out.Dropped)
	out.DroppedUnnamed += more
	return &out
}

// firstNames sorts names as the preview does and keeps the first
// maxAudiencePreview, with how many more there were.
func firstNames(names []string) ([]string, int) {
	sort.SliceStable(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	if len(names) > maxAudiencePreview {
		return names[:maxAudiencePreview], len(names) - maxAudiencePreview
	}
	return names, 0
}

// sameReach is whether a and b, both ascending, hold the same people.
func sameReach(a, b []uint) bool { return slices.Equal(a, b) }

// sortedReach is reached ascending, for comparing with a token's set.
func sortedReach(reached []uint) []uint {
	ids := slices.Clone(reached)
	slices.Sort(ids)
	return slices.Compact(ids)
}

// scheduleWhen is when a schedule press asks for: now, or a time from a
// minute ago (taken as now) to a year ahead.
func scheduleWhen(at *time.Time, now time.Time) (time.Time, error) {
	if at == nil {
		return now, nil
	}
	when := at.UTC()
	if when.Before(now.Add(-time.Minute)) || when.After(now.Add(maxScheduleAhead)) {
		return now, errMessageWhen
	}
	if when.Before(now) {
		return now, nil
	}
	return when, nil
}
