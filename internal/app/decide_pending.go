package app

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// errAlreadyHandled: a pending item (group, guardian link, account request)
// was decided by someone else first.
var errAlreadyHandled = apiErr(http.StatusConflict, "already_handled", "This was already handled")

// stillLive adds a condition to decidePendingIn's UPDATE for a queue whose
// rows lapse with the clock. The row stays pending until a sweep stamps it, so
// the UPDATE itself must refuse a lapsed one; refused is what the caller then
// gets instead of errAlreadyHandled.
type stillLive struct {
	cond    string
	refused error
}

// decidePendingIn is the one conditional UPDATE every approval queue shares: it
// moves the row from its pending status to the decided one only while it is
// still pending (and, with live, still within its condition), and answers
// errAlreadyHandled when another decision won.
func decidePendingIn(tx *gorm.DB, model any, id uint, pending string, updates map[string]any, live ...stillLive) error {
	q := tx.Model(model).Where("id = ? AND status = ?", id, pending)
	for _, l := range live {
		q = q.Where(l.cond)
	}
	res := q.Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		if len(live) > 0 {
			var open int64
			if err := tx.Model(model).Where("id = ? AND status = ?", id, pending).Count(&open).Error; err != nil {
				return err
			}
			if open > 0 {
				return live[0].refused
			}
		}
		return errAlreadyHandled
	}
	return nil
}

// answerAlreadyHandled answers 409 with who got there first when err says the
// item was already decided, and reports whether it did.
func (a *App) answerAlreadyHandled(c *gin.Context, err error, kind string, subjectID uint) bool {
	if !errors.Is(err, errAlreadyHandled) {
		return false
	}
	a.alreadyHandled(c, kind, subjectID)
	return true
}
