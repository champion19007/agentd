package repair_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/repair"
	"github.com/champion19007/agentd/internal/ports"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// --- doubles ----------------------------------------------------------------

type stubClock struct{ now time.Time }

func (c stubClock) Now() time.Time                             { return c.now }
func (c stubClock) Sleep(context.Context, time.Duration) error { return nil }

type stubIDs struct{}

func (stubIDs) NewRunID() domain.RunID           { return "run-1" }
func (stubIDs) NewIncidentID() domain.IncidentID { return "inc-1" }
func (stubIDs) NewBindingID() domain.BindingID   { return "bnd-2" }

type stubModel struct {
	text      string
	truncated bool
	err       error
	calls     int
}

func (m *stubModel) Complete(_ context.Context, _ ports.ModelRequest) (ports.ModelResponse, error) {
	m.calls++
	if m.err != nil {
		return ports.ModelResponse{}, m.err
	}
	return ports.ModelResponse{Text: m.text, Truncated: m.truncated}, nil
}

// replayExtractor returns whatever the test says the candidate would extract.
type replayExtractor struct {
	out domain.Extraction
	err error
}

func (replayExtractor) Dialects() []string { return []string{"css"} }
func (e replayExtractor) Extract(context.Context, domain.RawResponse, domain.Binding) (domain.Extraction, error) {
	return e.out, e.err
}

type memStore struct {
	ports.Store

	index     *domain.SnapshotIndex
	binding   domain.Binding
	last      domain.Extraction
	hasLast   bool
	saved     []domain.Binding
	activated []int
	audit     []domain.AuditEvent
}

func (s *memStore) Snapshots(context.Context, domain.CheckID) (*domain.SnapshotIndex, error) {
	return s.index, nil
}

func (s *memStore) ActiveBinding(context.Context, domain.CheckID) (domain.Binding, error) {
	return s.binding, nil
}

func (s *memStore) LastResult(context.Context, domain.CheckID) (domain.Extraction, error) {
	if !s.hasLast {
		return domain.Extraction{}, ports.ErrNotFound
	}
	return s.last, nil
}

func (s *memStore) SaveBinding(_ context.Context, b domain.Binding) error {
	s.saved = append(s.saved, b)
	return nil
}

// ActivateBinding exists so that a test can prove it is never called.
func (s *memStore) ActivateBinding(_ context.Context, _ domain.CheckID, version int) error {
	s.activated = append(s.activated, version)
	return nil
}

func (s *memStore) SaveIncident(context.Context, *domain.Incident) error { return nil }

// AppendAudit captures the event the orchestrator records alongside a
// proposal, so a test can assert Agentd attributed its own work to itself.
func (s *memStore) AppendAudit(_ context.Context, e domain.AuditEvent) error {
	if err := e.Validate(); err != nil {
		return err
	}
	s.audit = append(s.audit, e)
	return nil
}

func (s *memStore) Update(ctx context.Context, fn func(context.Context, ports.Tx) error) error {
	return fn(ctx, txOf{memStore: s})
}

// unimplementedWriter supplies the Writer half of a Tx. Nesting it one level
// deeper than the explicit methods means the real ones win by shallower depth,
// while anything the code under test is not supposed to call panics on a nil
// interface -- which is exactly the assertion we want.
type unimplementedWriter struct{ ports.Writer }

// txOf turns the store into a Tx. The store is its own transaction because
// these tests care about outcomes, not about isolation.
type txOf struct {
	*memStore
	unimplementedWriter
}

// --- fixtures ---------------------------------------------------------------

func check(t *testing.T) *domain.Check {
	t.Helper()
	c, err := domain.NewCheck("chk-1", domain.Definition{
		Intent: domain.ScalarIntent{
			Label:   "price",
			Purpose: "the advertised price of the standard plan",
			Type:    domain.TypeNumber,
		},
		Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
		Schedule:  domain.Schedule{Interval: time.Hour},
		CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}
	return c
}

func scalar(text string) domain.Extraction {
	return domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: text, Type: domain.TypeNumber}}
}

