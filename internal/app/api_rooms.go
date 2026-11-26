package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"fold/internal/audit"
	"fold/internal/policy"
	"fold/internal/rooms"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Rooms (M2.1a, 9.1). Optional by design: while the rooms_enabled setting
// is off, every room endpoint answers 404 and no room UI shows. Admins add,
// edit and deactivate rooms; rooms are never deleted, so bookings keep
// their history.

// Room field limits, matching the CHECKs in the baseline migration.
const (
	maxRoomName     = 100
	maxRoomBuilding = 100
	maxRoomFeatures = 500
	maxRoomNotes    = 2000
	maxRoomCapacity = 100000
)

func (a *App) roomsEnabled() bool {
	return boolSettingIn(a.DB, settingRoomsEnabled, false)
}

// roomsOn hides every room route while rooms are off, before auth, so a
// church that doesn't use rooms shows no trace of them.
func (a *App) roomsOn() gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.roomsEnabled() {
			c.Next()
			return
		}
		fail(c, errRoomRouteHidden)
		c.Abort()
	}
}

// Only the event and date pickers distinguish a disabled switch from a missing resource.
var (
	errRoomsOff        = apiErr(http.StatusNotFound, "rooms_off", "room booking is turned off")
	errRoomRouteHidden = apiErr(http.StatusNotFound, "", "not found")
)

func (a *App) requireRooms(off *apiError) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := roomsOnIn(a.DB, off); err != nil {
			fail(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}

func roomsOnIn(tx *gorm.DB, off *apiError) error {
	on, err := boolSettingErrIn(tx, settingRoomsEnabled, false)
	if err == nil && !on {
		return off
	}
	return err
}

// roomManager lets through only people who may manage rooms (section 6).
func (a *App) roomManager() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !policy.CanManageRooms(asPerson(apiUser(c))) {
			jsonError(c, http.StatusForbidden, "admin required")
			c.Abort()
			return
		}
		c.Next()
	}
}

func lockRoomManagerIn(tx *gorm.DB, session *models.User, mode actorLock) (*models.User, error) {
	actor, err := lockActorIn(tx, session, mode)
	if err != nil {
		return nil, err
	}
	if !policy.CanManageRooms(asPerson(actor)) {
		return nil, errSecuritySession
	}
	return actor, nil
}

func (a *App) readAdminRoomList(c *gin.Context, surface string, read func(*gorm.DB) error) bool {
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if a.hooks.readerBegin != nil {
			a.hooks.readerBegin(surface)
		}
		if err := roomsOnIn(tx, errRoomRouteHidden); err != nil {
			return err
		}
		me, err := lockRoomManagerIn(tx, apiUser(c), actorRead)
		if err != nil {
			return err
		}
		judged.judge(me)
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized(surface)
		}
		return read(tx)
	})
	if err == nil {
		err = a.acknowledgeDecision(c, judged, true)
	}
	if err != nil {
		fail(c, err)
		return false
	}
	return true
}

type roomJSON struct {
	ID                     uint   `json:"id"`
	Name                   string `json:"name"`
	Building               string `json:"building"`
	Capacity               *int   `json:"capacity"`
	Features               string `json:"features"`
	Notes                  string `json:"notes"`
	Active                 bool   `json:"active"`
	Restricted             bool   `json:"restricted"`
	DefaultSetupMinutes    int    `json:"defaultSetupMinutes"`
	DefaultTeardownMinutes int    `json:"defaultTeardownMinutes"`
	// Revision is the room's token (roomRevisionIn), read with the fields
	// beside it; an edit names it (DEC-171).
	Revision string `json:"revision"`
}

func toRoomJSON(r *models.Room, revision string) roomJSON {
	return roomJSON{ID: r.ID, Name: r.Name, Building: r.Building, Capacity: r.Capacity, Features: r.Features,
		Notes: r.Notes, Active: r.Active, Restricted: r.Restricted,
		DefaultSetupMinutes: r.DefaultSetupMinutes, DefaultTeardownMinutes: r.DefaultTeardownMinutes, Revision: revision}
}

