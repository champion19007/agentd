package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/extract"
	"github.com/champion19007/agentd/internal/adapters/model"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/repair"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/ports"
)

var pipelineBase = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

// fixedClock implements ports.Clock for deterministic testing.
type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time                             { return c.now }
func (c fixedClock) Sleep(context.Context, time.Duration) error { return nil }

type fixedIDs struct {
	seq int
}

func (i *fixedIDs) NewRunID() domain.RunID { i.seq++; return domain.RunID("run-" + itoa(i.seq)) }
func (i *fixedIDs) NewIncidentID() domain.IncidentID {
	i.seq++
	return domain.IncidentID("inc-" + itoa(i.seq))
}
func (i *fixedIDs) NewBindingID() domain.BindingID {
	i.seq++
	return domain.BindingID("bind-" + itoa(i.seq))
}

func itoa(n int) string {
	b := make([]byte, 0, 10)
	for n > 0 {
		b = append(b, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	if len(b) == 0 {
		return "0"
	}
	return string(b)
}

// pipelineStore implements the ports.Store methods needed across run and repair pipelines.
type pipelineStore struct {
	ports.Store

	bindings      map[int]domain.Binding
	activeVersion int
	lastResult    domain.Extraction
	hasLast       bool
	runs          map[domain.RunKey]*domain.Run
	snapshots     map[domain.CheckID]*domain.SnapshotIndex
	incidentLogs  map[domain.CheckID]*domain.IncidentLog
	openIncList   []*domain.Incident
	audit         []domain.AuditEvent
}

func newPipelineStore(b domain.Binding) *pipelineStore {
	store := &pipelineStore{
		bindings:      make(map[int]domain.Binding),
		activeVersion: b.Version,
		runs:          make(map[domain.RunKey]*domain.Run),
		snapshots:     make(map[domain.CheckID]*domain.SnapshotIndex),
		incidentLogs:  make(map[domain.CheckID]*domain.IncidentLog),
	}
	if b.ID != "" {
		store.bindings[b.Version] = b
	}
	return store
}

func (s *pipelineStore) ActiveBinding(context.Context, domain.CheckID) (domain.Binding, error) {
	b, ok := s.bindings[s.activeVersion]
	if !ok {
		return domain.Binding{}, ports.ErrNotFound
	}
	return b, nil
}

func (s *pipelineStore) SaveBinding(_ context.Context, b domain.Binding) error {
	s.bindings[b.Version] = b
	return nil
}

func (s *pipelineStore) ActivateBinding(_ context.Context, _ domain.CheckID, version int) error {
	s.activeVersion = version
	return nil
}

func (s *pipelineStore) LastResult(context.Context, domain.CheckID) (domain.Extraction, error) {
	if !s.hasLast {
		return domain.Extraction{}, ports.ErrNotFound
	}
	return s.lastResult, nil
}

func (s *pipelineStore) CreateRun(_ context.Context, r *domain.Run) error {
	s.runs[r.Key()] = r
	return nil
}

func (s *pipelineStore) UpdateRun(_ context.Context, r *domain.Run) error {
	s.runs[r.Key()] = r
	return nil
}

func (s *pipelineStore) Snapshots(_ context.Context, id domain.CheckID) (*domain.SnapshotIndex, error) {
	idx, ok := s.snapshots[id]
	if !ok {
		idx = domain.NewSnapshotIndex(id)
		s.snapshots[id] = idx
	}
	return idx, nil
}

func (s *pipelineStore) SaveSnapshot(_ context.Context, snap domain.Snapshot) error {
	idx, ok := s.snapshots[snap.CheckID()]
	if !ok {
		idx = domain.NewSnapshotIndex(snap.CheckID())
		s.snapshots[snap.CheckID()] = idx
	}
	return idx.Add(snap)
}

func (s *pipelineStore) Incidents(_ context.Context, id domain.CheckID) (*domain.IncidentLog, error) {
	log, ok := s.incidentLogs[id]
	if !ok {
		log = domain.NewIncidentLog(id)
		s.incidentLogs[id] = log
	}
	return log, nil
}

func (s *pipelineStore) OpenIncidents(context.Context) ([]*domain.Incident, error) {
	var out []*domain.Incident
	for _, log := range s.incidentLogs {
		for _, inc := range log.All() {
			if inc.Open() {
				out = append(out, inc)
			}
		}
	}
	return out, nil
}

func (s *pipelineStore) SaveIncident(_ context.Context, inc *domain.Incident) error {
	log, ok := s.incidentLogs[inc.CheckID()]
	if !ok {
		log = domain.NewIncidentLog(inc.CheckID())
		s.incidentLogs[inc.CheckID()] = log
	}
	return nil
}

func (s *pipelineStore) AppendAudit(_ context.Context, e domain.AuditEvent) error {
	s.audit = append(s.audit, e)
	return nil
}

func (s *pipelineStore) SaveCheck(_ context.Context, _ *domain.Check) error {
	return nil
}

func (s *pipelineStore) PutSnapshot(_ context.Context, snap domain.Snapshot) error {
	idx, ok := s.snapshots[snap.CheckID()]
	if !ok {
		idx = domain.NewSnapshotIndex(snap.CheckID())
		s.snapshots[snap.CheckID()] = idx
	}
	return idx.Add(snap)
}

func (s *pipelineStore) MarkSnapshotKnownGood(_ context.Context, check domain.CheckID, id domain.SnapshotID) error {
	idx, ok := s.snapshots[check]
	if !ok {
		idx = domain.NewSnapshotIndex(check)
		s.snapshots[check] = idx
	}
	return idx.MarkKnownGood(id)
}

func (s *pipelineStore) DeleteSnapshots(_ context.Context, _ domain.CheckID, _ []domain.SnapshotID) error {
	return nil
}

func (s *pipelineStore) Update(ctx context.Context, fn func(context.Context, ports.Tx) error) error {
	return fn(ctx, s)
}

type pipelineSource struct {
	resp domain.RawResponse
	err  error
}

func (s *pipelineSource) Fetch(context.Context, domain.SourceSpec, domain.SecretBundle) (domain.RawResponse, error) {
	return s.resp, s.err
}

type pipelineNotifier struct {
	notifs []domain.Notification
}

func (n *pipelineNotifier) Kinds() []domain.DestinationKind {
	return []domain.DestinationKind{domain.DestinationNotify}
}

func (n *pipelineNotifier) Deliver(_ context.Context, notif domain.Notification) error {
	n.notifs = append(n.notifs, notif)
	return nil
}

func testCheck(id domain.CheckID, in domain.Intent) *domain.Check {
	c, _ := domain.NewCheck(id, domain.Definition{
		Intent:    in,
		Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
		Schedule:  domain.Schedule{Interval: time.Hour},
		CreatedAt: pipelineBase,
	})
	return c
}

// ----------------------------------------------------------------------------
// Layer 3 Scenarios
// ----------------------------------------------------------------------------

// Scenario 1: No Change — Payload unchanged, hash gate suppresses model calls.
func TestPipeline_Scenario1_NoChange(t *testing.T) {
	ctx := context.Background()
	in := domain.ScalarIntent{Label: "price", Purpose: "product price", Type: domain.TypeNumber}
	chk := testCheck("chk-sc1", in)
	binding := domain.Binding{
		ID: "b-sc1", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{{Target: "price", Dialect: extract.DialectCSS, Expression: ".price"}},
	}
	html := `<html><body><span class="price">49.99</span></body></html>`

	store := newPipelineStore(binding)
	store.hasLast = true
	store.lastResult = domain.Extraction{
		Kind:   domain.IntentScalar,
		Scalar: domain.Value{Text: "49.99"},
	}

	src := &pipelineSource{
		resp: domain.RawResponse{ContentType: "text/html", Body: []byte(html), FetchedAt: pipelineBase},
	}
	scriptedModel := model.NewScripted() // No responses queued -> Complete would fail/panic if called
	ext := extract.New()
	notif := &pipelineNotifier{}

	orch := run.New(run.Deps{
		Clock:       fixedClock{now: pipelineBase},
		IDs:         &fixedIDs{},
		Store:       store,
		Source:      src,
		Extract:     ext,
		Fingerprint: ext,
		Model:       scriptedModel,
		Notifier:    notif,
	})

	outcome, err := orch.Run(ctx, chk, domain.Slot(1))
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if !outcome.Run.Terminal() {
		t.Error("expected run to be terminal")
	}
	if outcome.Run.State() != domain.StateQuiet {
		t.Errorf("expected StateQuiet on unchanged payload, got %s", outcome.Run.State())
	}
	// Verify zero model cost / zero calls
	if len(scriptedModel.Calls()) != 0 {
		t.Errorf("model was called %d times; hash gate MUST suppress model on unchanged payload", len(scriptedModel.Calls()))
	}
}

// Scenario 2: Real Change — Semantic price update, model returns structured verdict.
func TestPipeline_Scenario2_RealChange(t *testing.T) {
	ctx := context.Background()
	in := domain.ScalarIntent{Label: "price", Purpose: "product price", Type: domain.TypeNumber}
	chk := testCheck("chk-sc2", in)
	binding := domain.Binding{
		ID: "b-sc2", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{{Target: "price", Dialect: extract.DialectCSS, Expression: ".price"}},
	}
	html := `<html><body><span class="price">59.99</span></body></html>`

	store := newPipelineStore(binding)
	store.hasLast = true
	store.lastResult = domain.Extraction{
		Kind:   domain.IntentScalar,
		Scalar: domain.Value{Text: "49.99", Type: domain.TypeNumber},
	}

	src := &pipelineSource{
		resp: domain.RawResponse{ContentType: "text/html", Body: []byte(html), FetchedAt: pipelineBase},
	}
	scriptedModel := model.NewScripted().WithTextResponse(`{"verdict": "changed", "explanation": "Price increased from 49.99 to 59.99"}`)
	ext := extract.New()
	notif := &pipelineNotifier{}

	orch := run.New(run.Deps{
		Clock:       fixedClock{now: pipelineBase},
		IDs:         &fixedIDs{},
		Store:       store,
		Source:      src,
		Extract:     ext,
		Fingerprint: ext,
		Model:       scriptedModel,
		Notifier:    notif,
	})

	outcome, err := orch.Run(ctx, chk, domain.Slot(1))
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if outcome.Run.State() != domain.StateChanged {
		t.Errorf("expected StateChanged, got %s", outcome.Run.State())
	}
	if !strings.Contains(outcome.Run.Explanation(), "Price increased") {
		t.Errorf("explanation missing model reasoning: %q", outcome.Run.Explanation())
	}
	if len(scriptedModel.Calls()) != 1 {
		t.Errorf("model calls = %d, want 1", len(scriptedModel.Calls()))
	}
}

// Scenario 3: Extraction Degradation — Required field present, optional field missing.
func TestPipeline_Scenario3_ExtractionDegradation(t *testing.T) {
	ctx := context.Background()
	in := domain.RecordIntent{
		Label:   "product",
		Purpose: "product details",
		Fields: []domain.Field{
			{Name: "title", Type: domain.TypeString, Required: true},
			{Name: "badge", Type: domain.TypeString, Required: false}, // Optional
		},
	}
	chk := testCheck("chk-sc3", in)
	binding := domain.Binding{
		ID: "b-sc3", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentRecord,
		Locators: []domain.Locator{
			{Target: "title", Dialect: extract.DialectCSS, Expression: ".title"},
			{Target: "badge", Dialect: extract.DialectCSS, Expression: ".badge"},
		},
	}
	// Title exists, badge missing
	html := `<html><body><h1 class="title">Smart Watch</h1><!-- badge removed --></body></html>`

	store := newPipelineStore(binding)
	src := &pipelineSource{
		resp: domain.RawResponse{ContentType: "text/html", Body: []byte(html), FetchedAt: pipelineBase},
	}
	scriptedModel := model.NewScripted().WithTextResponse(`{"verdict": "changed", "explanation": "initial observation"}`)
	ext := extract.New()
	notif := &pipelineNotifier{}

	orch := run.New(run.Deps{
		Clock:       fixedClock{now: pipelineBase},
		IDs:         &fixedIDs{},
		Store:       store,
		Source:      src,
		Extract:     ext,
		Fingerprint: ext,
		Model:       scriptedModel,
		Notifier:    notif,
	})

	outcome, err := orch.Run(ctx, chk, domain.Slot(1))
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if outcome.Run.State() != domain.StateDegraded {
		t.Errorf("expected StateDegraded for missing optional field, got %s", outcome.Run.State())
	}
	// No structural incident opened
	log, _ := store.Incidents(ctx, chk.ID())
	if log.Len() != 0 {
		t.Errorf("incidents = %d, expected 0 for degraded optional field", log.Len())
	}
}

// Scenario 4: Structural Failure — Required field missing, incident opened.
func TestPipeline_Scenario4_StructuralFailure(t *testing.T) {
	ctx := context.Background()
	in := domain.ScalarIntent{Label: "price", Purpose: "product price", Type: domain.TypeNumber}
	chk := testCheck("chk-sc4", in)
	binding := domain.Binding{
		ID: "b-sc4", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{{Target: "price", Dialect: extract.DialectCSS, Expression: ".price"}},
	}
	// Price element completely gone
	html := `<html><body><h1 class="title">Product</h1></body></html>`

	store := newPipelineStore(binding)
	src := &pipelineSource{
		resp: domain.RawResponse{ContentType: "text/html", Body: []byte(html), FetchedAt: pipelineBase},
	}
	ext := extract.New()

	orch := run.New(run.Deps{
		Clock:       fixedClock{now: pipelineBase},
		IDs:         &fixedIDs{},
		Store:       store,
		Source:      src,
		Extract:     ext,
		Fingerprint: ext,
		Model:       model.NewScripted(),
		Notifier:    &pipelineNotifier{},
	})

	outcome, err := orch.Run(ctx, chk, domain.Slot(1))
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if outcome.Run.State() != domain.StateFailed {
		t.Errorf("expected StateFailed, got %s", outcome.Run.State())
	}
	if outcome.Run.Failure().Class != domain.ClassStructural {
		t.Errorf("expected ClassStructural failure, got %s", outcome.Run.Failure().Class)
	}
}

// Scenario 5: Successful Repair — Broken check -> Candidate generated -> Verified -> Approved -> Active.
func TestPipeline_Scenario5_SuccessfulRepair(t *testing.T) {
	ctx := context.Background()
	in := domain.ScalarIntent{Label: "price", Purpose: "standard price", Type: domain.TypeNumber}
	chk := testCheck("chk-sc5", in)
	oldBinding := domain.Binding{
		ID: "b-sc5-old", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{{Target: "price", Dialect: extract.DialectCSS, Expression: ".old-price"}},
	}

	goodHTML := `<html><body><span class="old-price">49</span></body></html>`
	brokenHTML := `<html><body><span class="new-price">49</span></body></html>`

	store := newPipelineStore(oldBinding)
	store.hasLast = true
	store.lastResult = domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "49"}}

	sGood, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(goodHTML), "fp-good", pipelineBase)
	sBroken, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(brokenHTML), "fp-broken", pipelineBase.Add(time.Hour))
	idx, _ := store.Snapshots(ctx, chk.ID())
	_ = idx.Add(sGood)
	_ = idx.MarkKnownGood(sGood.ID())
	_ = idx.Add(sBroken)

	log, _ := store.Incidents(ctx, chk.ID())
	inc, _ := log.Open("inc-sc5", domain.Failure{Class: domain.ClassStructural, Summary: "price missing"}, 3, pipelineBase)

	// Model proposes new locator targeting .new-price and confirms semantic verification for Gate G4
	repairModel := model.NewScripted().
		WithTextResponse("price\t.new-price\nRATIONALE: price moved to .new-price class").
		WithTextResponse(`{"satisfies": true, "reason": "price is correctly located"}`)
	ext := extract.New()
	notif := &pipelineNotifier{}

	repairOrch := repair.New(repair.Deps{
		Clock:    fixedClock{now: pipelineBase.Add(time.Hour)},
		IDs:      &fixedIDs{},
		Store:    store,
		Extract:  ext,
		Model:    repairModel,
		Notifier: notif,
	}, "css")

	// 1. Generate & verify proposal
	prop, err := repairOrch.Propose(ctx, chk, inc)
	if err != nil {
		t.Fatalf("repair Propose failed: %v", err)
	}
	if prop == nil {
		t.Fatal("expected proposal to be generated")
	}
	if inc.State() != domain.IncidentAwaitingApproval {
		t.Errorf("incident state = %s, want awaiting_approval", inc.State())
	}

	// 2. Human approval activates new binding
	approvedBinding, err := repairOrch.Approve(ctx, chk, inc.ID(), "operator-alice")
	if err != nil {
		t.Fatalf("Approve failed: %v", err)
	}
	if approvedBinding.Version != 2 {
		t.Errorf("approved binding version = %d, want 2", approvedBinding.Version)
	}
	if store.activeVersion != 2 {
		t.Errorf("store active version = %d, want 2", store.activeVersion)
	}

	// 3. Subsequent run with new binding succeeds cleanly
	runSrc := &pipelineSource{
		resp: domain.RawResponse{ContentType: "text/html", Body: []byte(brokenHTML), FetchedAt: pipelineBase.Add(2 * time.Hour)},
	}
	runOrch := run.New(run.Deps{
		Clock:       fixedClock{now: pipelineBase.Add(2 * time.Hour)},
		IDs:         &fixedIDs{},
		Store:       store,
		Source:      runSrc,
		Extract:     ext,
		Fingerprint: ext,
		Model:       model.NewScripted(), // No model needed on quiet run
		Notifier:    notif,
	})

	outcome, err := runOrch.Run(ctx, chk, domain.Slot(2))
	if err != nil {
		t.Fatalf("Run after repair failed: %v", err)
	}
	if outcome.Run.State() != domain.StateQuiet && !outcome.Run.State().Succeeded() {
		t.Errorf("expected clean run after repair approval, got %s", outcome.Run.State())
	}
}

