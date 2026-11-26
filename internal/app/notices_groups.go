package app

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"fold/internal/audit"
	"fold/internal/notify"
	"fold/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Notices about groups: approval, leadership and who may see what. The rule
// for notice writers: each lives in notices_<domain>.go for the domain it
// tells people about, reads its recipients and names with the error-returning
// helpers in notice_people.go, and returns any failure so the caller's
// transaction rolls back rather than skip a recipient.

const approvalsLink = "/admin?tab=approvals"

func notifyGroupPending(tx *gorm.DB, g *models.Group, actor uint) error {
	admins, err := activeAdminIDs(tx)
	if err != nil {
		return err
	}
	actorName, err := nameOf(tx, actor)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.GroupPending, To: admins, Actor: &actor,
		SubjectType: "group", SubjectID: g.ID, Link: approvalsLink,
		Vars:      notify.Vars{Actor: actorName, Group: g.Name}, // group-ok: admin tasks go to admins
		Data:      map[string]any{"groupId": g.ID},
		DedupeKey: fmt.Sprintf("group.pending:%d", g.ID),
	})
	return err
}

// notifyGroupDecided tells a pending group's leaders how an admin decided
// it (DEC-96), in the decision's transaction. A leader may always see their
// own group, approved or not.
func notifyGroupDecided(tx *gorm.DB, g *models.Group, actor uint, approved bool, note string) error {
	kind := notify.GroupRejected
	if approved {
		kind = notify.GroupApproved
	}
	leaders, err := leaderIDs(tx, g.ID)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: kind, To: leaders, Actor: &actor,
		SubjectType: "group", SubjectID: g.ID, Link: "/groups/" + strconv.FormatUint(uint64(g.ID), 10),
		Vars: notify.Vars{Group: g.Name, Note: note}, // group-ok: its own leaders
		// No dedupe key: the pending → decided transition sends it once, and a
		// group set back to pending and decided again earns a new notice.
	})
	return err
}

func notifyDeletionRequested(tx *gorm.DB, g *models.Group, actor uint) error {
	admins, err := activeAdminIDs(tx)
	if err != nil {
		return err
	}
	actorName, err := nameOf(tx, actor)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.GroupDeletion, To: admins, Actor: &actor,
		SubjectType: "group", SubjectID: g.ID, Link: approvalsLink,
		Vars: notify.Vars{Actor: actorName, Group: g.Name}, // group-ok: admin tasks go to admins
		Data: map[string]any{"groupId": g.ID},
	})
	return err
}

func notifyDeletionKept(tx *gorm.DB, g *models.Group, actor uint) error {
	asker, err := deletionRequesterIn(tx, g)
	if err != nil || asker == 0 || asker == actor {
		return err
	}
	to := []uint{asker}
	facts, err := loadNoticeFacts(tx, []uint{g.ID}, to)
	if err != nil {
		return err
	}
	m := notify.Msg{Kind: notify.GroupDeletionKept, To: to, Actor: &actor, Link: "/my-groups"}
	if facts.canSeeGroup(asker, g) {
		m.Vars.Group = g.Name // group-ok: only for an asker who can still see the group
		m.Link = fmt.Sprintf("/groups/%d", g.ID)
		m.SubjectType, m.SubjectID = "group", g.ID
	}
	_, err = notify.Send(tx, m)
	return err
}

// notifyLeadership tells the other admins about a leadership change, from
// the RoleChanged or MemberRemoved entry just written (DEC-31).
func notifyLeadership(tx *gorm.DB, e audit.Entry) error {
	meta := audit.ReadRoleMeta(e.Meta)
	userID, oldRole, newRole := meta.UserID, meta.OldRole, meta.NewRole
	var change string
	switch {
	case e.Action == audit.MemberRemoved:
		if oldRole != models.MembershipLeader {
			return nil // removing a plain member isn't a leadership change
		}
		change = "removed_from_group"
	case newRole == models.MembershipLeader:
		change = "added"
	case e.Actor != nil && *e.Actor == userID:
		change = "stepped_down"
	default:
		change = "removed"
	}
	var g models.Group
	if err := tx.Unscoped().First(&g, e.TargetID).Error; err != nil {
		return err
	}
	actor := "Fold"
	if e.Actor != nil {
		var err error
		if actor, err = nameOf(tx, *e.Actor); err != nil {
			return err
		}
	}
	person, err := nameOf(tx, userID)
	if err != nil {
		return err
	}
	admins, err := activeAdminIDs(tx)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.LeadershipChanged, To: admins, Actor: e.Actor,
		SubjectType: "group", SubjectID: g.ID, Link: fmt.Sprintf("/groups/%d", g.ID),
		Vars: notify.Vars{Actor: actor, Person: person, Group: g.Name, Change: change}, // group-ok: leadership notices go to admins
		Data: map[string]any{"groupId": g.ID, "userId": userID},
	})
	if err != nil {
		return err
	}
	return notifyYourRole(tx, e, &g, userID, oldRole, newRole)
}

