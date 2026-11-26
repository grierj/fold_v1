package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"fold/internal/notify"
	"fold/internal/testdb"
	"fold/models"

	"gorm.io/gorm"
)

// groupDeleteState is every row a group delete or a Keep would write: the
// group, its memberships, events, dates and registrations, and every notice,
// audit row and open deletion task.
func groupDeleteState(t *testing.T, app *App, gid uint) string {
	t.Helper()
	var g models.Group
	gormMust(t, app.DB.Unscoped().First(&g, gid))
	count := func(model any, where string, args ...any) int64 {
		var n int64
		gormMust(t, app.DB.Model(model).Where(where, args...).Count(&n))
		return n
	}
	return fmt.Sprintf("group deleted=%v requested=%v memberships=%d events=%d dates=%d registrations=%d notices=%d audit=%d open tasks=%d",
		g.DeletedAt.Valid, g.DeletionRequested,
		count(&models.Membership{}, "group_id = ? AND status = ?", gid, models.MembershipActive),
		count(&models.Event{}, "group_id = ?", gid),
		count(&models.EventOccurrence{}, "event_id IN (SELECT id FROM events WHERE group_id = ?)", gid),
		count(&models.Registration{}, "occurrence_id IN (SELECT o.id FROM event_occurrences o JOIN events e ON e.id = o.event_id WHERE e.group_id = ?)", gid),
		count(&models.Notification{}, "true"),
		count(&models.AuditLog{}, "true"),
		count(&models.Notification{}, "kind = ? AND subject_id = ? AND resolved_at IS NULL", notify.GroupDeletion, gid))
}

// deletionRequestFixture is a public group "Solo" whose lone leader has asked
// to delete it, with one event the leader and an outsider are signed up for.
type deletionRequestFixture struct {
	app                  *App
	admin, lead          *apiClient
	adminUser, leadUser  *models.User
	other                *apiClient
	gid                  uint
	event                eventJSON
	dates                []models.EventOccurrence
	outsider             *models.User
	outsiderClient       *apiClient
	deletePath, keepPath string
}

func newDeletionRequestFixture(t *testing.T) deletionRequestFixture {
	t.Helper()
	app := testApp(t)
	f := deletionRequestFixture{app: app}
	f.adminUser = newTestUser(t, app, "request admin", "password1234", models.RoleAdmin)
	newTestUser(t, app, "other admin", "password1234", models.RoleAdmin)
	f.leadUser = newTestUser(t, app, "request lead", "password1234", models.RoleMember)
	f.outsider = newTestUser(t, app, "request outsider", "password1234", models.RoleMember)
	f.admin = loginAPI(t, app, f.adminUser.Name, "password1234")
	f.other = loginAPI(t, app, "other admin", "password1234")
	f.lead = loginAPI(t, app, f.leadUser.Name, "password1234")
	f.outsiderClient = loginAPI(t, app, f.outsider.Name, "password1234")
	f.gid = createGroupAs(t, f.admin, "Solo", models.VisibilityPublic)
	f.event = createFeedEvent(t, f.admin, f.gid, "Solo supper", feedStart(5), "FREQ=WEEKLY;COUNT=2")
	gormMust(t, app.DB.Where("event_id = ?", f.event.ID).Order("starts_at").Find(&f.dates))
	addMemberEmail(t, f.admin, f.gid, f.leadUser.Email)
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/groups/%d/members/%d/role", f.gid, f.leadUser.ID), map[string]any{"role": "leader"}, http.StatusOK)
	if w := confirmDeparture(t, f.admin, "POST", "/api/v1/groups/"+itoa(f.gid)+"/leave", "/api/v1/groups/"+itoa(f.gid)+"/departure-preview", ""); w.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", w.Code, w.Body.String())
	}
	signUp(t, app, f.leadUser, f.dates[0].ID, models.RegGoing)
	signUp(t, app, f.outsider, f.dates[1].ID, models.RegGoing)
	apiDo(t, f.lead, "POST", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", map[string]any{}, http.StatusOK)
	f.deletePath = "/api/v1/admin/groups/" + itoa(f.gid) + "?deletionRequest=1"
	f.keepPath = "/api/v1/admin/groups/" + itoa(f.gid) + "/reject-deletion"
	return f
}

