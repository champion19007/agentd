package policy_test

import (
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/policy"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func check(t *testing.T, onQuiet bool) *domain.Check {
	t.Helper()
	dest := domain.Destination{Kind: domain.DestinationNone}
	if onQuiet {
		dest = domain.Destination{Kind: domain.DestinationNotify, Target: "ops@example.test", OnQuiet: true}
	}
	c, err := domain.NewCheck("chk-1", domain.Definition{
		Intent: domain.ScalarIntent{
			Label:   "price",
			Purpose: "the advertised price of the standard plan",
			Type:    domain.TypeNumber,
		},
		Source:      domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
		Schedule:    domain.Schedule{Interval: time.Hour},
		Destination: dest,
		CreatedAt:   base,
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}
	return c
}

func result() domain.Extraction {
	return domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "49", Type: domain.TypeNumber}}
}

// run builds a terminal run in the given state.
func run(t *testing.T, state domain.RunState, f *domain.Failure) *domain.Run {
	t.Helper()
	r, err := domain.NewRun("run-1", "chk-1", 1, 1, base)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state != domain.StateSkippedOverload && state != domain.StateInterrupted {
		if err := r.Start(base.Add(time.Second), 1); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	end := base.Add(2 * time.Second)
	switch state {
	case domain.StateQuiet:
		err = r.Quiet(end, "sha256:a", result())
	case domain.StateChanged:
		err = r.Changed(end, "sha256:a", result(), "the price went from 39 to 49")
	case domain.StateDegraded:
		err = r.Degrade(end, "sha256:a", result(), *f)
	case domain.StateFailed:
		err = r.Fail(end, *f)
	case domain.StateInterrupted:
		err = r.Interrupt(end, "agentd was shutting down")
	case domain.StateSkippedOverload:
		err = r.SkipOverloaded(end, "the runner was at capacity")
	}
	if err != nil {
		t.Fatalf("driving run to %q: %v", state, err)
	}
	return r
}

func failure(class domain.FailureClass) *domain.Failure {
	return &domain.Failure{Class: class, Summary: "something happened"}
}

// TestQuietRunsSayNothing is the product's defining behaviour. The common case
// must be silent, or the silence stops meaning anything.
func TestQuietRunsSayNothing(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check: check(t, false),
		Run:   run(t, domain.StateQuiet, nil),
	})

	if d.Notify {
		t.Error("a quiet run notified; silence is the product")
	}
	if d.OpenIncident || d.AttemptRepair || d.Retry {
		t.Errorf("a quiet run asked for action: %+v", d)
	}
}

func TestQuietRunsSpeakWhenAsked(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check: check(t, true), // OnQuiet
		Run:   run(t, domain.StateQuiet, nil),
	})

	if !d.Notify {
		t.Error("a check configured to report every run stayed silent")
	}
	if d.Severity != domain.SeverityInfo {
		t.Errorf("Severity = %q, want %q", d.Severity, domain.SeverityInfo)
	}
}

func TestSuccessClosesAnOpenIncident(t *testing.T) {
	for _, state := range []domain.RunState{domain.StateQuiet, domain.StateChanged} {
		t.Run(string(state), func(t *testing.T) {
			d := policy.Decide(policy.Situation{
				Check:        check(t, false),
				Run:          run(t, state, nil),
				IncidentOpen: true,
			})
			if !d.ResolveIncident {
				t.Error("a working check left its incident open")
			}
		})
	}
}

func TestChangedRunsNotify(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check: check(t, false),
		Run:   run(t, domain.StateChanged, nil),
	})

	if !d.Notify || d.Severity != domain.SeverityInfo {
		t.Errorf("a changed run should notify at info level, got %+v", d)
	}
	if d.Body == "" {
		t.Error("a change notification should say what changed")
	}
}

