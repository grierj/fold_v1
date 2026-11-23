package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fold/internal/testdb"
	"fold/models"

	"gorm.io/gorm"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func TestBackoff(t *testing.T) {
	mid := func() float64 { return 0.5 } // no jitter
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{{1, 30 * time.Second}, {2, time.Minute}, {3, 2 * time.Minute}, {7, 32 * time.Minute}, {8, time.Hour}, {50, time.Hour}, {0, 30 * time.Second}} {
		if got := Backoff(tc.attempt, mid); got != tc.want {
			t.Errorf("attempt %d: %v, want %v", tc.attempt, got, tc.want)
		}
	}
	if lo, hi := Backoff(3, func() float64 { return 0 }), Backoff(3, func() float64 { return 0.999999 }); lo != 96*time.Second || hi < 143*time.Second || hi > 144*time.Second {
		t.Errorf("jitter is ±20%%: %v .. %v", lo, hi)
	}
}

// enqueue queues one job of a throwaway type with the given attempts.
func enqueue(db *gorm.DB, kind string, payload any, at time.Time, key string, attempts int) (bool, error) {
	return Type{Name: kind, MaxAttempts: attempts}.Enqueue(db, payload, at, key)
}

func TestEnqueueDedupes(t *testing.T) {
	db := testdb.Shared(t)
	now := time.Now()
	if ok, err := enqueue(db, "x", map[string]int{"n": 1}, now, "x:1", 0); !ok || err != nil {
		t.Fatalf("first: %v %v", ok, err)
	}
	if ok, err := enqueue(db, "x", nil, now, "x:1", 0); ok || err != nil {
		t.Fatalf("a repeated key should add nothing: %v %v", ok, err)
	}
	for i := 0; i < 2; i++ {
		if ok, err := enqueue(db, "x", nil, now, "", 0); !ok || err != nil {
			t.Fatalf("no key never conflicts: %v %v", ok, err)
		}
	}
	var jobs []models.Job
	db.Order("id").Find(&jobs)
	if len(jobs) != 3 || jobs[0].Payload != `{"n": 1}` || jobs[0].MaxAttempts != DefaultMaxAttempts || jobs[0].Status != models.JobQueued {
		t.Fatalf("jobs: %+v", jobs)
	}
}

// EnqueueMany adds every new job in batched INSERTs and skips the keys
// some job already has, as Enqueue does one at a time.
func TestEnqueueManyDedupes(t *testing.T) {
	db := testdb.Shared(t)
	now := time.Now()
	if _, err := enqueue(db, "x", nil, now, "x:1", 0); err != nil {
		t.Fatal(err)
	}
	specs := []Spec{{Type: Type{Name: "x"}, RunAt: now, UniqueKey: "x:1"}}
	for i := 2; i <= 1200; i++ {
		specs = append(specs, Spec{Type: Type{Name: "x", MaxAttempts: 3}, Payload: map[string]int{"n": i}, RunAt: now, UniqueKey: "x:" + strconv.Itoa(i)})
	}
	n, err := EnqueueMany(db, specs)
	if err != nil || n != 1199 {
		t.Fatalf("added %d (%v), want 1199", n, err)
	}
	if n, err := EnqueueMany(db, specs); err != nil || n != 0 {
		t.Fatalf("again: added %d (%v)", n, err)
	}
	var j models.Job
	if db.Where("unique_key = ?", "x:7").First(&j); j.Payload != `{"n": 7}` || j.MaxAttempts != 3 || j.Status != models.JobQueued {
		t.Fatalf("job: %+v", j)
	}
	if n, err := EnqueueMany(db, nil); n != 0 || err != nil {
		t.Fatalf("none: %d %v", n, err)
	}
}

func runner(db *gorm.DB, kinds ...Kind) *Runner {
	r := NewRunner(db, kinds...)
	r.Rnd = func() float64 { return 0.5 }
	return r
}

func TestJobRunsOnceAndIsDone(t *testing.T) {
	db := testdb.Shared(t)
	var got string
	r := runner(db, Kind{Type: Type{Name: "hello"}, Run: func(_ context.Context, p json.RawMessage) error { got = string(p); return nil }})
	enqueue(db, "hello", map[string]string{"to": "you"}, time.Now(), "", 0)
	enqueue(db, "hello", nil, time.Now().Add(time.Hour), "", 0) // not due
	if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
		t.Fatalf("run: %v %v", ran, err)
	}
	if got != `{"to": "you"}` {
		t.Fatalf("payload %q", got)
	}
	if ran, _ := r.RunOnce(context.Background(), "w"); ran {
		t.Fatal("a job not yet due ran")
	}
	var j models.Job
	db.Order("id").First(&j)
	if j.Status != models.JobDone || j.Attempts != 1 || j.LockedAt != nil {
		t.Fatalf("job: %+v", j)
	}
}

