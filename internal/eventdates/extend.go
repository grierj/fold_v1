// Package eventdates keeps an event's occurrence rows in step with its rule:
// it generates them at creation and extends the rolling horizon daily. The
// clock itself (rules, wall times, expansion) is internal/events and has no
// database; this is the part that writes rows and takes the advisory lock.
package eventdates

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"time"

	"fold/internal/events"
	"fold/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LockID is the session advisory lock the daily occurrences.extend job takes.
// It is not the migration lock, so a migrate and an extend can overlap.
const LockID = 731002511

const insertBatch = 500

// idChunk bounds an IN list; repairBatch is how many events one repair query
// picks.
const (
	idChunk     = 1000
	repairBatch = 200
)

// AfterEnsure runs in the transaction that added an event's new dates
// (their original dates), so the app can book the series room for them
// (9.2) without this package knowing about rooms.
type AfterEnsure func(tx *gorm.DB, ev *models.Event, added []string) error

// Extend inserts any missing occurrences through the rolling horizon.
// Rows that already exist, including cancelled dates and per-date overrides,
// are left alone. Safe to run twice. after, if not nil, sees each event's
// new dates in the same transaction. It also settles each repeating event's
// clock evidence (settleClocks), and then repairs the evidence of ended
// series (RepairClocks), which it never extends.
func Extend(db *gorm.DB, now time.Time, after AfterEnsure) error {
	// A one-time event has all its dates from creation, and a series whose end
	// date has passed cannot make new ones, so neither is loaded.
	cutoff := liveCutoff(now)
	var evs []models.Event
	if err := db.Where("rrule <> '' AND (rrule_until IS NULL OR rrule_until >= ?)", cutoff).
		Order("id").Find(&evs).Error; err != nil {
		return err
	}
	// One bad event must not stop the rest from being extended.
	var errs []error
	for i := range evs {
		ev := &evs[i]
		err := db.Transaction(func(tx *gorm.DB) error {
			// Re-read under the event's lock: an edit that committed since the
			// list was read wins, and none runs while this adds its dates. One
			// that ended the series or made it one-time leaves nothing to add
			// (RepairClocks settles an ended series).
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(ev, ev.ID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil // deleted since
				}
				return err
			}
			if !live(ev, cutoff) {
				return nil
			}
			x, err := expansion(ev, now)
			if err != nil {
				return err
			}
			if err := dropDirtyClocks(tx, ev); err != nil {
				return err
			}
			added, err := ensure(tx, ev, x)
			if err != nil {
				return err
			}
			if err := settleClocks(tx, ev, x, now); err != nil {
				return err
			}
			if after == nil || len(added) == 0 {
				return nil
			}
			return after(tx, ev, added)
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, RepairClocks(db, now))
	return errors.Join(errs...)
}

// liveCutoff is the end date before which a series makes no new dates.
func liveCutoff(now time.Time) string {
	return models.FormatCalendarDate(now.AddDate(0, 0, -1))
}

// live is whether ev repeats and its end date is not before cutoff: the Go
// form of the selection Extend makes, rechecked under the event's lock.
func live(ev *models.Event, cutoff string) bool {
	return ev.RRule != "" && (ev.RRuleUntil == nil || models.FormatCalendarDate(*ev.RRuleUntil) >= cutoff)
}

func Ensure(db *gorm.DB, ev *models.Event, now time.Time) error {
	x, err := expansion(ev, now)
	if err != nil {
		return err
	}
	_, err = ensure(db, ev, x)
	return err
}

// expansion is the writer's walk of ev through its horizon at now.
func expansion(ev *models.Event, now time.Time) (events.Expansion, error) {
	until := ""
	if ev.RRuleUntil != nil {
		until = models.FormatCalendarDate(*ev.RRuleUntil)
	}
	start, _, err := events.ParseWall(ev.StartsLocal, ev.TZID)
	if err != nil {
		return events.Expansion{}, fmt.Errorf("event %d: %w", ev.ID, err)
	}
	x, err := events.Expand(events.Spec{
		StartsLocal: ev.StartsLocal, DurationMinutes: ev.DurationMinutes,
		TZID: ev.TZID, RRule: ev.RRule, Until: until,
	}, events.WindowEnd(start, now))
	if err != nil {
		return events.Expansion{}, fmt.Errorf("event %d: %w", ev.ID, err)
	}
	return x, nil
}

// ensure inserts the dates of x that ev does not have yet, with their clock
// evidence from the same walk, and returns the original dates it added.
func ensure(db *gorm.DB, ev *models.Event, x events.Expansion) ([]string, error) {
	// The dates come out as text from to_char, not date::text, which follows
	// the session's DateStyle.
	var dates []string
	if err := db.Model(&models.EventOccurrence{}).Where("event_id = ?", ev.ID).
		Pluck("to_char(original_date, 'YYYY-MM-DD')", &dates).Error; err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(dates))
	for _, d := range dates {
		have[d] = true
	}
	rows := make([]models.EventOccurrence, 0)
	for _, w := range x.Whens {
		if have[w.OriginalDate] {
			continue
		}
		date, err := models.ParseCalendarDate(w.OriginalDate)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", ev.ID, err)
		}
		rows = append(rows, models.EventOccurrence{
			EventID: ev.ID, OriginalDate: date,
			StartsAt: w.StartsAt, EndsAt: w.EndsAt,
			Status: models.OccurrenceScheduled,
		})
	}
	if len(rows) == 0 {
		return nil, nil
	}
	added := make([]string, len(rows))
	for i, r := range rows {
		added[i] = models.FormatCalendarDate(r.OriginalDate)
	}
	// A day with several times has a row per time here; the first in the
	// walk's order is inserted and the rest conflict.
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_id"}, {Name: "original_date"}},
		DoNothing: true,
	}).CreateInBatches(&rows, insertBatch).Error; err != nil {
		return nil, err
	}
	if ev.RRule == "" {
		return added, nil
	}
	// The ids RETURNING gives back skip the conflicting rows, so the dates'
	// rows are read again.
	ids, err := occurrenceIDs(db, ev.ID, added)
	if err != nil {
		return nil, err
	}
	return added, ReplaceClocks(db, x, ids, false)
}