// store builds a store holding a known-good capture and a newer broken one,
// which is the situation every repair starts from.
func store(t *testing.T, withKnownGood bool) *memStore {
	t.Helper()
	idx := domain.NewSnapshotIndex("chk-1")

	good, err := domain.NewSnapshot("chk-1", "text/html", []byte("<div class=price>39</div>"), "fp-v1", base)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if err := idx.Add(good); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if withKnownGood {
		if err := idx.MarkKnownGood(good.ID()); err != nil {
			t.Fatalf("MarkKnownGood: %v", err)
		}
	}

	broken, err := domain.NewSnapshot("chk-1", "text/html", []byte("<span data-price>49</span>"), "fp-v2", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if err := idx.Add(broken); err != nil {
		t.Fatalf("Add: %v", err)
	}

	return &memStore{
		index: idx,
		binding: domain.Binding{
			ID: "bnd-1", CheckID: "chk-1", DefinitionVersion: 1,
			IntentKind: domain.IntentScalar, Fingerprint: "fp-v1", Version: 1,
			Origin:    domain.OriginInferred,
			Locators:  []domain.Locator{{Target: "price", Dialect: "css", Expression: ".price"}},
			DerivedAt: base,
		},
		last:    scalar("39"),
		hasLast: true,
	}
}

func incident(t *testing.T, budget int) *domain.Incident {
	t.Helper()
	log := domain.NewIncidentLog("chk-1")
	i, err := log.Open("inc-1", domain.Failure{
		Class:   domain.ClassStructural,
		Summary: "the price is no longer where this check expects it",
	}, budget, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return i
}

const goodAnswer = "price\t[data-price]\nRATIONALE: the price moved into a span with a data-price attribute"

func orchestrator(st *memStore, m *stubModel, ex replayExtractor) *repair.Orchestrator {
	return repair.New(repair.Deps{
		Clock:   stubClock{now: base.Add(2 * time.Hour)},
		IDs:     stubIDs{},
		Store:   st,
		Model:   m,
		Extract: ex,
	}, "css")
}

// --- tests ------------------------------------------------------------------

// TestVerifiedProposalIsRecordedButNotApplied is the central test of this
// package. A good repair goes all the way to a proposal and stops there.
func TestVerifiedProposalIsRecordedButNotApplied(t *testing.T) {
	st := store(t, true)
	i := incident(t, 3)
	o := orchestrator(st, &stubModel{text: goodAnswer}, replayExtractor{out: scalar("49")})

	p, err := o.Propose(context.Background(), check(t), i)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if p == nil {
		t.Fatal("no proposal was produced")
	}
	if p.Approval != domain.ApprovalPending {
		t.Errorf("Approval = %q, want %q", p.Approval, domain.ApprovalPending)
	}
	if p.Binding.Origin != domain.OriginRepaired {
		t.Errorf("Origin = %q, want %q", p.Binding.Origin, domain.OriginRepaired)
	}
	if p.Binding.Version != 2 {
		t.Errorf("Version = %d, want 2", p.Binding.Version)
	}
	if p.Binding.Fingerprint != "fp-v2" {
		t.Errorf("Fingerprint = %q, want the shape the source has now", p.Binding.Fingerprint)
	}
	if i.State() != domain.IncidentAwaitingApproval {
		t.Errorf("State = %q, want %q", i.State(), domain.IncidentAwaitingApproval)
	}

	// Every required gate, recorded individually. "It failed verification" is
	// not an answer an operator can act on.
	seen := map[domain.Gate]bool{}
	for _, g := range p.Gates {
		if !g.Passed {
			t.Errorf("gate %q failed on a proposal that was accepted: %s", g.Gate, g.Detail)
		}
		seen[g.Gate] = true
	}
	for _, want := range domain.RequiredGates {
		if !seen[want] {
			t.Errorf("the %q gate was never run", want)
		}
	}

	// Stored, so a human can review it. Not activated, ever.
	if len(st.saved) != 1 {
		t.Errorf("saved %d bindings, want 1", len(st.saved))
	}
	if len(st.activated) != 0 {
		t.Fatalf("a binding was activated without approval: versions %v", st.activated)
	}
	if _, err := i.ApprovedBinding(); !errors.Is(err, domain.ErrNotApproved) {
		t.Errorf("ApprovedBinding returned %v, want ErrNotApproved", err)
	}
}

// TestCandidateThatDoesNotReproduceTheIntentIsDiscarded: shape checking is the
// difference between a repair and a guess.
func TestCandidateThatDoesNotReproduceTheIntentIsDiscarded(t *testing.T) {
	st := store(t, true)
	i := incident(t, 3)
	// The candidate parses, but finds nothing when replayed.
	missing := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Type: domain.TypeNumber, Missing: true}}
	o := orchestrator(st, &stubModel{text: goodAnswer}, replayExtractor{out: missing})

	p, err := o.Propose(context.Background(), check(t), i)

	if !errors.Is(err, repair.ErrNotVerified) {
		t.Fatalf("Propose returned %v, want ErrNotVerified", err)
	}
	if p != nil {
		t.Error("an unverified candidate was returned as a proposal")
	}
	if i.State() != domain.IncidentOpen {
		t.Errorf("State = %q, want the incident still open", i.State())
	}
	if len(st.saved) != 0 {
		t.Error("an unverified candidate was stored")
	}
	// The attempt is still spent: the budget exists to stop unbounded trying.
	if got := len(i.Attempts()); got != 1 {
		t.Errorf("attempts recorded = %d, want 1", got)
	}
	if i.Attempts()[0].Outcome != domain.AttemptUnverified {
		t.Errorf("attempt outcome = %q, want %q", i.Attempts()[0].Outcome, domain.AttemptUnverified)
	}
}