// roomBody is the create and edit body. On edit, an absent field keeps its
// value; capacity is raw so null (no limit) differs from absent.
type roomBody struct {
	Name                   *string         `json:"name"`
	Building               *string         `json:"building"`
	Capacity               json.RawMessage `json:"capacity"`
	Features               *string         `json:"features"`
	Notes                  *string         `json:"notes"`
	Active                 *bool           `json:"active"`
	Restricted             *bool           `json:"restricted"`
	DefaultSetupMinutes    *int            `json:"defaultSetupMinutes"`
	DefaultTeardownMinutes *int            `json:"defaultTeardownMinutes"`
}

// apply copies the body onto r and returns the names of the fields that
// changed, or the refusal for a field that is out of bounds.
func (b roomBody) apply(r *models.Room) ([]string, error) {
	var changed []string
	text := func(field string, v *string, max int, dst *string, tooLong error) error {
		if v == nil {
			return nil
		}
		s := strings.TrimSpace(*v)
		if utf8.RuneCountInString(s) > max {
			return tooLong
		}
		if s != *dst {
			*dst = s
			changed = append(changed, field)
		}
		return nil
	}
	for _, check := range []func() error{
		func() error { return text("name", b.Name, maxRoomName, &r.Name, errNameTooLong) },
		func() error {
			return text("building", b.Building, maxRoomBuilding, &r.Building, errBuildingTooLong)
		},
		func() error {
			return text("features", b.Features, maxRoomFeatures, &r.Features, errFeaturesTooLong)
		},
		func() error { return text("notes", b.Notes, maxRoomNotes, &r.Notes, errNotesTooLong) },
	} {
		if err := check(); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(r.Name) == "" {
		return nil, errRoomNameRequired
	}
	if len(b.Capacity) > 0 {
		capacity, ok := parseOptionalPositive(b.Capacity, maxRoomCapacity)
		if !ok {
			return nil, errInvalidRoomCapacity
		}
		if !equalIntPtr(capacity, r.Capacity) {
			r.Capacity = capacity
			changed = append(changed, "capacity")
		}
	}
	minutes := func(field string, v *int, dst *int) error {
		if v == nil {
			return nil
		}
		if *v < 0 || *v > rooms.MaxPaddingMinutes {
			return errInvalidSetupMinutes
		}
		if *v != *dst {
			*dst = *v
			changed = append(changed, field)
		}
		return nil
	}
	if err := minutes("defaultSetupMinutes", b.DefaultSetupMinutes, &r.DefaultSetupMinutes); err != nil {
		return nil, err
	}
	if err := minutes("defaultTeardownMinutes", b.DefaultTeardownMinutes, &r.DefaultTeardownMinutes); err != nil {
		return nil, err
	}
	flag := func(field string, v *bool, dst *bool) {
		if v != nil && *v != *dst {
			*dst = *v
			changed = append(changed, field)
		}
	}
	flag("active", b.Active, &r.Active)
	flag("restricted", b.Restricted, &r.Restricted)
	return changed, nil
}

// parseOptionalPositive reads null as "no limit" and a whole number from 1
// to max as itself.
func parseOptionalPositive(raw json.RawMessage, max int) (*int, bool) {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < 1 || n > max {
		return nil, false
	}
	return &n, true
}

func equalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func roomNameTaken(err error) bool {
	return pgConstraint(err, pgUniqueViolation, "idx_rooms_building_name")
}

// apiAdminListRooms reads the rooms, their revisions and the approval switch
// in one snapshot, so a revision never belongs to newer fields than the ones
// beside it.
func (a *App) apiAdminListRooms(c *gin.Context) {
	out := adminRoomsJSON{Rooms: []roomJSON{}}
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		var rooms []models.Room
		if err := tx.Order("active DESC, lower(building), lower(name)").Find(&rooms).Error; err != nil {
			return err
		}
		ids := make([]uint, len(rooms))
		for i := range rooms {
			ids[i] = rooms[i].ID
		}
		revisions, err := a.roomRevisionsIn(tx, ids)
		if err != nil {
			return err
		}
		for i := range rooms {
			revision, ok := revisions[rooms[i].ID]
			if !ok {
				return fmt.Errorf("room %d: no revision in the list's snapshot", rooms[i].ID)
			}
			out.Rooms = append(out.Rooms, toRoomJSON(&rooms[i], revision))
		}
		out.NeedApproval, err = boolSettingErrIn(tx, settingRoomApproval, false)
		return err
	}, readSnapshot)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not list rooms")
		return
	}
	c.JSON(http.StatusOK, out)
}

