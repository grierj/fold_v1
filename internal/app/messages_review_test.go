package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/internal/policy"
	"fold/models"
)

// reviewAnswer is the body of a review, or of a confirmation the review did
// not hold for.
type reviewAnswer struct {
	Code   string              `json:"code"`
	Review messageReviewJSON   `json:"review"`
	Change *audienceChangeJSON `json:"change"`
}

func takeReview(t *testing.T, c *apiClient, path string) messageReviewJSON {
	t.Helper()
	w := c.do("GET", path+"/review", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("review: %d %s", w.Code, w.Body.String())
	}
	var out messageReviewResponseJSON
	decodeJSON(t, w, &out)
	if out.Review.Token == "" {
		t.Fatalf("a review with no token: %s", w.Body.String())
	}
	return out.Review
}

func revisionOf(t *testing.T, c *apiClient, path string) string {
	t.Helper()
	w := c.do("GET", path, nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("read %s: %d %s", path, w.Code, w.Body.String())
	}
	var out messageResponseJSON
	decodeJSON(t, w, &out)
	if out.Message.Revision == "" {
		t.Fatalf("no revision for %s", path)
	}
	return out.Message.Revision
}

// confirmSend presses the confirmation with revision and the review's token.
func confirmSend(c *apiClient, path, revision, token string, extra map[string]any) *httptest.ResponseRecorder {
	body := map[string]any{"revision": revision, "review": token}
	for k, v := range extra {
		body[k] = v
	}
	return c.do("POST", path+"/schedule", body, true)
}

func decodeReview(t *testing.T, w *httptest.ResponseRecorder) reviewAnswer {
	t.Helper()
	var out reviewAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("answer: %s", w.Body.String())
	}
	return out
}

// stillDraft fails unless the message at path is an unscheduled draft with
// no send job and no schedule record.
func stillDraft(t *testing.T, f composeFixture, path string) {
	t.Helper()
	id := idOfPath(t, path)
	var m models.Message
	gormMust(t, f.app.DB.First(&m, id))
	if m.Status != models.MessageDraft || m.ScheduledAt != nil || jobCountFor(t, f.app, kindMessageSend, id) != 0 || auditCountFor(t, f.app, audit.MessageScheduled, id) != 0 {
		t.Fatalf("a refused send scheduled something: %+v", m)
	}
}

func TestAdminEditAndSendRequireTheAuthorsAudience(t *testing.T) {
	f := newComposeFixture(t)
	aud := map[string]any{"type": "group", "groupId": f.group.ID}
	path := f.draft(t, f.leader, "News", "<p>Hi</p>", aud)
	revision := revisionOf(t, f.admin, path)
	body := map[string]any{"subject": "Office news", "bodyHtml": "<p>Hi</p>", "revision": revision,
		"audience": map[string]any{"type": "group", "groupId": f.otherGroup.ID}}
	wantCode(t, "retarget draft", f.admin.do("PUT", path, body, true), http.StatusForbidden, "message_not_yours")
	if got := revisionOf(t, f.admin, path); got != revision {
		t.Fatal("the refused edit changed the draft")
	}

	body["audience"] = aud
	wantCode(t, "edit within the author's audience", f.admin.do("PUT", path, body, true), http.StatusOK, "")
	revision = revisionOf(t, f.admin, path)
	review := takeReview(t, f.admin, path)
	id := idOfPath(t, path)
	var m models.Message
	gormMust(t, f.app.DB.First(&m, id))
	if m.AuthorID == nil || *m.AuthorID != f.leaderUser.ID {
		t.Fatalf("the edit changed the author: %+v", m)
	}

	addMember(t, f.app, f.hostUser, f.group, models.MembershipLeader)
	demote := "/api/v1/groups/" + itoa(f.group.ID) + "/members/" + itoa(f.leaderUser.ID) + "/role"
	wantCode(t, "demote the author", f.admin.do("PUT", demote, map[string]any{"role": "member"}, true), http.StatusOK, "")
	wantCode(t, "review after the author's demotion", f.admin.do("GET", path+"/review", nil, false), http.StatusForbidden, "message_not_yours")
	wantCode(t, "confirm a prior review after the author's demotion", confirmSend(f.admin, path, revision, review.Token, nil), http.StatusForbidden, "message_not_yours")
	stillDraft(t, f, path)
}

