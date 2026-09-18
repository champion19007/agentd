package domain

import (
	"errors"
	"time"
)

var (
	// ErrIncidentOpen reports an attempt to open a second incident for a
	// check that already has one open.
	ErrIncidentOpen = errors.New("this check already has an open incident")

	// ErrIncidentClosed reports an attempt to work on a closed incident.
	ErrIncidentClosed = errors.New("incident is closed")

	// ErrRepairBudgetExhausted reports that an incident has used its bounded
	// number of repair attempts. Agentd stops trying and says so rather than
	// grinding against a source it cannot read.
	ErrRepairBudgetExhausted = errors.New("repair attempt budget exhausted")

	// ErrNoProposal reports an approval or rejection with nothing to act on.
	ErrNoProposal = errors.New("incident has no proposal")

	// ErrNotApproved reports an attempt to resolve an incident whose repair a
	// human has not approved. This is the guard behind the product's central
	// promise, so it is a distinct, testable error rather than a condition
	// buried in a branch.
	ErrNotApproved = errors.New("repair has not been approved by a human")
)

// IncidentState is where an incident is in its life.
type IncidentState string

const (
	// IncidentOpen means Agentd has noticed a structural break and is
	// working on it.
	IncidentOpen IncidentState = "open"

	// IncidentAwaitingApproval means a repair has been proposed and
	// verified, and is waiting for a human. Agentd will wait here
	// indefinitely; it never times out into applying the repair itself.
	IncidentAwaitingApproval IncidentState = "awaiting_approval"

	// IncidentResolved means an approved repair was applied, or the source
	// started working again on its own.
	IncidentResolved IncidentState = "resolved"

	// IncidentAbandoned means Agentd ran out of repair attempts, or a human
	// rejected the proposal. The check stays broken and loudly so.
	IncidentAbandoned IncidentState = "abandoned"
)

// Open reports whether the incident is still live.
func (s IncidentState) Open() bool {
	return s == IncidentOpen || s == IncidentAwaitingApproval
}

// Valid reports whether s is a known state.
func (s IncidentState) Valid() bool {
	switch s {
	case IncidentOpen, IncidentAwaitingApproval, IncidentResolved, IncidentAbandoned:
		return true
	}
	return false
}

// AttemptOutcome is how one repair attempt ended.
type AttemptOutcome string

const (
	// AttemptProposed means the model produced a candidate that passed
	// verification against stored evidence.
	AttemptProposed AttemptOutcome = "proposed"

	// AttemptUnverified means the candidate did not reproduce the intent
	// when replayed against a known-good snapshot. It is discarded, never
	// shown to the operator as a suggestion, because an unverified repair is
	// exactly the guess Agentd promises not to make.
	AttemptUnverified AttemptOutcome = "unverified"

	// AttemptModelError means the model could not be reached or returned
	// nothing usable.
	AttemptModelError AttemptOutcome = "model_error"
)

// RepairAttempt is the record of one try, kept whether or not it worked so
// that an operator can see what was tried on their behalf.
type RepairAttempt struct {
	// Number is the attempt's position, starting at 1.
	Number int

	// At is when the attempt was made.
	At time.Time

	// Outcome is how it ended.
	Outcome AttemptOutcome

	// Note explains the outcome in the operator's terms.
	Note string
}

// ApprovalState tracks the human decision on a proposal.
type ApprovalState string

const (
	// ApprovalPending is the only state a freshly verified proposal can be
	// in. Nothing is applied from here.
	ApprovalPending ApprovalState = "pending"
	// ApprovalApproved means a human said yes.
	ApprovalApproved ApprovalState = "approved"
	// ApprovalRejected means a human said no.
	ApprovalRejected ApprovalState = "rejected"
)

// Gate is one check a candidate binding must pass before a human is asked to
// look at it.
//
// The gates are recorded individually rather than collapsed into one pass or
// fail because "it failed verification" is not an answer anybody can act on.
// An operator deciding whether to trust the next proposal wants to know
// whether the candidate was malformed, or found nothing, or found the wrong
// thing -- those are three different stories about how well Agentd understands
// their source.
type Gate string

