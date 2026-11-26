package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"fold/internal/audit"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestRoomAdminWritesRequireCurrentAdmin(t *testing.T) {
	f := newBookingFixture(t)
	approvalOn(t, f)
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("request a room: %d %s", code, body)
	}
	reqID := itoa(listRequests(t, f.admin)[0].ID)
	roomID := itoa(f.hall.ID)
	f.roomFeed(t, f.hall.ID)
	var admin models.User
	if err := f.app.DB.Where("role = ?", models.RoleAdmin).First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	oldSession := admin
	oldSession.SessionVersion++
	oldRole := *f.leaderUser
	oldRole.Role = models.RoleAdmin

	for _, route := range []struct {
		name, method, id, body string
		handle                 gin.HandlerFunc
	}{
		{"create room", "POST", "", `{"name":"Additional room"}`, f.app.apiAdminCreateRoom},
		{"edit room", "PUT", roomID, `{"name":"Updated hall"}`, f.app.apiAdminUpdateRoom},
		{"set rooms", "PUT", "", `{"enabled":false}`, f.app.apiSetRoomsEnabled},
		{"set room approval", "PUT", "", `{"enabled":false}`, f.app.apiSetRoomApproval},
		{"confirm room request", "POST", reqID, `{}`, f.app.apiConfirmRoomRequest},
		{"reject room request", "POST", reqID, `{}`, f.app.apiRejectRoomRequest},
		{"match location", "POST", "", `{"location":"Hall","roomId":` + roomID + `}`, f.app.apiAdminMatchLocation},
		{"create room feed", "POST", roomID, "", f.app.apiAdminCreateRoomFeed},
		{"revoke room feed", "DELETE", roomID, "", f.app.apiAdminRevokeRoomFeed},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, session := range []*models.User{&oldSession, &oldRole} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(route.method, "/", strings.NewReader(route.body))
				c.Set("apiUser", session)
				c.AddParam("id", route.id)
				route.handle(c)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("decision status = %d, want 401", w.Code)
				}
			}
		})
	}
	if !f.app.roomsEnabled() || !boolSettingIn(f.app.DB, settingRoomApproval, false) ||
		len(listRequests(t, f.admin)) != 1 {
		t.Fatal("a refused write changed a room switch or the request")
	}
	if n := contactCount(t, f.app, &models.RoomFeedToken{}, "room_id = ?", f.hall.ID); n != 1 {
		t.Fatalf("room feed tokens = %d, want one", n)
	}
	if tok, err := latestRoomFeed(f.app.DB, f.hall.ID); err != nil || tok.ID == 0 || tok.RevokedAt != nil {
		t.Fatalf("original room feed is no longer active: %v", err)
	}
}

