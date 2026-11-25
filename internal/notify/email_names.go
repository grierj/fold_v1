package notify

import (
	"encoding/json"

	"fold/models"
)

type emailNameSource uint8

const (
	emailActor emailNameSource = iota + 1
	emailLeadership
	emailHousehold
)

type namedEmail struct {
	source emailNameSource
	title  string
	body   string
}

const emailHouseholdPersonKey = "emailHouseholdPersonId"

// EmailNames describes the names a stored email or guardian copy can carry
// (O-16, #493). Known is false when a row lacks reliable attribution.
type EmailNames struct {
	Known             bool
	UserIDs           []uint
	HouseholdPersonID uint
	Title             string
	Body              string
}

// EmailNames returns the identity to check and the name-free text to send
// when it is unavailable. Legacy registration rows have no household person
// reference, and the registration may have been transferred or deleted.
func (k Kind) EmailNames(n *models.Notification) (EmailNames, bool) {
	names := EmailNames{Title: k.namedEmail.title, Body: k.namedEmail.body}
	switch k.namedEmail.source {
	case emailActor:
		if n.ActorID != nil && *n.ActorID != 0 {
			names.Known = true
			names.UserIDs = []uint{*n.ActorID}
		}
	case emailLeadership:
		var d struct {
			UserID uint `json:"userId"`
		}
		if json.Unmarshal([]byte(n.Data), &d) == nil && d.UserID != 0 {
			names.Known = true
			names.UserIDs = []uint{d.UserID}
			if n.ActorID != nil && *n.ActorID != d.UserID {
				names.UserIDs = append(names.UserIDs, *n.ActorID)
			}
		}
	case emailHousehold:
		var d struct {
			PersonID *uint `json:"emailHouseholdPersonId"`
		}
		if json.Unmarshal([]byte(n.Data), &d) == nil && d.PersonID != nil {
			names.Known = true
			names.HouseholdPersonID = *d.PersonID
		}
	default:
		return EmailNames{}, false
	}
	return names, true
}
