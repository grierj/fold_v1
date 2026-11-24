package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fold/internal/audit"
	"fold/models"

	"gorm.io/gorm"
)

// groupReadFixture is a group with a leader who is not an admin, a member who
// is going on the first of three weekly dates, and a teen member only the
// group's managers see. The dates have no place of their own, so they show
// the group's.
type groupReadFixture struct {
	app        *App
	adminUser  *models.User
	admin      *apiClient
	group      uint
	leaderUser *models.User
	leader     *apiClient
	memberUser *models.User
	member     *apiClient
	teen       *models.User
	event      eventJSON
	dates      []models.EventOccurrence
}

const (
	groupReadPlace = "Snapshot hall"
	groupReadTitle = "Snapshot supper"
)

func newGroupReadFixture(t *testing.T, visibility string) groupReadFixture {
	t.Helper()
	app := testApp(t)
	f := groupReadFixture{app: app}
	f.adminUser = newTestUser(t, app, "snapshot admin", "password1234", models.RoleAdmin)
	f.admin = loginAPI(t, app, f.adminUser.Name, "password1234")
	f.group = createFeedGroup(t, f.admin, "Snapshot circle", visibility, groupReadPlace)
	f.leaderUser, f.leader = groupLeader(t, app, f.group, "snapshot leader")
	f.memberUser = groupReadMember(t, f, "snapshot member")
	f.member = loginAPI(t, app, f.memberUser.Name, "password1234")
	f.teen = activeTeen(t, app, "snapshot teen", f.adminUser.ID)
	gormMust(t, app.DB.Create(&models.Membership{UserID: f.teen.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	f.event = createFeedEvent(t, f.admin, f.group, groupReadTitle, feedStart(7), "FREQ=WEEKLY;COUNT=3")
	gormMust(t, app.DB.Where("event_id = ?", f.event.ID).Order("starts_at").Find(&f.dates))
	if len(f.dates) != 3 {
		t.Fatalf("fixture: %d dates", len(f.dates))
	}
	gormMust(t, app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	return f
}

// groupReadMember is a new active member of the fixture's group.
func groupReadMember(t *testing.T, f groupReadFixture, name string) *models.User {
	t.Helper()
	u := newTestUser(t, f.app, name, "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Membership{UserID: u.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
	return u
}

// groupReadRoutes are the three group reads, each with its readerAuthorized
// surface.
var groupReadRoutes = []struct{ surface, suffix string }{
	{"group detail", groupDetailSuffix},
	{"group events", "/events"},
	{"group members", "/members"},
}

// groupDetailSuffix is the detail route's query as the current client sends it.
const groupDetailSuffix = "?" + groupDatePaging

func groupReadPath(group uint, suffix string) string {
	return "/api/v1/groups/" + itoa(group) + suffix
}

// atGroupReadBegin runs commit once, inside the next group read's
// transaction before its first statement.
func atGroupReadBegin(app *App, commit func()) *atomic.Bool {
	var fired atomic.Bool
	app.hooks.groupReadBegin = func() {
		if fired.CompareAndSwap(false, true) {
			commit()
		}
	}
	return &fired
}

// TestGroupReadsJudgeAuthorityInTheirSnapshot: each group read judges the
// group, its roster and its management, and reads what it shows, in one
// snapshot. Right after the judgment another connection demotes the leader,
// adds a second teen, signs a new person up, retitles the first date and moves
// the group's place, in one commit. The answer is byte for byte the answer
// before that commit; the next read shows the commit and none of the
// manager's fields.
func TestGroupReadsJudgeAuthorityInTheirSnapshot(t *testing.T) {
	for _, route := range groupReadRoutes {
		t.Run(route.surface, func(t *testing.T) {
			f := newGroupReadFixture(t, models.VisibilityInviteOnly)
			newTeen := activeTeen(t, f.app, "seam teen", f.adminUser.ID)
			joiner := newTestUser(t, f.app, "seam joiner", "password1234", models.RoleMember)
			const retitled, moved = "Retitled at the seam", "Moved at the seam"
			path := groupReadPath(f.group, route.suffix)
			before := apiDo(t, f.leader, "GET", path, nil, http.StatusOK)
			if route.suffix != "/events" {
				wantIn(t, "the manager's answer", before, f.teen.PublicName())
			}
			if route.suffix != "/members" {
				wantIn(t, "the manager's answer", before, `"revision":"1.`, groupReadPlace, f.memberUser.PublicName())
			}
			fired := atReaderSeam(f.app, "authorized", route.surface, func() {
				if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
					for _, step := range []*gorm.DB{
						tx.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipMember),
						tx.Create(&models.Membership{UserID: newTeen.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}),
						tx.Create(&models.Registration{UserID: &joiner.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}),
						tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[0].ID).Update("title_override", retitled),
						tx.Model(&models.Group{}).Where("id = ?", f.group).Update("location", moved),
					} {
						if step.Error != nil {
							return step.Error
						}
					}
					return nil
				}); err != nil {
					t.Error(err)
				}
			})
			got := apiDo(t, f.leader, "GET", path, nil, http.StatusOK)
			if !fired.Load() {
				t.Fatal("the read never reached its seam")
			}
			if got != before {
				t.Fatalf("the read straddled the commit:\nbefore %s\ngot    %s", before, got)
			}
			after := apiDo(t, f.leader, "GET", path, nil, http.StatusOK)
			wantOut(t, "the demoted leader's next answer", after, f.teen.PublicName(), newTeen.PublicName(), `"revision"`, `"canManage":true`)
			if route.suffix != "/members" {
				wantIn(t, "the next answer", after, retitled, moved)
			}
		})
	}
}

// TestGroupReadsJudgeTheSessionInTheirSnapshot: a session that ends after the
// middleware accepted it and before the snapshot's first statement reads as
// signed out: the optional reads answer exactly what a signed-out visitor
// gets (the hidden group's 404 included), and the roster answers 401. Each
// session epoch, a disabled account and an erased one are tried. A session
// that ends after the judgment leaves the answer the snapshot's, and the next
// read with the old cookie has lost the access.
func TestGroupReadsJudgeTheSessionInTheirSnapshot(t *testing.T) {
	ends := []struct {
		name string
		end  func(t *testing.T, f groupReadFixture, u *models.User)
	}{
		{"session version", func(t *testing.T, f groupReadFixture, u *models.User) {
			gormMust(t, f.app.DB.Model(u).Update("session_version", sessionVersionBump()))
		}},
		{"logout version", func(t *testing.T, f groupReadFixture, u *models.User) {
			gormMust(t, f.app.DB.Model(u).Update("logout_version", gorm.Expr("logout_version + 1")))
		}},
		{"disabled", func(t *testing.T, f groupReadFixture, u *models.User) {
			gormMust(t, f.app.DB.Model(u).Update("status", models.UserDisabled))
		}},
		{"erased", func(t *testing.T, f groupReadFixture, u *models.User) {
			apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(u.ID), nil, http.StatusOK)
		}},
	}
	for _, end := range ends {
		for _, vis := range []string{models.VisibilityPublic, models.VisibilityInviteOnly} {
			t.Run(end.name+"/"+vis, func(t *testing.T) {
				f := newGroupReadFixture(t, vis)
				anonymous := newAPIClient(t, f.app)
				for _, route := range groupReadRoutes {
					u := groupReadMember(t, f, "ending "+strings.ReplaceAll(route.surface, " ", "-"))
					c := loginAPI(t, f.app, u.Name, "password1234")
					path := groupReadPath(f.group, route.suffix)
					wantIn(t, "the member's answer", apiDo(t, c, "GET", path, nil, http.StatusOK), f.memberUser.PublicName())
					fired := atGroupReadBegin(f.app, func() { end.end(t, f, u) })
					w := c.do("GET", path, nil, false)
					if !fired.Load() {
						t.Fatal("the read never reached its seam")
					}
					if route.suffix == "/members" {
						if w.Code != http.StatusUnauthorized || answerCode(t, w) != "signed_out" {
							t.Fatalf("%s with an ended session: %d %s", path, w.Code, w.Body.String())
						}
						continue
					}
					anon := anonymous.do("GET", path, nil, false)
					if w.Code != anon.Code || w.Body.String() != anon.Body.String() {
						t.Fatalf("%s with an ended session is not the signed-out answer:\ngot  %d %s\nanon %d %s", path, w.Code, w.Body.String(), anon.Code, anon.Body.String())
					}
					if want := map[string]int{models.VisibilityPublic: http.StatusOK, models.VisibilityInviteOnly: http.StatusNotFound}[vis]; w.Code != want {
						t.Fatalf("%s: %d, want %d", path, w.Code, want)
					}
				}
			})
		}
	}

	t.Run("after the judgment", func(t *testing.T) {
		f := newGroupReadFixture(t, models.VisibilityInviteOnly)
		for _, route := range groupReadRoutes {
			_, c := groupLeader(t, f.app, f.group, "judged "+strings.ReplaceAll(route.surface, " ", "-"))
			var u models.User
			gormMust(t, f.app.DB.Where("name = ?", "judged "+strings.ReplaceAll(route.surface, " ", "-")).First(&u))
			path := groupReadPath(f.group, route.suffix)
			before := apiDo(t, c, "GET", path, nil, http.StatusOK)
			marker := "Marker after " + route.surface
			fired := atReaderSeam(f.app, "authorized", route.surface, func() {
				if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
					if err := tx.Model(&u).Update("session_version", sessionVersionBump()).Error; err != nil {
						return err
					}
					return tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[0].ID).Update("title_override", marker).Error
				}); err != nil {
					t.Error(err)
				}
			})
			if got := apiDo(t, c, "GET", path, nil, http.StatusOK); !fired.Load() || got != before {
				t.Fatalf("%s: the snapshot's answer changed with a commit after its judgment (fired %t):\nbefore %s\ngot    %s", path, fired.Load(), before, got)
			}
			want := http.StatusNotFound
			if route.suffix == "/members" {
				want = http.StatusUnauthorized
			}
			apiDo(t, c, "GET", path, nil, want)
		}
	})
}

// TestGroupReadsShowAnErasureWhole: a subject erased by the real pipeline right
// after a manager's group read is judged is wholly there in that answer, on
// each route, and wholly gone from the next read of every route, while a
// survivor's change committed with it shows only in the next read. The
// subject is a plain member, or a leader who is not the group's sole leader
// and is its shown leader, holding a household whose adult's row was not
// restored (the recovery evidence a manager sees). The manager's standing and
// the first date's revision stay as they were; the shown leader becomes the
// one the pipeline names.
func TestGroupReadsShowAnErasureWhole(t *testing.T) {
	for _, leads := range []bool{false, true} {
		for _, route := range groupReadRoutes {
			name := "member/" + route.surface
			if leads {
				name = "co-leader/" + route.surface
			}
			t.Run(name, func(t *testing.T) { groupReadErasure(t, route.surface, route.suffix, leads) })
		}
	}
}

