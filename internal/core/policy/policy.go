// Package policy holds the rules that decide what happens after a run: when a
// failure is worth telling a human about, when breakage escalates into a
// repair incident, and what may never happen without approval.
//
// Everything here is a pure function of a Situation. Policy decides; the run
// orchestrator acts. Keeping the two apart means every alerting rule can be
// table-tested without a store, a clock or a notifier, and it means a change
// in what Agentd says to an operator is a change in one readable place.
package policy

import (
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

// Situation is everything the policy needs to know about a finished run. It
// is a value, so a decision is reproducible from it alone.
type Situation struct {
	// Check is the check that ran.
	Check *domain.Check

	// Run is the finished run. Policy is only consulted on terminal runs.
	Run *domain.Run

	// Attempt is which try within this slot has just finished, counting
	// from 1.
	Attempt int

	// IncidentOpen reports whether the check already has an open incident.
	// A check that is already known to be broken does not open a second one.
	IncidentOpen bool

	// HasKnownGood reports whether a known-good snapshot exists to verify a
	// repair against. Without one there is nothing to verify a candidate
	// binding with, and proposing an unverified repair is the guess Agentd
	// promises not to make.
	HasKnownGood bool
}

// Decision is what should happen next. It describes actions rather than
// performing them.
type Decision struct {
	// Retry asks for another attempt within the same slot.
	Retry bool

	// RetryAfter is how long to wait before that attempt.
	RetryAfter time.Duration

	// OpenIncident asks for a repair incident to be opened.
	OpenIncident bool

	// AttemptRepair asks for a repair to be proposed. It is never set
	// without OpenIncident or an already-open incident, and never set
	// without a known-good snapshot to verify against.
	AttemptRepair bool

	// ResolveIncident asks for an open incident to be closed because the
	// check is working again.
	ResolveIncident bool

	// Notify asks for the operator to be told.
	Notify bool

	// Severity ranks the notification.
	Severity domain.Severity

	// Subject and Body are the message, in the operator's terms.
	Subject string
	Body    string
}

// Decide works out what to do about a finished run.
//
// The shape of this function is the product's character in one place. Quiet
// runs say nothing unless asked to. Degraded runs speak up but do not
// escalate. Only a structural failure reaches for a repair, and even then the
// most it can do is ask.
func Decide(s Situation) Decision {
	if s.Run == nil || s.Check == nil || !s.Run.State().Terminal() {
		return Decision{}
	}

	name := s.Check.ActiveDefinition().Intent.Name()

	switch s.Run.State() {
	case domain.StateQuiet:
		return quiet(s, name)

	case domain.StateChanged:
		d := Decision{
			Notify:   true,
			Severity: domain.SeverityInfo,
			Subject:  name + " changed",
			Body:     s.Run.Explanation(),
		}
		// A change proves the check works, so anything open about it is over.
		d.ResolveIncident = s.IncidentOpen
		return d

	case domain.StateDegraded:
		return degraded(s, name)

	case domain.StateFailed:
		return failed(s, name)

	case domain.StateInterrupted:
		// Agentd stopped; the source did nothing wrong. Saying anything here
		// would train the operator to ignore Agentd's messages.
		return Decision{}

	case domain.StateSkippedOverload:
		// This one does speak, even though nothing is broken. A skipped slot
		// is a hole in the record, and a silence with an unexplained hole in
		// it is not a silence anyone should trust.
		return Decision{
			Notify:   true,
			Severity: domain.SeverityWarning,
			Subject:  name + " was skipped",
			Body:     s.Run.Explanation(),
		}
	}
	return Decision{}
}

// quiet decides about a run where nothing changed, which should be the
// overwhelming majority of runs.
func quiet(s Situation, name string) Decision {
	d := Decision{ResolveIncident: s.IncidentOpen}
	if s.Check.ActiveDefinition().Destination.OnQuiet {
		d.Notify = true
		d.Severity = domain.SeverityInfo
		d.Subject = name + " is unchanged"
		d.Body = s.Run.Explanation()
	}
	return d
}

// degraded decides about a run that produced a thinner result than hoped.
func degraded(s Situation, name string) Decision {
	d := Decision{
		Notify:   true,
		Severity: domain.SeverityWarning,
		Subject:  name + " was checked with reduced confidence",
		Body:     s.Run.Explanation(),
	}
	// A degraded run still extracted something, so the binding is doing its
	// job and there is nothing to repair. Escalating here would spend a
	// repair budget on a working check.
	if f := s.Run.Failure(); f != nil && f.Class.Retryable() {
		d.Retry, d.RetryAfter = retry(s)
	}
	return d
}

// failed decides about a run that produced nothing usable.
func failed(s Situation, name string) Decision {
	f := s.Run.Failure()
	if f == nil {
		// CheckInvariants forbids this, so reaching it means a bug rather
		// than a broken source. Say so instead of guessing.
		return Decision{
			Notify:   true,
			Severity: domain.SeverityAlert,
			Subject:  name + " failed",
			Body:     "the check failed and Agentd could not classify why; this is a bug in Agentd, not in the source",
		}
	}

	d := Decision{
		Notify:   true,
		Severity: domain.SeverityAlert,
		Subject:  name + " could not be checked",
		Body:     f.Summary,
	}

	if f.Class.Retryable() {
		d.Retry, d.RetryAfter = retry(s)
		if d.Retry {
			// Nothing is wrong yet; it may well work in thirty seconds.
			// Telling the operator now would be crying wolf.
			d.Notify = false
			d.Severity = ""
		}
		return d
	}

	if !f.Class.OpensIncident() {
		// Auth and fatal failures need a person, not a repair. Naming what
		// kind of attention is needed is the difference between a useful
		// alert and a pager going off.
		switch f.Class {
		case domain.ClassAuth:
			d.Body = f.Summary + " -- this needs a credential updated; Agentd cannot fix it and will not keep retrying."
		case domain.ClassFatal:
			d.Body = f.Summary + " -- Agentd has stopped running this check."
		}
		return d
	}

	// Structural: the source changed shape.
	d.Subject = name + " is no longer where Agentd expects it"
	d.OpenIncident = !s.IncidentOpen

	if !s.HasKnownGood {
		d.Body = f.Summary + " -- Agentd has no known-good capture of this source to verify a repair against, so it will not propose one."
		return d
	}

	d.AttemptRepair = true
	d.Body = f.Summary + " -- Agentd will try to work out a new way to find it and will ask you before changing anything."
	return d
}

// retry works out whether another attempt is worth making, and when.
func retry(s Situation) (bool, time.Duration) {
	pol := s.Check.ActiveDefinition().Policy
	if s.Attempt >= pol.Retries() {
		return false, 0
	}
	return true, pol.Backoff(s.Attempt)
}

// Notification turns a Decision into a deliverable message for a check.
// It returns false when the decision asks for no notification, so that the
// caller never has to guess whether a zero Notification means silence.
func Notification(s Situation, d Decision, at time.Time) (domain.Notification, bool) {
	if !d.Notify {
		return domain.Notification{}, false
	}
	return domain.Notification{
		CheckID:     s.Check.ID(),
		Destination: s.Check.ActiveDefinition().Destination,
		Severity:    d.Severity,
		Subject:     d.Subject,
		Body:        d.Body,
		OccurredAt:  at,
	}, true
}

// ApprovalRequest builds the message that asks a human to decide about a
// proposed repair. It is the only notification Agentd sends that expects an
// answer, and it exists as its own constructor so that the asking is never
// accidentally worded as telling.
func ApprovalRequest(c *domain.Check, i *domain.Incident, at time.Time) (domain.Notification, bool) {
	p := i.Proposal()
	if p == nil {
		return domain.Notification{}, false
	}
	name := c.ActiveDefinition().Intent.Name()
	return domain.Notification{
		CheckID:       c.ID(),
		Destination:   c.ActiveDefinition().Destination,
		Severity:      domain.SeverityAlert,
		Subject:       "Approve a repair for " + name + "?",
		Body:          p.Rationale + "\n\nAgentd has checked this against a known-good capture and it reproduces what you asked for. Nothing has been changed. Approve it to apply it.",
		NeedsDecision: true,
		IncidentID:    i.ID(),
		OccurredAt:    at,
	}, true
}