// Two editors of one draft opened the same read: the first save lands, the
// second is refused with nothing written, and each answer carries the token of
// what it left.
func TestMessageEditNamesItsRead(t *testing.T) {
	f := newComposeFixture(t)
	aud := map[string]any{"type": "group", "groupId": f.group.ID}
	path := f.draft(t, f.leader, "First", "<p>Hi</p>", aud)
	opened := revisionOf(t, f.leader, path)
	if got := revisionOf(t, f.admin, path); got != opened {
		t.Fatalf("an admin's read %q, the author's %q", got, opened)
	}
	var won messageResponseJSON
	w := f.admin.do("PUT", path, map[string]any{"subject": "The office's", "bodyHtml": "<p>Hi</p>", "audience": aud, "revision": opened}, true)
	decodeJSON(t, w, &won)
	if w.Code != http.StatusOK || won.Message.Revision == "" || won.Message.Revision == opened || won.Message.Subject != "The office's" {
		t.Fatalf("first save: %d %s", w.Code, w.Body.String())
	}
	if won.Message.Revision != revisionOf(t, f.leader, path) {
		t.Fatal("the answer's token is not the stored message's")
	}
	w = f.leader.do("PUT", path, map[string]any{"subject": "The leader's", "bodyHtml": "<p>Hi</p>", "audience": aud, "revision": opened}, true)
	if w.Code != http.StatusConflict || errorCodeOf(w) != "message_changed" {
		t.Fatalf("second save: %d %s", w.Code, w.Body.String())
	}
	var m models.Message
	gormMust(t, f.app.DB.First(&m, idOfPath(t, path)))
	if m.Subject != "The office's" {
		t.Fatalf("the refused save wrote: %q", m.Subject)
	}
	for _, given := range []string{"", "garbled", strings.Replace(opened, ".m", ".g", 1)} {
		if w := f.leader.do("PUT", path, map[string]any{"subject": "x", "bodyHtml": "<p>Hi</p>", "audience": aud, "revision": given}, true); errorCodeOf(w) != "message_changed" {
			t.Fatalf("revision %q: %d %s", given, w.Code, w.Body.String())
		}
	}
	// A save that puts the message back as it was still moves the token, so a
	// send whose answer was lost is never applied again over it.
	before := revisionOf(t, f.leader, path)
	w = f.leader.do("PUT", path, map[string]any{"subject": "The office's", "bodyHtml": "<p>Hi</p>", "audience": aud, "revision": before}, true)
	decodeJSON(t, w, &won)
	if w.Code != http.StatusOK || won.Message.Revision == before {
		t.Fatalf("an unchanged save: %d, token moved %v", w.Code, won.Message.Revision != before)
	}
	// Someone who may not edit the message gets no token and the refusal they
	// always got, before any comparison.
	var seen messageResponseJSON
	decodeJSON(t, f.admin.do("GET", path, nil, false), &seen)
	if w := f.host.do("PUT", path, map[string]any{"subject": "x", "bodyHtml": "<p>Hi</p>", "audience": aud, "revision": seen.Message.Revision}, true); w.Code != http.StatusNotFound {
		t.Fatalf("someone else's message: %d %s", w.Code, w.Body.String())
	}
	if w := f.host.do("GET", path+"/review", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("someone else's review: %d", w.Code)
	}
}

// A message that is sent or sending, or an automatic reminder, has no token,
// and its edit and review are refused as they always were.
func TestMessageRevisionOnlyForWhatMayChange(t *testing.T) {
	f := newComposeFixture(t)
	id := f.scheduleNow(t, f.leader, "Gone", "<p>Hi</p>", map[string]any{"type": "group", "groupId": f.group.ID})
	path := "/api/v1/messages/" + itoa(id)
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("status", models.MessageSending))
	var out messageResponseJSON
	decodeJSON(t, f.leader.do("GET", path, nil, false), &out)
	if out.Message.Revision != "" {
		t.Fatalf("a sending message has a token: %q", out.Message.Revision)
	}
	if w := f.leader.do("GET", path+"/review", nil, false); w.Code != http.StatusConflict || errorCodeOf(w) != "message_locked" {
		t.Fatalf("review while sending: %d %s", w.Code, w.Body.String())
	}
	soon := time.Now().Add(time.Hour).Truncate(time.Second)
	reminder := models.Message{Status: models.MessageScheduled, ScheduledAt: &soon, Category: models.MessageCategoryReminder, OccurrenceID: &f.groupOcc.ID,
		Subject: "Reminder", BodyHTML: "<p>Soon</p>", BodyText: "Soon", Audience: `{"type":"occurrence","occurrenceId":` + itoa(f.groupOcc.ID) + `}`}
	gormMust(t, f.app.DB.Create(&reminder))
	rpath := "/api/v1/messages/" + itoa(reminder.ID)
	decodeJSON(t, f.admin.do("GET", rpath, nil, false), &out)
	if out.Message.Revision != "" {
		t.Fatalf("a reminder has a token: %q", out.Message.Revision)
	}
	for _, w := range []*httptest.ResponseRecorder{
		f.admin.do("GET", rpath+"/review", nil, false),
		f.admin.do("PUT", rpath, map[string]any{"subject": "x", "bodyHtml": "<p>x</p>", "audience": map[string]any{"type": "occurrence", "occurrenceId": f.groupOcc.ID}}, true),
		f.admin.do("POST", rpath+"/schedule", map[string]any{}, true),
	} {
		if w.Code != http.StatusConflict || errorCodeOf(w) != "message_reminder" {
			t.Fatalf("a reminder: %d %s", w.Code, w.Body.String())
		}
	}
	var stored models.Message
	gormMust(t, f.app.DB.First(&stored, reminder.ID))
	if stored.Subject != "Reminder" || stored.Status != models.MessageScheduled || !stored.ScheduledAt.Equal(soon) {
		t.Fatalf("a reminder changed: %+v", stored)
	}
}

