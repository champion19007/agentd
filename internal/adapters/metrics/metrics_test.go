package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMetricsRegistryAllMetrics(t *testing.T) {
	reg := NewRegistry()

	// 1. agentd_runs_total{check,state}
	reg.RecordRun("check-1", "quiet")
	reg.RecordRun("check-1", "changed")
	reg.RecordRun("check-2", "failed")

	// 2. agentd_run_duration_seconds{check,stage}
	reg.RecordRunDuration("check-1", "fetch", 0.12)
	reg.RecordRunDuration("check-1", "extract", 0.03)

	// 3. agentd_check_staleness_seconds{check}
	reg.SetCheckStaleness("check-1", 45.5)
	reg.SetCheckStaleness("check-2", 0.0)

	// 4. agentd_model_tokens_total{model,purpose}
	// 5. agentd_model_cost_micros_total{model,purpose}
	reg.RecordModelUsage("claude-opus-5", "evaluate", 500, 1500)
	reg.RecordModelUsage("claude-opus-5", "repair", 1200, 3600)

	// 6. agentd_incidents_total{check,outcome}
	reg.RecordIncident("check-1", "opened")
	reg.RecordIncident("check-1", "proposed")
	reg.RecordIncident("check-1", "approved")

	// 7. agentd_incident_resolution_seconds
	reg.RecordIncidentResolution(125.0)

	// 8. agentd_queue_depth
	reg.SetQueueDepth(3)

	// 9. agentd_plugin_calls_total{plugin,result}
	reg.RecordPluginCall("webhook", "ok")
	reg.RecordPluginCall("slack", "error")

	out := reg.Export()

	expectedMetrics := []string{
		`agentd_runs_total{check="check-1",state="changed"} 1`,
		`agentd_runs_total{check="check-1",state="quiet"} 1`,
		`agentd_runs_total{check="check-2",state="failed"} 1`,
		`agentd_run_duration_seconds_bucket{check="check-1",stage="fetch"`,
		`agentd_run_duration_seconds_sum{check="check-1",stage="fetch"} 0.12`,
		`agentd_check_staleness_seconds{check="check-1"} 45.5`,
		`agentd_check_staleness_seconds{check="check-2"} 0`,
		`agentd_model_tokens_total{model="claude-opus-5",purpose="evaluate"} 500`,
		`agentd_model_tokens_total{model="claude-opus-5",purpose="repair"} 1200`,
		`agentd_model_cost_micros_total{model="claude-opus-5",purpose="evaluate"} 1500`,
		`agentd_model_cost_micros_total{model="claude-opus-5",purpose="repair"} 3600`,
		`agentd_incidents_total{check="check-1",outcome="approved"} 1`,
		`agentd_incidents_total{check="check-1",outcome="opened"} 1`,
		`agentd_incidents_total{check="check-1",outcome="proposed"} 1`,
		`agentd_incident_resolution_seconds_sum 125`,
		`agentd_queue_depth 3`,
		`agentd_plugin_calls_total{plugin="slack",result="error"} 1`,
		`agentd_plugin_calls_total{plugin="webhook",result="ok"} 1`,
	}

	for _, expected := range expectedMetrics {
		if !strings.Contains(out, expected) {
			t.Errorf("export missing expected line: %s\nFull output:\n%s", expected, out)
		}
	}

	// Test HTTP Handler
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	reg.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Errorf("expected text/plain content type, got %s", contentType)
	}
	if !strings.Contains(rec.Body.String(), "agentd_queue_depth 3") {
		t.Error("http response body missing metric")
	}
}

func TestMetricsRegistryConcurrency(t *testing.T) {
	reg := NewRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			reg.RecordRun("check-conc", "quiet")
			reg.RecordRunDuration("check-conc", "fetch", 0.05)
			reg.SetCheckStaleness("check-conc", float64(id))
			reg.RecordModelUsage("model-a", "eval", 10, 100)
			reg.RecordIncident("check-conc", "opened")
			reg.RecordIncidentResolution(10.0)
			reg.SetQueueDepth(id)
			reg.RecordPluginCall("plugin-a", "ok")
			_ = reg.Export()
		}(i)
	}

	wg.Wait()
}

func TestMetricsRegistry_CheckIDClamped(t *testing.T) {
	reg := NewRegistry()
	oversizedID := strings.Repeat("x", 200)
	reg.RecordRun(oversizedID, "succeeded")

	out := reg.Export()
	expectedLabel := strings.Repeat("x", 128)
	unexpectedLabel := strings.Repeat("x", 129)

	if !strings.Contains(out, `check="`+expectedLabel+`"`) {
		t.Errorf("export missing clamped check label of 128 chars")
	}
	if strings.Contains(out, `check="`+unexpectedLabel) {
		t.Errorf("export contains unclamped check label exceeding 128 chars")
	}
}