// TestDegradedNotifiesButDoesNotEscalate is the other half of keeping degraded
// and failed apart. A thinner result is worth a word, not a repair.
func TestDegradedNotifiesButDoesNotEscalate(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check:        check(t, false),
		Run:          run(t, domain.StateDegraded, failure(domain.ClassSemantic)),
		HasKnownGood: true,
	})

	if !d.Notify {
		t.Error("a degraded run stayed silent")
	}
	if d.Severity != domain.SeverityWarning {
		t.Errorf("Severity = %q, want %q", d.Severity, domain.SeverityWarning)
	}
	if d.OpenIncident || d.AttemptRepair {
		t.Error("a degraded run reached for a repair; its binding is still working")
	}
}

// TestRetryableFailuresStaySilentWhileRetrying keeps Agentd from crying wolf
// over a timeout that resolves itself thirty seconds later.
func TestRetryableFailuresStaySilentWhileRetrying(t *testing.T) {
	for _, class := range []domain.FailureClass{domain.ClassTransient, domain.ClassRateLimited} {
		t.Run(string(class), func(t *testing.T) {
			d := policy.Decide(policy.Situation{
				Check:   check(t, false),
				Run:     run(t, domain.StateFailed, failure(class)),
				Attempt: 1,
			})

			if !d.Retry {
				t.Fatal("a retryable failure did not ask for a retry")
			}
			if d.RetryAfter <= 0 {
				t.Errorf("RetryAfter = %v, want a positive delay", d.RetryAfter)
			}
			if d.Notify {
				t.Error("Agentd complained about a failure it was about to retry")
			}
			if d.OpenIncident {
				t.Error("a retryable failure opened an incident")
			}
		})
	}
}

func TestRetriesAreBoundedAndThenReported(t *testing.T) {
	c := check(t, false)
	last := c.ActiveDefinition().Policy.Retries()

	d := policy.Decide(policy.Situation{
		Check:   c,
		Run:     run(t, domain.StateFailed, failure(domain.ClassTransient)),
		Attempt: last, // the budget is spent
	})

	if d.Retry {
		t.Error("retries continued past the budget")
	}
	if !d.Notify {
		t.Error("Agentd gave up retrying and said nothing; the failure must surface")
	}
	if d.Severity != domain.SeverityAlert {
		t.Errorf("Severity = %q, want %q", d.Severity, domain.SeverityAlert)
	}
}

func TestBackoffGrows(t *testing.T) {
	var p domain.Policy

	first, second, third := p.Backoff(1), p.Backoff(2), p.Backoff(3)

	if !(first < second && second < third) {
		t.Errorf("backoff did not grow: %v, %v, %v", first, second, third)
	}
	if p.Backoff(50) > time.Hour {
		t.Errorf("backoff is uncapped: %v", p.Backoff(50))
	}
}

// TestOnlyStructuralFailuresReachForARepair is the guard on the expensive,
// attention-consuming path.
func TestOnlyStructuralFailuresReachForARepair(t *testing.T) {
	tests := []struct {
		class        domain.FailureClass
		wantIncident bool
		wantRepair   bool
	}{
		{domain.ClassAuth, false, false},
		{domain.ClassFatal, false, false},
		{domain.ClassStructural, true, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			d := policy.Decide(policy.Situation{
				Check:        check(t, false),
				Run:          run(t, domain.StateFailed, failure(tt.class)),
				Attempt:      1,
				HasKnownGood: true,
			})

			if d.OpenIncident != tt.wantIncident {
				t.Errorf("OpenIncident = %v, want %v", d.OpenIncident, tt.wantIncident)
			}
			if d.AttemptRepair != tt.wantRepair {
				t.Errorf("AttemptRepair = %v, want %v", d.AttemptRepair, tt.wantRepair)
			}
			if !d.Notify {
				t.Error("a non-retryable failure stayed silent")
			}
		})
	}
}

func TestNoSecondIncidentForAnAlreadyBrokenCheck(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check:        check(t, false),
		Run:          run(t, domain.StateFailed, failure(domain.ClassStructural)),
		Attempt:      1,
		IncidentOpen: true,
		HasKnownGood: true,
	})

	if d.OpenIncident {
		t.Error("a second incident was opened for a check that already had one")
	}
}

