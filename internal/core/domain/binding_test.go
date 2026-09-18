package domain_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

func validBinding() domain.Binding {
	return domain.Binding{
		ID:                "bnd-1",
		CheckID:           "chk-1",
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Fingerprint:       "fp-v1",
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators: []domain.Locator{
			{Target: "price", Dialect: "css", Expression: ".plan--standard .price"},
		},
		DerivedAt:   base,
		DerivedFrom: "sha256:knowngood",
	}
}

// TestBindingIsTraceable asserts the four things a binding must be traceable
// to. Traceability is what makes a repair reviewable: an operator looking at a
// proposal needs to know which check it serves, which intent it was derived
// for, which source shape it was derived against, and which version it is.
func TestBindingIsTraceable(t *testing.T) {
	b := validBinding()

	tr := b.Trace()

	if tr.CheckID != "chk-1" {
		t.Errorf("Trace.CheckID = %q, want chk-1", tr.CheckID)
	}
	if tr.DefinitionVersion != 1 {
		t.Errorf("Trace.DefinitionVersion = %d, want 1", tr.DefinitionVersion)
	}
	if tr.IntentKind != domain.IntentScalar {
		t.Errorf("Trace.IntentKind = %q, want %q", tr.IntentKind, domain.IntentScalar)
	}
	if tr.Fingerprint != "fp-v1" {
		t.Errorf("Trace.Fingerprint = %q, want fp-v1", tr.Fingerprint)
	}
	if tr.BindingVersion != 1 {
		t.Errorf("Trace.BindingVersion = %d, want 1", tr.BindingVersion)
	}
}

func TestBindingRejectsMissingProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.Binding)
	}{
		{"no id", func(b *domain.Binding) { b.ID = "" }},
		{"no check", func(b *domain.Binding) { b.CheckID = "" }},
		{"no definition version", func(b *domain.Binding) { b.DefinitionVersion = 0 }},
		{"no fingerprint", func(b *domain.Binding) { b.Fingerprint = "" }},
		{"no binding version", func(b *domain.Binding) { b.Version = 0 }},
		{"unknown intent kind", func(b *domain.Binding) { b.IntentKind = "table" }},
		{"unknown origin", func(b *domain.Binding) { b.Origin = "handwritten" }},
		{"no locators", func(b *domain.Binding) { b.Locators = nil }},
		{"locator with no dialect", func(b *domain.Binding) { b.Locators[0].Dialect = "" }},
		{"locator with no expression", func(b *domain.Binding) { b.Locators[0].Expression = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := validBinding()
			tt.mutate(&b)

			if err := b.Validate(); err == nil {
				t.Error("Validate accepted a binding that is not fully traceable")
			}
		})
	}

	if err := validBinding().Validate(); err != nil {
		t.Errorf("a well formed binding should validate, got %v", err)
	}
}

func TestBindingRejectsDuplicateTargets(t *testing.T) {
	b := validBinding()
	b.Locators = append(b.Locators, domain.Locator{
		Target: "price", Dialect: "css", Expression: ".other",
	})

	if err := b.Validate(); err == nil {
		t.Error("Validate accepted two locators for the same target")
	}
}