func TestAdminRoomCatalogGateReadFailure(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	admin := pinnedAs(t, loginAPI(t, app, "admin", "password1234"))
	if err := setSettingIn(app.DB, settingRoomsEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	failures := failStatement(t, app, func(tx *gorm.DB) bool {
		return tx.Statement.Table == "settings" && strings.Contains(fmt.Sprint(tx.Statement.Clauses["WHERE"].Expression), settingRoomsEnabled)
	})
	paths := []string{"/api/v1/admin/rooms"}
	for _, path := range paths {
		w := admin.do("GET", path, nil, false)
		wantCode(t, path, w, http.StatusInternalServerError, "server_error")
		wantNoAck(t, path, w)
	}
	if n := failures.Load(); int(n) != len(paths) {
		t.Fatalf("switch query failed %d times for %d routes", n, len(paths))
	}
}

func TestSiteReportsRoomSwitchReadFailure(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	if err := setSettingIn(app.DB, settingRoomsEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	failures := failStatement(t, app, func(tx *gorm.DB) bool {
		return tx.Statement.Table == "settings" && strings.Contains(fmt.Sprint(tx.Statement.Clauses["WHERE"].Expression), settingRoomsEnabled)
	})

	guest := newAPIClient(t, app)
	w := guest.do("GET", "/api/v1/site", nil, false)
	wantCode(t, "site could not read the rooms switch", w, http.StatusInternalServerError, "server_error")
	if failures.Load() != 1 || strings.Contains(w.Body.String(), "roomsEnabled") {
		t.Fatalf("site reported a switch value after a failed read: %s", w.Body.String())
	}
}

// Rooms are invisible until an admin turns them on (9.1, M2.1a); then only
// admins manage them, names are unique per building, and nothing is deleted.
func TestRoomCatalog(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
	newTestUser(t, app, "member", "password1234", models.RoleMember)
	admin := loginAPI(t, app, "admin", "password1234")
	member := loginAPI(t, app, "member", "password1234")

	// Off: every room route is 404, for anyone, and /site says so.
	for _, c := range []*apiClient{admin, member, newAPIClient(t, app)} {
		if w := c.do("GET", "/api/v1/admin/rooms", nil, false); w.Code != http.StatusNotFound {
			t.Fatalf("rooms off: GET %d", w.Code)
		}
	}
	if w := admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Hall"}, true); w.Code != http.StatusNotFound {
		t.Fatalf("rooms off: POST %d", w.Code)
	}
	if !strings.Contains(newAPIClient(t, app).do("GET", "/api/v1/site", nil, false).Body.String(), `"roomsEnabled":false`) {
		t.Fatal("site should say rooms are off")
	}

	// Only an admin turns rooms on.
	if w := member.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": true}, true); w.Code != http.StatusForbidden {
		t.Fatalf("member turning rooms on: %d", w.Code)
	}
	if w := admin.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("missing enabled: %d", w.Code)
	}
	if w := admin.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": true}, true); w.Code != http.StatusOK {
		t.Fatalf("turn on: %d %s", w.Code, w.Body.String())
	}
	if contactCount(t, app, &models.AuditLog{}, "action = ?", audit.RoomsEnabledChanged) != 1 {
		t.Fatal("turning rooms on should be audited")
	}
	if !strings.Contains(newAPIClient(t, app).do("GET", "/api/v1/site", nil, false).Body.String(), `"roomsEnabled":true`) {
		t.Fatal("site should say rooms are on")
	}

	// On: members are refused, admins manage.
	if w := member.do("GET", "/api/v1/admin/rooms", nil, false); w.Code != http.StatusForbidden {
		t.Fatalf("member list: %d", w.Code)
	}
	var created struct {
		Room roomJSON `json:"room"`
	}
	w := admin.do("POST", "/api/v1/admin/rooms", map[string]any{
		"name": "  Fellowship Hall ", "building": "Main", "capacity": 80, "features": "kitchen, projector",
		"notes": "Key at the office", "defaultSetupMinutes": 30, "defaultTeardownMinutes": 15,
	}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &created)
	hall := created.Room
	if hall.Name != "Fellowship Hall" || hall.Capacity == nil || *hall.Capacity != 80 || !hall.Active || hall.DefaultSetupMinutes != 30 || hall.DefaultTeardownMinutes != 15 {
		t.Fatalf("created room: %+v", hall)
	}
	for _, tc := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"name": "fellowship hall", "building": "main"}, http.StatusConflict}, // same name, any case
		{map[string]any{"name": "Fellowship Hall", "building": "Annex"}, http.StatusCreated}, // another building is fine
		{map[string]any{"name": "   "}, http.StatusBadRequest},                               // a name is required
		{map[string]any{"name": "Nursery", "capacity": 0}, http.StatusBadRequest},
		{map[string]any{"name": "Nursery", "defaultSetupMinutes": 241}, http.StatusBadRequest},
	} {
		w := admin.do("POST", "/api/v1/admin/rooms", tc.body, true)
		if w.Code != tc.want {
			t.Fatalf("POST %v: %d %s, want %d", tc.body, w.Code, w.Body.String(), tc.want)
		}
		if tc.want == http.StatusConflict && !strings.Contains(w.Body.String(), `"code":"room_name_taken"`) {
			t.Fatalf("duplicate name code: %s", w.Body.String())
		}
	}

	// A new room is always active, whatever the body says.
	var fresh struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Chapel", "active": false}, true), &fresh)
	if !fresh.Room.Active {
		t.Fatalf("a new room should be active: %+v", fresh.Room)
	}
	// Renaming into a taken name is the same conflict.
	wantCode(t, "rename into a taken name", admin.do("PUT", "/api/v1/admin/rooms/"+itoa(fresh.Room.ID),
		withRevision(map[string]any{"name": "Fellowship Hall", "building": "Main"}, fresh.Room.Revision), true), http.StatusConflict, "room_name_taken")

	// Edit keeps absent fields, clears capacity with null, and audits what changed.
	id := itoa(hall.ID)
	var updated struct {
		Room roomJSON `json:"room"`
	}
	w = admin.do("PUT", "/api/v1/admin/rooms/"+id, withRevision(map[string]any{"capacity": nil, "restricted": true}, adminRoomRevision(t, admin, hall.ID)), true)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &updated)
	if updated.Room.Capacity != nil || !updated.Room.Restricted || updated.Room.Notes != "Key at the office" || updated.Room.DefaultSetupMinutes != 30 {
		t.Fatalf("edited room: %+v", updated.Room)
	}
	if contactCount(t, app, &models.AuditLog{}, "action = ? AND target_id = ? AND meta_json->'changed' ? 'restricted'", audit.RoomUpdated, hall.ID) != 1 {
		t.Fatal("the edit should be audited with its changed fields")
	}

	// Deactivating keeps the room; there is no delete route.
	if w := admin.do("PUT", "/api/v1/admin/rooms/"+id, withRevision(map[string]any{"active": false}, updated.Room.Revision), true); w.Code != http.StatusOK {
		t.Fatalf("deactivate: %d", w.Code)
	}
	if w := admin.do("DELETE", "/api/v1/admin/rooms/"+id, nil, true); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete should not exist: %d", w.Code)
	}
	var list struct {
		Rooms []roomJSON `json:"rooms"`
	}
	decodeJSON(t, admin.do("GET", "/api/v1/admin/rooms", nil, false), &list)
	if len(list.Rooms) != 3 || list.Rooms[len(list.Rooms)-1].ID != hall.ID || list.Rooms[len(list.Rooms)-1].Active {
		t.Fatalf("list should keep the inactive room, last: %+v", list.Rooms)
	}
	for _, missing := range []string{"999999", "18446744073709551615", "abc"} {
		if w := admin.do("PUT", "/api/v1/admin/rooms/"+missing, map[string]any{"active": true}, true); w.Code != http.StatusNotFound {
			t.Fatalf("unknown room %s: %d", missing, w.Code)
		}
	}

	// Off again: hidden again, nothing deleted.
	admin.do("PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": false}, true)
	if w := admin.do("GET", "/api/v1/admin/rooms", nil, false); w.Code != http.StatusNotFound {
		t.Fatalf("rooms off again: %d", w.Code)
	}
	if contactCount(t, app, &models.Room{}, "1=1") != 3 {
		t.Fatal("turning rooms off must not delete rooms")
	}
}