// TestNoRepairWithoutEvidence stops Agentd proposing a fix it cannot check.
func TestNoRepairWithoutEvidence(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check:        check(t, false),
		Run:          run(t, domain.StateFailed, failure(domain.ClassStructural)),
		Attempt:      1,
		HasKnownGood: false,
	})

	if d.AttemptRepair {
		t.Error("a repair was attempted with no known-good capture to verify it against")
	}
	if !d.OpenIncident {
		t.Error("the breakage should still be recorded as an incident")
	}
	if !d.Notify {
		t.Error("the operator should be told Agentd cannot help here")
	}
}

// TestInterruptionIsSilent: Agentd stopping is not news about the source.
func TestInterruptionIsSilent(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check: check(t, false),
		Run:   run(t, domain.StateInterrupted, nil),
	})

	if d.Notify || d.OpenIncident || d.Retry {
		t.Errorf("an interrupted run produced actions: %+v", d)
	}
}

// TestOverloadSkipIsReported: a hole in the record has to be explained, or
// the silence around it is a lie.
func TestOverloadSkipIsReported(t *testing.T) {
	d := policy.Decide(policy.Situation{
		Check: check(t, false),
		Run:   run(t, domain.StateSkippedOverload, nil),
	})

	if !d.Notify {
		t.Error("a skipped slot was not reported; an unexplained gap makes the silence untrustworthy")
	}
	if d.Severity != domain.SeverityWarning {
		t.Errorf("Severity = %q, want %q", d.Severity, domain.SeverityWarning)
	}
	if d.OpenIncident {
		t.Error("an overload skip opened an incident; nothing is wrong with the source")
	}
}

func TestNonTerminalRunsGetNoDecision(t *testing.T) {
	r, err := domain.NewRun("run-1", "chk-1", 1, 1, base)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if d := (policy.Decide(policy.Situation{Check: check(t, false), Run: r})); d != (policy.Decision{}) {
		t.Errorf("a pending run produced %+v, want no decision", d)
	}
}

func TestNotificationOnlyWhenDecided(t *testing.T) {
	s := policy.Situation{Check: check(t, true), Run: run(t, domain.StateChanged, nil)}
	d := policy.Decide(s)

	n, ok := policy.Notification(s, d, base)
	if !ok {
		t.Fatal("a notifying decision produced no notification")
	}
	if err := n.Validate(); err != nil {
		t.Errorf("the notification is not well formed: %v", err)
	}
	if n.NeedsDecision {
		t.Error("an ordinary report was marked as needing a decision")
	}

	if _, ok := policy.Notification(s, policy.Decision{}, base); ok {
		t.Error("a silent decision produced a notification")
	}
}

// TestApprovalRequestAsksRatherThanTells pins the wording contract: the one
// message that expects an answer must be marked as expecting one.
func TestApprovalRequestAsksRatherThanTells(t *testing.T) {
	c := check(t, false)
	log := domain.NewIncidentLog("chk-1")
	i, err := log.Open("inc-1", *failure(domain.ClassStructural), 2, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, ok := policy.ApprovalRequest(c, i, base); ok {
		t.Error("an approval was requested with no proposal to approve")
	}

	b := domain.Binding{
		ID: "bnd-2", CheckID: "chk-1", DefinitionVersion: 1,
		IntentKind: domain.IntentScalar, Fingerprint: "fp-v2", Version: 2,
		Origin:   domain.OriginRepaired,
		Locators: []domain.Locator{{Target: "price", Dialect: "css", Expression: ".p"}},
	}
	if err := i.Propose(domain.RepairProposal{
		Binding:         b,
		Rationale:       "the price moved into a new container",
		VerifiedAgainst: "sha256:current",
		VerifiedAt:      base,
	}, base); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	n, ok := policy.ApprovalRequest(c, i, base)
	if !ok {
		t.Fatal("no approval request was produced")
	}
	if !n.NeedsDecision {
		t.Error("the approval request is not marked as needing a decision")
	}
	if n.IncidentID != i.ID() {
		t.Errorf("IncidentID = %q, want %q", n.IncidentID, i.ID())
	}
	if err := n.Validate(); err != nil {
		t.Errorf("the notification is not well formed: %v", err)
	}
}
