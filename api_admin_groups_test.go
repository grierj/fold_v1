package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	apiDo(t, f.admin, "POST", f.keepPath, nil, http.StatusOK)
	task := findKind(feed(t, f.other, ""), notify.GroupDeletion)
	if task == nil || task.Waiting || task.Resolution == nil || task.Resolution.Outcome != "kept" || task.Resolution.By != f.adminUser.Name {
		t.Fatalf("after keep: %+v", task)
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
