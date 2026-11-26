package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"fold/models"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// dateFactsQuery names the rows one loadDateFacts statement reads. Every list
// may be empty.
type dateFactsQuery struct {
	// MessageID, when set, reads that message's status, scheduled time and
	// audience as they stand in the same snapshot.
	MessageID uint
	// People are the accounts loaded (deleted ones too, marked), with each
	// teen's latest account consent.
	People []uint
	// Viewers are the people whose memberships (any status) and church-wide
	// host rows are loaded, for every group and event below.
	Viewers []uint
	// Holders are the people whose registrations are loaded, on every date of
	// the events below: the ones they hold (their own, or one they made for a
	// household person) and so every own registration, with its live grant
	// columns.
	Holders []uint
	// EventIDs and the events of OccurrenceIDs are the events whose holder
	// registrations are loaded. Every date those registrations name is loaded,
	// with OccurrenceIDs themselves.
	EventIDs      []uint
	OccurrenceIDs []uint
	// GroupIDs are groups loaded besides the loaded events' groups.
	GroupIDs []uint
	// SeriesTitles are events whose scheduled, untitled dates' visibility
	// overrides are loaded (the hidden-series title probe).
	SeriesTitles []uint
	// Teens are the teens whose confirmed guardian links are loaded.
	Teens []uint
	// Settings are the settings keys read.
	Settings []string
}