// The request card's Delete answers the leader's open request: the group goes
// and the leader hears it was done through the member notice.
func TestRequestCardDeletesAnOpenRequest(t *testing.T) {
	f := newDeletionRequestFixture(t)
	mark := markNotices(t, f.app)
	apiDo(t, f.admin, "DELETE", f.deletePath, nil, http.StatusOK)
	drainNotices(t, f.app)
	if got := groupDeleteState(t, f.app, f.gid); !strings.HasPrefix(got, "group deleted=true requested=true memberships=0 events=0 dates=0 registrations=0 ") || !strings.HasSuffix(got, " open tasks=0") {
		t.Fatalf("after the delete: %s", got)
	}
	wantGroupDeleted(t, f.app, "the leader", f.leadUser.ID, f.lead, f.leadUser.Email, mark, "Solo was deleted")
	if got := noticesSince(t, f.app, f.outsider.ID, notify.EventDeleted, mark.notice); len(got) != 1 {
		t.Fatalf("the outsider got %d event notices, want 1", len(got))
	}
	task := findKind(feed(t, f.other, ""), notify.GroupDeletion)
	if task == nil || task.Waiting || task.Resolution == nil || task.Resolution.Outcome != "deleted" {
		t.Fatalf("the other admin's task: %+v", task)
	}
}

// waitLockWaiters waits until some request is waiting on a lock.
func waitLockWaiters(t *testing.T, app *App) {
	t.Helper()
	testdb.WaitUntil(t, "a request waiting on a lock", 5*time.Second, func() bool {
		var n int64
		gormMust(t, app.DB.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n))
		return n > 0
	})
}

// Once the request is no longer open (the leader withdrew it, someone joined,
// which withdraws it, or another admin kept the group) the card's Delete and
// Keep group write nothing and answer the queue's already-handled 409 with
// how it ended. The Groups tab's Delete, with no marker, still deletes.
func TestRequestCardRefusesARequestNoLongerOpen(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		end           func(f deletionRequestFixture)
	}{
		{"withdrawn", "withdrawn", func(f deletionRequestFixture) {
			apiDo(t, f.lead, "DELETE", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", nil, http.StatusOK)
		}},
		{"someone joined", "withdrawn", func(f deletionRequestFixture) {
			apiDo(t, f.outsiderClient, "POST", "/api/v1/groups/"+itoa(f.gid)+"/join", nil, http.StatusOK)
		}},
		{"kept", "kept", func(f deletionRequestFixture) {
			apiDo(t, f.other, "POST", f.keepPath, nil, http.StatusOK)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeletionRequestFixture(t)
			tc.end(f)
			drainNotices(t, f.app)
			before := groupDeleteState(t, f.app, f.gid)
			for _, p := range []struct{ method, path string }{{"DELETE", f.deletePath}, {"POST", f.keepPath}} {
				w := f.admin.do(p.method, p.path, nil, true)
				wantCode(t, p.method+" on a request "+tc.name, w, http.StatusConflict, "already_handled")
				if !strings.Contains(w.Body.String(), `"resolution":"`+tc.outcome+`"`) {
					t.Fatalf("%s %s: %s, want resolution %s", p.method, tc.name, w.Body.String(), tc.outcome)
				}
				if got := groupDeleteState(t, f.app, f.gid); got != before {
					t.Fatalf("%s on a request %s wrote:\n%s\nwant\n%s", p.method, tc.name, got, before)
				}
			}
			apiDo(t, f.admin, "DELETE", "/api/v1/admin/groups/"+itoa(f.gid), nil, http.StatusOK)
			if got := groupDeleteState(t, f.app, f.gid); !strings.HasPrefix(got, "group deleted=true ") {
				t.Fatalf("the Groups tab's delete: %s", got)
			}
		})
	}
}

