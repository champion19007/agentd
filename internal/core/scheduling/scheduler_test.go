// These tests live inside the package so they can exercise nextWake directly.
// Pacing is the kind of logic that is easy to get subtly wrong and hard to
// notice in production -- a loop that wakes slightly too often just looks like
// a busy process -- so it is worth testing at the unit rather than only
// through Loop.
package scheduling

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fixedRandom returns the same number every time, so that jitter is a
// deterministic multiplier rather than a source of flakiness.
type fixedRandom float64

func (r fixedRandom) Float64() float64 { return float64(r) }

// testClock is a clock that never really sleeps. It advances its own notion of
// now by whatever it was asked to wait for, records the request, and cancels
// the loop after a set number of passes. Simulated time is what lets a
// scheduler test cover hours of behaviour in microseconds.
type testClock struct {
	now    time.Time
	slept  []time.Duration
	budget int
	cancel context.CancelFunc
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	c.budget--
	if c.budget <= 0 {
		c.cancel()
		return context.Canceled
	}
	return ctx.Err()
}

// fakeStore implements only the methods the scheduler calls.
type fakeStore struct {
	ports.Store
	checks         []*domain.Check
	ran            map[domain.RunKey]bool
	enabledErr     error
	runForSlotErr  error
	recentRunsErr  error
}

func (s *fakeStore) EnabledChecks(context.Context) ([]*domain.Check, error) {
	if s.enabledErr != nil {
		return nil, s.enabledErr
	}
	return s.checks, nil
}

func (s *fakeStore) RunForSlot(_ context.Context, id domain.CheckID, slot domain.Slot) (*domain.Run, error) {
	if s.runForSlotErr != nil {
		return nil, s.runForSlotErr
	}
	if s.ran[domain.RunKey{CheckID: id, Slot: slot}] {
		return &domain.Run{}, nil
	}
	return nil, ports.ErrNotFound
}

