// Package scheduling decides which checks are due and when the runner should
// next wake up. It is pure core: it holds no timers, opens no connections and
// reads no clock of its own. It does not sleep; outer tickers provide ticks.
package scheduling

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Options tunes the scheduler's pacing.
type Options struct {
	// MinWake is the shortest the scheduler will ever sleep, so that a
	// misconfigured check cannot spin the loop.
	MinWake time.Duration

	// MaxWake bounds how long the scheduler will sleep with nothing due, so
	// that a newly added check is picked up promptly.
	MaxWake time.Duration

	// Jitter is the fraction of the computed delay, in [0, 1], that may be
	// added at random so that checks sharing a due instant do not stampede
	// the same source.
	Jitter float64
}

// DefaultOptions are sane starting values.
func DefaultOptions() Options {
	return Options{
		MinWake: time.Second,
		MaxWake: time.Minute,
		Jitter:  0.1,
	}
}

// Runner executes a single due check in a given slot. Implemented by the run
// package.
type Runner interface {
	Run(ctx context.Context, c *domain.Check, slot domain.Slot) error
}

// DueCheck represents a check and slot combination that is eligible to run.
type DueCheck struct {
	Check    *domain.Check
	Slot     domain.Slot
	Priority int
	LateBy   time.Duration
}

// Scheduler calculates slot boundaries, determines due checks according to
// configured catch-up policies, and sorts work by priority.
//
// The Scheduler itself does NOT sleep: it receives time through an injected clock
// or explicitly via Tick / DueChecks invocations from an outer loop or ticker.
type Scheduler struct {
	clock  ports.Clock
	random ports.Random
	store  ports.Store
	opts   Options
}

// New builds a Scheduler. Every capability it needs arrives as an argument; it
// constructs nothing for itself.
func New(clk ports.Clock, rnd ports.Random, st ports.Store, opts Options) *Scheduler {
	return &Scheduler{clock: clk, random: rnd, store: st, opts: opts}
}

// Tick evaluates due checks at the instant 'now' and dispatches them to Runner.
// It performs due selection and dispatch without sleeping.
func (s *Scheduler) Tick(ctx context.Context, now time.Time, r Runner) error {
	due, err := s.DueChecks(ctx, now)
	if err != nil {
		return err
	}
	for _, d := range due {
		if err := r.Run(ctx, d.Check, d.Slot); err != nil {
			return err
		}
	}
	return nil
}

// DueChecks finds all checks with slots due to be executed at 'now', taking into
// account each check's CatchUpPolicy (skip, once, backfill). The results are sorted
// with higher-priority checks first, then chronologically by slot.
func (s *Scheduler) DueChecks(ctx context.Context, now time.Time) ([]DueCheck, error) {
	checks, err := s.store.EnabledChecks(ctx)
	if err != nil {
		return nil, err
	}
	return s.dueWithChecks(ctx, checks, now)
}

// dueWithChecks evaluates due slots for a given set of checks.
func (s *Scheduler) dueWithChecks(ctx context.Context, checks []*domain.Check, now time.Time) ([]DueCheck, error) {
	var due []DueCheck

	for _, c := range checks {
		if !c.Enabled() {
			continue
		}
		sched := c.Schedule()
		currSlot := sched.SlotAt(now)
		policy := sched.CatchUpPolicy()

		// Check the most recent run for this check
		recent, err := s.store.RecentRuns(ctx, c.ID(), 1)
		if err != nil && !errors.Is(err, ports.ErrNotFound) {
			return nil, err
		}

		var candidateSlots []domain.Slot
		if len(recent) == 0 {
			// Never run before: schedule current slot
			candidateSlots = append(candidateSlots, currSlot)
		} else {
			lastSlot := recent[0].Slot()
			if lastSlot >= currSlot {
				// Already ran for this slot or later
				continue
			}

			switch policy {
			case domain.CatchUpSkip:
				// Skip all missed slots; only schedule current slot
				candidateSlots = append(candidateSlots, currSlot)

			case domain.CatchUpOnce:
				// Schedule current slot as the single catch-up run
				candidateSlots = append(candidateSlots, currSlot)

			case domain.CatchUpBackfill:
				// Schedule missed slots up to current slot, bounded to prevent exhaustion
				start := lastSlot + 1
				const maxBackfill = 100
				if currSlot-start > maxBackfill {
					start = currSlot - maxBackfill
				}
				for slot := start; slot <= currSlot; slot++ {
					candidateSlots = append(candidateSlots, slot)
				}
			default:
				candidateSlots = append(candidateSlots, currSlot)
			}
		}

		// Verify against store to ensure one run per slot
		for _, slot := range candidateSlots {
			_, err := s.store.RunForSlot(ctx, c.ID(), slot)
			switch {
			case errors.Is(err, ports.ErrNotFound):
				slotStart := sched.SlotStart(slot)
				lateBy := now.Sub(slotStart)
				if lateBy < 0 {
					lateBy = 0
				}
				due = append(due, DueCheck{
					Check:    c,
					Slot:     slot,
					Priority: c.ActiveDefinition().Policy.Priority,
					LateBy:   lateBy,
				})
			case err != nil:
				return nil, err
			}
		}
	}

	// Sort due checks: higher priority first; for ties, earlier slot first
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].Priority != due[j].Priority {
			return due[i].Priority > due[j].Priority
		}
		return due[i].Slot < due[j].Slot
	})

	return due, nil
}

// due is retained for backwards compatibility with existing callers.
func (s *Scheduler) due(ctx context.Context, checks []*domain.Check, now time.Time) ([]*domain.Check, error) {
	dueList, err := s.dueWithChecks(ctx, checks, now)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Check, 0, len(dueList))
	for _, d := range dueList {
		out = append(out, d.Check)
	}
	return out, nil
}

// Loop runs an outer process loop driving Tick and sleeping between passes until
// ctx is cancelled.
func (s *Scheduler) Loop(ctx context.Context, r Runner) error {
	for {
		now := s.clock.Now()

		checks, err := s.store.EnabledChecks(ctx)
		if err != nil {
			return err
		}

		due, err := s.dueWithChecks(ctx, checks, now)
		if err != nil {
			return err
		}
		for _, d := range due {
			if err := r.Run(ctx, d.Check, d.Slot); err != nil {
				return err
			}
		}

		// Pacing is computed from every enabled check relative to current clock time.
		current := s.clock.Now()
		if err := s.clock.Sleep(ctx, s.nextWake(current, checks)); err != nil {
			return err
		}
	}
}

// NextWake returns how long until the soonest slot boundary among enabled checks,
// nudged later by jitter and clamped into [MinWake, MaxWake].
func (s *Scheduler) NextWake(now time.Time, checks []*domain.Check) time.Duration {
	return s.nextWake(now, checks)
}

func (s *Scheduler) nextWake(now time.Time, checks []*domain.Check) time.Duration {
	delay := s.opts.MaxWake

	for _, c := range checks {
		if !c.Enabled() {
			continue
		}
		sched := c.Schedule()
		until := sched.SlotStart(sched.SlotAt(now) + 1).Sub(now)
		if until < delay {
			delay = until
		}
	}

	if s.opts.Jitter > 0 && delay > 0 {
		delay += time.Duration(float64(delay) * s.opts.Jitter * s.random.Float64())
	}

	if delay < s.opts.MinWake {
		return s.opts.MinWake
	}
	if delay > s.opts.MaxWake {
		return s.opts.MaxWake
	}
	return delay
}
