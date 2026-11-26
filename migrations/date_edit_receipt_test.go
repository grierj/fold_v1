package migrations_test

import "testing"

func TestDateEditReceiptContract(t *testing.T) {
	t.Parallel()
	const version = 18
	_, db, p := freshAt(t, version-1)
	eventID, occID := seriesStartsFixture(t, db)
	const events = `SELECT id, to_jsonb(e)::text FROM events e`
	const dates = `SELECT id, (to_jsonb(o) - 'edit_receipt')::text FROM event_occurrences o`
	const receipt = `SELECT edit_receipt IS NULL FROM event_occurrences WHERE id = $1`
	storeReceipt := func() {
		t.Helper()
		var stored bool
		if err := db.QueryRow(`UPDATE event_occurrences SET edit_receipt = decode(repeat('ab', 32), 'hex') WHERE id = $1
			RETURNING edit_receipt = decode(repeat('ab', 32), 'hex')`, occID).Scan(&stored); err != nil || !stored {
			t.Fatalf("stored receipt = %t, %v, want true", stored, err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE events SET title = 'Renamed' WHERE id = $1`, eventID)
	exec(`UPDATE event_occurrences SET title_override = 'Date title' WHERE id = $1`, occID)
	storeReceipt()

	checkWrites := func(revision int64) {
		t.Helper()
		for _, row := range []struct {
			table, title string
			id           int64
		}{
			{"events", "title", eventID},
			{"event_occurrences", "title_override", occID},
		} {
			for _, write := range []struct {
				set  string
				want int64
			}{
				{"updated_at = now(), edit_revision = 99, " + row.title + " = " + row.title, revision},
				{row.title + " = " + row.title + " || '!', edit_revision = 99", revision + 1},
			} {
				var got int64
				q := `UPDATE ` + row.table + ` SET ` + write.set + ` WHERE id = $1 RETURNING edit_revision`
				if err := db.QueryRow(q, row.id).Scan(&got); err != nil || got != write.want {
					t.Fatalf("%s returned revision %d, %v, want %d", q, got, err, write.want)
				}
			}
		}
	}
	checkUp := func(label string, revision int64) {
		t.Helper()
		beforeEvents, beforeDates := snapshotRows[int64](t, db, events), snapshotRows[int64](t, db, dates)
		upTo(t, p, version)
		sameRows(t, "events after "+label, snapshotRows[int64](t, db, events), beforeEvents)
		sameRows(t, "dates after "+label, snapshotRows[int64](t, db, dates), beforeDates)
		var empty bool
		if err := db.QueryRow(receipt, occID).Scan(&empty); pgErrorCode(err) != "42703" {
			t.Fatalf("receipt read after %s: %v, want undefined_column", label, err)
		}
		checkWrites(revision)
	}
	checkUp("Up", 2)

	beforeEvents, beforeDates := snapshotRows[int64](t, db, events), snapshotRows[int64](t, db, dates)
	downTo(t, p, version-1)
	sameRows(t, "events after Down", snapshotRows[int64](t, db, events), beforeEvents)
	sameRows(t, "dates after Down", snapshotRows[int64](t, db, dates), beforeDates)
	var empty bool
	if err := db.QueryRow(receipt, occID).Scan(&empty); err != nil || !empty {
		t.Fatalf("receipt is NULL after Down = %t, %v, want true", empty, err)
	}
	if _, err := db.Exec(`UPDATE event_occurrences SET edit_receipt = '\x01' WHERE id = $1`, occID); pgErrorCode(err) != "23514" {
		t.Fatalf("short receipt after Down: %v, want check_violation", err)
	}
	storeReceipt()
	checkWrites(3)
	if err := db.QueryRow(receipt, occID).Scan(&empty); err != nil || !empty {
		t.Fatalf("receipt is NULL after tracked write = %t, %v, want true", empty, err)
	}
	checkUp("second Up", 4)
}