// adminRoomRevision is the revision the admin room list gives c for room id.
func adminRoomRevision(t *testing.T, c *apiClient, id uint) string {
	t.Helper()
	for _, r := range adminRoomList(t, c) {
		if r.ID == id && r.Revision != "" {
			return r.Revision
		}
	}
	t.Fatalf("the room list has no revision for room %d", id)
	return ""
}

func adminRoomList(t *testing.T, c *apiClient) []roomJSON {
	t.Helper()
	var list struct {
		Rooms []roomJSON `json:"rooms"`
	}
	decodeJSON(t, c.do("GET", "/api/v1/admin/rooms", nil, false), &list)
	return list.Rooms
}

// roomEditorState is every row a room edit or its side effects could touch:
// the rooms beside the bookings, requests, events, audit rows, notices and
// jobs (roomState).
func roomEditorState(t *testing.T, app *App) string {
	t.Helper()
	var rooms []models.Room
	gormMust(t, app.DB.Order("id").Find(&rooms))
	b, err := json.Marshal(rooms)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + roomState(t, app)
}

// newRoomEditorFixture is the booking fixture with a second admin, the hall
// booked for event A and a chapel waiting for approval on event B, so a refused edit
// has bookings, a request, audit rows and notices it could disturb.
func newRoomEditorFixture(t *testing.T) (bookingFixture, *apiClient) {
	t.Helper()
	f := newBookingFixture(t)
	newTestUser(t, f.app, "admin2", "password1234", models.RoleAdmin)
	other := loginAPI(t, f.app, "admin2", "password1234")
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book A: %d %s", code, body)
	}
	approvalOn(t, f)
	if code, body := f.book(t, f.leader, f.eventB, map[string]any{"roomId": newRoom(t, f, "Chapel").ID}); code != http.StatusOK {
		t.Fatalf("request B: %d %s", code, body)
	}
	if len(listRequests(t, f.admin)) == 0 {
		t.Fatal("the fixture has no waiting request")
	}
	return f, other
}

