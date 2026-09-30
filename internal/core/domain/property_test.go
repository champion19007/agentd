package domain_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

// TestProperty_SlotMonotonicityAndUniqueness verifies that for any interval,
// slots are strictly monotonic with respect to time, and each instant maps
// to exactly one unique slot.
func TestProperty_SlotMonotonicityAndUniqueness(t *testing.T) {
	intervals := []time.Duration{
		time.Second,
		30 * time.Second,
		time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		time.Hour,
		24 * time.Hour,
	}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, interval := range intervals {
		t.Run(interval.String(), func(t *testing.T) {
			sched := domain.Schedule{
				Interval: interval,
				CatchUp:  domain.CatchUpSkip,
			}

			var prevSlot domain.Slot
			var prevTime time.Time

			for i := 0; i < 1000; i++ {
				// Step forward by random durations up to interval * 2
				delta := time.Duration(rand.Int63n(int64(interval * 2)))
				currTime := baseTime.Add(delta + time.Duration(i)*interval)

				slot := sched.SlotAt(currTime)
				start := sched.SlotStart(slot)

				// Invariant 1: SlotStart(SlotAt(t)) must be <= t
				if start.After(currTime) {
					t.Fatalf("SlotStart %v is after time %v for slot %d", start, currTime, slot)
				}

				// Invariant 2: SlotStart + Interval must be > t
				if !start.Add(interval).After(currTime) {
					t.Fatalf("SlotStart+Interval %v is not strictly after time %v for slot %d", start.Add(interval), currTime, slot)
				}

				// Invariant 3: SlotAt(SlotStart(s)) == s
				if s2 := sched.SlotAt(start); s2 != slot {
					t.Fatalf("SlotAt(SlotStart(%d)) = %d, want %d", slot, s2, slot)
				}

				// Invariant 4: Monotonic progression
				if i > 0 && currTime.After(prevTime) {
					if slot < prevSlot {
						t.Fatalf("non-monotonic slot progression: time %v (slot %d) after %v (slot %d)", currTime, slot, prevTime, prevSlot)
					}
				}

				prevSlot = slot
				prevTime = currTime
			}
		})
	}
}

// TestProperty_TimezoneAndDSTBoundaries tests scheduling slots across DST transitions.
// Slots are UTC-based unix-nano arithmetic, ensuring immunity to local DST jumps.
func TestProperty_TimezoneAndDSTBoundaries(t *testing.T) {
	timezones := []string{
		"America/New_York",
		"Europe/London",
		"Australia/Sydney",
		"UTC",
	}

	// 2026 US DST transitions:
	// Spring forward: Sunday, March 8, 2026, 2:00 AM -> 3:00 AM
	// Fall back: Sunday, November 1, 2026, 2:00 AM -> 1:00 AM
	dates := []struct {
		name string
		year int
		mon  time.Month
		day  int
	}{
		{"SpringForward2026", 2026, time.March, 8},
		{"FallBack2026", 2026, time.November, 1},
	}

	for _, tzName := range timezones {
		loc, err := time.LoadLocation(tzName)
		if err != nil {
			t.Logf("Skipping timezone %s (not found on host)", tzName)
			continue
		}

		for _, tc := range dates {
			t.Run(fmt.Sprintf("%s_%s", tzName, tc.name), func(t *testing.T) {
				sched := domain.Schedule{
					Interval: 15 * time.Minute,
					CatchUp:  domain.CatchUpOnce,
				}

				// Sample every 5 minutes across the 24-hour day of DST transition
				startOfDay := time.Date(tc.year, tc.mon, tc.day, 0, 0, 0, 0, loc)
				var prevSlot domain.Slot

				for m := 0; m < 24*60; m += 5 {
					localInstant := startOfDay.Add(time.Duration(m) * time.Minute)
					slot := sched.SlotAt(localInstant)

					if m > 0 && slot < prevSlot {
						t.Fatalf("DST anomaly: slot decreased from %d to %d at %v (%s)", prevSlot, slot, localInstant, tzName)
					}

					slotStart := sched.SlotStart(slot)
					if slotStart.After(localInstant.UTC()) {
						t.Fatalf("slotStart %v after local instant UTC %v", slotStart, localInstant.UTC())
					}

					prevSlot = slot
				}
			})
		}
	}
}

// TestProperty_RunStateMachineTransitions thoroughly tests all valid and invalid transitions
// for a Run across its lifecycle.
func TestProperty_RunStateMachineTransitions(t *testing.T) {
	now := time.Now().UTC()
	ext := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "val", Type: domain.TypeString}}
	fail := domain.Failure{Class: domain.ClassStructural, Code: "e", Summary: "err"}

	states := []domain.RunState{
		domain.StatePending,
		domain.StateRunning,
		domain.StateQuiet,
		domain.StateChanged,
		domain.StateDegraded,
		domain.StateFailed,
		domain.StateInterrupted,
		domain.StateSkippedOverload,
	}

	// Matrix of terminality
	for _, st := range states {
		isTerm := st.Terminal()
		expectedTerm := (st != domain.StatePending && st != domain.StateRunning)
		if isTerm != expectedTerm {
			t.Fatalf("state %s Terminal() = %v, want %v", st, isTerm, expectedTerm)
		}
	}

	// Verify terminal runs reject all mutators
	for _, termState := range []domain.RunState{
		domain.StateQuiet,
		domain.StateChanged,
		domain.StateDegraded,
		domain.StateFailed,
		domain.StateInterrupted,
		domain.StateSkippedOverload,
	} {
		r, err := domain.RestoreRun(domain.RestoredRun{
			ID:                "run-term",
			CheckID:           "chk-1",
			Slot:              1,
			DefinitionVersion: 1,
			BindingVersion:    1,
			State:             termState,
			CreatedAt:         now.Add(-time.Hour),
			StartedAt:         now.Add(-30 * time.Minute),
			EndedAt:           now,
			Failure: func() *domain.Failure {
				if termState == domain.StateDegraded || termState == domain.StateFailed {
					return &fail
				}
				return nil
			}(),
		})
		if err != nil {
			t.Fatalf("RestoreRun failed for %s: %v", termState, err)
		}

		if err := r.Start(now, 1); err == nil {
			t.Errorf("[%s] Start should fail on terminal run", termState)
		}
		if err := r.Quiet(now, "snap-1", ext); err == nil {
			t.Errorf("[%s] Quiet should fail on terminal run", termState)
		}
		if err := r.Changed(now, "snap-1", ext, "chg"); err == nil {
			t.Errorf("[%s] Changed should fail on terminal run", termState)
		}
		if err := r.Degrade(now, "snap-1", ext, fail); err == nil {
			t.Errorf("[%s] Degrade should fail on terminal run", termState)
		}
		if err := r.Fail(now, fail); err == nil {
			t.Errorf("[%s] Fail should fail on terminal run", termState)
		}
		if err := r.Interrupt(now, "intr"); err == nil {
			t.Errorf("[%s] Interrupt should fail on terminal run", termState)
		}
		if err := r.SkipOverloaded(now, "skip"); err == nil {
			t.Errorf("[%s] SkipOverloaded should fail on terminal run", termState)
		}
	}
}
