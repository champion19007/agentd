// Package ports declares the interfaces the domain core depends on.
//
// Every interface here is defined for the convenience of the core, not of the
// adapter that implements it. Adapters import ports; ports imports nothing
// outside the standard library and internal/core/domain. This is what keeps
// the dependency direction pointing inward.
package ports