// A handler learns when it is on its last attempt, so it can record the
// failure itself before giving up (M3.5).
func TestLastAttempt(t *testing.T) {
	db := testdb.Shared(t)
	var seen []bool
	r := runner(db, Kind{Type: Type{Name: "count"}, Run: func(ctx context.Context, _ json.RawMessage) error {
		seen = append(seen, LastAttempt(ctx))
		return errors.New("again")
	}})
	enqueue(db, "count", nil, time.Now(), "", 3)
	for range 3 {
		db.Model(&models.Job{}).Where("status = ?", models.JobFailed).Update("run_at", time.Now().Add(-time.Second))
		if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
			t.Fatalf("run: %v %v", ran, err)
		}
	}
	if len(seen) != 3 || seen[0] || seen[1] || !seen[2] {
		t.Fatalf("last attempt seen as %v", seen)
	}
	if LastAttempt(context.Background()) {
		t.Fatal("outside a job it is never the last attempt")
	}
}

// A handler that returns Later waits until then without using an attempt
// or recording an error, even on what would be its last attempt, and
// runs from the top when it is due.
func TestLaterWaitsWithoutUsingAnAttempt(t *testing.T) {
	db := testdb.Shared(t)
	calls := 0
	at := time.Now().Add(9 * time.Hour).Truncate(time.Second)
	r := runner(db, Kind{Type: Type{Name: "wait"}, Run: func(context.Context, json.RawMessage) error {
		calls++
		if calls <= 3 {
			return fmt.Errorf("quiet hours: %w", Later(at))
		}
		return nil
	}})
	enqueue(db, "wait", nil, time.Now(), "wait:1", 1)
	for i := 1; i <= 3; i++ {
		if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
			t.Fatalf("run %d: %v %v", i, ran, err)
		}
		var j models.Job
		db.First(&j)
		if j.Status != models.JobQueued || j.Attempts != 0 || j.LastError != "" || !j.RunAt.Equal(at) || j.LockedAt != nil {
			t.Fatalf("run %d: %+v", i, j)
		}
		if ran, _ := r.RunOnce(context.Background(), "w"); ran {
			t.Fatal("a waiting job ran early")
		}
		db.Model(&j).Update("run_at", time.Now().Add(-time.Second))
	}
	if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
		t.Fatalf("last run: %v %v", ran, err)
	}
	var j models.Job
	db.First(&j)
	if j.Status != models.JobDone || j.Attempts != 1 || calls != 4 {
		t.Fatalf("after it was due: %+v, %d calls", j, calls)
	}
}

// A failing job retries with backoff and is dead after max_attempts.
func TestFailingJobRetriesThenDies(t *testing.T) {
	db := testdb.Shared(t)
	calls := 0
	r := runner(db, Kind{Type: Type{Name: "flaky"}, Run: func(context.Context, json.RawMessage) error { calls++; return errors.New("provider down") }})
	enqueue(db, "flaky", nil, time.Now(), "", 3)
	for attempt := 1; attempt <= 3; attempt++ {
		if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
			t.Fatalf("attempt %d: %v %v", attempt, ran, err)
		}
		var j models.Job
		db.First(&j)
		if attempt < 3 {
			wait := time.Until(j.RunAt)
			want := Backoff(attempt, r.Rnd)
			if j.Status != models.JobFailed || j.LastError != "provider down" || wait < want-5*time.Second || wait > want {
				t.Fatalf("attempt %d: %+v (waits %v, want %v)", attempt, j, wait, want)
			}
			if ran, _ := r.RunOnce(context.Background(), "w"); ran {
				t.Fatal("a job in backoff ran early")
			}
			db.Model(&j).Update("run_at", time.Now().Add(-time.Second))
		} else if j.Status != models.JobDead || j.Attempts != 3 {
			t.Fatalf("after the last attempt: %+v", j)
		}
	}
	if ran, _ := r.RunOnce(context.Background(), "w"); ran || calls != 3 {
		t.Fatalf("a dead job ran again: %d calls", calls)
	}
}

