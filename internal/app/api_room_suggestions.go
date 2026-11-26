package app

import (
	"log"
	"strings"
	"time"

	"fold/internal/grouptags"
	"fold/internal/policy"
	"fold/internal/rooms"
	"fold/models"

	"gorm.io/gorm"
)

// Room suggestions (M2.3b, 9.3, DEC-110) ride along with each room picker's
// own list: the one-date picker, the series picker and the create forms'
// room check. They are a shortcut, never a gate: every room in the list
// stays pickable, and suggestions only reorder it.

// roomSuggestionJSON is one suggested room: its id, how many counting
// usages its reason names, the reason, and how many of the checked dates
// it is taken on. Never another event's title, group or date (Rule 5).
type roomSuggestionJSON struct {
	RoomID    uint   `json:"roomId"`
	Uses      int    `json:"uses"`
	Reason    string `json:"reason"`
	BusyDates int    `json:"busyDates"`
}

type roomSuggestionsJSON struct {
	Rooms []roomSuggestionJSON `json:"rooms"`
	// OtherKinds: nothing matched the group's kind, so these are labelled
	// "Used by other kinds of events" (Rule 3).
	OtherKinds bool `json:"otherKinds"`
	Dates      int  `json:"dates"`
}

// suggestFor is who is choosing and for what. EventID is 0 on a create
// form; Visibility is the date's effective visibility, the series' own, or
// the one picked on the create form.
type suggestFor struct {
	person     *policy.Person
	eventID    uint
	groupID    *uint
	visibility string
	capacity   *int
	windows    []rooms.Window
	tags       *string
}

// roomSuggestions runs the engine for a picker on db at now: a picker's
// snapshot, so the suggestions come from the state its list does. A failed
// read leaves the picker without suggestions rather than failing it (inside a
// transaction it runs in a savepoint, lenientReadIn): the full list still
// works. Only rooms the picker offers are kept.
func roomSuggestions(db *gorm.DB, now time.Time, s suggestFor, offered map[uint]bool) roomSuggestionsJSON {
	out := roomSuggestionsJSON{Rooms: []roomSuggestionJSON{}}
	subject := rooms.Subject{
		EventID: s.eventID, GroupID: s.groupID, Visibility: s.visibility, Capacity: s.capacity,
		IncludeRestricted: policy.CanBookRoom(s.person, true, true), Windows: s.windows, Now: now.UTC(),
	}
	lenientReadIn(db, func(db *gorm.DB) {
		if s.tags != nil {
			subject.Tags = grouptags.Parse(*s.tags)
		} else if s.groupID != nil {
			var g models.Group
			if err := db.Select("tags").First(&g, *s.groupID).Error; err != nil {
				log.Printf("room suggestions: %v", err)
				return
			}
			subject.Tags = grouptags.Parse(g.Tags)
		}
		res, err := rooms.Suggest(db, subject)
		if err != nil {
			log.Printf("room suggestions: %v", err)
			return
		}
		out.OtherKinds, out.Dates = res.OtherKinds, res.Dates
		for _, r := range res.Suggestions {
			if offered[r.RoomID] {
				out.Rooms = append(out.Rooms, roomSuggestionJSON{RoomID: r.RoomID, Uses: r.Uses, Reason: r.Reason, BusyDates: r.BusyDates})
			}
		}
	})
	return out
}

func windowsOf(occs []models.EventOccurrence) []rooms.Window {
	out := []rooms.Window{}
	for i := range occs {
		if len(out) == rooms.MaxBusyDates {
			break
		}
		out = append(out, rooms.Window{Start: occs[i].StartsAt, End: occs[i].EndsAt, OccurrenceID: occs[i].ID})
	}
	return out
}

func offeredRooms(list []seriesRoomJSON) map[uint]bool {
	out := make(map[uint]bool, len(list))
	for _, r := range list {
		out[r.ID] = true
	}
	return out
}

// createVisibility is V_E on a create form: the visibility picked there,
// else what the event would get (the group's, or public church-wide). An
// empty or unknown value falls back the same way; the create itself refuses
// a bad one, so this only ever guesses what the form will send.
func createVisibility(picked *string, g *models.Group) string {
	if picked != nil && policy.ValidVisibility(strings.TrimSpace(*picked)) {
		return strings.TrimSpace(*picked)
	}
	if g != nil {
		return g.Visibility
	}
	return models.VisibilityPublic
}

func groupTags(g *models.Group) *string {
	if g == nil {
		return nil
	}
	return &g.Tags
}