func TestCollectionBindingNeedsARootLocator(t *testing.T) {
	b := validBinding()
	b.IntentKind = domain.IntentCollection

	if err := b.Validate(); err == nil {
		t.Error("Validate accepted a collection binding with no root locator")
	}

	b.Locators = append(b.Locators, domain.Locator{
		Target: domain.CollectionRoot, Dialect: "css", Expression: "tr.plan",
	})
	if err := b.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBindingCoversIntent(t *testing.T) {
	scalar := validBinding()

	if err := scalar.Covers(priceIntent()); err != nil {
		t.Errorf("Covers: %v", err)
	}

	// A binding for a different shape cannot satisfy this intent, and finding
	// that out here is cheaper than finding it out mid-run.
	if err := scalar.Covers(planRecord()); err == nil {
		t.Error("a scalar binding covered a record intent")
	}

	record := validBinding()
	record.IntentKind = domain.IntentRecord
	if err := record.Covers(planRecord()); err != nil {
		t.Errorf("a binding with a locator for the required field should cover: %v", err)
	}

	// Optional fields need no locator: their absence degrades rather than
	// breaks.
	record.Locators = []domain.Locator{
		{Target: "seats", Dialect: "css", Expression: ".seats"},
	}
	if err := record.Covers(planRecord()); err == nil {
		t.Error("a binding missing the required field should not cover the intent")
	}
}

func TestStaleBindingIsNotYetABrokenOne(t *testing.T) {
	b := validBinding()

	if b.Stale("fp-v1") {
		t.Error("a binding matching the current fingerprint is not stale")
	}
	if !b.Stale("fp-v2") {
		t.Error("a binding derived against a different shape is stale")
	}
	// Staleness is a reason to look closely, not a failure in itself: a
	// redesigned page may still extract correctly. Nothing here returns an
	// error, and that is deliberate.
}

// TestCheckCannotCarryABinding is the structural guarantee behind the
// modelling rule. It walks the Definition type looking for anywhere a caller
// could stash an extraction path, so that "a selector is not configuration"
// is enforced by the type graph rather than by reviewer vigilance.
//
// If this test fails, a locator has leaked into the user's configuration and
// repair verification has lost the stable thing it verifies against.
func TestCheckCannotCarryABinding(t *testing.T) {
	forbiddenTypes := map[reflect.Type]bool{
		reflect.TypeOf(domain.Binding{}): true,
		reflect.TypeOf(domain.Locator{}): true,
	}
	// Names that betray an extraction path hiding under a different type.
	forbiddenNames := []string{"selector", "xpath", "jsonpath", "locator", "expression", "binding", "regex", "extract"}

	var walk func(t *testing.T, typ reflect.Type, path string, depth int)
	walk = func(t *testing.T, typ reflect.Type, path string, depth int) {
		if depth > 8 {
			return
		}
		if forbiddenTypes[typ] {
			t.Errorf("%s reaches %s; a check must not be able to carry an extraction path", path, typ)
			return
		}

		switch typ.Kind() {
		case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Map:
			walk(t, typ.Elem(), path+"[]", depth+1)
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				lower := strings.ToLower(f.Name)
				for _, bad := range forbiddenNames {
					if strings.Contains(lower, bad) {
						t.Errorf("%s.%s names an extraction path; that belongs on a Binding, not on a Check", path, f.Name)
					}
				}
				walk(t, f.Type, path+"."+f.Name, depth+1)
			}
		}
	}

	walk(t, reflect.TypeOf(domain.Definition{}), "Definition", 0)
}

// TestIntentSurvivesARepairedBinding states the separation as behaviour
// rather than as structure: repairing a check replaces where it looks, and
// leaves what it wants to know untouched.
func TestIntentSurvivesARepairedBinding(t *testing.T) {
	c := newCheck(t)
	before := c.Intent()

	// The source is redesigned. A new binding is derived against the new
	// shape, for the same check and the same definition version.
	repaired := validBinding()
	repaired.Fingerprint = "fp-v2"
	repaired.Version = 2
	repaired.Origin = domain.OriginRepaired
	repaired.Locators = []domain.Locator{
		{Target: "price", Dialect: "css", Expression: "[data-testid=price]"},
	}
	repaired.DerivedAt = at(24 * time.Hour)

	if err := repaired.Validate(); err != nil {
		t.Fatalf("the repaired binding should be valid: %v", err)
	}
	if err := repaired.Covers(before); err != nil {
		t.Fatalf("the repaired binding should still cover the unchanged intent: %v", err)
	}

	if c.ActiveVersion() != 1 {
		t.Error("repairing a binding must not revise the check")
	}
	if c.Intent().Description() != before.Description() {
		t.Error("repairing a binding must not change what the operator asked for")
	}
	if repaired.DefinitionVersion != 1 {
		t.Error("the repaired binding should still serve definition version 1")
	}
}