// A panic is a failure, never a crashed worker. A kind this build doesn't
// know waits (an upgrade rolled back), and is dead only after a week.
func TestPanicsAndUnknownKinds(t *testing.T) {
	db := testdb.Shared(t)
	r := runner(db, Kind{Type: Type{Name: "boom", MaxAttempts: 2}, Run: func(context.Context, json.RawMessage) error { panic("oops") }})
	enqueue(db, "boom", nil, time.Now(), "", 2)
	enqueue(db, "nobody", nil, time.Now(), "", 5)
	r.RunOnce(context.Background(), "w")
	r.RunOnce(context.Background(), "w")
	var boom, nobody models.Job
	db.Where("kind = ?", "boom").First(&boom)
	db.Where("kind = ?", "nobody").First(&nobody)
	if boom.Status != models.JobFailed || boom.LastError != "panic: oops" {
		t.Fatalf("boom: %+v", boom)
	}
	if nobody.Status != models.JobQueued || nobody.Attempts != 0 || time.Until(nobody.RunAt) < 50*time.Minute {
		t.Fatalf("an unknown kind waits for a build that knows it, without using an attempt: %+v", nobody)
	}
	// After a week it is dead.
	db.Model(&nobody).Updates(map[string]any{"created_at": time.Now().AddDate(0, 0, -8), "run_at": time.Now()})
	r.RunOnce(context.Background(), "w")
	db.First(&nobody, nobody.ID)
	if nobody.Status != models.JobDead {
		t.Fatalf("an unknown kind after a week: %+v", nobody)
	}
}

// A job whose worker stopped mid-run is taken back after its lease; one
// that had used its last attempt is dead instead of running again. A slow
// worker whose lease ran out can't overwrite the new holder's outcome.
func TestExpiredLeases(t *testing.T) {
	db := testdb.Shared(t)
	runs := 0
	r := runner(db, Kind{Type: Type{Name: "work"}, Run: func(context.Context, json.RawMessage) error { runs++; return nil }})
	r.Lease = time.Minute
	enqueue(db, "work", nil, time.Now(), "a", 3)
	enqueue(db, "work", nil, time.Now(), "b", 1)
	stale := time.Now().Add(-2 * time.Minute)
	goneA := holdByHand(t, db, "a", "gone", stale)
	holdByHand(t, db, "b", "gone", stale)
	r.RunOnce(context.Background(), "w")
	r.RunOnce(context.Background(), "w")
	var a, b models.Job
	db.Where("unique_key = ?", "a").First(&a)
	db.Where("unique_key = ?", "b").First(&b)
	if a.Status != models.JobDone || a.Attempts != 2 || runs != 1 {
		t.Fatalf("taken back: %+v (%d runs)", a, runs)
	}
	if b.Status != models.JobDead || b.LastError != "the worker stopped while running it" {
		t.Fatalf("out of attempts: %+v", b)
	}
	// The old holder finishing late changes nothing.
	if err := r.finish(claimed{ID: a.ID, ClaimSeq: goneA, Attempts: 1, MaxAttempts: 3}, errors.New("late"), false); err != nil {
		t.Fatal(err)
	}
	db.First(&a, a.ID)
	if a.Status != models.JobDone {
		t.Fatalf("a late finish overwrote the outcome: %+v", a)
	}
}

type deadCall struct {
	id       uint
	kind     string
	attempts int
	text     string
}