func groupReadErasure(t *testing.T, surface, suffix string, leads bool) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	subject := groupReadMember(t, f, "erasable subject")
	const phone, care = "+1 555 0100 777", "Erasable care note"
	gormMust(t, f.app.DB.Model(subject).Update("phone", phone))
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &subject.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	h := models.Household{Name: "Erasable household", CreatedBy: subject.ID}
	gormMust(t, f.app.DB.Create(&h))
	gormMust(t, f.app.DB.Create(&models.HouseholdAccount{HouseholdID: h.ID, UserID: subject.ID, Relation: models.HouseholdRelationGuardian}))
	birth := time.Now().UTC().AddDate(-7, 0, 0)
	child := models.HouseholdPerson{HouseholdID: h.ID, FirstName: "Eli", LastName: "Erasablechild", Kind: models.HouseholdChild, BirthDate: &birth, CareNotes: care, CreatedBy: subject.ID}
	gormMust(t, f.app.DB.Create(&child))
	gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: subject.ID, PersonID: &child.ID, Status: models.GuardianConfirmed, ConfirmedBy: &f.adminUser.ID}))
	gormMust(t, f.app.DB.Create(&models.Registration{HouseholdPersonID: &child.ID, RegisteredBy: &subject.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	secrets := []string{subject.PublicName(), subject.Email, phone, child.FirstName, child.LastName, care, h.Name}
	var recoverable models.HouseholdPerson
	if leads {
		gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", subject.ID, f.group).Update("role", models.MembershipLeader))
		gormMust(t, f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("leader_id", subject.ID))
		adult := time.Now().UTC().AddDate(-30, 0, 0)
		recoverable = models.HouseholdPerson{HouseholdID: h.ID, FirstName: "Ruth", LastName: "Erasablerecovery", Kind: models.HouseholdAdult, BirthDate: &adult, CreatedBy: subject.ID}
		gormMust(t, f.app.DB.Create(&recoverable))
		meta, err := json.Marshal(droppedRegistrationMeta{PersonID: &recoverable.ID, RegisteredBy: &subject.ID, OccurrenceID: f.dates[0].ID, AttendanceMode: models.AttendanceInPerson})
		gormMust(t, &gorm.DB{Error: err})
		gormMust(t, f.app.DB.Create(&models.AuditLog{Action: audit.RegistrationNotRestored, TargetType: "event", TargetID: f.event.ID, MetaJSON: string(meta)}))
		secrets = append(secrets, recoverable.FirstName, recoverable.LastName)
	}
	// The care notes show to a manager inside their window.
	newMutableClock(f.app, f.dates[0].StartsAt.Add(-time.Hour))
	defer func() { f.app.clock = nil }()
	const survivor = "Survivor's retitled date"
	shown := secrets
	if suffix == "/members" {
		shown = secrets[:3]
	}

	path := groupReadPath(f.group, suffix)
	detail := groupDetailPath(f.group)
	readDetail := func(t *testing.T) (groupDetailJSON, string) {
		t.Helper()
		raw := apiDo(t, f.leader, "GET", detail, nil, http.StatusOK)
		var got groupDetailJSON
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &got)})
		return got, raw
	}
	was, raw := readDetail(t)
	if leads {
		if was.Group.LeaderName != subject.PublicName() {
			t.Fatalf("fixture: the shown leader is %q: %s", was.Group.LeaderName, raw)
		}
		if len(was.UpcomingEvents) == 0 || !strings.Contains(raw, `"status":"not_restored"`) {
			t.Fatalf("fixture: the manager sees no recovery evidence: %s", raw)
		}
	}
	firstRevision := func(d groupDetailJSON) string {
		for _, o := range d.UpcomingEvents {
			if o.ID == f.dates[0].ID {
				return o.Revision
			}
		}
		return ""
	}
	before := apiDo(t, f.leader, "GET", path, nil, http.StatusOK)
	wantIn(t, "the manager's answer", before, shown...)
	fired := atReaderSeam(f.app, "authorized", surface, func() {
		apiDo(t, f.admin, "DELETE", "/api/v1/admin/users/"+itoa(subject.ID), nil, http.StatusOK)
		if err := f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Update("title_override", survivor).Error; err != nil {
			t.Error(err)
		}
	})
	if got := apiDo(t, f.leader, "GET", path, nil, http.StatusOK); !fired.Load() || got != before {
		t.Fatalf("the read straddled the erasure (fired %t):\nbefore %s\ngot    %s", fired.Load(), before, got)
	}
	f.app.hooks.readerAuthorized = nil
	ids := []string{fmt.Sprintf(`"id":%d,`, subject.ID), fmt.Sprintf(`"userId":%d,`, subject.ID), fmt.Sprintf(`"personId":%d,`, child.ID)}
	if leads {
		ids = append(ids, fmt.Sprintf(`"personId":%d,`, recoverable.ID), `"not_restored"`)
	}
	for _, next := range groupReadRoutes {
		raw := apiDo(t, f.leader, "GET", groupReadPath(f.group, next.suffix), nil, http.StatusOK)
		wantOut(t, next.surface+" after the erasure", raw, secrets...)
		wantOut(t, next.surface+" after the erasure", raw, ids...)
		wantIn(t, next.surface+" after the erasure", raw, f.memberUser.PublicName())
		if next.suffix != "/members" {
			wantIn(t, next.surface+" after the erasure", raw, survivor, `"revision":"1.`)
		}
	}
	now, raw := readDetail(t)
	var g models.Group
	gormMust(t, f.app.DB.First(&g, f.group))
	var shownLeader models.User
	gormMust(t, f.app.DB.First(&shownLeader, g.LeaderID))
	if !now.CanManage || now.Group.MemberCount != was.Group.MemberCount-1 || now.Group.LeaderName != shownLeader.PublicName() || g.LeaderID == subject.ID {
		t.Fatalf("after the erasure: canManage %t, %d members (was %d), leader %q (the row names %d, %q): %s", now.CanManage, now.Group.MemberCount, was.Group.MemberCount, now.Group.LeaderName, g.LeaderID, shownLeader.PublicName(), raw)
	}
	if !leads && now.Group.LeaderName != was.Group.LeaderName {
		t.Fatalf("a member's erasure moved the shown leader: %q, was %q", now.Group.LeaderName, was.Group.LeaderName)
	}
	if r := firstRevision(now); r == "" || r != firstRevision(was) {
		t.Fatalf("the first date's revision: %q, was %q", r, firstRevision(was))
	}
}

// TestGroupReadsUseOneConnection: every read of the three group routes, a
// later page and a role change's roster reply goes through its one snapshot.
// On a pool of one connection a read on a.DB beside the snapshot waits for the
// connection the snapshot holds and fails at the context's deadline; a
// lenient read that escaped would answer 200 with a default, so each optional
// value is checked for its real value: the room, seats, remote seats,
// childcare, a guardian's view of a teen's contact, care notes, the
// recoverable row, a household's waitlist place and the next date.
func TestGroupReadsUseOneConnection(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	first := f.dates[0]
	day := models.FormatCalendarDate(first.OriginalDate)
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Updates(map[string]any{"remote_available": true, "childcare_available": true, "childcare_capacity": 10}))
	if w := f.admin.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": true}, true); w.Code != http.StatusOK {
		t.Fatalf("rooms on: %d %s", w.Code, w.Body.String())
	}
	w := f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Snapshot Room", "capacity": 40}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("room: %d %s", w.Code, w.Body.String())
	}
	var room struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, w, &room)
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d/dates/%s/room", f.event.ID, day), map[string]any{"roomId": room.Room.ID}, http.StatusOK)

	// The leader is the roster teen's confirmed guardian: a teen's contact
	// shows only through that link, on the roster and on the date.
	roster := seedRosterPeople(t, f.app, f.group, f.event.ID, first.ID, f.leaderUser, f.adminUser)
	remote := groupReadMember(t, f, "snapshot remote")
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &remote.ID, OccurrenceID: first.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceRemote, Source: "web"}))
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("user_id = ? AND occurrence_id = ?", f.memberUser.ID, first.ID).Update("childcare_count", 2))
	// The holder's child waits for the second date behind one person.
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Update("capacity_override", 1))
	ahead, behind := time.Now().UTC().Add(-2*time.Minute), time.Now().UTC().Add(-time.Minute)
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.memberUser.ID, OccurrenceID: f.dates[1].ID, Status: models.RegWaitlisted, WaitlistedAt: &ahead, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	gormMust(t, f.app.DB.Create(&models.Registration{HouseholdPersonID: &roster.child.ID, RegisteredBy: &roster.holder.ID, OccurrenceID: f.dates[1].ID, Status: models.RegWaitlisted, WaitlistedAt: &behind, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	holderC := loginAPI(t, f.app, roster.holder.Name, "password1234")
	newMutableClock(f.app, first.StartsAt.Add(-time.Hour))
	defer func() { f.app.clock = nil }()

	one := oneConnectionPool(t, f.app)
	f.app.DB = one
	read := func(t *testing.T, c *apiClient, path string) string {
		t.Helper()
		oneConnection(t, f.app, one)
		w := c.do("GET", path, nil, false)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s (a read outside the snapshot waits for the one connection)", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	dateOf := func(t *testing.T, dates []occurrenceJSON, raw string, id uint) occurrenceJSON {
		t.Helper()
		for _, d := range dates {
			if d.ID == id {
				return d
			}
		}
		t.Fatalf("date %d missing: %s", id, raw)
		return occurrenceJSON{}
	}
	wantManagerDate := func(t *testing.T, d occurrenceJSON, raw string) {
		t.Helper()
		if d.Room == nil || d.Room.Name != "Snapshot Room" || d.SeatsUsed < 3 || d.RemoteSeatsUsed != 1 || d.ChildcareUsed == nil || *d.ChildcareUsed < 2 || d.Revision == "" {
			t.Fatalf("the manager's date lost an optional value: %s", raw)
		}
		wantRosterPeople(t, d, raw, roster)
	}

	t.Run("detail", func(t *testing.T) {
		raw := read(t, f.leader, groupDetailPath(f.group))
		var got groupDetailJSON
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &got)})
		if !got.CanManage || got.Group.NextEventAt == nil || !got.Group.NextEventAt.Equal(first.StartsAt) || got.Group.LeaderName == "" {
			t.Fatalf("detail summary: %s", raw)
		}
		wantIn(t, "the manager's roster", raw, f.teen.PublicName(), roster.teenEmail)
		wantManagerDate(t, dateOf(t, got.UpcomingEvents, raw, first.ID), raw)
	})
	t.Run("events", func(t *testing.T) {
		raw := read(t, f.leader, groupReadPath(f.group, "/events"))
		var got groupEventsJSON
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &got)})
		wantManagerDate(t, dateOf(t, got.Events, raw, first.ID), raw)
	})
	t.Run("members", func(t *testing.T) {
		wantIn(t, "the manager's roster", read(t, f.leader, groupReadPath(f.group, "/members")), f.teen.PublicName(), roster.teenEmail)
	})
	t.Run("later page and a household's waitlist place", func(t *testing.T) {
		raw := read(t, holderC, groupDetailPath(f.group, "limit=1"))
		var page groupDetailJSON
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &page)})
		if len(page.UpcomingEvents) != 1 || !page.HasMore || page.NextCursor == "" {
			t.Fatalf("first page: %s", raw)
		}
		raw = read(t, holderC, groupDetailPath(f.group, "limit=1", "cursor="+page.NextCursor))
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &page)})
		d := dateOf(t, page.UpcomingEvents, raw, f.dates[1].ID)
		if len(d.MyHouseholdRegistrations) != 1 || d.MyHouseholdRegistrations[0].WaitlistPosition == nil || *d.MyHouseholdRegistrations[0].WaitlistPosition != 2 {
			t.Fatalf("the household's waitlist place: %s", raw)
		}
		raw = read(t, f.member, groupReadPath(f.group, "/events?limit=1&cursor="+page.NextCursor))
		var events groupEventsJSON
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &events)})
		if len(events.Events) != 1 || events.Events[0].ID != f.dates[2].ID {
			t.Fatalf("events after the cursor: %s", raw)
		}
		raw = read(t, f.member, groupReadPath(f.group, "/events?from="+models.FormatCalendarDate(f.dates[1].OriginalDate)))
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &events)})
		d = dateOf(t, events.Events, raw, f.dates[1].ID)
		if d.MyWaitlistPosition == nil || *d.MyWaitlistPosition != 1 {
			t.Fatalf("the member's waitlist place: %s", raw)
		}
	})
	t.Run("role change reply", func(t *testing.T) {
		oneConnection(t, f.app, one)
		w := f.leader.do("PUT", groupReadPath(f.group, "/members/"+itoa(remote.ID)+"/role"), map[string]any{"role": "leader"}, true)
		if w.Code != http.StatusOK {
			t.Fatalf("role change: %d %s", w.Code, w.Body.String())
		}
		wantIn(t, "the role change's reply", w.Body.String(), f.teen.PublicName(), roster.teenEmail)
	})
}