// DEC-171: an admin's edit of a room names the read it was made from. Two
// admins edit the hall from the same read: the first saves and answers the
// revision the next list shows; the second, and a save naming no read, are
// 409 room_changed with nothing written: no room change, audit row, booking,
// request, notice or job, whatever the body would change (deactivating,
// restricting or the padding defaults included). Named from the current read,
// the same body saves, a partial body included.
func TestRoomEditNamesItsRead(t *testing.T) {
	f, other := newRoomEditorFixture(t)
	path := "/api/v1/admin/rooms/" + itoa(f.hall.ID)
	read := adminRoomRevision(t, f.admin, f.hall.ID)
	if got := adminRoomRevision(t, other, f.hall.ID); got != read {
		t.Fatalf("two admins read %q and %q", read, got)
	}

	var first struct {
		Room roomJSON `json:"room"`
	}
	w := other.do("PUT", path, withRevision(map[string]any{"name": "Great Hall", "notes": "Keys at the office"}, read), true)
	if w.Code != http.StatusOK {
		t.Fatalf("the first save: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &first)
	if first.Room.Name != "Great Hall" || first.Room.Revision == "" || first.Room.Revision == read {
		t.Fatalf("the first save's answer: %+v", first.Room)
	}
	if got := adminRoomRevision(t, f.admin, f.hall.ID); got != first.Room.Revision {
		t.Fatalf("the answer's revision %q, the next list's %q", first.Room.Revision, got)
	}

	before := roomEditorState(t, f.app)
	for label, body := range map[string]map[string]any{
		"a stale rename":                    withRevision(map[string]any{"name": "Fellowship Hall"}, read),
		"a stale deactivation":              withRevision(map[string]any{"active": false}, read),
		"a stale restriction":               withRevision(map[string]any{"restricted": true}, read),
		"stale padding defaults":            withRevision(map[string]any{"defaultSetupMinutes": 60, "defaultTeardownMinutes": 15}, read),
		"a stale save that changes nothing": withRevision(map[string]any{"name": "Great Hall"}, read),
		"a save that names no read":         {"name": "Fellowship Hall", "active": false},
		"an empty revision":                 withRevision(map[string]any{"restricted": true}, ""),
		"another room's revision":           withRevision(map[string]any{"restricted": true}, adminRoomRevision(t, f.admin, f.nursery.ID)),
	} {
		wantCode(t, label, f.admin.do("PUT", path, body, true), http.StatusConflict, "room_changed")
		if after := roomEditorState(t, f.app); after != before {
			t.Fatalf("%s wrote:\n%s\nwant\n%s", label, after, before)
		}
	}

	var second struct {
		Room roomJSON `json:"room"`
	}
	w = f.admin.do("PUT", path, withRevision(map[string]any{"restricted": true}, first.Room.Revision), true)
	if w.Code != http.StatusOK {
		t.Fatalf("the partial save from the current read: %d %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &second)
	if !second.Room.Restricted || second.Room.Name != "Great Hall" || second.Room.Notes != "Keys at the office" || second.Room.Revision == first.Room.Revision {
		t.Fatalf("the partial save's answer: %+v", second.Room)
	}
	if got := adminRoomRevision(t, f.admin, f.hall.ID); got != second.Room.Revision {
		t.Fatalf("the answer's revision %q, the next list's %q", second.Room.Revision, got)
	}

	// A save that changes nothing writes nothing and answers the revision it named.
	before = roomEditorState(t, f.app)
	var same struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, f.admin.do("PUT", path, withRevision(map[string]any{"restricted": true}, second.Room.Revision), true), &same)
	if same.Room.Revision != second.Room.Revision {
		t.Fatalf("an unchanged save moved the revision: %q, want %q", same.Room.Revision, second.Room.Revision)
	}
	if after := roomEditorState(t, f.app); after != before {
		t.Fatalf("an unchanged save wrote:\n%s\nwant\n%s", after, before)
	}
}

// A room's token covers every column, moves with a write that only stamps
// updated_at, is the same in a session in another time zone, and is never
// another room's.
func TestRoomRevisionCoversTheWholeRow(t *testing.T) {
	f := newBookingFixture(t)
	token := func() string {
		t.Helper()
		got, err := f.app.roomRevisionIn(f.app.DB, f.hall.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := token(); !regexp.MustCompile(`^1\.[0-9a-f]{16}\.r`+itoa(f.hall.ID)+`\.[0-9a-f]{32}$`).MatchString(got) || got != f.hall.Revision {
		t.Fatalf("the hall's token %q, created with %q", got, f.hall.Revision)
	}
	for _, change := range []string{
		"updated_at = updated_at + interval '1 second'",
		"name = 'Hall Two'", "building = 'Annex'", "capacity = 21", "capacity = NULL", "features = 'piano'",
		"notes = 'Keys'", "active = false", "restricted = true", "default_setup_minutes = 31",
		"default_teardown_minutes = 5",
	} {
		before := token()
		gormMust(t, f.app.DB.Exec("UPDATE rooms SET "+change+" WHERE id = ?", f.hall.ID))
		if now := token(); now == before {
			t.Errorf("%s left the token at %q", change, now)
		}
	}
	before := token()
	gormMust(t, f.app.DB.Exec("UPDATE rooms SET created_at = created_at - interval '1 day' WHERE id = ?", f.hall.ID))
	if now := token(); now != before {
		t.Errorf("created_at, which never changes, moved the token: %q, want %q", now, before)
	}
	here := token()
	if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL TIME ZONE 'Asia/Tokyo'").Error; err != nil {
			return err
		}
		elsewhere, err := f.app.roomRevisionIn(tx, f.hall.ID)
		if err == nil && elsewhere != here {
			err = fmt.Errorf("a session in another time zone read %q, want %q", elsewhere, here)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := f.app.roomRevisionsIn(f.app.DB, []uint{f.hall.ID, f.nursery.ID, 999999})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || batch[f.hall.ID] != here || batch[f.nursery.ID] == here || !strings.Contains(batch[f.nursery.ID], ".r"+itoa(f.nursery.ID)+".") {
		t.Fatalf("the list's tokens %v, the hall's %q", batch, here)
	}
	if err := f.app.checkRoomRevision(f.app.DB, f.nursery.ID, here); !errors.Is(err, errRoomChanged) {
		t.Fatalf("the hall's token taken for the nursery's: %v", err)
	}
}

func TestAdminRoomCatalogReadsOneSnapshot(t *testing.T) {
	const path = "/api/v1/admin/rooms"
	const surface = "admin room catalog"
	t.Run("catalog and acknowledgment", func(t *testing.T) {
		app := testApp(t)
		user := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
		admin := loginAPI(t, app, user.Name, "password1234")
		apiDo(t, admin, "PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": true}, http.StatusOK)
		var created roomResponseJSON
		decodeJSON(t, admin.do("POST", path, map[string]any{"name": "Chapel", "notes": "Original note"}, true), &created)
		before := apiDo(t, admin, "GET", path, nil, http.StatusOK)
		wantIn(t, "original catalog", before, "Original note", `"needApproval":false`, `"revision":"`+created.Room.Revision+`"`)
		writer := app.DB
		fired := atReaderSeam(app, "authorized", surface, func() {
			if err := writer.Transaction(func(tx *gorm.DB) error {
				if err := tx.Model(user).Update("role", models.RoleMember).Error; err != nil {
					return err
				}
				if err := tx.Model(&models.Room{}).Where("id = ?", created.Room.ID).Update("notes", "Updated note").Error; err != nil {
					return err
				}
				return setSettingIn(tx, settingRoomApproval, "true")
			}); err != nil {
				t.Fatal(err)
			}
		})
		pinned := pinnedAs(t, admin)
		pinned.ctx = oneConnection(t, app, oneConnectionPool(t, app))
		w := pinned.do("GET", path, nil, false)
		if !fired.Load() || w.Code != http.StatusOK || w.Body.String() != before {
			t.Fatalf("catalog after concurrent edit: %d %s, want %s (edit committed: %t)", w.Code, w.Body, before, fired.Load())
		}
		wantAck(t, "catalog", w, pinned, user)
	})

	for _, change := range []struct {
		name   string
		apply  func(*gorm.DB, *models.User) error
		status int
		code   string
	}{
		{"rooms turned off", func(db *gorm.DB, _ *models.User) error {
			return setSettingIn(db, settingRoomsEnabled, "false")
		}, http.StatusNotFound, "not_found"},
		{"admin demoted", func(db *gorm.DB, u *models.User) error {
			return db.Model(u).Update("role", models.RoleMember).Error
		}, http.StatusUnauthorized, "signed_out"},
		{"session ended", func(db *gorm.DB, u *models.User) error {
			return db.Model(u).Update("session_version", sessionVersionBump()).Error
		}, http.StatusUnauthorized, "signed_out"},
	} {
		t.Run(change.name, func(t *testing.T) {
			app := testApp(t)
			user := newTestUser(t, app, "admin", "password1234", models.RoleAdmin)
			admin := pinnedAs(t, loginAPI(t, app, user.Name, "password1234"))
			apiDo(t, admin, "PUT", "/api/v1/admin/settings/rooms", map[string]any{"enabled": true}, http.StatusOK)
			app.hooks.readerBegin = func(s string) {
				if s == surface {
					if err := change.apply(app.DB, user); err != nil {
						t.Fatal(err)
					}
				}
			}
			w := admin.do("GET", path, nil, false)
			wantCode(t, change.name, w, change.status, change.code)
			wantNoAck(t, change.name, w)
		})
	}
}

// The list reads each room's fields and revision in one snapshot: a rename
// committed right after the rooms were read leaves the answer with the old
// name and the old revision, so an edit made from it is refused instead of
// overwriting the rename.
func TestRoomListReadsFieldsAndRevisionsInOneSnapshot(t *testing.T) {
	f := newBookingFixture(t)
	var fired atomic.Bool
	name := "test:room_list_seam"
	gormMust(t, &gorm.DB{Error: f.app.DB.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "rooms" && strings.Contains(tx.Statement.SQL.String(), "ORDER BY active DESC") && fired.CompareAndSwap(false, true) {
			if err := f.app.DB.Exec("UPDATE rooms SET name = 'Renamed', updated_at = now() WHERE id = ?", f.hall.ID).Error; err != nil {
				t.Error(err)
			}
		}
	})})
	t.Cleanup(func() { _ = f.app.DB.Callback().Query().Remove(name) })
	var shown roomJSON
	for _, r := range adminRoomList(t, f.admin) {
		if r.ID == f.hall.ID {
			shown = r
		}
	}
	if !fired.Load() {
		t.Fatal("the list never reached its seam")
	}
	if shown.Name != "Fellowship Hall" {
		t.Fatalf("the list showed %q, want the name its snapshot read", shown.Name)
	}
	before := roomEditorState(t, f.app)
	wantCode(t, "an edit made from the list", f.admin.do("PUT", "/api/v1/admin/rooms/"+itoa(f.hall.ID),
		withRevision(map[string]any{"name": "Fellowship Hall"}, shown.Revision), true), http.StatusConflict, "room_changed")
	if after := roomEditorState(t, f.app); after != before {
		t.Fatalf("the refused edit wrote:\n%s\nwant\n%s", after, before)
	}
}

// An admin disabled or demoted while the edit waits for the room's lock is
// refused (the actor is read after the lock), not told the room changed, and
// nothing is written. Each change is what its admin route writes, the session
// version bumped with it; a role taken away with the session kept is refused
// by the room-manager rule itself. The edit runs on a pool whose default is Repeatable
// Read: the route pins Read Committed, so the actor read sees what committed
// during the wait.
func TestRoomEditByAnAdminRevokedWhileItWaitsWritesNothing(t *testing.T) {
	for name, change := range map[string]string{
		"disabled":                         "status = '" + models.UserDisabled + "', session_version = session_version + 1",
		"demoted":                          "role = '" + models.RoleMember + "', session_version = session_version + 1",
		"no longer an admin, session kept": "role = '" + models.RoleMember + "'",
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newRoomEditorFixture(t)
			var admin models.User
			gormMust(t, f.app.DB.Where("email = ?", testEmail("admin")).First(&admin))
			edit := withRevision(map[string]any{"active": false, "restricted": true}, adminRoomRevision(t, f.admin, f.hall.ID))
			before := roomEditorState(t, f.app)
			holder := f.app.DB.Begin()
			defer holder.Rollback()
			orig := f.app.DB
			f.app.DB = repeatableReadPool(t, f.app)
			t.Cleanup(func() { f.app.DB = orig })
			gormMust(t, holder.Exec("SELECT 1 FROM rooms WHERE id = ? FOR UPDATE", f.hall.ID))
			answer := make(chan *httptest.ResponseRecorder, 1)
			go func() { answer <- f.admin.do("PUT", "/api/v1/admin/rooms/"+itoa(f.hall.ID), edit, true) }()
			waitForLockedUsersQuery(t, f.app, "%rooms%FOR UPDATE%")
			gormMust(t, holder.Exec("UPDATE users SET "+change+" WHERE id = ?", admin.ID))
			gormMust(t, holder.Commit())
			wantCode(t, "the revoked admin's edit", <-answer, http.StatusUnauthorized, "")
			f.app.DB = orig
			if after := roomEditorState(t, f.app); after != before {
				t.Fatalf("the revoked admin's edit wrote:\n%s\nwant\n%s", after, before)
			}
		})
	}
}

