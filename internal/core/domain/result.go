package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// RawResponse is what a Source returned, before anyone has tried to find
// meaning in it.
type RawResponse struct {
	// ContentType is the media type the source reported.
	ContentType string

	// Body is the bytes as received.
	Body []byte

	// Status is the source's own status code where the protocol has one.
	Status int

	// Fingerprint is the shape of this response, computed by the adapter
	// that understands the format. Comparing it to a Binding's fingerprint
	// is how Agentd notices that a source has been redesigned.
	Fingerprint SourceFingerprint

	// FetchedAt is when the fetch completed, supplied by the adapter.
	FetchedAt time.Time
}

// Value is one extracted value, kept as text with its intended type rather
// than as a parsed Go value. The core compares and reports; it does not do
// arithmetic on what it extracts, and keeping the original text means an
// explanation can quote exactly what was on the page.
type Value struct {
	// Text is the extracted text, as found.
	Text string

	// Type is the type the intent expected.
	Type ValueType

	// Missing marks a field the binding located nothing for.
	Missing bool
}

// Record is one extracted record: field name to value.
type Record map[string]Value

// Extraction is the result of applying a Binding to a RawResponse. Exactly
// one of its shapes is populated, matching the intent's kind.
type Extraction struct {
	// Kind says which shape is populated.
	Kind IntentKind

	// Scalar holds the value for a scalar intent.
	Scalar Value

	// Record holds the fields for a record intent.
	Record Record

	// Collection holds the rows for a collection intent.
	Collection []Record
}

// Validate reports whether e is internally consistent: populated in the shape
// its Kind claims.
func (e Extraction) Validate() error {
	switch e.Kind {
	case IntentScalar:
		if e.Record != nil || e.Collection != nil {
			return invalidf("scalar extraction also carries record or collection data")
		}
	case IntentRecord:
		if e.Collection != nil {
			return invalidf("record extraction also carries collection data")
		}
		if e.Record == nil {
			return invalidf("record extraction carries no record")
		}
	case IntentCollection:
		if e.Record != nil {
			return invalidf("collection extraction also carries a single record")
		}
	default:
		return invalidf("extraction has unknown kind %q", e.Kind)
	}
	return nil
}

// Satisfies reports whether e delivers what in asks for, and classifies the
// shortfall when it does not.
//
// The distinction it draws is the one the whole product rests on: a missing
// required field means the binding no longer locates the intent, which is
// structural and opens a repair incident. A missing optional field means less
// was learned than hoped, which degrades the run and tells a human. The two
// must not collapse into one "extraction failed".
func (e Extraction) Satisfies(in Intent) *Failure {
	if in == nil {
		return &Failure{Class: ClassFatal, Summary: "this check has no intent to satisfy"}
	}
	if e.Kind != in.Kind() {
		return &Failure{
			Class:   ClassStructural,
			Code:    "shape_mismatch",
			Summary: "the source no longer has the shape this check expects",
		}
	}

	structural := func(what string) *Failure {
		return &Failure{
			Class:   ClassStructural,
			Code:    "missing_required",
			Summary: "the source no longer contains " + what + " where this check expects it",
		}
	}
	degraded := func(what string) *Failure {
		return &Failure{
			Class:   ClassSemantic,
			Code:    "missing_optional",
			Summary: what + " was not found; the rest of the check still worked",
		}
	}

	switch v := in.(type) {
	case ScalarIntent:
		if e.Scalar.Missing {
			return structural(v.Name())
		}

	case RecordIntent:
		for _, f := range v.Fields {
			got, ok := e.Record[f.Name]
			if (!ok || got.Missing) && f.Required {
				return structural(f.Name)
			}
			if (!ok || got.Missing) && !f.Required {
				return degraded(f.Name)
			}
		}

	case CollectionIntent:
		if len(e.Collection) < v.MinItems {
			return structural(v.Name())
		}
		for _, row := range e.Collection {
			for _, f := range v.Element.RequiredFields() {
				got, ok := row[f.Name]
				if !ok || got.Missing {
					return structural(f.Name)
				}
			}
		}
	}
	return nil
}