// A send names the text it was pressed on: one pressed on an older text is
// refused, with nothing scheduled and no job; on the text as it is, it goes.
func TestMessageSendNamesItsText(t *testing.T) {
	f := newComposeFixture(t)
	aud := map[string]any{"type": "group", "groupId": f.group.ID}
	path := f.draft(t, f.leader, "News", "<p>Hi</p>", aud)
	pressed := revisionOf(t, f.leader, path)
	review := takeReview(t, f.leader, path)
	// Another tab changes the words after the review.
	if w := f.leader.do("PUT", path, map[string]any{"subject": "Other words", "bodyHtml": "<p>Hi</p>", "audience": aud, "revision": pressed}, true); w.Code != http.StatusOK {
		t.Fatalf("other tab: %d %s", w.Code, w.Body.String())
	}
	if w := confirmSend(f.leader, path, pressed, review.Token, nil); w.Code != http.StatusConflict || errorCodeOf(w) != "message_changed" {
		t.Fatalf("send on older text: %d %s", w.Code, w.Body.String())
	}
	stillDraft(t, f, path)
	now := revisionOf(t, f.leader, path)
	// The review was of the older text, so it is not this text's.
	if w := confirmSend(f.leader, path, now, review.Token, nil); w.Code != http.StatusConflict || errorCodeOf(w) != "message_review_needed" {
		t.Fatalf("a review of older text: %d %s", w.Code, w.Body.String())
	}
	stillDraft(t, f, path)
	fresh := takeReview(t, f.leader, path)
	w := confirmSend(f.leader, path, now, fresh.Token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	var out messageResponseJSON
	decodeJSON(t, w, &out)
	if out.Message.Status != models.MessageScheduled || out.Message.Revision == "" || out.Message.Revision == now {
		t.Fatalf("scheduled: %+v", out)
	}
	// The answer is the message alone: it names no one.
	if strings.Contains(w.Body.String(), `"summary"`) || strings.Contains(w.Body.String(), `"recipients"`) {
		t.Fatalf("the schedule answer names people: %s", w.Body.String())
	}
	if jobCountFor(t, f.app, kindMessageSend, idOfPath(t, path)) != 1 {
		t.Fatal("no send job")
	}
	// The same press again, its answer lost: the revision moved, so nothing is
	// scheduled a second time.
	if w := confirmSend(f.leader, path, now, fresh.Token, nil); w.Code != http.StatusConflict || errorCodeOf(w) != "message_changed" {
		t.Fatalf("the same press again: %d %s", w.Code, w.Body.String())
	}
	if jobCountFor(t, f.app, kindMessageSend, idOfPath(t, path)) != 1 || auditCountFor(t, f.app, audit.MessageScheduled, idOfPath(t, path)) != 1 {
		t.Fatal("a second press scheduled it again")
	}
}

// A token that does not open for this send (another message's, another
// sender's, an older revision's, garbled, none) asks for a full review with a
// fresh one: never an error page, never a send.
func TestMessageReviewTokenIsBound(t *testing.T) {
	f := newComposeFixture(t)
	aud := map[string]any{"type": "group", "groupId": f.group.ID}
	path := f.draft(t, f.leader, "Mine", "<p>Hi</p>", aud)
	other := f.draft(t, f.leader, "Other", "<p>Hi</p>", aud)
	revision := revisionOf(t, f.leader, path)
	mine := takeReview(t, f.leader, path).Token
	tokens := map[string]string{
		"another message's": takeReview(t, f.leader, other).Token,
		"another sender's":  takeReview(t, f.admin, path).Token,
		"garbled":           mine[:len(mine)-4] + "AAAA",
		"not base64":        "%%%",
		"none":              "",
	}
	// An older revision's: a review, then an edit that leaves the list alone.
	old := revisionOf(t, f.leader, other)
	stale := takeReview(t, f.leader, other).Token
	if w := f.leader.do("PUT", other, map[string]any{"subject": "Other, again", "bodyHtml": "<p>Hi</p>", "audience": aud, "revision": old}, true); w.Code != http.StatusOK {
		t.Fatalf("edit: %d", w.Code)
	}
	for name, token := range tokens {
		w := confirmSend(f.leader, path, revision, token, nil)
		got := decodeReview(t, w)
		if w.Code != http.StatusConflict || got.Code != "message_review_needed" || got.Change != nil || got.Review.Token == "" || got.Review.Summary.Eligible == 0 {
			t.Fatalf("%s token: %d %s", name, w.Code, w.Body.String())
		}
		stillDraft(t, f, path)
	}
	w := confirmSend(f.leader, other, revisionOf(t, f.leader, other), stale, nil)
	if got := decodeReview(t, w); w.Code != http.StatusConflict || got.Code != "message_review_needed" {
		t.Fatalf("an older revision's token: %d %s", w.Code, w.Body.String())
	}
	stillDraft(t, f, other)
	// The answer's fresh token is good for the next press.
	got := decodeReview(t, confirmSend(f.leader, path, revision, "", nil))
	if w := confirmSend(f.leader, path, revision, got.Review.Token, nil); w.Code != http.StatusOK {
		t.Fatalf("with the fresh token: %d %s", w.Code, w.Body.String())
	}
}

// A member who joins between the review and the confirmation is named in the
// change, nothing is scheduled, and the fresh token sends it; a second change
// is answered the same way.
func TestMessageConfirmationNamesAJoiner(t *testing.T) {
	for _, sender := range []string{"leader", "admin"} {
		t.Run(sender, func(t *testing.T) {
			f := newComposeFixture(t)
			c := map[string]*apiClient{"leader": f.leader, "admin": f.admin}[sender]
			path := f.draft(t, c, "Welcome", "<p>Hi</p>", map[string]any{"type": "group", "groupId": f.group.ID})
			revision := revisionOf(t, c, path)
			review := takeReview(t, c, path)
			pat := newTestUser(t, f.app, "pat", "password1234", models.RoleMember)
			gormMust(t, f.app.DB.Model(pat).Update("display_name", "Pat Lee"))
			addMember(t, f.app, pat, f.group, models.MembershipMember)
			w := confirmSend(c, path, revision, review.Token, nil)
			got := decodeReview(t, w)
			if w.Code != http.StatusConflict || got.Code != "message_list_changed" || got.Change == nil ||
				strings.Join(got.Change.Added, ",") != "Pat Lee" || len(got.Change.Dropped) != 0 || got.Change.DroppedUnnamed != 0 ||
				got.Review.Summary.Eligible != review.Summary.Eligible+1 {
				t.Fatalf("a joiner: %d %s", w.Code, w.Body.String())
			}
			stillDraft(t, f, path)
			sam := newTestUser(t, f.app, "sam", "password1234", models.RoleMember)
			gormMust(t, f.app.DB.Model(sam).Update("display_name", "Sam Roe"))
			addMember(t, f.app, sam, f.group, models.MembershipMember)
			w = confirmSend(c, path, revision, got.Review.Token, nil)
			again := decodeReview(t, w)
			if w.Code != http.StatusConflict || again.Code != "message_list_changed" || strings.Join(again.Change.Added, ",") != "Sam Roe" {
				t.Fatalf("a second change: %d %s", w.Code, w.Body.String())
			}
			if w := confirmSend(c, path, revision, again.Review.Token, nil); w.Code != http.StatusOK {
				t.Fatalf("send to the list as it is now: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// Between the review and the confirmation, one person leaves the group, one
// turns off messages from leaders, one is disabled and one is erased. Each is
// refused. Whoever the audience still lists is named (as the picker names
// them); whoever left or was erased is only counted. The change gives no
// reason, and a leader's answer gives no one's reason anywhere: the summary is
// the picker's, which shows each person's reason to an admin only.
func TestMessageConfirmationCountsWhoLeft(t *testing.T) {
	reasons := []string{skipNotActive, skipOptedOut, skipNoEmail, skipNotWorking, skipComplained, skipGuest, skipHidden, "won't", "opted", "disabled", "erased", "deleted", "left"}
	for _, sender := range []string{"leader", "admin"} {
		for _, tc := range []struct {
			name   string
			change func(f composeFixture, u *models.User)
			named  bool
		}{
			{"leaves", func(f composeFixture, u *models.User) { f.setMembership(t, u, models.MembershipLeft) }, false},
			{"opts out", func(f composeFixture, u *models.User) {
				if w := loginAPI(t, f.app, u.Name, "password1234").do("PUT", "/api/v1/me/notification-settings", map[string]any{"category": "messages", "off": true}, true); w.Code != http.StatusOK {
					t.Fatalf("opt out: %d", w.Code)
				}
			}, true},
			{"is disabled", func(f composeFixture, u *models.User) {
				apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(u.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
			}, true},
			{"is erased", func(f composeFixture, u *models.User) {
				f.admin.autoReview = true
				if r := deleteAccount(f.admin, u.ID, nil); r.code != http.StatusOK {
					t.Fatalf("erase: %d %s", r.code, r.body)
				}
			}, false},
		} {
			t.Run(sender+" "+tc.name, func(t *testing.T) {
				f := newComposeFixture(t)
				c := map[string]*apiClient{"leader": f.leader, "admin": f.admin}[sender]
				robin := newTestUser(t, f.app, "robin", "password1234", models.RoleMember)
				gormMust(t, f.app.DB.Model(robin).Update("display_name", "Robin Vale"))
				addMember(t, f.app, robin, f.group, models.MembershipMember)
				path := f.draft(t, c, "Hello", "<p>Hi</p>", map[string]any{"type": "group", "groupId": f.group.ID})
				revision := revisionOf(t, c, path)
				review := takeReview(t, c, path)
				tc.change(f, robin)
				w := confirmSend(c, path, revision, review.Token, nil)
				got := decodeReview(t, w)
				if w.Code != http.StatusConflict || got.Code != "message_list_changed" || got.Change == nil || len(got.Change.Added) != 0 {
					t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
				}
				if tc.named && (strings.Join(got.Change.Dropped, ",") != "Robin Vale" || got.Change.DroppedUnnamed != 0) {
					t.Fatalf("%s should be named: %+v", tc.name, got.Change)
				}
				if !tc.named && (len(got.Change.Dropped) != 0 || got.Change.DroppedUnnamed != 1) {
					t.Fatalf("%s should be counted only: %+v", tc.name, got.Change)
				}
				change, _ := json.Marshal(got.Change)
				for _, why := range reasons {
					if strings.Contains(strings.ToLower(string(change)), strings.ToLower(why)) {
						t.Fatalf("the change gives a reason (%q): %s", why, change)
					}
				}
				for _, r := range got.Review.Summary.Recipients {
					if sender == "leader" && r.Skipped != "" && r.Skipped != skipHidden {
						t.Fatalf("a leader's answer gives %s's reason: %q", r.Name, r.Skipped)
					}
				}
				if !tc.named && strings.Contains(w.Body.String(), "Robin") {
					t.Fatalf("%s is named somewhere in the answer: %s", tc.name, w.Body.String())
				}
				stillDraft(t, f, path)
			})
		}
	}
}

// The added list holds the first 100 names and counts the rest; a person
// dropped is named only when the summary's list shows them.
func TestAudienceChangeCapsItsNames(t *testing.T) {
	reach := audienceReach{named: map[uint]string{}, listed: map[uint]bool{}}
	var confirmed []uint
	for i := uint(1); i <= 250; i++ {
		reach.named[i] = fmt.Sprintf("Person %03d", i)
		if i <= 120 {
			reach.reached = append(reach.reached, i)
		} else {
			confirmed = append(confirmed, i)
			reach.listed[i] = i <= 160
		}
	}
	confirmed = append(confirmed, 900, 901)
	got := audienceChange(confirmed, reach)
	if len(got.Added) != maxAudiencePreview || got.AddedMore != 20 || got.Added[0] != "Person 001" {
		t.Fatalf("added: %d named, %d more, first %q", len(got.Added), got.AddedMore, got.Added[0])
	}
	if len(got.Dropped) != 40 || got.Dropped[0] != "Person 121" || got.DroppedUnnamed != 90+2 {
		t.Fatalf("dropped: %d named, %d unnamed", len(got.Dropped), got.DroppedUnnamed)
	}
	if audienceChange(reach.reached, reach) != nil {
		t.Fatal("no change is nil")
	}
}

// The picker lists the first 100 people by name. Someone past that who turns
// off messages between the review and the confirmation is only counted: the
// list the sender can see never showed them.
func TestMessageConfirmationCountsWhoThePickerDoesNotList(t *testing.T) {
	f := newComposeFixture(t)
	for i := 0; i < maxAudiencePreview; i++ {
		u := models.User{Email: testEmail(fmt.Sprintf("filler%03d", i)), Role: models.RoleMember, Name: fmt.Sprintf("Aaa %03d", i), Status: models.UserActive}
		gormMust(t, f.app.DB.Create(&u))
		addMember(t, f.app, &u, f.group, models.MembershipMember)
	}
	zed := newTestUser(t, f.app, "zed", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(zed).Update("display_name", "Zzz Last"))
	addMember(t, f.app, zed, f.group, models.MembershipMember)
	path := f.draft(t, f.leader, "Hello", "<p>Hi</p>", map[string]any{"type": "group", "groupId": f.group.ID})
	revision := revisionOf(t, f.leader, path)
	review := takeReview(t, f.leader, path)
	if !review.Summary.More {
		t.Fatalf("the list is not capped: %d people", review.Summary.People)
	}
	if w := loginAPI(t, f.app, zed.Name, "password1234").do("PUT", "/api/v1/me/notification-settings", map[string]any{"category": "messages", "off": true}, true); w.Code != http.StatusOK {
		t.Fatalf("opt out: %d", w.Code)
	}
	w := confirmSend(f.leader, path, revision, review.Token, nil)
	got := decodeReview(t, w)
	if w.Code != http.StatusConflict || got.Code != "message_list_changed" || got.Change == nil ||
		len(got.Change.Dropped) != 0 || got.Change.DroppedUnnamed != 1 || strings.Contains(w.Body.String(), "Zzz") {
		t.Fatalf("someone past the list's cap: %d %s", w.Code, w.Body.String())
	}
	stillDraft(t, f, path)
}

// Every audience the composer can make reviews and sends; push counts in the
// list only when the message asks for it.
func TestMessageReviewEveryAudienceKind(t *testing.T) {
	f := newComposeFixture(t)
	var member models.User
	gormMust(t, f.app.DB.Where("name = ?", "member").First(&member))
	var cwOcc models.EventOccurrence
	gormMust(t, f.app.DB.Where("event_id = ?", f.churchEvent.ID).First(&cwOcc))
	for _, occ := range []uint{f.groupOcc.ID, cwOcc.ID} {
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &member.ID, OccurrenceID: occ, Status: models.RegGoing}))
	}
	for _, tc := range []struct {
		name string
		c    *apiClient
		aud  map[string]any
	}{
		{"group", f.leader, map[string]any{"type": policy.AudienceGroup, "groupId": f.group.ID, "roles": []string{"member"}}},
		{"date", f.leader, map[string]any{"type": policy.AudienceOccurrence, "occurrenceId": f.groupOcc.ID, "statuses": []string{"going", "waitlisted"}}},
		{"series", f.leader, map[string]any{"type": policy.AudienceEventSeries, "eventId": f.groupEvent.ID, "statuses": []string{"going"}}},
		{"church-wide date", f.host, map[string]any{"type": policy.AudienceOccurrence, "occurrenceId": cwOcc.ID}},
		{"everyone", f.admin, map[string]any{"type": policy.AudienceAllMembers}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := f.draft(t, tc.c, "To "+tc.name, "<p>Hi</p>", tc.aud)
			revision := revisionOf(t, tc.c, path)
			review := takeReview(t, tc.c, path)
			if review.Summary.Eligible == 0 {
				t.Fatalf("nobody: %+v", review.Summary)
			}
			if w := confirmSend(tc.c, path, revision, review.Token, nil); w.Code != http.StatusOK {
				t.Fatalf("send: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// Push rows count in the list only when the message asks for push: a person
// the email skips who has push on joins the list with the switch, so the
// switch is part of the reviewed list.
func TestMessageReachFollowsItsPushSwitch(t *testing.T) {
	f := newComposeFixture(t)
	pushConsentRow(t, f.app, f.opted, "all", models.ConsentOptedIn)
	newPushDevice(t, f.app, f.opted, "opted-phone")
	var m models.Message
	gormMust(t, f.app.DB.First(&m, idOfPath(t, f.draft(t, f.leader, "Hi", "<p>Hi</p>", map[string]any{"type": "group", "groupId": f.group.ID}))))
	reach := func(push bool) []uint {
		m.PushAlert = push
		_, _, r, err := f.app.messageReachIn(f.app.DB, f.leaderUser, &m)
		if err != nil {
			t.Fatal(err)
		}
		return sortedReach(r.reached)
	}
	with, without := reach(true), reach(false)
	has := func(ids []uint, id uint) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	if !has(with, f.opted.ID) || has(without, f.opted.ID) || len(with) != len(without)+1 {
		t.Fatalf("with push %v, without %v (opted %d)", with, without, f.opted.ID)
	}
}

// A message scheduled for later goes to the list as it stands when it goes:
// a member who joined after the confirmation gets it, with no check.
func TestScheduledMessageGoesToTheListAtSendTime(t *testing.T) {
	f := newComposeFixture(t)
	path := f.draft(t, f.leader, "Later", "<p>Hi</p>", map[string]any{"type": "group", "groupId": f.group.ID})
	soon := time.Now().Add(2 * time.Minute)
	if w := confirmSend(f.leader, path, revisionOf(t, f.leader, path), takeReview(t, f.leader, path).Token, map[string]any{"scheduledAt": soon}); w.Code != http.StatusOK {
		t.Fatalf("schedule: %d %s", w.Code, w.Body.String())
	}
	late := newTestUser(t, f.app, "late", "password1234", models.RoleMember)
	addMember(t, f.app, late, f.group, models.MembershipMember)
	id := idOfPath(t, path)
	gormMust(t, f.app.DB.Model(&models.Message{}).Where("id = ?", id).Update("scheduled_at", time.Now().Add(-time.Second)))
	gormMust(t, f.app.DB.Exec("UPDATE jobs SET run_at = now() WHERE kind = ?", kindMessageSend))
	runMessageJobs(t, f.app)
	if row, ok := recipientsOf(t, f.app, id)[late.Email]; !ok || row.Status != models.RecipientSent {
		t.Fatalf("the member who joined later: %+v (present %v)", row, ok)
	}
}

// A revocation that commits while a create, an edit or a send waits for the
// author's row is a 401 with nothing written, on a pool whose default is Repeatable
// Read too: the handler pins Read Committed.
func TestMessageWritesSeeARevocationAfterTheirLockWait(t *testing.T) {
	for _, repeatable := range []bool{false, true} {
		for _, route := range []string{"create", "edit", "send"} {
			t.Run(fmt.Sprintf("%s repeatable=%v", route, repeatable), func(t *testing.T) {
				f := newComposeFixture(t)
				aud := map[string]any{"type": "group", "groupId": f.group.ID}
				path := f.draft(t, f.leader, "Before", "<p>Hi</p>", aud)
				body := confirmedSend(t, f.leader, path, map[string]any{})
				method, target, lock := "POST", path+"/schedule", "%users%FOR NO KEY UPDATE%"
				switch route {
				case "edit":
					method, target = "PUT", path
					body = namedEdit(t, f.leader, path, map[string]any{"subject": "After", "bodyHtml": "<p>Hi</p>", "audience": aud})
				case "create":
					target, lock = "/api/v1/messages", "%users%FOR SHARE%"
					body = map[string]any{"subject": "Created", "bodyHtml": "<p>Hi</p>", "audience": aud}
				}
				orig := f.app.DB
				hold := holdUserRow(t, f.app, f.leaderUser.ID)
				if repeatable {
					f.app.DB = repeatableReadPool(t, f.app)
					t.Cleanup(func() { f.app.DB = orig })
				}
				res := bgDo(f.leader, method, target, body)
				waitForLockedUsersQuery(t, f.app, lock)
				gormMust(t, hold.Exec("UPDATE users SET session_version = session_version + 1 WHERE id = ?", f.leaderUser.ID))
				gormMust(t, hold.Commit())
				if r := awaitResult(t, res); r.code != http.StatusUnauthorized {
					t.Fatalf("%s: %d %s", route, r.code, r.body)
				}
				var m models.Message
				gormMust(t, orig.First(&m, idOfPath(t, path)))
				var made int64
				gormMust(t, orig.Model(&models.Message{}).Where("subject = ?", "Created").Count(&made))
				if m.Subject != "Before" || m.Status != models.MessageDraft || made != 0 {
					t.Fatalf("a revoked %s wrote: %+v (created %d)", route, m, made)
				}
			})
		}
	}
}

// confirmedSend is the body of a schedule press as the sender's confirmation
// sends it (DEC-169): extra, with the message's revision as c reads it now
// and a review c takes now. Either read failing fails the test.
func confirmedSend(t *testing.T, c *apiClient, path string, extra map[string]any) map[string]any {
	t.Helper()
	out := namedEdit(t, c, path, extra)
	out["review"] = takeReview(t, c, path).Token
	return out
}

// namedEdit is body with the message's revision as c reads it now, the read an
// editor opened with. A read that fails, or has no revision, fails the test.
func namedEdit(t *testing.T, c *apiClient, path string, body map[string]any) map[string]any {
	t.Helper()
	out := maps.Clone(body)
	if out == nil {
		out = map[string]any{}
	}
	out["revision"] = revisionOf(t, c, path)
	return out
}

// confirmedEdit is the body of an edit of a scheduled message's list as the
// composer sends it once the sender confirmed it (DEC-169): body with the
// revision c reads now and the token of the review its first PUT answered,
// which writes nothing. Any other answer to that PUT fails the test.
func confirmedEdit(t *testing.T, c *apiClient, path string, body map[string]any) map[string]any {
	t.Helper()
	out := namedEdit(t, c, path, body)
	w := c.do("PUT", path, out, true)
	got := decodeReview(t, w)
	if w.Code != http.StatusConflict || got.Code != "message_review_needed" || got.Review.Token == "" {
		t.Fatalf("an edit of a scheduled message's list: %d %s", w.Code, w.Body.String())
	}
	out["review"] = got.Review.Token
	return out
}

// Preview, "Send a test to me" and the first press of Schedule or Send now
// each save a scheduled message through PUT /messages/:id, so this table is
// all three at the server. An edit that changes who a scheduled message
// reaches (its audience, or its push switch) writes nothing until the sender
// confirms the proposed list; any other edit, and a draft's, saves as before.
func TestScheduledListEditNeedsTheSendersConfirmation(t *testing.T) {
	everyone := map[string]any{"type": "group", "groupId": "GROUP", "roles": []string{"member", "leader"}}
	for _, tc := range []struct {
		name string
		edit map[string]any
		// asks: the first PUT is message_review_needed with a review of the
		// proposal, whose eligible count is asked.
		asks  bool
		count int
		draft bool
		// joins: someone joins the group before the edit (a membership
		// change while it waits never prompts by itself).
		joins bool
	}{
		{name: "the audience", edit: map[string]any{"audience": map[string]any{"type": "group", "groupId": "GROUP", "roles": []string{"leader"}}}, asks: true, count: 1},
		{name: "the push switch", edit: map[string]any{"pushAlert": false}, asks: true, count: 2},
		{name: "the same audience in another order", edit: map[string]any{"audience": map[string]any{"type": "group", "groupId": "GROUP", "roles": []string{"leader", "member"}}}},
		{name: "the words only", edit: map[string]any{"subject": "New words"}},
		{name: "the words only, after someone joined", edit: map[string]any{"subject": "New words"}, joins: true},
		{name: "a draft's audience", edit: map[string]any{"audience": map[string]any{"type": "group", "groupId": "GROUP", "roles": []string{"leader"}}}, draft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newComposeFixture(t)
			pushConsentRow(t, f.app, f.opted, "all", models.ConsentOptedIn)
			newPushDevice(t, f.app, f.opted, "opted-phone")
			group := func(aud map[string]any) map[string]any {
				out := maps.Clone(aud)
				out["groupId"] = f.group.ID
				return out
			}
			path := f.draft(t, f.leader, "Later", "<p>Hi</p>", group(everyone))
			later := time.Now().Add(48 * time.Hour).Truncate(time.Second)
			if !tc.draft {
				if w := confirmSend(f.leader, path, revisionOf(t, f.leader, path), takeReview(t, f.leader, path).Token, map[string]any{"scheduledAt": later}); w.Code != http.StatusOK {
					t.Fatalf("schedule: %d %s", w.Code, w.Body.String())
				}
			}
			if tc.joins {
				addMember(t, f.app, newTestUser(t, f.app, "joiner", "password1234", models.RoleMember), f.group, models.MembershipMember)
			}
			body := map[string]any{"subject": "Later", "bodyHtml": "<p>Hi</p>", "audience": group(everyone), "pushAlert": true}
			for k, v := range tc.edit {
				if aud, ok := v.(map[string]any); ok {
					v = group(aud)
				}
				body[k] = v
			}
			var before models.Message
			gormMust(t, f.app.DB.First(&before, idOfPath(t, path)))
			opened := revisionOf(t, f.leader, path)
			w := f.leader.do("PUT", path, namedEdit(t, f.leader, path, body), true)
			if !tc.asks {
				var out messageEditResponseJSON
				decodeJSON(t, w, &out)
				if w.Code != http.StatusOK || out.Review != "" {
					t.Fatalf("an edit that needs no confirmation: %d %s", w.Code, w.Body.String())
				}
				return
			}
			got := decodeReview(t, w)
			if w.Code != http.StatusConflict || got.Code != "message_review_needed" || got.Change != nil || got.Review.Token == "" || got.Review.Summary.Eligible != tc.count {
				t.Fatalf("the first PUT: %d %s", w.Code, w.Body.String())
			}
			// Nothing written: the sender who goes back keeps the old list
			// scheduled for its time.
			var after models.Message
			gormMust(t, f.app.DB.First(&after, before.ID))
			if after.Audience != before.Audience || after.PushAlert != before.PushAlert || after.Subject != before.Subject || after.Status != models.MessageScheduled ||
				!after.ScheduledAt.Equal(*before.ScheduledAt) || revisionOf(t, f.leader, path) != opened || auditCountFor(t, f.app, audit.MessageEdited, before.ID) != 0 {
				t.Fatalf("a refused edit wrote: %+v", after)
			}
			// A token of the stored list, or of the proposal at another
			// revision, does not confirm the proposal.
			body["revision"] = opened
			for name, token := range map[string]string{"the stored list's": takeReview(t, f.leader, path).Token, "garbled": got.Review.Token[:len(got.Review.Token)-4] + "AAAA"} {
				body["review"] = token
				if w := f.leader.do("PUT", path, body, true); errorCodeOf(w) != "message_review_needed" {
					t.Fatalf("%s token: %d %s", name, w.Code, w.Body.String())
				}
			}
			// Someone joins before the sender confirms: the change, named, and
			// a fresh review of the proposal.
			pat := newTestUser(t, f.app, "pat", "password1234", models.RoleMember)
			gormMust(t, f.app.DB.Model(pat).Update("display_name", "Pat Lee"))
			addMember(t, f.app, pat, f.group, models.MembershipLeader)
			body["review"] = got.Review.Token
			w = f.leader.do("PUT", path, body, true)
			changed := decodeReview(t, w)
			if w.Code != http.StatusConflict || changed.Code != "message_list_changed" || changed.Change == nil || strings.Join(changed.Change.Added, ",") != "Pat Lee" ||
				changed.Review.Summary.Eligible != tc.count+1 {
				t.Fatalf("a joiner before the confirmation: %d %s", w.Code, w.Body.String())
			}
			// Confirmed: saved, still scheduled for its time, with the token of
			// that list for the revision it left, which a schedule press names.
			body["review"] = changed.Review.Token
			w = f.leader.do("PUT", path, body, true)
			var saved messageEditResponseJSON
			decodeJSON(t, w, &saved)
			if w.Code != http.StatusOK || saved.Review == "" || saved.Message.Status != models.MessageScheduled || !saved.Message.ScheduledAt.Equal(later) {
				t.Fatalf("the confirmed edit: %d %s", w.Code, w.Body.String())
			}
			gormMust(t, f.app.DB.First(&after, before.ID))
			if after.RecipientCount != tc.count+1 || auditCountFor(t, f.app, audit.MessageEdited, before.ID) != 1 {
				t.Fatalf("the confirmed edit stored: %+v", after)
			}
			if w := confirmSend(f.leader, path, saved.Message.Revision, saved.Review, map[string]any{"scheduledAt": later}); w.Code != http.StatusOK {
				t.Fatalf("a schedule press with the edit's token: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
