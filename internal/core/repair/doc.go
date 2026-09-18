// Package repair holds repair orchestration: turning a breakage into a
// candidate spec, verifying that candidate against evidence, and recording it
// as a proposal. Applying a proposal is not part of this package's job; that
// happens only on an explicit human approval.
package repair