// A second admin's edit that waits on the first's lock is compared after the
// wait: the first commits a change, and the waiting edit, named from the same
// read, is refused with nothing written.
func TestRoomEditComparesAfterTheLockWait(t *testing.T) {
	f, other := newRoomEditorFixture(t)
	read := adminRoomRevision(t, f.admin, f.hall.ID)
	holder := f.app.DB.Begin()
	defer holder.Rollback()
	gormMust(t, holder.Exec("SELECT 1 FROM rooms WHERE id = ? FOR UPDATE", f.hall.ID))
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answer <- other.do("PUT", "/api/v1/admin/rooms/"+itoa(f.hall.ID), withRevision(map[string]any{"active": false}, read), true)
	}()
	waitForLockedUsersQuery(t, f.app, "%rooms%FOR UPDATE%")
	gormMust(t, holder.Exec("UPDATE rooms SET default_setup_minutes = 45, updated_at = now() WHERE id = ?", f.hall.ID))
	gormMust(t, holder.Commit())
	before := roomEditorState(t, f.app)
	wantCode(t, "the edit that waited", <-answer, http.StatusConflict, "room_changed")
	if after := roomEditorState(t, f.app); after != before {
		t.Fatalf("the edit that waited wrote:\n%s\nwant\n%s", after, before)
	}
}