// TestGroupReadsDecideOnOneClock: a group read takes its clock once, and
// counts household visibility and ages from the church's date at that clock,
// with the zone read in its snapshot. An adult-kind person turns 18 on church
// date B: the day before, a leader does not see them on the group's dates and
// the admin sees 17; on B, the leader sees them and the admin sees 18. Moving
// the clock past the first date's start, or the zone to Auckland, after the
// judgment changes nothing in that answer. A stored zone that is not a zone
// fails a page with dates, and not one without; no zone at all is UTC.
func TestGroupReadsDecideOnOneClock(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	la, err := time.LoadLocation("America/Los_Angeles")
	gormMust(t, &gorm.DB{Error: err})
	gormMust(t, &gorm.DB{Error: setSettingIn(f.app.DB, settingTimezone, "America/Los_Angeles")})
	occ := f.dates[0]
	holder := groupReadMember(t, f, "birthday holder")
	h := models.Household{Name: "Birthday household", CreatedBy: holder.ID}
	gormMust(t, f.app.DB.Create(&h))
	gormMust(t, f.app.DB.Create(&models.HouseholdAccount{HouseholdID: h.ID, UserID: holder.ID, Relation: models.HouseholdRelationAdult}))
	birth := time.Now().UTC().AddDate(-30, 0, 0)
	person := models.HouseholdPerson{HouseholdID: h.ID, FirstName: "Bea", LastName: "Groupbirthday", Kind: models.HouseholdAdult, BirthDate: &birth, CreatedBy: holder.ID}
	gormMust(t, f.app.DB.Create(&person))
	gormMust(t, f.app.DB.Create(&models.Registration{HouseholdPersonID: &person.ID, RegisteredBy: &holder.ID, OccurrenceID: occ.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))

	wall := time.Now().In(la)
	day := func(offset int) time.Time {
		return time.Date(wall.Year(), wall.Month(), wall.Day()+offset, 0, 0, 0, 0, la)
	}
	clock := newMutableClock(f.app, time.Now())
	defer func() { f.app.clock = nil }()
	// set pins the clock at, turns the person 18 on b, and puts the first
	// date an hour after the clock.
	set := func(t *testing.T, at, b time.Time) {
		t.Helper()
		if b.Month() == time.February && b.Day() == 29 {
			t.Skip("an 18th birthday on February 29 has no 18-years-earlier date")
		}
		born := time.Date(b.Year()-18, b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
		gormMust(t, f.app.DB.Model(&models.HouseholdPerson{}).Where("id = ?", person.ID).Update("birth_date", born))
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", occ.ID).Updates(map[string]any{"starts_at": at.Add(time.Hour).UTC(), "ends_at": at.Add(3 * time.Hour).UTC(), "original_date": models.FormatCalendarDate(at.UTC())}))
		clock.set(at)
	}
	firstDate := func(t *testing.T, c *apiClient, suffix string) (*occurrenceJSON, string) {
		t.Helper()
		raw := apiDo(t, c, "GET", groupReadPath(f.group, suffix), nil, http.StatusOK)
		var page struct {
			UpcomingEvents []occurrenceJSON `json:"upcomingEvents"`
			Events         []occurrenceJSON `json:"events"`
		}
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &page)})
		for _, d := range append(page.UpcomingEvents, page.Events...) {
			if d.ID == occ.ID {
				return &d, raw
			}
		}
		return nil, raw
	}
	ageOf := func(d *occurrenceJSON) *int {
		for _, row := range d.Attendees {
			if row.HouseholdPerson != nil && row.HouseholdPerson.PersonID == person.ID {
				return row.HouseholdPerson.Age
			}
		}
		return nil
	}
	want := func(t *testing.T, label string, leaderSees bool, age int) {
		t.Helper()
		for _, suffix := range []string{groupDetailSuffix, "/events?from=" + models.FormatCalendarDate(day(-3))} {
			d, raw := firstDate(t, f.leader, suffix)
			if d == nil || !strings.Contains(raw, f.memberUser.PublicName()) {
				t.Fatalf("%s %q: the leader's read has no roster: %s", label, suffix, raw)
			}
			if strings.Contains(raw, person.FirstName) != leaderSees {
				t.Fatalf("%s %q: the leader sees the person %t, want %t: %s", label, suffix, !leaderSees, leaderSees, raw)
			}
			d, raw = firstDate(t, f.admin, suffix)
			if d == nil || ageOf(d) == nil || *ageOf(d) != age {
				t.Fatalf("%s %q: the admin's read, want age %d: %s", label, suffix, age, raw)
			}
		}
	}

	t.Run("the day before, at church midnight", func(t *testing.T) {
		set(t, day(-2).Add(23*time.Hour+30*time.Minute), day(-1))
		want(t, "before", false, 17)
	})
	t.Run("on the birthday", func(t *testing.T) {
		set(t, day(2).Add(30*time.Minute), day(2))
		want(t, "birthday", true, 18)
	})
	t.Run("the zone and the clock are read once", func(t *testing.T) {
		at := day(-2).Add(23*time.Hour + 30*time.Minute)
		set(t, at, day(-1))
		defer func() {
			f.app.hooks.readerAuthorized = nil
			gormMust(t, &gorm.DB{Error: setSettingIn(f.app.DB, settingTimezone, "America/Los_Angeles")})
		}()
		for _, c := range []*apiClient{f.leader, f.admin} {
			clock.set(at)
			f.app.hooks.readerAuthorized = func(surface string) {
				if surface != "group detail" {
					return
				}
				// Auckland's date at this instant is already the birthday,
				// and the clock passes the date's start.
				if err := setSettingIn(f.app.DB, settingTimezone, "Pacific/Auckland"); err != nil {
					t.Error(err)
				}
				clock.set(at.Add(2 * time.Hour))
			}
			raw := apiDo(t, c, "GET", groupDetailPath(f.group), nil, http.StatusOK)
			f.app.hooks.readerAuthorized = nil
			gormMust(t, &gorm.DB{Error: setSettingIn(f.app.DB, settingTimezone, "America/Los_Angeles")})
			var got groupDetailJSON
			gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &got)})
			if got.Group.NextEventAt == nil || !got.Group.NextEventAt.Equal(at.Add(time.Hour).UTC()) {
				t.Fatalf("the next date moved with a clock set after the judgment: %s", raw)
			}
			var d *occurrenceJSON
			for i := range got.UpcomingEvents {
				if got.UpcomingEvents[i].ID == occ.ID {
					d = &got.UpcomingEvents[i]
				}
			}
			if d == nil {
				t.Fatalf("the first date left the list with a clock set after the judgment: %s", raw)
			}
			if c == f.leader && strings.Contains(raw, person.FirstName) {
				t.Fatalf("the leader's read took the zone committed after its judgment: %s", raw)
			}
			if c == f.admin && (ageOf(d) == nil || *ageOf(d) != 17) {
				t.Fatalf("the admin's read took the zone committed after its judgment: %s", raw)
			}
		}
	})
	t.Run("a stored zone that is not one", func(t *testing.T) {
		gormMust(t, f.app.DB.Exec("UPDATE settings SET value = 'Local' WHERE key = ?", settingTimezone))
		defer func() { gormMust(t, &gorm.DB{Error: setSettingIn(f.app.DB, settingTimezone, "America/Los_Angeles")}) }()
		for _, suffix := range []string{groupDetailSuffix, "/events"} {
			w := f.leader.do("GET", groupReadPath(f.group, suffix), nil, false)
			if w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
				t.Fatalf("%q with a bad zone: %d %s", suffix, w.Code, w.Body.String())
			}
		}
		apiDo(t, f.leader, "GET", groupReadPath(f.group, "/members"), nil, http.StatusOK)
		apiDo(t, f.leader, "GET", groupReadPath(f.group, "/events?from=2000-01-01&to=2000-01-02"), nil, http.StatusOK)
		gormMust(t, f.app.DB.Exec("DELETE FROM settings WHERE key = ?", settingTimezone))
		apiDo(t, f.leader, "GET", groupDetailPath(f.group), nil, http.StatusOK)
	})
}