// The marker is exactly 1 or absent: any other value is refused, never read
// as the Groups tab's delete that is never refused. Keep group on an open
// request keeps it and closes the task.
func TestDeletionMarkerAndKeep(t *testing.T) {
	f := newDeletionRequestFixture(t)
	before := groupDeleteState(t, f.app, f.gid)
	for _, bad := range []string{"0", "true", ""} {
		w := f.admin.do("DELETE", "/api/v1/admin/groups/"+itoa(f.gid)+"?deletionRequest="+bad, nil, true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("marker %q: %d %s", bad, w.Code, w.Body.String())
		}
	}
	if got := groupDeleteState(t, f.app, f.gid); got != before {
		t.Fatalf("a bad marker wrote:\n%s\nwant\n%s", got, before)
	}
	mark := markNotices(t, f.app)
	apiDo(t, f.admin, "POST", f.keepPath, nil, http.StatusOK)
	task := findKind(feed(t, f.other, ""), notify.GroupDeletion)
	if task == nil || task.Waiting || task.Resolution == nil || task.Resolution.Outcome != "kept" || task.Resolution.By != f.adminUser.Name {
		t.Fatalf("after keep: %+v", task)
	}
	const kept = notify.GroupDeletionKept
	notice := findKind(feed(t, f.lead, ""), kept)
	if notice == nil || notice.Title != "Solo was kept" || notice.Link != "/groups/"+itoa(f.gid) || notice.Waiting {
		t.Fatalf("the asker's in-app notice: %+v", notice)
	}
	rows := noticesSince(t, f.app, f.leadUser.ID, kept, mark.notice)
	if len(rows) != 1 || rows[0].PushTitle != "" {
		t.Fatalf("the asker's stored notices: %+v", rows)
	}
	if n := contactCount(t, f.app, &models.NotificationDelivery{}, "notification_id = ?", rows[0].ID); n != 0 {
		t.Fatalf("Keep wrote %d delivery rows for the in-app notice", n)
	}
	if n := contactCount(t, f.app, &models.Job{}, "unique_key = ?", notify.DeliverKey(rows[0].ID)); n != 0 {
		t.Fatalf("Keep queued %d delivery jobs for the in-app notice", n)
	}
	if n := contactCount(t, f.app, &models.Notification{}, "kind = ? AND id > ?", kept, mark.notice); n != 1 {
		t.Fatalf("Keep sent %d notices, want one", n)
	}
	wantCode(t, "repeat Keep", f.other.do("POST", f.keepPath, nil, true), http.StatusConflict, "already_handled")
	if n := contactCount(t, f.app, &models.Notification{}, "kind = ? AND id > ?", kept, mark.notice); n != 1 {
		t.Fatalf("repeat Keep left %d notices, want one", n)
	}
}

func TestKeepTellsTheSoleAdminWhoAskedBeforeAnotherAdminJoined(t *testing.T) {
	app := testApp(t)
	asker := newTestUser(t, app, "first admin", "password1234", models.RoleAdmin)
	lead := loginAPI(t, app, asker.Name, "password1234")
	gid := createGroupAs(t, lead, "Solo", models.VisibilityPublic)
	apiDo(t, lead, "POST", "/api/v1/groups/"+itoa(gid)+"/deletion-request", nil, http.StatusOK)
	if n := contactCount(t, app, &models.Notification{}, "kind = ? AND subject_id = ?", notify.GroupDeletion, gid); n != 0 {
		t.Fatalf("the sole admin's request created %d tasks", n)
	}

	other := newTestUser(t, app, "next admin", "password1234", models.RoleAdmin)
	admin := loginAPI(t, app, other.Name, "password1234")
	keep := "/api/v1/admin/groups/" + itoa(gid) + "/reject-deletion"
	apiDo(t, admin, "POST", keep, nil, http.StatusOK)
	notice := findKind(feed(t, lead, ""), notify.GroupDeletionKept)
	if notice == nil || notice.Title != "Solo was kept" || notice.Link != "/groups/"+itoa(gid) {
		t.Fatalf("the sole admin's Keep notice: %+v", notice)
	}
	wantCode(t, "repeat Keep", admin.do("POST", keep, nil, true), http.StatusConflict, "already_handled")
	if n := contactCount(t, app, &models.Notification{}, "kind = ? AND user_id = ?", notify.GroupDeletionKept, asker.ID); n != 1 {
		t.Fatalf("the requester got %d Keep notices, want one", n)
	}
}

