package main

import (
	"fmt"
	"regexp"
	"strings"

	"fold/internal/policy"
	"fold/models"

	"gorm.io/gorm"
)

// The writers of room_bookings.status and room_requests.status for rows that
// exist (docs/agents/lifecycles.md); placeBooking and openRoomRequest insert
// a move's first status (TestRoomStatusHasOneWriter).

var roomStatusInSetRe = regexp.MustCompile(`(?i)(?:^|[\s,."])status"?\s*=`)

// changeBookings moves the bookings that match where by move, in one UPDATE
// whose WHERE is the move's from-statuses (policy.RoomBookingMove) and where.
// It writes the status only when the move changes it (a date that moves and
// keeps its room, and a waiting booking that becomes lost, keep theirs), then
// set with setArgs, and updated_at = now(). It returns the rows moved; the database's error comes back as it is, so trySavepoint can tell
// the exclusion constraint's refusal.
//
// The WHERE tests the row's status only. A move that starts from some states
// of a status (waiting, not lost; confirmed, not double-booked) relies on its
// caller: where names rows already in that state, and the caller's locks
// (the event's, and lockRequest for a decision) keep them there.
func changeBookings(tx *gorm.DB, move policy.RoomBookingMove, set string, setArgs []any, where string, whereArgs ...any) (int64, error) {
	if !move.Updates() {
		return 0, fmt.Errorf("room booking move %q changes no booking row", move)
	}
	if roomStatusInSetRe.MatchString(set) {
		return 0, fmt.Errorf("room booking move %q: the move decides the status, not the caller's columns", move)
	}
	var cols []string
	var args []any
	from := move.FromStatuses()
	if to := move.To(); to != "" && !(len(from) == 1 && from[0] == to.Status()) {
		cols, args = append(cols, "status = ?"), append(args, to.Status())
	}
	if set != "" {
		cols, args = append(cols, set), append(args, setArgs...)
	}
	cols = append(cols, "updated_at = now()")
	args = append(append(args, from), whereArgs...)
	res := tx.Exec("UPDATE room_bookings SET "+strings.Join(cols, ", ")+" WHERE status IN ? AND ("+where+")", args...)
	return res.RowsAffected, res.Error
}

// roomRequestMoveColumns is every column a decision may write beside the
// status. Naming them keeps the status out of a caller's fields under any
// spelling gorm resolves (the column, the field, a quoted identifier).
var roomRequestMoveColumns = map[string]bool{"decided_by": true, "decided_at": true, "reject_reason": true, "updated_at": true}

// moveRoomRequest decides req by move: one UPDATE of req's row whose WHERE
// adds the move's from-set, writing the move's status and fields together.
// moved is false when the row was no longer in the from-set.
func moveRoomRequest(tx *gorm.DB, req *models.RoomRequest, move policy.RoomRequestMove, fields map[string]any) (moved bool, err error) {
	from, err := roomRequestMoveFrom(move)
	if err != nil {
		return false, err
	}
	if req == nil || req.ID == 0 {
		return false, fmt.Errorf("room request move %q: no request to move", move)
	}
	for k := range fields {
		if !roomRequestMoveColumns[k] {
			return false, fmt.Errorf("room request move %q: %q is not a column a decision writes; the move decides the status", move, k)
		}
	}
	set := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		set[k] = v
	}
	set["status"] = move.To()
	res := tx.Model(req).Where("status IN ?", from).Updates(set)
	return res.RowsAffected > 0, res.Error
}

// moveRoomRequestsWhere moves every request that matches where by move in one
// statement, stamping updated_at with the transaction's time, and returns the
// ids it moved.
func moveRoomRequestsWhere(tx *gorm.DB, move policy.RoomRequestMove, where string, args ...any) ([]uint, error) {
	from, err := roomRequestMoveFrom(move)
	if err != nil {
		return nil, err
	}
	var ids []uint
	err = tx.Raw(`UPDATE room_requests SET status = ?, updated_at = now() WHERE status IN ? AND (`+where+`) RETURNING id`,
		append([]any{move.To(), from}, args...)...).Scan(&ids).Error
	return ids, err
}

func roomRequestMoveFrom(move policy.RoomRequestMove) ([]string, error) {
	from := move.From()
	if len(from) == 0 || move.To() == "" {
		return nil, fmt.Errorf("room request move %q changes no request's status: an insert or a delete writes it", move)
	}
	return from, nil
}