// failStatement makes PostgreSQL itself fail every statement match picks, by
// replacing its SQL with a division by zero, so a read outside a savepoint
// aborts its transaction for every read after it.
func failStatement(t *testing.T, app *App, match func(tx *gorm.DB) bool) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	name := fmt.Sprintf("test:fail_statement:%d", queryCounterSeq.Add(1))
	fn := func(tx *gorm.DB) {
		if match(tx) {
			n.Add(1)
			tx.Statement.SQL.Reset()
			tx.Statement.SQL.WriteString("SELECT 1/0")
			tx.Statement.Vars = nil
		}
	}
	gormMust(t, &gorm.DB{Error: app.DB.Callback().Query().Before("gorm:query").Register(name, fn)})
	gormMust(t, &gorm.DB{Error: app.DB.Callback().Row().Before("gorm:row").Register(name, fn)})
	t.Cleanup(func() {
		_ = app.DB.Callback().Query().Remove(name)
		_ = app.DB.Callback().Row().Remove(name)
	})
	return &n
}

func selects(tx *gorm.DB, part string) bool {
	return strings.Contains(strings.Join(tx.Statement.Selects, " "), part)
}

// TestGroupReadFailuresStayWhole: a real PostgreSQL error in a read the group
// list shows a default for (seats, remote seats, childcare, the room, a
// guardian's link) rolls back only its savepoint: the page answers with that
// default and every read after it. A failed required read (the counts, the
// household facts, the roster) answers the generic 500 and nothing of the
// page.
func TestGroupReadFailuresStayWhole(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	first := f.dates[0]
	gormMust(t, f.app.DB.Model(&models.Event{}).Where("id = ?", f.event.ID).Updates(map[string]any{"remote_available": true, "childcare_available": true, "childcare_capacity": 10}))
	gormMust(t, f.app.DB.Model(&models.Registration{}).Where("user_id = ? AND occurrence_id = ?", f.memberUser.ID, first.ID).Update("childcare_count", 2))
	remote := groupReadMember(t, f, "failure remote")
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &remote.ID, OccurrenceID: first.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceRemote, Source: "web"}))
	// The leader is the teen's confirmed guardian, the only way a teen's
	// contact shows on the roster and the date.
	gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.teen.ID, OccurrenceID: first.ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	gormMust(t, f.app.DB.Create(&models.GuardianLink{GuardianUserID: f.leaderUser.ID, TeenUserID: &f.teen.ID, Status: models.GuardianConfirmed, ConfirmedBy: &f.adminUser.ID}))
	if w := f.admin.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": true}, true); w.Code != http.StatusOK {
		t.Fatalf("rooms on: %d %s", w.Code, w.Body.String())
	}
	w := f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Failure Room", "capacity": 40}, true)
	var room struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, w, &room)
	apiDo(t, f.admin, "PUT", fmt.Sprintf("/api/v1/events/%d/dates/%s/room", f.event.ID, models.FormatCalendarDate(first.OriginalDate)), map[string]any{"roomId": room.Room.ID}, http.StatusOK)

	date := func(t *testing.T) (occurrenceJSON, string) {
		t.Helper()
		raw := apiDo(t, f.leader, "GET", groupDetailPath(f.group), nil, http.StatusOK)
		var got groupDetailJSON
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &got)})
		if got.Group.NextEventAt == nil || len(got.UpcomingEvents) != 3 || got.UpcomingEvents[0].ID != first.ID ||
			!strings.Contains(raw, f.memberUser.PublicName()) || !strings.Contains(raw, f.teen.PublicName()) {
			t.Fatalf("a read after the failed one lost its rows: %s", raw)
		}
		return got.UpcomingEvents[0], raw
	}
	if d, raw := date(t); d.SeatsUsed != 2 || d.RemoteSeatsUsed != 1 || d.ChildcareUsed == nil || *d.ChildcareUsed != 2 || d.Room == nil || !strings.Contains(raw, f.teen.Email) {
		t.Fatalf("fixture: %s", raw)
	}

	for _, c := range []struct {
		name  string
		match func(*gorm.DB) bool
		check func(d occurrenceJSON, raw string) bool
	}{
		{"seats", func(tx *gorm.DB) bool {
			return selects(tx, "AS seats") && strings.Contains(fmt.Sprint(tx.Statement.Clauses["WHERE"].Expression), models.AttendanceInPerson)
		}, func(d occurrenceJSON, _ string) bool {
			return d.SeatsUsed == 0 && d.RemoteSeatsUsed == 1 && d.Room != nil
		}},
		{"remote seats", func(tx *gorm.DB) bool {
			return selects(tx, "AS seats") && strings.Contains(fmt.Sprint(tx.Statement.Clauses["WHERE"].Expression), models.AttendanceRemote)
		}, func(d occurrenceJSON, _ string) bool { return d.SeatsUsed == 2 && d.RemoteSeatsUsed == 0 }},
		{"childcare", func(tx *gorm.DB) bool { return selects(tx, "AS children") }, func(d occurrenceJSON, _ string) bool {
			return d.ChildcareUsed != nil && *d.ChildcareUsed == 0 && d.SeatsUsed == 2
		}},
		{"room", func(tx *gorm.DB) bool { return strings.Contains(tx.Statement.SQL.String(), "FROM room_bookings") }, func(d occurrenceJSON, _ string) bool {
			return d.Room == nil && d.SeatsUsed == 2
		}},
		// A failed guardian lookup grants nothing: the teen stays listed
		// without their contact.
		{"guardian link", func(tx *gorm.DB) bool {
			return tx.Statement.Table == "guardian_links" && tx.Statement.SQL.Len() == 0 && guardianLinkQuery(tx)
		}, func(d occurrenceJSON, raw string) bool {
			return !strings.Contains(raw, f.teen.Email) && d.SeatsUsed == 2
		}},
	} {
		t.Run("optional "+c.name, func(t *testing.T) {
			n := failStatement(t, f.app, c.match)
			d, raw := date(t)
			if n.Load() == 0 {
				t.Fatal("the failure was not exercised")
			}
			if !c.check(d, raw) {
				t.Fatalf("no default after the failed %s read: %s", c.name, raw)
			}
		})
	}

	for _, c := range []struct {
		name  string
		match func(*gorm.DB) bool
	}{
		{"counts", func(tx *gorm.DB) bool { return selects(tx, "count(*) as n") }},
		{"household facts", func(tx *gorm.DB) bool { return tx.Statement.Table == "household_accounts" }},
		{"roster", func(tx *gorm.DB) bool {
			return tx.Statement.Table == "memberships" && strings.Contains(fmt.Sprint(tx.Statement.Joins), "users")
		}},
	} {
		t.Run("required "+c.name, func(t *testing.T) {
			n := failStatement(t, f.app, c.match)
			w := f.leader.do("GET", groupDetailPath(f.group), nil, false)
			if n.Load() == 0 {
				t.Fatal("the failure was not exercised")
			}
			if w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
				t.Fatalf("a failed %s read: %d %s", c.name, w.Code, w.Body.String())
			}
		})
	}
}

// guardianLinkQuery is whether tx is confirmedGuardianLinks' read.
func guardianLinkQuery(tx *gorm.DB) bool {
	for _, c := range tx.Statement.Clauses {
		if strings.Contains(fmt.Sprint(c.Expression), "teen_user_id IN") {
			return true
		}
	}
	return false
}

// TestGroupReadRefusalsJudgeVisibilityFirst: a malformed page or date window
// on a group the viewer may not see answers the group's 404, never its 400; a
// visible group's malformed request keeps its 400; a signed-in viewer who may
// see the group and not its roster gets members_only.
func TestGroupReadRefusalsJudgeVisibilityFirst(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	outsider := newTestUser(t, f.app, "snapshot outsider", "password1234", models.RoleMember)
	out := loginAPI(t, f.app, outsider.Name, "password1234")
	hidden := out.do("GET", groupReadPath(f.group, ""), nil, false)
	if hidden.Code != http.StatusNotFound {
		t.Fatalf("hidden group: %d %s", hidden.Code, hidden.Body.String())
	}
	for _, q := range []string{"?limit=101", "?cursor=%25%25", "?cursor=" + strings.Repeat("A", 600), "/events?from=nope", "/events?from=2026-05-02&to=2026-05-01", "/events?limit=0", "/members"} {
		for _, c := range []*apiClient{out, newAPIClient(t, f.app)} {
			w := c.do("GET", groupReadPath(f.group, q), nil, false)
			want, body := http.StatusNotFound, hidden.Body.String()
			if q == "/members" && c != out {
				want, body = http.StatusUnauthorized, w.Body.String()
			}
			if w.Code != want || w.Body.String() != body {
				t.Fatalf("%s on a hidden group: %d %s", q, w.Code, w.Body.String())
			}
		}
	}
	for q, code := range map[string]string{groupDetailSuffix + "&limit=101": "group_upcoming_page", "/events?limit=0": "group_upcoming_page", "/events?from=nope": "invalid"} {
		w := f.member.do("GET", groupReadPath(f.group, q), nil, false)
		if w.Code != http.StatusBadRequest || answerCode(t, w) != code {
			t.Fatalf("%s on a visible group: %d %s", q, w.Code, w.Body.String())
		}
	}
	if w := f.member.do("GET", groupReadPath(f.group, "/events?from=nope"), nil, false); !strings.Contains(w.Body.String(), string(errDateWindowFormat)) {
		t.Fatalf("the window's message: %s", w.Body.String())
	}
	private := createFeedGroup(t, f.admin, "Snapshot private", models.VisibilityPrivate, "Private hall")
	w := out.do("GET", groupReadPath(private, "/members"), nil, false)
	if w.Code != http.StatusForbidden || answerCode(t, w) != "members_only" {
		t.Fatalf("a visible group's roster for a non-member: %d %s", w.Code, w.Body.String())
	}
}

