package app

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fold/internal/audit"
	"fold/internal/events"
	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Room feeds (M2.2b, 8.2): an admin-issued secret link per room that
// exports its confirmed bookings, so the church office or a calendar on
// the door can follow the room. Only admins make or turn off the link
// (section 6). The link is the only credential and anyone who can see the
// screen can read it, so the feed describes only dates a signed-out viewer
// can see; every other date is titled "Reserved" without identifying it.

const roomFeedTitle = "Reserved"

type roomFeedJSON struct {
	HasToken  bool       `json:"hasToken"`
	CreatedAt *time.Time `json:"createdAt"`
	Revision  string     `json:"revision"`
	// URL is the raw link, only in the response that made it.
	URL string `json:"url,omitempty"`
}

func (a *App) roomFeedURL(roomID uint, raw string) string {
	return a.emailBaseURL() + "/cal/rooms/" + strconv.FormatUint(uint64(roomID), 10) + "/" + raw + ".ics"
}

// The latest row still names the state after the link has been turned off.
func latestRoomFeed(tx *gorm.DB, roomID uint) (models.RoomFeedToken, error) {
	var tok models.RoomFeedToken
	err := tx.Select("id", "created_at", "revoked_at").Where("room_id = ?", roomID).
		Order("id DESC").Limit(1).Find(&tok).Error
	return tok, err
}

func (a *App) roomFeedOf(roomID uint, tok models.RoomFeedToken) roomFeedJSON {
	out := roomFeedJSON{Revision: a.roomFeedRevision(roomID, tok)}
	if tok.ID != 0 && tok.RevokedAt == nil {
		out.HasToken = true
		out.CreatedAt = &tok.CreatedAt
	}
	return out
}

func roomFeedRoomID(c *gin.Context) (uint, error) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 63)
	if err != nil || id == 0 {
		return 0, errRoomGone
	}
	return uint(id), nil
}

func (a *App) apiAdminGetRoomFeed(c *gin.Context) {
	id, err := roomFeedRoomID(c)
	if err != nil {
		fail(c, err)
		return
	}
	var out roomFeedJSON
	var judged sessionJudgment
	err = decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if err := roomsOnIn(tx); err != nil {
			return err
		}
		me, err := lockRoomManagerIn(tx, apiUser(c), actorRead)
		if err != nil {
			return err
		}
		judged.judge(me)
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("admin room feed")
		}
		if err := tx.Select("id").First(&models.Room{}, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errRoomGone
			}
			return err
		}
		tok, err := latestRoomFeed(tx, id)
		if err != nil {
			return err
		}
		out = a.roomFeedOf(id, tok)
		return nil
	})
	if ackErr := a.acknowledgeDecision(c, judged, err == nil || decidedRefusal(err)); ackErr != nil {
		err = ackErr
	}
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, roomFeedResponseJSON{Feed: out})
}

func (a *App) apiAdminCreateRoomFeed(c *gin.Context) {
	a.changeRoomFeed(c, true)
}

func (a *App) apiAdminRevokeRoomFeed(c *gin.Context) {
	a.changeRoomFeed(c, false)
}

