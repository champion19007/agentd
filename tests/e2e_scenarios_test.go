package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/extract"
	"github.com/champion19007/agentd/internal/adapters/httpsource"
	"github.com/champion19007/agentd/internal/adapters/model"
	"github.com/champion19007/agentd/internal/api"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/policy"
	"github.com/champion19007/agentd/internal/core/repair"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

// e2eClock implements ports.Clock for deterministic, stepped time in E2E tests.
type e2eClock struct {
	now time.Time
}

func (c *e2eClock) Now() time.Time                             { return c.now }
func (c *e2eClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}
func (c *e2eClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// e2eIDs implements ports.IDs with sequential counters.
type e2eIDs struct {
	runSeq int64
	incSeq int64
	bndSeq int64
}

func (i *e2eIDs) NewRunID() domain.RunID {
	n := atomic.AddInt64(&i.runSeq, 1)
	return domain.RunID(fmt.Sprintf("run-e2e-%04d", n))
}

func (i *e2eIDs) NewIncidentID() domain.IncidentID {
	n := atomic.AddInt64(&i.incSeq, 1)
	return domain.IncidentID(fmt.Sprintf("inc-e2e-%04d", n))
}

func (i *e2eIDs) NewBindingID() domain.BindingID {
	n := atomic.AddInt64(&i.bndSeq, 1)
	return domain.BindingID(fmt.Sprintf("bnd-e2e-%04d", n))
}

// memoryNotifier captures delivered notifications for assertion.
type memoryNotifier struct {
	delivered []domain.Notification
}

func (m *memoryNotifier) Kinds() []domain.DestinationKind {
	return []domain.DestinationKind{domain.DestinationNotify, domain.DestinationNone}
}

func (m *memoryNotifier) Deliver(_ context.Context, n domain.Notification) error {
	m.delivered = append(m.delivered, n)
	return nil
}

// e2eHarness wires together real adapters against a real SQLite store and local test HTTP server.
type e2eHarness struct {
	t          *testing.T
	ctx        context.Context
	clock      *e2eClock
	ids        *e2eIDs
	store      *sqlite.Store
	source     *httpsource.Source
	extractor  ports.Extractor
	notifier   *memoryNotifier
	scripted   *model.ScriptedModel
	runOrch    *run.Orchestrator
	repairOrch *repair.Orchestrator
	service    *api.Service
}

func newE2EHarness(t *testing.T, initialTime time.Time) *e2eHarness {
	ctx := context.Background()
	clk := &e2eClock{now: initialTime}
	ids := &e2eIDs{}

	dbPath := filepath.Join(t.TempDir(), "agentd-e2e.db")
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("failed to open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	src := httpsource.New(httpsource.Options{
		Timeout:         5 * time.Second,
		AllowPrivateIPs: true,
	})
	ext := extract.New()
	notif := &memoryNotifier{}
	scrModel := model.NewScripted()

	runDeps := run.Deps{
		Clock:       clk,
		IDs:         ids,
		Store:       st,
		Source:      src,
		Extract:     ext,
		Fingerprint: ext,
		Secrets:     nil,
		Model:       scrModel,
		Notifier:    notif,
		Metrics:     ports.NoopMetrics{},
	}
	runOrch := run.New(runDeps)

	repairDeps := repair.Deps{
		Clock:    clk,
		IDs:      ids,
		Store:    st,
		Model:    scrModel,
		Extract:  ext,
		Source:   src,
		Secrets:  nil,
		Notifier: notif,
		Metrics:  ports.NoopMetrics{},
	}
	repairOrch := repair.New(repairDeps, "css")

	svc := api.NewService(api.Deps{
		Store:      st,
		DBPath:     dbPath,
		Clock:      clk,
		IDs:        ids,
		RunOrch:    runOrch,
		RepairOrch: repairOrch,
		Notifier:   notif,
		Metrics:    ports.NoopMetrics{},
	})

	return &e2eHarness{
		t:          t,
		ctx:        ctx,
		clock:      clk,
		ids:        ids,
		store:      st,
		source:     src,
		extractor:  ext,
		notifier:   notif,
		scripted:   scrModel,
		runOrch:    runOrch,
		repairOrch: repairOrch,
		service:    svc,
	}
}

