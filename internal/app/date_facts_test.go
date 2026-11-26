package main

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// normalizeTimes puts every time in v (a pointer to a struct) in UTC, so two
// loads of one row compare equal whatever zone the driver used.
func normalizeTimes(v reflect.Value) {
	v = reflect.Indirect(v)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Type() {
		case timeType:
			f.Set(reflect.ValueOf(f.Interface().(time.Time).UTC()))
		case reflect.PointerTo(timeType):
			if !f.IsNil() {
				t := f.Elem().Interface().(time.Time).UTC()
				f.Set(reflect.ValueOf(&t))
			}
		case deletedAtType:
			d := f.Interface().(gorm.DeletedAt)
			d.Time = d.Time.UTC()
			f.Set(reflect.ValueOf(d))
		default:
			if f.Kind() == reflect.Struct {
				normalizeTimes(f.Addr())
			}
		}
	}
}

// decodeMatchesFind requires decodeFactRows, over the table's factColumns as
// the statement selects them, to give what gorm scans from the same rows with
// every other field left zero.
func decodeMatchesFind[T any](t *testing.T, db *gorm.DB, where string, args ...any) {
	t.Helper()
	sch, err := schema.Parse(new(T), &factSchemas, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	table := sch.Table
	var want []T
	if err := db.Unscoped().Where(where, args...).Order("id").Find(&want).Error; err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatalf("%s: no fixture rows", table)
	}
	var raw []byte
	if err := db.Raw("SELECT COALESCE(jsonb_agg(to_jsonb(x) - 'fold_order' ORDER BY x.fold_order), '[]') FROM (SELECT "+factCols(table, "y")+", y.id AS fold_order FROM "+table+" y WHERE "+where+") x", args...).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	got, err := decodeFactRows[T](raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: decoded %d rows, gorm found %d", table, len(got), len(want))
	}
	read := idSetOf(factColumns[table])
	for i := range got {
		w := reflect.ValueOf(&want[i]).Elem()
		for _, field := range sch.Fields {
			if field.DBName != "" && !read[field.DBName] {
				fv := w.FieldByIndex(field.StructField.Index)
				fv.Set(reflect.Zero(fv.Type()))
			}
		}
		normalizeTimes(reflect.ValueOf(&got[i]))
		normalizeTimes(reflect.ValueOf(&want[i]))
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s row %d:\n decoded %+v\n gorm    %+v", table, i, got[i], want[i])
		}
	}
}

