package domain

import (
	"errors"
	"fmt"
)

// FailureClass is the taxonomy Agentd reasons about. Everything downstream --
// whether to retry, whether to wake a human, whether to attempt a repair --
// is decided from the class, never from an error string.
type FailureClass string

const (
	// ClassTransient is a failure expected to resolve on its own: a timeout,
	// a connection reset, a 503. Retry.
	ClassTransient FailureClass = "transient"

	// ClassRateLimited is a source asking to be left alone for a while.
	// Retry, but more slowly.
	ClassRateLimited FailureClass = "rate_limited"

	// ClassAuth is a credential that is missing, expired or rejected. Only a
	// human can fix it, so retrying is noise.
	ClassAuth FailureClass = "auth"

	// ClassStructural is the signal Agentd exists for: the source was
	// reached and read, but the binding no longer locates the intent. The
	// source changed shape. This is what opens a repair incident.
	ClassStructural FailureClass = "structural"

	// ClassSemantic is extraction that succeeded structurally but produced a
	// value that cannot be true: a price of -1, a date in the year 3000.
	// The binding may be pointing at the wrong thing.
	ClassSemantic FailureClass = "semantic"

	// ClassFatal is a misconfiguration Agentd cannot work around, such as an
	// intent that no longer parses. Stop and say so.
	ClassFatal FailureClass = "fatal"
)

// Valid reports whether c is one of the six known classes.
func (c FailureClass) Valid() bool {
	switch c {
	case ClassTransient, ClassRateLimited, ClassAuth, ClassStructural, ClassSemantic, ClassFatal:
		return true
	}
	return false
}

// Retryable reports whether running the same check again, unchanged, could
// plausibly succeed. Auth, structural, semantic and fatal failures all need
// something to change first, so retrying them only burns quota and erodes
// trust in the silence.
func (c FailureClass) Retryable() bool {
	return c == ClassTransient || c == ClassRateLimited
}

// OpensIncident reports whether a failure of this class warrants opening a
// repair incident. Only structural failures do: they are the case where the
// intent is still valid but the way of reaching it is not.
//
// Semantic failures deliberately do not open one. A nonsensical value might
// mean a stale binding, but it might equally mean the source published bad
// data, and proposing a new binding for a source that is simply wrong today
// would be Agentd inventing a problem to solve. Semantic failures degrade the
// run and surface to a human instead.
func (c FailureClass) OpensIncident() bool { return c == ClassStructural }

// Degrades reports whether a failure of this class still leaves a usable, if
// diminished, result. A degraded run is not a failed run, and the two must
// not be collapsed: a rate-limited check that served its last known-good
// value is in a different situation from one that has no value at all.
func (c FailureClass) Degrades() bool {
	return c == ClassRateLimited || c == ClassSemantic
}

// Failure is a classified problem, described in terms the operator can act on.
type Failure struct {
	// Class drives every decision Agentd makes about this failure.
	Class FailureClass

	// Code is a short stable identifier for this kind of failure, suitable
	// for grouping in a UI. Optional.
	Code string

	// Summary explains what went wrong in the user's terms. It is not a
	// stack trace and not a provider error passed through verbatim.
	Summary string

	// Detail carries the underlying error text for an operator who asks for
	// it. It is never shown by default.
	Detail string
}

// Validate reports whether f is well formed.
func (f Failure) Validate() error {
	if !f.Class.Valid() {
		return invalidf("failure class %q is not one of the six known classes", f.Class)
	}
	if !nonEmpty(f.Summary) {
		return invalidf("failure needs a summary a human can act on")
	}
	return nil
}

// Error lets a Failure travel as an error.
func (f Failure) Error() string {
	if f.Code != "" {
		return fmt.Sprintf("%s (%s): %s", f.Class, f.Code, f.Summary)
	}
	return fmt.Sprintf("%s: %s", f.Class, f.Summary)
}

// FailureClass satisfies Classified, so a Failure returned as an error keeps
// its classification through Classify.
func (f Failure) FailureClass() FailureClass { return f.Class }

// Classified is implemented by errors that know their own class. An adapter
// that understands its failure modes -- an HTTP adapter seeing 401, an
// extractor finding no match -- signals that by returning an error that
// implements this interface, usually by wrapping a Failure.
type Classified interface {
	error
	FailureClass() FailureClass
}

// Classify turns any error into a Failure.
//
// An error that does not classify itself becomes ClassTransient. This default
// is deliberate and is the safe direction: treating an unknown error as
// transient costs a retry, whereas treating it as structural would open a
// repair incident and ask a human to approve a new binding for a source that
// may be perfectly healthy. Agentd never guesses its way into a repair.
func Classify(err error) Failure {
	if err == nil {
		return Failure{}
	}

	var c Classified
	if errors.As(err, &c) {
		class := c.FailureClass()
		if !class.Valid() {
			class = ClassTransient
		}
		var f Failure
		if errors.As(err, &f) {
			f.Class = class
			if !nonEmpty(f.Summary) {
				f.Summary = err.Error()
			}
			return f
		}
		return Failure{
			Class:   class,
			Summary: err.Error(),
			Detail:  err.Error(),
		}
	}

	return Failure{
		Class:   ClassTransient,
		Code:    "unclassified",
		Summary: "the check could not complete and the cause was not recognised; it will be retried",
		Detail:  err.Error(),
	}
}
