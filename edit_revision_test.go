package main

import (
	"encoding/hex"
	"errors"
	"testing"

	"fold/models"
)

// The tokens keep the bytes they had before edit_revision.go, so a page open
// across the deploy still saves.
func TestEditRevisionTokensKeepTheirBytes(t *testing.T) {
	a := &App{}
	gen := hex.EncodeToString(a.processKey("date-edit-generation:v1")[:8])
	ev := &models.Event{ID: 7, EditRevision: 3}
	if got, want := a.eventRevision(ev), "1."+gen+".7.3"; got != want {
		t.Fatalf("event token %q, want %q", got, want)
	}
	occ := &models.EventOccurrence{ID: 9, EditRevision: 4}
	if got, want := a.dateToken(ev, occ, nil), "1."+gen+".9.3.4.-"; got != want {
		t.Fatalf("date token %q, want %q", got, want)
	}
	if err := a.checkEventRevision(ev, "1."+gen+".7.3"); err != nil {
		t.Fatalf("the current token: %v", err)
	}
	for _, given := range []string{"", "1." + gen + ".7.2", "1." + gen + ".8.3", "1.0000000000000000.7.3", "garbled"} {
		if err := a.checkEventRevision(ev, given); !errors.Is(err, errEventChanged) {
			t.Fatalf("%q: %v, want errEventChanged", given, err)
		}
	}
}
