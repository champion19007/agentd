package run_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/ports"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// --- test doubles -----------------------------------------------------------
//
// These are small, hand-written and behavioural. A generated mock of each port
// would be longer than the code under test and would assert on call sequences
// rather than on outcomes, which is not what any of these tests care about.

type stubClock struct{ now time.Time }

func (c *stubClock) Now() time.Time                             { return c.now }
func (c *stubClock) Sleep(context.Context, time.Duration) error { return nil }

type stubIDs struct{ n int }

func (s *stubIDs) NewRunID() domain.RunID           { s.n++; return domain.RunID("run-1") }
func (s *stubIDs) NewIncidentID() domain.IncidentID { return "inc-1" }
func (s *stubIDs) NewBindingID() domain.BindingID   { return "bnd-1" }

type stubSecrets struct{ err error }

func (s stubSecrets) Resolve(context.Context, []domain.SecretRef) (domain.SecretBundle, error) {
	return domain.SecretBundle{}, s.err
}

type stubSource struct {
	raw domain.RawResponse
	err error
}

func (s stubSource) Fetch(context.Context, domain.SourceSpec, domain.SecretBundle) (domain.RawResponse, error) {
	return s.raw, s.err
}

type stubFingerprint struct {
	fp  domain.SourceFingerprint
	err error
}

func (s stubFingerprint) Fingerprint(context.Context, domain.RawResponse) (domain.SourceFingerprint, error) {
	return s.fp, s.err
}

type stubExtractor struct {
	out domain.Extraction
	err error
}

func (s stubExtractor) Dialects() []string { return []string{"css"} }
func (s stubExtractor) Extract(context.Context, domain.RawResponse, domain.Binding) (domain.Extraction, error) {
	return s.out, s.err
}

// memStore keeps just enough state to exercise the orchestrator. It embeds
// ports.Store so any method the orchestrator is not supposed to touch panics.
type memStore struct {
	ports.Store

	binding   domain.Binding
	noBinding bool
	last      domain.Extraction
	hasLast   bool
	runs      map[domain.RunKey]*domain.Run
	snapshots map[domain.SnapshotID]domain.Snapshot
	knownGood map[domain.SnapshotID]bool
	audit     []domain.AuditEvent
	createErr error
}

func newStore(b domain.Binding) *memStore {
	return &memStore{
		binding:   b,
		runs:      map[domain.RunKey]*domain.Run{},
		snapshots: map[domain.SnapshotID]domain.Snapshot{},
		knownGood: map[domain.SnapshotID]bool{},
	}
}

func (s *memStore) ActiveBinding(context.Context, domain.CheckID) (domain.Binding, error) {
	if s.noBinding {
		return domain.Binding{}, ports.ErrNotFound
	}
	return s.binding, nil
}

func (s *memStore) LastResult(context.Context, domain.CheckID) (domain.Extraction, error) {
	if !s.hasLast {
		return domain.Extraction{}, ports.ErrNotFound
	}
	return s.last, nil
}

func (s *memStore) CreateRun(_ context.Context, r *domain.Run) error {
	if s.createErr != nil {
		return s.createErr
	}
	if _, ok := s.runs[r.Key()]; ok {
		return domain.ErrDuplicateRun
	}
	s.runs[r.Key()] = r
	return nil
}

func (s *memStore) UpdateRun(_ context.Context, r *domain.Run) error {
	s.runs[r.Key()] = r
	return nil
}

func (s *memStore) PutSnapshot(_ context.Context, snap domain.Snapshot) error {
	s.snapshots[snap.ID()] = snap
	return nil
}

func (s *memStore) MarkSnapshotKnownGood(_ context.Context, _ domain.CheckID, id domain.SnapshotID) error {
	s.knownGood[id] = true
	return nil
}

