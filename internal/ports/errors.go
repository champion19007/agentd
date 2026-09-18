package ports

import "errors"

// ErrNotFound reports that a store held nothing for the identifier asked for.
// It is declared here, on the port, so that the core can distinguish "no such
// thing" from "the store broke" without knowing which store it is talking to.
var ErrNotFound = errors.New("not found")