// Scenario 6: Rejected Repair — Operator rejects proposal; binding remains unchanged.
func TestPipeline_Scenario6_RejectedRepair(t *testing.T) {
	ctx := context.Background()
	in := domain.ScalarIntent{Label: "price", Purpose: "standard price", Type: domain.TypeNumber}
	chk := testCheck("chk-sc6", in)
	oldBinding := domain.Binding{
		ID: "b-sc6-old", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{{Target: "price", Dialect: extract.DialectCSS, Expression: ".old-price"}},
	}

	goodHTML := `<html><body><span class="old-price">49</span></body></html>`
	brokenHTML := `<html><body><span class="new-price">49</span></body></html>`

	store := newPipelineStore(oldBinding)
	store.hasLast = true
	store.lastResult = domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "49"}}

	sGood, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(goodHTML), "fp-good", pipelineBase)
	sBroken, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(brokenHTML), "fp-broken", pipelineBase.Add(time.Hour))
	idx, _ := store.Snapshots(ctx, chk.ID())
	_ = idx.Add(sGood)
	_ = idx.MarkKnownGood(sGood.ID())
	_ = idx.Add(sBroken)

	log, _ := store.Incidents(ctx, chk.ID())
	inc, _ := log.Open("inc-sc6", domain.Failure{Class: domain.ClassStructural, Summary: "price missing"}, 3, pipelineBase)

	repairModel := model.NewScripted().
		WithTextResponse("price\t.new-price\nRATIONALE: locator proposal").
		WithTextResponse(`{"satisfies": true, "reason": "price is correctly located"}`)
	ext := extract.New()

	repairOrch := repair.New(repair.Deps{
		Clock:    fixedClock{now: pipelineBase.Add(time.Hour)},
		IDs:      &fixedIDs{},
		Store:    store,
		Extract:  ext,
		Model:    repairModel,
		Notifier: &pipelineNotifier{},
	}, "css")

	_, err := repairOrch.Propose(ctx, chk, inc)
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}

	// Operator rejects proposal
	err = repairOrch.Reject(ctx, inc.ID(), "operator-alice", "unacceptable proposed selector")
	if err != nil {
		t.Fatalf("Reject failed: %v", err)
	}

	if inc.State() != domain.IncidentAbandoned {
		t.Errorf("incident state = %s, want abandoned", inc.State())
	}
	if store.activeVersion != 1 {
		t.Errorf("active binding version changed after rejection: %d", store.activeVersion)
	}
}

