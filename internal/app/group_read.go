package app

import (
	"errors"
	"net/http"
	"time"

	"fold/internal/policy"
	"fold/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// groupRead is one group read's snapshot: the transaction, its one clock, and
// the viewer, group and relationship judged in it.
type groupRead struct {
	tx     *gorm.DB
	now    time.Time
	viewer *models.User
	group  *models.Group
	rel    policy.Rel
}

// errGroupHidden is the group 404 (a missing group and one the viewer may not
// see answer alike), answered by answerGroupRead with jsonError.
var errGroupHidden = errors.New("group not found")

// readGroup runs read in one read-only Repeatable Read snapshot on the
// request's context, so the group, the viewer's standing in it and everything
// the read shows come from one committed state: a demotion, an erasure or a
// session ended between the middleware and the payload never pairs old
// authority with newer rows. The viewer is read again there (sessionCurrent):
// a session no longer current reads as signed out, or is errSecuritySession
// when required. The group and the relationship are read on the snapshot
// too, and a group the viewer may not see is errGroupHidden. read builds its
// answer on r.tx and r.now only; the caller writes it after the transaction
// commits. A participating request is acknowledged for the viewer judged
// here once the transaction has ended (decidingSnapshot), on its answer and on
// the refusals the snapshot decided whose rollback was confirmed (the hidden
// group's 404 alike for a missing group), never on a failure, a failed COMMIT
// or an unconfirmed rollback.
func (a *App) readGroup(c *gin.Context, id uint, required bool, read func(r groupRead) error) error {
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if a.hooks.groupReadBegin != nil {
			a.hooks.groupReadBegin()
		}
		now := a.now().UTC()
		viewer, err := snapshotViewerIn(tx, apiUser(c))
		if err != nil {
			return err
		}
		judged.judge(viewer)
		if required && viewer == nil {
			return errSecuritySession
		}
		g, rel, err := loadGroupIn(tx, viewer, id)
		if err != nil {
			return err
		}
		if err := read(groupRead{tx: tx, now: now, viewer: viewer, group: g, rel: rel}); err != nil {
			return err
		}
		if a.hooks.groupReadEnd != nil {
			a.hooks.groupReadEnd(tx)
		}
		return nil
	})
	var window errDateWindow
	decided := err == nil || errors.Is(err, errGroupHidden) || errors.As(err, &window) || decidedRefusal(err)
	if ackErr := a.acknowledgeDecision(c, judged, decided); ackErr != nil {
		return ackErr
	}
	return err
}

// groupReadAuthorized runs the readerAuthorized seam once a group read has
// judged what it may show.
func (a *App) groupReadAuthorized(surface string) {
	if a.hooks.readerAuthorized != nil {
		a.hooks.readerAuthorized(surface)
	}
}

// answerGroupRead answers a failed group read: the hidden-group 404, a
// malformed date window's 400, or fail.
func answerGroupRead(c *gin.Context, err error) {
	var window errDateWindow
	switch {
	case errors.Is(err, errGroupHidden):
		jsonError(c, http.StatusNotFound, "group not found")
	case errors.As(err, &window):
		jsonError(c, http.StatusBadRequest, string(window))
	default:
		fail(c, err)
	}
}

// loadGroupIn loads group id on db as u may see it, with u's standing there:
// errGroupHidden when it doesn't exist or u can't see it (an invite-only
// group never answers 403 to outsiders).
func loadGroupIn(db *gorm.DB, u *models.User, id uint) (*models.Group, policy.Rel, error) {
	var g models.Group
	if err := db.First(&g, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = errGroupHidden
		}
		return nil, policy.Rel{}, err
	}
	rel, err := viewerRelIn(db, u, g.ID)
	if err != nil {
		return nil, policy.Rel{}, err
	}
	if !policy.CanSeeGroup(asPerson(u), asGroup(&g), rel) {
		return nil, policy.Rel{}, errGroupHidden
	}
	return &g, rel, nil
}

// viewerRelIn is u's standing in group groupID on db; the empty relationship
// for a signed-out visitor.
func viewerRelIn(db *gorm.DB, u *models.User, groupID uint) (policy.Rel, error) {
	if u == nil {
		return policy.Rel{}, nil
	}
	return groupRel(db, u.ID, groupID)
}