// TestGroupDetailRequiresTheDatePagingMarker: GET /groups/:id answers only a
// request with datePaging=2. On a visible group an older client's first page,
// continuation and refresh, signed in or out, get group_upcoming_page with no
// group data, before the read is authorized and acknowledged like any other
// refused page; a marked read answers and continues past its first page. A
// hidden or missing group gives one 404 with or without the marker, and the
// group's events route needs no marker.
func TestGroupDetailRequiresTheDatePagingMarker(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityPublic)
	anon := newAPIClient(t, f.app)
	first := decodeGroupRead[groupDetailJSON](t, apiDo(t, f.member, "GET", groupDetailPath(f.group, "limit=1"), nil, http.StatusOK))
	if len(first.UpcomingEvents) != 1 || first.UpcomingEvents[0].ID != f.dates[0].ID || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("the marked first page: %+v", first)
	}
	next := decodeGroupRead[groupDetailJSON](t, apiDo(t, f.member, "GET", groupDetailPath(f.group, "limit=1", "cursor="+first.NextCursor), nil, http.StatusOK))
	if len(next.UpcomingEvents) != 1 || next.UpcomingEvents[0].ID != f.dates[1].ID || !next.HasMore {
		t.Fatalf("the marked continuation: %+v", next)
	}
	wantIn(t, "the signed-out marked read", apiDo(t, anon, "GET", groupDetailPath(f.group), nil, http.StatusOK), `"name":"Snapshot circle"`, groupReadTitle)

	unmarked := "/api/v1/groups/" + itoa(f.group)
	fired := atReaderSeam(f.app, "authorized", "group detail", func() {})
	for _, c := range []struct {
		name   string
		client *apiClient
	}{{"member", f.member}, {"signed out", anon}} {
		for _, q := range []string{"", "?limit=1", "?cursor=" + first.NextCursor, "?limit=1&cursor=" + first.NextCursor,
			"?datePaging=", "?datePaging=1", "?datePaging=02", "?datePaging=3&cursor=" + first.NextCursor} {
			w := c.client.do("GET", unmarked+q, nil, false)
			if w.Code != http.StatusBadRequest || answerCode(t, w) != "group_upcoming_page" {
				t.Fatalf("%s %q: %d %s", c.name, q, w.Code, w.Body.String())
			}
			wantOut(t, c.name+" "+q, w.Body.String(), "Snapshot circle", groupReadTitle, groupReadPlace, f.memberUser.PublicName())
		}
	}
	pinned := pinnedAs(t, f.member)
	w := pinned.do("GET", unmarked, nil, false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a pinned unmarked read: %d %s", w.Code, w.Body.String())
	}
	wantAck(t, "a pinned unmarked read", w, pinned, f.memberUser)
	if fired.Load() {
		t.Fatal("an unmarked read reached the reader's authorization")
	}
	f.app.hooks.readerAuthorized = nil
	apiDo(t, anon, "GET", groupReadPath(f.group, "/events"), nil, http.StatusOK)

	hidden := createFeedGroup(t, f.admin, "Marker hidden circle", models.VisibilityInviteOnly, "Marker hall")
	outsider := newTestUser(t, f.app, "marker outsider", "password1234", models.RoleMember)
	for _, c := range []struct {
		name   string
		client *apiClient
	}{{"an outsider", loginAPI(t, f.app, outsider.Name, "password1234")}, {"signed out", anon}} {
		want := c.client.do("GET", "/api/v1/groups/"+itoa(hidden), nil, false)
		if want.Code != http.StatusNotFound {
			t.Fatalf("%s, the hidden group: %d %s", c.name, want.Code, want.Body.String())
		}
		for _, id := range []uint{hidden, hidden + 100000} {
			for _, q := range []string{"", "?datePaging=2", "?datePaging=1", "?datePaging=2&limit=1", "?limit=1", "?cursor=" + first.NextCursor} {
				w := c.client.do("GET", "/api/v1/groups/"+itoa(id)+q, nil, false)
				if w.Code != want.Code || w.Body.String() != want.Body.String() {
					t.Fatalf("%s, group %d %q: %d %s, want %s", c.name, id, q, w.Code, w.Body.String(), want.Body.String())
				}
			}
		}
	}
}

// TestRoleChangeRepliesWithTheActorsCurrentRoster: a role change's reply is
// the roster as the actor may see it once the change committed, read in its
// own snapshot: an actor demoted in between gets the member's roster, one
// removed from an invite-only group gets its 404, and the change stays made
// either way.
func TestRoleChangeRepliesWithTheActorsCurrentRoster(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	rolePath := func(u *models.User) string { return groupReadPath(f.group, "/members/"+itoa(u.ID)+"/role") }
	promoted := func(u *models.User) bool { return relIn(f.app.DB, u.ID, f.group).Leader }

	reply := apiDo(t, f.leader, "PUT", rolePath(f.memberUser), map[string]any{"role": "leader"}, http.StatusOK)
	wantIn(t, "the leader's reply", reply, f.teen.PublicName())
	if !promoted(f.memberUser) {
		t.Fatal("the role change did not commit")
	}

	second := groupReadMember(t, f, "second promoted")
	fired := atGroupReadBegin(f.app, func() {
		if err := f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipMember).Error; err != nil {
			t.Error(err)
		}
	})
	reply = apiDo(t, f.leader, "PUT", rolePath(second), map[string]any{"role": "leader"}, http.StatusOK)
	if !fired.Load() || !promoted(second) {
		t.Fatalf("fired %t, promoted %t", fired.Load(), promoted(second))
	}
	wantOut(t, "the demoted actor's reply", reply, f.teen.PublicName(), f.teen.Email)
	wantIn(t, "the demoted actor's reply", reply, second.PublicName())

	third := groupReadMember(t, f, "third promoted")
	fired = atGroupReadBegin(f.app, func() {
		if err := f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.memberUser.ID, f.group).Update("status", "removed").Error; err != nil {
			t.Error(err)
		}
	})
	w := loginAPI(t, f.app, f.memberUser.Name, "password1234").do("PUT", rolePath(third), map[string]any{"role": "leader"}, true)
	if !fired.Load() || !promoted(third) || w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "group not found") {
		t.Fatalf("a removed actor's reply: fired %t promoted %t %d %s", fired.Load(), promoted(third), w.Code, w.Body.String())
	}
}

func wantIn(t *testing.T, what, raw string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(raw, p) {
			t.Fatalf("%s lacks %q: %s", what, p, raw)
		}
	}
}

func wantOut(t *testing.T, what, raw string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(raw, p) {
			t.Fatalf("%s shows %q: %s", what, p, raw)
		}
	}
}

// TestGroupReadRevisionsAreEditBases: on stable data a manager's group-row
// revision is the managed date read's, on the detail and the events routes.
// A group place moved, or the manager demoted, right after the group read is
// judged leaves that answer's revision, and a guarded edit on it is refused:
// date_changed for the place, the manager's refusal once demoted. A fresh read
// after the move gives the revision the managed read gives, and it saves.
func TestGroupReadRevisionsAreEditBases(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	occ := f.dates[0]
	day := models.FormatCalendarDate(occ.OriginalDate)
	rowRevision := func(t *testing.T, suffix string) string {
		t.Helper()
		raw := apiDo(t, f.leader, "GET", groupReadPath(f.group, suffix), nil, http.StatusOK)
		var page struct {
			UpcomingEvents []occurrenceJSON `json:"upcomingEvents"`
			Events         []occurrenceJSON `json:"events"`
		}
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &page)})
		for _, d := range append(page.UpcomingEvents, page.Events...) {
			if d.ID == occ.ID && d.Revision != "" {
				return d.Revision
			}
		}
		t.Fatalf("no revision for the date: %s", raw)
		return ""
	}
	for _, suffix := range []string{groupDetailSuffix, "/events"} {
		if got, want := rowRevision(t, suffix), editBaseOf(t, f.leader, f.event.ID, day).Revision; got != want {
			t.Fatalf("%q: the group row's revision %s, the managed read's %s", suffix, got, want)
		}
	}

	moved := atReaderSeam(f.app, "authorized", "group detail", func() {
		if err := f.app.DB.Model(&models.Group{}).Where("id = ?", f.group).Update("location", "Moved hall").Error; err != nil {
			t.Error(err)
		}
	})
	stale := rowRevision(t, groupDetailSuffix)
	if !moved.Load() {
		t.Fatal("the read never reached its seam")
	}
	wantDateEdit(t, f.leader, f.event.ID, day, dateEditBody(stale, map[string]any{"title": "On the old place"}), http.StatusConflict, "date_changed")
	fresh := rowRevision(t, groupDetailSuffix)
	if fresh == stale || fresh != editBaseOf(t, f.leader, f.event.ID, day).Revision {
		t.Fatalf("after the move: fresh %s, stale %s", fresh, stale)
	}
	wantDateEdit(t, f.leader, f.event.ID, day, dateEditBody(fresh, map[string]any{"title": "On the new place"}), http.StatusOK, "")

	base := rowRevision(t, groupDetailSuffix)
	demoted := atReaderSeam(f.app, "authorized", "group detail", func() {
		if err := f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipMember).Error; err != nil {
			t.Error(err)
		}
	})
	if got := rowRevision(t, groupDetailSuffix); !demoted.Load() || got != base {
		t.Fatalf("the demotion's answer: fired %t, revision %s want %s", demoted.Load(), got, base)
	}
	wantDateEdit(t, f.leader, f.event.ID, day, dateEditBody(base, map[string]any{"title": "After the demotion"}), http.StatusForbidden, "")
	var o models.EventOccurrence
	gormMust(t, f.app.DB.First(&o, occ.ID))
	if o.TitleOverride == nil || *o.TitleOverride != "On the new place" {
		t.Fatalf("the demoted manager's edit wrote: %+v", o.TitleOverride)
	}
}