func TestKeepUsesOnlyAnUnambiguousLegacyRequester(t *testing.T) {
	for _, evidence := range []string{"same actor", "different actors", "missing actor", "no tasks"} {
		t.Run(evidence, func(t *testing.T) {
			f := newDeletionRequestFixture(t)
			gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.gid).Update("deletion_requester_id", nil))
			tasks := f.app.DB.Where("kind = ? AND subject_id = ?", notify.GroupDeletion, f.gid)
			switch evidence {
			case "different actors":
				gormMust(t, tasks.Model(&models.Notification{}).Where("user_id = ?", f.adminUser.ID).Update("actor_id", f.outsider.ID))
			case "missing actor":
				gormMust(t, tasks.Model(&models.Notification{}).Where("user_id = ?", f.adminUser.ID).Update("actor_id", nil))
			case "no tasks":
				gormMust(t, tasks.Delete(&models.Notification{}))
			}
			apiDo(t, f.admin, "POST", f.keepPath, nil, http.StatusOK)
			got := findKind(feed(t, f.lead, ""), notify.GroupDeletionKept)
			if (got != nil) != (evidence == "same actor") {
				t.Fatalf("requester's notice: %+v", got)
			}
			if n := contactCount(t, f.app, &models.Notification{}, "kind = ? AND user_id <> ?", notify.GroupDeletionKept, f.leadUser.ID); n != 0 {
				t.Fatalf("Keep notified %d other accounts", n)
			}
		})
	}
}

func TestWithdrawDeletionRefusesAnAlreadyClosedRequest(t *testing.T) {
	f := newDeletionRequestFixture(t)
	path := "/api/v1/groups/" + itoa(f.gid) + "/deletion-request"
	apiDo(t, f.lead, "DELETE", path, nil, http.StatusOK)
	before := groupDeleteState(t, f.app, f.gid)
	wantCode(t, "repeat withdrawal", f.lead.do("DELETE", path, nil, true), http.StatusConflict, "already_handled")
	if got := groupDeleteState(t, f.app, f.gid); got != before {
		t.Fatalf("repeat withdrawal wrote:\n%s\nwant\n%s", got, before)
	}
}

func TestKeepNotifiesTheAskerWithoutRevealingAHiddenGroup(t *testing.T) {
	f := newDeletionRequestFixture(t)
	// A request left open when its leader departed, as on an older release.
	gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.gid).Update("visibility", models.VisibilityInviteOnly))
	gormMust(t, f.app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.gid, f.leadUser.ID).Update("status", models.MembershipLeft))
	gormMust(t, f.app.DB.Create(&models.Membership{GroupID: f.gid, UserID: f.outsider.ID, Role: models.MembershipLeader, Status: models.MembershipActive}))
	apiDo(t, f.lead, "GET", groupDetailPath(f.gid), nil, http.StatusNotFound)

	apiDo(t, f.admin, "POST", f.keepPath, nil, http.StatusOK)
	notice := findKind(feed(t, f.lead, ""), notify.GroupDeletionKept)
	if notice == nil || notice.SubjectID != nil || notice.Link != "/my-groups" ||
		notice.Title != "Group deletion request closed" || strings.Contains(notice.Body, "Solo") {
		t.Fatalf("the hidden group's notice: %+v", notice)
	}
	if notice := findKind(feed(t, f.outsiderClient, ""), notify.GroupDeletionKept); notice != nil {
		t.Fatalf("the replacement leader received another person's outcome: %+v", notice)
	}
}

func TestErasingAFormerGroupDeletionRequesterClearsTheirIdentity(t *testing.T) {
	f := newDeletionRequestFixture(t)
	// The request outlived the leader's membership, so erasure needs no handoff.
	gormMust(t, f.app.DB.Model(&models.Membership{}).Where("group_id = ? AND user_id = ?", f.gid, f.leadUser.ID).Update("status", models.MembershipLeft))
	gormMust(t, f.app.DB.Create(&models.Membership{GroupID: f.gid, UserID: f.outsider.ID, Role: models.MembershipLeader, Status: models.MembershipActive}))
	apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(f.leadUser.ID), nil, http.StatusOK)
	var group models.Group
	gormMust(t, f.app.DB.First(&group, f.gid))
	if group.DeletionRequesterID != nil {
		t.Fatalf("group retained erased requester %d", *group.DeletionRequesterID)
	}
	apiDo(t, f.admin, "POST", f.keepPath, nil, http.StatusOK)
	if n := contactCount(t, f.app, &models.Notification{}, "kind = ?", notify.GroupDeletionKept); n != 0 {
		t.Fatalf("Keep wrote %d notices after the requester's erasure", n)
	}
}