// notifyYourRole tells the person whose role changed that they now lead
// the group, or no longer do and are still a member (DEC-134). It follows
// every RoleChanged entry (a leader's or admin's change, a handoff), never
// a removal, which has its own notices. Someone who changed their own role
// is skipped (the Actor rule in notify.Send).
func notifyYourRole(tx *gorm.DB, e audit.Entry, g *models.Group, userID uint, oldRole, newRole string) error {
	if e.Action != audit.RoleChanged || userID == 0 || g.ID == 0 {
		return nil
	}
	var change string
	switch {
	case newRole == models.MembershipLeader && oldRole != models.MembershipLeader:
		change = "leader"
	case oldRole == models.MembershipLeader && newRole == models.MembershipMember:
		change = "member"
	default:
		return nil
	}
	vars, link := notify.Vars{Change: change}, "/my-groups"
	sees, err := userCanSeeGroupIn(tx, userID, g)
	if err != nil {
		return err
	}
	if sees {
		vars.Group, link = g.Name, fmt.Sprintf("/groups/%d", g.ID) // group-ok: only when userCanSeeGroupIn says the person may see it
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.YourRole, To: []uint{userID}, Actor: e.Actor,
		SubjectType: "group", SubjectID: g.ID, Link: link, Vars: vars,
	})
	return err
}

// notifyNotRestored tells a re-enabled person they were left out of a full
// group (DEC-47). Their membership is already gone in tx, so an
// invite-only group's name and link are left out.
func notifyNotRestored(tx *gorm.DB, actor *uint, g *models.Group, userID uint) error {
	vars, link := notify.Vars{}, "/"
	sees, err := userCanSeeGroupIn(tx, userID, g)
	if err != nil {
		return err
	}
	if sees {
		vars.Group, link = g.Name, fmt.Sprintf("/groups/%d", g.ID) // group-ok: only when userCanSeeGroupIn says the person may see it
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.MemberNotRestored, To: []uint{userID}, Actor: actor,
		SubjectType: "group", SubjectID: g.ID, Link: link, Vars: vars,
	})
	return err
}

// notifyVisibilityOverride tells the group's other leaders and the admins
// that an event is more open than its group (DEC-17, DEC-28).
func notifyVisibilityOverride(tx *gorm.DB, actor uint, ev *models.Event, g *models.Group, newVis, note string) error {
	leaders, err := leaderIDs(tx, g.ID)
	if err != nil {
		return err
	}
	admins, err := activeAdminIDs(tx)
	if err != nil {
		return err
	}
	actorName, err := nameOf(tx, actor)
	if err != nil {
		return err
	}
	_, err = notify.Send(tx, notify.Msg{
		Kind: notify.VisibilityOverride, To: append(leaders, admins...), Actor: &actor,
		SubjectType: "event", SubjectID: ev.ID, Link: fmt.Sprintf("/groups/%d", g.ID),
		Vars: notify.Vars{Actor: actorName, Event: firstNonEmpty(ev.Title, g.Name), // group-ok: leaders and admins only
			Visibility: visWord(newVis), GroupVis: visWord(g.Visibility), Note: note},
		Data: map[string]any{"eventId": ev.ID, "groupId": g.ID},
	})
	return err
}

// groupDeletion is who deleting a group tells (DEC-177): its members, and for
// each of its events the other people eventDeleteTells reads, with the
// members left out so nobody hears twice. eventIDs is every event the delete
// removes, soft-deleted ones included.
type groupDeletion struct {
	members  []uint
	events   []eventDeletion
	eventIDs []uint
}

