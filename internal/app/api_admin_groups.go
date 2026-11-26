package app

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"fold/internal/audit"
	"fold/internal/notify"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (a *App) apiPendingGroups(c *gin.Context) {
	var groups []models.Group
	if err := a.DB.Where("status = ?", models.GroupPending).Order("created_at").Find(&groups).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not list groups")
		return
	}
	a.respondGroups(c, groups)
}

func (a *App) respondGroups(c *gin.Context, groups []models.Group) {
	out, err := a.groupSummaries(apiUser(c), groups)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not list groups")
		return
	}
	c.JSON(http.StatusOK, groupListResponseJSON{Groups: out})
}

// apiApproveGroup and apiRejectGroup decide a pending group. Both are
// conditional on the group still being pending, and resolve every admin's
// task in the same transaction (5.12). A request from the bell names its
// notification (fromNotification); only Approve is allow-listed (6.6).
func (a *App) apiApproveGroup(c *gin.Context) {
	var body struct {
		Tags             []string `json:"tags"`
		FromNotification *uint    `json:"fromNotification"`
	}
	if !bindOptionalJSON(c, &body) {
		return
	}
	// Tags are the admin's to change only when they sent some; otherwise the
	// leader's own edits stay as they are.
	var tags *string
	if body.Tags != nil {
		joined := joinTags(body.Tags)
		if err := tagsField.check(joined); err != nil {
			fail(c, err)
			return
		}
		tags = &joined
	}
	a.decideGroup(c, notify.ActionApproveGroup, body.FromNotification, models.GroupActive, tags, "")
}

const maxRejectMessage = 500

// Notification text never carries contact details (5.12), so free text that
// becomes one is checked for an email address or a phone-shaped number: a run
// of ten or more digits (with the usual separators), or a local number like
// 555-0143. Dates such as 2026-11-05 or a 2026-2027 season are not phones.
var (
	digitRun    = regexp.MustCompile(`\+?\d[\d\s().-]{6,}\d`)
	localNumber = regexp.MustCompile(`\b\d{3}[-.\s]\d{4}\b`)
)