func idSetOf(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// The facts statement's columns decode to exactly what gorm scans from the
// same rows (NULLs, dates, timestamps and soft deletes included), and no
// field the statement does not read is set.
func TestDateFactsDecodeLikeGorm(t *testing.T) {
	f := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	db := f.app.DB
	birth := time.Date(2010, 3, 4, 0, 0, 0, 0, time.UTC)
	if err := db.Model(f.other).Updates(map[string]any{"birth_date": birth, "account_type": models.AccountTeen, "deleted_at": time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE event_occurrences SET visibility_override = 'public', starts_at_override = now() WHERE id = ?", f.dates[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE events SET rrule_until = '2030-01-02' WHERE id = ?", f.event.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.TeenConsent{TeenUserID: f.other.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: f.issuer.ID, GranterRole: "admin", EvidenceText: "Checked with a parent"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.GuardianLink{GuardianUserID: f.issuer.ID, TeenUserID: &f.other.ID, Status: models.GuardianConfirmed}).Error; err != nil {
		t.Fatal(err)
	}
	decodeMatchesFind[models.User](t, db, "id IN ?", []uint{f.issuer.ID, f.recipient.ID, f.other.ID})
	decodeMatchesFind[models.Event](t, db, "id = ?", f.event.ID)
	decodeMatchesFind[models.Group](t, db, "id = ?", f.group)
	decodeMatchesFind[models.EventOccurrence](t, db, "event_id = ?", f.event.ID)
	decodeMatchesFind[models.Registration](t, db, "user_id = ?", f.recipient.ID)
	decodeMatchesFind[models.Membership](t, db, "group_id = ?", f.group)
	decodeMatchesFind[models.TeenConsent](t, db, "teen_user_id = ?", f.other.ID)
	decodeMatchesFind[models.GuardianLink](t, db, "teen_user_id = ?", f.other.ID)

	// A listed column the row lacks stops the decode.
	if _, err := decodeFactRows[models.Membership]([]byte(`[{"id": 1}]`)); err == nil {
		t.Error("a row missing columns decoded")
	}
}

// fillUnreadFactColumns gives every person, event, group and date non-default
// values in the left-out columns a decision could plausibly read
// (factColumnsLeftOut): an age under 18 and a hidden-contact preference,
// places, a group's name, capacities, the remote, childcare, video, waitlist,
// guest and reminder-push settings, durations, the start override, the series
// end, the clock flag and the cancel source. The query-per-answer source reads
// them on full rows, so a parity check after this fails if a decision or merge
// field read one (an exact-date grant that read age would refuse a known
// minor where the statement's rows, with no birth date, read an adult). The
// room columns, which need a room row, are kept out by factNeverRead instead.
func fillUnreadFactColumns(t *testing.T, db *gorm.DB) {
	t.Helper()
	gormMust(t, db.Exec(`UPDATE users SET birth_date = CURRENT_DATE - interval '15 years', hide_contact_from_leaders = true`))
	gormMust(t, db.Exec(`UPDATE events SET location = 'Unread event place', capacity = 7, waitlist_enabled = NOT waitlist_enabled,
		guests_allowed = guests_allowed + 2, reminder_push = NOT reminder_push, remote_available = true, remote_capacity = 3,
		childcare_available = true, childcare_capacity = 2, video_provider = 'jitsi', video_reveal = 'registered_only',
		duration_minutes = duration_minutes + 17, rrule_until = DATE '2031-01-01', series_clock_dirty = true,
		leader_opened = (group_id IS NOT NULL)`))
	gormMust(t, db.Exec(`UPDATE groups SET name = name || ' (unread name)', location = 'Unread group place'`))
	gormMust(t, db.Exec(`UPDATE event_occurrences SET location_override = 'Unread date place', duration_override = 23,
		capacity_override = 5, remote_capacity_override = 2, starts_at_override = starts_at + interval '1 hour',
		cancel_source = CASE WHEN status = 'scheduled' THEN 'manual' ELSE cancel_source END`))
}

// paritySubject is one (reader, audience) the two fact sources must agree on.
type paritySubject struct {
	label  string
	reader *models.User
	au     audienceJSON
	// reminder, when set, is a reminder's own date.
	reminder *uint
}

// checkDateFactsParity judges each case with the query-per-answer source and
// the one-statement source in one quiescent snapshot, and requires every
// answer to match: who the audience still reaches, who may see the
// reminder's date, and the reader's whole merge map.
func checkDateFactsParity(t *testing.T, app *App, cases ...paritySubject) {
	t.Helper()
	now := time.Now().UTC()
	for _, c := range cases {
		err := app.DB.Transaction(func(tx *gorm.DB) error {
			m := &models.Message{ID: 0, OccurrenceID: c.reminder}
			q := rowFactsQuery(m, c.au, models.MessageRecipient{UserID: c.reader.ID})
			facts, err := app.loadDateFacts(tx, q)
			if err != nil {
				return err
			}
			mem, db := facts.source(app), app.dbDates(tx)
			for _, since := range []time.Time{now, now.Add(-30 * 24 * time.Hour)} {
				want, werr := audienceSeersFrom(db, c.au, []uint{c.reader.ID}, since, now)
				got, gerr := audienceSeersFrom(mem, c.au, []uint{c.reader.ID}, since, now)
				if !errors.Is(gerr, werr) && fmt.Sprint(gerr) != fmt.Sprint(werr) || !maps.Equal(got, want) {
					t.Errorf("%s: seers since %s: memDates %v %v, dbDates %v %v", c.label, since, got, gerr, want, werr)
				}
			}
			if c.reminder != nil {
				want, werr := reminderReadersFrom(db, *c.reminder, []uint{c.reader.ID}, now)
				got, gerr := reminderReadersFrom(mem, *c.reminder, []uint{c.reader.ID}, now)
				if !errors.Is(gerr, werr) || !maps.Equal(got, want) {
					t.Errorf("%s: reminder readers: memDates %v %v, dbDates %v %v", c.label, got, gerr, want, werr)
				}
			}
			// Each source judges its own row of the reader, as production
			// does: the full row for the queries, the statement's for memDates.
			reader := facts.liveUser(c.reader.ID)
			if reader == nil {
				return nil
			}
			var full models.User
			if err := tx.First(&full, c.reader.ID).Error; err != nil {
				return err
			}
			want, werr := app.readerEventValuesFrom(db, &full, c.au, now)
			got, gerr := app.readerEventValuesFrom(mem, reader, c.au, now)
			if werr != nil || gerr != nil || !maps.Equal(got, want) {
				t.Errorf("%s: values:\n memDates %v %v\n dbDates  %v %v", c.label, got, gerr, want, werr)
			}
			return nil
		}, readSnapshot)
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
	}
}

func readVisCases(f readVisFixture, label string, reader *models.User) []paritySubject {
	occ0 := f.occs[0].ID
	return []paritySubject{
		{label + ": the first date", reader, f.occAudience(f.occs[0]), &occ0},
		{label + ": the second date", reader, f.occAudience(f.occs[1]), &f.occs[1].ID},
		{label + ": the series", reader, f.seriesAudience(), nil},
		{label + ": several events", reader, audienceJSON{Type: policy.AudienceEvents, EventIDs: []uint{f.ev.ID}}, nil},
		{label + ": a waitlist audience", reader, audienceJSON{Type: policy.AudienceEventSeries, EventID: f.ev.ID, Statuses: []string{models.RegWaitlisted}}, nil},
	}
}

// The one-statement source answers every date question as the separate
// queries do, across ordinary visibility, overrides, titles, cancellation,
// membership, deletion, guests and ties.
func TestDateFactsMatchTheQueries(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	f.register(t, f.reader, f.occs[0])
	f.register(t, f.reader, f.occs[2])
	fillUnreadFactColumns(t, f.app.DB)
	steps := []struct {
		label string
		apply func()
	}{
		{"public", func() {}},
		{"renamed", func() { f.rename(t, "Renamed night") }},
		{"a closed first date", func() { f.setDate(t, f.occs[0], "visibility_override", models.VisibilityInviteOnly) }},
		{"an empty date title", func() {
			f.setDate(t, f.occs[0], "visibility_override", nil)
			f.setDate(t, f.occs[0], "title_override", "")
		}},
		{"a titled date", func() { f.setDate(t, f.occs[0], "title_override", "Special night") }},
		{"a cancelled date", func() { f.setDate(t, f.occs[0], "status", models.OccurrenceCancelled) }},
		{"a tied start", func() {
			f.setDate(t, f.occs[0], "status", models.OccurrenceScheduled)
			f.setDate(t, f.occs[2], "starts_at", f.occs[0].StartsAt)
			f.setDate(t, f.occs[2], "visibility_override", models.VisibilityInviteOnly)
		}},
		{"an invite-only group with an opened date", func() {
			if err := f.app.DB.Model(&models.Group{}).Where("id = ?", f.group.ID).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.app.DB.Model(&models.Event{}).Where("id = ?", f.ev.ID).Update("visibility", models.VisibilityInviteOnly).Error; err != nil {
				t.Fatal(err)
			}
			f.setDate(t, f.occs[1], "visibility_override", models.VisibilityPublic)
		}},
		{"a member of the hidden group", func() { addMember(t, f.app, f.reader, &f.group, models.MembershipMember) }},
		{"a member who left", func() { f.setMembership(t, f.reader, models.MembershipLeft) }},
		{"a guest", func() {
			if err := f.app.DB.Model(f.reader).Update("status", models.UserGuest).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"a deleted event", func() {
			if err := f.app.DB.Model(f.reader).Update("status", models.UserActive).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.app.DB.Exec("UPDATE events SET deleted_at = now() WHERE id = ?", f.ev.ID).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"a deleted group", func() {
			if err := f.app.DB.Exec("UPDATE events SET deleted_at = NULL WHERE id = ?", f.ev.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.app.DB.Exec("UPDATE groups SET deleted_at = now() WHERE id = ?", f.group.ID).Error; err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, s := range steps {
		s.apply()
		checkDateFactsParity(t, f.app, readVisCases(f, s.label, f.reader)...)
		checkDateFactsParity(t, f.app, readVisCases(f, s.label+", the leader", f.leaderU)...)
		checkDateFactsParity(t, f.app, readVisCases(f, s.label+", an admin", f.adminU)...)
	}
}

// Exact-date grants, a kept departure date, ended grants, teen consent and a
// household holding agree too.
func TestDateFactsMatchTheQueriesForGrantsAndHouseholds(t *testing.T) {
	f := newOccurrenceReadFixture(t)
	app := f.app
	fillUnreadFactColumns(t, app.DB)
	cases := func(label string) []paritySubject {
		return []paritySubject{
			{label + ": the invited date", f.recipient, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: f.dates[1].ID}, &f.dates[1].ID},
			{label + ": another date", f.recipient, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: f.dates[0].ID}, &f.dates[0].ID},
			{label + ": the series", f.recipient, audienceJSON{Type: policy.AudienceEventSeries, EventID: f.event.ID}, nil},
			{label + ": several events", f.recipient, audienceJSON{Type: policy.AudienceEvents, EventIDs: []uint{f.event.ID}}, nil},
		}
	}
	checkDateFactsParity(t, app, cases("titled grant")...)
	if err := app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Update("title_override", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Create(&models.Registration{OccurrenceID: f.dates[0].ID, UserID: &f.recipient.ID, Status: models.RegGoing}).Error; err != nil {
		t.Fatal(err)
	}
	checkDateFactsParity(t, app, cases("untitled grant")...)
	if err := app.DB.Model(f.recipient).Update("account_type", models.AccountTeen).Error; err != nil {
		t.Fatal(err)
	}
	checkDateFactsParity(t, app, cases("a teen without consent")...)
	if err := app.DB.Create(&models.TeenConsent{TeenUserID: f.recipient.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: f.issuer.ID, GranterRole: "admin", EvidenceText: "Checked with a parent"}).Error; err != nil {
		t.Fatal(err)
	}
	checkDateFactsParity(t, app, cases("a teen with consent")...)
	if err := app.DB.Create(&models.TeenConsent{TeenUserID: f.recipient.ID, Scope: models.TeenConsentAccount, Status: models.ConsentRevoked, GrantedBy: f.issuer.ID, GranterRole: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	checkDateFactsParity(t, app, cases("a teen whose consent was granted, then revoked")...)
	if err := app.DB.Create(&models.TeenConsent{TeenUserID: f.recipient.ID, Scope: models.TeenConsentAccount, Status: models.ConsentGranted, GrantedBy: f.issuer.ID, GranterRole: "admin", EvidenceText: "Checked again"}).Error; err != nil {
		t.Fatal(err)
	}
	checkDateFactsParity(t, app, cases("a teen whose consent was granted again")...)
	// A household person's registration puts its holder in the audience; it
	// lends the holder no grant of the person's.
	household := models.Household{Name: "Other family", CreatedBy: f.other.ID}
	gormMust(t, app.DB.Create(&household))
	person := models.HouseholdPerson{HouseholdID: household.ID, FirstName: "Kid", Kind: models.HouseholdChild, CreatedBy: f.other.ID}
	gormMust(t, app.DB.Create(&person))
	if err := app.DB.Create(&models.Registration{OccurrenceID: f.dates[1].ID, HouseholdPersonID: &person.ID, RegisteredBy: &f.other.ID, Status: models.RegGoing}).Error; err != nil {
		t.Fatal(err)
	}
	checkDateFactsParity(t, app,
		paritySubject{"household holder: the date", f.other, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: f.dates[1].ID}, &f.dates[1].ID},
		paritySubject{"household holder: the series", f.other, audienceJSON{Type: policy.AudienceEventSeries, EventID: f.event.ID}, nil})
	endSeededAccess(t, app.DB, f.access.RegistrationID, policy.OccurrenceDeclined, &f.recipient.ID, time.Now().UTC())
	checkDateFactsParity(t, app, cases("an ended grant")...)
	if err := app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.dates[1].ID).Update("status", models.OccurrenceCancelled).Error; err != nil {
		t.Fatal(err)
	}
	checkDateFactsParity(t, app, cases("a cancelled date")...)
	missing := f.dates[2].ID + 1000
	checkDateFactsParity(t, app, paritySubject{"a missing date", f.recipient, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: missing}, &missing})

	d := newDepartureReadFixture(t, models.VisibilityInviteOnly, models.RegGoing, policy.OccurrenceOriginDepartureSelf)
	fillUnreadFactColumns(t, d.app.DB)
	departed := []paritySubject{
		{"departure: the kept date", d.recipient, audienceJSON{Type: policy.AudienceOccurrence, OccurrenceID: d.dates[1].ID}, &d.dates[1].ID},
		{"departure: the series", d.recipient, audienceJSON{Type: policy.AudienceEventSeries, EventID: d.event.ID}, nil},
	}
	checkDateFactsParity(t, d.app, departed...)
	endSeededAccess(t, d.app.DB, d.access.RegistrationID, policy.OccurrenceDeclined, &d.recipient.ID, time.Now().UTC())
	checkDateFactsParity(t, d.app, departed...)
}

// A hidden series' title probe finds a valid grant behind many invalid ones,
// as the paged query does.
func TestDateFactsMatchTheQueriesBehindManyGrants(t *testing.T) {
	app := testApp(t)
	issuer := newTestUser(t, app, "issuer", "password1234", models.RoleAdmin)
	reader := newTestUser(t, app, "reader", "password1234", models.RoleMember)
	admin := loginAPI(t, app, issuer.Name, "password1234")
	group := createFeedGroup(t, admin, "Hidden group many", models.VisibilityInviteOnly, "")
	ev := createFeedEvent(t, admin, group, "Hidden series many", feedStart(7), "FREQ=DAILY;COUNT=55")
	var dates []models.EventOccurrence
	if err := app.DB.Where("event_id = ?", ev.ID).Order("id").Find(&dates).Error; err != nil || len(dates) != 55 {
		t.Fatal(err, len(dates))
	}
	rows := make([]accessRow, 0, len(dates))
	for _, d := range dates {
		rows = append(rows, seedReadInvitation(t, app, issuer, reader, ev, d))
	}
	for _, r := range rows[:len(rows)-1] {
		if err := app.DB.Model(&models.Registration{}).Where("id = ?", r.RegistrationID).Update("status", models.RegCancelled).Error; err != nil {
			t.Fatal(err)
		}
	}
	fillUnreadFactColumns(t, app.DB)
	series := audienceJSON{Type: policy.AudienceEventSeries, EventID: ev.ID}
	checkDateFactsParity(t, app, paritySubject{"the last grant is valid", reader, series, nil})
	endSeededAccess(t, app.DB, rows[len(rows)-1].RegistrationID, policy.OccurrenceDeclined, &reader.ID, time.Now().UTC())
	checkDateFactsParity(t, app, paritySubject{"no valid grant", reader, series, nil})
}

// A question past what the statement loaded is an error, never "may not see".
func TestDateFactsRefuseQuestionsTheyCannotAnswer(t *testing.T) {
	f := newReadVisFixture(t, models.VisibilityPublic)
	facts, err := f.app.loadDateFacts(f.app.DB, rowFactsQuery(&models.Message{}, f.occAudience(f.occs[0]), models.MessageRecipient{UserID: f.reader.ID}))
	if err != nil {
		t.Fatal(err)
	}
	mem := facts.source(f.app)
	now := time.Now().UTC()
	checks := map[string]error{}
	_, checks["another holder"] = mem.holdings(f.occAudience(f.occs[0]), nil, []uint{f.leaderU.ID}, defaultRegStatuses, now)
	_, checks["another date"] = mem.occurrences([]uint{f.occs[1].ID + 1000})
	_, checks["another person"] = mem.liveUsers([]uint{f.leaderU.ID})
	_, checks["another viewer"] = mem.candidates(f.leaderU, f.ev.ID)
	_, checks["an unloaded series probe"] = mem.untitledOpenDate(f.ev.ID, []string{policy.Public})
	_, _, checks["another reader's date"] = mem.registeredDate(f.ev.ID, f.occAudience(f.occs[0]), f.leaderU.ID, now)
	for label, err := range checks {
		if !errors.Is(err, errDateFactsGap) {
			t.Errorf("%s: %v, want errDateFactsGap", label, err)
		}
	}
	if !slices.Contains(facts.q.Settings, settingTimezone) {
		t.Error("the row query does not read the church's zone")
	}
}

// Both fact sources answer liveEvents in the order the ids were asked, each
// once and without deleted events, whatever order the database returns rows
// in, so an audience's targets come out the same from either.
func TestLiveEventsKeepTheAskedOrder(t *testing.T) {
	f := newComposeFixture(t)
	gormMust(t, f.app.DB.Delete(&models.Event{}, f.otherCW.ID))
	group, church, deleted := f.groupEvent.ID, f.churchEvent.ID, f.otherCW.ID
	asks := []struct{ ask, want []uint }{
		{[]uint{group, church}, []uint{group, church}},
		{[]uint{church, group}, []uint{church, group}},
		{[]uint{church, deleted, group, church}, []uint{church, group}},
		{[]uint{church, deleted + 1000000, group, church}, []uint{church, group}},
	}
	gormMust(t, &gorm.DB{Error: f.app.DB.Transaction(func(tx *gorm.DB) error {
		facts, err := f.app.loadDateFacts(tx, dateFactsQuery{EventIDs: []uint{group, church, deleted, deleted + 1000000}})
		if err != nil {
			return err
		}
		sources := map[string]dateFactSource{"dbDates": f.app.dbDates(tx), "memDates": facts.source(f.app)}
		queries := countDBReads(t, tx)
		defer queries.Stop()
		for name, src := range sources {
			for _, c := range asks {
				before := queries.Reads()
				evs, err := src.liveEvents(c.ask)
				if err != nil {
					return err
				}
				if name == "dbDates" && queries.Reads()-before != 1 {
					t.Errorf("dbDates.liveEvents(%v) ran %d reads, want 1", c.ask, queries.Reads()-before)
				}
				got := []uint{}
				for _, ev := range evs {
					got = append(got, ev.ID)
				}
				if !slices.Equal(got, c.want) {
					t.Errorf("%s.liveEvents(%v) = %v, want %v", name, c.ask, got, c.want)
				}
			}
		}
		return nil
	}, readSnapshot)})
}

// Try again's answers from the one statement (audienceMembersAmong,
// audienceTargetsFrom) match the queries message.send asks: who the author's
// audience reaches today among the people asked about, and the author's
// standing toward its targets, for every kind of audience.
func TestRetryFactsMatchTheAudienceQueries(t *testing.T) {
	f := newComposeFixture(t)
	gormMust(t, f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &f.teen.ID, Status: models.RegGoing}))
	gormMust(t, f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &f.opted.ID, Status: models.RegWaitlisted}))
	gormMust(t, f.app.DB.Model(&models.Membership{}).Where("user_id = ?", f.bouncing.ID).Update("status", models.MembershipLeft))
	// An outsider going to a date closed to them, and a disabled account.
	outsider := newTestUser(t, f.app, "closed out", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Create(&models.Registration{OccurrenceID: f.groupOcc.ID, UserID: &outsider.ID, Status: models.RegGoing}))
	gormMust(t, f.app.DB.Model(&models.EventOccurrence{}).Where("id = ?", f.groupOcc.ID).Update("visibility_override", models.VisibilityInviteOnly))
	disabled := newTestUser(t, f.app, "disabled one", "password1234", models.RoleMember)
	gormMust(t, f.app.DB.Model(disabled).Update("status", models.UserDisabled))
	people := []uint{f.teen.ID, f.opted.ID, f.bouncing.ID, f.hostUser.ID, f.leaderUser.ID, outsider.ID, disabled.ID}
	fillUnreadFactColumns(t, f.app.DB)
	audiences := []audienceJSON{
		{Type: policy.AudienceGroup, GroupID: f.group.ID},
		{Type: policy.AudienceGroup, GroupID: f.group.ID, Roles: []string{models.MembershipLeader}},
		{Type: policy.AudienceAllMembers},
		{Type: policy.AudienceUsers, UserIDs: []uint{f.teen.ID, f.hostUser.ID}},
		{Type: policy.AudienceOccurrence, OccurrenceID: f.groupOcc.ID},
		{Type: policy.AudienceOccurrence, OccurrenceID: f.groupOcc.ID, Statuses: []string{models.RegWaitlisted}},
		{Type: policy.AudienceEventSeries, EventID: f.groupEvent.ID},
		{Type: policy.AudienceEvents, EventIDs: []uint{f.groupEvent.ID, f.churchEvent.ID}},
		{Type: policy.AudienceEvents, EventIDs: []uint{f.churchEvent.ID, f.groupEvent.ID}},
	}
	now := time.Now().UTC()
	var admin models.User
	gormMust(t, f.app.DB.Where("role = ?", models.RoleAdmin).First(&admin))
	for _, author := range []*models.User{f.leaderUser, f.hostUser, &admin} {
		for _, au := range audiences {
			label := fmt.Sprintf("%s to %+v", author.Name, au)
			gormMust(t, &gorm.DB{Error: f.app.DB.Transaction(func(tx *gorm.DB) error {
				var normalized = au
				if err := normalized.normalize(); err != nil {
					return err
				}
				// message.send judges the author's full row as it stands now;
				// Try again judges the statement's row of them.
				var full models.User
				if err := tx.First(&full, author.ID).Error; err != nil {
					return err
				}
				wantTargets, werr := f.app.audienceTargets(tx, &full, normalized)
				ids, ierr := f.app.resolveAudienceIn(tx, &full, &au, now)
				q := dateFactsQuery{People: append([]uint{author.ID}, people...), Viewers: append([]uint{author.ID}, people...), Holders: people,
					Settings: []string{settingTimezone}}
				switch normalized.Type {
				case policy.AudienceOccurrence:
					q.OccurrenceIDs = []uint{normalized.OccurrenceID}
				case policy.AudienceEventSeries:
					q.EventIDs = []uint{normalized.EventID}
				case policy.AudienceEvents:
					q.EventIDs = normalized.EventIDs
				case policy.AudienceGroup:
					q.GroupIDs = []uint{normalized.GroupID}
				}
				facts, err := f.app.loadDateFacts(tx, q)
				if err != nil {
					return err
				}
				mem := facts.source(f.app)
				gotTargets, gerr := audienceTargetsFrom(mem, facts.liveUser(author.ID), normalized)
				if !errors.Is(gerr, werr) || !reflect.DeepEqual(gotTargets, wantTargets) {
					t.Errorf("%s: targets %v %v, want %v %v", label, gotTargets, gerr, wantTargets, werr)
				}
				if ierr != nil {
					return nil // refused: Try again refuses before asking who
				}
				got, err := audienceMembersAmong(mem, normalized, people, now)
				if err != nil {
					return err
				}
				for _, id := range people {
					if got[id] != slices.Contains(ids, id) {
						t.Errorf("%s: person %d: memDates %v, message.send %v", label, id, got[id], slices.Contains(ids, id))
					}
				}
				return nil
			}, readSnapshot)})
		}
	}
}
