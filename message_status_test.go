package main

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"fold/internal/policy"
	"fold/models"
)

func TestMessageStatusesMatchTheSchema(t *testing.T) {
	check := enumeratedChecks(t)["messages.status"]
	named := map[string]bool{}
	for _, m := range policy.MessageMoves() {
		for _, s := range append(m.From(), m.To()) {
			if s != "" {
				named[s] = true
			}
		}
	}
	var fromTable []string
	for s := range named {
		fromTable = append(fromTable, s)
	}
	slices.Sort(fromTable)
	consts := []string{models.MessageDraft, models.MessageScheduled, models.MessageSending, models.MessageSent, models.MessageCancelled, models.MessageFailed}
	slices.Sort(consts)
	if len(check) == 0 || !reflect.DeepEqual(fromTable, check) || !reflect.DeepEqual(consts, check) {
		t.Fatalf("messages_status_check allows %v; the moves name %v and models names %v", check, fromTable, consts)
	}
}

// messageInEveryStatus writes one message in each of statuses for author.
func messageInEveryStatus(t *testing.T, app *App, statuses []string, author uint) map[uint]string {
	t.Helper()
	at := time.Now().Add(time.Hour)
	out := map[uint]string{}
	for _, s := range statuses {
		m := models.Message{AuthorID: &author, Status: s, Category: models.MessageCategoryAnnouncement, Subject: s, ScheduledAt: &at}
		if err := app.DB.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
		out[m.ID] = s
	}
	return out
}

func messageStatusOf(t *testing.T, app *App, id uint) string {
	t.Helper()
	var m models.Message
	if err := app.DB.Select("status").First(&m, id).Error; err != nil {
		t.Fatal(err)
	}
	return m.Status
}

// moveMessage's SQL moves a row exactly when the move's Go from-set allows
// its status, to the move's status (an edit keeps it), and a false extra
// condition moves nothing.
func TestMoveMessageSQLMatchesTheRule(t *testing.T) {
	statuses := enumeratedChecks(t)["messages.status"] // before testApp: it clears the database
	app := testApp(t)
	author := newTestUser(t, app, "author", "password1234", models.RoleMember)
	for _, move := range policy.MessageMoves() {
		if len(move.From()) == 0 {
			continue
		}
		for id, from := range messageInEveryStatus(t, app, statuses, author.ID) {
			if moved, err := moveMessage(app.DB, id, move, map[string]any{"subject": "missed"}, "subject <> ?", from); err != nil || moved {
				t.Fatalf("%q from %s with a false condition: moved %v, %v", move, from, moved, err)
			}
			moved, err := moveMessage(app.DB, id, move, map[string]any{"subject": "moved"}, "")
			if err != nil {
				t.Fatal(err)
			}
			want := from
			if move.Allows(from) && move.To() != "" {
				want = move.To()
			}
			if moved != move.Allows(from) || messageStatusOf(t, app, id) != want {
				t.Errorf("%q from %s: moved %v to %s, want moved %v to %s", move, from, moved, messageStatusOf(t, app, id), move.Allows(from), want)
			}
		}
	}
}

// The set form moves exactly the matching rows whose status the move
// allows, and returns their ids.
func TestMoveMessagesWhereSQLMatchesTheRule(t *testing.T) {
	statuses := enumeratedChecks(t)["messages.status"] // before testApp: it clears the database
	app := testApp(t)
	other := newTestUser(t, app, "other", "password1234", models.RoleMember)
	for i, move := range policy.MessageMoves() {
		if len(move.From()) == 0 || move.To() == "" {
			continue
		}
		author := newTestUser(t, app, "author"+itoa(uint(i)), "password1234", models.RoleMember)
		rows := messageInEveryStatus(t, app, statuses, author.ID)
		bystanders := messageInEveryStatus(t, app, statuses, other.ID)
		ids, err := moveMessagesWhere(app.DB, move, "author_id = ?", author.ID)
		if err != nil {
			t.Fatal(err)
		}
		var want []uint
		for id, from := range rows {
			if move.Allows(from) {
				want = append(want, id)
			}
		}
		slices.Sort(ids)
		slices.Sort(want)
		if !reflect.DeepEqual(ids, want) {
			t.Errorf("%q moved %v, want %v", move, ids, want)
		}
		for id, from := range rows {
			if got, wantStatus := messageStatusOf(t, app, id), map[bool]string{true: move.To(), false: from}[move.Allows(from)]; got != wantStatus {
				t.Errorf("%q from %s: now %s, want %s", move, from, got, wantStatus)
			}
		}
		for id, from := range bystanders {
			if got := messageStatusOf(t, app, id); got != from {
				t.Errorf("%q moved another author's %s message to %s", move, from, got)
			}
		}
	}
}

func TestMoveMessageRefusesWhatTheRuleDoesNotHold(t *testing.T) {
	statuses := enumeratedChecks(t)["messages.status"] // before testApp: it clears the database
	app := testApp(t)
	author := newTestUser(t, app, "author", "password1234", models.RoleMember)
	var draft uint
	for id, s := range messageInEveryStatus(t, app, statuses, author.ID) {
		if s == models.MessageDraft {
			draft = id
		}
	}
	for name, try := range map[string]func() error{
		"a creation move": func() error {
			_, err := moveMessage(app.DB, draft, policy.MessageCreateDraft, map[string]any{"subject": "x"}, "")
			return err
		},
		"an unknown move": func() error {
			_, err := moveMessage(app.DB, draft, policy.MessageMove("publish"), map[string]any{"subject": "x"}, "")
			return err
		},
		"a status in the fields": func() error {
			_, err := moveMessage(app.DB, draft, policy.MessageEdit, map[string]any{"status": models.MessageSent}, "")
			return err
		},
		"the status by its field name": func() error {
			_, err := moveMessage(app.DB, draft, policy.MessageEdit, map[string]any{"Status": models.MessageSent}, "")
			return err
		},
		"an edit as a set": func() error {
			_, err := moveMessagesWhere(app.DB, policy.MessageEdit, "author_id = ?", author.ID)
			return err
		},
	} {
		if try() == nil {
			t.Errorf("%s was written", name)
		}
	}
	if got := messageStatusOf(t, app, draft); got != models.MessageDraft {
		t.Fatalf("the draft is now %s", got)
	}
}