// TestGroupReadsJudgeEveryAuthorityChangeInTheirSnapshot: whatever changes
// what the viewer may see, committed right after a group read judged it, the
// answer is byte for byte the one before the commit, and the next read is the
// new authority's answer, with the keys it allows and without the ones it
// doesn't. The changes: removal from an invite-only group, an admin demoted
// to a plain member, a group made private and its event invite-only, a commit
// that leaves the role as it was, and a guardian link revoked or confirmed
// (the only way a manager sees a teen's contact).
//
// The roster (/members) is for members and admins alone (policy.CanSeeRoster),
// and a tightening takes neither from anyone, so on that route the tightening
// is read by a member and changes nothing; an outsider is refused there before
// any judgment the commit could race.
func TestGroupReadsJudgeEveryAuthorityChangeInTheirSnapshot(t *testing.T) {
	const retitled = "Retitled with the role unchanged"
	type variant struct {
		name       string
		visibility string
		// setup returns the viewer, the commit made at the seam and the check
		// of the next read.
		setup func(t *testing.T, f groupReadFixture, suffix string) (*apiClient, func(tx *gorm.DB) error, func(t *testing.T, code int, raw string))
	}
	teenGoing := func(t *testing.T, f groupReadFixture) {
		t.Helper()
		gormMust(t, f.app.DB.Create(&models.Registration{UserID: &f.teen.ID, OccurrenceID: f.dates[0].ID, Status: models.RegGoing, AttendanceMode: models.AttendanceInPerson, Source: "web"}))
	}
	// teenOnDate puts the teen on the first date, with the leader as their
	// guardian by a link in status link.
	teenOnDate := func(t *testing.T, f groupReadFixture, link string) *models.GuardianLink {
		t.Helper()
		teenGoing(t, f)
		l := models.GuardianLink{GuardianUserID: f.leaderUser.ID, TeenUserID: &f.teen.ID, Status: link, ConfirmedBy: &f.adminUser.ID}
		gormMust(t, f.app.DB.Create(&l))
		return &l
	}
	// public is what any viewer who may see the route keeps: the group's
	// member on every route, the date's title on the dated ones, the
	// group's name on its page.
	public := func(t *testing.T, f groupReadFixture, suffix, raw string) {
		t.Helper()
		wantIn(t, "the next answer", raw, f.memberUser.PublicName())
		switch suffix {
		case groupDetailSuffix:
			wantIn(t, "the next answer", raw, `"name":"Snapshot circle"`, groupReadTitle)
		case "/events":
			wantIn(t, "the next answer", raw, groupReadTitle)
		}
	}
	manager := func(t *testing.T, f groupReadFixture, suffix, raw string) {
		t.Helper()
		wantIn(t, "the manager's answer", raw, f.teen.PublicName())
		if suffix != "/members" {
			wantIn(t, "the manager's answer", raw, `"revision":"1.`)
		}
		if suffix == groupDetailSuffix {
			wantIn(t, "the manager's answer", raw, `"canManage":true`)
		}
	}
	notManager := func(t *testing.T, f groupReadFixture, suffix, raw string) {
		t.Helper()
		wantOut(t, "the next answer", raw, f.teen.PublicName(), f.teen.Email, `"revision"`, `"canManage":true`)
		if suffix == groupDetailSuffix {
			wantIn(t, "the next answer", raw, `"canManage":false`)
		}
	}
	variants := []variant{
		{"removed from an invite-only group", models.VisibilityInviteOnly, func(t *testing.T, f groupReadFixture, _ string) (*apiClient, func(*gorm.DB) error, func(*testing.T, int, string)) {
			return f.leader, func(tx *gorm.DB) error {
					return tx.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("status", "removed").Error
				}, func(t *testing.T, code int, raw string) {
					if code != http.StatusNotFound || raw != `{"code":"not_found","error":"group not found"}` {
						t.Fatalf("the removed leader's next read: %d %s", code, raw)
					}
				}
		}},
		{"admin demoted to a member", models.VisibilityInviteOnly, func(t *testing.T, f groupReadFixture, suffix string) (*apiClient, func(*gorm.DB) error, func(*testing.T, int, string)) {
			u := newTestUser(t, f.app, "demoted admin", "password1234", models.RoleAdmin)
			gormMust(t, f.app.DB.Create(&models.Membership{UserID: u.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}))
			return loginAPI(t, f.app, u.Name, "password1234"), func(tx *gorm.DB) error {
					return tx.Model(&models.User{}).Where("id = ?", u.ID).Update("role", models.RoleMember).Error
				}, func(t *testing.T, code int, raw string) {
					if code != http.StatusOK {
						t.Fatalf("the demoted admin's next read: %d %s", code, raw)
					}
					public(t, f, suffix, raw)
					notManager(t, f, suffix, raw)
				}
		}},
		{"group made private, its event invite-only", models.VisibilityPublic, func(t *testing.T, f groupReadFixture, suffix string) (*apiClient, func(*gorm.DB) error, func(*testing.T, int, string)) {
			viewer := f.member
			if suffix != "/members" {
				outsider := newTestUser(t, f.app, "tightened outsider", "password1234", models.RoleMember)
				viewer = loginAPI(t, f.app, outsider.Name, "password1234")
			}
			anonymous := newAPIClient(t, f.app)
			path := groupReadPath(f.group, suffix)
			if suffix != "/members" {
				wantIn(t, "the signed-out answer", apiDo(t, anonymous, "GET", path, nil, http.StatusOK), groupReadTitle)
			}
			return viewer, func(tx *gorm.DB) error {
					if err := tx.Model(&models.Group{}).Where("id = ?", f.group).Update("visibility", models.VisibilityPrivate).Error; err != nil {
						return err
					}
					return tx.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("visibility", models.VisibilityInviteOnly).Error
				}, func(t *testing.T, code int, raw string) {
					if code != http.StatusOK {
						t.Fatalf("the next read after the tightening: %d %s", code, raw)
					}
					switch suffix {
					case groupDetailSuffix:
						wantIn(t, "the outsider's next answer", raw, `"name":"Snapshot circle"`, `"upcomingEvents":[]`, `"members":[]`)
						wantOut(t, "the outsider's next answer", raw, groupReadTitle, f.memberUser.PublicName())
					case "/events":
						wantIn(t, "the outsider's next answer", raw, `"events":[]`, `"hasMore":false`)
						wantOut(t, "the outsider's next answer", raw, groupReadTitle, f.memberUser.PublicName())
					default:
						wantIn(t, "the member's next roster", raw, f.memberUser.PublicName())
					}
					if suffix != "/members" {
						apiDo(t, anonymous, "GET", path, nil, http.StatusNotFound)
					}
				}
		}},
		{"role unchanged", models.VisibilityInviteOnly, func(t *testing.T, f groupReadFixture, suffix string) (*apiClient, func(*gorm.DB) error, func(*testing.T, int, string)) {
			teenGoing(t, f)
			joiner := newTestUser(t, f.app, "unchanged joiner", "password1234", models.RoleMember)
			return f.leader, func(tx *gorm.DB) error {
					if err := tx.Create(&models.Membership{UserID: joiner.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}).Error; err != nil {
						return err
					}
					return tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[0].ID).Update("title_override", retitled).Error
				}, func(t *testing.T, code int, raw string) {
					if code != http.StatusOK {
						t.Fatalf("the leader's next read: %d %s", code, raw)
					}
					manager(t, f, suffix, raw)
					if suffix == "/events" {
						wantIn(t, "the leader's next answer", raw, retitled)
					} else {
						wantIn(t, "the leader's next answer", raw, joiner.PublicName())
					}
				}
		}},
		{"guardian link revoked", models.VisibilityInviteOnly, func(t *testing.T, f groupReadFixture, suffix string) (*apiClient, func(*gorm.DB) error, func(*testing.T, int, string)) {
			l := teenOnDate(t, f, models.GuardianConfirmed)
			wantIn(t, "the guardian's answer", apiDo(t, f.leader, "GET", groupReadPath(f.group, suffix), nil, http.StatusOK), f.teen.Email)
			return f.leader, func(tx *gorm.DB) error {
					return tx.Model(l).Update("status", models.GuardianRevoked).Error
				}, func(t *testing.T, code int, raw string) {
					if code != http.StatusOK {
						t.Fatalf("the leader's next read: %d %s", code, raw)
					}
					manager(t, f, suffix, raw)
					wantOut(t, "the next answer once the link is revoked", raw, f.teen.Email)
				}
		}},
		{"guardian link confirmed", models.VisibilityInviteOnly, func(t *testing.T, f groupReadFixture, suffix string) (*apiClient, func(*gorm.DB) error, func(*testing.T, int, string)) {
			l := teenOnDate(t, f, models.GuardianPending)
			wantOut(t, "the pending guardian's answer", apiDo(t, f.leader, "GET", groupReadPath(f.group, suffix), nil, http.StatusOK), f.teen.Email)
			return f.leader, func(tx *gorm.DB) error {
					return tx.Model(l).Update("status", models.GuardianConfirmed).Error
				}, func(t *testing.T, code int, raw string) {
					if code != http.StatusOK {
						t.Fatalf("the leader's next read: %d %s", code, raw)
					}
					manager(t, f, suffix, raw)
					wantIn(t, "the next answer once the link is confirmed", raw, f.teen.Email)
				}
		}},
	}
	for _, v := range variants {
		for _, route := range groupReadRoutes {
			t.Run(v.name+"/"+route.surface, func(t *testing.T) {
				f := newGroupReadFixture(t, v.visibility)
				viewer, change, next := v.setup(t, f, route.suffix)
				path := groupReadPath(f.group, route.suffix)
				before := apiDo(t, viewer, "GET", path, nil, http.StatusOK)
				// The change commits with a retitled date and a new member,
				// so every route has rows the commit changed.
				sentinel := newTestUser(t, f.app, "sentinel member", "password1234", models.RoleMember)
				fired := atReaderSeam(f.app, "authorized", route.surface, func() {
					if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
						if err := change(tx); err != nil {
							return err
						}
						if err := tx.Create(&models.Membership{UserID: sentinel.ID, GroupID: f.group, Role: models.MembershipMember, Status: models.MembershipActive}).Error; err != nil {
							return err
						}
						return tx.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Update("title_override", "Sentinel title").Error
					}); err != nil {
						t.Error(err)
					}
				})
				w := viewer.do("GET", path, nil, false)
				if !fired.Load() {
					t.Fatal("the read never reached its seam")
				}
				if w.Code != http.StatusOK || w.Body.String() != before {
					t.Fatalf("the read straddled the commit:\nbefore %s\ngot    %d %s", before, w.Code, w.Body.String())
				}
				f.app.hooks.readerAuthorized = nil
				w = viewer.do("GET", path, nil, false)
				next(t, w.Code, w.Body.String())
			})
		}
	}
}