// OnDead fires once, with the job's id, kind, attempts and cleaned error,
// when a job dies; a retrying failure and a Later never call it.
func TestOnDeadFiresOnceWhenAJobDies(t *testing.T) {
	db := testdb.Shared(t)
	var calls []deadCall
	r := runner(db,
		Kind{Type: Type{Name: "die"}, Run: func(context.Context, json.RawMessage) error { return errors.New("provider down\nsecond line") }},
		Kind{Type: Type{Name: "retry"}, Run: func(context.Context, json.RawMessage) error { return errors.New("try later") }},
		Kind{Type: Type{Name: "wait"}, Run: func(context.Context, json.RawMessage) error { return Later(time.Now().Add(time.Hour)) }},
	)
	r.OnDead = func(id uint, kind string, attempts int, errText string) {
		calls = append(calls, deadCall{id, kind, attempts, errText})
	}
	enqueue(db, "retry", nil, time.Now().Add(-3*time.Second), "", 3)
	enqueue(db, "wait", nil, time.Now().Add(-2*time.Second), "", 1)
	enqueue(db, "die", nil, time.Now().Add(-time.Second), "", 1)
	for range 3 {
		if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
			t.Fatalf("run: %v %v", ran, err)
		}
	}
	var die models.Job
	db.Where("kind = ?", "die").First(&die)
	if die.Status != models.JobDead {
		t.Fatalf("die: %+v", die)
	}
	want := deadCall{die.ID, "die", 1, "provider down"}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("hook calls %+v, want just %+v", calls, want)
	}
	// Nothing is due now (one in backoff, one waiting), and a dead job is not run again.
	if ran, _ := r.RunOnce(context.Background(), "w"); ran || len(calls) != 1 {
		t.Fatalf("ran %v, hook calls %+v", ran, calls)
	}
}

// A job whose worker stopped during its last attempt dies on the next claim,
// which counts one more attempt in the row but never runs; OnDead reports the
// attempts that ran.
func TestOnDeadCountsTheAttemptsThatRan(t *testing.T) {
	db := testdb.Shared(t)
	var calls []deadCall
	r := runner(db, Kind{Type: Type{Name: "stopped"}, Run: func(context.Context, json.RawMessage) error {
		t.Error("a claim past the last attempt ran the handler")
		return nil
	}})
	r.Lease = time.Minute
	r.OnDead = func(id uint, kind string, attempts int, errText string) {
		calls = append(calls, deadCall{id, kind, attempts, errText})
	}
	enqueue(db, "stopped", nil, time.Now(), "stopped", 1)
	holdByHand(t, db, "stopped", "gone", time.Now().Add(-2*time.Minute))
	if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
		t.Fatalf("run: %v %v", ran, err)
	}
	var j models.Job
	db.Where("unique_key = ?", "stopped").First(&j)
	if j.Status != models.JobDead || j.Attempts != 2 {
		t.Fatalf("stopped job: %+v", j)
	}
	want := deadCall{j.ID, "stopped", 1, "the worker stopped while running it"}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("hook calls %+v, want just %+v", calls, want)
	}
}

// A late finish by a worker that lost its lease lands no rows, so the job is
// not announced dead by it; the holder that landed the change is.
func TestOnDeadSkippedWhenLeaseLost(t *testing.T) {
	db := testdb.Shared(t)
	var calls []deadCall
	r := runner(db)
	r.OnDead = func(id uint, kind string, attempts int, errText string) {
		calls = append(calls, deadCall{id, kind, attempts, errText})
	}
	enqueue(db, "work", nil, time.Now(), "work:1", 1)
	gone := holdByHand(t, db, "work:1", "gone", time.Now())
	holder := holdByHand(t, db, "work:1", "holder", time.Now())
	var j models.Job
	db.First(&j)
	job := claimed{ID: j.ID, ClaimSeq: gone, Kind: "work", Attempts: 1, MaxAttempts: 1}
	if err := r.finish(job, errors.New("late"), false); err != nil {
		t.Fatal(err)
	}
	db.First(&j, j.ID)
	if len(calls) != 0 || j.Status != models.JobRunning {
		t.Fatalf("a lost lease announced a death: %+v, %+v", calls, j)
	}
	job.ClaimSeq = holder
	if err := r.finish(job, errors.New("boom"), false); err != nil {
		t.Fatal(err)
	}
	db.First(&j, j.ID)
	if want := (deadCall{j.ID, "work", 1, "boom"}); j.Status != models.JobDead || len(calls) != 1 || calls[0] != want {
		t.Fatalf("holder finish: %+v, %+v", calls, j)
	}
}