// AppendAudit records the event the orchestrator writes alongside every run.
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
		Intent: domain.RecordIntent{
			Label:   "plan",
			Purpose: "the standard plan as advertised",
			Fields: []domain.Field{
				{Name: "price", Description: "monthly price", Type: domain.TypeNumber, Required: true},
				{Name: "seats", Description: "included seats", Type: domain.TypeNumber},
			},
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

func binding() domain.Binding {
	return domain.Binding{
		ID: "bnd-1", CheckID: "chk-1", DefinitionVersion: 1,
		IntentKind: domain.IntentRecord, Fingerprint: "fp-v1", Version: 1,
		Origin: domain.OriginInferred,
		Locators: []domain.Locator{
			{Target: "price", Dialect: "css", Expression: ".price"},
		},
		DerivedAt: base,
	}
}

func record(price, seats string, seatsMissing bool) domain.Extraction {
	return domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
		"price": {Text: price, Type: domain.TypeNumber},
		"seats": {Text: seats, Type: domain.TypeNumber, Missing: seatsMissing},
	}}
}

// orchestrator wires a run orchestrator around the given doubles.
func orchestrator(st *memStore, src stubSource, ex stubExtractor, secrets stubSecrets) *run.Orchestrator {
	return run.New(run.Deps{
		Clock:       &stubClock{now: base},
		IDs:         &stubIDs{},
		Store:       st,
		Source:      src,
		Extract:     ex,
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     secrets,
	})
}

func okSource() stubSource {
	return stubSource{raw: domain.RawResponse{
		ContentType: "text/html",
		Body:        []byte("<html>price 49</html>"),
		Status:      200,
		FetchedAt:   base,
	}}
}

// --- tests ------------------------------------------------------------------

