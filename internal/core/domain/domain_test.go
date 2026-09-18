// Package domain_test exercises the domain from the outside, through the same
// API the rest of Agentd uses. Testing through the exported surface is what
// proves an invariant cannot be broken by a caller, as opposed to merely not
// being broken by the code as currently written.
//
// Every test here is deterministic: instants are constants, nothing sleeps,
// nothing reads a clock, and no test depends on map iteration order.
package domain_test

import (
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

// base is the fixed instant every test builds its timeline from.
var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// at returns an instant offset from base, so that orderings in tests are
// obvious at a glance.
func at(d time.Duration) time.Time { return base.Add(d) }

// priceIntent is a minimal valid scalar intent.
func priceIntent() domain.ScalarIntent {
	return domain.ScalarIntent{
		Label:   "price",
		Purpose: "the advertised monthly price of the standard plan",
		Type:    domain.TypeNumber,
	}
}

// planRecord is a minimal valid record intent with one required and one
// optional field.
func planRecord() domain.RecordIntent {
	return domain.RecordIntent{
		Label:   "plan",
		Purpose: "the standard plan as advertised on the pricing page",
		Fields: []domain.Field{
			{Name: "price", Description: "monthly price", Type: domain.TypeNumber, Required: true},
			{Name: "seats", Description: "included seats", Type: domain.TypeNumber},
		},
	}
}

// planCollection is a minimal valid collection intent.
func planCollection() domain.CollectionIntent {
	return domain.CollectionIntent{
		Label:   "plans",
		Purpose: "every plan listed on the pricing page",
		Element: planRecord(),
	}
}

// definition builds a valid Definition around an intent.
func definition(in domain.Intent) domain.Definition {
	return domain.Definition{
		Intent: in,
		Source: domain.SourceSpec{
			Kind: domain.SourceHTTP,
			URL:  "https://example.test/pricing",
		},
		Schedule:  domain.Schedule{Interval: time.Hour},
		CreatedAt: base,
	}
}

// newCheck builds a valid Check or fails the test.
func newCheck(t *testing.T) *domain.Check {
	t.Helper()
	c, err := domain.NewCheck("chk-1", definition(priceIntent()))
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}
	return c
}

// mustInvariants fails the test if an aggregate's invariants do not hold.
func mustInvariants(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("invariant violated: %v", err)
	}
}

func TestNewCheckStartsWithOneActiveVersion(t *testing.T) {
	c := newCheck(t)

	mustInvariants(t, c.CheckInvariants())
	if got := c.ActiveVersion(); got != 1 {
		t.Errorf("ActiveVersion = %d, want 1", got)
	}
	if got := len(c.Versions()); got != 1 {
		t.Errorf("len(Versions) = %d, want 1", got)
	}
	if !c.Enabled() {
		t.Error("a new check should be enabled")
	}
}

func TestNewCheckRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name string
		id   domain.CheckID
		def  domain.Definition
	}{
		{
			name: "no id",
			id:   "",
			def:  definition(priceIntent()),
		},
		{
			name: "no intent",
			id:   "chk-1",
			def: domain.Definition{
				Source:   domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
				Schedule: domain.Schedule{Interval: time.Hour},
			},
		},
		{
			name: "no source url",
			id:   "chk-1",
			def: domain.Definition{
				Intent:   priceIntent(),
				Source:   domain.SourceSpec{Kind: domain.SourceHTTP},
				Schedule: domain.Schedule{Interval: time.Hour},
			},
		},
		{
			name: "zero interval",
			id:   "chk-1",
			def: domain.Definition{
				Intent: priceIntent(),
				Source: domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := domain.NewCheck(tt.id, tt.def); err == nil {
				t.Fatal("NewCheck accepted an invalid definition")
			}
		})
	}
}

func TestReviseKeepsExactlyOneVersionActive(t *testing.T) {
	c := newCheck(t)

	v2, err := c.Revise(definition(planRecord()))
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}
	v3, err := c.Revise(definition(planCollection()))
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}

	mustInvariants(t, c.CheckInvariants())
	if v2 != 2 || v3 != 3 {
		t.Errorf("Revise returned versions %d and %d, want 2 and 3", v2, v3)
	}
	if got := c.ActiveVersion(); got != 3 {
		t.Errorf("ActiveVersion = %d, want 3", got)
	}
	if got := c.ActiveDefinition().Intent.Kind(); got != domain.IntentCollection {
		t.Errorf("active intent kind = %q, want %q", got, domain.IntentCollection)
	}
	if got := len(c.Versions()); got != 3 {
		t.Errorf("len(Versions) = %d, want 3; older versions must be kept so past runs stay explicable", got)
	}
}

func TestReviseIgnoresACallerSuppliedVersion(t *testing.T) {
	c := newCheck(t)

	// A caller trying to force a version number must not be able to create a
	// gap or a duplicate, either of which would break the invariant.
	def := definition(planRecord())
	def.Version = 99
	v, err := c.Revise(def)
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}

	mustInvariants(t, c.CheckInvariants())
	if v != 2 {
		t.Errorf("Revise assigned version %d, want 2", v)
	}
}

func TestReviseRejectsInvalidDefinitionWithoutChangingActive(t *testing.T) {
	c := newCheck(t)

	bad := definition(domain.ScalarIntent{Label: "price"}) // no purpose, no type
	if _, err := c.Revise(bad); err == nil {
		t.Fatal("Revise accepted an invalid definition")
	}

	mustInvariants(t, c.CheckInvariants())
	if got := c.ActiveVersion(); got != 1 {
		t.Errorf("ActiveVersion = %d after a rejected revision, want 1", got)
	}
	if got := len(c.Versions()); got != 1 {
		t.Errorf("len(Versions) = %d after a rejected revision, want 1", got)
	}
}

func TestActivateRollsBackToAnEarlierVersion(t *testing.T) {
	c := newCheck(t)
	if _, err := c.Revise(definition(planRecord())); err != nil {
		t.Fatalf("Revise: %v", err)
	}

	if err := c.Activate(1); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	mustInvariants(t, c.CheckInvariants())
	if got := c.ActiveVersion(); got != 1 {
		t.Errorf("ActiveVersion = %d, want 1", got)
	}
}

func TestActivateRejectsAVersionThatDoesNotExist(t *testing.T) {
	c := newCheck(t)

	if err := c.Activate(7); err == nil {
		t.Fatal("Activate accepted a version the check does not have")
	}
	mustInvariants(t, c.CheckInvariants())
	if got := c.ActiveVersion(); got != 1 {
		t.Errorf("ActiveVersion = %d after a rejected activation, want 1", got)
	}
}

func TestSlotsPartitionTime(t *testing.T) {
	s := domain.Schedule{Interval: time.Hour}

	first := s.SlotAt(base)
	same := s.SlotAt(base.Add(59 * time.Minute))
	next := s.SlotAt(base.Add(time.Hour))

	if first != same {
		t.Errorf("instants 59 minutes apart fell in slots %d and %d; they should share one", first, same)
	}
	if next != first+1 {
		t.Errorf("slot after one interval = %d, want %d", next, first+1)
	}
	if got := s.SlotStart(first); got.After(base) {
		t.Errorf("SlotStart(%d) = %v, which is after the instant it contains", first, got)
	}
}
