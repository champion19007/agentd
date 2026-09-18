package domain

import (
	"errors"
	"time"
)

// ErrTerminal reports an attempt to change a run that has already ended.
var ErrTerminal = errors.New("run is terminal and cannot be changed")

// ErrBadTransition reports a state change the run's lifecycle does not allow.
var ErrBadTransition = errors.New("illegal run state transition")

// ErrDuplicateRun reports a second run for a (check, slot) pair that already
// has one. The store enforces this with a unique index; the domain names the
// error so that the core can react to it without knowing about SQL.
var ErrDuplicateRun = errors.New("a run already exists for this check and slot")

// RunState is where a run is in its lifecycle. The non-terminal states say
// what is happening; the terminal states say what happened.
type RunState string

const (
	// StatePending is a run that has been created for a slot but has not
	// started.
	StatePending RunState = "pending"

	// StateRunning is a run in flight.
	StateRunning RunState = "running"

	// StateQuiet is the outcome that should be most common: the check ran,
	// extraction worked, and nothing the operator cares about changed. It is
	// recorded rather than merely implied, because a silence you can audit is
	// the only kind worth trusting.
	StateQuiet RunState = "quiet"

	// StateChanged is a successful run where the extracted result differs
	// from the previous one.
	StateChanged RunState = "changed"

	// StateDegraded is a run that produced a usable result of reduced
	// confidence: an optional field was missing, a collection came back
	// short, a value looked wrong. Something was learned, with a caveat.
	StateDegraded RunState = "degraded"

	// StateFailed is a run that produced no usable result at all.
	//
	// Degraded and failed are kept apart deliberately. Collapsing them would
	// force a single alerting policy onto two situations that call for
	// different responses, and would make an operator who is told "failed"
	// unable to tell whether they still have data.
	StateFailed RunState = "failed"

	// StateInterrupted is a run cut short by shutdown or cancellation. It is
	// nobody's fault and implies nothing about the source, so it must not be
	// counted as a failure when deciding whether to open an incident.
	StateInterrupted RunState = "interrupted"

	// StateSkippedOverload is a run that never started because the runner was
	// already saturated. Recording the skip rather than dropping it silently
	// is what keeps the silence trustworthy: a gap in the record always has
	// a stated reason.
	StateSkippedOverload RunState = "skipped_overload"
)

// Valid reports whether s is a known state.
func (s RunState) Valid() bool {
	switch s {
	case StatePending, StateRunning, StateQuiet, StateChanged, StateDegraded,
		StateFailed, StateInterrupted, StateSkippedOverload:
		return true
	}
	return false
}

// Terminal reports whether s is an ending state. A run in a terminal state is
// immutable.
func (s RunState) Terminal() bool {
	return s.Valid() && s != StatePending && s != StateRunning
}

// Succeeded reports whether the run produced a result the operator can rely
// on without caveat.
func (s RunState) Succeeded() bool {
	return s == StateQuiet || s == StateChanged
}

// CountsAsFailure reports whether this outcome should count towards a check's
// failure history. Interruptions and overload skips are about Agentd, not
// about the source, so they do not count.
func (s RunState) CountsAsFailure() bool {
	return s == StateFailed || s == StateDegraded
}