func hasContactDetails(s string) bool {
	if strings.Contains(s, "@") || localNumber.MatchString(s) {
		return true
	}
	for _, run := range digitRun.FindAllString(s, -1) {
		digits := 0
		for _, r := range run {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits >= 10 {
			return true
		}
	}
	return false
}

func (a *App) apiRejectGroup(c *gin.Context) {
	var body struct {
		FromNotification *uint  `json:"fromNotification"`
		Message          string `json:"message"`
	}
	if !bindOptionalJSON(c, &body) {
		return
	}
	// An optional message to the leaders, carried on their notice (DEC-96).
	message := strings.TrimSpace(body.Message)
	if utf8.RuneCountInString(message) > maxRejectMessage {
		fail(c, errMessageTooLong500)
		return
	}
	if hasContactDetails(message) {
		fail(c, errMessageHasContact)
		return
	}
	a.decideGroup(c, "reject", body.FromNotification, models.GroupArchived, nil, message)
}

func (a *App) decideGroup(c *gin.Context, action string, fromNotification *uint, status string, tags *string, note string) {
	id, _ := strconv.Atoi(c.Param("id"))
	var g models.Group
	if err := a.DB.First(&g, id).Error; err != nil {
		jsonError(c, http.StatusNotFound, "group not found")
		return
	}
	if !a.fromNotification(c, fromNotification, action, g.ID) {
		return
	}
	me := apiUser(c)
	outcome, auditAction := "approved", audit.GroupApproved
	if action != notify.ActionApproveGroup {
		outcome, auditAction = "rejected", audit.GroupRejected
	}
	err := a.contactMutation(contactChange{GroupID: g.ID}, func(tx *gorm.DB) error {
		set := map[string]any{"status": status}
		if tags != nil {
			set["tags"] = *tags
		}
		if err := decidePendingIn(tx, &models.Group{}, g.ID, models.GroupPending, set); err != nil {
			return err
		}
		// Answer with the row as stored, not the unlocked read from above.
		if err := tx.First(&g, g.ID).Error; err != nil {
			return err
		}
		if _, err := notify.Resolve(tx, notify.GroupPending, g.ID, &me.ID, outcome); err != nil {
			return err
		}
		if err := notifyGroupDecided(tx, &g, me.ID, outcome == "approved", note); err != nil {
			return err
		}
		var meta map[string]any
		if note != "" {
			meta = map[string]any{"message": note}
		}
		return audit.Write(tx, audit.Entry{Actor: &me.ID, Action: auditAction, TargetType: "group", TargetID: g.ID, Meta: meta, IP: c.ClientIP()})
	})
	if a.answerAlreadyHandled(c, err, notify.GroupPending, g.ID) {
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not "+action+" group")
		return
	}
	if action == notify.ActionApproveGroup {
		summary, err := a.groupSummary(apiUser(c), &g)
		if err != nil {
			jsonError(c, http.StatusInternalServerError, "could not "+action+" group")
			return
		}
		c.JSON(http.StatusOK, groupResponseJSON{Group: summary})
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}

func (a *App) apiAdminListGroups(c *gin.Context) {
	var groups []models.Group
	if err := a.DB.Order("name").Find(&groups).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not list groups")
		return
	}
	a.respondGroups(c, groups)
}

func (a *App) apiAdminAddMember(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var g models.Group
	if err := a.DB.First(&g, id).Error; err != nil {
		jsonError(c, http.StatusNotFound, "group not found")
		return
	}
	var body struct {
		UserID      uint   `json:"userId"`
		SelectionID string `json:"selectionId"`
		Role        string `json:"role"`
		// OverLimit: add even though the group is full (DEC-39); audited.
		OverLimit bool `json:"overLimit"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || (body.UserID == 0) == (body.SelectionID == "") {
		jsonError(c, 400, "choose one person by selection or userId")
		return
	}
	var u models.User
	if body.SelectionID != "" {
		selected, ok := a.resolvePeopleSelection(c, body.SelectionID)
		if !ok {
			return
		}
		u = *selected
	} else if err := a.DB.First(&u, body.UserID).Error; err != nil {
		jsonError(c, 404, "user not found")
		return
	}
	if policy.TeenAddNeedsApproval(asPerson(&u)) {
		fail(c, errTeenAddApproval)
		return
	}
	role := body.Role
	if role == "" {
		role = models.MembershipMember
	}
	if role != models.MembershipLeader && role != models.MembershipMember {
		jsonError(c, http.StatusBadRequest, "role must be leader or member")
		return
	}
	err := a.changeRole(apiUser(c), c.ClientIP(), &g, u.ID, role, body.OverLimit, &memberAddOptions{}, func(me, target *models.User, actor, t policy.Rel, leaders int) bool {
		// Admins may add someone straight in with either role.
		return policy.IsAdmin(asPerson(me)) && policy.CanChangeRole(asPerson(me), actor, asPerson(target),
			policy.Rel{Member: true, Leader: t.Leader}, role == models.MembershipLeader, leaders)
	})
	switch err {
	case nil:
	case errRoleRefused: // here the refusal is the leader rule
		fail(c, errLeaderRuleAdults)
		return
	default:
		fail(c, err)
		return
	}
	a.membershipAdded(c, &g, u.ID)
}

func (a *App) apiDeletionRequests(c *gin.Context) {
	var groups []models.Group
	if err := a.DB.Where("deletion_requested = ?", true).Order("updated_at").Find(&groups).Error; err != nil {
		jsonError(c, http.StatusInternalServerError, "could not list groups")
		return
	}
	a.respondGroups(c, groups)
}

// apiRejectDeletion keeps a group whose lone leader asked to delete it. It is
// only an answer to an open request. The contact gate keeps the asker's
// account and access stable while the group lock orders the decision.
func (a *App) apiRejectDeletion(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	err := a.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := lockContacts(tx); err != nil {
			return err
		}
		g, actor, err := lockAdminGroupIn(tx, apiUser(c), uint(id))
		if err != nil {
			return err
		}
		if !g.DeletionRequested {
			return errAlreadyHandled
		}
		if err := notifyDeletionKept(tx, g, actor.ID); err != nil {
			return err
		}
		return clearDeletionRequestIn(tx, g.ID, &actor.ID, "kept")
	}, contactGateTx)
	if a.answerAlreadyHandled(c, err, notify.GroupDeletion, uint(id)) {
		return
	}
	if err != nil {
		answerAdminGroupWrite(c, err, "could not keep the group")
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}

// errDeletionMarker: the marker that a delete answers a deletion request is
// either absent or exactly "1"; anything else is refused rather than read as
// a delete that is never refused.
var errDeletionMarker = apiErr(http.StatusBadRequest, "", "deletionRequest must be 1 or left out")

// apiAdminDeleteGroup removes any group. ?deletionRequest=1 marks the
// Approvals card's delete, which answers the leader's request and is refused
// once that request is no longer open; the Groups tab's delete sends no
// marker and is never refused for the request's state.
func (a *App) apiAdminDeleteGroup(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	marker, marked := c.GetQuery("deletionRequest")
	if marked && marker != "1" {
		fail(c, errDeletionMarker)
		return
	}
	err := a.deleteGroup(c.Request.Context(), apiUser(c), uint(id), marked, c.ClientIP())
	if a.answerAlreadyHandled(c, err, notify.GroupDeletion, uint(id)) {
		return
	}
	if err != nil {
		answerAdminGroupWrite(c, err, "could not delete group")
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}

// answerAdminGroupWrite answers a failed admin write on a locked group: its
// own answer (a group gone while the request waited, a session that ended),
// else a 500.
func answerAdminGroupWrite(c *gin.Context, err error, failure string) {
	if isAPIError(err) {
		fail(c, err)
		return
	}
	jsonError(c, http.StatusInternalServerError, failure)
}

// apiGroupDeleteCount is how many other people deleting the group would tell
// (groupDeleteTells), for the confirmation to show. One snapshot judges the
// admin and reads the people; it binds nothing: the delete reads them again
// under its locks.
func (a *App) apiGroupDeleteCount(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var out signedUpCountJSON
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if a.hooks.readerBegin != nil {
			a.hooks.readerBegin("group delete count")
		}
		me, err := snapshotViewerIn(tx, apiUser(c))
		if err != nil {
			return err
		}
		if me == nil {
			return errSecuritySession
		}
		judged.judge(me)
		if !policy.IsAdmin(asPerson(me)) {
			return errSecuritySession
		}
		var g models.Group
		if err := tx.Where("id = ?", id).Limit(1).Find(&g).Error; err != nil {
			return err
		}
		if g.ID == 0 {
			return errGroupNotFound
		}
		d, err := groupDeleteTells(tx, g.ID, me.ID, a.now(), false)
		out.People = d.people()
		return err
	})
	if ackErr := a.acknowledgeDecision(c, judged, err == nil || decidedRefusal(err)); ackErr != nil {
		err = ackErr
	}
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
