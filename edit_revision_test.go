package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"fold/models"

	"gorm.io/gorm"
)

// The token formats: the date edit reads the generation at base[2:18], and a
// token from another purpose string would never match a running process's.
func TestEditRevisionTokensKeepTheirBytes(t *testing.T) {
	a := &App{}
	gen := hex.EncodeToString(a.processKey("date-edit-generation:v1")[:8])
	ev := &models.Event{ID: 7, EditRevision: 3}
	if got, want := a.eventRevision(ev, nil), "1."+gen+".7.3.c"; got != want {
		t.Fatalf("church-wide event token %q, want %q", got, want)
	}
	gid := uint(5)
	grouped := &models.Event{ID: 7, EditRevision: 3, GroupID: &gid}
	for vis, tag := range map[string]string{models.VisibilityPublic: "pub", models.VisibilityPrivate: "prv", models.VisibilityInviteOnly: "inv"} {
		if got, want := a.eventRevision(grouped, &models.Group{Visibility: vis}), "1."+gen+".7.3."+tag; got != want {
			t.Fatalf("event token in a %s group %q, want %q", vis, got, want)
		}
	}
	occ := &models.EventOccurrence{ID: 9, EditRevision: 4}
	if got, want := a.dateToken(ev, occ, nil), "1."+gen+".9.3.4.-"; got != want {
		t.Fatalf("date token %q, want %q", got, want)
	}
	if err := a.checkEventRevision(ev, nil, "1."+gen+".7.3.c"); err != nil {
		t.Fatalf("the current token: %v", err)
	}
	public, private := &models.Group{Visibility: models.VisibilityPublic}, &models.Group{Visibility: models.VisibilityPrivate}
	if err := a.checkEventRevision(grouped, public, "1."+gen+".7.3.pub"); err != nil {
		t.Fatalf("the current token in a group: %v", err)
	}
	for _, given := range []string{"", "1." + gen + ".7.3", "1." + gen + ".7.2.c", "1." + gen + ".8.3.c", "1.0000000000000000.7.3.c", "garbled"} {
		if err := a.checkEventRevision(ev, nil, given); !errors.Is(err, errEventChanged) {
			t.Fatalf("%q: %v, want errEventChanged", given, err)
		}
	}
	if err := a.checkEventRevision(grouped, private, "1."+gen+".7.3.pub"); !errors.Is(err, errEventChanged) {
		t.Fatalf("a token from before the group's change: %v, want errEventChanged", err)
	}
	// The group's last part is a digest of its row (TestGroupRevisionMovesWithEveryWrite).
	const digest = "0123456789abcdef0123456789abcdef"
	group := a.editRevision("g7", digest)
	if want := "1." + gen + ".g7." + digest; group != want {
		t.Fatalf("group token %q, want %q", group, want)
	}
	// An event's or a date's token is never a group's, nor the other way round.
	for _, given := range []string{"", "1." + gen + ".7.3", "1." + gen + ".7.3.c", "1." + gen + ".g8." + digest, "1.0000000000000000.g7." + digest, a.eventRevision(ev, nil), a.dateToken(ev, occ, nil)} {
		if err := checkEditRevision(given, group, errGroupChanged); !errors.Is(err, errGroupChanged) {
			t.Fatalf("%q: %v, want errGroupChanged", given, err)
		}
	}
	if err := a.checkEventRevision(ev, nil, group); !errors.Is(err, errEventChanged) {
		t.Fatalf("a group token taken for an event's: %v", err)
	}
}

