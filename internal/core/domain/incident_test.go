package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

func repairedBinding() domain.Binding {
	b := validBinding()
	b.Origin = domain.OriginRepaired
	b.Version = 2
	return b
}

func proposal() domain.RepairProposal {
	return domain.RepairProposal{
		Binding:         repairedBinding(),
		Rationale:       "the price moved into a new container; this locator finds it again",
		VerifiedAgainst: "sha256:knowngood",
		VerifiedAt:      at(time.Minute),
		Gates:           passingGates(at(time.Minute)),
	}
}

// passingGates builds a full set of passing verdicts, which a proposal needs
// before the domain will accept it. That refusal is the point: an unverified
// candidate must not be able to reach an operator as a suggestion.
func passingGates(at time.Time) []domain.GateResult {
	var out []domain.GateResult
	for _, g := range domain.RequiredGates {
		out = append(out, domain.GateResult{Gate: g, Passed: true, Detail: "checked", At: at})
	}
	return out
}

func openIncident(t *testing.T) (*domain.IncidentLog, *domain.Incident) {
	t.Helper()
	log := domain.NewIncidentLog("chk-1")
	i, err := log.Open("inc-1", structuralFailure(), 2, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return log, i
}

func TestOnlyOneIncidentIsOpenPerCheck(t *testing.T) {
	log, first := openIncident(t)

	// A check failing every five minutes must not open an incident every five
	// minutes; that would burn the repair budget over and over and bury the
	// operator in duplicate approval requests.
	_, err := log.Open("inc-2", structuralFailure(), 2, at(time.Minute))
	if !errors.Is(err, domain.ErrIncidentOpen) {
		t.Fatalf("Open returned %v, want ErrIncidentOpen", err)
	}
	mustInvariants(t, log.CheckInvariants())

	if err := first.ResolveRecovered(at(time.Hour)); err != nil {
		t.Fatalf("ResolveRecovered: %v", err)
	}
	if _, err := log.Open("inc-2", structuralFailure(), 2, at(2*time.Hour)); err != nil {
		t.Fatalf("Open after the first was resolved: %v", err)
	}
	mustInvariants(t, log.CheckInvariants())
	if got := log.Len(); got != 2 {
		t.Errorf("Len = %d, want 2", got)
	}
}

func TestOnlyStructuralFailuresOpenAnIncident(t *testing.T) {
	log := domain.NewIncidentLog("chk-1")

	for _, class := range []domain.FailureClass{
		domain.ClassTransient, domain.ClassRateLimited, domain.ClassAuth,
		domain.ClassSemantic, domain.ClassFatal,
	} {
		t.Run(string(class), func(t *testing.T) {
			_, err := log.Open("inc-x", domain.Failure{Class: class, Summary: "something"}, 2, base)
			if err == nil {
				t.Errorf("a %q failure opened a repair incident", class)
			}
		})
	}
}

// TestRepairAttemptsAreBounded pins the budget. Without it, a source Agentd
// cannot read would be retried against a paid model indefinitely.
func TestRepairAttemptsAreBounded(t *testing.T) {
	_, i := openIncident(t) // budget of 2

	for n := 1; n <= 2; n++ {
		a, err := i.RecordAttempt(domain.AttemptUnverified, "the candidate did not reproduce the intent", at(time.Duration(n)*time.Minute))
		if err != nil {
			t.Fatalf("attempt %d: %v", n, err)
		}
		if a.Number != n {
			t.Errorf("attempt numbered %d, want %d", a.Number, n)
		}
	}

	if got := i.AttemptsRemaining(); got != 0 {
		t.Errorf("AttemptsRemaining = %d, want 0", got)
	}
	_, err := i.RecordAttempt(domain.AttemptUnverified, "one more", at(time.Hour))
	if !errors.Is(err, domain.ErrRepairBudgetExhausted) {
		t.Fatalf("RecordAttempt past the budget returned %v, want ErrRepairBudgetExhausted", err)
	}
	if got := len(i.Attempts()); got != 2 {
		t.Errorf("a refused attempt was still recorded: %d attempts, want 2", got)
	}
	mustInvariants(t, i.CheckInvariants())
}

func TestDefaultRepairBudgetApplies(t *testing.T) {
	log := domain.NewIncidentLog("chk-1")
	i, err := log.Open("inc-1", structuralFailure(), 0, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if got := i.AttemptsRemaining(); got != domain.DefaultMaxRepairAttempts {
		t.Errorf("AttemptsRemaining = %d, want the default %d", got, domain.DefaultMaxRepairAttempts)
	}
}

// TestNoRepairWithoutApproval is the product's central promise expressed as a
// test. There must be no sequence of calls that yields an applicable binding
// without a named human approving it.
func TestNoRepairWithoutApproval(t *testing.T) {
	_, i := openIncident(t)

	if _, err := i.ApprovedBinding(); !errors.Is(err, domain.ErrNoProposal) {
		t.Fatalf("ApprovedBinding with no proposal returned %v, want ErrNoProposal", err)
	}

	if err := i.Propose(proposal(), at(time.Minute)); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got := i.State(); got != domain.IncidentAwaitingApproval {
		t.Errorf("State = %q, want %q", got, domain.IncidentAwaitingApproval)
	}

	// Proposed and verified is still not approved.
	if _, err := i.ApprovedBinding(); !errors.Is(err, domain.ErrNotApproved) {
		t.Fatalf("ApprovedBinding on a pending proposal returned %v, want ErrNotApproved", err)
	}
	if err := i.ResolveWithRepair(at(time.Hour)); !errors.Is(err, domain.ErrNotApproved) {
		t.Fatalf("ResolveWithRepair without approval returned %v, want ErrNotApproved", err)
	}

	// Nor can a caller approve it by writing to the copy they were handed.
	p := i.Proposal()
	p.Approval = domain.ApprovalApproved
	if _, err := i.ApprovedBinding(); !errors.Is(err, domain.ErrNotApproved) {
		t.Fatal("a proposal was approved by mutating a returned copy")
	}

	if err := i.Approve("dana", at(2*time.Hour)); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	b, err := i.ApprovedBinding()
	if err != nil {
		t.Fatalf("ApprovedBinding after approval: %v", err)
	}
	if b.Origin != domain.OriginRepaired {
		t.Errorf("approved binding origin = %q, want %q", b.Origin, domain.OriginRepaired)
	}
	if err := i.ResolveWithRepair(at(3 * time.Hour)); err != nil {
		t.Fatalf("ResolveWithRepair: %v", err)
	}
	mustInvariants(t, i.CheckInvariants())
}

func TestApprovalMustNameSomebody(t *testing.T) {
	_, i := openIncident(t)
	if err := i.Propose(proposal(), at(time.Minute)); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	// An approval with nobody attached is indistinguishable from Agentd
	// approving its own work.
	if err := i.Approve("", at(time.Hour)); err == nil {
		t.Error("Approve accepted an anonymous approval")
	}
	if err := i.Reject("", "no", at(time.Hour)); err == nil {
		t.Error("Reject accepted an anonymous rejection")
	}
	if _, err := i.ApprovedBinding(); !errors.Is(err, domain.ErrNotApproved) {
		t.Errorf("the proposal became approved anyway: %v", err)
	}
}

func TestRejectionLeavesTheCheckBroken(t *testing.T) {
	_, i := openIncident(t)
	if err := i.Propose(proposal(), at(time.Minute)); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if err := i.Reject("dana", "that locator points at the wrong table", at(time.Hour)); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if got := i.State(); got != domain.IncidentAbandoned {
		t.Errorf("State = %q, want %q", got, domain.IncidentAbandoned)
	}
	if i.Open() {
		t.Error("a rejected incident should be closed")
	}
	if _, err := i.ApprovedBinding(); !errors.Is(err, domain.ErrNotApproved) {
		t.Errorf("a rejected proposal yielded a binding: %v", err)
	}
	if i.Resolution() == "" {
		t.Error("a closed incident should say how it ended")
	}
	mustInvariants(t, i.CheckInvariants())
}

func TestUnverifiedProposalsAreRefused(t *testing.T) {
	_, i := openIncident(t)

	tests := []struct {
		name   string
		mutate func(*domain.RepairProposal)
	}{
		{"not verified against anything", func(p *domain.RepairProposal) { p.VerifiedAgainst = "" }},
		{"no verification time", func(p *domain.RepairProposal) { p.VerifiedAt = time.Time{} }},
		{"no rationale", func(p *domain.RepairProposal) { p.Rationale = "" }},
		{"binding not marked as a repair", func(p *domain.RepairProposal) { p.Binding.Origin = domain.OriginInferred }},
		{"binding for another check", func(p *domain.RepairProposal) { p.Binding.CheckID = "chk-other" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := proposal()
			tt.mutate(&p)

			if err := i.Propose(p, at(time.Minute)); err == nil {
				t.Error("Propose accepted a proposal that had not been properly verified")
			}
			if i.State() != domain.IncidentOpen {
				t.Errorf("State = %q after a refused proposal, want %q", i.State(), domain.IncidentOpen)
			}
		})
	}
}

func TestClosedIncidentsAreClosed(t *testing.T) {
	_, i := openIncident(t)
	if err := i.Abandon("agentd could not read this source", at(time.Hour)); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if _, err := i.RecordAttempt(domain.AttemptProposed, "late", at(2*time.Hour)); !errors.Is(err, domain.ErrIncidentClosed) {
		t.Errorf("RecordAttempt on a closed incident returned %v, want ErrIncidentClosed", err)
	}
	if err := i.Propose(proposal(), at(2*time.Hour)); !errors.Is(err, domain.ErrIncidentClosed) {
		t.Errorf("Propose on a closed incident returned %v, want ErrIncidentClosed", err)
	}
	if err := i.Approve("dana", at(2*time.Hour)); !errors.Is(err, domain.ErrIncidentClosed) {
		t.Errorf("Approve on a closed incident returned %v, want ErrIncidentClosed", err)
	}
	mustInvariants(t, i.CheckInvariants())
}

func TestRecoveryClosesAnIncidentWithoutARepair(t *testing.T) {
	_, i := openIncident(t)

	if err := i.ResolveRecovered(at(time.Hour)); err != nil {
		t.Fatalf("ResolveRecovered: %v", err)
	}

	if got := i.State(); got != domain.IncidentResolved {
		t.Errorf("State = %q, want %q", got, domain.IncidentResolved)
	}
	if i.Proposal() != nil {
		t.Error("a recovered incident should carry no proposal")
	}
	mustInvariants(t, i.CheckInvariants())
}

func TestCurrentFindsTheOpenIncident(t *testing.T) {
	log, i := openIncident(t)

	got, ok := log.Current()
	if !ok || got.ID() != i.ID() {
		t.Fatalf("Current = %v, %v; want the open incident", got, ok)
	}

	if err := i.ResolveRecovered(at(time.Hour)); err != nil {
		t.Fatalf("ResolveRecovered: %v", err)
	}
	if _, ok := log.Current(); ok {
		t.Error("Current found an open incident after it was resolved")
	}
}