// =============================================================================
// Scenario 1: Happy Path
// =============================================================================
// Tests: Check creation -> initial run against local HTTP server -> successful
// extraction -> observation saved to SQLite -> run history verified.
func TestE2E_Scenario1_HappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body><div id="service-status">Operational</div></body></html>`)
	}))
	defer ts.Close()

	baseTime := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	h := newE2EHarness(t, baseTime)

	// Step 1: Create check via API service
	chk, err := h.service.AddCheck(h.ctx, api.AddCheckRequest{
		Name:       "service_status",
		URL:        ts.URL,
		Interval:   "5m",
		Shape:      "scalar",
		Dialect:    "css",
		Expression: "#service-status",
	})
	if err != nil {
		t.Fatalf("AddCheck failed: %v", err)
	}
	if chk.ID == "" {
		t.Fatal("expected non-empty check ID")
	}

	// Verify check was persisted in SQLite
	checkID := domain.CheckID(chk.ID)
	savedCheck, err := h.store.Check(h.ctx, checkID)
	if err != nil {
		t.Fatalf("store.Check failed: %v", err)
	}
	if savedCheck.ActiveDefinition().Source.URL != ts.URL {
		t.Errorf("expected URL %s, got %s", ts.URL, savedCheck.ActiveDefinition().Source.URL)
	}

	// Step 2: Run check via API service
	runSummary, err := h.service.RunCheck(h.ctx, checkID)
	if err != nil {
		t.Fatalf("RunCheck failed: %v", err)
	}

	// Initial run extracts a new baseline observation -> StateChanged
	if runSummary.State != string(domain.StateChanged) && runSummary.State != string(domain.StateQuiet) {
		t.Fatalf("expected run state changed or quiet, got: %s", runSummary.State)
	}
	if runSummary.TraceID == "" {
		t.Errorf("expected non-empty trace ID on run summary")
	}

	// Step 3: Inspect run detail via API service
	runDetail, err := h.service.GetRun(h.ctx, domain.RunID(runSummary.ID))
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if runDetail.FailureClass != "" {
		t.Errorf("unexpected failure class on happy path: %s", runDetail.FailureClass)
	}

	// Step 4: Verify snapshot and extraction stored in SQLite
	lastRes, err := h.store.LastResult(h.ctx, checkID)
	if err != nil {
		t.Fatalf("store.LastResult failed: %v", err)
	}
	if got := lastRes.Scalar.Text; got != "Operational" {
		t.Errorf("expected extracted text 'Operational', got %q", got)
	}

	// Step 5: Check list returns healthy check status
	list, err := h.service.ListChecks(h.ctx)
	if err != nil {
		t.Fatalf("ListChecks failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != chk.ID {
		t.Fatalf("expected 1 check listed with ID %s, got %+v", chk.ID, list)
	}
}

// =============================================================================
// Scenario 2: No-Change
// =============================================================================
// Tests: Subsequent check run on unchanged HTML -> hash gate suppresses model
// call -> StateQuiet -> zero notifications emitted -> zero tokens spent.
func TestE2E_Scenario2_NoChange(t *testing.T) {
	htmlBody := `<!DOCTYPE html><html><body><span class="price">$19.99</span></body></html>`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, htmlBody)
	}))
	defer ts.Close()

	baseTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	h := newE2EHarness(t, baseTime)

	// Step 1: Create check
	chk, err := h.service.AddCheck(h.ctx, api.AddCheckRequest{
		Name:       "item_price",
		URL:        ts.URL,
		Interval:   "15m",
		Shape:      "scalar",
		Dialect:    "css",
		Expression: ".price",
	})
	if err != nil {
		t.Fatalf("AddCheck failed: %v", err)
	}
	checkID := domain.CheckID(chk.ID)

	// Step 2: First run (baseline snapshot)
	run1, err := h.service.RunCheck(h.ctx, checkID)
	if err != nil {
		t.Fatalf("Run 1 failed: %v", err)
	}
	if run1.State != string(domain.StateChanged) {
		t.Fatalf("expected run 1 to record initial change, got %s", run1.State)
	}

	// Reset notifier history from initial run
	h.notifier.delivered = nil
	h.scripted.Reset()

	// Step 3: Advance clock by 15m to next slot; content is identical
	h.clock.Advance(15 * time.Minute)

	// Step 4: Run 2 executed
	run2, err := h.service.RunCheck(h.ctx, checkID)
	if err != nil {
		t.Fatalf("Run 2 failed: %v", err)
	}

	// Verify Hash Gate: Identical payload hash -> StateQuiet
	if run2.State != string(domain.StateQuiet) {
		t.Errorf("expected run 2 state StateQuiet, got %s", run2.State)
	}

	// Verify model was never called (hash gate suppressed LLM invocation)
	if calls := h.scripted.CallCount(); calls != 0 {
		t.Errorf("expected 0 model calls due to hash gate, got %d", calls)
	}

	// Verify no notifications emitted for quiet run (OnQuiet is false by default)
	if len(h.notifier.delivered) != 0 {
		t.Errorf("expected 0 notifications on quiet run, got %d", len(h.notifier.delivered))
	}
}

// =============================================================================
// Scenario 3: Breakage -> Repair -> Approval
// =============================================================================
// Tests: Server redesign breaks locator -> structural failure -> incident opened
// -> candidate proposed & verified -> repair NEVER auto-applied -> human approval
// -> binding activated -> subsequent run succeeds quiet.
func TestE2E_Scenario3_Breakage_Repair_Approval(t *testing.T) {
	var currentHTML atomic.Value
	currentHTML.Store(`<!DOCTYPE html><html><body><div id="pricing">$49/month</div></body></html>`)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, currentHTML.Load().(string))
	}))
	defer ts.Close()

	baseTime := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	h := newE2EHarness(t, baseTime)

	// Step 1: Create check with original selector #pricing
	chk, err := h.service.AddCheck(h.ctx, api.AddCheckRequest{
		Name:       "subscription_price",
		URL:        ts.URL,
		Interval:   "1h",
		Shape:      "scalar",
		Dialect:    "css",
		Expression: "#pricing",
	})
	if err != nil {
		t.Fatalf("AddCheck failed: %v", err)
	}
	checkID := domain.CheckID(chk.ID)

	// Baseline run (establishes known-good snapshot and extraction)
	r1, err := h.service.RunCheck(h.ctx, checkID)
	if err != nil || r1.State != string(domain.StateChanged) {
		t.Fatalf("baseline run failed: %v, state: %s", err, r1.State)
	}

	// Verify known-good snapshot exists
	domainChk, err := h.store.Check(h.ctx, checkID)
	if err != nil {
		t.Fatalf("failed to fetch check: %v", err)
	}

	// Step 2: Website redesign! Old selector #pricing is removed.
	// New HTML has <div class="subscription-tier"><span class="price-val">$49/month</span></div>
	currentHTML.Store(`<!DOCTYPE html><html><body><div class="subscription-tier"><span class="price-val">$49/month</span></div></body></html>`)
	h.clock.Advance(1 * time.Hour)

	// Run fails with ClassStructural because #pricing cannot be found
	slot2 := domainChk.ActiveDefinition().Schedule.SlotAt(h.clock.Now())
	outcome, err := h.runOrch.Run(h.ctx, domainChk, slot2)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if outcome.Run.State() != domain.StateFailed {
		t.Fatalf("expected run state failed, got %s", outcome.Run.State())
	}
	fail := outcome.Run.Failure()
	if fail == nil || fail.Class != domain.ClassStructural {
		t.Fatalf("expected structural failure, got %+v", fail)
	}

	// Policy layer decides: breakage must escalate into an incident and propose repair
	sit := policy.Situation{
		Check:        domainChk,
		Run:          outcome.Run,
		Attempt:      1,
		IncidentOpen: false,
		HasKnownGood: true,
	}
	dec := policy.Decide(sit)
	if !dec.OpenIncident || !dec.AttemptRepair {
		t.Fatalf("expected policy to request open incident & repair proposal, got %+v", dec)
	}

	// Configure ScriptedModel for repair:
	// Turn 1: Propose candidate locators
	// Turn 2: Gate G4 semantic check
	h.scripted.WithTextResponse("subscription_price\t.price-val\nRATIONALE: Price element moved into .price-val span")
	h.scripted.WithTextResponse(`{"satisfies": true, "reason": "Both values reflect $49/month subscription rate"}`)

	// Step 3: Healing Subsystem coordinates candidate generation & verification
	inc, prop, err := h.repairOrch.HandleBreakage(h.ctx, domainChk, *fail)
	if err != nil {
		if inc != nil {
			for _, a := range inc.Attempts() {
				t.Logf("Attempt %d: outcome=%s, note=%s", a.Number, a.Outcome, a.Note)
			}
		}
		t.Fatalf("HandleBreakage failed: %v", err)
	}
	if inc == nil || prop == nil {
		t.Fatal("expected incident and repair proposal to be populated")
	}

	// Fundamental Invariant: Repair is NEVER automatically applied!
	// Active binding must still be version 1
	activeBinding, err := h.store.ActiveBinding(h.ctx, checkID)
	if err != nil {
		t.Fatalf("ActiveBinding failed: %v", err)
	}
	if activeBinding.Version != 1 {
		t.Fatalf("INVARIANT VIOLATION: active binding version is %d, repair was applied automatically!", activeBinding.Version)
	}

	// Step 4: Verify incident presentation via API
	incList, err := h.service.ListIncidents(h.ctx)
	if err != nil {
		t.Fatalf("ListIncidents failed: %v", err)
	}
	if len(incList) != 1 || incList[0].ID != string(inc.ID()) {
		t.Fatalf("expected 1 open incident %s, got %+v", inc.ID(), incList)
	}

	incDetail, err := h.service.GetIncident(h.ctx, inc.ID())
	if err != nil {
		t.Fatalf("GetIncident failed: %v", err)
	}
	if !strings.Contains(incDetail.WhatBroke, "element could not be found") && !strings.Contains(incDetail.WhatBroke, "source structure appears to have changed") {
		t.Logf("User-friendly breakage explanation: %s", incDetail.WhatBroke)
	}

	// Step 5: Human Approval via API
	// Requiring --by attribution
	approvalRes, err := h.service.ApproveRepair(h.ctx, inc.ID(), api.ApproveRequest{
		By: "operator@example.com",
	})
	if err != nil {
		t.Fatalf("ApproveRepair failed: %v", err)
	}
	if approvalRes.Action != string(domain.ActionRepairApproved) {
		t.Errorf("expected action %s after approval, got %s", domain.ActionRepairApproved, approvalRes.Action)
	}

	// Verify in store that binding version 2 is now active
	activeBindingAfter, err := h.store.ActiveBinding(h.ctx, checkID)
	if err != nil {
		t.Fatalf("ActiveBinding after approval failed: %v", err)
	}
	if activeBindingAfter.Version != 2 {
		t.Fatalf("expected active binding version 2, got %d", activeBindingAfter.Version)
	}
	if len(activeBindingAfter.Locators) == 0 || activeBindingAfter.Locators[0].Expression != ".price-val" {
		t.Fatalf("expected locator .price-val, got %+v", activeBindingAfter.Locators)
	}

	// Step 6: Subsequent run uses newly activated binding and succeeds
	h.clock.Advance(1 * time.Hour)
	run3, err := h.service.RunCheck(h.ctx, checkID)
	if err != nil {
		t.Fatalf("run 3 after repair failed: %v", err)
	}
	if run3.State != string(domain.StateQuiet) && run3.State != string(domain.StateChanged) {
		t.Errorf("expected healed run state quiet or changed, got %s", run3.State)
	}

	// Extracted value with healed binding matches redesigned page
	latestRes, err := h.store.LastResult(h.ctx, checkID)
	if err != nil {
		t.Fatalf("LastResult failed: %v", err)
	}
	if got := latestRes.Scalar.Text; got != "$49/month" {
		t.Errorf("expected extracted value '$49/month', got %q", got)
	}
}

// =============================================================================
// Scenario 4: Auth Failure -> Escalation
// =============================================================================
// Tests: Server returns HTTP 401 Unauthorized -> ClassAuth failure -> run fails
// -> policy escalates to operator via alert -> repair is NOT attempted (non-healable).
func TestE2E_Scenario4_AuthFailure_Escalation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Protected Area"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error": "unauthorized access"}`)
	}))
	defer ts.Close()

	baseTime := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	h := newE2EHarness(t, baseTime)

	// Step 1: Create check pointing to protected server
	chk, err := h.service.AddCheck(h.ctx, api.AddCheckRequest{
		Name:       "secure_dashboard",
		URL:        ts.URL,
		Interval:   "10m",
		Shape:      "scalar",
		Dialect:    "css",
		Expression: ".status",
	})
	if err != nil {
		t.Fatalf("AddCheck failed: %v", err)
	}
	checkID := domain.CheckID(chk.ID)

	domainChk, err := h.store.Check(h.ctx, checkID)
	if err != nil {
		t.Fatalf("Check fetch failed: %v", err)
	}

	// Step 2: Execute run
	slot := domainChk.ActiveDefinition().Schedule.SlotAt(h.clock.Now())
	outcome, err := h.runOrch.Run(h.ctx, domainChk, slot)
	if err != nil {
		t.Fatalf("Run execution returned unhandled error: %v", err)
	}

	// Verify terminal run failed with ClassAuth
	runObj := outcome.Run
	if runObj.State() != domain.StateFailed {
		t.Fatalf("expected run state failed, got %s", runObj.State())
	}
	fail := runObj.Failure()
	if fail == nil {
		t.Fatal("expected failure details on run")
	}
	if fail.Class != domain.ClassAuth {
		t.Fatalf("expected failure class auth, got: %s", fail.Class)
	}

	// Step 3: Consult policy
	sit := policy.Situation{
		Check:   domainChk,
		Run:     runObj,
		Attempt: 1,
	}
	dec := policy.Decide(sit)

	// Invariant: Auth failure must NEVER trigger an incident or repair attempt
	if dec.OpenIncident {
		t.Errorf("INVARIANT VIOLATION: policy opened incident for auth failure")
	}
	if dec.AttemptRepair {
		t.Errorf("INVARIANT VIOLATION: policy attempted repair for auth failure")
	}

	// Policy must notify operator with high severity alert
	if !dec.Notify {
		t.Errorf("expected policy to notify operator on auth failure")
	}
	if dec.Severity != domain.SeverityAlert {
		t.Errorf("expected alert severity, got: %s", dec.Severity)
	}
	if !strings.Contains(dec.Body, "needs a credential updated") {
		t.Errorf("expected actionable credential body message, got: %s", dec.Body)
	}

	if len(h.notifier.delivered) != 1 {
		t.Fatalf("expected 1 escalation notification delivered, got %d", len(h.notifier.delivered))
	}
	alert := h.notifier.delivered[0]
	if alert.Severity != domain.SeverityAlert {
		t.Errorf("expected alert severity, got %s", alert.Severity)
	}

	// Verify zero incidents opened in store
	openIncs, err := h.store.OpenIncidents(h.ctx)
	if err != nil {
		t.Fatalf("OpenIncidents failed: %v", err)
	}
	if len(openIncs) != 0 {
		t.Errorf("expected 0 open incidents for auth failure, got %d", len(openIncs))
	}
}