// eventDeletion is one event's part of a group's deletion: the rows and dates
// tellEventDeleted takes.
type eventDeletion struct {
	ev   *models.Event
	rows []signedUp
	occs map[uint]*models.EventOccurrence
}

// people is how many people the deletion tells: its confirmation's count.
func (d groupDeletion) people() int {
	seen := map[uint]bool{}
	for _, id := range d.members {
		seen[id] = true
	}
	for _, e := range d.events {
		for _, r := range e.rows {
			seen[r.UserID] = true
		}
	}
	return len(seen)
}

// groupDeleteTells is who deleting groupID tells, for the delete's notices and
// its confirmation's count: every active member whose account is listed
// (listedUsers) but the actor, and everyone else a delete of one of its live
// events would tell (eventDeleteTells). With lock, the delete's, the caller
// holds the group's row; this takes every event row of the group, then all
// their dates, each in one statement in id order (the order event-wide
// writers use, registration_locks.go), before it reads anyone. A join, a leave
// or a role change waits on the contact gate and the group row, and a sign-up
// or a withdrawal holds its date until it commits, so every answer that
// returned before the delete is read here, and one still in flight finds its
// group or date gone.
func groupDeleteTells(tx *gorm.DB, groupID, actor uint, now time.Time, lock bool) (groupDeletion, error) {
	var d groupDeletion
	q := tx.Unscoped().Where("group_id = ?", groupID).Order("id")
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var evs []models.Event
	if err := q.Find(&evs).Error; err != nil {
		return d, err
	}
	for _, ev := range evs {
		d.eventIDs = append(d.eventIDs, ev.ID)
	}
	if lock && len(d.eventIDs) > 0 {
		var held []models.EventOccurrence
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
			Where("event_id IN ?", d.eventIDs).Order("id").Find(&held).Error; err != nil {
			return d, err
		}
	}
	if err := listedMembers(tx, groupID).Where("memberships.user_id <> ?", actor).
		Order("memberships.user_id").Pluck("memberships.user_id", &d.members).Error; err != nil {
		return d, err
	}
	for i := range evs {
		if evs[i].DeletedAt.Valid {
			continue
		}
		rows, occs, err := eventDeleteTells(tx, evs[i].ID, actor, now, false)
		if err != nil {
			return d, err
		}
		rows = slices.DeleteFunc(rows, func(r signedUp) bool { return slices.Contains(d.members, r.UserID) })
		if len(rows) > 0 {
			d.events = append(d.events, eventDeletion{ev: &evs[i], rows: rows, occs: occs})
		}
	}
	return d, nil
}

// notifyGroupDeleted tells the people d names that g was deleted, in the
// delete's transaction before the rows go: each member one group.deleted
// notice, named when they may see the group, which links home (the group page
// is gone) and has no place and no other person's name; everyone else signed
// up for an upcoming date of one of its events the event.deleted notice an
// event's own delete writes, one per event. A guardian is never copied on the
// member notice (notify.Kind.NoGuardianCopy).
func notifyGroupDeleted(tx *gorm.DB, actor uint, g *models.Group, d groupDeletion) error {
	if len(d.members) > 0 {
		facts, err := loadNoticeFacts(tx, []uint{g.ID}, d.members)
		if err != nil {
			return err
		}
		err = notify.WithRecipients(tx, d.members, func(scope *notify.Scope) error {
			for _, uid := range d.members {
				m := notify.Msg{
					Kind: notify.GroupDeleted, To: []uint{uid}, Actor: &actor, Link: "/", Scope: scope,
					DedupeKey: fmt.Sprintf("%s:%d", notify.GroupDeleted, g.ID),
				}
				if facts.canSeeGroup(uid, g) {
					m.Vars.Group = g.Name // group-ok: only for a member noticeFacts says may see it
					m.SubjectType, m.SubjectID = "group", g.ID
				}
				if _, err := notify.Send(tx, m); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, e := range d.events {
		if err := tellEventDeleted(tx, actor, e.ev, e.rows, e.occs); err != nil {
			return err
		}
	}
	return nil
}