const (
	// GateWellFormed: the candidate is a valid binding at all.
	GateWellFormed Gate = "well_formed"
	// GateCovers: it has a locator for every target the intent requires.
	GateCovers Gate = "covers_intent"
	// GateReplays: it ran against the stored capture without erroring.
	GateReplays Gate = "replays"
	// GateSatisfies: what it extracted satisfies the intent.
	GateSatisfies Gate = "satisfies_intent"
	// GateShape: what it extracted looks like what the check used to get.
	// This is the gate that catches a locator pointing at the wrong element
	// of the right type, which every other gate would wave through.
	GateShape Gate = "matches_previous_shape"
)

// RequiredGates are the gates every proposal must have passed. GateShape is
// not among them: a check with no stored previous result has nothing to
// compare against, and refusing to help such a check would be worse than
// proposing a repair that has passed everything else.
var RequiredGates = []Gate{GateWellFormed, GateCovers, GateReplays, GateSatisfies}

// Valid reports whether g is a known gate.
func (g Gate) Valid() bool {
	switch g {
	case GateWellFormed, GateCovers, GateReplays, GateSatisfies, GateShape:
		return true
	}
	return false
}

// GateResult is one gate's verdict.
type GateResult struct {
	Gate   Gate
	Passed bool
	// Detail explains the verdict in the operator's terms.
	Detail string
	At     time.Time
}

// gatesPassed reports whether every required gate is present and passed.
func gatesPassed(results []GateResult) error {
	seen := make(map[Gate]bool, len(results))
	for _, r := range results {
		if !r.Gate.Valid() {
			return invalidf("proposal carries unknown verification gate %q", r.Gate)
		}
		if !r.Passed {
			return invalidf("proposal failed the %q gate: %s", r.Gate, r.Detail)
		}
		seen[r.Gate] = true
	}
	for _, g := range RequiredGates {
		if !seen[g] {
			return invalidf("proposal was never checked against the %q gate", g)
		}
	}
	return nil
}

// RepairProposal is a candidate binding that has been verified against stored
// evidence and is waiting for a human decision.
//
// A proposal is data, not an action. Nothing in this package applies one; the
// only path from proposal to active binding runs through Approve, and Approve
// requires a named approver.
type RepairProposal struct {
	// Binding is the candidate. Its Origin is OriginRepaired.
	Binding Binding

	// Rationale is the model's explanation, in the operator's terms.
	Rationale string

	// VerifiedAgainst is the stored capture the candidate was replayed
	// against.
	//
	// This is the current capture, not the known-good one. A repair is
	// derived for the shape the source has now, so replaying it against the
	// old capture would fail by construction. The known-good capture's role
	// is to supply the result the candidate's output is compared to.
	VerifiedAgainst SnapshotID

	// VerifiedAt is when that replay happened.
	VerifiedAt time.Time

	// Gates are the verdicts of every check the candidate had to pass.
	// Validate refuses a proposal that is missing one or failed one, so an
	// unverified candidate cannot reach an operator as a suggestion.
	Gates []GateResult

	// ProposedLocators is what Agentd suggested, kept separately from the
	// binding's own locators so that an edit does not erase the suggestion.
	// What Agentd proposed and what a human approved are different facts, and
	// the difference is the interesting one.
	ProposedLocators []Locator

	// Edited reports whether a human changed the locators before approving.
	Edited bool

	// Approval is the human decision, pending until one is made.
	Approval ApprovalState

	// ApprovedBy names whoever decided. Empty while pending.
	ApprovedBy string

	// DecidedAt is when the decision was made.
	DecidedAt time.Time
}

// Validate reports whether p is a proposal Agentd may show to a human.
func (p RepairProposal) Validate() error {
	if err := p.Binding.Validate(); err != nil {
		return err
	}
	if p.Binding.Origin != OriginRepaired {
		return invalidf("a repair proposal's binding must have origin %q, got %q", OriginRepaired, p.Binding.Origin)
	}
	if !nonEmpty(string(p.VerifiedAgainst)) {
		return invalidf("a repair proposal must name the snapshot it was verified against")
	}
	if p.VerifiedAt.IsZero() {
		return invalidf("a repair proposal must record when it was verified")
	}
	if !nonEmpty(p.Rationale) {
		return invalidf("a repair proposal must explain itself to the operator")
	}
	if err := gatesPassed(p.Gates); err != nil {
		return err
	}
	return nil
}