func (s *fakeStore) RecentRuns(_ context.Context, id domain.CheckID, limit int) ([]*domain.Run, error) {
	if s.recentRunsErr != nil {
		return nil, s.recentRunsErr
	}
	var out []*domain.Run
	for k, ran := range s.ran {
		if k.CheckID == id && ran {
			r, _ := domain.NewRun(domain.RunID("run-"+string(id)), id, k.Slot, 1, base)
			_ = r.Quiet(base, "snap", domain.Extraction{Kind: domain.IntentScalar})
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot() > out[j].Slot() })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// recorder is a Runner that remembers what it was asked to run.
type recorder struct {
	calls  []domain.RunKey
	runErr error
}

func (r *recorder) Run(_ context.Context, c *domain.Check, slot domain.Slot) error {
	if r.runErr != nil {
		return r.runErr
	}
	r.calls = append(r.calls, domain.RunKey{CheckID: c.ID(), Slot: slot})
	return nil
}

// check builds an enabled check with the given id and interval.
func check(t *testing.T, id domain.CheckID, interval time.Duration) *domain.Check {
	t.Helper()
	c, err := domain.NewCheck(id, domain.Definition{
		Intent: domain.ScalarIntent{
			Label:   "price",
			Purpose: "the advertised price",
			Type:    domain.TypeNumber,
		},
		Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
		Schedule:  domain.Schedule{Interval: interval},
		CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}
	return c
}

// noJitter is the default options with jitter disabled, so that tests about
// boundaries are not also tests about randomness.
func noJitter() Options {
	o := DefaultOptions()
	o.Jitter = 0
	return o
}

func newScheduler(opts Options, rnd float64) *Scheduler {
	return New(&testClock{now: base}, fixedRandom(rnd), &fakeStore{}, opts)
}

func TestNextWakeSleepsUntilTheSoonestBoundary(t *testing.T) {
	s := newScheduler(noJitter(), 0)

	// An hourly check at 12:00 has its next boundary at 13:00, which is
	// beyond MaxWake, so MaxWake wins.
	hourly := check(t, "hourly", time.Hour)
	if got := s.nextWake(base, []*domain.Check{hourly}); got != s.opts.MaxWake {
		t.Errorf("nextWake = %v, want MaxWake %v", got, s.opts.MaxWake)
	}

	// Ten seconds before a boundary, the loop should wake for it rather than
	// sleep through it.
	almost := base.Add(time.Hour - 10*time.Second)
	if got := s.nextWake(almost, []*domain.Check{hourly}); got != 10*time.Second {
		t.Errorf("nextWake = %v, want 10s", got)
	}
}

func TestNextWakeTakesTheSoonestOfManyChecks(t *testing.T) {
	s := newScheduler(noJitter(), 0)
	checks := []*domain.Check{
		check(t, "hourly", time.Hour),
		check(t, "often", 30*time.Second),
		check(t, "daily", 24*time.Hour),
	}

	// At 12:00 exactly, the 30-second check has a boundary 30 seconds out and
	// the others are far away.
	if got := s.nextWake(base, checks); got != 30*time.Second {
		t.Errorf("nextWake = %v, want 30s from the most frequent check", got)
	}
}

func TestNextWakeIgnoresDisabledChecks(t *testing.T) {
	s := newScheduler(noJitter(), 0)
	frequent := check(t, "often", 5*time.Second)
	frequent.Disable()

	// A disabled check must not drag the whole loop awake every 5 seconds.
	if got := s.nextWake(base, []*domain.Check{frequent}); got != s.opts.MaxWake {
		t.Errorf("nextWake = %v, want MaxWake; a disabled check should not set the pace", got)
	}
}

func TestNextWakeWithNoChecksStillSleeps(t *testing.T) {
	s := newScheduler(noJitter(), 0)

	// Nothing to do is not a reason to spin: a check added while the loop
	// sleeps has nobody to announce it, so the loop looks again within
	// MaxWake.
	if got := s.nextWake(base, nil); got != s.opts.MaxWake {
		t.Errorf("nextWake = %v, want MaxWake", got)
	}
}

func TestNextWakeClampsToMinWake(t *testing.T) {
	s := newScheduler(noJitter(), 0)

	// A boundary a millisecond away, and one already in the past because the
	// previous pass overran. Neither may drive the loop below MinWake.
	tight := check(t, "tight", time.Second)

	justBefore := base.Add(time.Second - time.Millisecond)
	if got := s.nextWake(justBefore, []*domain.Check{tight}); got != s.opts.MinWake {
		t.Errorf("nextWake = %v, want MinWake %v", got, s.opts.MinWake)
	}

	// A nanosecond before a boundary is the tightest the slot math can
	// produce, and it must still not drive the loop below MinWake.
	hair := base.Add(time.Second - time.Nanosecond)
	if got := s.nextWake(hair, []*domain.Check{tight}); got != s.opts.MinWake {
		t.Errorf("nextWake = %v, want MinWake %v", got, s.opts.MinWake)
	}
}

func TestNextWakeNeverExceedsMaxWake(t *testing.T) {
	opts := DefaultOptions() // jitter 0.1
	s := newScheduler(opts, 1.0)

	// Jitter is applied before clamping, so even the largest nudge cannot
	// push a wake past MaxWake.
	weekly := check(t, "weekly", 7*24*time.Hour)
	if got := s.nextWake(base, []*domain.Check{weekly}); got != opts.MaxWake {
		t.Errorf("nextWake = %v, want MaxWake %v", got, opts.MaxWake)
	}
}

func TestJitterOnlyDelays(t *testing.T) {
	opts := noJitter()
	opts.Jitter = 0.5
	hourly := check(t, "hourly", time.Hour)
	at := base.Add(time.Hour - 10*time.Second) // 10s to the boundary

	none := newScheduler(opts, 0).nextWake(at, []*domain.Check{hourly})
	full := newScheduler(opts, 1).nextWake(at, []*domain.Check{hourly})

	if none != 10*time.Second {
		t.Errorf("with a zero draw, nextWake = %v, want the unjittered 10s", none)
	}
	if full <= none {
		t.Errorf("with a full draw, nextWake = %v, want more than %v", full, none)
	}
	if full != 15*time.Second {
		t.Errorf("nextWake = %v, want 10s plus 50%% of 10s", full)
	}
	// Waking early costs an empty pass; waking late costs a late check. The
	// nudge must never be negative.
	if full < none {
		t.Error("jitter moved the wake earlier")
	}
}

func TestLoopRunsDueChecksAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clk := &testClock{now: base, budget: 3, cancel: cancel}
	store := &fakeStore{
		checks: []*domain.Check{check(t, "chk-1", time.Hour)},
		ran:    map[domain.RunKey]bool{},
	}
	s := New(clk, fixedRandom(0), store, noJitter())
	rec := &recorder{}

	err := s.Loop(ctx, rec)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Loop returned %v, want context.Canceled", err)
	}
	if len(rec.calls) == 0 {
		t.Fatal("Loop ran no checks")
	}
	if rec.calls[0].CheckID != "chk-1" {
		t.Errorf("first run was for %q, want chk-1", rec.calls[0].CheckID)
	}
	if len(clk.slept) != 3 {
		t.Errorf("the loop slept %d times, want 3", len(clk.slept))
	}
}