// TestGroupReadsCrossTheCareWindowOnOneClock: a group read decides a child's
// care notes, the dates it lists and the next date on the one clock it took.
// The window (policy: a day before the start to a day after the end) opens,
// the first date starts, or the window closes, right after the read is
// judged: the answer is byte for byte the one at the read's clock, and the
// next read follows the new one. The child stays listed throughout, so only
// the note differs. The group page lists only dates that have not started,
// so the window's close is read on /events with a window that reaches back
// to the date; the roster has no care notes.
func TestGroupReadsCrossTheCareWindowOnOneClock(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	first := f.dates[0]
	// The leader is the teen's guardian, not the child's: the notes reach
	// them only inside the window.
	people := seedRosterPeople(t, f.app, f.group, f.event.ID, first.ID, f.leaderUser, f.adminUser)
	clock := newMutableClock(f.app, time.Now())
	defer func() { f.app.clock = nil }()
	child := func(t *testing.T, raw string, dates []occurrenceJSON) (listed bool, note *string) {
		t.Helper()
		for _, d := range dates {
			if d.ID != first.ID {
				continue
			}
			for _, row := range d.Attendees {
				if row.HouseholdPerson != nil && row.HouseholdPerson.PersonID == people.child.ID {
					return true, row.HouseholdPerson.CareNotes
				}
			}
			t.Fatalf("the child left the date: %s", raw)
		}
		return false, nil
	}
	detail := func(t *testing.T) (groupDetailJSON, string) {
		t.Helper()
		raw := apiDo(t, f.leader, "GET", groupDetailPath(f.group), nil, http.StatusOK)
		var got groupDetailJSON
		gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &got)})
		return got, raw
	}
	// across reads path at, moves the clock to later at the seam, and
	// returns the answer read at at and the next read's.
	across := func(t *testing.T, surface, path string, at, later time.Time) (string, string) {
		t.Helper()
		clock.set(at)
		before := apiDo(t, f.leader, "GET", path, nil, http.StatusOK)
		fired := atReaderSeam(f.app, "authorized", surface, func() { clock.set(later) })
		defer func() { f.app.hooks.readerAuthorized = nil }()
		if got := apiDo(t, f.leader, "GET", path, nil, http.StatusOK); !fired.Load() || got != before {
			t.Fatalf("the read took a clock set after its judgment (fired %t):\nbefore %s\ngot    %s", fired.Load(), before, got)
		}
		f.app.hooks.readerAuthorized = nil
		return before, apiDo(t, f.leader, "GET", path, nil, http.StatusOK)
	}

	t.Run("the window opens", func(t *testing.T) {
		opens := first.StartsAt.Add(-24 * time.Hour)
		answer, next := across(t, "group detail", groupDetailPath(f.group), opens.Add(-30*time.Minute), opens.Add(30*time.Minute))
		got := decodeGroupRead[groupDetailJSON](t, answer)
		if listed, note := child(t, answer, got.UpcomingEvents); !listed || note != nil {
			t.Fatalf("before the window the leader sees the note: %s", answer)
		}
		got = decodeGroupRead[groupDetailJSON](t, next)
		if listed, note := child(t, next, got.UpcomingEvents); !listed || note == nil || *note != people.careNote {
			t.Fatalf("inside the window the leader lacks the note: %s", next)
		}
	})
	t.Run("the date starts", func(t *testing.T) {
		answer, next := across(t, "group detail", groupDetailPath(f.group), first.StartsAt.Add(-30*time.Minute), first.StartsAt.Add(30*time.Minute))
		got := decodeGroupRead[groupDetailJSON](t, answer)
		listed, note := child(t, answer, got.UpcomingEvents)
		if !listed || note == nil || got.Group.NextEventAt == nil || !got.Group.NextEventAt.Equal(first.StartsAt) {
			t.Fatalf("before the start: %s", answer)
		}
		got = decodeGroupRead[groupDetailJSON](t, next)
		if listed, _ := child(t, next, got.UpcomingEvents); listed || got.Group.NextEventAt == nil || !got.Group.NextEventAt.Equal(f.dates[1].StartsAt) {
			t.Fatalf("after the start the group page still lists the date: %s", next)
		}
	})
	t.Run("the window closes", func(t *testing.T) {
		day := models.FormatCalendarDate(first.OriginalDate)
		closes := first.EndsAt.Add(24 * time.Hour)
		answer, next := across(t, "group events", groupReadPath(f.group, "/events?from="+day+"&to="+day), closes.Add(-30*time.Minute), closes.Add(30*time.Minute))
		got := decodeGroupRead[groupEventsJSON](t, answer)
		if listed, note := child(t, answer, got.Events); !listed || note == nil || *note != people.careNote {
			t.Fatalf("inside the window the leader lacks the note: %s", answer)
		}
		got = decodeGroupRead[groupEventsJSON](t, next)
		if listed, note := child(t, next, got.Events); !listed || note != nil {
			t.Fatalf("after the window the leader still sees the note: %s", next)
		}
		wantOut(t, "after the window", next, people.careNote)
	})
	if _, raw := detail(t); strings.Contains(raw, people.careNote) {
		t.Fatalf("the group page shows a note after its date: %s", raw)
	}
}

// decodeGroupRead decodes a group read's answer into a fresh T: decoding into
// one already used keeps the pointers a later answer leaves out.
func decodeGroupRead[T any](t *testing.T, raw string) T {
	t.Helper()
	var out T
	gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &out)})
	return out
}

// TestGroupReadCommitFailureAnswersNothing: a group read whose COMMIT fails,
// after its whole answer was built, answers the generic 500 and nothing it
// read, on every route and on a role change's reply (whose change stays
// made). The read's backend is ended at groupReadEnd, so the failure is the
// commit's own.
func TestGroupReadCommitFailureAnswersNothing(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	endAtCommit := func(t *testing.T) *atomic.Bool {
		t.Helper()
		var fired atomic.Bool
		f.app.hooks.groupReadEnd = func(tx *gorm.DB) {
			if !fired.CompareAndSwap(false, true) {
				return
			}
			pid := backendPID(tx)
			if err := f.app.DB.Exec("SELECT pg_terminate_backend(?)", pid).Error; err != nil {
				t.Errorf("terminate: %v", err)
				return
			}
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				var n int64
				if err := f.app.DB.Raw("SELECT count(*) FROM pg_stat_activity WHERE pid = ?", pid).Scan(&n).Error; err == nil && n == 0 {
					return
				}
			}
			t.Errorf("backend %d did not exit", pid)
		}
		t.Cleanup(func() { f.app.hooks.groupReadEnd = nil })
		return &fired
	}
	wantNothing := func(t *testing.T, what string, fired *atomic.Bool, w *httptest.ResponseRecorder) {
		t.Helper()
		if !fired.Load() {
			t.Fatalf("%s never reached its commit", what)
		}
		if w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
			t.Fatalf("%s with a failed commit: %d %s", what, w.Code, w.Body.String())
		}
		wantOut(t, what, w.Body.String(), f.teen.PublicName(), f.memberUser.PublicName(), groupReadTitle)
	}
	for _, route := range groupReadRoutes {
		t.Run(route.surface, func(t *testing.T) {
			path := groupReadPath(f.group, route.suffix)
			wantIn(t, "the answer", apiDo(t, f.leader, "GET", path, nil, http.StatusOK), f.memberUser.PublicName())
			fired := endAtCommit(t)
			wantNothing(t, path, fired, f.leader.do("GET", path, nil, false))
			f.app.hooks.groupReadEnd = nil
			wantIn(t, "the next answer", apiDo(t, f.leader, "GET", path, nil, http.StatusOK), f.memberUser.PublicName())
		})
	}
	t.Run("role change reply", func(t *testing.T) {
		promoted := groupReadMember(t, f, "commit promoted")
		fired := endAtCommit(t)
		wantNothing(t, "the role change's reply", fired, f.leader.do("PUT", groupReadPath(f.group, "/members/"+itoa(promoted.ID)+"/role"), map[string]any{"role": "leader"}, true))
		if !relIn(f.app.DB, promoted.ID, f.group).Leader {
			t.Fatal("the role change did not stay made")
		}
	})
}

// TestGroupDatesReachTheTwoHundredFirst: cursor pages visit every date the
// viewer may see exactly once, in order, however many come first. A series
// whose first date is the 201st the group lists starts page 9 at the default
// limit (25) and page 3 at 100, for a member who sees the 200 before
// it; for a signed-out visitor, who sees none of them, it is first on page 1.
// On /events a daily series whose first 200 dates are hidden from the visitor
// starts their first page at its 201st date. The group page's upcoming window
// is 90 days and a rule repeats at most once a day, so the 200 dates before
// the group page's series are one-off events.
func TestGroupDatesReachTheTwoHundredFirst(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityPublic)
	var g models.Group
	gormMust(t, f.app.DB.First(&g, f.group))
	// walk reads every page of path at limit (0 sends none, for the default)
	// and returns the date ids in order, and the index of each page's first
	// date.
	walk := func(t *testing.T, c *apiClient, path string, limit int) ([]uint, []int) {
		t.Helper()
		var ids []uint
		var starts []int
		cursor := ""
		var params []string
		if limit == 0 {
			limit = maxGroupUpcomingOccurrences
		} else {
			params = append(params, "limit="+itoa(uint(limit)))
		}
		for page := 0; ; page++ {
			if page > 40 {
				t.Fatalf("%s never ended", path)
			}
			q, sep := path, "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			for _, p := range params {
				q, sep = q+sep+p, "&"
			}
			if cursor != "" {
				q += sep + "cursor=" + cursor
			}
			raw := apiDo(t, c, "GET", q, nil, http.StatusOK)
			var p struct {
				UpcomingEvents []occurrenceJSON `json:"upcomingEvents"`
				Events         []occurrenceJSON `json:"events"`
				HasMore        bool             `json:"hasMore"`
				Truncated      bool             `json:"truncated"`
				NextCursor     string           `json:"nextCursor"`
			}
			gormMust(t, &gorm.DB{Error: json.Unmarshal([]byte(raw), &p)})
			dates := append(p.UpcomingEvents, p.Events...)
			if p.HasMore != p.Truncated || p.HasMore != (p.NextCursor != "") || (p.HasMore && len(dates) != limit) || len(dates) > limit {
				t.Fatalf("%s page %d: %d dates, hasMore %t, truncated %t, cursor %q", q, page, len(dates), p.HasMore, p.Truncated, p.NextCursor)
			}
			starts = append(starts, len(ids))
			for _, d := range dates {
				ids = append(ids, d.ID)
			}
			if !p.HasMore {
				return ids, starts
			}
			cursor = p.NextCursor
		}
	}
	want := func(t *testing.T, what string, got, want []uint) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: got %d dates %v\nwant %d %v", what, len(got), got, len(want), want)
		}
	}

	t.Run("group page", func(t *testing.T) {
		start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
		var ahead []uint
		for i := 0; i < 200; i++ {
			_, o := seedEvent(t, f.app, &g, f.leaderUser.ID, start.Add(time.Duration(i)*time.Minute))
			ahead = append(ahead, o.ID)
		}
		series := []uint{f.dates[0].ID, f.dates[1].ID, f.dates[2].ID}
		path := groupDetailPath(f.group)
		for limit, page := range map[int]int{0: 8, 100: 2} {
			ids, starts := walk(t, f.member, path, limit)
			want(t, fmt.Sprintf("the member's pages at %d", limit), ids, append(append([]uint{}, ahead...), series...))
			if len(starts) != page+1 || starts[page] != 200 {
				t.Fatalf("at %d the series starts no page of its own: pages start at %v", limit, starts)
			}
		}
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id IN ?", ahead).Update("visibility_override", models.VisibilityInviteOnly))
		anonymous := newAPIClient(t, f.app)
		for _, limit := range []int{1, 0, 100} {
			ids, _ := walk(t, anonymous, path, limit)
			want(t, fmt.Sprintf("the visitor's pages at %d", limit), ids, series)
		}
		ids, _ := walk(t, f.member, path, 0)
		want(t, "the member's pages with the 200 hidden from visitors", ids, append(append([]uint{}, ahead...), series...))
	})
	t.Run("events", func(t *testing.T) {
		from := feedStart(1)
		ev := createFeedEvent(t, f.admin, f.group, "Two hundred first", from, "FREQ=DAILY;COUNT=230")
		var dates []models.EventOccurrence
		gormMust(t, f.app.DB.Where("event_id = ?", ev.ID).Order("starts_at, id").Find(&dates))
		if len(dates) != 230 {
			t.Fatalf("fixture: %d dates", len(dates))
		}
		var hidden, shown []uint
		for i, d := range dates {
			if i < 200 {
				hidden = append(hidden, d.ID)
			} else {
				shown = append(shown, d.ID)
			}
		}
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id IN ?", hidden).Update("visibility_override", models.VisibilityInviteOnly))
		path := groupReadPath(f.group, "/events?from="+models.FormatCalendarDate(dates[0].OriginalDate)+"&to="+models.FormatCalendarDate(dates[229].OriginalDate))
		anonymous := newAPIClient(t, f.app)
		ofSeries := map[uint]bool{}
		for _, d := range dates {
			ofSeries[d.ID] = true
		}
		for _, limit := range []int{1, 0, 100} {
			ids, _ := walk(t, anonymous, path, limit)
			var own []uint
			for _, id := range ids {
				if ofSeries[id] {
					own = append(own, id)
				}
			}
			want(t, fmt.Sprintf("the visitor's series at %d", limit), own, shown)
		}
		var inWindow []uint
		gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).
			Where("event_id IN (SELECT id FROM events WHERE group_id = ?) AND status = ? AND original_date BETWEEN ? AND ?", f.group, models.OccurrenceScheduled, dates[0].OriginalDate, dates[229].OriginalDate).
			Order("starts_at, id").Pluck("id", &inWindow))
		ids, _ := walk(t, f.member, path, 100)
		want(t, "the member's pages", ids, inWindow)
	})
}