// Incident is one episode of a check being structurally broken, from the
// break to its resolution or abandonment.
//
// Invariants: at most one incident per check is open at a time, and the
// number of repair attempts is bounded.
type Incident struct {
	id       IncidentID
	checkID  CheckID
	state    IncidentState
	cause    Failure
	openedAt time.Time
	closedAt time.Time

	maxAttempts int
	attempts    []RepairAttempt
	proposal    *RepairProposal
	resolution  string
}

// NewIncident opens an incident for a structural failure. Callers should
// normally go through an IncidentLog, which enforces the one-open-per-check
// invariant.
func NewIncident(id IncidentID, checkID CheckID, cause Failure, maxAttempts int, at time.Time) (*Incident, error) {
	if !nonEmpty(string(id)) {
		return nil, invalidf("incident needs an id")
	}
	if !nonEmpty(string(checkID)) {
		return nil, invalidf("incident %q needs a check id", id)
	}
	if err := cause.Validate(); err != nil {
		return nil, err
	}
	if !cause.Class.OpensIncident() {
		return nil, invalidf("a %s failure does not open a repair incident", cause.Class)
	}
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxRepairAttempts
	}
	return &Incident{
		id:          id,
		checkID:     checkID,
		state:       IncidentOpen,
		cause:       cause,
		openedAt:    at.UTC(),
		maxAttempts: maxAttempts,
	}, nil
}

// ID returns the incident's identifier.
func (i *Incident) ID() IncidentID { return i.id }

// CheckID returns the affected check.
func (i *Incident) CheckID() CheckID { return i.checkID }

// State returns the incident's state.
func (i *Incident) State() IncidentState { return i.state }

// Open reports whether the incident is still live.
func (i *Incident) Open() bool { return i.state.Open() }

// Cause returns the failure that opened the incident.
func (i *Incident) Cause() Failure { return i.cause }

// OpenedAt and ClosedAt return the incident's timeline.
func (i *Incident) OpenedAt() time.Time { return i.openedAt }
func (i *Incident) ClosedAt() time.Time { return i.closedAt }

// Attempts returns the repair attempts made so far. The slice is a copy.
func (i *Incident) Attempts() []RepairAttempt {
	out := make([]RepairAttempt, len(i.attempts))
	copy(out, i.attempts)
	return out
}

// AttemptsRemaining returns how many repair attempts are left in the budget.
func (i *Incident) AttemptsRemaining() int {
	left := i.maxAttempts - len(i.attempts)
	if left < 0 {
		return 0
	}
	return left
}

// Proposal returns the current proposal, or nil. The returned pointer is to a
// copy, so a caller cannot approve a proposal by writing to it.
func (i *Incident) Proposal() *RepairProposal {
	if i.proposal == nil {
		return nil
	}
	p := *i.proposal
	return &p
}

// Resolution explains how the incident ended, in the operator's terms.
func (i *Incident) Resolution() string { return i.resolution }

// RecordAttempt logs a repair attempt against the budget. It returns
// ErrRepairBudgetExhausted when the budget is already spent, without
// recording anything.
func (i *Incident) RecordAttempt(outcome AttemptOutcome, note string, at time.Time) (RepairAttempt, error) {
	if !i.state.Open() {
		return RepairAttempt{}, ErrIncidentClosed
	}
	if i.AttemptsRemaining() == 0 {
		return RepairAttempt{}, ErrRepairBudgetExhausted
	}
	a := RepairAttempt{
		Number:  len(i.attempts) + 1,
		At:      at.UTC(),
		Outcome: outcome,
		Note:    note,
	}
	i.attempts = append(i.attempts, a)
	return a, nil
}

