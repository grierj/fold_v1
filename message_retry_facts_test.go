package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// TestTryAgainAtChurchScale: Try again on a series announcement that failed
// for 22,000 people, more than the 21,845 whose ids, bound one per parameter
// in three lists, would pass PostgreSQL's 65,535-parameter limit. Half are
// members who still see the invite-only series; the other half are not and
// can't, so each of them takes the hidden-date path that looks for their own
// grants. It succeeds in one facts statement, queues the members and skips
// the rest. The time the presser's and the message's locks are held, and the
// statement's plan, are logged as evidence; the ceiling is only a guard
// against a hang.
func TestTryAgainAtChurchScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 22,000 people")
	}
	const people = 22000
	app := testApp(t)
	leader := newTestUser(t, app, "scale leader", "password1234", models.RoleMember)
	g := activeGroup(t, app, leader, "Scale group", models.VisibilityPublic, "")
	lc := loginAPI(t, app, leader.Name, "password1234")
	lc.holdNotices = true
	ev := createFeedEvent(t, lc, g.ID, "Scale series", feedStart(5), "FREQ=WEEKLY;COUNT=4")
	gormMust(t, app.DB.Model(&models.Event{}).Where("id = ?", ev.ID).Update("visibility", models.VisibilityInviteOnly))
	gormMust(t, app.DB.Exec("UPDATE event_occurrences SET visibility_override = NULL WHERE event_id = ?", ev.ID))
	var occ models.EventOccurrence
	gormMust(t, app.DB.Where("event_id = ?", ev.ID).Order("starts_at").First(&occ))
	au := audienceJSON{Type: policy.AudienceEventSeries, EventID: ev.ID}
	raw, _ := json.Marshal(au)
	at := time.Now().UTC().Add(-time.Hour)
	m := models.Message{AuthorID: &leader.ID, Status: models.MessageFailed, Category: models.MessageCategoryAnnouncement,
		Subject: "News", BodyHTML: "<p>News</p>", BodyText: "News", Audience: string(raw), ScheduledAt: &at}
	gormMust(t, app.DB.Create(&m))
	seeded := time.Now()
	gormMust(t, app.DB.Exec(`
		WITH u AS (
			INSERT INTO users (email, name, status, account_type, role, password_hash, created_at, updated_at)
			SELECT 'scale' || n || '@example.com', 'Scale ' || n, 'active', 'adult', 'member', 'x', now(), now()
			FROM generate_series(1, ?) n RETURNING id
		), r AS (
			INSERT INTO registrations (occurrence_id, user_id, status, created_at, updated_at)
			SELECT ?, id, 'going', now(), now() FROM u RETURNING user_id
		), mb AS (
			INSERT INTO memberships (user_id, group_id, role, status, joined_at)
			SELECT id, ?, 'member', 'active', now() FROM u WHERE id % 2 = 0 RETURNING user_id
		)
		INSERT INTO message_recipients (message_id, user_id, channel, address, status, error, created_at, updated_at)
		SELECT ?, u.id, 'email', 'scale' || u.id || '@example.com', 'failed', 'provider down', now(), now()
		FROM u`, people, occ.ID, g.ID, m.ID))
	t.Logf("seeded %d people in %s", people, time.Since(seeded))

	var locked time.Time
	app.hooks.retryLocked = func(*gorm.DB) { locked = time.Now() }
	start := time.Now()
	w := lc.do("POST", "/api/v1/messages/"+itoa(m.ID)+"/retry", nil, true)
	total, held := time.Since(start), time.Since(locked)
	if w.Code != http.StatusOK {
		t.Fatalf("Try again: %d %s", w.Code, w.Body.String())
	}
	t.Logf("Try again for %d failed rows: %s in all, locks held about %s", people, total, held)
	var counts []struct {
		Status, Reason string
		N              int
	}
	gormMust(t, app.DB.Raw("SELECT status, reason, count(*) AS n FROM message_recipients WHERE message_id = ? GROUP BY status, reason ORDER BY status", m.ID).Scan(&counts))
	got := map[string]int{}
	for _, c := range counts {
		got[c.Status+"/"+c.Reason] = c.N
	}
	if got[models.RecipientQueued+"/"] != people/2 || got[models.RecipientSuppressed+"/"+skipCantSeeDate] != people/2 {
		t.Fatalf("rows after Try again: %v", got)
	}
	if held > 60*time.Second {
		t.Fatalf("the locks were held %s", held)
	}

	// The plan of the one statement for the same question, at this size.
	var failed []models.MessageRecipient
	gormMust(t, app.DB.Select("id, user_id, copy_of_user_id").Where("message_id = ?", m.ID).Find(&failed))
	q := dateFactsQuery{MessageID: m.ID, People: []uint{leader.ID}, Viewers: []uint{leader.ID}, EventIDs: []uint{ev.ID},
		Settings: []string{settingTimezone, settingGuardianCopies}}
	for _, r := range failed {
		q.People, q.Viewers, q.Holders = append(q.People, r.UserID), append(q.Viewers, r.UserID), append(q.Holders, r.UserID)
	}
	rows, err := app.DB.Raw("EXPLAIN (ANALYZE, BUFFERS) "+dateFactsSQL, dateFactsArgs(q)).Rows()
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		gormMust(t, &gorm.DB{Error: rows.Scan(&line)})
		plan = append(plan, line)
	}
	gormMust(t, &gorm.DB{Error: rows.Close()})
	t.Logf("facts statement plan at %d people:\n%s", people, strings.Join(plan, "\n"))
}
