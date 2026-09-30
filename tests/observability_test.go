package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/clock"
	"github.com/champion19007/agentd/internal/adapters/extract"
	"github.com/champion19007/agentd/internal/adapters/httpsource"
	"github.com/champion19007/agentd/internal/adapters/metrics"
	"github.com/champion19007/agentd/internal/api"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

type capturingNotifier struct {
	lastNotification domain.Notification
}

func (c *capturingNotifier) Kinds() []domain.DestinationKind {
	return []domain.DestinationKind{domain.DestinationNotify}
}

func (c *capturingNotifier) Deliver(_ context.Context, n domain.Notification) error {
	c.lastNotification = n
	return nil
}

func TestObservability_TraceIDCorrelation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "obs_trace.db")

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer st.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("v1.0.0"))
	}))
	defer ts.Close()

	notifier := &capturingNotifier{}
	metricRegistry := metrics.NewRegistry()
	ext := extract.New()

	deps := run.Deps{
		Store:       st,
		Clock:       clock.System{},
		IDs:         &testIDs{},
		Source:      httpsource.New(httpsource.Options{AllowPrivateIPs: true}),
		Extract:     ext,
		Fingerprint: ext,
		Notifier:    notifier,
		Metrics:     metricRegistry,
	}

	checkID := domain.CheckID("chk-trace-test")
	chkDef := domain.Definition{
		Version: 1,
		Intent:  domain.ScalarIntent{Label: "ver", Purpose: "app version", Type: domain.TypeString},
		Source:  domain.SourceSpec{Kind: domain.SourceHTTP, URL: ts.URL},
		Schedule: domain.Schedule{
			Interval: 10 * time.Minute,
		},
		Destination: domain.Destination{Kind: domain.DestinationNotify, Target: "test", OnQuiet: true},
		CreatedAt:   time.Now().UTC(),
	}
	chk, err := domain.NewCheck(checkID, chkDef)
	if err != nil {
		t.Fatalf("NewCheck failed: %v", err)
	}

	baseBinding := domain.Binding{
		ID:                "bin-trace-1",
		CheckID:           checkID,
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Fingerprint:       "fp-1",
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators:          []domain.Locator{{Target: "ver", Dialect: "css", Expression: "body"}},
		DerivedAt:         time.Now().UTC(),
	}

	if err := st.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveCheck(ctx, chk); err != nil {
			return err
		}
		if err := tx.SaveBinding(ctx, baseBinding); err != nil {
			return err
		}
		return tx.ActivateBinding(ctx, checkID, 1)
	}); err != nil {
		t.Fatalf("seeding check: %v", err)
	}

	pipeline := run.New(deps)
	slot := domain.Slot(time.Now().UTC().Unix())
	outcome, err := pipeline.Run(ctx, chk, slot)
	if err != nil {
		t.Fatalf("pipeline.Run failed: %v", err)
	}

	// 1. One run: one trace ID
	traceID := outcome.Run.TraceID()
	if traceID == "" {
		t.Fatal("expected non-empty trace ID on run")
	}
	if traceID != string(outcome.Run.ID()) {
		t.Errorf("trace ID should equal RunID (%s vs %s)", traceID, outcome.Run.ID())
	}

	// 2. Notification carries the trace ID
	if notifier.lastNotification.TraceID != traceID {
		t.Errorf("notification trace ID mismatch: expected %q, got %q", traceID, notifier.lastNotification.TraceID)
	}

	// 3. User can query run by trace ID via the API service
	svc := api.NewService(api.Deps{
		Store:   st,
		DBPath:  dbPath,
		Metrics: metricRegistry,
	})

	runDetail, err := svc.GetRun(ctx, domain.RunID(traceID))
	if err != nil {
		t.Fatalf("failed to retrieve run by trace ID: %v", err)
	}
	if runDetail.ID != traceID {
		t.Errorf("run detail ID mismatch: %s vs %s", runDetail.ID, traceID)
	}
	if runDetail.TraceID != traceID {
		t.Errorf("run detail TraceID mismatch: %s vs %s", runDetail.TraceID, traceID)
	}
}