func (a *App) changeRoomFeed(c *gin.Context, issue bool) {
	id, err := roomFeedRoomID(c)
	if err != nil {
		fail(c, err)
		return
	}
	var body struct {
		Revision string `json:"revision"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}

	var out roomFeedJSON
	var raw string
	var judged sessionJudgment
	err = decidingTransaction(a.DB.WithContext(c.Request.Context()), readCommitted, func(tx *gorm.DB) error {
		room, err := lockRoom(tx, id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errRoomGone
			}
			return err
		}
		me, err := lockRoomManagerIn(tx, apiUser(c), actorRead)
		if err != nil {
			return err
		}
		judged.judge(me)
		if err := roomsOnIn(tx); err != nil {
			return err
		}
		if issue && !room.Active {
			return errRoomGone
		}
		tok, err := a.checkRoomFeedRevision(tx, id, body.Revision)
		if err != nil {
			return err
		}

		hadLink := tok.ID != 0 && tok.RevokedAt == nil
		if hadLink {
			now := time.Now().UTC()
			if err := tx.Model(&models.RoomFeedToken{}).Where("id = ? AND revoked_at IS NULL", tok.ID).
				Update("revoked_at", now).Error; err != nil {
				return err
			}
			tok.RevokedAt = &now
		}
		entry := audit.Entry{Actor: &me.ID, Action: audit.RoomFeedRevoked, TargetType: "room", TargetID: id, IP: c.ClientIP()}
		if issue {
			raw, err = newRawToken()
			if err != nil {
				return err
			}
			tok = models.RoomFeedToken{RoomID: id, TokenHash: hashToken(raw), CreatedBy: &me.ID}
			if err := tx.Create(&tok).Error; err != nil {
				return err
			}
			entry.Action = audit.RoomFeedCreated
			entry.Meta = map[string]any{"replaced": hadLink}
		}
		if issue || hadLink {
			if err := audit.Write(tx, entry); err != nil {
				return err
			}
		}
		out = a.roomFeedOf(id, tok)
		return nil
	})
	if ackErr := a.acknowledgeDecision(c, judged, err == nil || decidedRefusal(err)); ackErr != nil {
		err = ackErr
	}
	if err != nil {
		fail(c, err)
		return
	}
	if raw != "" {
		out.URL = a.roomFeedURL(id, raw)
	}
	c.JSON(http.StatusOK, roomFeedResponseJSON{Feed: out})
}

// feedRoom serves /cal/rooms/<roomId>/<token>.ics. Every failure that
// isn't the server's is the same plain 404: rooms off, a wrong, revoked or
// other room's token, or a room that was deactivated. Deactivating keeps
// the link, so turning the room back on brings it back; an admin turns the
// link off to end it. The switch, the link, the room and the bookings are
// read in one snapshot (serveFeedSnapshot), so a link revoked or rooms turned
// off never admit bookings written after that.
func (a *App) feedRoom(c *gin.Context) {
	param := c.Param("token")
	roomID, err := strconv.ParseUint(c.Param("roomId"), 10, 63)
	if err != nil || !strings.HasSuffix(param, ".ics") {
		feedNotFound(c)
		return
	}
	hash := hashToken(strings.TrimSuffix(param, ".ics"))
	a.serveFeedSnapshot(c, func(tx *gorm.DB, now time.Time) (events.Feed, error) {
		if on, err := boolSettingErrIn(tx, settingRoomsEnabled, false); err != nil {
			return events.Feed{}, err
		} else if !on {
			return events.Feed{}, errFeedNotFound
		}
		var tok models.RoomFeedToken
		if err := tx.Where("token_hash = ? AND revoked_at IS NULL", hash).Limit(1).Find(&tok).Error; err != nil {
			return events.Feed{}, err
		}
		if tok.ID == 0 || uint64(tok.RoomID) != roomID {
			return events.Feed{}, errFeedNotFound
		}
		var room models.Room
		if err := tx.Where("id = ?", tok.RoomID).Limit(1).Find(&room).Error; err != nil {
			return events.Feed{}, err
		}
		if room.ID == 0 || !room.Active {
			return events.Feed{}, errFeedNotFound
		}
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized("room feed")
		}
		return a.buildRoomFeedIn(tx, &room, now)
	})
}

// buildRoomFeedIn lists, on db, the room's confirmed bookings whose date
// starts in the feed window. Times are the event's, without setup and
// teardown.
func (a *App) buildRoomFeedIn(db *gorm.DB, room *models.Room, now time.Time) (events.Feed, error) {
	windowStart, windowEnd := feedWindow(now)
	roomLine := placeLine(&occurrenceRoomJSON{ID: room.ID, Name: room.Name, Building: room.Building}, "")
	host := a.feedHost()
	feed := events.Feed{Name: roomLine, Host: host, WindowStart: windowStart, WindowEnd: windowEnd, Singles: []events.FeedSingle{}}
	tz := "UTC"
	lenientReadIn(db, func(sp *gorm.DB) { tz = churchZoneOrUTC(sp).String() })

	var bookings []models.RoomBooking
	if err := db.Joins("JOIN event_occurrences o ON o.id = room_bookings.occurrence_id").
		Where("room_bookings.room_id = ? AND room_bookings.status = ? AND o.status = ? AND o.starts_at >= ? AND o.starts_at < ?",
			room.ID, models.BookingConfirmed, models.OccurrenceScheduled, windowStart, windowEnd).
		Order("room_bookings.starts_at, room_bookings.id").Find(&bookings).Error; err != nil {
		return events.Feed{}, err
	}
	if len(bookings) == 0 {
		return feed, nil
	}
	occIDs := make([]uint, 0, len(bookings))
	for _, b := range bookings {
		occIDs = append(occIDs, b.OccurrenceID)
	}
	occByID := map[uint]*models.EventOccurrence{}
	evByID := map[uint]*models.Event{}
	groups := map[uint]*models.Group{}
	if err := eachIDChunk(occIDs, func(chunk []uint) error {
		var occs []models.EventOccurrence
		if err := db.Where("id IN ?", chunk).Find(&occs).Error; err != nil {
			return err
		}
		evIDs, gIDs := []uint{}, []uint{}
		wantEv, wantGroup := map[uint]bool{}, map[uint]bool{}
		for i := range occs {
			occByID[occs[i].ID] = &occs[i]
			if id := occs[i].EventID; evByID[id] == nil && !wantEv[id] {
				wantEv[id] = true
				evIDs = append(evIDs, id)
			}
		}
		var evs []models.Event
		if err := db.Where("id IN ?", evIDs).Find(&evs).Error; err != nil {
			return err
		}
		for i := range evs {
			evByID[evs[i].ID] = &evs[i]
			if g := evs[i].GroupID; g != nil && groups[*g] == nil && !wantGroup[*g] {
				wantGroup[*g] = true
				gIDs = append(gIDs, *g)
			}
		}
		// Soft-deleted groups aren't loaded, so their dates are Reserved.
		var gs []models.Group
		if err := db.Where("id IN ?", gIDs).Find(&gs).Error; err != nil {
			return err
		}
		for i := range gs {
			groups[gs[i].ID] = &gs[i]
		}
		return nil
	}); err != nil {
		return events.Feed{}, err
	}

	for _, b := range bookings {
		occ := occByID[b.OccurrenceID]
		if occ == nil {
			continue
		}
		ev := evByID[occ.EventID]
		single := events.FeedSingle{
			UID:   "room-" + strconv.FormatUint(uint64(room.ID), 10) + "-booking-" + strconv.FormatUint(uint64(b.ID), 10) + "@" + host,
			Title: roomFeedTitle, Location: roomLine, TZID: tz,
			// The date's own and its group's change time too, so a booking that
			// turns Reserved (a date override, a group archived or set back to
			// pending) gets a newer DTSTAMP and calendars replace its old title.
			StartsAt: b.StartsAt, EndsAt: b.EndsAt, UpdatedAt: laterTime(occ.UpdatedAt, b.UpdatedAt), Status: "CONFIRMED",
		}
		if ev != nil && ev.GroupID != nil && groups[*ev.GroupID] != nil {
			// Group edits share one timestamp; all of them refresh the feed so a
			// status change replaces any details already cached by calendars.
			single.UpdatedAt = laterTime(single.UpdatedAt, groups[*ev.GroupID].UpdatedAt)
		}
		if ev != nil && roomFeedDescribes(ev, occ, groups) {
			grp := (*models.Group)(nil)
			if ev.GroupID != nil {
				grp = groups[*ev.GroupID]
			}
			groupLoc := ""
			if grp != nil && policy.CanSeeGroup(nil, asGroup(grp), policy.Rel{}) {
				groupLoc = grp.Location // group-ok: the group is public, so its place isn't hidden
			}
			eff := occurrenceToJSON(ev, occ, groupLoc)
			single.Title = eff.Title
			single.Location = placeLine(&occurrenceRoomJSON{ID: room.ID, Name: room.Name, Building: room.Building}, eff.Location)
			single.URL = a.emailBaseURL() + occurrenceDatePath(ev.ID, models.FormatCalendarDate(occ.OriginalDate))
			single.UpdatedAt = laterTime(laterTime(laterTime(ev.UpdatedAt, occ.UpdatedAt), b.UpdatedAt), single.UpdatedAt)
			// Only a public series contributes its description; a private or hidden
			// series opened for one date shows the title alone.
			if ev.Visibility == models.VisibilityPublic {
				single.Description = ev.Description
			}
		}
		feed.Singles = append(feed.Singles, single)
	}
	return feed, nil
}

// roomFeedDescribes: the room feed names a booking's event only when a
// signed-out viewer could see that date. A feed has no viewer, so the link
// holder is the public, and the rule is canSeeDate's signed-out path (PLAN
// 8.2, policy.CanSeeEvent): a group event needs a loaded (not deleted),
// active group and a public effective visibility. Every other confirmed
// booking, including one whose group was archived or set back to pending
// after booking, stays in the feed as a Reserved busy block.
func roomFeedDescribes(ev *models.Event, occ *models.EventOccurrence, groups map[uint]*models.Group) bool {
	return visibleSet{factSet: factSet{Groups: groups}}.canSeeDate(nil, ev, occ)
}
