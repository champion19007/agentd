// Package scheduling decides which checks are due and when the runner should
// next wake up. It is pure core: it holds no timers, opens no connections and
// reads no clock of its own.
package scheduling

import (
	"context"
	"errors"
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

// Scheduler drives the loop: ask the store which checks are enabled, work out
// which of them have a slot that has not been run, hand those to the runner,
// then wait until there is reason to look again.
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

// Loop runs until ctx is cancelled.
func (s *Scheduler) Loop(ctx context.Context, r Runner) error {
	for {
		now := s.clock.Now()

		checks, err := s.store.EnabledChecks(ctx)
		if err != nil {
			return err
		}

		due, err := s.due(ctx, checks, now)
		if err != nil {
			return err
		}
		for _, c := range due {
			if err := r.Run(ctx, c, c.Schedule().SlotAt(now)); err != nil {
				return err
			}
		}

		// Pacing is computed from every enabled check, not just the ones that
		// ran. A check that was already up to date this pass still has a next
		// slot boundary, and it is often the soonest one.
		if err := s.clock.Sleep(ctx, s.nextWake(now, checks)); err != nil {
			return err
		}
	}
}

// due returns the checks whose current slot has no run yet. Slot identity is
// what makes this exact: a check is due for slot N or it is not, with no
// reasoning about how close a timestamp is to another timestamp.
func (s *Scheduler) due(ctx context.Context, checks []*domain.Check, now time.Time) ([]*domain.Check, error) {
	var out []*domain.Check
	for _, c := range checks {
		if !c.Enabled() {
			continue
		}
		slot := c.Schedule().SlotAt(now)
		_, err := s.store.RunForSlot(ctx, c.ID(), slot)
		switch {
		case errors.Is(err, ports.ErrNotFound):
			out = append(out, c)
		case err != nil:
			return nil, err
		}
	}
	return out, nil
}

// nextWake returns how long the loop should sleep before looking for due
// checks again, given the instant the pass started and every enabled check.
//
// The delay is the time until the soonest slot boundary among those checks,
// nudged later by jitter and then clamped into [MinWake, MaxWake]. Clamping
// last is what makes the bounds real: jitter can never push a wake past
// MaxWake, and nothing can drive the loop below MinWake.
//
// Two edges matter more than they look:
//
// Because SlotAt floors, the next boundary is always ahead of now, but it can
// be a nanosecond ahead when a pass happens to land just before one. MinWake
// is what stops that from becoming a spin: a check that cannot keep up with
// its own interval should fall behind steadily rather than pin a core.
//
// No checks at all still sleeps MaxWake rather than spinning, because a check
// added while the loop is sleeping has nobody to announce it.
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

	// Jitter only ever delays. Waking early costs an empty pass, which is
	// cheap; waking late costs a late check, which is what the operator
	// notices. Note this is loop pacing, not per-check destampeding -- that
	// is Schedule.Jitter's job, applied when a run is placed within its slot,
	// because delaying the whole loop moves every check together and so
	// spreads nothing out.
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