// A hook that panics is logged and nothing more: the job stays dead, finish
// returns no error and the worker goes on to the next job.
func TestOnDeadPanicDoesNotStopWorker(t *testing.T) {
	db := testdb.Shared(t)
	ran := 0
	r := runner(db,
		Kind{Type: Type{Name: "die"}, Run: func(context.Context, json.RawMessage) error { return errors.New("nope") }},
		Kind{Type: Type{Name: "ok"}, Run: func(context.Context, json.RawMessage) error { ran++; return nil }},
	)
	hook := 0
	r.OnDead = func(uint, string, int, string) { hook++; panic("hook broke") }
	enqueue(db, "die", nil, time.Now().Add(-time.Second), "", 1)
	enqueue(db, "ok", nil, time.Now(), "", 1)
	for i := range 2 {
		if got, err := r.RunOnce(context.Background(), "w"); !got || err != nil {
			t.Fatalf("run %d: %v %v", i, got, err)
		}
	}
	var die, ok models.Job
	db.Where("kind = ?", "die").First(&die)
	db.Where("kind = ?", "ok").First(&ok)
	if hook != 1 || die.Status != models.JobDead || ok.Status != models.JobDone || ran != 1 {
		t.Fatalf("hook %d, die %+v, ok %+v", hook, die, ok)
	}
}

