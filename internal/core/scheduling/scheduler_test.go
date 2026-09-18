// These tests live inside the package so they can exercise nextWake directly.
// Pacing is the kind of logic that is easy to get subtly wrong and hard to
// notice in production -- a loop that wakes slightly too often just looks like
// a busy process -- so it is worth testing at the unit rather than only
// through Loop.
package scheduling

import (
	"context"
	"errors"
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

// fakeStore implements only the two methods the scheduler calls. The embedded
// interface supplies the rest of the method set so this stays a few lines
// rather than a generated mock; any method the scheduler is not supposed to
// call panics, which is the behaviour we want from a test double.
type fakeStore struct {
	ports.Store
	checks []*domain.Check
	ran    map[domain.RunKey]bool
}

func (s *fakeStore) EnabledChecks(context.Context) ([]*domain.Check, error) {
	return s.checks, nil
}

func (s *fakeStore) RunForSlot(_ context.Context, id domain.CheckID, slot domain.Slot) (*domain.Run, error) {
	if s.ran[domain.RunKey{CheckID: id, Slot: slot}] {
		return &domain.Run{}, nil
	}
	return nil, ports.ErrNotFound
}

// recorder is a Runner that remembers what it was asked to run.
type recorder struct {
	calls []domain.RunKey
}

func (r *recorder) Run(_ context.Context, c *domain.Check, slot domain.Slot) error {
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
