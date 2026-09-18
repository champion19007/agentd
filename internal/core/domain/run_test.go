package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

func newRun(t *testing.T) *domain.Run {
	t.Helper()
	r, err := domain.NewRun("run-1", "chk-1", 42, 1, base)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	return r
}

// structuralFailure is a valid Failure for tests that need one.
func structuralFailure() domain.Failure {
	return domain.Failure{
		Class:   domain.ClassStructural,
		Code:    "missing_required",
		Summary: "the price is no longer where this check expects it",
	}
}

func TestNewRunStartsPending(t *testing.T) {
	r := newRun(t)

	mustInvariants(t, r.CheckInvariants())
	if got := r.State(); got != domain.StatePending {
		t.Errorf("State = %q, want %q", got, domain.StatePending)
	}
	if r.Terminal() {
		t.Error("a pending run must not be terminal")
	}
	if want := (domain.RunKey{CheckID: "chk-1", Slot: 42}); r.Key() != want {
		t.Errorf("Key = %+v, want %+v", r.Key(), want)
	}
}

// TestRunTransitions walks the whole lifecycle table, both directions, so that
// the legal moves and the illegal ones are asserted from one place.
func TestRunTransitions(t *testing.T) {
	// advance drives a fresh run into the given state, or fails.
	advance := func(t *testing.T, to domain.RunState) *domain.Run {
		t.Helper()
		r := newRun(t)
		if to == domain.StatePending {
			return r
		}
		if to != domain.StateSkippedOverload && to != domain.StateInterrupted {
			if err := r.Start(at(time.Second), 1); err != nil {
				t.Fatalf("Start: %v", err)
			}
		}
		if to == domain.StateRunning {
			return r
		}
		if err := apply(r, to, at(2*time.Second)); err != nil {
			t.Fatalf("driving run to %q: %v", to, err)
		}
		return r
	}

	states := []domain.RunState{
		domain.StatePending, domain.StateRunning, domain.StateQuiet,
		domain.StateChanged, domain.StateDegraded, domain.StateFailed,
		domain.StateInterrupted, domain.StateSkippedOverload,
	}

	for _, from := range states {
		for _, to := range states {
			want := domain.CanTransition(from, to)
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				r := advance(t, from)
				err := apply(r, to, at(time.Minute))

				switch {
				case want && err != nil:
					t.Fatalf("%q -> %q should be legal, got %v", from, to, err)
				case !want && err == nil:
					t.Fatalf("%q -> %q should be illegal, but it was accepted", from, to)
				}
				mustInvariants(t, r.CheckInvariants())
			})
		}
	}
}

// apply drives a run into a state through its public API.
func apply(r *domain.Run, to domain.RunState, ts time.Time) error {
	switch to {
	case domain.StateRunning:
		return r.Start(ts, 1)
	case domain.StateQuiet:
		return r.Quiet(ts, "sha256:deadbeef")
	case domain.StateChanged:
		return r.Changed(ts, "sha256:deadbeef", "the price went up")
	case domain.StateDegraded:
		return r.Degrade(ts, "sha256:deadbeef", domain.Failure{
			Class:   domain.ClassSemantic,
			Summary: "the seat count was missing",
		})
	case domain.StateFailed:
		return r.Fail(ts, structuralFailure())
	case domain.StateInterrupted:
		return r.Interrupt(ts, "agentd was shutting down")
	case domain.StateSkippedOverload:
		return r.SkipOverloaded(ts, "the runner was at capacity")
	case domain.StatePending:
		return errors.New("a run cannot be driven back to pending")
	}
	return errors.New("unknown state")
}

func TestTerminalRunIsImmutable(t *testing.T) {
	terminal := []domain.RunState{
		domain.StateQuiet, domain.StateChanged, domain.StateDegraded,
		domain.StateFailed, domain.StateInterrupted, domain.StateSkippedOverload,
	}

	for _, state := range terminal {
		t.Run(string(state), func(t *testing.T) {
			r := newRun(t)
			if state != domain.StateSkippedOverload && state != domain.StateInterrupted {
				if err := r.Start(at(time.Second), 1); err != nil {
					t.Fatalf("Start: %v", err)
				}
			}
			if err := apply(r, state, at(2*time.Second)); err != nil {
				t.Fatalf("driving run to %q: %v", state, err)
			}

			ended := r.EndedAt()
			for _, again := range terminal {
				if err := apply(r, again, at(time.Hour)); !errors.Is(err, domain.ErrTerminal) {
					t.Fatalf("changing a %q run to %q returned %v, want ErrTerminal", state, again, err)
				}
			}
			if r.State() != state {
				t.Errorf("state changed to %q despite being terminal", r.State())
			}
			if !r.EndedAt().Equal(ended) {
				t.Errorf("end time moved from %v to %v despite being terminal", ended, r.EndedAt())
			}
		})
	}
}

// TestDegradedIsNotFailed pins the distinction the specification calls out. If
// someone later collapses the two states, this is the test that objects.
func TestDegradedIsNotFailed(t *testing.T) {
	if domain.StateDegraded == domain.StateFailed {
		t.Fatal("degraded and failed must be distinct states")
	}
	if domain.StateDegraded.Succeeded() || domain.StateFailed.Succeeded() {
		t.Error("neither degraded nor failed is a clean success")
	}
	if !domain.StateDegraded.CountsAsFailure() || !domain.StateFailed.CountsAsFailure() {
		t.Error("both degraded and failed should count towards a check's failure history")
	}

	degraded := newRun(t)
	if err := degraded.Start(at(time.Second), 1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := degraded.Degrade(at(2*time.Second), "sha256:abc", domain.Failure{
		Class:   domain.ClassSemantic,
		Summary: "the seat count was missing",
	}); err != nil {
		t.Fatalf("Degrade: %v", err)
	}

	// The operator still has data from a degraded run. That is the whole
	// reason the two states are kept apart.
	if degraded.SnapshotID() == "" {
		t.Error("a degraded run should still record what it managed to observe")
	}

	failed := newRun(t)
	if err := failed.Start(at(time.Second), 1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := failed.Fail(at(2*time.Second), structuralFailure()); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if failed.SnapshotID() != "" {
		t.Error("a failed run has no usable result to point at")
	}
}

func TestInterruptedAndSkippedAreNotTheSourcesFault(t *testing.T) {
	for _, state := range []domain.RunState{domain.StateInterrupted, domain.StateSkippedOverload} {
		if state.CountsAsFailure() {
			t.Errorf("%q must not count as a failure; it says nothing about the source", state)
		}
	}
}

func TestEndedRunsCarryAClassifiedFailure(t *testing.T) {
	r := newRun(t)
	if err := r.Start(at(time.Second), 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A failure with no class and no summary is not something an operator
	// could act on, so the run refuses to end with it.
	if err := r.Fail(at(2*time.Second), domain.Failure{}); err == nil {
		t.Fatal("Fail accepted an unclassified failure")
	}
	if r.State() != domain.StateRunning {
		t.Errorf("run moved to %q despite the rejected failure", r.State())
	}
	mustInvariants(t, r.CheckInvariants())
}

func TestRunRecordsTheDefinitionItRanUnder(t *testing.T) {
	if _, err := domain.NewRun("run-1", "chk-1", 1, 0, base); err == nil {
		t.Fatal("NewRun accepted a run with no definition version; a past run must stay explicable after the check is edited")
	}
}