// Scenario 7: Failed Verification — Candidate fails verification gate G1/G2; proposal not generated.
func TestPipeline_Scenario7_FailedVerification(t *testing.T) {
	ctx := context.Background()
	in := domain.ScalarIntent{Label: "price", Purpose: "standard price", Type: domain.TypeNumber}
	chk := testCheck("chk-sc7", in)
	oldBinding := domain.Binding{
		ID: "b-sc7-old", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{{Target: "price", Dialect: extract.DialectCSS, Expression: ".old-price"}},
	}

	goodHTML := `<html><body><span class="old-price">49</span></body></html>`
	brokenHTML := `<html><body><span class="not-a-number">Free Tier</span></body></html>`

	store := newPipelineStore(oldBinding)
	store.hasLast = true
	store.lastResult = domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "49", Type: domain.TypeNumber}}

	sGood, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(goodHTML), "fp-good", pipelineBase)
	sBroken, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(brokenHTML), "fp-broken", pipelineBase.Add(time.Hour))
	idx, _ := store.Snapshots(ctx, chk.ID())
	_ = idx.Add(sGood)
	_ = idx.MarkKnownGood(sGood.ID())
	_ = idx.Add(sBroken)

	log, _ := store.Incidents(ctx, chk.ID())
	inc, _ := log.Open("inc-sc7", domain.Failure{Class: domain.ClassStructural, Summary: "price missing"}, 3, pipelineBase)

	// Model proposes selector pointing to "Free Tier" which fails G2 shape/number validation
	repairModel := model.NewScripted().WithTextResponse("price\t.not-a-number\nRATIONALE: points to free tier text")
	ext := extract.New()

	repairOrch := repair.New(repair.Deps{
		Clock:    fixedClock{now: pipelineBase.Add(time.Hour)},
		IDs:      &fixedIDs{},
		Store:    store,
		Extract:  ext,
		Model:    repairModel,
		Notifier: &pipelineNotifier{},
	}, "css")

	_, err := repairOrch.Propose(ctx, chk, inc)
	if err == nil || !errors.Is(err, repair.ErrNotVerified) {
		t.Errorf("expected ErrNotVerified, got: %v", err)
	}

	// Verify attempt was recorded as unverified
	if len(inc.Attempts()) != 1 {
		t.Fatalf("attempts = %d, want 1", len(inc.Attempts()))
	}
	if inc.Attempts()[0].Outcome != domain.AttemptUnverified {
		t.Errorf("outcome = %s, want %s", inc.Attempts()[0].Outcome, domain.AttemptUnverified)
	}
	if inc.Proposal() != nil {
		t.Error("proposal must NOT be attached on failed verification")
	}
}