// allowedTransitions is the lifecycle, written out so that it can be read and
// tested rather than inferred from scattered conditionals.
var allowedTransitions = map[RunState][]RunState{
	StatePending: {
		StateRunning,
		// Never started: the runner was saturated, or shutdown arrived first.
		StateSkippedOverload,
		StateInterrupted,
		// Also never started, but for a reason the operator must hear about:
		// the check is misconfigured, or the store could not say how to run
		// it. Recording that as a failed run rather than leaving a pending
		// one is the honest account of what happened.
		StateFailed,
	},
	StateRunning: {
		StateQuiet,
		StateChanged,
		StateDegraded,
		StateFailed,
		StateInterrupted,
	},
}

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to RunState) bool {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// RunKey is the identity that must be unique: one run per check per slot.
// Making it a comparable value means duplicate detection is an equality test
// rather than a judgement about timestamps.
type RunKey struct {
	CheckID CheckID
	Slot    Slot
}

// Run is one execution of one check in one slot.
//
// Invariants: a run is immutable once terminal, and there is at most one run
// per (check, slot).
type Run struct {
	id                RunID
	checkID           CheckID
	slot              Slot
	definitionVersion int
	bindingVersion    int

	state     RunState
	createdAt time.Time
	startedAt time.Time
	endedAt   time.Time

	snapshotID  SnapshotID
	result      Extraction
	failure     *Failure
	explanation string
}

// NewRun creates a pending run for a check and slot.
func NewRun(id RunID, checkID CheckID, slot Slot, definitionVersion int, at time.Time) (*Run, error) {
	if !nonEmpty(string(id)) {
		return nil, invalidf("run needs an id")
	}
	if !nonEmpty(string(checkID)) {
		return nil, invalidf("run %q needs a check id", id)
	}
	if definitionVersion < 1 {
		return nil, invalidf("run %q needs the definition version it ran under", id)
	}
	return &Run{
		id:                id,
		checkID:           checkID,
		slot:              slot,
		definitionVersion: definitionVersion,
		state:             StatePending,
		createdAt:         at.UTC(),
	}, nil
}

// ID returns the run's identifier.
func (r *Run) ID() RunID { return r.id }

// CheckID returns the check that was run.
func (r *Run) CheckID() CheckID { return r.checkID }

// Slot returns the scheduling slot this run belongs to.
func (r *Run) Slot() Slot { return r.slot }

// Key returns the uniqueness key for this run.
func (r *Run) Key() RunKey { return RunKey{CheckID: r.checkID, Slot: r.slot} }

// DefinitionVersion returns the check definition version this run used.
func (r *Run) DefinitionVersion() int { return r.definitionVersion }

// BindingVersion returns the binding version this run used, or zero if it
// never got as far as extraction.
func (r *Run) BindingVersion() int { return r.bindingVersion }

// State returns the run's current state.
func (r *Run) State() RunState { return r.state }

// Terminal reports whether the run has ended.
func (r *Run) Terminal() bool { return r.state.Terminal() }

// CreatedAt, StartedAt and EndedAt return the run's timeline. A zero time
// means that stage was never reached.
func (r *Run) CreatedAt() time.Time { return r.createdAt }
func (r *Run) StartedAt() time.Time { return r.startedAt }
func (r *Run) EndedAt() time.Time   { return r.endedAt }

// SnapshotID returns the snapshot this run captured, if any.
func (r *Run) SnapshotID() SnapshotID { return r.snapshotID }

// Result returns what this run extracted. It is the zero Extraction for a run
// that never got that far.
func (r *Run) Result() Extraction { return r.result }

// Failure returns the classified failure that ended this run, if any. The
// returned pointer is to a copy.
func (r *Run) Failure() *Failure {
	if r.failure == nil {
		return nil
	}
	f := *r.failure
	return &f
}

// Explanation returns what happened, in the operator's terms.
func (r *Run) Explanation() string { return r.explanation }

// transition applies a state change after checking that it is legal and that
// the run has not already ended. Every state change goes through here, which
// is what makes "immutable once terminal" a property of the type rather than
// a rule contributors have to remember.
func (r *Run) transition(to RunState, at time.Time) error {
	if r.state.Terminal() {
		return ErrTerminal
	}
	if !to.Valid() {
		return invalidf("unknown run state %q", to)
	}
	if !CanTransition(r.state, to) {
		return ErrBadTransition
	}
	r.state = to
	if to.Terminal() {
		r.endedAt = at.UTC()
	}
	return nil
}

// Start moves a pending run into flight.
func (r *Run) Start(at time.Time, bindingVersion int) error {
	if err := r.transition(StateRunning, at); err != nil {
		return err
	}
	r.startedAt = at.UTC()
	r.bindingVersion = bindingVersion
	return nil
}

// Quiet ends the run with nothing changed. The result is still recorded: a
// silence nobody can audit is not one an operator should trust.
func (r *Run) Quiet(at time.Time, snap SnapshotID, result Extraction) error {
	if err := r.transition(StateQuiet, at); err != nil {
		return err
	}
	r.snapshotID = snap
	r.result = result
	r.explanation = "the source was checked and nothing changed"
	return nil
}

// Changed ends the run with a difference found.
func (r *Run) Changed(at time.Time, snap SnapshotID, result Extraction, explanation string) error {
	if err := r.transition(StateChanged, at); err != nil {
		return err
	}
	r.snapshotID = snap
	r.result = result
	r.explanation = explanation
	if !nonEmpty(r.explanation) {
		r.explanation = "the source changed"
	}
	return nil
}

// Degrade ends the run with a usable but diminished result. Whatever was
// extracted is kept, because having part of an answer is the whole reason
// this is not a failure.
func (r *Run) Degrade(at time.Time, snap SnapshotID, result Extraction, f Failure) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := r.transition(StateDegraded, at); err != nil {
		return err
	}
	r.snapshotID = snap
	r.result = result
	r.failure = &f
	r.explanation = f.Summary
	return nil
}

// Fail ends the run with no usable result.
func (r *Run) Fail(at time.Time, f Failure) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := r.transition(StateFailed, at); err != nil {
		return err
	}
	r.failure = &f
	r.explanation = f.Summary
	return nil
}

// Interrupt ends the run because Agentd stopped, not because the source
// misbehaved.
func (r *Run) Interrupt(at time.Time, reason string) error {
	if err := r.transition(StateInterrupted, at); err != nil {
		return err
	}
	r.explanation = reason
	if !nonEmpty(r.explanation) {
		r.explanation = "the run was interrupted before it could finish"
	}
	return nil
}

// SkipOverloaded ends a run that never started because the runner was
// saturated.
func (r *Run) SkipOverloaded(at time.Time, reason string) error {
	if err := r.transition(StateSkippedOverload, at); err != nil {
		return err
	}
	r.explanation = reason
	if !nonEmpty(r.explanation) {
		r.explanation = "this check was skipped because the runner was already at capacity"
	}
	return nil
}

// CheckInvariants reports whether the run's invariants hold.
func (r *Run) CheckInvariants() error {
	if !r.state.Valid() {
		return invalidf("run %q is in unknown state %q", r.id, r.state)
	}
	if r.state.Terminal() && r.endedAt.IsZero() {
		return invalidf("run %q is terminal but has no end time", r.id)
	}
	if !r.state.Terminal() && !r.endedAt.IsZero() {
		return invalidf("run %q has an end time but is not terminal", r.id)
	}
	if r.state == StateRunning && r.startedAt.IsZero() {
		return invalidf("run %q is running but has no start time", r.id)
	}
	switch r.state {
	case StateDegraded, StateFailed:
		if r.failure == nil {
			return invalidf("run %q ended %s without a classified failure", r.id, r.state)
		}
		if err := r.failure.Validate(); err != nil {
			return err
		}
	case StateQuiet, StateChanged:
		if r.failure != nil {
			return invalidf("run %q succeeded but carries a failure", r.id)
		}
	}
	return nil
}