// Propose attaches a verified proposal and moves the incident to awaiting
// approval. It does not apply anything.
func (i *Incident) Propose(p RepairProposal, at time.Time) error {
	if !i.state.Open() {
		return ErrIncidentClosed
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Binding.CheckID != i.checkID {
		return invalidf("proposal for check %q cannot be attached to an incident for check %q", p.Binding.CheckID, i.checkID)
	}
	p.Approval = ApprovalPending
	p.ApprovedBy = ""
	p.DecidedAt = time.Time{}
	p.Edited = false
	// Remember the suggestion before anyone can change it.
	p.ProposedLocators = append([]Locator(nil), p.Binding.Locators...)
	i.proposal = &p
	i.state = IncidentAwaitingApproval
	return nil
}

// Approve records a human's approval of the proposal. The approver must be
// named: an approval with nobody attached to it is indistinguishable from
// Agentd approving its own work.
func (i *Incident) Approve(by string, at time.Time) error {
	if !i.state.Open() {
		return ErrIncidentClosed
	}
	if i.proposal == nil {
		return ErrNoProposal
	}
	if !nonEmpty(by) {
		return invalidf("an approval must name the person who gave it")
	}
	i.proposal.Approval = ApprovalApproved
	i.proposal.ApprovedBy = by
	i.proposal.DecidedAt = at.UTC()
	return nil
}

// ApproveWithEdits records an approval of a proposal the operator changed
// first.
//
// This is the common case in practice and the one most worth modelling
// honestly: Agentd found roughly the right place, a person who knows the
// source corrected it, and the corrected version is what gets applied. What
// Agentd suggested is kept in ProposedLocators, because the gap between the
// two is the only honest measure of how well Agentd understands this source.
//
// The edited locators are not re-verified here. They were written by a person
// who can see the page, and demanding that a human's correction pass Agentd's
// own gates would be Agentd second-guessing the authority it just deferred to.
// The audit trail records that they were edited and by whom.
func (i *Incident) ApproveWithEdits(by string, locators []Locator, at time.Time) error {
	if !i.state.Open() {
		return ErrIncidentClosed
	}
	if i.proposal == nil {
		return ErrNoProposal
	}
	if !nonEmpty(by) {
		return invalidf("an approval must name the person who gave it")
	}
	if len(locators) == 0 {
		return invalidf("an edited repair needs at least one locator")
	}
	for _, l := range locators {
		if err := l.Validate(); err != nil {
			return err
		}
	}

	edited := append([]Locator(nil), locators...)
	i.proposal.Binding.Locators = edited
	i.proposal.Edited = !sameLocators(i.proposal.ProposedLocators, edited)
	i.proposal.Approval = ApprovalApproved
	i.proposal.ApprovedBy = by
	i.proposal.DecidedAt = at.UTC()
	return nil
}

// sameLocators reports whether two locator lists are identical, so that an
// "edit" that changed nothing is not recorded as one.
func sameLocators(a, b []Locator) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Reject records a human's refusal and abandons the incident. The check stays
// broken, which is the correct loud outcome.
func (i *Incident) Reject(by, reason string, at time.Time) error {
	if !i.state.Open() {
		return ErrIncidentClosed
	}
	if i.proposal == nil {
		return ErrNoProposal
	}
	if !nonEmpty(by) {
		return invalidf("a rejection must name the person who gave it")
	}
	i.proposal.Approval = ApprovalRejected
	i.proposal.ApprovedBy = by
	i.proposal.DecidedAt = at.UTC()
	i.state = IncidentAbandoned
	i.closedAt = at.UTC()
	i.resolution = reason
	if !nonEmpty(i.resolution) {
		i.resolution = "the proposed repair was rejected; this check is still broken"
	}
	return nil
}

// ApprovedBinding returns the binding an approved proposal authorises, and is
// the only way to get one out of an incident.
//
// This is the narrow gate the product's central promise rests on: there is no
// path from a proposal to an active binding that does not pass through a
// human approval recorded on the proposal.
func (i *Incident) ApprovedBinding() (Binding, error) {
	if i.proposal == nil {
		return Binding{}, ErrNoProposal
	}
	if i.proposal.Approval != ApprovalApproved {
		return Binding{}, ErrNotApproved
	}
	return i.proposal.Binding, nil
}

// ResolveWithRepair closes the incident after an approved repair has been
// applied. It refuses if no human approved.
func (i *Incident) ResolveWithRepair(at time.Time) error {
	if !i.state.Open() {
		return ErrIncidentClosed
	}
	if _, err := i.ApprovedBinding(); err != nil {
		return err
	}
	i.state = IncidentResolved
	i.closedAt = at.UTC()
	i.resolution = "an operator approved a repaired binding and it was applied"
	return nil
}

// ResolveRecovered closes the incident because the check started working
// again without a repair, which happens when a source change is reverted.
func (i *Incident) ResolveRecovered(at time.Time) error {
	if !i.state.Open() {
		return ErrIncidentClosed
	}
	i.state = IncidentResolved
	i.closedAt = at.UTC()
	i.resolution = "the check started working again on its own; no repair was applied"
	return nil
}

// Abandon closes the incident without a repair, which is what happens when
// the attempt budget runs out.
func (i *Incident) Abandon(reason string, at time.Time) error {
	if !i.state.Open() {
		return ErrIncidentClosed
	}
	i.state = IncidentAbandoned
	i.closedAt = at.UTC()
	i.resolution = reason
	if !nonEmpty(i.resolution) {
		i.resolution = "Agentd could not repair this check and has stopped trying"
	}
	return nil
}

// CheckInvariants reports whether the incident's invariants hold.
func (i *Incident) CheckInvariants() error {
	if !i.state.Valid() {
		return invalidf("incident %q is in unknown state %q", i.id, i.state)
	}
	if len(i.attempts) > i.maxAttempts {
		return invalidf("incident %q made %d repair attempts against a budget of %d", i.id, len(i.attempts), i.maxAttempts)
	}
	for n, a := range i.attempts {
		if a.Number != n+1 {
			return invalidf("incident %q has attempts numbered out of order", i.id)
		}
	}
	if !i.state.Open() && i.closedAt.IsZero() {
		return invalidf("incident %q is closed but has no close time", i.id)
	}
	if i.state.Open() && !i.closedAt.IsZero() {
		return invalidf("incident %q has a close time but is still open", i.id)
	}
	if i.state == IncidentAwaitingApproval && i.proposal == nil {
		return invalidf("incident %q is awaiting approval with no proposal", i.id)
	}
	if i.state == IncidentResolved && i.proposal != nil {
		if i.proposal.Approval == ApprovalPending {
			return invalidf("incident %q was resolved with an unapproved proposal", i.id)
		}
	}
	return nil
}

// IncidentLog is the set of incidents for one check, and the place the
// one-open-per-check invariant lives.
type IncidentLog struct {
	checkID   CheckID
	incidents []*Incident
}

// NewIncidentLog creates an empty log for a check.
func NewIncidentLog(checkID CheckID) *IncidentLog {
	return &IncidentLog{checkID: checkID}
}

// CheckID returns the check this log belongs to.
func (l *IncidentLog) CheckID() CheckID { return l.checkID }

// Len returns how many incidents the check has had.
func (l *IncidentLog) Len() int { return len(l.incidents) }

// Current returns the open incident, if there is one.
func (l *IncidentLog) Current() (*Incident, bool) {
	for _, i := range l.incidents {
		if i.Open() {
			return i, true
		}
	}
	return nil, false
}

// Open starts a new incident, refusing if one is already open. Refusing is
// what keeps a check that fails every five minutes from opening a hundred
// incidents overnight and burning its repair budget a hundred times over.
func (l *IncidentLog) Open(id IncidentID, cause Failure, maxAttempts int, at time.Time) (*Incident, error) {
	if _, open := l.Current(); open {
		return nil, ErrIncidentOpen
	}
	i, err := NewIncident(id, l.checkID, cause, maxAttempts, at)
	if err != nil {
		return nil, err
	}
	l.incidents = append(l.incidents, i)
	return i, nil
}

// All returns every incident, oldest first. The slice is a copy, though the
// incidents themselves are shared.
func (l *IncidentLog) All() []*Incident {
	out := make([]*Incident, len(l.incidents))
	copy(out, l.incidents)
	return out
}

// CheckInvariants reports whether the log's invariants hold.
func (l *IncidentLog) CheckInvariants() error {
	open := 0
	for _, i := range l.incidents {
		if i.checkID != l.checkID {
			return invalidf("log for check %q holds an incident for check %q", l.checkID, i.checkID)
		}
		if err := i.CheckInvariants(); err != nil {
			return err
		}
		if i.Open() {
			open++
		}
	}
	if open > 1 {
		return invalidf("check %q has %d open incidents, want at most 1", l.checkID, open)
	}
	return nil
}