// Scenario 8: Model Outage — Model provider 503/error handled gracefully.
func TestPipeline_Scenario8_ModelOutage(t *testing.T) {
	ctx := context.Background()
	in := domain.ScalarIntent{Label: "price", Purpose: "standard price", Type: domain.TypeNumber}
	chk := testCheck("chk-sc8", in)
	oldBinding := domain.Binding{
		ID: "b-sc8-old", CheckID: chk.ID(), Version: 1, IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{{Target: "price", Dialect: extract.DialectCSS, Expression: ".old-price"}},
	}

	goodHTML := `<html><body><span class="old-price">49</span></body></html>`
	brokenHTML := `<html><body><span class="new-price">49</span></body></html>`

	store := newPipelineStore(oldBinding)
	store.hasLast = true
	store.lastResult = domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: "49", Type: domain.TypeNumber}}

	sGood, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(goodHTML), "fp-good", pipelineBase)
	sBroken, _ := domain.NewSnapshot(chk.ID(), "text/html", []byte(brokenHTML), "fp-broken", pipelineBase.Add(time.Hour))
	idx, _ := store.Snapshots(ctx, chk.ID())
	_ = idx.Add(sGood)
	_ = idx.MarkKnownGood(sGood.ID())
	_ = idx.Add(sBroken)

	log, _ := store.Incidents(ctx, chk.ID())
	inc, _ := log.Open("inc-sc8", domain.Failure{Class: domain.ClassStructural, Summary: "price missing"}, 3, pipelineBase)

	// Model returns transient outage error
	modelErr := domain.Failure{Class: domain.ClassTransient, Code: "upstream_unavailable", Summary: "503 Service Unavailable"}
	repairModel := model.NewScripted().WithError(modelErr)
	ext := extract.New()

	repairOrch := repair.New(repair.Deps{
		Clock:    fixedClock{now: pipelineBase.Add(time.Hour)},
		IDs:      &fixedIDs{},
		Store:    store,
		Extract:  ext,
		Model:    repairModel,
		Notifier: &pipelineNotifier{},
	}, "css")

	_, err := repairOrch.Propose(ctx, chk, inc)
	if err == nil {
		t.Fatal("expected error on model outage, got nil")
	}

	// Attempt recorded as AttemptModelError
	if len(inc.Attempts()) != 1 {
		t.Fatalf("attempts = %d, want 1", len(inc.Attempts()))
	}
	if inc.Attempts()[0].Outcome != domain.AttemptModelError {
		t.Errorf("outcome = %s, want %s", inc.Attempts()[0].Outcome, domain.AttemptModelError)
	}
	if inc.Open() != true {
		t.Error("incident should remain open to retry after outage resolves")
	}
}
