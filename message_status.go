package main

import (
	"fmt"

	"gorm.io/gorm"

	"fold/internal/policy"
	"fold/models"
)

// moveMessage changes message id's status by move, in one conditional
// UPDATE: its WHERE is the move's from-set (policy.MessageMove) and, when
// given, where with args; it writes the move's status and fields together.
// moved is false when no row matched, which each caller answers as the race
// it lost. An edit keeps the status and writes only fields. This and
// moveMessagesWhere are the only writers of messages.status besides the
// creators that insert a move's first status (TestMessageStatusHasOneWriter).
func moveMessage(tx *gorm.DB, id uint, move policy.MessageMove, fields map[string]any, where string, args ...any) (moved bool, err error) {
	from, err := messageMoveFrom(move, fields)
	if err != nil {
		return false, err
	}
	set := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		set[k] = v
	}
	if to := move.To(); to != "" {
		set["status"] = to
	}
	q := tx.Model(&models.Message{}).Where("id = ? AND status IN ?", id, from)
	if where != "" {
		q = q.Where(where, args...)
	}
	res := q.Updates(set)
	return res.RowsAffected > 0, res.Error
}

// moveMessagesWhere moves every message that matches where by move in one
// statement, stamping updated_at with the transaction's time, and returns
// the ids it moved.
func moveMessagesWhere(tx *gorm.DB, move policy.MessageMove, where string, args ...any) ([]uint, error) {
	from, err := messageMoveFrom(move, nil)
	if err != nil {
		return nil, err
	}
	if move.To() == "" {
		return nil, fmt.Errorf("message move %q keeps the status, which moveMessagesWhere does not write", move)
	}
	var ids []uint
	err = tx.Raw(`UPDATE messages SET status = ?, updated_at = now() WHERE status IN ? AND (`+where+`) RETURNING id`,
		append([]any{move.To(), from}, args...)...).Scan(&ids).Error
	return ids, err
}

func messageMoveFrom(move policy.MessageMove, fields map[string]any) ([]string, error) {
	from := move.From()
	if len(from) == 0 {
		return nil, fmt.Errorf("message move %q starts from no status: a row it makes is inserted by its creator", move)
	}
	if _, ok := fields["status"]; ok {
		return nil, fmt.Errorf("message move %q: the move decides the status, not the caller's fields", move)
	}
	return from, nil
}