// dateFactsMessage is a message as the facts statement read it.
type dateFactsMessage struct {
	ID          uint       `json:"id"`
	Status      string     `json:"status"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	Audience    string     `json:"audience"`
}

// dateFacts is everything one statement read: one snapshot, so no answer
// pairs a row with another committed after it. Rows are loaded whether or
// not deleted; memDates applies the soft-delete rule each query would.
type dateFacts struct {
	q dateFactsQuery
	// asked is q's id lists as sets, for memDates' coverage checks.
	asked    dateFactsAsked
	message  *dateFactsMessage
	users    map[uint]*models.User
	consents map[uint]models.TeenConsent
	events   map[uint]*models.Event
	groups   map[uint]*models.Group
	members  []models.Membership
	hosts    map[uint]map[uint]bool
	occs     map[uint]*models.EventOccurrence
	regs     []models.Registration
	live     map[uint]bool
	untitled map[uint][]string
	links    []models.GuardianLink
	settings map[string]string
	// The rows above indexed once, so each question reads only its own
	// person's rows: memberships by user, registrations by their user and by
	// their holder (holderOf), and the events the loaded dates name.
	membersOf map[uint][]*models.Membership
	ownRegs   map[uint][]*models.Registration
	heldRegs  map[uint][]*models.Registration
	occEvents map[uint]bool
}

// index builds f's lookup maps from its rows.
func (f *dateFacts) index() {
	f.membersOf, f.ownRegs, f.heldRegs, f.occEvents = map[uint][]*models.Membership{}, map[uint][]*models.Registration{},
		map[uint][]*models.Registration{}, map[uint]bool{}
	for i := range f.members {
		m := &f.members[i]
		f.membersOf[m.UserID] = append(f.membersOf[m.UserID], m)
	}
	for i := range f.regs {
		r := &f.regs[i]
		if r.UserID != nil {
			f.ownRegs[*r.UserID] = append(f.ownRegs[*r.UserID], r)
		}
		f.heldRegs[holderOf(*r)] = append(f.heldRegs[holderOf(*r)], r)
	}
	for _, o := range f.occs {
		f.occEvents[o.EventID] = true
	}
}

// factColumns are the only columns the facts statement reads of each table:
// the ones a claimed row's or Try again's decisions and merge fields read
// (each named with its reader in factColumnReaders), and nothing else. The
// statement travels as one JSON value per row, so a column no decision reads
// stays out even when it is harmless: places (an event's, a date's or a
// group's location, a room), a group's name and the event's other settings
// most of all, since they belong to events and groups the reader may not see.
// A user's password is read only as whether one is set (has_password). No
// decision reads a person's age or contact preference: an exact-date grant
// judges its recipient as a policy.OccurrenceActor (occurrenceActorOf), which
// has neither, so birth_date and hide_contact_from_leaders stay out and no row
// here can stand for an adult by a missing birth date.
// TestFactColumnsCoverTheModels holds every model column to a named reader or
// a reason it is left out.
var factColumns = map[string][]string{
	"users":         {"id", "name", "display_name", "role", "status", "account_type", "deleted_at"},
	"teen_consents": {"teen_user_id", "status"},
	"events":        {"id", "group_id", "title", "visibility", "tzid", "reminder_email_minutes", "deleted_at"},
	"groups":        {"id", "status", "visibility", "deleted_at"},
	"memberships":   {"id", "user_id", "group_id", "role", "status"},
	"event_occurrences": {"id", "event_id", "original_date", "starts_at", "ends_at", "status", "visibility_override",
		"title_override"},
	"registrations": {"id", "occurrence_id", "user_id", "registered_by", "status", "access_origin", "access_issuer_id",
		"access_request_id", "access_departure_membership_id", "access_departure_actor_id", "access_granted_at",
		"access_ended_at", "access_end_reason"},
	"guardian_links": {"guardian_user_id", "teen_user_id"},
}

// factCols is table's factColumns, each qualified by alias.
func factCols(table, alias string) string {
	cols := factColumns[table]
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + c
	}
	return strings.Join(out, ", ")
}

// dateFactsSQL is the one statement. Its CTEs follow the dependency chain the
// separate queries walk (named dates, their events, holder registrations, the
// dates those name, their events and groups, the viewers' memberships and host
// rows), and every branch reads the statement's single snapshot. Each id list
// is one bigint[] parameter, so the statement binds the same few parameters
// however many people it reads.
var dateFactsSQL = `
WITH named_occ AS (
	SELECT id, event_id FROM event_occurrences WHERE id = ANY(CAST(@occurrences AS bigint[]))
), evset AS (
	SELECT id FROM events WHERE id = ANY(CAST(@events AS bigint[]))
	UNION SELECT event_id FROM named_occ
), held AS (
	SELECT ` + factCols("registrations", "registrations") + `, (` + registrationAccessLiveSQL + `) AS fold_access_live
	FROM registrations JOIN event_occurrences o ON o.id = registrations.occurrence_id
	WHERE o.event_id IN (SELECT id FROM evset)
	AND COALESCE(registrations.user_id, registrations.registered_by) = ANY(CAST(@holders AS bigint[]))
), occs AS (
	SELECT ` + factCols("event_occurrences", "o") + ` FROM event_occurrences o
	WHERE o.id IN (SELECT id FROM named_occ UNION SELECT occurrence_id FROM held)
), evs AS (
	SELECT ` + factCols("events", "e") + ` FROM events e WHERE e.id IN (SELECT id FROM evset UNION SELECT event_id FROM occs)
), grps AS (
	SELECT ` + factCols("groups", "g") + ` FROM groups g
	WHERE g.id IN (SELECT group_id FROM evs WHERE group_id IS NOT NULL) OR g.id = ANY(CAST(@groups AS bigint[]))
), people AS (
	SELECT ` + factCols("users", "u") + `, u.password_hash IS NOT NULL AS has_password
	FROM users u WHERE u.id = ANY(CAST(@people AS bigint[]))
)
SELECT
	(SELECT to_jsonb(m) FROM (SELECT id, status, scheduled_at, audience::text AS audience FROM messages WHERE id = @message) m) AS message,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM people x) AS users,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM (
		SELECT DISTINCT ON (c.teen_user_id) ` + factCols("teen_consents", "c") + ` FROM teen_consents c
		WHERE c.teen_user_id IN (SELECT id FROM people WHERE account_type = @teen) AND c.scope = @consentScope
		ORDER BY c.teen_user_id, c.at DESC, c.id DESC) x) AS consents,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM evs x) AS events,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM grps x) AS groups,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM (
		SELECT ` + factCols("memberships", "mb") + ` FROM memberships mb
		WHERE mb.user_id = ANY(CAST(@viewers AS bigint[])) AND mb.group_id IN (SELECT id FROM grps)) x) AS memberships,
	(SELECT COALESCE(jsonb_agg(jsonb_build_object('user_id', h.user_id, 'event_id', h.event_id)), '[]')
		FROM event_hosts h JOIN evs ON evs.id = h.event_id AND evs.group_id IS NULL
		WHERE h.user_id = ANY(CAST(@viewers AS bigint[]))) AS hosts,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM occs x) AS occurrences,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM held x) AS registrations,
	(SELECT COALESCE(jsonb_agg(jsonb_build_object('event_id', u.event_id, 'overrides', u.overrides)), '[]') FROM (
		SELECT event_id, array_agg(DISTINCT visibility_override) AS overrides FROM event_occurrences
		WHERE event_id = ANY(CAST(@series AS bigint[])) AND status = @scheduled AND title_override IS NULL AND visibility_override IS NOT NULL
		GROUP BY event_id) u) AS untitled,
	(SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]') FROM (
		SELECT ` + factCols("guardian_links", "l") + ` FROM guardian_links l
		WHERE l.teen_user_id = ANY(CAST(@teens AS bigint[])) AND l.status = @confirmed) x) AS links,
	(SELECT COALESCE(jsonb_agg(jsonb_build_object('key', s.key, 'value', s.value)), '[]')
		FROM settings s WHERE s.key IN @settings) AS settings
