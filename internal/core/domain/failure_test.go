package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/champion19007/agentd/internal/core/domain"
)

// classifiedError is the shape an adapter uses to report a failure it
// understands. It is a few lines rather than a generated mock because a mock
// of a one-method interface is just a worse version of this.
type classifiedError struct {
	class domain.FailureClass
	msg   string
}

func (e classifiedError) Error() string                     { return e.msg }
func (e classifiedError) FailureClass() domain.FailureClass { return e.class }

// TestClassifyDefaultsToTransient is the single most important test in this
// package. An unrecognised error that classified as structural would open a
// repair incident and ask a human to approve a new binding for a source that
// may be perfectly healthy -- Agentd guessing its way into a repair, which is
// exactly what it promises not to do.
func TestClassifyDefaultsToTransient(t *testing.T) {
	unknown := []error{
		errors.New("something went wrong"),
		fmt.Errorf("wrapped: %w", errors.New("connection reset by peer")),
		fmt.Errorf("no such element: div.price"), // reads structural, is not classified
	}

	for _, err := range unknown {
		t.Run(err.Error(), func(t *testing.T) {
			got := domain.Classify(err)

			if got.Class != domain.ClassTransient {
				t.Errorf("Classify(%v).Class = %q, want %q", err, got.Class, domain.ClassTransient)
			}
			if got.Class == domain.ClassStructural {
				t.Error("an unclassified error must never become structural")
			}
			if got.Detail != err.Error() {
				t.Errorf("Detail = %q, want the original error text", got.Detail)
			}
			if got.Summary == "" {
				t.Error("even an unrecognised failure needs something a human can read")
			}
		})
	}
}

func TestClassifyPreservesAnAdaptersClassification(t *testing.T) {
	classes := []domain.FailureClass{
		domain.ClassTransient, domain.ClassRateLimited, domain.ClassAuth,
		domain.ClassStructural, domain.ClassSemantic, domain.ClassFatal,
	}

	for _, class := range classes {
		t.Run(string(class), func(t *testing.T) {
			err := classifiedError{class: class, msg: "adapter said so"}

			if got := domain.Classify(err).Class; got != class {
				t.Errorf("Classify.Class = %q, want %q", got, class)
			}
			// Classification must survive wrapping, because an adapter's
			// error usually travels up through a few layers of context.
			wrapped := fmt.Errorf("fetching %s: %w", "https://example.test", err)
			if got := domain.Classify(wrapped).Class; got != class {
				t.Errorf("Classify(wrapped).Class = %q, want %q", got, class)
			}
		})
	}
}

func TestClassifyRejectsAnUnknownClassFromAnAdapter(t *testing.T) {
	// An adapter that invents a class is a bug, and the safe reading of a bug
	// is transient. Trusting the string would let a typo open an incident.
	err := classifiedError{class: "catastrophic", msg: "adapter invented a class"}

	if got := domain.Classify(err).Class; got != domain.ClassTransient {
		t.Errorf("Classify.Class = %q, want %q", got, domain.ClassTransient)
	}
}

func TestClassifyKeepsAFailureTravellingAsAnError(t *testing.T) {
	original := domain.Failure{
		Class:   domain.ClassAuth,
		Code:    "token_expired",
		Summary: "the API token for this check has expired",
	}

	got := domain.Classify(fmt.Errorf("fetch: %w", original))

	if got.Class != domain.ClassAuth {
		t.Errorf("Class = %q, want %q", got.Class, domain.ClassAuth)
	}
	if got.Code != original.Code {
		t.Errorf("Code = %q, want %q", got.Code, original.Code)
	}
	if got.Summary != original.Summary {
		t.Errorf("Summary = %q, want the operator-facing text to survive", got.Summary)
	}
}

func TestClassifyNilIsTheZeroFailure(t *testing.T) {
	if got := domain.Classify(nil); got != (domain.Failure{}) {
		t.Errorf("Classify(nil) = %+v, want the zero Failure", got)
	}
}

// TestFailureClassBehaviour pins the decisions each class drives. These are
// the branches the rest of the core will switch on, so an accidental change
// here would quietly change what Agentd does about a broken check.
func TestFailureClassBehaviour(t *testing.T) {
	tests := []struct {
		class         domain.FailureClass
		retryable     bool
		opensIncident bool
		degrades      bool
	}{
		{domain.ClassTransient, true, false, false},
		{domain.ClassRateLimited, true, false, true},
		{domain.ClassAuth, false, false, false},
		{domain.ClassStructural, false, true, false},
		{domain.ClassSemantic, false, false, true},
		{domain.ClassFatal, false, false, false},
	}

	if len(tests) != 6 {
		t.Fatalf("the taxonomy has %d classes covered, want all 6", len(tests))
	}

	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			if !tt.class.Valid() {
				t.Fatalf("%q should be a valid class", tt.class)
			}
			if got := tt.class.Retryable(); got != tt.retryable {
				t.Errorf("Retryable = %v, want %v", got, tt.retryable)
			}
			if got := tt.class.OpensIncident(); got != tt.opensIncident {
				t.Errorf("OpensIncident = %v, want %v", got, tt.opensIncident)
			}
			if got := tt.class.Degrades(); got != tt.degrades {
				t.Errorf("Degrades = %v, want %v", got, tt.degrades)
			}
		})
	}
}

func TestOnlyStructuralFailuresOpenIncidents(t *testing.T) {
	// Repair is expensive and asks for a human's attention, so exactly one
	// class may trigger it.
	opens := 0
	for _, class := range []domain.FailureClass{
		domain.ClassTransient, domain.ClassRateLimited, domain.ClassAuth,
		domain.ClassStructural, domain.ClassSemantic, domain.ClassFatal,
	} {
		if class.OpensIncident() {
			opens++
		}
	}
	if opens != 1 {
		t.Errorf("%d classes open an incident, want exactly 1 (structural)", opens)
	}
}

func TestUnknownClassIsNotValid(t *testing.T) {
	if domain.FailureClass("weird").Valid() {
		t.Error("an unknown class must not validate")
	}
	if domain.FailureClass("").Valid() {
		t.Error("the empty class must not validate")
	}
}

func TestFailureNeedsSomethingAHumanCanRead(t *testing.T) {
	if err := (domain.Failure{Class: domain.ClassAuth}).Validate(); err == nil {
		t.Error("a failure with no summary should not validate")
	}
	if err := (domain.Failure{Class: "nope", Summary: "x"}).Validate(); err == nil {
		t.Error("a failure with an unknown class should not validate")
	}
	if err := (domain.Failure{Class: domain.ClassAuth, Summary: "the token expired"}).Validate(); err != nil {
		t.Errorf("a well formed failure should validate, got %v", err)
	}
}
