package app

import (
	"reflect"
	"strings"
	"testing"

	"fold/internal/guardkit"
)

// room_bookings.status and room_requests.status each have one writer of an
// existing row's status: changeBookings, and moveRoomRequest with
// moveRoomRequestsWhere for a set. Their WHEREs are the moves' from-sets in
// internal/policy (room_lifecycle.go). placeBooking inserts a booking's first
// status and openRoomRequest a request's (docs/agents/lifecycles.md).
//
// The guard covers status. Booking request_id also participates in the lifecycle.

var roomStatusWriterCensus = []string{
	"changeBookings: raw UPDATE room_bookings with a column list built at run time",
	"moveRoomRequest: Updates of a map given \"status\"",
	"moveRoomRequestsWhere: raw UPDATE of room_requests.status",
	"openRoomRequest: Create of a room request",
	"placeBooking: raw INSERT INTO room_bookings",
}

var (
	roomBookingStatusGuard = newTableStatusGuard("RoomBooking", "room_bookings", "a room booking")
	roomRequestStatusGuard = newTableStatusGuard("RoomRequest", "room_requests", "a room request")
)

func roomStatusWrites(p *guardkit.Parsed) []tableStatusWrite {
	return append(roomBookingStatusGuard.writes(p), roomRequestStatusGuard.writes(p)...)
}

func TestRoomStatusHasOneWriter(t *testing.T) {
	parsed := guardkit.Production(t).Parse(0)
	for _, p := range parsed.Problems {
		t.Error(p)
	}
	writes := roomStatusWrites(parsed)
	if got := tableStatusCensus(writes); !reflect.DeepEqual(got, roomStatusWriterCensus) {
		var where []string
		for _, w := range writes {
			where = append(where, w.at+" "+w.fn+": "+w.shape)
		}
		t.Errorf("the writes of room_bookings.status and room_requests.status are\n%s\nwant exactly %v; change a booking's status with changeBookings and a policy.RoomBookingMove, a request's with moveRoomRequest and a policy.RoomRequestMove", strings.Join(where, "\n"), roomStatusWriterCensus)
	}
}

