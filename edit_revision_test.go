package main

import (
	"encoding/hex"
	"errors"
	"testing"

	"fold/models"
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