`

// dateFactsArgs are dateFactsSQL's parameters for q: each id list one
// bigint[] literal, whatever its length.
func dateFactsArgs(q dateFactsQuery) map[string]any {
	ids := func(list []uint) string { return pgBigintArray(dedupeIDs(list)) }
	keys := q.Settings
	if len(keys) == 0 {
		keys = []string{""}
	}
	return map[string]any{
		"occurrences": ids(q.OccurrenceIDs), "events": ids(q.EventIDs), "holders": ids(q.Holders),
		"groups": ids(q.GroupIDs), "people": ids(q.People), "viewers": ids(q.Viewers), "series": ids(q.SeriesTitles),
		"teens": ids(q.Teens), "settings": keys, "message": q.MessageID,
		"teen": models.AccountTeen, "consentScope": models.TeenConsentAccount,
		"scheduled": models.OccurrenceScheduled, "confirmed": models.GuardianConfirmed,
	}
}

// factPasswordOnFile stands in for the password hash the statement never
// reads: a loaded user who has one gets this pointer, so CanSignIn answers as
// on the full row. As a hash it matches no password.
var factPasswordOnFile = new(string)

// loadDateFacts reads q's rows in one statement on db, and has consumed and
// closed its result before it returns. A failed read, scan or decode is
// returned, never read as "may not see".
func (a *App) loadDateFacts(db *gorm.DB, q dateFactsQuery) (*dateFacts, error) {
	var raw struct{ message, users, consents, events, groups, members, hosts, occs, regs, untitled, links, settings []byte }
	rows, err := db.Raw(dateFactsSQL, dateFactsArgs(q)).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("date facts: the statement returned no row")
	}
	if err := rows.Scan(&raw.message, &raw.users, &raw.consents, &raw.events, &raw.groups, &raw.members, &raw.hosts,
		&raw.occs, &raw.regs, &raw.untitled, &raw.links, &raw.settings); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if a.hooks.dateFactsRead != nil {
		a.hooks.dateFactsRead()
	}
	f := &dateFacts{q: q, asked: askedOf(q), users: map[uint]*models.User{}, consents: map[uint]models.TeenConsent{}, events: map[uint]*models.Event{},
		groups: map[uint]*models.Group{}, hosts: map[uint]map[uint]bool{}, occs: map[uint]*models.EventOccurrence{},
		live: map[uint]bool{}, untitled: map[uint][]string{}, settings: map[string]string{}}
	if len(raw.message) > 0 && string(raw.message) != "null" {
		f.message = &dateFactsMessage{}
		if err := json.Unmarshal(raw.message, f.message); err != nil {
			return nil, fmt.Errorf("date facts: message: %w", err)
		}
	}
	users, err := decodeFactRows[models.User](raw.users)
	if err != nil {
		return nil, err
	}
	var passwords []struct {
		ID          uint `json:"id"`
		HasPassword bool `json:"has_password"`
	}
	if err := json.Unmarshal(raw.users, &passwords); err != nil {
		return nil, fmt.Errorf("date facts: passwords: %w", err)
	}
	for i := range users {
		if passwords[i].ID != users[i].ID {
			return nil, errors.New("date facts: users out of step")
		}
		if passwords[i].HasPassword {
			users[i].PasswordHash = factPasswordOnFile
		}
		f.users[users[i].ID] = &users[i]
	}
	consents, err := decodeFactRows[models.TeenConsent](raw.consents)
	if err != nil {
		return nil, err
	}
	for _, c := range consents {
		f.consents[c.TeenUserID] = c
	}
	evs, err := decodeFactRows[models.Event](raw.events)
	if err != nil {
		return nil, err
	}
	for i := range evs {
		f.events[evs[i].ID] = &evs[i]
	}
	groups, err := decodeFactRows[models.Group](raw.groups)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		f.groups[groups[i].ID] = &groups[i]
	}
	if f.members, err = decodeFactRows[models.Membership](raw.members); err != nil {
		return nil, err
	}
	var hosts []struct {
		UserID  uint `json:"user_id"`
		EventID uint `json:"event_id"`
	}
	if err := json.Unmarshal(raw.hosts, &hosts); err != nil {
		return nil, fmt.Errorf("date facts: hosts: %w", err)
	}
	for _, h := range hosts {
		if f.hosts[h.UserID] == nil {
			f.hosts[h.UserID] = map[uint]bool{}
		}
		f.hosts[h.UserID][h.EventID] = true
	}
	occs, err := decodeFactRows[models.EventOccurrence](raw.occs)
	if err != nil {
		return nil, err
	}
	for i := range occs {
		f.occs[occs[i].ID] = &occs[i]
	}
	if f.regs, err = decodeFactRows[models.Registration](raw.regs); err != nil {
		return nil, err
	}
	var live []struct {
		ID   uint `json:"id"`
		Live bool `json:"fold_access_live"`
	}
	if err := json.Unmarshal(raw.regs, &live); err != nil {
		return nil, fmt.Errorf("date facts: grants: %w", err)
	}
	for _, l := range live {
		f.live[l.ID] = l.Live
	}
	var untitled []struct {
		EventID   uint     `json:"event_id"`
		Overrides []string `json:"overrides"`
	}
	if err := json.Unmarshal(raw.untitled, &untitled); err != nil {
		return nil, fmt.Errorf("date facts: untitled dates: %w", err)
	}
	for _, u := range untitled {
		f.untitled[u.EventID] = u.Overrides
	}
	if f.links, err = decodeFactRows[models.GuardianLink](raw.links); err != nil {
		return nil, err
	}
	var settings []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw.settings, &settings); err != nil {
		return nil, fmt.Errorf("date facts: settings: %w", err)
	}
	for _, s := range settings {
		f.settings[s.Key] = s.Value
	}
	f.index()
	return f, nil
}

var (
	factSchemas   sync.Map
	timeType      = reflect.TypeFor[time.Time]()
	deletedAtType = reflect.TypeFor[gorm.DeletedAt]()
)

// decodeFactRows reads a jsonb array of the facts statement's rows into
// models of type T, column by gorm column name, as gorm would scan the same
// columns: the table's factColumns, and no other field is set. A listed
// column the model does not have or the row lacks, or a value its field
// cannot hold, is an error: a schema the decoder does not understand stops
// the send.
func decodeFactRows[T any](raw []byte) ([]T, error) {
	var objs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &objs); err != nil {
		return nil, fmt.Errorf("date facts: %w", err)
	}
	sch, err := schema.Parse(new(T), &factSchemas, schema.NamingStrategy{})
	if err != nil {
		return nil, err
	}
	cols, ok := factColumns[sch.Table]
	if !ok {
		return nil, fmt.Errorf("date facts: %s has no column list", sch.Table)
	}
	out := make([]T, len(objs))
	for i, obj := range objs {
		rv := reflect.ValueOf(&out[i]).Elem()
		for _, name := range cols {
			field := sch.FieldsByDBName[name]
			if field == nil {
				return nil, fmt.Errorf("date facts: %s has no model column %s", sch.Table, name)
			}
			value, ok := obj[name]
			if !ok {
				return nil, fmt.Errorf("date facts: %s has no column %s", sch.Table, name)
			}
			if string(value) == "null" {
				continue
			}
			if err := decodeColumn(rv.FieldByIndex(field.StructField.Index), value); err != nil {
				return nil, fmt.Errorf("date facts: %s.%s: %w", sch.Table, name, err)
			}
		}
	}
	return out, nil
}

func decodeColumn(v reflect.Value, raw json.RawMessage) error {
	switch v.Type() {
	case timeType:
		t, err := parsePGTime(raw)
		if err == nil {
			v.Set(reflect.ValueOf(t))
		}
		return err
	case reflect.PointerTo(timeType):
		t, err := parsePGTime(raw)
		if err == nil {
			v.Set(reflect.ValueOf(&t))
		}
		return err
	case deletedAtType:
		t, err := parsePGTime(raw)
		if err == nil {
			v.Set(reflect.ValueOf(gorm.DeletedAt{Time: t, Valid: true}))
		}
		return err
	}
	return json.Unmarshal(raw, v.Addr().Interface())
}

// parsePGTime reads how to_jsonb writes a timestamptz (with its offset, in
// local time as the driver returns it) or a date (midnight UTC, as the driver
// returns it).
func parsePGTime(raw json.RawMessage) (time.Time, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, err
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.In(time.Local), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unreadable time %q", s)
}

// dateFactsAsked is a dateFactsQuery's id lists as sets.
type dateFactsAsked struct {
	people, viewers, holders, events, occurrences, groups, series map[uint]bool
}

func idSet(ids []uint) map[uint]bool {
	out := make(map[uint]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func askedOf(q dateFactsQuery) dateFactsAsked {
	return dateFactsAsked{people: idSet(q.People), viewers: idSet(q.Viewers), holders: idSet(q.Holders), events: idSet(q.EventIDs),
		occurrences: idSet(q.OccurrenceIDs), groups: idSet(q.GroupIDs), series: idSet(q.SeriesTitles)}
}
