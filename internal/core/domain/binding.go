package domain

import "time"

// BindingOrigin records how a Binding came to exist. It matters because a
// repaired binding carries different weight from one that has been working
// since the check was created.
type BindingOrigin string

const (
	// OriginInferred is a binding derived when the check was first set up.
	OriginInferred BindingOrigin = "inferred"
	// OriginRepaired is a binding proposed to fix a structural break and
	// approved by a human. It is never applied without that approval.
	OriginRepaired BindingOrigin = "repaired"
)

// Valid reports whether o is a known origin.
func (o BindingOrigin) Valid() bool {
	return o == OriginInferred || o == OriginRepaired
}

// Locator is one opaque instruction for finding a piece of intent inside a
// source. Its Expression is a CSS selector, a JSON path, a regular expression
// or whatever else the Extractor for its Dialect understands.
//
// The core never parses, validates or reasons about an Expression. It carries
// it from the Model that proposed it to the Extractor that will use it, and
// compares it to other expressions only for equality. Keeping the core
// illiterate about dialects is what lets a new extractor be added without the
// domain changing.
type Locator struct {
	// Target names what this locator finds. For a scalar intent it is the
	// intent's own name; for a record it is a field name; for a collection
	// the reserved target CollectionRoot locates the repeating element and
	// the remaining targets are field names resolved within it.
	Target string

	// Dialect names the extractor that understands Expression.
	Dialect string

	// Expression is the extractor's instruction, opaque here.
	Expression string
}

// CollectionRoot is the reserved Locator target naming the repeating element
// of a collection intent.
const CollectionRoot = "$root"

// Validate reports whether l is well formed.
func (l Locator) Validate() error {
	if !nonEmpty(l.Target) {
		return invalidf("locator needs a target")
	}
	if !nonEmpty(l.Dialect) {
		return invalidf("locator for %q needs a dialect", l.Target)
	}
	if !nonEmpty(l.Expression) {
		return invalidf("locator for %q needs an expression", l.Target)
	}
	return nil
}

// Binding is the derived way of locating a Check's intent inside a particular
// version of its source.
//
// A Binding is disposable. When a source changes shape the binding breaks,
// Agentd proposes a new one, and a human approves it. The Check that the
// binding serves does not change at all, because what the operator wanted to
// know did not change. That separation is what makes a proposed repair
// verifiable: there is a stable statement of intent to verify it against.
//
// Every Binding is traceable to the four things that produced it.
type Binding struct {
	// ID identifies this binding.
	ID BindingID

	// CheckID is the check whose intent this binding serves.
	CheckID CheckID

	// DefinitionVersion is the check definition version whose intent this
	// binding was derived for. A binding does not survive a change of
	// intent.
	DefinitionVersion int

	// IntentKind is the shape of intent this binding satisfies, recorded so
	// that a mismatch is caught without reloading the check.
	IntentKind IntentKind

	// Fingerprint is the shape of the source version this binding was
	// derived against. When a fetched source no longer matches it, the
	// binding is in doubt.
	Fingerprint SourceFingerprint

	// Version is the binding's number within its check, starting at 1. A
	// repair produces the next version.
	Version int

	// Origin records whether this binding was inferred at setup or repaired
	// after a break.
	Origin BindingOrigin

	// Locators are the opaque instructions themselves.
	Locators []Locator

	// DerivedAt is when this binding was produced, supplied by the caller.
	DerivedAt time.Time

	// DerivedFrom is the snapshot the binding was derived against, kept so
	// that a repair can be re-verified later against the same evidence.
	DerivedFrom SnapshotID
}

// Trace is the provenance of a Binding: the four things it must be traceable
// to. Returning it as a value makes "is this binding traceable" a single
// assertion rather than four scattered field checks.
type Trace struct {
	CheckID           CheckID
	DefinitionVersion int
	IntentKind        IntentKind
	Fingerprint       SourceFingerprint
	BindingVersion    int
}

// Trace returns b's provenance.
func (b Binding) Trace() Trace {
	return Trace{
		CheckID:           b.CheckID,
		DefinitionVersion: b.DefinitionVersion,
		IntentKind:        b.IntentKind,
		Fingerprint:       b.Fingerprint,
		BindingVersion:    b.Version,
	}
}

// Validate reports whether b is well formed and fully traceable.
func (b Binding) Validate() error {
	if !nonEmpty(string(b.ID)) {
		return invalidf("binding needs an id")
	}
	if !nonEmpty(string(b.CheckID)) {
		return invalidf("binding %q is not traceable to a check", b.ID)
	}
	if b.DefinitionVersion < 1 {
		return invalidf("binding %q is not traceable to a definition version", b.ID)
	}
	if !nonEmpty(string(b.Fingerprint)) {
		return invalidf("binding %q is not traceable to a source fingerprint", b.ID)
	}
	switch b.IntentKind {
	case IntentScalar, IntentRecord, IntentCollection:
	default:
		return invalidf("binding %q has unknown intent kind %q", b.ID, b.IntentKind)
	}
	if b.Version < 1 {
		return invalidf("binding %q needs a version of 1 or greater", b.ID)
	}
	if !b.Origin.Valid() {
		return invalidf("binding %q has unknown origin %q", b.ID, b.Origin)
	}
	if len(b.Locators) == 0 {
		return invalidf("binding %q has no locators", b.ID)
	}
	seen := make(map[string]bool, len(b.Locators))
	for _, l := range b.Locators {
		if err := l.Validate(); err != nil {
			return err
		}
		if seen[l.Target] {
			return invalidf("binding %q has two locators for target %q", b.ID, l.Target)
		}
		seen[l.Target] = true
	}
	if b.IntentKind == IntentCollection && !seen[CollectionRoot] {
		return invalidf("binding %q is for a collection but has no %s locator", b.ID, CollectionRoot)
	}
	return nil
}

// Covers reports whether b has a locator for every target the intent requires.
// A binding that is missing a required target cannot satisfy the intent, and
// discovering that here is cheaper than discovering it mid-run.
func (b Binding) Covers(in Intent) error {
	if in == nil {
		return invalidf("binding %q was checked against a nil intent", b.ID)
	}
	if b.IntentKind != in.Kind() {
		return invalidf("binding %q is for a %s intent but was checked against a %s intent", b.ID, b.IntentKind, in.Kind())
	}

	have := make(map[string]bool, len(b.Locators))
	for _, l := range b.Locators {
		have[l.Target] = true
	}

	missing := func(target string) error {
		return invalidf("binding %q has no locator for %q", b.ID, target)
	}

	switch v := in.(type) {
	case ScalarIntent:
		if !have[v.Name()] {
			return missing(v.Name())
		}
	case RecordIntent:
		for _, f := range v.RequiredFields() {
			if !have[f.Name] {
				return missing(f.Name)
			}
		}
	case CollectionIntent:
		if !have[CollectionRoot] {
			return missing(CollectionRoot)
		}
		for _, f := range v.Element.RequiredFields() {
			if !have[f.Name] {
				return missing(f.Name)
			}
		}
	default:
		return invalidf("binding %q was checked against an unknown intent type", b.ID)
	}
	return nil
}

// Stale reports whether the source has changed shape since b was derived.
// A stale binding is not yet a broken one: it may still extract correctly.
// It is a reason to look closely, not a reason to open an incident.
func (b Binding) Stale(current SourceFingerprint) bool {
	return b.Fingerprint != current
}