func TestLoopSkipsASlotThatAlreadyRan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := check(t, "chk-1", time.Hour)
	slot := c.Schedule().SlotAt(base)

	clk := &testClock{now: base, budget: 1, cancel: cancel}
	store := &fakeStore{
		checks: []*domain.Check{c},
		// This slot has already been run, which is exactly the situation
		// "one run per (check, slot)" exists to protect.
		ran: map[domain.RunKey]bool{{CheckID: "chk-1", Slot: slot}: true},
	}
	rec := &recorder{}

	if err := New(clk, fixedRandom(0), store, noJitter()).Loop(ctx, rec); !errors.Is(err, context.Canceled) {
		t.Fatalf("Loop returned %v, want context.Canceled", err)
	}

	if len(rec.calls) != 0 {
		t.Errorf("the loop ran %v, want nothing: that slot already had a run", rec.calls)
	}
}

func checkWithPolicy(t *testing.T, id domain.CheckID, interval time.Duration, catchUp domain.CatchUpPolicy, priority int) *domain.Check {
	t.Helper()
	c, err := domain.NewCheck(id, domain.Definition{
		Intent: domain.ScalarIntent{
			Label:   "price",
			Purpose: "the advertised price",
			Type:    domain.TypeNumber,
		},
		Source:   domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
		Schedule: domain.Schedule{Interval: interval, CatchUp: catchUp},
		Policy:   domain.Policy{Priority: priority},
		CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}
	return c
}

func TestCatchUpPolicies(t *testing.T) {
	// Base is 12:00.
	// Last run was slot at 10:00 (slotBase - 2).
	// Now is 13:00 (slotBase + 1). Missed slots: 11:00, 12:00, 13:00.
	interval := time.Hour
	now := base.Add(time.Hour)
	currentSlot := domain.Schedule{Interval: interval}.SlotAt(now)
	lastSlot := currentSlot - 3 // ran 3 slots ago

	ctx := context.Background()

	t.Run("CatchUpSkip schedules only current slot", func(t *testing.T) {
		c := checkWithPolicy(t, "chk-skip", interval, domain.CatchUpSkip, 0)
		store := &fakeStore{
			checks: []*domain.Check{c},
			ran:    map[domain.RunKey]bool{{CheckID: "chk-skip", Slot: lastSlot}: true},
		}
		s := New(&testClock{now: now}, fixedRandom(0), store, noJitter())

		due, err := s.DueChecks(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 {
			t.Fatalf("expected 1 due slot for skip policy, got %d", len(due))
		}
		if due[0].Slot != currentSlot {
			t.Errorf("expected due slot %d, got %d", currentSlot, due[0].Slot)
		}
	})

	t.Run("CatchUpOnce schedules exactly one run for current slot", func(t *testing.T) {
		c := checkWithPolicy(t, "chk-once", interval, domain.CatchUpOnce, 0)
		store := &fakeStore{
			checks: []*domain.Check{c},
			ran:    map[domain.RunKey]bool{{CheckID: "chk-once", Slot: lastSlot}: true},
		}
		s := New(&testClock{now: now}, fixedRandom(0), store, noJitter())

		due, err := s.DueChecks(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 {
			t.Fatalf("expected 1 due slot for once policy, got %d", len(due))
		}
		if due[0].Slot != currentSlot {
			t.Errorf("expected due slot %d, got %d", currentSlot, due[0].Slot)
		}
	})

	t.Run("CatchUpBackfill schedules all missed slots in order", func(t *testing.T) {
		c := checkWithPolicy(t, "chk-backfill", interval, domain.CatchUpBackfill, 0)
		store := &fakeStore{
			checks: []*domain.Check{c},
			ran:    map[domain.RunKey]bool{{CheckID: "chk-backfill", Slot: lastSlot}: true},
		}
		s := New(&testClock{now: now}, fixedRandom(0), store, noJitter())

		due, err := s.DueChecks(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		expectedSlots := []domain.Slot{lastSlot + 1, lastSlot + 2, currentSlot}
		if len(due) != len(expectedSlots) {
			t.Fatalf("expected %d due slots for backfill, got %d", len(expectedSlots), len(due))
		}
		for i, expected := range expectedSlots {
			if due[i].Slot != expected {
				t.Errorf("due[%d] slot = %d, want %d", i, due[i].Slot, expected)
			}
		}
	})
}

func TestSchedulerPrioritizesHigherPriorityChecks(t *testing.T) {
	ctx := context.Background()
	cLow := checkWithPolicy(t, "chk-low", time.Hour, domain.CatchUpOnce, 1)
	cHigh := checkWithPolicy(t, "chk-high", time.Hour, domain.CatchUpOnce, 10)
	cMid := checkWithPolicy(t, "chk-mid", time.Hour, domain.CatchUpOnce, 5)

	store := &fakeStore{
		checks: []*domain.Check{cLow, cHigh, cMid},
		ran:    map[domain.RunKey]bool{},
	}
	s := New(&testClock{now: base}, fixedRandom(0), store, noJitter())

	due, err := s.DueChecks(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 3 {
		t.Fatalf("expected 3 due checks, got %d", len(due))
	}
	if due[0].Check.ID() != "chk-high" || due[1].Check.ID() != "chk-mid" || due[2].Check.ID() != "chk-low" {
		t.Errorf("order = [%s, %s, %s], want [chk-high, chk-mid, chk-low]",
			due[0].Check.ID(), due[1].Check.ID(), due[2].Check.ID())
	}
}

func TestTickDoesNotSleepAndDispatchesDue(t *testing.T) {
	ctx := context.Background()
	c := check(t, "chk-1", time.Hour)
	slot := c.Schedule().SlotAt(base)

	clk := &testClock{now: base}
	store := &fakeStore{
		checks: []*domain.Check{c},
		ran:    map[domain.RunKey]bool{},
	}
	s := New(clk, fixedRandom(0), store, noJitter())
	rec := &recorder{}

	if err := s.Tick(ctx, base, rec); err != nil {
		t.Fatal(err)
	}
	if len(clk.slept) != 0 {
		t.Errorf("Tick slept %d times; the scheduler itself must NOT sleep", len(clk.slept))
	}
	if len(rec.calls) != 1 || rec.calls[0].Slot != slot {
		t.Errorf("expected 1 call for slot %d, got %v", slot, rec.calls)
	}
}

func TestMultipleSchedulerInvocationsDoNotProduceDuplicateRuns(t *testing.T) {
	ctx := context.Background()
	c := check(t, "chk-1", time.Hour)
	slot := c.Schedule().SlotAt(base)

	store := &fakeStore{
		checks: []*domain.Check{c},
		ran:    map[domain.RunKey]bool{},
	}
	s := New(&testClock{now: base}, fixedRandom(0), store, noJitter())

	// First tick finds it due
	due1, err := s.DueChecks(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(due1) != 1 {
		t.Fatalf("first DueChecks = %d, want 1", len(due1))
	}

	// Mark slot as ran (as would happen after claim)
	store.ran[domain.RunKey{CheckID: c.ID(), Slot: slot}] = true

	// Second tick at same instant finds 0 due
	due2, err := s.DueChecks(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(due2) != 0 {
		t.Errorf("second DueChecks = %d, want 0; duplicate runs must not be produced", len(due2))
	}
}

func TestScheduler_CoverageAndEdgeCases(t *testing.T) {
	ctx := context.Background()
	c := check(t, "chk-1", time.Hour)

	// 1. NextWake public method
	s := newScheduler(noJitter(), 0)
	dur := s.NextWake(base, []*domain.Check{c})
	if dur <= 0 {
		t.Errorf("NextWake = %v, want > 0", dur)
	}

	// 2. due backwards compatibility method
	store := &fakeStore{
		checks: []*domain.Check{c},
		ran:    map[domain.RunKey]bool{},
	}
	s = New(&testClock{now: base}, fixedRandom(0), store, noJitter())
	dueList, err := s.due(ctx, []*domain.Check{c}, base)
	if err != nil {
		t.Fatalf("s.due error: %v", err)
	}
	if len(dueList) != 1 || dueList[0].ID() != c.ID() {
		t.Errorf("s.due = %v, want [%v]", dueList, c.ID())
	}

	// s.due error path
	storeErr := &fakeStore{
		checks:        []*domain.Check{c},
		recentRunsErr: errors.New("db error"),
	}
	sErr := New(&testClock{now: base}, fixedRandom(0), storeErr, noJitter())
	if _, err := sErr.due(ctx, []*domain.Check{c}, base); err == nil {
		t.Error("s.due expected error when store fails, got nil")
	}

	// 3. Tick runner error
	recErr := &recorder{runErr: errors.New("runner exploded")}
	if err := s.Tick(ctx, base, recErr); err == nil {
		t.Error("Tick expected error on runner error, got nil")
	}

	// Tick DueChecks error
	storeEnabledErr := &fakeStore{enabledErr: errors.New("store failed")}
	sTickErr := New(&testClock{now: base}, fixedRandom(0), storeEnabledErr, noJitter())
	if err := sTickErr.Tick(ctx, base, &recorder{}); err == nil {
		t.Error("Tick expected error when store.EnabledChecks fails, got nil")
	}

	// 4. DueChecks store error
	if _, err := sTickErr.DueChecks(ctx, base); err == nil {
		t.Error("DueChecks expected error when EnabledChecks fails, got nil")
	}

	// 5. dueWithChecks: recentRuns error
	storeRecentErr := &fakeStore{
		checks:        []*domain.Check{c},
		recentRunsErr: errors.New("recent error"),
	}
	sRecentErr := New(&testClock{now: base}, fixedRandom(0), storeRecentErr, noJitter())
	if _, err := sRecentErr.DueChecks(ctx, base); err == nil {
		t.Error("DueChecks expected error when RecentRuns fails, got nil")
	}

	// dueWithChecks: RunForSlot error (non-notFound)
	storeSlotErr := &fakeStore{
		checks:        []*domain.Check{c},
		runForSlotErr: errors.New("run for slot error"),
	}
	sSlotErr := New(&testClock{now: base}, fixedRandom(0), storeSlotErr, noJitter())
	if _, err := sSlotErr.DueChecks(ctx, base); err == nil {
		t.Error("DueChecks expected error when RunForSlot fails, got nil")
	}

	// 6. Loop error paths
	// Loop with store.EnabledChecks error
	clk := &testClock{now: base, budget: 2, cancel: func() {}}
	sLoopErr1 := New(clk, fixedRandom(0), storeEnabledErr, noJitter())
	if err := sLoopErr1.Loop(ctx, &recorder{}); err == nil {
		t.Error("Loop expected error when store.EnabledChecks fails")
	}

	// Loop with dueWithChecks error
	sLoopErr2 := New(clk, fixedRandom(0), storeRecentErr, noJitter())
	if err := sLoopErr2.Loop(ctx, &recorder{}); err == nil {
		t.Error("Loop expected error when dueWithChecks fails")
	}

	// Loop with runner error
	sLoopErr3 := New(clk, fixedRandom(0), store, noJitter())
	if err := sLoopErr3.Loop(ctx, recErr); err == nil {
		t.Error("Loop expected error when runner fails")
	}

	// 7. CatchUpBackfill bounded by maxBackfill (100)
	cBackfill := checkWithPolicy(t, "chk-bf", time.Minute, domain.CatchUpBackfill, 1)
	sched := cBackfill.Schedule()
	currSlot := sched.SlotAt(base)
	// Simulate last slot was 200 slots ago
	oldSlot := currSlot - 200
	storeBackfill := &fakeStore{
		checks: []*domain.Check{cBackfill},
		ran: map[domain.RunKey]bool{
			{CheckID: cBackfill.ID(), Slot: oldSlot}: true,
		},
	}
	sBf := New(&testClock{now: base}, fixedRandom(0), storeBackfill, noJitter())
	dueBf, err := sBf.DueChecks(ctx, base)
	if err != nil {
		t.Fatalf("DueChecks backfill error: %v", err)
	}
	// maxBackfill = 100, candidate slots are currSlot - 100 to currSlot, total 101 slots
	if len(dueBf) != 101 {
		t.Errorf("DueChecks backfill count = %d, want 101 (bounded by 100)", len(dueBf))
	}
}