// occurrenceIDs maps the given original dates of event eventID to their rows.
func occurrenceIDs(db *gorm.DB, eventID uint, days []string) (map[uint]string, error) {
	out := make(map[uint]string, len(days))
	for chunk := range slices.Chunk(days, idChunk) {
		var rows []struct {
			ID  uint
			Day string
		}
		if err := db.Model(&models.EventOccurrence{}).
			Select("id, to_char(original_date, 'YYYY-MM-DD') AS day").
			Where("event_id = ? AND original_date IN ?", eventID, chunk).Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ID] = r.Day
		}
	}
	return out, nil
}

// ExtendLocked runs Extend while holding LockID. A second caller, or a
// second app process, skips the work instead of waiting. The lock is taken
// on its own connection so it covers every query Extend makes on the pool.
func ExtendLocked(ctx context.Context, db *gorm.DB, now time.Time, after AfterEnsure) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", LockID).Scan(&got); err != nil {
		return err
	}
	if !got {
		return nil
	}
	// The request context may be cancelled by now; the unlock must still run.
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", LockID)
	return Extend(db, now, after)
}

// The clock evidence (DEC-155, models.OccurrenceSeriesStart). Every writer
// records it from the walk that wrote the dates, in the dates' transaction;
// the date page reads one row of it and never walks the rule.