func TestJoinReplyWhenTheGroupWasJustDeleted(t *testing.T) {
	f := newDeletionRequestFixture(t)
	deleted := false
	const callback = "test:delete-before-join-reply"
	err := f.app.DB.Callback().Query().Before("gorm:query").Register(callback, func(db *gorm.DB) {
		if _, readingMembership := db.Statement.Dest.(*models.Membership); deleted || !readingMembership || !db.Statement.RaiseErrorOnNotFound {
			return
		}
		deleted = true
		if n := contactCount(t, f.app, &models.Membership{}, "group_id = ? AND user_id = ?", f.gid, f.outsider.ID); n != 1 {
			t.Fatalf("the join did not commit before the reply read: %d membership rows", n)
		}
		apiDo(t, f.admin, "DELETE", "/api/v1/admin/groups/"+itoa(f.gid), nil, http.StatusOK)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(callback) })
	w := f.outsiderClient.do("POST", "/api/v1/groups/"+itoa(f.gid)+"/join", nil, true)
	if !deleted || w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "group not found") {
		t.Fatalf("join after delete=%v: %d %s", deleted, w.Code, w.Body.String())
	}
}

// An admin whose session ended, or who was demoted, while the delete or the
// Keep waited is refused as every admin writer refuses (401), with nothing
// written. The change commits in the transaction that holds what the request
// waits on: the contact gate for the delete, the group row for Keep.
func TestRequestCardRechecksTheAdminAfterItsLock(t *testing.T) {
	for _, change := range []string{"demoted", "signed out"} {
		for _, action := range []string{"delete", "keep"} {
			t.Run(change+" "+action, func(t *testing.T) {
				f := newDeletionRequestFixture(t)
				drainNotices(t, f.app)
				before := groupDeleteState(t, f.app, f.gid)
				hold := holdContactGate(t, f.app)
				method, path := "DELETE", f.deletePath
				if action == "keep" {
					var g models.Group
					gormMust(t, hold.Raw("SELECT * FROM groups WHERE id = ? FOR UPDATE", f.gid).Scan(&g))
					method, path = "POST", f.keepPath
				}
				done := queueContact(func() *httptest.ResponseRecorder { return f.admin.do(method, path, nil, true) })
				waitLockWaiters(t, f.app)
				set := map[string]any{"session_version": gorm.Expr("session_version + 1")}
				if change == "demoted" {
					set["role"] = models.RoleMember
				}
				gormMust(t, hold.Model(&models.User{}).Where("id = ?", f.adminUser.ID).Updates(set))
				if err := hold.Commit().Error; err != nil {
					t.Fatal(err)
				}
				w := awaitContactResponse(t, done)
				wantCode(t, change+" "+action, w, http.StatusUnauthorized, "signed_out")
				if got := groupDeleteState(t, f.app, f.gid); got != before {
					t.Fatalf("a stale admin's %s wrote:\n%s\nwant\n%s", action, got, before)
				}
			})
		}
	}
}

// Every way someone else comes into the group withdraws the lone leader's
// request in the add's own transaction, under the group lock the card's
// Delete takes: a join, a leader's add by email, an admin's add and an
// invitation accepted. The card's Delete is then refused.
func TestEveryAddWithdrawsTheDeletionRequest(t *testing.T) {
	for _, path := range []string{"join", "leader add", "admin add", "invitation"} {
		t.Run(path, func(t *testing.T) {
			f := newDeletionRequestFixture(t)
			switch path {
			case "join":
				apiDo(t, f.outsiderClient, "POST", "/api/v1/groups/"+itoa(f.gid)+"/join", nil, http.StatusOK)
			case "leader add":
				addMemberEmail(t, f.lead, f.gid, f.outsider.Email)
			case "admin add":
				apiDo(t, f.admin, "POST", "/api/v1/admin/groups/"+itoa(f.gid)+"/members", map[string]any{"userId": f.outsider.ID}, http.StatusOK)
			case "invitation":
				token := invitedIntoGroup(t, f.app, f.lead, "invited@example.com", f.gid)
				apiDo(t, anonClient(t, f.app, "192.0.2.9"), "POST", "/api/v1/invitations/accept", acceptBody(token), http.StatusOK)
			}
			var g models.Group
			gormMust(t, f.app.DB.First(&g, f.gid))
			if g.DeletionRequested {
				t.Fatalf("the request is still open after a %s", path)
			}
			task := findKind(feed(t, f.other, ""), notify.GroupDeletion)
			if task == nil || task.Waiting || task.Resolution == nil || task.Resolution.Outcome != "withdrawn" {
				t.Fatalf("the admins' task after a %s: %+v", path, task)
			}
			wantCode(t, "the card's delete", f.admin.do("DELETE", f.deletePath, nil, true), http.StatusConflict, "already_handled")
		})
	}
}