func TestRoomStatusGuardCatchesBypasses(t *testing.T) {
	const decls = `
func loadRequest(tx *gorm.DB, id uint) (*models.RoomRequest, error) { return nil, nil }
`
	for _, c := range []struct{ name, src, want string }{
		{"a computed key in an inline map", `package app
func decide(tx *gorm.DB, col string) error {
	return tx.Model(&models.RoomRequest{}).Updates(map[string]any{col: "confirmed"}).Error
}`, "decide: Updates of a map with computed keys"},
		{"a confirmation as it was written", `package app
func confirm(tx *gorm.DB, b models.RoomBooking) error {
	return tx.Exec(` + "`UPDATE room_bookings SET status = ?, updated_at = now() WHERE id = ?`" + `, models.BookingConfirmed, b.ID).Error
}`, "confirm: raw UPDATE of room_bookings.status"},
		{"a move with status last, as it was written", `package app
func move(tx *gorm.DB, id uint) error {
	return tx.Exec(` + "`UPDATE room_bookings SET starts_at = ?, padded_range = tstzrange(?, ?), status = ?, updated_at = now() WHERE id = ?`" + `).Error
}`, "move: raw UPDATE of room_bookings.status"},
		{"a withdrawal as it was written", `package app
func withdraw(tx *gorm.DB, eventID uint) error {
	return tx.Exec(` + "`UPDATE room_requests SET status = ?, updated_at = now()\n\t\tWHERE event_id = ? AND series AND status = ? RETURNING id`" + `).Error
}`, "withdraw: raw UPDATE of room_requests.status"},
		{"a decision as it was written", `package app
func decide(tx *gorm.DB, req *models.RoomRequest) error {
	return tx.Model(req).Updates(map[string]any{"status": models.RoomRequestConfirmed, "decided_at": time.Now()}).Error
}`, `decide: Updates of a map with "status"`},
		{"a query held in a variable", `package app
func decide(tx *gorm.DB, id uint) error {
	q := tx.Model(&models.RoomRequest{}).Where("id = ?", id)
	return q.Updates(map[string]any{"status": "rejected"}).Error
}`, `decide: Updates of a map with "status"`},
		{"a map given status later", `package app
func decide(tx *gorm.DB, id uint) error {
	fields := map[string]any{"decided_at": time.Now()}
	fields["status"] = models.RoomRequestRejected
	return tx.Model(&models.RoomRequest{}).Where("id = ?", id).Updates(fields).Error
}`, `decide: Updates of a map given "status"`},
		{"a bare Update on a loaded request", `package app
func decide(tx *gorm.DB, id uint) error {
	r, _ := loadRequest(tx, id)
	return tx.Model(r).Update("status", models.RoomRequestWithdrawn).Error
}` + decls, `decide: Update("status")`},
		{"a range over bookings", `package app
func reject(tx *gorm.DB) error {
	var bookings []models.RoomBooking
	for _, b := range bookings {
		tx.Model(&b).UpdateColumn("status", models.BookingRejected)
	}
	return nil
}`, `reject: UpdateColumn("status")`},
		{"the table by name", `package app
func reject(tx *gorm.DB, id uint) error {
	return tx.Table("room_bookings").Where("request_id = ?", id).Update("status", "rejected").Error
}`, `reject: Update("status")`},
		{"a column named at run time", `package app
func set(tx *gorm.DB, b *models.RoomBooking, col string) error {
	return tx.Model(b).Update(col, "confirmed").Error
}`, "set: Update of a computed column"},
		{"a helper that writes its caller's columns", `package app
func writeRequest(tx *gorm.DB, id uint, cols map[string]any) error {
	return tx.Model(&models.RoomRequest{}).Where("id = ?", id).Updates(cols).Error
}`, "writeRequest: Updates of a parameter's map"},
		{"a saved booking", `package app
func confirm(tx *gorm.DB, b *models.RoomBooking) error {
	b.Status = models.BookingConfirmed
	return tx.Save(b).Error
}`, "confirm: Save of a room booking"},
		{"a booking struct as the update", `package app
func confirm(tx *gorm.DB, id uint) error {
	return tx.Model(&models.RoomBooking{}).Where("id = ?", id).Updates(models.RoomBooking{Status: "confirmed"}).Error
}`, "confirm: Updates of a room booking struct"},
		{"a new booking through gorm", `package app
func book(tx *gorm.DB) error {
	b := models.RoomBooking{RoomID: 1}
	return tx.Create(&b).Error
}`, "book: Create of a room booking"},
		{"a raw request insert", `package app
func ask(tx *gorm.DB) error {
	return tx.Exec("INSERT INTO room_requests (event_id, room_id) VALUES (1, 1)").Error
}`, "ask: raw INSERT INTO room_requests"},
		{"raw SQL in a constant, with an alias", `package app
const rejectSQL = ` + "`UPDATE room_bookings AS b SET \"status\" = 'rejected' WHERE b.request_id = ?`" + `
`, "rejectSQL: raw UPDATE of room_bookings.status"},
		{"a moved date's update, as it was before the writer", `package app
func moveRoomBookings(tx *gorm.DB, b models.RoomBooking, status string) error {
	return tx.Exec(` + "`UPDATE room_bookings SET starts_at = ?, ends_at = ?, padded_range = `+paddedRangeSQL+`, status = ?, updated_at = now() WHERE id = ?`" + `,
		b.StartsAt, b.EndsAt, b.StartsAt, b.SetupMinutes, b.EndsAt, b.TeardownMinutes, status, b.ID).Error
}`, "moveRoomBookings: raw UPDATE room_bookings with a column list built at run time"},
		{"raw SQL with its columns built at run time", `package app
func set(tx *gorm.DB, cols string) error {
	return tx.Exec("UPDATE room_requests SET " + cols + " WHERE id = 1").Error
}`, "set: raw UPDATE room_requests with a column list built at run time"},
		{"another table's status, and other room columns", `package app
func other(tx *gorm.DB, req *models.RoomRequest, ev *models.Event, id uint) error {
	tx.Model(ev).Updates(map[string]any{"status": "active", "default_room_id": nil})
	tx.Model(&models.Membership{}).Where("id = ?", id).Update("status", "left")
	tx.Exec("UPDATE event_occurrences SET status = 'cancelled' WHERE id = ?", id)
	tx.Exec("UPDATE room_bookings SET request_id = NULL WHERE status = 'requested'")
	tx.Exec("UPDATE room_requests SET reject_reason = '' WHERE status = 'pending'")
	tx.Model(req).Update("reject_reason", "")
	tx.Delete(req)
	return tx.Where("occurrence_id = ?", id).Delete(&models.RoomBooking{}).Error
}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := guardkit.Sources{"internal/app/x.go": c.src}.Parse(0)
			if len(p.Problems) > 0 {
				t.Fatal(p.Problems)
			}
			got := strings.Join(tableStatusCensus(roomStatusWrites(p)), "\n")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