func (a *App) apiAdminCreateRoom(c *gin.Context) {
	var body roomBody
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	body.Active = nil
	room := models.Room{Active: true}
	if _, err := body.apply(&room); err != nil {
		fail(c, err)
		return
	}
	var revision string
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		me, err := lockRoomManagerIn(tx, apiUser(c), actorShare)
		if err != nil {
			return err
		}
		if err := tx.Create(&room).Error; err != nil {
			return err
		}
		if err := audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.RoomCreated, TargetType: "room", TargetID: room.ID,
			Meta: map[string]any{"name": room.Name, "building": room.Building}, IP: c.ClientIP()}); err != nil {
			return err
		}
		revision, err = a.roomRevisionIn(tx, room.ID)
		return err
	}, readCommitted)
	if roomNameTaken(err) {
		fail(c, errRoomNameTaken)
		return
	}
	if isAPIError(err) {
		fail(c, err)
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not add the room")
		return
	}
	c.JSON(http.StatusCreated, roomResponseJSON{Room: toRoomJSON(&room, revision)})
}

// apiAdminUpdateRoom saves an admin's edit of a room. The edit names the read
// it was made from (revision, roomRevisionIn): an older one, or none, is 409
// room_changed, writing nothing (DEC-171).
func (a *App) apiAdminUpdateRoom(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 63)
	if err != nil || id == 0 {
		jsonError(c, http.StatusNotFound, "room not found")
		return
	}
	var body struct {
		roomBody
		Revision string `json:"revision"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	var room models.Room
	var revision string
	errNotFound := errors.New("not found")
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		// The room's lock is the last every booking writer takes (lockRoom),
		// and this holds no other: then the actor, read after the wait.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&room, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errNotFound
			}
			return err
		}
		me, err := lockRoomManagerIn(tx, apiUser(c), actorRead)
		if err != nil {
			return err
		}
		if err := a.checkRoomRevision(tx, room.ID, body.Revision); err != nil {
			return err
		}
		changed, err := body.apply(&room)
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			if err := tx.Save(&room).Error; err != nil {
				return err
			}
			if err := audit.Write(tx, audit.Entry{Actor: &me.ID, Action: audit.RoomUpdated, TargetType: "room", TargetID: room.ID,
				Meta: map[string]any{"changed": changed}, IP: c.ClientIP()}); err != nil {
				return err
			}
		}
		revision, err = a.roomRevisionIn(tx, room.ID)
		return err
	}, readCommitted)
	switch {
	case errors.Is(err, errNotFound):
		jsonError(c, http.StatusNotFound, "room not found")
	case isAPIError(err):
		fail(c, err)
	case roomNameTaken(err):
		fail(c, errRoomNameTaken)
	case err != nil:
		jsonError(c, http.StatusInternalServerError, "could not save the room")
	default:
		c.JSON(http.StatusOK, roomResponseJSON{Room: toRoomJSON(&room, revision)})
	}
}

// apiSetRoomsEnabled turns rooms on or off (9.1). Turning them off hides
// every room screen and endpoint; nothing is deleted.
func (a *App) apiSetRoomsEnabled(c *gin.Context) {
	if enabled, ok := a.setRoomSwitch(c, settingRoomsEnabled, audit.RoomsEnabledChanged); ok {
		c.JSON(http.StatusOK, roomsEnabledResponseJSON{RoomsEnabled: enabled})
	}
}

// setRoomSwitch is the body of both rooms switches: read {enabled}, store it
// in the setting and audit it in one transaction. It answers the errors
// itself and reports ok only after a saved change.
func (a *App) setRoomSwitch(c *gin.Context, key, action string) (enabled, ok bool) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return false, false
	}
	err := a.DB.Transaction(func(tx *gorm.DB) error {
		me, err := lockRoomManagerIn(tx, apiUser(c), actorShare)
		if err != nil {
			return err
		}
		return writeAuditedSetting(tx, key, strconv.FormatBool(*body.Enabled), audit.Entry{
			Actor: &me.ID, Action: action, Meta: map[string]any{"enabled": *body.Enabled}, IP: c.ClientIP(),
		})
	}, readCommitted)
	if isAPIError(err) {
		fail(c, err)
		return false, false
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "could not save the setting")
		return false, false
	}
	return *body.Enabled, true
}