// ReplaceClocks replaces the evidence of dates (original date by occurrence
// id) with what x proves for each. A date x does not prove loses its
// evidence, unless keepUnproven, when what it has stays: the caller vouches
// that it was written for this same schedule.
func ReplaceClocks(db *gorm.DB, x events.Expansion, dates map[uint]string, keepUnproven bool) error {
	if len(dates) == 0 {
		return nil
	}
	days := make([]string, 0, len(dates))
	ids := make([]uint, 0, len(dates))
	for id, day := range dates {
		days = append(days, day)
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	clocks := x.Clocks(days)
	tzid := x.TZID()
	var drop []uint
	var rows []models.OccurrenceSeriesStart
	for _, id := range ids {
		cs, ok := clocks[dates[id]]
		if !ok && keepUnproven {
			continue
		}
		drop = append(drop, id)
		for _, c := range cs {
			row := models.OccurrenceSeriesStart{OccurrenceID: id, StartsAt: c.StartsAt.UTC(), IsFirst: c.First, TZID: tzid}
			if c.WrittenLocal != "" {
				written := c.WrittenLocal
				row.WrittenLocal = &written
			}
			rows = append(rows, row)
		}
	}
	if err := DropDateClocks(db, drop); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return db.CreateInBatches(&rows, insertBatch).Error
}

// DropDateClocks removes the evidence of the given dates (occurrence ids).
func DropDateClocks(db *gorm.DB, ids []uint) error {
	for chunk := range slices.Chunk(ids, idChunk) {
		if err := db.Where("occurrence_id IN ?", chunk).Delete(&models.OccurrenceSeriesStart{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// DropClocks removes the evidence of every date of event eventID, past ones
// included: a writer that cannot vouch for it (a previous binary dirtied the
// event, series_clock_dirty) calls it before writing anything new.
func DropClocks(db *gorm.DB, eventID uint) error {
	return db.Exec(`DELETE FROM occurrence_series_starts s USING event_occurrences o
		WHERE o.id = s.occurrence_id AND o.event_id = ?`, eventID).Error
}

// ClearDirty marks event eventID's evidence current again, by an UPDATE of
// series_clock_dirty alone: no trigger fires and updated_at stays. Call it
// last, in the transaction that replaced or dropped the evidence.
func ClearDirty(db *gorm.DB, eventID uint) error {
	return db.Model(&models.Event{}).Where("id = ?", eventID).UpdateColumn("series_clock_dirty", false).Error
}

func dropDirtyClocks(tx *gorm.DB, ev *models.Event) error {
	if !ev.SeriesClockDirty {
		return nil
	}
	return DropClocks(tx, ev.ID)
}

// settleClocks fills the evidence of ev's upcoming scheduled dates that have
// none from x, the current walk, when that walk repeats the one that wrote
// them (x.Replayable), and then clears a dirty flag. The caller has the event
// locked and has dropped dirty evidence first. Dates already past keep what
// they have or stay unknown: nothing rebuilds the schedule they had. A date x
// does not prove stays unknown and is tried again on the next run.
func settleClocks(tx *gorm.DB, ev *models.Event, x events.Expansion, now time.Time) error {
	if ev.RRule != "" && x.Replayable() {
		var rows []struct {
			ID  uint
			Day string
		}
		if err := tx.Raw(`SELECT o.id, to_char(o.original_date, 'YYYY-MM-DD') AS day FROM event_occurrences o
			WHERE o.event_id = ? AND o.status = ? AND o.starts_at > ?
			AND NOT EXISTS (SELECT 1 FROM occurrence_series_starts s WHERE s.occurrence_id = o.id)
			ORDER BY o.id`, ev.ID, models.OccurrenceScheduled, now).Scan(&rows).Error; err != nil {
			return err
		}
		dates := make(map[uint]string, len(rows))
		unproven := 0
		for _, r := range rows {
			dates[r.ID] = r.Day
			if !x.Proves(r.Day) {
				unproven++
			}
		}
		if unproven > 0 {
			log.Printf("occurrences: event %d: %d upcoming dates past what the rule's walk proves; their usual clock stays unknown until a later run", ev.ID, unproven)
		}
		if err := ReplaceClocks(tx, x, dates, true); err != nil {
			return err
		}
	}
	if !ev.SeriesClockDirty {
		return nil
	}
	if err := ClearDirty(tx, ev.ID); err != nil {
		return err
	}
	ev.SeriesClockDirty = false
	return nil
}

// RepairClocks settles the evidence of repeating series Extend does not
// extend (their end date has passed) that are dirty or have an upcoming
// scheduled date with no evidence, in batches of repairBatch events, each
// under its event's lock. It adds no date and runs no AfterEnsure. A series
// whose rule no longer expands only loses dirty evidence; that is logged.
func RepairClocks(db *gorm.DB, now time.Time) error {
	cutoff := liveCutoff(now)
	var errs []error
	var after uint
	for {
		var ids []uint
		if err := db.Model(&models.Event{}).
			Where(`id > ? AND rrule <> '' AND rrule_until IS NOT NULL AND rrule_until < ?`, after, cutoff).
			Where(`series_clock_dirty OR EXISTS (SELECT 1 FROM event_occurrences o WHERE o.event_id = events.id
				AND o.status = ? AND o.starts_at > ?
				AND NOT EXISTS (SELECT 1 FROM occurrence_series_starts s WHERE s.occurrence_id = o.id))`,
				models.OccurrenceScheduled, now).
			Order("id").Limit(repairBatch).Pluck("id", &ids).Error; err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, id := range ids {
			errs = append(errs, db.Transaction(func(tx *gorm.DB) error {
				var ev models.Event
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ev, id).Error; err != nil {
					if errors.Is(err, gorm.ErrRecordNotFound) {
						return nil
					}
					return err
				}
				if ev.RRule == "" || live(&ev, cutoff) {
					return nil // no longer an ended series: Extend settles it
				}
				if err := dropDirtyClocks(tx, &ev); err != nil {
					return err
				}
				x, err := expansion(&ev, now)
				if err != nil {
					log.Printf("occurrences: %v; its usual clocks stay unknown", err)
					x = events.Expansion{}
				}
				return settleClocks(tx, &ev, x, now)
			}))
		}
		if len(ids) < repairBatch {
			return errors.Join(errs...)
		}
		after = ids[len(ids)-1]
	}
}