// Equal reports whether two extractions carry the same values, which is how a
// quiet run is told from a changed one.
func (e Extraction) Equal(other Extraction) bool {
	if e.Kind != other.Kind {
		return false
	}
	switch e.Kind {
	case IntentScalar:
		return e.Scalar == other.Scalar
	case IntentRecord:
		return recordsEqual(e.Record, other.Record)
	case IntentCollection:
		if len(e.Collection) != len(other.Collection) {
			return false
		}
		for i := range e.Collection {
			if !recordsEqual(e.Collection[i], other.Collection[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func recordsEqual(a, b Record) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av != bv {
			return false
		}
	}
	return true
}

// Severity ranks a notification so that a Notifier can route it.
type Severity string

const (
	// SeverityInfo is a change the operator asked to hear about.
	SeverityInfo Severity = "info"
	// SeverityWarning is a degraded result.
	SeverityWarning Severity = "warning"
	// SeverityAlert is a broken check or an incident needing a decision.
	SeverityAlert Severity = "alert"
)

// Notification is a message for a human, assembled by the core and delivered
// by an adapter. The core decides whether to speak; the adapter decides how.
type Notification struct {
	// CheckID is the check this concerns.
	CheckID CheckID

	// Destination is where it goes, taken from the check definition.
	Destination Destination

	// Severity ranks it.
	Severity Severity

	// Subject is a one-line summary.
	Subject string

	// Body explains what happened in the operator's terms.
	Body string

	// NeedsDecision marks a notification that is asking for an approval
	// rather than reporting something. Agentd asks; it does not proceed.
	NeedsDecision bool

	// IncidentID is set when NeedsDecision is true.
	IncidentID IncidentID

	// OccurredAt is when the thing being reported happened.
	OccurredAt time.Time

	// TraceID correlates this notification to the run that produced it.
	TraceID string
}

// Validate reports whether n is well formed.
func (n Notification) Validate() error {
	if !nonEmpty(string(n.CheckID)) {
		return invalidf("notification needs a check id")
	}
	if !nonEmpty(n.Subject) {
		return invalidf("notification for check %q needs a subject", n.CheckID)
	}
	if n.NeedsDecision && !nonEmpty(string(n.IncidentID)) {
		return invalidf("notification for check %q asks for a decision but names no incident", n.CheckID)
	}
	return nil
}

// ContentHash returns a deterministic SHA-256 digest of the notification's
// semantic content, used for idempotent deduplication.
func (n Notification) ContentHash() string {
	h := sha256.New()
	h.Write([]byte(n.CheckID))
	h.Write([]byte{0})
	h.Write([]byte(n.Severity))
	h.Write([]byte{0})
	h.Write([]byte(n.Subject))
	h.Write([]byte{0})
	h.Write([]byte(n.Body))
	h.Write([]byte{0})
	h.Write([]byte(n.IncidentID))
	h.Write([]byte{0})
	if n.NeedsDecision {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Secret is a resolved credential. Its String and GoString are redacted so
// that a secret cannot reach a log through an ordinary format verb.
type Secret struct {
	value string
}

// NewSecret wraps a resolved value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the underlying value. The name is deliberately awkward: the
// call site should be obvious in review.
func (s Secret) Reveal() string { return s.value }

// String redacts.
func (s Secret) String() string { return "[redacted]" }

// GoString redacts under the %#v verb too.
func (s Secret) GoString() string { return "[redacted]" }

// SecretBundle is the set of secrets one fetch needs, keyed by reference.
type SecretBundle map[SecretRef]Secret

// Get returns a secret and whether it was resolved.
func (b SecretBundle) Get(ref SecretRef) (Secret, bool) {
	s, ok := b[ref]
	return s, ok
}