func TestFirstRunIsReportedAsChanged(t *testing.T) {
	st := newStore(binding())
	o := orchestrator(st, okSource(), stubExtractor{out: record("49", "5", false)}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Calling a first observation "quiet" would claim a comparison that never
	// happened.
	if got := out.Run.State(); got != domain.StateChanged {
		t.Errorf("State = %q, want %q", got, domain.StateChanged)
	}
	mustInvariants(t, out.Run.CheckInvariants())
}

func TestUnchangedResultIsQuiet(t *testing.T) {
	st := newStore(binding())
	st.last, st.hasLast = record("49", "5", false), true
	o := orchestrator(st, okSource(), stubExtractor{out: record("49", "5", false)}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.Run.State(); got != domain.StateQuiet {
		t.Errorf("State = %q, want %q", got, domain.StateQuiet)
	}
	if out.Run.Failure() != nil {
		t.Error("a quiet run carries a failure")
	}
}

func TestChangedResultIsReported(t *testing.T) {
	st := newStore(binding())
	st.last, st.hasLast = record("39", "5", false), true
	o := orchestrator(st, okSource(), stubExtractor{out: record("49", "5", false)}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.Run.State(); got != domain.StateChanged {
		t.Errorf("State = %q, want %q", got, domain.StateChanged)
	}
	if out.Run.Explanation() == "" {
		t.Error("a changed run should explain itself")
	}
}

// TestMissingRequiredFieldIsStructural and the test below it are the pair that
// matters most: the same symptom, a field that is not there, lands in two
// different states depending on whether the intent said it was required.
func TestMissingRequiredFieldIsStructural(t *testing.T) {
	st := newStore(binding())
	st.last, st.hasLast = record("49", "5", false), true
	broken := domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
		"price": {Type: domain.TypeNumber, Missing: true},
		"seats": {Text: "5", Type: domain.TypeNumber},
	}}
	o := orchestrator(st, okSource(), stubExtractor{out: broken}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.Run.State(); got != domain.StateFailed {
		t.Fatalf("State = %q, want %q", got, domain.StateFailed)
	}
	if f := out.Run.Failure(); f == nil || f.Class != domain.ClassStructural {
		t.Fatalf("Failure = %+v, want a structural one", f)
	}
}

func TestMissingOptionalFieldOnlyDegrades(t *testing.T) {
	st := newStore(binding())
	st.last, st.hasLast = record("49", "5", false), true
	o := orchestrator(st, okSource(), stubExtractor{out: record("49", "", true)}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.Run.State(); got != domain.StateDegraded {
		t.Fatalf("State = %q, want %q", got, domain.StateDegraded)
	}
	// The run still kept what it managed to get, which is the difference
	// between degraded and failed.
	if out.Run.SnapshotID() == "" {
		t.Error("a degraded run recorded no capture")
	}
}

func TestUnknownFetchErrorIsTransientNotStructural(t *testing.T) {
	st := newStore(binding())
	src := stubSource{err: errors.New("no such element: div.price")}
	o := orchestrator(st, src, stubExtractor{}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f := out.Run.Failure()
	if f == nil {
		t.Fatal("a failed fetch produced no failure")
	}
	// The message reads structural. Classifying on text would open a repair
	// incident against a source that may be perfectly healthy.
	if f.Class != domain.ClassTransient {
		t.Errorf("Class = %q, want %q", f.Class, domain.ClassTransient)
	}
}

func TestUnresolvableSecretIsAuth(t *testing.T) {
	st := newStore(binding())
	o := orchestrator(st, okSource(), stubExtractor{}, stubSecrets{err: errors.New("keyring locked")})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f := out.Run.Failure()
	if f == nil || f.Class != domain.ClassAuth {
		t.Fatalf("Failure = %+v, want an auth failure; retrying cannot conjure a credential", f)
	}
	if f.Class.Retryable() {
		t.Error("an auth failure was marked retryable")
	}
}

func TestCheckWithNoBindingIsFatalNotStructural(t *testing.T) {
	st := newStore(domain.Binding{})
	st.noBinding = true
	o := orchestrator(st, okSource(), stubExtractor{}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	f := out.Run.Failure()
	if f == nil || f.Class != domain.ClassFatal {
		t.Fatalf("Failure = %+v, want fatal; there is no earlier binding to repair", f)
	}
}

func TestSuccessfulRunMarksItsCaptureKnownGood(t *testing.T) {
	st := newStore(binding())
	o := orchestrator(st, okSource(), stubExtractor{out: record("49", "5", false)}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !out.Captured {
		t.Fatal("a successful run captured nothing")
	}
	// Without this, there would be no evidence to verify a future repair.
	if !st.knownGood[out.Snapshot.ID()] {
		t.Error("a successful run did not mark its capture known-good")
	}
	if _, ok := st.snapshots[out.Snapshot.ID()]; !ok {
		t.Error("the capture was not stored")
	}
}

func TestFailedRunDoesNotMarkItsCaptureKnownGood(t *testing.T) {
	st := newStore(binding())
	broken := domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
		"price": {Type: domain.TypeNumber, Missing: true},
	}}
	o := orchestrator(st, okSource(), stubExtractor{out: broken}, stubSecrets{})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if st.knownGood[out.Snapshot.ID()] {
		t.Error("a broken capture was marked known-good; it would then be trusted to verify a repair")
	}
	// It is still stored: it is the evidence a repair will be derived from.
	if _, ok := st.snapshots[out.Snapshot.ID()]; !ok {
		t.Error("the broken capture was not stored; a repair needs it")
	}
}

func TestDuplicateSlotIsRefused(t *testing.T) {
	st := newStore(binding())
	o := orchestrator(st, okSource(), stubExtractor{out: record("49", "5", false)}, stubSecrets{})
	c := check(t)

	if _, err := o.Run(context.Background(), c, 1); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	_, err := o.Run(context.Background(), c, 1)
	if !errors.Is(err, domain.ErrDuplicateRun) {
		t.Fatalf("second Run for the same slot returned %v, want ErrDuplicateRun", err)
	}
}

func TestCancelledFetchIsInterruptedNotFailed(t *testing.T) {
	st := newStore(binding())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	o := orchestrator(st, stubSource{err: context.Canceled}, stubExtractor{}, stubSecrets{})

	out, err := o.Run(ctx, check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := out.Run.State(); got != domain.StateInterrupted {
		t.Fatalf("State = %q, want %q; Agentd stopping says nothing about the source", got, domain.StateInterrupted)
	}
	if out.Run.State().CountsAsFailure() {
		t.Error("an interruption counted as a failure")
	}
}

func TestSkipOverloadedRecordsTheGap(t *testing.T) {
	st := newStore(binding())
	o := orchestrator(st, okSource(), stubExtractor{}, stubSecrets{})

	r, err := o.SkipOverloaded(context.Background(), check(t), 7, "the runner was at capacity")
	if err != nil {
		t.Fatalf("SkipOverloaded: %v", err)
	}

	if got := r.State(); got != domain.StateSkippedOverload {
		t.Errorf("State = %q, want %q", got, domain.StateSkippedOverload)
	}
	if _, ok := st.runs[domain.RunKey{CheckID: "chk-1", Slot: 7}]; !ok {
		t.Error("the skipped slot left no record; an unexplained gap makes the silence untrustworthy")
	}
	if r.Explanation() == "" {
		t.Error("the skip did not say why")
	}
}

func mustInvariants(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("invariant violated: %v", err)
	}
}

type stubModel struct {
	calls int
	resp  ports.ModelResponse
	err   error
}

func (m *stubModel) Complete(ctx context.Context, req ports.ModelRequest) (ports.ModelResponse, error) {
	m.calls++
	return m.resp, m.err
}

type stubNotifier struct {
	delivered []domain.Notification
}

func (n *stubNotifier) Kinds() []domain.DestinationKind {
	return []domain.DestinationKind{domain.DestinationNotify}
}

func (n *stubNotifier) Deliver(ctx context.Context, notif domain.Notification) error {
	n.delivered = append(n.delivered, notif)
	return nil
}

func TestHashGateSuppressesModelWhenUnchanged(t *testing.T) {
	st := newStore(binding())
	// Existing result from previous run is 49
	st.last, st.hasLast = record("49", "5", false), true

	model := &stubModel{resp: ports.ModelResponse{Text: "model explanation"}}
	notifier := &stubNotifier{}

	o := run.New(run.Deps{
		Clock:       &stubClock{now: base},
		IDs:         &stubIDs{},
		Store:       st,
		Source:      okSource(),
		Extract:     stubExtractor{out: record("49", "5", false)}, // identical extraction
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
		Model:       model,
		Notifier:    notifier,
	})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.Run.State() != domain.StateQuiet {
		t.Errorf("State = %q, want quiet", out.Run.State())
	}
	// THE HASH GATE INVARIANT: Model must NOT be called when payload matches!
	if model.calls != 0 {
		t.Errorf("model was called %d times; the hash gate must eliminate model calls on quiet runs", model.calls)
	}
	if len(notifier.delivered) != 0 {
		t.Errorf("quiet run sent %d notifications, want 0", len(notifier.delivered))
	}
}

func TestHashGateInvokesModelWhenPayloadChanged(t *testing.T) {
	st := newStore(binding())
	// Previous result was 39; new result is 49
	st.last, st.hasLast = record("39", "5", false), true

	model := &stubModel{resp: ports.ModelResponse{Text: `{"verdict": "changed", "explanation": "the price increased from 39 to 49"}`}}
	notifier := &stubNotifier{}

	c := check(t)
	// Enable notification
	cDef := c.ActiveDefinition()
	cDef.Destination = domain.Destination{Kind: domain.DestinationNotify, Target: "ops@example.test"}
	_, _ = c.Revise(cDef)

	o := run.New(run.Deps{
		Clock:       &stubClock{now: base},
		IDs:         &stubIDs{},
		Store:       st,
		Source:      okSource(),
		Extract:     stubExtractor{out: record("49", "5", false)},
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
		Model:       model,
		Notifier:    notifier,
	})

	out, err := o.Run(context.Background(), c, 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.Run.State() != domain.StateChanged {
		t.Errorf("State = %q, want changed", out.Run.State())
	}
	// Model must be called when payload changes!
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 when payload changed", model.calls)
	}
	if out.Run.Explanation() != "the price increased from 39 to 49" {
		t.Errorf("Explanation = %q, want model response text", out.Run.Explanation())
	}
	if len(notifier.delivered) != 1 {
		t.Errorf("delivered notifications = %d, want 1", len(notifier.delivered))
	}
}

type sequenceSource struct {
	responses []domain.RawResponse
	errors    []error
	call      int
}

func (s *sequenceSource) Fetch(context.Context, domain.SourceSpec, domain.SecretBundle) (domain.RawResponse, error) {
	idx := s.call
	s.call++
	if idx < len(s.errors) && s.errors[idx] != nil {
		return domain.RawResponse{}, s.errors[idx]
	}
	if idx < len(s.responses) {
		return s.responses[idx], nil
	}
	return okSource().raw, nil
}

func TestOrchestratorRetriesTransientFailureAndSucceeds(t *testing.T) {
	st := newStore(binding())
	st.last, st.hasLast = record("49", "5", false), true

	// First attempt fails transiently; second attempt succeeds
	seq := &sequenceSource{
		errors: []error{errors.New("connection reset by peer"), nil},
		responses: []domain.RawResponse{
			{},
			okSource().raw,
		},
	}

	clk := &stubClock{now: base}
	o := run.New(run.Deps{
		Clock:       clk,
		IDs:         &stubIDs{},
		Store:       st,
		Source:      seq,
		Extract:     stubExtractor{out: record("49", "5", false)},
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
	})

	out, err := o.Run(context.Background(), check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if seq.call != 2 {
		t.Errorf("source called %d times, want 2 (1 retry)", seq.call)
	}
	if out.Run.State() != domain.StateQuiet {
		t.Errorf("State = %q after successful retry, want quiet", out.Run.State())
	}
}

func TestOrchestratorExhaustsRetriesAndPersistsFailure(t *testing.T) {
	st := newStore(binding())
	// Policy allows 2 retries (DefaultMaxRetries = 3)
	seq := &sequenceSource{
		errors: []error{
			errors.New("timeout 1"),
			errors.New("timeout 2"),
			errors.New("timeout 3"),
			errors.New("timeout 4"),
		},
	}

	c := check(t)
	cDef := c.ActiveDefinition()
	cDef.Policy.MaxRetries = 3
	cDef.Destination = domain.Destination{Kind: domain.DestinationNotify, Target: "ops@example.test"}
	_, _ = c.Revise(cDef)

	notifier := &stubNotifier{}
	clk := &stubClock{now: base}

	o := run.New(run.Deps{
		Clock:       clk,
		IDs:         &stubIDs{},
		Store:       st,
		Source:      seq,
		Extract:     stubExtractor{},
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
		Notifier:    notifier,
	})

	out, err := o.Run(context.Background(), c, 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Initial attempt + 2 retries = 3 attempts total
	if seq.call != 3 {
		t.Errorf("source calls = %d, want 3 attempts", seq.call)
	}
	if out.Run.State() != domain.StateFailed {
		t.Errorf("State = %q, want failed after retry exhaustion", out.Run.State())
	}
	// After exhausting retries, operator should be alerted
	if len(notifier.delivered) != 1 {
		t.Errorf("delivered notifications = %d, want 1", len(notifier.delivered))
	}
}

type stubMetrics struct {
	ports.NoopMetrics
	queueDepths []int
}

func (m *stubMetrics) SetQueueDepth(depth int) {
	m.queueDepths = append(m.queueDepths, depth)
}

func TestRunOrchestrator_SkipOverloadedAndDescribeFallback(t *testing.T) {
	ctx := context.Background()
	st := newStore(binding())
	c := check(t)
	clk := &stubClock{now: base}

	o := run.New(run.Deps{
		Clock:       clk,
		IDs:         &stubIDs{},
		Store:       st,
		Source:      okSource(),
		Extract:     stubExtractor{out: record("49", "5", false)},
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
	})

	// 1. SkipOverloaded
	r, err := o.SkipOverloaded(ctx, c, 42, "runner queue saturated")
	if err != nil {
		t.Fatalf("SkipOverloaded failed: %v", err)
	}
	if r.State() != domain.StateSkippedOverload {
		t.Errorf("expected StateSkippedOverload, got: %s", r.State())
	}
	if r.Slot() != 42 {
		t.Errorf("expected slot 42, got %d", r.Slot())
	}

	// Verify run was persisted
	savedRun := st.runs[r.Key()]
	if savedRun == nil {
		t.Fatalf("expected saved overloaded run in store: %+v", r.Key())
	}

	// 2. Rule-based change explanation when Model is nil
	st2 := newStore(binding())
	st2.last, st2.hasLast = record("39", "5", false), true

	o2 := run.New(run.Deps{
		Clock:       clk,
		IDs:         &stubIDs{},
		Store:       st2,
		Source:      okSource(),
		Extract:     stubExtractor{out: record("49", "5", false)},
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
		Model:       nil, // rule-based fallback!
	})

	out, err := o2.Run(ctx, c, 10)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if out.Run.State() != domain.StateChanged {
		t.Errorf("expected StateChanged, got %s", out.Run.State())
	}
	if !strings.Contains(out.Run.Explanation(), "is different from the last time Agentd looked") {
		t.Errorf("expected fallback explanation, got: %s", out.Run.Explanation())
	}
}

func TestWorkerPool_WithMetricsCoverage(t *testing.T) {
	sm := &stubMetrics{}
	runner := run.JobRunnerFunc(func(ctx context.Context, c *domain.Check, slot domain.Slot) error {
		return nil
	})
	overload := &stubOverloader{}
	clk := &stubClock{now: base}

	pool := run.NewWorkerPool(1, 5, clk, runner, overload).WithMetrics(sm)
	defer pool.Shutdown(context.Background())

	c := check(t)
	_, _ = pool.Submit(context.Background(), run.Job{Check: c, Slot: 1})

	time.Sleep(10 * time.Millisecond)
	if len(sm.queueDepths) == 0 {
		t.Error("expected metrics queue depths recorded")
	}
}

func TestRunOrchestrator_EvaluationUncertainAndInitialModel(t *testing.T) {
	ctx := context.Background()
	clk := &stubClock{now: base}

	// 1. Model returns VerdictUncertain -> Run degrades with ClassSemantic
	t.Run("VerdictUncertain degrades run", func(t *testing.T) {
		st := newStore(binding())
		st.last, st.hasLast = record("39", "5", false), true
		c := check(t)

		uncertainModel := &stubModel{
			resp: ports.ModelResponse{
				Text: `{"verdict": "uncertain", "explanation": "page layout ambiguous"}`,
			},
		}

		o := run.New(run.Deps{
			Clock:       clk,
			IDs:         &stubIDs{},
			Store:       st,
			Source:      okSource(),
			Extract:     stubExtractor{out: record("49", "5", false)},
			Fingerprint: stubFingerprint{fp: "fp-v1"},
			Secrets:     stubSecrets{},
			Model:       uncertainModel,
		})

		out, err := o.Run(ctx, c, 1)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if out.Run.State() != domain.StateDegraded {
			t.Errorf("expected StateDegraded for uncertain verdict, got: %s", out.Run.State())
		}
		if out.Run.Failure() == nil || out.Run.Failure().Code != "uncertain_evaluation" {
			t.Errorf("expected failure code uncertain_evaluation, got: %+v", out.Run.Failure())
		}
	})

	// 2. First observation with model configured
	t.Run("first observation model explanation", func(t *testing.T) {
		st := newStore(binding()) // hasLast = false
		c := check(t)

		initModel := &stubModel{
			resp: ports.ModelResponse{Text: "initial price observed at 49"},
		}

		o := run.New(run.Deps{
			Clock:       clk,
			IDs:         &stubIDs{},
			Store:       st,
			Source:      okSource(),
			Extract:     stubExtractor{out: record("49", "5", false)},
			Fingerprint: stubFingerprint{fp: "fp-v1"},
			Secrets:     stubSecrets{},
			Model:       initModel,
		})

		out, err := o.Run(ctx, c, 1)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if out.Run.State() != domain.StateChanged {
			t.Errorf("expected StateChanged on first observation, got: %s", out.Run.State())
		}
		if out.Run.Explanation() != "initial price observed at 49" {
			t.Errorf("expected model explanation on first run, got: %s", out.Run.Explanation())
		}
	})

	// 3. Secrets required but Deps.Secrets == nil
	t.Run("secrets required but resolver missing", func(t *testing.T) {
		st := newStore(binding())
		defWithSecret := check(t).ActiveDefinition()
		defWithSecret.Source = domain.SourceSpec{
			Kind:          domain.SourceHTTP,
			URL:           "https://example.test",
			SecretHeaders: map[string]domain.SecretRef{"Authorization": "api-key"},
		}
		cWithSecret, _ := domain.NewCheck("chk-sec", defWithSecret)

		o := run.New(run.Deps{
			Clock:       clk,
			IDs:         &stubIDs{},
			Store:       st,
			Source:      okSource(),
			Extract:     stubExtractor{out: record("49", "5", false)},
			Fingerprint: stubFingerprint{fp: "fp-v1"},
			Secrets:     nil, // Nil secrets resolver!
		})

		out, err := o.Run(ctx, cWithSecret, 1)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if out.Run.State() != domain.StateFailed {
			t.Errorf("expected StateFailed, got: %s", out.Run.State())
		}
		if out.Run.Failure() == nil || out.Run.Failure().Code != "secret_resolver_missing" {
			t.Errorf("expected failure code secret_resolver_missing, got: %+v", out.Run.Failure())
		}
	})
}

func TestOrchestratorDegradationModes(t *testing.T) {
	ctx := context.Background()
	clk := &stubClock{now: base}
	b := binding()
	st := newStore(b)
	st.last, st.hasLast = record("49", "5", false), true

	sm := &stubModel{}
	o := run.New(run.Deps{
		Clock:       clk,
		IDs:         &stubIDs{},
		Store:       st,
		Source:      okSource(),
		Extract:     stubExtractor{out: record("59", "5", false)}, // Changed value!
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
		Model:       sm,
	})

	// Default: Normal mode
	if o.Degradation() != domain.DegradationNormal {
		t.Fatalf("expected initial degradation normal, got: %s", o.Degradation())
	}

	// 1. ShedModel mode: model evaluation should be bypassed
	o.SetDegradation(domain.DegradationShedModel)
	if o.Degradation() != domain.DegradationShedModel {
		t.Fatalf("expected degradation shed_model, got: %s", o.Degradation())
	}

	out, err := o.Run(ctx, check(t), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Run.State() != domain.StateChanged {
		t.Errorf("expected StateChanged, got: %s", out.Run.State())
	}
	if sm.calls != 0 {
		t.Errorf("expected 0 model calls under ShedModel, got: %d", sm.calls)
	}
	if !strings.Contains(out.Run.Explanation(), "load shedding") {
		t.Errorf("expected load shedding explanation, got: %s", out.Run.Explanation())
	}

	// 2. Reduced retention mode on failed run
	o.SetDegradation(domain.DegradationReducedRetention)
	oFail := run.New(run.Deps{
		Clock:       clk,
		IDs:         &stubIDs{},
		Store:       st,
		Source:      okSource(),
		Extract:     stubExtractor{err: errors.New("broken selector")},
		Fingerprint: stubFingerprint{fp: "fp-v1"},
		Secrets:     stubSecrets{},
	})
	oFail.SetDegradation(domain.DegradationReducedRetention)

	outFail, err := oFail.Run(ctx, check(t), 2)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if outFail.Run.State() != domain.StateFailed {
		t.Errorf("expected StateFailed, got: %s", outFail.Run.State())
	}
}
