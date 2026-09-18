package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Identifiers are distinct named types rather than bare strings so that a
// CheckID can never be passed where a RunID belongs.
type (
	// CheckID identifies a Check.
	CheckID string
	// RunID identifies a Run.
	RunID string
	// SnapshotID is the content address of a Snapshot: "sha256:" followed by
	// the lowercase hex digest of its body.
	SnapshotID string
	// IncidentID identifies an Incident.
	IncidentID string
	// BindingID identifies a Binding.
	BindingID string
)

// SourceFingerprint summarises the observed shape of a source, as opposed to
// its exact bytes. Two fetches of a page whose contents changed but whose
// structure did not share a fingerprint. Bindings are derived against a
// fingerprint, so a change in fingerprint is what puts a binding in doubt.
//
// How a fingerprint is computed is an adapter's business; the core only
// compares them.
type SourceFingerprint string

// ErrInvalid reports a value that violates an aggregate's invariants. Callers
// distinguish causes with errors.Is against the sentinels in this package.
var ErrInvalid = errors.New("invalid")

// invalidf builds an ErrInvalid-wrapped error with context.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// nonEmpty reports whether s contains a non-space character.
func nonEmpty(s string) bool { return strings.TrimSpace(s) != "" }