// The row token (rowRevisionIn) of a date's booking moves with every column
// and with a write that only stamps updated_at; a session in another time zone
// reads the same token; no booking reads a fixed token bound to the date. The
// group's digest keeps the bytes the group form's tokens had before the
// helper (TestGroupRevisionMovesWithEveryWrite covers its row).
func TestRowRevisionCoversTheWholeRow(t *testing.T) {
	f := newBookingFixture(t)
	var occ models.EventOccurrence
	gormMust(t, f.app.DB.Where("event_id = ?", f.eventA.ID).First(&occ))
	token := func() string {
		t.Helper()
		got, err := f.app.dateRoomRevisionIn(f.app.DB, occ.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	empty := token()
	if want := f.app.editRevision("b"+itoa(occ.ID), "none"); empty != want {
		t.Fatalf("no booking: %q, want %q", empty, want)
	}
	if code, body := f.book(t, f.leader, f.eventA, map[string]any{"roomId": f.hall.ID}); code != http.StatusOK {
		t.Fatalf("book: %d %s", code, body)
	}
	booked := token()
	if !regexp.MustCompile(`^1\.[0-9a-f]{16}\.b`+itoa(occ.ID)+`\.[0-9a-f]{32}$`).MatchString(booked) || booked == empty {
		t.Fatalf("booked token %q", booked)
	}
	gormMust(t, f.app.DB.Exec("UPDATE room_bookings SET status = 'requested' WHERE occurrence_id = ?", occ.ID))
	var chapel struct {
		Room roomJSON `json:"room"`
	}
	decodeJSON(t, f.admin.do("POST", "/api/v1/admin/rooms", map[string]any{"name": "Chapel"}, true), &chapel)
	gormMust(t, f.app.DB.Exec("INSERT INTO room_requests (event_id, room_id, status) VALUES (?, ?, 'pending')", f.eventA.ID, f.hall.ID))
	for _, change := range []string{
		"updated_at = updated_at + interval '1 second'",
		"room_id = " + itoa(chapel.Room.ID),
		"starts_at = starts_at - interval '1 minute', padded_range = tstzrange(starts_at - interval '1 minute' - make_interval(mins => setup_minutes::int), ends_at + make_interval(mins => teardown_minutes::int), '[)')",
		"ends_at = ends_at + interval '1 minute', padded_range = tstzrange(starts_at - make_interval(mins => setup_minutes::int), ends_at + interval '1 minute' + make_interval(mins => teardown_minutes::int), '[)')",
		"setup_minutes = setup_minutes + 1, padded_range = tstzrange(starts_at - make_interval(mins => setup_minutes::int + 1), ends_at + make_interval(mins => teardown_minutes::int), '[)')",
		"teardown_minutes = teardown_minutes + 1, padded_range = tstzrange(starts_at - make_interval(mins => setup_minutes::int), ends_at + make_interval(mins => teardown_minutes::int + 1), '[)')",
		"status = 'rejected'", "allow_overlap = true, overlap_reason = 'Shared'", "overlap_reason = 'Shared twice'",
		"created_by = NULL", "request_id = (SELECT max(id) FROM room_requests)",
		"id = id + 1000",
	} {
		before := token()
		gormMust(t, f.app.DB.Exec("UPDATE room_bookings SET "+change+" WHERE occurrence_id = ?", occ.ID))
		if now := token(); now == before {
			t.Errorf("%s left the token at %q", change, now)
		}
	}
	before := token()
	gormMust(t, f.app.DB.Exec("UPDATE room_requests SET status = 'confirmed' WHERE id = (SELECT max(id) FROM room_requests)"))
	if now := token(); now == before {
		t.Errorf("the request's decision left the token at %q", now)
	}
	here := token()
	if err := f.app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL TIME ZONE 'Asia/Tokyo'").Error; err != nil {
			return err
		}
		elsewhere, err := f.app.dateRoomRevisionIn(tx, occ.ID)
		if err == nil && elsewhere != here {
			err = fmt.Errorf("a session in another time zone read %q, want %q", elsewhere, here)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	g := newTestGroup(t, f.app, f.leaderUser, "Digest", models.GroupActive)
	var old string
	gormMust(t, f.app.DB.Raw("SELECT md5((to_jsonb(g) - ARRAY['updated_at','created_at'])::text || coalesce(extract(epoch FROM g.updated_at)::text, '')) FROM groups g WHERE g.id = ?", g.ID).Scan(&old))
	got, err := f.app.groupRevisionIn(f.app.DB, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.app.editRevision("g"+itoa(g.ID), old); got != want {
		t.Fatalf("group token %q, want the bytes it had before the helper %q", got, want)
	}
}