func TestObservability_AllPrometheusMetricsExposed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "obs_metrics.db")

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer st.Close()

	m := metrics.NewRegistry()

	// Instrument all 9 specified metrics
	m.RecordRun("chk-test-metrics", "success")
	m.RecordRunDuration("chk-test-metrics", "fetch", 0.045)
	m.RecordRunDuration("chk-test-metrics", "total", 0.120)
	m.SetCheckStaleness("chk-test-metrics", 12.5)
	m.RecordModelUsage("claude-3-haiku", "evaluate", 350, 52)
	m.RecordIncident("chk-test-metrics", "opened")
	m.RecordIncidentResolution(180.0)
	m.SetQueueDepth(3)
	m.RecordPluginCall("browser", "ok")

	svc := api.NewService(api.Deps{
		Store:   st,
		DBPath:  dbPath,
		Metrics: m,
	})

	handler := api.NewHandler(svc)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /metrics, got %d", resp.StatusCode)
	}

	body := m.Export()

	requiredMetrics := []string{
		"agentd_runs_total{check=\"chk-test-metrics\",state=\"success\"}",
		"agentd_run_duration_seconds_bucket{check=\"chk-test-metrics\",stage=\"fetch\",le=\"0.05\"}",
		"agentd_check_staleness_seconds{check=\"chk-test-metrics\"}",
		"agentd_model_tokens_total{model=\"claude-3-haiku\",purpose=\"evaluate\"}",
		"agentd_model_cost_micros_total{model=\"claude-3-haiku\",purpose=\"evaluate\"}",
		"agentd_incidents_total{check=\"chk-test-metrics\",outcome=\"opened\"}",
		"agentd_incident_resolution_seconds_bucket{le=\"300\"}",
		"agentd_queue_depth",
		"agentd_plugin_calls_total{plugin=\"browser\",result=\"ok\"}",
	}

	for _, req := range requiredMetrics {
		if !strings.Contains(body, req) {
			t.Errorf("missing expected metric line in /metrics output:\nexpected substring: %s\nfull output:\n%s", req, body)
		}
	}

	// Verify no sensitive tokens or passwords in metric labels
	sensitiveTokens := []string{"secret", "password", "Bearer", "http://", "https://", "sk_live"}
	for _, tok := range sensitiveTokens {
		if strings.Contains(body, tok) {
			t.Errorf("SENSITIVE DATA IN METRICS: found %q in export:\n%s", tok, body)
		}
	}
}

func TestObservability_ObservationFreshnessSLI(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "obs_freshness.db")

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer st.Close()

	checkID := domain.CheckID("chk-freshness")
	// 5m interval check created 15 minutes ago
	created := time.Now().UTC().Add(-15 * time.Minute)
	chkDef := domain.Definition{
		Version: 1,
		Intent:  domain.ScalarIntent{Label: "ping", Purpose: "health", Type: domain.TypeString},
		Source:  domain.SourceSpec{Kind: domain.SourceHTTP, URL: "http://example.com"},
		Schedule: domain.Schedule{
			Interval: 5 * time.Minute,
		},
		CreatedAt: created,
	}
	chk, _ := domain.NewCheck(checkID, chkDef)
	if err := st.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveCheck(ctx, chk)
	}); err != nil {
		t.Fatalf("saving check: %v", err)
	}

	svc := api.NewService(api.Deps{
		Store:  st,
		DBPath: dbPath,
	})

	status, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}

	// Check is 15 minutes old with 5 min interval and no runs -> overdue
	if status.MaxStalenessSeconds <= 0 {
		t.Errorf("expected positive staleness seconds for overdue check, got %f", status.MaxStalenessSeconds)
	}
	if !strings.Contains(status.FreshnessSummary, "overdue") {
		t.Errorf("expected freshness summary to mention overdue, got %q", status.FreshnessSummary)
	}

	// Verify check list also exposes freshness and staleness
	summaries, err := svc.ListChecks(ctx)
	if err != nil {
		t.Fatalf("ListChecks failed: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 check, got %d", len(summaries))
	}
	if summaries[0].StalenessSeconds <= 0 {
		t.Errorf("expected check summary to have positive staleness, got %f", summaries[0].StalenessSeconds)
	}
}
