// Package domain is the heart of Agentd: the aggregates that hold the user's
// durable intent, the record of what happened when that intent was checked,
// and the machinery for proposing a repair when a source changes shape.
//
// The load-bearing distinction in this package is Check versus Binding:
//
//   - A Check is what the human wants to know, expressed as an Intent. It
//     survives redesigns of the source it watches.
//   - A Binding is a derived, disposable answer to "where does that live in
//     this particular version of the source". It is never authored by hand.
//
// Because intent is stable, a proposed repair can be verified against it. If
// the selector were the configuration, there would be nothing left to check a
// repair against, and Agentd could only ever guess.
//
// This package computes; it does not act. It reads no clock, opens no
// connections and touches no disk. Every instant it works with is passed in.
package domain
