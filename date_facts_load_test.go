package main

import (
	"encoding/json"
	"go/ast"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"fold/internal/guardkit"
	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// factColumnReaders names, for every column the facts statement reads, the
// decision or merge field that reads it. It must list factColumns exactly: a
// column with no reader here is not read.
var factColumnReaders = map[string]map[string]string{
	"users": {
		"id": "row key", "name": "PublicName (first_name, a copy's teen)", "display_name": "PublicName",
		"role": "asPerson (admin)", "status": "CanSignIn, eventViewer, audience and copy rules",
		"account_type": "asPerson (teen), occurrenceActorOf (teen consent), copy rule", "deleted_at": "liveUser",
	},
	"teen_consents": {"teen_user_id": "consents by teen", "status": "occurrenceActorOf (teen consent)"},
	"events": {
		"id": "row key", "group_id": "viewOf, factSetFor, accessScopeOf", "title": "messageEvent, effectiveOccurrenceTitle",
		"visibility": "viewOf, canSeeDate", "tzid": "messageEvent.timeText",
		"reminder_email_minutes": "retryReminderFrom (reminders on)", "deleted_at": "liveEvent",
	},
	"groups": {"id": "row key", "status": "asGroup, occurrenceFactsOf", "visibility": "asGroup", "deleted_at": "liveGroup"},
	"memberships": {
		"id": "membershipRel (exists)", "user_id": "membersOf", "group_id": "factSetFor, groupRel, audienceMembersAmong",
		"role": "membershipRel, audienceMembersAmong", "status": "membershipRel, audienceMembersAmong",
	},
	"event_occurrences": {
		"id": "row key", "event_id": "holdings, candidates, invitations", "original_date": "manage_link (date page path)",
		"starts_at": "holdings, registeredDate, event_time, retryReminderFrom", "ends_at": "occurrenceFactsOf (grant expiry)",
		"status": "holdings, readerDateValues, occurrenceFactsOf", "visibility_override": "canSeeDate, registeredDate",
		"title_override": "messageEvent, effectiveOccurrenceTitle, untitledGrant",
	},
	"registrations": {
		"id": "row key, live grants", "occurrence_id": "holdings, invitations", "user_id": "holderOf, liveGrants, invitations",
		"registered_by": "holderOf", "status": "holdings, registeredDate, invitations",
		"access_origin": "occurrenceAccessPolicy", "access_issuer_id": "occurrenceAccessPolicy",
		"access_request_id": "occurrenceAccessPolicy", "access_departure_membership_id": "occurrenceAccessPolicy",
		"access_departure_actor_id": "occurrenceAccessPolicy", "access_granted_at": "occurrenceAccessPolicy",
		"access_ended_at": "occurrenceAccessPolicy", "access_end_reason": "occurrenceAccessPolicy",
	},
	"guardian_links": {"guardian_user_id": "copyRule pairs", "teen_user_id": "copyRule pairs"},
}

// factColumnsLeftOut is every model column the facts statement does not read,
// and why. A column in neither list fails TestFactColumnsCoverTheModels, so a
// new column is a decision.
var factColumnsLeftOut = map[string]map[string]string{
	"users": {
		"email": "contact", "phone": "contact", "password_hash": "credential (read only as has_password)",
		"avatar_key": "unused", "avatar_focal_x": "unused", "avatar_focal_y": "unused",
		"administratively_disabled": "status carries it", "session_version": "session epoch",
		"logout_version": "session epoch", "timezone": "unused", "created_at": "unused", "updated_at": "unused",
		"birth_date":                "age (OccurrenceActor has none)",
		"hide_contact_from_leaders": "contact preference (OccurrenceActor has none)",
	},
	"teen_consents": {
		"id": "unused (consents are keyed by teen)", "scope": "filtered in SQL", "at": "ordered in SQL", "granted_by": "unused", "granter_role": "unused",
		"evidence_text": "private note",
	},
	"events": {
		"description": "unused text", "childcare_notes": "unused text", "childcare_ages": "unused text",
		"video_url": "a link shown only to registrants", "location": "place", "default_room_id": "place",
		"default_room_pending": "place", "leader_opened": "unused", "capacity": "unused", "waitlist_enabled": "unused",
		"guests_allowed": "unused", "reminder_push": "unused", "remote_available": "unused", "remote_capacity": "unused",
		"childcare_available": "unused", "childcare_capacity": "unused", "video_provider": "unused", "video_reveal": "unused",
		"starts_local": "unused (dates carry starts_at)", "duration_minutes": "unused (dates carry ends_at)", "rrule": "unused",
		"rrule_until": "unused", "series_clock_dirty": "unused", "edit_revision": "date edit token only",
		"created_by": "unused", "created_at": "unused", "updated_at": "unused",
	},
	"groups": {
		"name": "a hidden group's name", "location": "place", "description": "unused text", "tags": "unused text",
		"image_url": "unused", "cover_focal_x": "unused", "cover_focal_y": "unused", "leader_id": "display only",
		"member_limit": "unused", "deletion_requested": "unused", "edit_revision": "group form token only",
		"created_at": "unused", "updated_at": "unused",
	},
	"memberships": {"joined_at": "unused", "tenure": "unused"},
	"event_occurrences": {
		"location_override": "place", "starts_at_override": "unused (starts_at is the date's start)",
		"duration_override": "unused (ends_at)", "capacity_override": "unused", "remote_capacity_override": "unused",
		"cancel_source": "unused", "edit_revision": "date edit token only", "edit_receipt": "edit bookkeeping",
		"created_at": "unused", "updated_at": "unused",
	},
	"registrations": {
		"household_person_id": "a household person (holderOf reads registered_by)", "access_ended_by": "audit only",
		"childcare_requested": "unused", "attendance_mode": "unused", "source": "unused",
		"manage_token_hash": "credential", "guest_count": "unused", "childcare_count": "unused",
		"childcare_ages_note": "private note", "waitlisted_at": "unused", "promoted_at": "unused",
		"removed_by": "unused", "removal_note": "private note", "created_at": "unused", "updated_at": "unused",
	},
	"guardian_links": {
		"id": "unused (copyRule pairs by guardian and teen)", "status": "filtered in SQL", "person_id": "unused", "confirmed_by": "unused", "check_note": "private note",
		"created_at": "unused", "updated_at": "unused", "decided_at": "unused",
	},
}

// factSecretColumns may never be selected, whatever the lists say.
var factSecretColumns = []string{"password_hash", "manage_token_hash", "email", "phone", "evidence_text", "check_note",
	"childcare_ages_note", "removal_note", "childcare_notes", "video_url", "description", "session_version", "logout_version"}

// factNeverRead are columns of one table the statement may never read, whatever
// the lists say: places and a group's name, which belong to events and groups
// the reader may not see, the household person a registration is for, and a
// person's age and contact preference, which no message decision reads (a
// loaded row's missing birth date must never be mistaken for an adult's).
var factNeverRead = map[string][]string{
	"users":             {"birth_date", "hide_contact_from_leaders"},
	"events":            {"location", "default_room_id", "default_room_pending"},
	"groups":            {"name", "location"},
	"event_occurrences": {"location_override"},
	"registrations":     {"household_person_id"},
}

// Every column of every model the statement decodes is either read, with a
// named reader (factColumnReaders), or left out with a reason, never both;
// the credential, contact, note, place and group-name columns are left out.
func TestFactColumnsCoverTheModels(t *testing.T) {
	for _, model := range []any{&models.User{}, &models.TeenConsent{}, &models.Event{}, &models.Group{}, &models.Membership{},
		&models.EventOccurrence{}, &models.Registration{}, &models.GuardianLink{}} {
		sch, err := schema.Parse(model, &factSchemas, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		read, out, readers := factColumns[sch.Table], factColumnsLeftOut[sch.Table], factColumnReaders[sch.Table]
		for _, c := range sch.DBNames {
			in := slices.Contains(read, c)
			_, left := out[c]
			switch {
			case in && left:
				t.Errorf("%s.%s is both read and left out", sch.Table, c)
			case !in && !left:
				t.Errorf("%s.%s is neither read nor left out with a reason", sch.Table, c)
			case in && slices.Contains(factSecretColumns, c):
				t.Errorf("%s.%s is a secret column the statement reads", sch.Table, c)
			case in && slices.Contains(factNeverRead[sch.Table], c):
				t.Errorf("%s.%s is in factNeverRead, which the statement never reads", sch.Table, c)
			}
		}
		for _, c := range read {
			if !slices.Contains(sch.DBNames, c) {
				t.Errorf("%s.%s is read but is not a model column", sch.Table, c)
			}
			if readers[c] == "" {
				t.Errorf("%s.%s is read but names no reader in factColumnReaders", sch.Table, c)
			}
		}
		for c := range readers {
			if !slices.Contains(read, c) {
				t.Errorf("%s.%s names a reader but is not in factColumns", sch.Table, c)
			}
		}
	}
}

// The statement selects named columns only (no "*"), names no secret column
// but for has_password's presence test, and binds each id list as one array.
func TestDateFactsStatementSelectsNoSecrets(t *testing.T) {
	if strings.Contains(dateFactsSQL, "*") {
		t.Error("the facts statement selects a whole row")
	}
	bare := strings.ReplaceAll(dateFactsSQL, "u.password_hash IS NOT NULL AS has_password", "")
	for _, c := range factSecretColumns {
		if regexp.MustCompile(`\b` + c + `\b`).MatchString(bare) {
			t.Errorf("the facts statement names %s", c)
		}
	}
	if regexp.MustCompile(`(?i)\bIN\s+@(occurrences|events|holders|groups|people|viewers|series|teens)\b`).MatchString(dateFactsSQL) {
		t.Error("an id list is expanded into one parameter per id")
	}
}

// What the statement returns holds none of the credentials, contact details,
// notes, places or group names of the people, registrations, links, consents,
// events and groups it reads, and each table's rows carry exactly its
// factColumns, while their decisions still come through: a person with a
// password can sign in, one without cannot.
func TestDateFactsStatementReturnsNoSecrets(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	db := f.app.DB
	const mark = "SECRET-SENTINEL"
	gormMust(t, db.Model(f.recipient).Updates(map[string]any{"phone": mark + "-phone", "email": "secret-sentinel@example.com",
		"birth_date": "1987-06-05", "hide_contact_from_leaders": true}))
	gormMust(t, db.Model(f.other).Updates(map[string]any{"account_type": models.AccountTeen, "password_hash": nil}))
	gormMust(t, db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", mark+"-hash", f.issuer.ID))
	gormMust(t, db.Model(&models.Registration{}).Where("user_id = ?", f.recipient.ID).Updates(map[string]any{
		"manage_token_hash": mark + "-token", "childcare_ages_note": mark + "-ages", "removal_note": mark + "-removal"}))
	gormMust(t, db.Model(&models.Event{}).Where("id = ?", f.event.ID).Updates(map[string]any{
		"description": mark + "-description", "video_url": "https://video.example/" + mark, "childcare_notes": mark + "-childcare"}))
	gormMust(t, db.Model(&models.Group{}).Where("id = ?", f.group).Updates(map[string]any{"description": mark + "-group", "tags": mark + "-tags",
		"location": mark + "-group-place"}))
	gormMust(t, db.Model(&models.Event{}).Where("id = ?", f.event.ID).Update("location", mark+"-event-place"))
	gormMust(t, db.Model(&models.EventOccurrence{}).Where("event_id = ?", f.event.ID).Update("location_override", mark+"-date-place"))
	gormMust(t, db.Create(&models.TeenConsent{TeenUserID: f.other.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted,
		GrantedBy: f.issuer.ID, GranterRole: "admin", EvidenceText: mark + "-evidence"}))
	gormMust(t, db.Create(&models.GuardianLink{GuardianUserID: f.issuer.ID, TeenUserID: &f.other.ID, Status: models.GuardianConfirmed, CheckNote: mark + "-check"}))
	people := []uint{f.issuer.ID, f.recipient.ID, f.other.ID}
	q := dateFactsQuery{People: people, Viewers: people, Holders: people, EventIDs: []uint{f.event.ID}, GroupIDs: []uint{f.group},
		Teens: []uint{f.other.ID}, SeriesTitles: []uint{f.event.ID}, Settings: []string{settingTimezone}}
	rows, err := db.Raw(dateFactsSQL, dateFactsArgs(q)).Rows()
	if err != nil {
		t.Fatal(err)
	}
	cols := make([][]byte, 12)
	ptrs := make([]any, len(cols))
	for i := range cols {
		ptrs[i] = &cols[i]
	}
	if !rows.Next() {
		t.Fatal("no row")
	}
	gormMust(t, &gorm.DB{Error: rows.Scan(ptrs...)})
	gormMust(t, &gorm.DB{Error: rows.Close()})
	all := strings.ToLower(string(slices.Concat(cols...)))
	if strings.Contains(all, strings.ToLower(mark)) || strings.Contains(all, "@example.com") || strings.Contains(all, "$2a$") {
		t.Fatalf("the statement returned a secret: %s", all)
	}
	if strings.Contains(all, "retained hidden group") || strings.Contains(all, "retained group place") {
		t.Errorf("the statement returned the group's name or place: %s", all)
	}
	if strings.Contains(all, "1987-06-05") || strings.Contains(all, "birth_date") || strings.Contains(all, "hide_contact") {
		t.Errorf("the statement returned an age or a contact preference: %s", all)
	}
	for _, want := range []string{"departed recipient", "retained hidden series", "has_password"} {
		if !strings.Contains(all, want) {
			t.Errorf("the statement lost %q: %s", want, all)
		}
	}
	// Each table's rows carry its factColumns and nothing else, however the
	// statement selects them (cols follow loadDateFacts' scan order).
	tables := map[int]string{1: "users", 2: "teen_consents", 3: "events", 4: "groups", 5: "memberships", 7: "event_occurrences",
		8: "registrations", 10: "guardian_links"}
	extra := map[string][]string{"users": {"has_password"}, "registrations": {"fold_access_live"}}
	for i, table := range tables {
		var objs []map[string]json.RawMessage
		if err := json.Unmarshal(cols[i], &objs); err != nil || len(objs) == 0 {
			t.Fatalf("%s: no rows to check (%v): %s", table, err, cols[i])
		}
		want := slices.Concat(factColumns[table], extra[table])
		slices.Sort(want)
		for _, obj := range objs {
			got := slices.Sorted(maps.Keys(obj))
			if !slices.Equal(got, want) {
				t.Errorf("%s: the statement returned %v, want %v", table, got, want)
			}
		}
	}
	facts, err := f.app.loadDateFacts(db, q)
	if err != nil {
		t.Fatal(err)
	}
	if u := facts.liveUser(f.recipient.ID); u == nil || !u.CanSignIn() || u.Email != "" || u.Phone != "" || u.PasswordHash != factPasswordOnFile || u.CheckPassword("password1234") ||
		u.BirthDate != nil || u.HideContactFromLeaders {
		t.Errorf("the recipient as loaded: %+v", u)
	}
	if u := facts.users[f.other.ID]; u == nil || u.CanSignIn() {
		t.Errorf("a person with no password as loaded: %+v", u)
	}
	if len(facts.links) != 1 || facts.links[0].CheckNote != "" || facts.consents[f.other.ID].EvidenceText != "" || facts.consents[f.other.ID].Status != models.ConsentGranted {
		t.Errorf("links %+v, consents %+v", facts.links, facts.consents)
	}
}

// The statement binds the same parameters for one person as for thirty
// thousand: no id list grows the bind count toward PostgreSQL's 65,535.
func TestDateFactsBindsTheSameParametersForAnyCount(t *testing.T) {
	app := testApp(t)
	binds := func(n int) int {
		ids := make([]uint, n)
		for i := range ids {
			ids[i] = uint(i + 1)
		}
		q := dateFactsQuery{MessageID: 1, People: ids, Viewers: ids, Holders: ids, Teens: ids, EventIDs: ids, OccurrenceIDs: ids,
			GroupIDs: ids, SeriesTitles: ids, Settings: []string{settingTimezone, settingGuardianCopies}}
		var out []map[string]any
		stmt := app.DB.Session(&gorm.Session{DryRun: true}).Raw(dateFactsSQL, dateFactsArgs(q)).Find(&out).Statement
		return len(stmt.Vars)
	}
	one, many := binds(1), binds(30000)
	if one != many || one < 9 || many > 20 {
		t.Fatalf("binds: %d for one id, %d for 30,000", one, many)
	}
}

// factsAccountProblems names each function that reads the facts statement's
// rows (a dateFactSource, memDates, dateFacts or copyRule receiver or
// parameter, or a call of
// loadDateFacts) and builds a full account from them. Those rows carry no
// birth date, and policy.ContactAccount reads a missing one as an adult's, so
// such code judges people as policy.OccurrenceActor (occurrenceActorOf).
func factsAccountProblems(parsed *guardkit.Parsed) []string {
	problems := slices.Clone(parsed.Problems)
	factsType := func(e ast.Expr) bool {
		if star, ok := e.(*ast.StarExpr); ok {
			e = star.X
		}
		id, ok := e.(*ast.Ident)
		return ok && (id.Name == "dateFactSource" || id.Name == "memDates" || id.Name == "dateFacts" || id.Name == "copyRule")
	}
	for _, f := range parsed.Package("main") {
		for _, decl := range f.AST.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			reads := false
			for _, list := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
				if list == nil {
					continue
				}
				for _, field := range list.List {
					reads = reads || factsType(field.Type)
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && strings.HasSuffix(guardkit.Callee(call), "loadDateFacts") {
					reads = true
				}
				return true
			})
			if !reads {
				continue
			}
			builds := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					switch id.Name {
					case "contactAccount", "ContactAccount", "occurrenceAccountsIn", "occurrenceAccountsInZone", "occurrenceAccountIn",
						"occurrenceAccountInZone":
						builds = true
					}
				}
				return !builds
			})
			if builds {
				problems = append(problems, f.Path+": "+fn.Name.Name+" builds a ContactAccount from the facts statement's rows")
			}
		}
	}
	return problems
}

func TestFactsStatementRowsNeverBecomeAContactAccount(t *testing.T) {
	for _, p := range factsAccountProblems(guardkit.Production(t).Parse(0)) {
		t.Error(p)
	}
}

// The guard catches the mistake it exists for.
func TestFactsAccountGuardCatchesAContactAccount(t *testing.T) {
	bad := guardkit.Sources{"bad.go": `package main
func (m memDates) adult(id uint) bool { return contactAccount(m.f.users[id], now, zone).Active }
func (a *App) other(tx *gorm.DB) { f, _ := a.loadDateFacts(tx, q); _ = policy.ContactAccount{Person: asPerson(f.users[1])} }
func (a *App) values(src dateFactSource, u *models.User) bool { return policy.CanSearchPeople(contactAccount(u, now, zone), false, false) }
func fine(u *models.User) policy.ContactAccount { return contactAccount(u, now, zone) }
`}
	if got := factsAccountProblems(bad.Parse(0)); len(got) != 3 {
		t.Fatalf("problems %v, want the three facts readers", got)
	}
}