// Six workers (app processes) contend for 200 jobs: every job runs exactly
// once (FOR UPDATE SKIP LOCKED).
func TestTwoWorkersNeverRunAJobTwice(t *testing.T) {
	db := testdb.Shared(t)
	var mu sync.Mutex
	seen := map[int]int{}
	kind := Kind{Type: Type{Name: "count"}, Run: func(_ context.Context, p json.RawMessage) error {
		time.Sleep(2 * time.Millisecond) // long enough for workers to contend
		var body struct{ N int }
		_ = json.Unmarshal(p, &body)
		mu.Lock()
		seen[body.N]++
		mu.Unlock()
		return nil
	}}
	for i := 0; i < 200; i++ {
		if _, err := enqueue(db, "count", map[string]int{"n": i}, time.Now(), fmt.Sprintf("count:%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, name := range []string{"process-1", "process-2", "process-3", "process-4", "process-5", "process-6"} {
		r := runner(db, kind)
		r.ID = name
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ran, err := r.RunOnce(context.Background(), r.ID)
				if err != nil {
					t.Error(err)
					return
				}
				if !ran {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != 200 {
		t.Fatalf("ran %d distinct jobs, want 200", len(seen))
	}
	for n, c := range seen {
		if c != 1 {
			t.Fatalf("job %d ran %d times", n, c)
		}
	}
	var by []struct {
		LockedBy string
		N        int
	}
	db.Raw("SELECT locked_by, count(*) AS n FROM jobs WHERE status = 'done' GROUP BY locked_by").Scan(&by)
	if len(by) != 6 {
		t.Fatalf("want every process to have claimed jobs: %+v", by)
	}
}

// A worker that has gone idle picks up a job queued later, by polling.
func TestIdleWorkerPicksUpNewJob(t *testing.T) {
	db := testdb.Shared(t)
	started := make(chan int, 2)
	r := runner(db, Kind{Type: Type{Name: "ping"}, Run: func(_ context.Context, p json.RawMessage) error {
		var body struct{ N int }
		_ = json.Unmarshal(p, &body)
		started <- body.N
		return nil
	}})
	r.Poll = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	wait := r.Start(ctx, 1)
	defer func() { cancel(); wait() }()
	// The first job has run, so the worker is back to waiting out its poll
	// when the second is queued. The timeout only bounds a hang.
	for n := 1; n <= 2; n++ {
		if _, err := enqueue(db, "ping", map[string]int{"n": n}, time.Now(), "", 0); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-started:
			if got != n {
				t.Fatalf("started job %d, want %d", got, n)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("job %d never started", n)
		}
	}
}

func TestPruneDone(t *testing.T) {
	db := testdb.Shared(t)
	for _, s := range []string{models.JobDone, models.JobDismissed, models.JobDead, models.JobQueued} {
		enqueue(db, s, nil, time.Now(), "", 0)
		db.Model(&models.Job{}).Where("kind = ?", s).Updates(map[string]any{"status": s, "updated_at": time.Now().AddDate(0, 0, -40)})
	}
	n, err := PruneDone(db, time.Now().AddDate(0, 0, -30))
	var left int64
	db.Model(&models.Job{}).Count(&left)
	if err != nil || n != 2 || left != 2 {
		t.Fatalf("pruned %d (%v), %d left", n, err, left)
	}
}

func TestCleanError(t *testing.T) {
	for in, want := range map[string]string{
		"550 <lee@example.org>: rejected\nmore detail": "550 <[email]>: rejected",
		"bad\x00byte":        "badbyte",
		"connection refused": "connection refused",
		// A push endpoint's path is the device's secret (M3.13).
		`Post "https://fcm.googleapis.com/fcm/send/dQw4w9WgXcQ:APA91bH": EOF`:         `Post "https://fcm.googleapis.com/[path]": EOF`,
		"to https://user:pw@push.example.org:443/x?token=abc failed":                  "to https://push.example.org:443/[path] failed",
		"key BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4b":           "key [token]",
		"vapid t=eyJ0eXAiOiJKV1QiLCJhbGciOiJFUzI1NiJ9.eyJhdWQiOiJodHRwcyJ9.c2ln, k=x": "vapid t=[token], k=x",
	} {
		if got := CleanError(errors.New(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestTypedNilPayloadIsAnEmptyObject(t *testing.T) {
	db := testdb.Shared(t)
	var none map[string]string
	enqueue(db, "x", none, time.Now(), "", 0)
	var j models.Job
	db.First(&j)
	if j.Payload != "{}" {
		t.Fatalf("payload %q", j.Payload)
	}
}

// holdByHand puts the job with key in the state a claim by worker leaves it,
// locked at lockedAt on its first attempt, with a new claim number, which it
// returns.
func holdByHand(t *testing.T, db *gorm.DB, key, worker string, lockedAt time.Time) int64 {
	t.Helper()
	var seq int64
	if err := db.Raw(`UPDATE jobs SET status = ?, attempts = 1, locked_at = ?, locked_by = ?, claim_seq = nextval('jobs_claim_seq')
		WHERE unique_key = ? RETURNING claim_seq`, models.JobRunning, lockedAt, worker, key).Scan(&seq).Error; err != nil || seq == 0 {
		t.Fatalf("holding %s: %d, %v", key, seq, err)
	}
	return seq
}

// logLines captures the log until the test ends and returns the lines so far
// that contain what.
func logLines(t *testing.T) func(what string) []string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prev) })
	return func(what string) []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, what) {
				out = append(out, l)
			}
		}
		return out
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

const lostLease = "ran past its lease"

// stepper hands each run of a test kind its own behavior, in claim order, and
// lets the test hold a run at a point and let it go.
type stepper struct {
	mu    sync.Mutex
	runs  []func(ctx context.Context) error
	calls int
}

func (s *stepper) run(ctx context.Context, _ json.RawMessage) error {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()
	if i >= len(s.runs) {
		return fmt.Errorf("unexpected run %d", i+1)
	}
	return s.runs[i](ctx)
}

func (s *stepper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// gate is one point a run stops at: reached closes when it gets there and the
// run waits until release closes (the cleanup closes it).
type gate struct{ reached, release chan struct{} }

func newGate(t *testing.T) gate {
	g := gate{reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	})
	return g
}

func (g gate) stop() { close(g.reached); <-g.release }

func (g gate) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func runAsync(r *Runner, worker string) <-chan error {
	done := make(chan error, 1)
	go func() {
		ran, err := r.RunOnce(context.Background(), worker)
		if err == nil && !ran {
			err = errors.New("found no job to claim")
		}
		done <- err
	}()
	return done
}

func jobByKey(t *testing.T, db *gorm.DB, key string) models.Job {
	t.Helper()
	var j models.Job
	if err := db.Where("unique_key = ?", key).First(&j).Error; err != nil {
		t.Fatal(err)
	}
	return j
}

// sameClaim fails unless the job is still exactly as claim left it.
func sameClaim(t *testing.T, what string, claim, now models.Job) {
	t.Helper()
	if now.Status != models.JobRunning || now.ClaimSeq == nil || claim.ClaimSeq == nil || *now.ClaimSeq != *claim.ClaimSeq ||
		now.LockedBy != claim.LockedBy || now.Attempts != claim.Attempts || !now.RunAt.Equal(claim.RunAt) ||
		now.LockedAt == nil || !now.LockedAt.Equal(*claim.LockedAt) || now.LastError != claim.LastError || now.Payload != claim.Payload {
		t.Fatalf("%s changed the current claim: was %+v (claim %v), now %+v (claim %v)", what, claim, deref(claim.ClaimSeq), now, deref(now.ClaimSeq))
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// waitIn writes a mark on the job and queues it again for at in one
// transaction, as a handler that goes back to wait does (jobs.LaterIn).
func waitIn(ctx context.Context, db *gorm.DB, key string, at time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE jobs SET payload = '{"waited": true}' WHERE unique_key = ?`, key).Error; err != nil {
			return err
		}
		return LaterIn(ctx, tx, at)
	})
}

// LaterIn queues the handler's own claim again in the handler's transaction,
// giving its attempt back; the handler then returns Requeued and the runner
// writes and logs nothing. Outside a claimed job LaterIn refuses.
func TestLaterInQueuesItsOwnClaim(t *testing.T) {
	db := testdb.Shared(t)
	lines := logLines(t)
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	r := runner(db, Kind{Type: Type{Name: "wait"}, Run: func(ctx context.Context, _ json.RawMessage) error {
		if err := waitIn(ctx, db, "wait:1", at); err != nil {
			return err
		}
		return Requeued()
	}})
	enqueue(db, "wait", nil, time.Now().Add(-time.Second), "wait:1", 2)
	if ran, err := r.RunOnce(context.Background(), "w"); !ran || err != nil {
		t.Fatalf("run: %v %v", ran, err)
	}
	j := jobByKey(t, db, "wait:1")
	if j.Status != models.JobQueued || j.Attempts != 0 || !j.RunAt.Equal(at) || j.LockedAt != nil || j.Payload != `{"waited": true}` || j.LastError != "" {
		t.Fatalf("after its own wait: %+v", j)
	}
	if got := lines("jobs:"); len(got) != 0 {
		t.Fatalf("a committed wait logged %q", got)
	}
	if err := LaterIn(context.Background(), db, at); err == nil || errors.Is(err, ErrClaimLost) {
		t.Fatalf("outside a claimed job: %v", err)
	}
}

// A handler commits its wait for a time already due and is slow to return;
// meanwhile a worker with the same label claims the job again, with the same
// attempt count (the wait gave its attempt back), and is still running it
// when the first handler returns. Whatever the first handler returns, its
// finish lands nothing: the second claim keeps the job, its attempt and its
// lease, no third claim can take it, and the job runs twice in all. Requeued,
// the usual return, logs nothing; anything else is a lost claim and says so.
func TestARequeuedHandlerNeverTouchesTheNextClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		returns func() error
		logs    bool
	}{
		{"Requeued", Requeued, false},
		{"Later", func() error { return Later(time.Now().Add(time.Hour)) }, true},
		{"success", func() error { return nil }, true},
		{"failure", func() error { return errors.New("boom") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Shared(t)
			lines := logLines(t)
			first, second := newGate(t), newGate(t)
			steps := &stepper{runs: []func(ctx context.Context) error{
				func(ctx context.Context) error {
					if err := waitIn(ctx, db, "wait:1", time.Now().Add(-time.Second)); err != nil {
						return err
					}
					first.stop()
					return tc.returns()
				},
				func(context.Context) error { second.stop(); return nil },
			}}
			kind := Kind{Type: Type{Name: "wait"}, Run: steps.run}
			enqueue(db, "wait", nil, time.Now().Add(-time.Second), "wait:1", 5)

			firstDone := runAsync(runner(db, kind), "w")
			first.await(t, "the first handler's wait to commit")
			secondDone := runAsync(runner(db, kind), "w")
			second.await(t, "the same worker label to claim the job again")
			claim := jobByKey(t, db, "wait:1")
			if claim.Status != models.JobRunning || claim.LockedBy != "w" || claim.Attempts != 1 {
				t.Fatalf("the second claim: %+v", claim)
			}

			close(first.release)
			if err := <-firstDone; err != nil {
				t.Fatalf("the first worker: %v", err)
			}
			sameClaim(t, "the first handler's finish", claim, jobByKey(t, db, "wait:1"))
			if ran, err := runner(db, kind).RunOnce(context.Background(), "w"); ran || err != nil {
				t.Fatalf("a third claim while the second runs: ran %v, %v", ran, err)
			}
			if got := lines(lostLease); tc.logs != (len(got) == 1) || len(got) > 1 {
				t.Fatalf("lost-claim log lines %q, want one: %v", got, tc.logs)
			}

			close(second.release)
			if err := <-secondDone; err != nil {
				t.Fatal(err)
			}
			if j := jobByKey(t, db, "wait:1"); j.Status != models.JobDone || j.Attempts != 1 || steps.count() != 2 {
				t.Fatalf("after the second claim: %+v, %d runs", j, steps.count())
			}
		})
	}
}

// A slow handler's lease runs out and the same worker label takes the job
// back; that claim fails on the job's last attempt, an admin retries it and
// the label claims it again on its first attempt, matching the slow handler's
// worker and attempt count. When the slow handler reaches its wait, LaterIn
// finds its claim gone, its transaction rolls back and its finish records
// nothing; the current claim keeps the job.
func TestAStaleClaimCannotWaitAfterAnAdminRetry(t *testing.T) {
	db := testdb.Shared(t)
	lines := logLines(t)
	slow, current := newGate(t), newGate(t)
	steps := &stepper{runs: []func(ctx context.Context) error{
		func(ctx context.Context) error {
			slow.stop()
			if err := waitIn(ctx, db, "wait:1", time.Now().Add(time.Hour)); err != nil {
				return err
			}
			return Requeued()
		},
		func(context.Context) error { return errors.New("boom") },
		func(context.Context) error { current.stop(); return nil },
	}}
	kind := Kind{Type: Type{Name: "wait"}, Run: steps.run}
	enqueue(db, "wait", nil, time.Now().Add(-time.Second), "wait:1", 2)

	slowDone := runAsync(runner(db, kind), "w")
	slow.await(t, "the slow handler to start")
	takeBack := runner(db, kind)
	takeBack.Lease = time.Nanosecond
	if err := <-runAsync(takeBack, "w"); err != nil {
		t.Fatal(err)
	}
	if j := jobByKey(t, db, "wait:1"); j.Status != models.JobDead || j.Attempts != 2 {
		t.Fatalf("after the claim that took it back: %+v", j)
	}
	if ok, err := Retry(db, jobByKey(t, db, "wait:1").ID); !ok || err != nil {
		t.Fatalf("retry: %v %v", ok, err)
	}
	currentDone := runAsync(runner(db, kind), "w")
	current.await(t, "the retried job's claim")
	claim := jobByKey(t, db, "wait:1")
	if claim.Status != models.JobRunning || claim.LockedBy != "w" || claim.Attempts != 1 {
		t.Fatalf("the retried claim: %+v", claim)
	}

	close(slow.release)
	if err := <-slowDone; err != nil {
		t.Fatalf("the slow worker: %v", err)
	}
	sameClaim(t, "the stale handler's wait", claim, jobByKey(t, db, "wait:1"))
	if got := lines(lostLease); len(got) != 1 {
		t.Fatalf("lost-claim log lines %q, want one", got)
	}
	close(current.release)
	if err := <-currentDone; err != nil {
		t.Fatal(err)
	}
	if j := jobByKey(t, db, "wait:1"); j.Status != models.JobDone || j.Attempts != 1 || j.Payload != "{}" || steps.count() != 3 {
		t.Fatalf("after the current claim: %+v, %d runs", j, steps.count())
	}
}

// A handler whose lease ran out while the same worker label took the job back
// returns an ordinary Later (it waited without LaterIn). Its finish lands
// nothing on the claim that holds the job, and the runner logs the lost lease
// once.
func TestALostLeaseLaterIsLoggedAndLandsNothing(t *testing.T) {
	db := testdb.Shared(t)
	lines := logLines(t)
	slow, current := newGate(t), newGate(t)
	steps := &stepper{runs: []func(ctx context.Context) error{
		func(context.Context) error { slow.stop(); return Later(time.Now().Add(time.Hour)) },
		func(context.Context) error { current.stop(); return nil },
	}}
	kind := Kind{Type: Type{Name: "wait"}, Run: steps.run}
	enqueue(db, "wait", nil, time.Now().Add(-time.Second), "wait:1", 5)

	slowDone := runAsync(runner(db, kind), "w")
	slow.await(t, "the slow handler to start")
	takeBack := runner(db, kind)
	takeBack.Lease = time.Nanosecond
	currentDone := runAsync(takeBack, "w")
	current.await(t, "the same label to take the job back")
	claim := jobByKey(t, db, "wait:1")

	close(slow.release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
	sameClaim(t, "the lost lease's Later", claim, jobByKey(t, db, "wait:1"))
	if got := lines(lostLease); len(got) != 1 {
		t.Fatalf("lost-claim log lines %q, want one", got)
	}
	close(current.release)
	if err := <-currentDone; err != nil {
		t.Fatal(err)
	}
	if j := jobByKey(t, db, "wait:1"); j.Status != models.JobDone || j.Attempts != 2 {
		t.Fatalf("after the current claim: %+v", j)
	}
}
