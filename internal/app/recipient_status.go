package app

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gorm.io/gorm"

	"fold/internal/policy"
)

// moveRecipientsWhere moves every message_recipients row that matches where
// by move, in one conditional UPDATE over the table as r: its WHERE is the
// move's from-set and channels (policy.RecipientMove) and where with args;
// it writes the move's status and fields together and returns how many rows
// moved, which each caller answers as before. fields must stamp updated_at,
// with the caller's own clock (a Go time or now()). This, moveRecipient, the
// two creators and the erasure's scrub (eraseMessagesIn) are the only writers
// of message_recipients.status (TestRecipientStatusHasOneWriter).
func moveRecipientsWhere(tx *gorm.DB, move policy.RecipientMove, fields map[string]any, where string, args ...any) (int64, error) {
	from, channels, err := recipientMoveFrom(move, fields)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(where) == "" {
		return 0, fmt.Errorf("recipient move %q: a set needs a condition", move)
	}
	cols := make([]string, 0, len(fields))
	for k := range fields {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	var set strings.Builder
	vars := make([]any, 0, len(cols)+1+len(args))
	vars = append(vars, move.To())
	for _, k := range cols {
		set.WriteString(", " + k + " = ?")
		vars = append(vars, fields[k])
	}
	// The from-set and channels are written as literals: a statement over one
	// status and channel then matches a partial index on them
	// (idx_message_recipients_push_queued) as the statements it replaced did.
	res := tx.Exec(`UPDATE message_recipients r SET status = ?`+set.String()+
		` WHERE r.status IN (`+from+`) AND r.channel IN (`+channels+`) AND (`+where+`)`, append(vars, args...)...)
	return res.RowsAffected, res.Error
}

// moveRecipient moves row id by move, as moveRecipientsWhere, when it also
// matches where (none when ""). moved is false when the row did not match.
func moveRecipient(tx *gorm.DB, id uint, move policy.RecipientMove, fields map[string]any, where string, args ...any) (moved bool, err error) {
	cond, all := "r.id = ?", append([]any{id}, args...)
	if where != "" {
		cond += " AND (" + where + ")"
	}
	n, err := moveRecipientsWhere(tx, move, fields, cond, all...)
	return n > 0, err
}

// recipientMoveColumns is every column a move may write beside the status.
// Naming them, and refusing the rest, keeps the status out of a caller's
// fields: a key is written into the SET list as given.
var recipientMoveColumns = map[string]bool{
	"reason": true, "error": true, "provider_message_id": true, "sent_at": true,
	"audience_admitted_at": true, "updated_at": true,
}

var recipientLiteralRe = regexp.MustCompile(`^[a-z_]+$`)

// recipientMoveFrom checks move and fields and returns the move's from-set
// and channels as SQL lists.
func recipientMoveFrom(move policy.RecipientMove, fields map[string]any) (from, channels string, err error) {
	if len(move.From()) == 0 {
		return "", "", fmt.Errorf("recipient move %q starts from no status: a row it makes is inserted by its creator", move)
	}
	for k := range fields {
		if !recipientMoveColumns[k] {
			return "", "", fmt.Errorf("recipient move %q: %q is not a column a move writes; the move decides the status", move, k)
		}
	}
	if _, ok := fields["updated_at"]; !ok {
		return "", "", fmt.Errorf("recipient move %q: every move stamps updated_at", move)
	}
	list := func(vals []string) (string, error) {
		quoted := make([]string, len(vals))
		for i, v := range vals {
			if !recipientLiteralRe.MatchString(v) {
				return "", fmt.Errorf("recipient move %q: %q can't be written as a literal", move, v)
			}
			quoted[i] = "'" + v + "'"
		}
		return strings.Join(quoted, ", "), nil
	}
	if from, err = list(move.From()); err != nil {
		return "", "", err
	}
	channels, err = list(move.Channels())
	return from, channels, err
}

// inDefaultTransaction runs fn as a gorm write on db runs: inside the
// transaction db is already in, or in one of its own when db is in none
// (gorm's default transaction). moveRecipientsWhere is a raw statement, which
// gorm does not wrap.
func inDefaultTransaction(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	switch db.Statement.ConnPool.(type) {
	case gorm.TxBeginner, gorm.ConnPoolBeginner:
		return db.Transaction(fn)
	}
	return fn(db)
}