var openTasks = regexp.MustCompile(`open tasks=\d+`)

// The card's Delete decides, under the group's row lock, that the request is
// still the leader's alone: the flag is set and the group has at most one
// listed member. A request that has since gained a second listed member by a
// path that left the flag set (an add from before every add withdrew it) is
// withdrawn there, which commits:
// the delete is refused as already handled, the group, its members and its
// events stay, and the admins' task is closed. So is a request whose leader
// was disabled or erased and handed the group to a successor, which the
// handoff withdraws. The count read is unchanged by any of it.
func TestRequestCardRefusesARequestThatGainedAMember(t *testing.T) {
	for _, path := range []string{"re-enabled member", "older add", "successor after a disable", "successor after an erasure"} {
		t.Run(path, func(t *testing.T) {
			f := newDeletionRequestFixture(t)
			other := newTestUser(t, f.app, "request other "+itoa(uint(len(path))), "password1234", models.RoleMember)
			switch path {
			case "re-enabled member":
				apiDo(t, f.lead, "DELETE", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", nil, http.StatusOK)
				apiDo(t, f.admin, "POST", "/api/v1/admin/groups/"+itoa(f.gid)+"/members", map[string]any{"userId": other.ID}, http.StatusOK)
				apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(other.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
				apiDo(t, f.lead, "POST", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", map[string]any{}, http.StatusOK)
				apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(other.ID), map[string]any{"status": models.UserActive}, http.StatusOK)
			case "older add":
				gormMust(t, f.app.DB.Create(&models.Membership{UserID: other.ID, GroupID: f.gid, Role: models.MembershipMember, Status: models.MembershipActive}))
			case "successor after a disable":
				apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(f.leadUser.ID), map[string]any{"status": models.UserDisabled, "successors": map[string]uint{itoa(f.gid): f.adminUser.ID}}, http.StatusOK)
			case "successor after an erasure":
				apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(f.leadUser.ID), map[string]any{"successors": map[string]uint{itoa(f.gid): f.adminUser.ID}}, http.StatusOK)
			}
			drainNotices(t, f.app)
			if path != "older add" {
				var g models.Group
				gormMust(t, f.app.DB.First(&g, f.gid))
				if g.DeletionRequested {
					t.Fatalf("%s left the request open", path)
				}
			}
			count := groupDeleteCount(t, f.other, f.gid)
			before := groupDeleteState(t, f.app, f.gid)
			w := f.other.do("DELETE", f.deletePath, nil, true)
			wantCode(t, "the card's delete after a "+path, w, http.StatusConflict, "already_handled")
			if !strings.Contains(w.Body.String(), `"resolution":"withdrawn"`) {
				t.Fatalf("the refusal: %s", w.Body.String())
			}
			after := groupDeleteState(t, f.app, f.gid)
			if want := openTasks.ReplaceAllString(strings.Replace(before, "requested=true", "requested=false", 1), "open tasks=0"); after != want {
				t.Fatalf("after the refused delete:\n%s\nwant\n%s", after, want)
			}
			task := findKind(feed(t, f.other, ""), notify.GroupDeletion)
			if task == nil || task.Waiting || task.Resolution == nil || task.Resolution.Outcome != "withdrawn" {
				t.Fatalf("the admins' task: %+v", task)
			}
			if got := groupDeleteCount(t, f.other, f.gid); got != count {
				t.Fatalf("the count moved from %d to %d", count, got)
			}
			wantCode(t, "Keep group afterwards", f.other.do("POST", f.keepPath, nil, true), http.StatusConflict, "already_handled")
		})
	}
}