// TestGroupReadsAcknowledgeTheViewerTheirSnapshotJudged: a participating
// group read (M8.1) is acknowledged for the viewer its snapshot judged, once
// the snapshot has ended: on each route, a later page and the role change's
// roster reply, with the same body a plain read gets; on the refusals the
// snapshot decides (a missing group's 404 exactly like a hidden one's, a
// malformed page); for a session that ended after the middleware accepted it
// as the signed-out visitor it reads as, never the former owner; and not at
// all for a roster that answers 401, a failed read or a failed COMMIT. A
// plain read carries no acknowledgment. A demotion committed after the
// judgment leaves the answer and its acknowledgment the snapshot's.
func TestGroupReadsAcknowledgeTheViewerTheirSnapshotJudged(t *testing.T) {
	f := newGroupReadFixture(t, models.VisibilityInviteOnly)
	leader := pinnedAs(t, f.leader)
	outsiderUser := newTestUser(t, f.app, "ack outsider", "password1234", models.RoleMember)
	outsider := pinnedAs(t, loginAPI(t, f.app, outsiderUser.Name, "password1234"))
	anonPlain := newAPIClient(t, f.app)
	anonPlain.fetchCSRF()
	anon := pinnedAs(t, anonPlain)

	t.Run("answers", func(t *testing.T) {
		for _, route := range groupReadRoutes {
			path := groupReadPath(f.group, route.suffix)
			plain := f.leader.do("GET", path, nil, false)
			wantNoAck(t, "a plain "+route.surface, plain)
			w := leader.do("GET", path, nil, false)
			if w.Code != http.StatusOK || w.Body.String() != plain.Body.String() {
				t.Fatalf("%s: %d, the same body as a plain read %t", route.surface, w.Code, w.Body.String() == plain.Body.String())
			}
			wantAck(t, route.surface, w, leader, f.leaderUser)
		}
		page := decodeGroupRead[groupDetailJSON](t, apiDo(t, leader, "GET", groupDetailPath(f.group, "limit=1"), nil, http.StatusOK))
		if !page.HasMore || page.NextCursor == "" {
			t.Fatalf("fixture: one page holds every date")
		}
		for _, next := range []string{groupDetailSuffix + "&limit=1&cursor=" + page.NextCursor, "/events?limit=1&cursor=" + page.NextCursor} {
			w := f.member.do("GET", groupReadPath(f.group, next), nil, false)
			wantNoAck(t, "a plain later page", w)
			w = pinnedAs(t, f.member).do("GET", groupReadPath(f.group, next), nil, false)
			if w.Code != http.StatusOK {
				t.Fatalf("later page %s: %d %s", next, w.Code, w.Body.String())
			}
			wantAck(t, "a later page", w, f.member, f.memberUser)
		}
		promoted := groupReadMember(t, f, "ack promoted")
		w := leader.do("PUT", groupReadPath(f.group, "/members/"+itoa(promoted.ID)+"/role"), map[string]any{"role": "leader"}, true)
		if w.Code != http.StatusOK {
			t.Fatalf("role change: %d %s", w.Code, w.Body.String())
		}
		wantAck(t, "the role change's roster reply", w, leader, f.leaderUser)
	})

	t.Run("refusals", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			client *apiClient
			viewer *models.User
		}{{"signed out", anon, nil}, {"an outsider", outsider, outsiderUser}} {
			for _, route := range groupReadRoutes {
				if route.suffix == "/members" && c.viewer == nil {
					continue
				}
				hidden := c.client.do("GET", groupReadPath(f.group, route.suffix), nil, false)
				missing := c.client.do("GET", groupReadPath(f.group+100000, route.suffix), nil, false)
				if hidden.Code != http.StatusNotFound || missing.Code != hidden.Code || missing.Body.String() != hidden.Body.String() {
					t.Fatalf("%s, %s: hidden %d %s, missing %d %s", c.name, route.surface, hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
				}
				wantAck(t, c.name+" hidden "+route.surface, hidden, c.client, c.viewer)
				wantAck(t, c.name+" missing "+route.surface, missing, c.client, c.viewer)
			}
		}
		w := leader.do("GET", groupDetailPath(f.group, "cursor=not-a-cursor"), nil, false)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("a malformed page: %d %s", w.Code, w.Body.String())
		}
		wantAck(t, "a malformed page", w, leader, f.leaderUser)
		w = anon.do("GET", groupReadPath(f.group, "/members"), nil, false)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("a signed-out roster: %d %s", w.Code, w.Body.String())
		}
		wantNoAck(t, "a signed-out roster", w)
	})

	t.Run("a session ended before the snapshot", func(t *testing.T) {
		for _, route := range groupReadRoutes {
			ended := groupReadMember(t, f, "ack ended "+route.surface)
			c := pinnedAs(t, loginAPI(t, f.app, ended.Name, "password1234"))
			fired := atGroupReadBegin(f.app, func() {
				gormMust(t, f.app.DB.Model(&models.User{}).Where("id = ?", ended.ID).Update("session_version", gorm.Expr("session_version + 1")))
			})
			w := c.do("GET", groupReadPath(f.group, route.suffix), nil, false)
			f.app.hooks.groupReadBegin = nil
			if !fired.Load() {
				t.Fatal("the read never began")
			}
			if route.suffix == "/members" {
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("the ended roster: %d %s", w.Code, w.Body.String())
				}
				wantNoAck(t, "the ended roster", w)
				continue
			}
			signedOut := anonPlain.do("GET", groupReadPath(f.group, route.suffix), nil, false)
			if w.Code != http.StatusNotFound || w.Body.String() != signedOut.Body.String() {
				t.Fatalf("%s for an ended session: %d %s, signed out %s", route.surface, w.Code, w.Body.String(), signedOut.Body.String())
			}
			wantAck(t, route.surface+" for an ended session", w, c, nil)
		}
	})

	t.Run("a demotion after the judgment", func(t *testing.T) {
		path := groupDetailPath(f.group)
		before := apiDo(t, leader, "GET", path, nil, http.StatusOK)
		wantIn(t, "the manager's page", before, `"canManage":true`, f.teen.PublicName())
		fired := atReaderSeam(f.app, "authorized", "group detail", func() {
			gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipMember))
		})
		w := leader.do("GET", path, nil, false)
		f.app.hooks.readerAuthorized = nil
		if !fired.Load() || w.Code != http.StatusOK || w.Body.String() != before {
			t.Fatalf("the read straddled the demotion (fired %t): %d\nbefore %s\ngot    %s", fired.Load(), w.Code, before, w.Body.String())
		}
		wantAck(t, "the snapshot's page", w, leader, f.leaderUser)
		after := leader.do("GET", path, nil, false)
		wantAck(t, "the next page", after, leader, f.leaderUser)
		wantOut(t, "the demoted leader's next page", after.Body.String(), `"canManage":true`, f.teen.PublicName())
		gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ? AND group_id = ?", f.leaderUser.ID, f.group).Update("role", models.MembershipLeader))
	})

	t.Run("failures", func(t *testing.T) {
		n := failStatement(t, f.app, func(tx *gorm.DB) bool {
			return tx.Statement.Table == "memberships" && strings.Contains(fmt.Sprint(tx.Statement.Joins), "users")
		})
		w := leader.do("GET", groupReadPath(f.group, "/members"), nil, false)
		if n.Load() == 0 || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
			t.Fatalf("a failed roster read (failed %d): %d %s", n.Load(), w.Code, w.Body.String())
		}
		wantNoAck(t, "a failed roster read", w)
	})

	t.Run("a failed commit", func(t *testing.T) {
		for _, route := range groupReadRoutes {
			var fired atomic.Bool
			f.app.hooks.groupReadEnd = func(tx *gorm.DB) {
				if fired.CompareAndSwap(false, true) {
					endBackend(t, f.app, tx)
				}
			}
			w := leader.do("GET", groupReadPath(f.group, route.suffix), nil, false)
			f.app.hooks.groupReadEnd = nil
			if !fired.Load() || w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
				t.Fatalf("%s with a failed commit (fired %t): %d %s", route.surface, fired.Load(), w.Code, w.Body.String())
			}
			wantNoAck(t, route.surface+" with a failed commit", w)
		}
	})
}

// endBackend ends tx's backend from another connection and waits for it to
// exit, so the transaction's next statement, COMMIT included, fails.
func endBackend(t *testing.T, app *App, tx *gorm.DB) {
	t.Helper()
	pid := backendPID(tx)
	if err := app.DB.Exec("SELECT pg_terminate_backend(?)", pid).Error; err != nil {
		t.Errorf("terminate: %v", err)
		return
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var n int64
		if err := app.DB.Raw("SELECT count(*) FROM pg_stat_activity WHERE pid = ?", pid).Scan(&n).Error; err == nil && n == 0 {
			return
		}
	}
	t.Errorf("backend %d did not exit", pid)
}
