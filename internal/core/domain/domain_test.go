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

func TestScheduleCatchUpPolicyDefaultAndValidation(t *testing.T) {
	sDefault := domain.Schedule{Interval: time.Hour}
	if sDefault.CatchUpPolicy() != domain.CatchUpOnce {
		t.Errorf("default CatchUpPolicy = %v, want once", sDefault.CatchUpPolicy())
	}
	if err := sDefault.Validate(); err != nil {
		t.Errorf("validating default schedule: %v", err)
	}

	for _, p := range []domain.CatchUpPolicy{domain.CatchUpSkip, domain.CatchUpOnce, domain.CatchUpBackfill} {
		s := domain.Schedule{Interval: time.Hour, CatchUp: p}
		if s.CatchUpPolicy() != p {
			t.Errorf("CatchUpPolicy = %v, want %v", s.CatchUpPolicy(), p)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("validating schedule with %s: %v", p, err)
		}
	}

	sBad := domain.Schedule{Interval: time.Hour, CatchUp: "invalid"}
	if err := sBad.Validate(); err == nil {
		t.Error("expected error validating unknown catch-up policy")
	}
}

func TestSlotsAcrossDSTTransitions(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("skipping DST test; America/New_York location not available")
	}

	// 2026-03-08: US Spring Forward (02:00 -> 03:00 local)
	// 01:00 EST = 06:00 UTC
	// 01:30 EST = 06:30 UTC
	// 03:00 EDT = 07:00 UTC (1 hour later in real/UTC time, despite 2 hours on wall clock)
	// 04:00 EDT = 08:00 UTC
	s := domain.Schedule{Interval: time.Hour}

	t0 := time.Date(2026, 3, 8, 1, 0, 0, 0, loc).UTC()
	t1 := time.Date(2026, 3, 8, 3, 0, 0, 0, loc).UTC()
	t2 := time.Date(2026, 3, 8, 4, 0, 0, 0, loc).UTC()

	slot0 := s.SlotAt(t0)
	slot1 := s.SlotAt(t1)
	slot2 := s.SlotAt(t2)

	if slot1 != slot0+1 {
		t.Errorf("spring forward: slot1 (%d) != slot0+1 (%d)", slot1, slot0+1)
	}
	if slot2 != slot1+1 {
		t.Errorf("spring forward: slot2 (%d) != slot1+1 (%d)", slot2, slot1+1)
	}

	// 2026-11-01: US Fall Back (02:00 -> 01:00 local)
	// 01:00 EDT = 05:00 UTC
	// 01:00 EST (repeated hour) = 06:00 UTC
	// 02:00 EST = 07:00 UTC
	fall0 := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	fall1 := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	fall2 := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)

	fSlot0 := s.SlotAt(fall0)
	fSlot1 := s.SlotAt(fall1)
	fSlot2 := s.SlotAt(fall2)

	if fSlot1 != fSlot0+1 || fSlot2 != fSlot1+1 {
		t.Errorf("fall back: slots should advance monotonically: %d, %d, %d", fSlot0, fSlot1, fSlot2)
	}
}

func TestCheckStalenessCalculation(t *testing.T) {
	c := newCheck(t) // 1-hour interval, created at base
	// Without any run, at base + 30m: not stale (threshold is 1.5 * 1h = 90m)
	stale0 := domain.CheckStaleness(c, nil, base.Add(30*time.Minute), 1.5)
	if stale0.IsStale {
		t.Errorf("check with 30m elapsed should not be stale: %+v", stale0)
	}
	if stale0.Elapsed != 30*time.Minute {
		t.Errorf("Elapsed = %v, want 30m", stale0.Elapsed)
	}

	// At base + 91m: is stale (threshold 90m)
	stale1 := domain.CheckStaleness(c, nil, base.Add(91*time.Minute), 1.5)
	if !stale1.IsStale {
		t.Errorf("check with 91m elapsed should be stale (threshold 90m): %+v", stale1)
	}

	// With a finished terminal run at base + 60m:
	r, err := domain.NewRun("run-1", c.ID(), 1, 1, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(base.Add(10*time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Quiet(base.Add(60*time.Minute), "snap-1", domain.Extraction{Kind: domain.IntentScalar}); err != nil {
		t.Fatal(err)
	}

	// Now at base + 100m (40m after run ended): not stale
	stale2 := domain.CheckStaleness(c, r, base.Add(100*time.Minute), 1.5)
	if stale2.IsStale {
		t.Errorf("check with 40m since last run should not be stale: %+v", stale2)
	}
	if stale2.Elapsed != 40*time.Minute {
		t.Errorf("Elapsed = %v, want 40m", stale2.Elapsed)
	}

	// At base + 160m (100m after run ended): stale (> 90m)
	stale3 := domain.CheckStaleness(c, r, base.Add(160*time.Minute), 1.5)
	if !stale3.IsStale {
		t.Errorf("check with 100m since last run should be stale: %+v", stale3)
	}
}

func TestDegradationLevelMethods(t *testing.T) {
	cases := []struct {
		lvl              domain.DegradationLevel
		name             string
		shedsHealing     bool
		shedsModel       bool
		reducesRetention bool
		shedsRuns        bool
	}{
		{domain.DegradationNormal, "normal", false, false, false, false},
		{domain.DegradationShedHealing, "shed_healing", true, false, false, false},
		{domain.DegradationShedModel, "shed_model", true, true, false, false},
		{domain.DegradationReducedRetention, "reduced_retention", true, true, true, false},
		{domain.DegradationShedRuns, "shed_runs", true, true, true, true},
		{domain.DegradationLevel(99), "unknown", true, true, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.lvl.String() != tc.name {
				t.Errorf("String() = %q, want %q", tc.lvl.String(), tc.name)
			}
			if tc.lvl.ShedsHealing() != tc.shedsHealing {
				t.Errorf("ShedsHealing() = %v, want %v", tc.lvl.ShedsHealing(), tc.shedsHealing)
			}
			if tc.lvl.ShedsModel() != tc.shedsModel {
				t.Errorf("ShedsModel() = %v, want %v", tc.lvl.ShedsModel(), tc.shedsModel)
			}
			if tc.lvl.ReducesRetention() != tc.reducesRetention {
				t.Errorf("ReducesRetention() = %v, want %v", tc.lvl.ReducesRetention(), tc.reducesRetention)
			}
			if tc.lvl.ShedsRuns() != tc.shedsRuns {
				t.Errorf("ShedsRuns() = %v, want %v", tc.lvl.ShedsRuns(), tc.shedsRuns)
			}
		})
	}
}