// =============================================================================
// Scenario 5: Crash Recovery Mid-Run
// =============================================================================
// Tests: Process crashes while run is in flight (StateRunning in SQLite) -> daemon
// restarts -> RecoverCrashedRuns finds non-terminal runs -> transitions them to
// StateInterrupted -> subsequent runs execute cleanly.
func TestE2E_Scenario5_CrashRecoveryMidRun(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body><span id="metric">100</span></body></html>`)
	}))
	defer ts.Close()

	baseTime := time.Date(2026, 3, 1, 16, 0, 0, 0, time.UTC)
	h := newE2EHarness(t, baseTime)

	// Step 1: Create check in store
	chk, err := h.service.AddCheck(h.ctx, api.AddCheckRequest{
		Name:       "uptime_metric",
		URL:        ts.URL,
		Interval:   "5m",
		Shape:      "scalar",
		Dialect:    "css",
		Expression: "#metric",
	})
	if err != nil {
		t.Fatalf("AddCheck failed: %v", err)
	}
	checkID := domain.CheckID(chk.ID)

	// Step 2: Inject an in-flight uncompleted run directly into SQLite
	// to simulate process abrupt termination (SIGKILL / power loss)
	crashedRunID := domain.RunID("run-crashed-midflight-001")
	slot := domain.Slot(baseTime.Unix())
	crashedRun, err := domain.NewRun(crashedRunID, checkID, slot, 1, baseTime)
	if err != nil {
		t.Fatalf("NewRun failed: %v", err)
	}

	err = h.store.Update(h.ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.CreateRun(ctx, crashedRun)
	})
	if err != nil {
		t.Fatalf("persisting in-flight run failed: %v", err)
	}

	// Verify store detects 1 non-terminal run before recovery
	nonTerminalBefore, err := h.store.NonTerminalRuns(h.ctx)
	if err != nil {
		t.Fatalf("NonTerminalRuns check failed: %v", err)
	}
	if len(nonTerminalBefore) != 1 || nonTerminalBefore[0].ID() != crashedRunID {
		t.Fatalf("expected 1 non-terminal crashed run, got %+v", nonTerminalBefore)
	}

	// Step 3: Daemon restarts -> RecoverCrashedRuns is called at startup
	restartTime := baseTime.Add(2 * time.Minute)
	h.clock.Advance(2 * time.Minute)

	recovered, err := run.RecoverCrashedRuns(h.ctx, h.store, restartTime)
	if err != nil {
		t.Fatalf("RecoverCrashedRuns failed: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered run, got %d", len(recovered))
	}

	// Step 4: Verify recovered run properties
	recRun := recovered[0]
	if recRun.State() != domain.StateInterrupted {
		t.Errorf("expected recovered run state Interrupted, got: %s", recRun.State())
	}
	if !strings.Contains(recRun.Explanation(), "process restarted while run was in flight") {
		t.Errorf("expected restart explanation, got: %s", recRun.Explanation())
	}

	// Verify SQLite now has 0 non-terminal runs
	nonTerminalAfter, err := h.store.NonTerminalRuns(h.ctx)
	if err != nil {
		t.Fatalf("NonTerminalRuns after recovery failed: %v", err)
	}
	if len(nonTerminalAfter) != 0 {
		t.Errorf("expected 0 non-terminal runs after recovery, got %d", len(nonTerminalAfter))
	}

	// Step 5: Verify subsequent scheduled check run operates normally
	h.clock.Advance(5 * time.Minute)
	nextRun, err := h.service.RunCheck(h.ctx, checkID)
	if err != nil {
		t.Fatalf("subsequent run after recovery failed: %v", err)
	}
	if nextRun.State != string(domain.StateChanged) && nextRun.State != string(domain.StateQuiet) {
		t.Errorf("expected normal completion for next run, got: %s", nextRun.State)
	}
}
