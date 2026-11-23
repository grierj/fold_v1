package main

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

var errRollbackTest = errors.New("roll the test back")

var regStatusValues = []string{models.RegGoing, models.RegMaybe, models.RegDeclined, models.RegCancelled, models.RegWaitlisted, models.RegRemoved}

func TestRegistrationStatusesMatchTheSchema(t *testing.T) {
	check := enumeratedChecks(t)["registrations.status"]
	named := map[string]bool{}
	for _, m := range policy.RegistrationMoves() {
		for _, from := range append(m.From(), policy.RegNone) {
			for _, to := range regStatusValues {
				if m.StartsFrom(from) && from != policy.RegNone {
					named[from] = true
				}
				if m.AllowsStep(from, to) {
					named[to] = true
				}
			}
		}
	}
	var fromTable []string
	for s := range named {
		fromTable = append(fromTable, s)
	}
	slices.Sort(fromTable)
	consts := slices.Clone(regStatusValues)
	slices.Sort(consts)
	if len(check) == 0 || !reflect.DeepEqual(fromTable, check) || !reflect.DeepEqual(consts, check) {
		t.Fatalf("registrations_status_check allows %v; the moves name %v and models names %v", check, fromTable, consts)
	}
}

// registrationRowsFixture is one date and a member per status.
func registrationRowsFixture(t *testing.T) (*App, models.EventOccurrence, []*models.User) {
	t.Helper()
	app := testApp(t)
	admin, gid := openGroup(t, app, "Rows")
	eid, date := seedCappedEvent(t, app, admin, gid, nil, true)
	var occ models.EventOccurrence
	if err := app.DB.Where("event_id = ? AND original_date = ?", eid, date).First(&occ).Error; err != nil {
		t.Fatal(err)
	}
	var users []*models.User
	for _, s := range regStatusValues {
		users = append(users, newTestUser(t, app, "rows-"+s, "password1234", models.RoleMember))
	}
	return app, occ, users
}

func registrationIn(status string, userID, occurrenceID uint) models.Registration {
	r := models.Registration{UserID: &userID, OccurrenceID: occurrenceID, Status: status, Source: models.RegSourceWeb, AttendanceMode: models.AttendanceInPerson}
	if status == models.RegWaitlisted {
		at := time.Now().UTC()
		r.WaitlistedAt = &at
	}
	return r
}

// The SQL form of a move's from-set (the manager's removal and promotion read
// rows with it) picks exactly the rows the Go form starts from, over a row in
// every status.
func TestRegistrationStartsFromSQLMatchesTheRule(t *testing.T) {
	app, occ, users := registrationRowsFixture(t)
	for i, s := range regStatusValues {
		r := registrationIn(s, users[i].ID, occ.ID)
		if err := app.DB.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range policy.RegistrationMoves() {
		var got []string
		if err := app.DB.Model(&models.Registration{}).Where("occurrence_id = ?", occ.ID).
			Where(registrationStartsFrom(m)).Order("status").Pluck("status", &got).Error; err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, s := range regStatusValues {
			if m.StartsFrom(s) {
				want = append(want, s)
			}
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%q: SQL picks %v, Go starts from %v", m, got, want)
		}
	}
}

// promoteRegistration's conditional update moves exactly the rows the rule
// promotes, to going, and leaves every other status alone.
func TestPromoteRegistrationSQLMatchesTheRule(t *testing.T) {
	app, occ, users := registrationRowsFixture(t)
	for i, s := range regStatusValues {
		r := registrationIn(s, users[i].ID, occ.ID)
		if err := app.DB.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
		promoted, err := promoteRegistration(app.DB, r, "hash-"+s, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		var after models.Registration
		if err := app.DB.First(&after, r.ID).Error; err != nil {
			t.Fatal(err)
		}
		want := policy.RegPromote.Allows(s, models.RegGoing)
		if promoted != want || (after.Status == models.RegGoing) != (want || s == models.RegGoing) ||
			(!want && after.Status != s) {
			t.Errorf("promote from %s: promoted %v, now %s; the rule promotes: %v", s, promoted, after.Status, want)
		}
	}
}

// saveRegistration and saveRegistrationStep write exactly the (move, from,
// to) triples the rule holds, last or as a step, and a refusal writes nothing.
// A row's id must agree with its from: none makes a row, any other status
// writes one.
func TestSaveRegistrationRefusesMovesTheRuleDoesNotHold(t *testing.T) {
	app, occ, users := registrationRowsFixture(t)
	uid := users[0].ID
	froms := append([]string{policy.RegNone}, regStatusValues...)
	err := app.DB.Transaction(func(tx *gorm.DB) error {
		for _, m := range policy.RegistrationMoves() {
			for _, from := range froms {
				for _, to := range regStatusValues {
					for _, step := range []bool{false, true} {
						if err := tx.SavePoint("reg_case").Error; err != nil {
							return err
						}
						var id uint
						if from != policy.RegNone {
							seed := registrationIn(from, uid, occ.ID)
							if err := tx.Create(&seed).Error; err != nil {
								return err
							}
							id = seed.ID
						}
						r := registrationIn(to, uid, occ.ID)
						r.ID = id
						save, allowed := saveRegistration, m.Allows(from, to)
						if step {
							save, allowed = saveRegistrationStep, m.AllowsStep(from, to)
						}
						err := save(tx, m, from, &r)
						var rows []models.Registration
						if err := tx.Where("occurrence_id = ? AND user_id = ?", occ.ID, uid).Find(&rows).Error; err != nil {
							return err
						}
						switch {
						case allowed && err != nil:
							t.Errorf("%q %s>%s (step %v) refused: %v", m, from, to, step, err)
						case !allowed && err == nil:
							t.Errorf("%q %s>%s (step %v) was written", m, from, to, step)
						case allowed && (len(rows) != 1 || rows[0].Status != to):
							t.Errorf("%q %s>%s (step %v) left %v", m, from, to, step, rows)
						case !allowed && from == policy.RegNone && len(rows) != 0:
							t.Errorf("%q %s>%s (step %v) refused but made a row", m, from, to, step)
						case !allowed && from != policy.RegNone && (len(rows) != 1 || rows[0].Status != from):
							t.Errorf("%q %s>%s (step %v) refused but left %v", m, from, to, step, rows)
						}
						if err := tx.RollbackTo("reg_case").Error; err != nil {
							return err
						}
					}
				}
			}
		}
		// The id and the from disagree: a new row judged as going, an
		// existing row judged as none.
		r := registrationIn(models.RegGoing, uid, occ.ID)
		if err := saveRegistration(tx, policy.RegAddByManager, models.RegGoing, &r); err == nil {
			t.Error("a row with no id was saved as judged going")
		}
		seed := registrationIn(models.RegMaybe, uid, occ.ID)
		if err := tx.Create(&seed).Error; err != nil {
			return err
		}
		seed.Status = models.RegGoing
		if err := saveRegistration(tx, policy.RegAddByManager, policy.RegNone, &seed); err == nil {
			t.Error("an existing row was saved as judged no row")
		}
		return errRollbackTest
	})
	if err != errRollbackTest {
		t.Fatal(err)
	}
}