// A leader's request reads its eligibility under the group's row lock, which
// every add takes: an add that commits while the request waits for the lock
// is counted, and the request is refused with the flag left clear.
func TestDeletionRequestCountsAnAddThatLandedWhileItWaited(t *testing.T) {
	f := newDeletionRequestFixture(t)
	apiDo(t, f.lead, "DELETE", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", nil, http.StatusOK)
	hold := f.app.DB.Begin()
	gormMust(t, hold)
	defer hold.Rollback()
	var g models.Group
	gormMust(t, hold.Raw("SELECT * FROM groups WHERE id = ? FOR UPDATE", f.gid).Scan(&g))
	asked := queueContact(func() *httptest.ResponseRecorder {
		return f.lead.do("POST", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", map[string]any{}, true)
	})
	waitLockWaiters(t, f.app)
	gormMust(t, hold.Create(&models.Membership{UserID: f.outsider.ID, GroupID: f.gid, Role: models.MembershipMember, Status: models.MembershipActive}))
	gormMust(t, hold.Commit())
	wantCode(t, "a request behind an add", awaitContactResponse(t, asked), http.StatusBadRequest, "group_has_members")
	var after models.Group
	gormMust(t, f.app.DB.First(&after, f.gid))
	if after.DeletionRequested {
		t.Fatal("the request was opened on a group with two members")
	}
}

// wantRequestWithdrawnAtTheCard presses the card's Delete as c and wants the
// refusal that withdraws the request: 409 with this resolution ("withdrawn",
// or "" when no task row is left to resolve), the group, its members, events,
// dates and registrations as they were, the flag clear and every task closed.
func wantRequestWithdrawnAtTheCard(t *testing.T, f deletionRequestFixture, c *apiClient, resolution string) {
	t.Helper()
	drainNotices(t, f.app)
	before := groupDeleteState(t, f.app, f.gid)
	w := c.do("DELETE", f.deletePath, nil, true)
	wantCode(t, "the card's delete", w, http.StatusConflict, "already_handled")
	if !strings.Contains(w.Body.String(), `"resolution":"`+resolution+`"`) || !strings.Contains(w.Body.String(), `"resolvedBy":"Fold"`) {
		t.Fatalf("the refusal: %s", w.Body.String())
	}
	want := openTasks.ReplaceAllString(strings.Replace(before, "requested=true", "requested=false", 1), "open tasks=0")
	if after := groupDeleteState(t, f.app, f.gid); after != want {
		t.Fatalf("after the refused delete:\n%s\nwant\n%s", after, want)
	}
}

// The request stands only for the person who asked: the group's one listed
// member must be the requester. A request that outlived its asker, with someone who did not ask
// as the one listed member, is withdrawn at the card and the group stays: a
// re-enabled member promoted to co-leader after which the asker left, and the
// state an older handoff left for a disable and for an erasure (the erased
// asker's task rows naming nobody, or gone).
func TestRequestCardRefusesARequestThatOutlivedItsAsker(t *testing.T) {
	for _, history := range []string{"promoted co-leader, asker left", "older handoff after a disable", "older handoff after an erasure", "older handoff, tasks gone", "nobody listed"} {
		t.Run(history, func(t *testing.T) {
			f := newDeletionRequestFixture(t)
			other := newTestUser(t, f.app, "outlived other", "password1234", models.RoleMember)
			if history == "promoted co-leader, asker left" {
				apiDo(t, f.lead, "DELETE", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", nil, http.StatusOK)
				apiDo(t, f.admin, "POST", "/api/v1/admin/groups/"+itoa(f.gid)+"/members", map[string]any{"userId": other.ID}, http.StatusOK)
				apiDo(t, f.admin, "PUT", "/api/v1/admin/users/"+itoa(other.ID), map[string]any{"status": models.UserDisabled}, http.StatusOK)
				apiDo(t, f.lead, "POST", "/api/v1/groups/"+itoa(f.gid)+"/deletion-request", map[string]any{}, http.StatusOK)
				// An older re-enable left the flag and open task in place.
				gormMust(t, f.app.DB.Model(other).Update("status", models.UserActive))
				apiDo(t, f.admin, "PUT", "/api/v1/groups/"+itoa(f.gid)+"/members/"+itoa(other.ID)+"/role", map[string]any{"role": models.MembershipLeader}, http.StatusOK)
				if w := confirmDeparture(t, f.lead, "POST", "/api/v1/groups/"+itoa(f.gid)+"/leave", "/api/v1/groups/"+itoa(f.gid)+"/departure-preview", ""); w.Code != http.StatusOK {
					t.Fatalf("the asker's leave: %d %s", w.Code, w.Body.String())
				}
			} else if history == "nobody listed" {
				// A request nobody is left to answer for: the asker's
				// membership ended outside any writer that withdraws it.
				gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leadUser.ID, f.gid).Update("status", models.MembershipLeft))
			} else {
				// What a handoff from before the card's rule left: the flag
				// set, the asker unlisted, the successor the one listed member.
				gormMust(t, f.app.DB.Create(&models.Membership{UserID: other.ID, GroupID: f.gid, Role: models.MembershipLeader, Status: models.MembershipActive}))
				switch history {
				case "older handoff after a disable":
					gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", f.leadUser.ID).Update("status", models.UserDisabled))
				default:
					gormMust(t, f.app.DB.Where("user_id = ? AND group_id = ?", f.leadUser.ID, f.gid).Delete(&models.Membership{}))
					gormMust(t, f.app.DB.Delete(&models.User{}, f.leadUser.ID))
					tasks := f.app.DB.Where("kind = ? AND subject_id = ?", notify.GroupDeletion, f.gid)
					if history == "older handoff after an erasure" {
						gormMust(t, tasks.Model(&models.Notification{}).Update("actor_id", nil))
					} else {
						gormMust(t, tasks.Delete(&models.Notification{}))
					}
				}
			}
			var g models.Group
			gormMust(t, f.app.DB.First(&g, f.gid))
			if !g.DeletionRequested {
				t.Fatal("fixture: the request is not open")
			}
			if n, _ := memberCount(f.app.DB, f.gid); n != 1 && history != "nobody listed" {
				t.Fatalf("fixture: %d listed members, want the one who did not ask", n)
			}
			resolution := "withdrawn"
			if history == "older handoff, tasks gone" {
				resolution = ""
			}
			wantRequestWithdrawnAtTheCard(t, f, f.other, resolution)
		})
	}
}

func TestRequestCardDeletesAnAdminLeadersOwnRequest(t *testing.T) {
	for _, presser := range []string{"another admin", "newly appointed admin", "the asker"} {
		t.Run(presser, func(t *testing.T) {
			app := testApp(t)
			asker := newTestUser(t, app, "lone admin", "password1234", models.RoleAdmin)
			askerClient := loginAPI(t, app, asker.Name, "password1234")
			press := askerClient
			if presser == "another admin" {
				newTestUser(t, app, "pressing admin", "password1234", models.RoleAdmin)
				press = loginAPI(t, app, "pressing admin", "password1234")
			}
			gid := createGroupAs(t, askerClient, "Admin's own", models.VisibilityPublic)
			apiDo(t, askerClient, "POST", "/api/v1/groups/"+itoa(gid)+"/deletion-request", map[string]any{}, http.StatusOK)
			var tasks int64
			gormMust(t, app.DB.Model(&models.Notification{}).Where("kind = ? AND subject_id = ?", notify.GroupDeletion, gid).Count(&tasks))
			if (tasks > 0) != (presser == "another admin") {
				t.Fatalf("fixture: %d task rows", tasks)
			}
			if presser == "newly appointed admin" {
				newTestUser(t, app, "late admin", "password1234", models.RoleAdmin)
				press = loginAPI(t, app, "late admin", "password1234")
			}
			apiDo(t, press, "DELETE", "/api/v1/admin/groups/"+itoa(gid)+"?deletionRequest=1", nil, http.StatusOK)
			if err := app.DB.First(&models.Group{}, gid).Error; err == nil {
				t.Fatal("the group is still there")
			}
		})
	}
}