// TestCandidateOfTheWrongKindIsDiscarded catches the locator that finds
// something of the right shape but plainly the wrong thing.
func TestCandidateOfTheWrongKindIsDiscarded(t *testing.T) {
	st := store(t, true)
	st.last, st.hasLast = domain.Extraction{
		Kind:   domain.IntentScalar,
		Scalar: domain.Value{Text: "39", Type: domain.TypeNumber},
	}, true
	i := incident(t, 3)

	// Satisfies the intent, but returns a string where a number used to be.
	wrong := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "Buy now", Type: domain.TypeString}}
	o := orchestrator(st, &stubModel{text: goodAnswer}, replayExtractor{out: wrong})

	if _, err := o.Propose(context.Background(), check(t), i); !errors.Is(err, repair.ErrNotVerified) {
		t.Fatalf("Propose returned %v, want ErrNotVerified", err)
	}
}

func TestNoProposalWithoutAKnownGoodCapture(t *testing.T) {
	st := store(t, false) // nothing marked known-good
	i := incident(t, 3)
	m := &stubModel{text: goodAnswer}
	o := orchestrator(st, m, replayExtractor{out: scalar("49")})

	_, err := o.Propose(context.Background(), check(t), i)

	if !errors.Is(err, repair.ErrNoEvidence) {
		t.Fatalf("Propose returned %v, want ErrNoEvidence", err)
	}
	// And it worked that out before spending a model call.
	if m.calls != 0 {
		t.Errorf("the model was called %d times with nothing to verify against", m.calls)
	}
}

func TestBudgetIsRespected(t *testing.T) {
	st := store(t, true)
	i := incident(t, 1)
	missing := domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Type: domain.TypeNumber, Missing: true}}
	m := &stubModel{text: goodAnswer}
	o := orchestrator(st, m, replayExtractor{out: missing})
	c := check(t)

	if _, err := o.Propose(context.Background(), c, i); !errors.Is(err, repair.ErrNotVerified) {
		t.Fatalf("first Propose returned %v, want ErrNotVerified", err)
	}

	_, err := o.Propose(context.Background(), c, i)
	if !errors.Is(err, domain.ErrRepairBudgetExhausted) {
		t.Fatalf("second Propose returned %v, want ErrRepairBudgetExhausted", err)
	}
	if m.calls != 1 {
		t.Errorf("the model was called %d times against a budget of 1", m.calls)
	}
}

func TestClosedIncidentsAreNotRepaired(t *testing.T) {
	st := store(t, true)
	i := incident(t, 3)
	if err := i.Abandon("gave up", base.Add(time.Minute)); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	m := &stubModel{text: goodAnswer}

	_, err := orchestrator(st, m, replayExtractor{out: scalar("49")}).Propose(context.Background(), check(t), i)

	if !errors.Is(err, domain.ErrIncidentClosed) {
		t.Fatalf("Propose returned %v, want ErrIncidentClosed", err)
	}
	if m.calls != 0 {
		t.Error("a closed incident still spent a model call")
	}
}

func TestModelFailureSpendsAnAttemptAndSaysSo(t *testing.T) {
	st := store(t, true)
	i := incident(t, 3)
	o := orchestrator(st, &stubModel{err: errors.New("provider timeout")}, replayExtractor{})

	if _, err := o.Propose(context.Background(), check(t), i); err == nil {
		t.Fatal("Propose succeeded despite the model failing")
	}

	attempts := i.Attempts()
	if len(attempts) != 1 || attempts[0].Outcome != domain.AttemptModelError {
		t.Fatalf("attempts = %+v, want one model_error", attempts)
	}
	// The attempt log is read by a person deciding whether to trust Agentd.
	if strings.Contains(attempts[0].Note, "provider timeout") {
		t.Error("the attempt note leaked a raw provider error to the operator")
	}
}

func TestTruncatedModelAnswerIsRejected(t *testing.T) {
	st := store(t, true)
	i := incident(t, 3)
	o := orchestrator(st, &stubModel{text: goodAnswer, truncated: true}, replayExtractor{out: scalar("49")})

	// A half-written locator may parse and be wrong, which is worse than
	// nothing because it would be presented as verified.
	if _, err := o.Propose(context.Background(), check(t), i); err == nil {
		t.Fatal("a truncated answer was accepted")
	}
}

func TestUnparseableModelAnswersAreRejected(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"no locators", "RATIONALE: I think it moved"},
		{"no rationale", "price\t[data-price]"},
		{"prose", "The price now lives in a span element near the top."},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := store(t, true)
			i := incident(t, 3)
			o := orchestrator(st, &stubModel{text: tt.text}, replayExtractor{out: scalar("49")})

			if _, err := o.Propose(context.Background(), check(t), i); err == nil {
				t.Error("an unparseable answer produced a proposal")
			}
			if len(st.saved) != 0 {
				t.Error("an unparseable answer produced a stored binding")
			}
		})
	}
}

// TestProposalSurvivesToApprovalAndOnlyThenYieldsABinding walks the whole
// path, ending where the product says it must: at a person.
func TestProposalSurvivesToApprovalAndOnlyThenYieldsABinding(t *testing.T) {
	st := store(t, true)
	i := incident(t, 3)
	o := orchestrator(st, &stubModel{text: goodAnswer}, replayExtractor{out: scalar("49")})

	if _, err := o.Propose(context.Background(), check(t), i); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if _, err := i.ApprovedBinding(); !errors.Is(err, domain.ErrNotApproved) {
		t.Fatalf("a verified proposal yielded a binding before approval: %v", err)
	}

	if err := i.Approve("dana", base.Add(3*time.Hour)); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	b, err := i.ApprovedBinding()
	if err != nil {
		t.Fatalf("ApprovedBinding: %v", err)
	}
	if b.Version != 2 || b.Origin != domain.OriginRepaired {
		t.Errorf("approved binding = %+v, want repaired version 2", b)
	}
	// Even now, this package did not activate it. That is the caller's job,
	// and only with this approval in hand.
	if len(st.activated) != 0 {
		t.Errorf("repair activated a binding itself: %v", st.activated)
	}
}
