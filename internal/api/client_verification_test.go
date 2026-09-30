package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/api"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

// setupVerificationEnv initializes an in-memory or temporary database populated with
// test checks, runs, and an incident for end-to-end client verification.
func setupVerificationEnv(t *testing.T) (*api.Service, *sqlite.Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentd_verify.db")
	ctx := context.Background()

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	baseTime := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	svc := api.NewService(api.Deps{
		Store:  st,
		DBPath: dbPath,
		Clock:  stubClock{now: baseTime},
	})

	// Pre-populate with check
	_, err = svc.AddCheck(ctx, api.AddCheckRequest{
		ID:         "check-pricing-v1",
		Name:       "Pricing Monitor",
		URL:        "https://example.com/pricing",
		Interval:   "10m",
		Target:     "price",
		Expression: "span.price",
		Dialect:    "css",
	})
	if err != nil {
		t.Fatalf("AddCheck: %v", err)
	}

	// Pre-populate with a completed run
	runID := domain.RunID("run-verify-1")
	run, err := domain.NewRun(runID, "check-pricing-v1", domain.Slot(1), 1, baseTime)
	if err != nil {
		t.Fatalf("domain.NewRun: %v", err)
	}
	_ = run.Start(baseTime, 1)
	_ = run.Quiet(baseTime.Add(2*time.Second), "hash-12345", domain.Extraction{})
	_ = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.CreateRun(ctx, run)
	})

	// Pre-populate with an open incident and proposal
	c, _ := st.Check(ctx, "check-pricing-v1")
	incLog := domain.NewIncidentLog(c.ID())
	inc, err := incLog.Open("inc-pricing-1", domain.Failure{
		Class:   domain.ClassStructural,
		Summary: "selector span.price matched nothing",
	}, 3, baseTime)
	if err != nil {
		t.Fatalf("incLog.Open: %v", err)
	}

	proposal := domain.RepairProposal{
		Binding: domain.Binding{
			ID:                "bin-pricing-2",
			CheckID:           "check-pricing-v1",
			DefinitionVersion: 1,
			IntentKind:        domain.IntentScalar,
			Fingerprint:       "fp-repaired",
			Version:           2,
			Origin:            domain.OriginRepaired,
			Locators: []domain.Locator{
				{Target: "price", Dialect: "css", Expression: "div.pricing-tag"},
			},
			DerivedAt: baseTime,
		},
		Rationale:       "Element changed to div.pricing-tag",
		VerifiedAgainst: "snap-pricing-1",
		VerifiedAt:      baseTime,
		Gates: []domain.GateResult{
			{Gate: domain.GateG1Structural, Passed: true, Detail: "ok", At: baseTime},
			{Gate: domain.GateG2Shape, Passed: true, Detail: "ok", At: baseTime},
			{Gate: domain.GateG3Stability, Passed: true, Detail: "ok", At: baseTime},
			{Gate: domain.GateG4Semantic, Passed: true, Detail: "ok", At: baseTime},
			{Gate: domain.GateG5Continuity, Passed: true, Detail: "ok", At: baseTime},
		},
	}
	_, _ = inc.RecordAttempt(domain.AttemptProposed, "repaired locator proposed", baseTime)
	_ = inc.Propose(proposal, baseTime)

	_ = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, proposal.Binding); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, inc)
	})

	return svc, st, dbPath
}

// ----------------------------------------------------------------------------
// 1. LOCAL HTTP API TESTS
// ----------------------------------------------------------------------------

func TestLocalHTTPAPI_SecurityDefaultsAndBinding(t *testing.T) {
	svc, _, _ := setupVerificationEnv(t)

	// Refusal to bind to non-loopback without allowRemote
	nonLoopback := []string{
		"0.0.0.0:8080",
		"0.0.0.0",
		"192.168.1.100:8080",
		"10.0.0.1:8080",
		"api.corp.internal:8080",
	}

	for _, addr := range nonLoopback {
		err := api.ValidateBindAddress(addr, false)
		if err == nil {
			t.Errorf("ValidateBindAddress(%q, false) expected security rejection, got nil", addr)
		} else if !strings.Contains(err.Error(), "refusing to bind to non-loopback address") {
			t.Errorf("ValidateBindAddress(%q, false) error %q does not mention security refusal", addr, err)
		}

		_, srvErr := api.NewServer(addr, svc, false)
		if srvErr == nil {
			t.Errorf("NewServer(%q, ..., false) expected error, got nil", addr)
		}
	}

	// Permitted when allowRemote = true
	for _, addr := range nonLoopback {
		if err := api.ValidateBindAddress(addr, true); err != nil {
			t.Errorf("ValidateBindAddress(%q, true) unexpected error: %v", addr, err)
		}
	}

	// Loopback bindings permitted
	loopbackAddrs := []string{
		"127.0.0.1:8080",
		"127.0.0.1:0",
		"localhost:8080",
		"[::1]:8080",
	}
	for _, addr := range loopbackAddrs {
		if err := api.ValidateBindAddress(addr, false); err != nil {
			t.Errorf("ValidateBindAddress(%q, false) unexpected error: %v", addr, err)
		}
	}

	// Verify server starts on loopback ephemeral port
	srv, err := api.NewServer("127.0.0.1:0", svc, false)
	if err != nil {
		t.Fatalf("NewServer loopback: %v", err)
	}

	go func() {
		_ = srv.Start()
	}()

	time.Sleep(50 * time.Millisecond)
	activeAddr := srv.Addr()
	if !strings.HasPrefix(activeAddr, "127.0.0.1:") {
		t.Errorf("activeAddr = %s, expected 127.0.0.1:*", activeAddr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Errorf("srv.Shutdown: %v", err)
	}
}

func TestLocalHTTPAPI_AllRoutesAndMethods(t *testing.T) {
	svc, _, _ := setupVerificationEnv(t)
	srv, err := api.NewServer("127.0.0.1:0", svc, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	go func() {
		_ = srv.Start()
	}()
	time.Sleep(50 * time.Millisecond)

	baseURL := "http://" + srv.Addr()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Helper for HTTP requests
	sendReq := func(method, path string, body []byte) (*http.Response, []byte) {
		req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s request failed: %v", method, path, err)
		}
		defer resp.Body.Close()
		respBytes, _ := io.ReadAll(resp.Body)
		return resp, respBytes
	}

	// 1. GET /v1/status
	resp, body := sendReq(http.MethodGet, "/v1/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/status status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}
	var status api.StatusResponse
	if err := json.Unmarshal(body, &status); err != nil || !status.Healthy {
		t.Errorf("GET /v1/status invalid response: %s", string(body))
	}
	// POST /v1/status -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodPost, "/v1/status", []byte(`{}`))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/status status = %d, want 405", resp.StatusCode)
	}

	// 2. POST /v1/init
	initReq, _ := json.Marshal(api.InitRequest{Path: filepath.Join(t.TempDir(), "init_test.db")})
	resp, body = sendReq(http.MethodPost, "/v1/init", initReq)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /v1/init status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}
	// GET /v1/init -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodGet, "/v1/init", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/init status = %d, want 405", resp.StatusCode)
	}
	// Malformed JSON body on /v1/init -> 400 Bad Request
	resp, _ = sendReq(http.MethodPost, "/v1/init", []byte(`{"path": broken json`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/init with malformed JSON status = %d, want 400", resp.StatusCode)
	}

	// 3. GET /v1/checks and POST /v1/checks
	resp, body = sendReq(http.MethodGet, "/v1/checks", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/checks status = %d, want 200", resp.StatusCode)
	}
	var checks []api.CheckSummary
	if err := json.Unmarshal(body, &checks); err != nil || len(checks) == 0 {
		t.Errorf("GET /v1/checks expected non-empty list: %s", string(body))
	}

	// Add check via POST
	addReq, _ := json.Marshal(api.AddCheckRequest{
		ID:         "check-new",
		Name:       "New Service Monitor",
		URL:        "https://example.com/api",
		Interval:   "5m",
		Target:     "status",
		Expression: "code",
	})
	resp, body = sendReq(http.MethodPost, "/v1/checks", addReq)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("POST /v1/checks status = %d, want 201. Body: %s", resp.StatusCode, string(body))
	}

	// Malformed JSON on POST /v1/checks -> 400 Bad Request
	resp, _ = sendReq(http.MethodPost, "/v1/checks", []byte(`{"id": "broken", interval:`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/checks malformed JSON status = %d, want 400", resp.StatusCode)
	}
	// Missing mandatory fields / invalid duration on POST /v1/checks -> 400 Bad Request
	badDurationReq, _ := json.Marshal(api.AddCheckRequest{ID: "check-bad", Name: "Bad", URL: "http://x", Interval: "invalid-duration"})
	resp, _ = sendReq(http.MethodPost, "/v1/checks", badDurationReq)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/checks bad duration status = %d, want 400", resp.StatusCode)
	}
	// Invalid methods on /v1/checks -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodDelete, "/v1/checks", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /v1/checks status = %d, want 405", resp.StatusCode)
	}

	// 4. GET /v1/checks/{id}
	resp, body = sendReq(http.MethodGet, "/v1/checks/check-new", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/checks/check-new status = %d, want 200", resp.StatusCode)
	}
	// Nonexistent check -> 404 Not Found
	resp, _ = sendReq(http.MethodGet, "/v1/checks/nonexistent-check-xyz", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/checks/nonexistent status = %d, want 404", resp.StatusCode)
	}
	// Invalid method on check item -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodPost, "/v1/checks/check-new", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/checks/check-new status = %d, want 405", resp.StatusCode)
	}

	// 5. POST /v1/checks/{id}/run
	resp, body = sendReq(http.MethodPost, "/v1/checks/check-new/run", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /v1/checks/check-new/run status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}
	// GET on /run -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodGet, "/v1/checks/check-new/run", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/checks/check-new/run status = %d, want 405", resp.StatusCode)
	}

	// 6. DELETE /v1/checks/{id}
	resp, body = sendReq(http.MethodDelete, "/v1/checks/check-new", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("DELETE /v1/checks/check-new status = %d, want 200", resp.StatusCode)
	}

	// 7. GET /v1/runs and GET /v1/runs/{id}
	resp, body = sendReq(http.MethodGet, "/v1/runs?check_id=check-pricing-v1&limit=5", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/runs status = %d, want 200", resp.StatusCode)
	}
	var runs []api.RunSummary
	if err := json.Unmarshal(body, &runs); err != nil || len(runs) == 0 {
		t.Errorf("GET /v1/runs expected runs: %s", string(body))
	}
	// POST on /v1/runs -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodPost, "/v1/runs", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/runs status = %d, want 405", resp.StatusCode)
	}

	// GET /v1/runs/{id}
	resp, body = sendReq(http.MethodGet, "/v1/runs/run-verify-1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/runs/run-verify-1 status = %d, want 200", resp.StatusCode)
	}
	// Missing run -> 404 Not Found
	resp, _ = sendReq(http.MethodGet, "/v1/runs/nonexistent-run-999", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/runs/nonexistent status = %d, want 404", resp.StatusCode)
	}
	// POST on run item -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodPost, "/v1/runs/run-verify-1", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/runs/{id} status = %d, want 405", resp.StatusCode)
	}

	// 8. GET /v1/incidents and GET /v1/incidents/{id}
	resp, body = sendReq(http.MethodGet, "/v1/incidents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/incidents status = %d, want 200", resp.StatusCode)
	}
	var incidents []api.IncidentSummary
	if err := json.Unmarshal(body, &incidents); err != nil || len(incidents) == 0 {
		t.Errorf("GET /v1/incidents expected incidents: %s", string(body))
	}
	// POST on /v1/incidents -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodPost, "/v1/incidents", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/incidents status = %d, want 405", resp.StatusCode)
	}

	// GET /v1/incidents/{id}
	resp, body = sendReq(http.MethodGet, "/v1/incidents/inc-pricing-1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/incidents/inc-pricing-1 status = %d, want 200", resp.StatusCode)
	}
	// Missing incident -> 404 Not Found
	resp, _ = sendReq(http.MethodGet, "/v1/incidents/nonexistent-inc-999", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/incidents/nonexistent status = %d, want 404", resp.StatusCode)
	}

	// 9. POST /v1/incidents/{id}/approve and reject
	// GET on /approve -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodGet, "/v1/incidents/inc-pricing-1/approve", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/incidents/{id}/approve status = %d, want 405", resp.StatusCode)
	}
	// GET on /reject -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodGet, "/v1/incidents/inc-pricing-1/reject", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/incidents/{id}/reject status = %d, want 405", resp.StatusCode)
	}
	// Malformed JSON on /approve -> 400 Bad Request
	resp, _ = sendReq(http.MethodPost, "/v1/incidents/inc-pricing-1/approve", []byte(`{by: invalid json`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/incidents/{id}/approve with malformed JSON status = %d, want 400", resp.StatusCode)
	}
	// Missing mandatory 'by' field on /approve -> 400 Bad Request
	resp, _ = sendReq(http.MethodPost, "/v1/incidents/inc-pricing-1/approve", []byte(`{"note": "approved"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/incidents/{id}/approve missing by status = %d, want 400", resp.StatusCode)
	}
	// Valid approval
	appReq, _ := json.Marshal(api.ApproveRequest{By: "operator-charlie", Note: "verified fix"})
	resp, body = sendReq(http.MethodPost, "/v1/incidents/inc-pricing-1/approve", appReq)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /v1/incidents/inc-pricing-1/approve status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}

	// 10. POST /v1/gc
	gcReq, _ := json.Marshal(api.GCRequest{DryRun: true, RunDays: 30})
	resp, body = sendReq(http.MethodPost, "/v1/gc", gcReq)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /v1/gc status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}
	// GET on /v1/gc -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodGet, "/v1/gc", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/gc status = %d, want 405", resp.StatusCode)
	}
	// Malformed JSON on /v1/gc -> 400 Bad Request
	resp, _ = sendReq(http.MethodPost, "/v1/gc", []byte(`{dry_run: not-json}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/gc malformed JSON status = %d, want 400", resp.StatusCode)
	}

	// 11. POST /v1/backup
	backupDest := filepath.Join(t.TempDir(), "api_test_backup.db")
	bReq, _ := json.Marshal(api.BackupRequest{To: backupDest, Verify: true})
	resp, body = sendReq(http.MethodPost, "/v1/backup", bReq)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /v1/backup status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}
	// GET on /v1/backup -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodGet, "/v1/backup", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/backup status = %d, want 405", resp.StatusCode)
	}
	// Malformed JSON on /v1/backup -> 400 Bad Request
	resp, _ = sendReq(http.MethodPost, "/v1/backup", []byte(`{broken: json`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/backup malformed JSON status = %d, want 400", resp.StatusCode)
	}

	// 12. GET /v1/audit
	resp, body = sendReq(http.MethodGet, "/v1/audit?limit=25", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/audit status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}
	var auditEvents []domain.AuditEvent
	if err := json.Unmarshal(body, &auditEvents); err != nil {
		t.Errorf("GET /v1/audit decode error: %v", err)
	}
	// POST on /v1/audit -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodPost, "/v1/audit", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/audit status = %d, want 405", resp.StatusCode)
	}

	// 13. POST /v1/mcp
	mcpPingReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "mcp-test-1",
		"method":  "ping",
	})
	resp, body = sendReq(http.MethodPost, "/v1/mcp", mcpPingReq)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /v1/mcp status = %d, want 200. Body: %s", resp.StatusCode, string(body))
	}
	// GET on /v1/mcp -> 405 Method Not Allowed
	resp, _ = sendReq(http.MethodGet, "/v1/mcp", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/mcp status = %d, want 405", resp.StatusCode)
	}
}

func TestLocalHTTPAPI_ConcurrentQueriesUnderLoad(t *testing.T) {
	svc, _, _ := setupVerificationEnv(t)
	srv, err := api.NewServer("127.0.0.1:0", svc, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	go func() {
		_ = srv.Start()
	}()
	time.Sleep(50 * time.Millisecond)

	baseURL := "http://" + srv.Addr()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// 50 concurrent workers running 10 requests each = 500 requests under load
	workers := 50
	requestsPerWorker := 10
	var wg sync.WaitGroup
	errCh := make(chan error, workers*requestsPerWorker)

	endpoints := []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodGet, "/v1/status", nil},
		{http.MethodGet, "/v1/checks", nil},
		{http.MethodGet, "/v1/checks/check-pricing-v1", nil},
		{http.MethodGet, "/v1/runs?check_id=check-pricing-v1&limit=5", nil},
		{http.MethodGet, "/v1/runs/run-verify-1", nil},
		{http.MethodGet, "/v1/audit?limit=10", nil},
		{http.MethodPost, "/v1/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)},
		{http.MethodPost, "/v1/mcp", []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)},
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for r := 0; r < requestsPerWorker; r++ {
				ep := endpoints[(workerID+r)%len(endpoints)]
				req, err := http.NewRequest(ep.method, baseURL+ep.path, bytes.NewReader(ep.body))
				if err != nil {
					errCh <- fmt.Errorf("worker %d req %d error: %w", workerID, r, err)
					return
				}
				if ep.body != nil {
					req.Header.Set("Content-Type", "application/json")
				}
				resp, err := httpClient.Do(req)
				if err != nil {
					errCh <- fmt.Errorf("worker %d req %d HTTP error: %w", workerID, r, err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errCh <- fmt.Errorf("worker %d req %d on %s got status %d", workerID, r, ep.path, resp.StatusCode)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	var failures []error
	for err := range errCh {
		failures = append(failures, err)
	}

	if len(failures) > 0 {
		t.Fatalf("%d out of %d concurrent requests failed under load: %v", len(failures), workers*requestsPerWorker, failures[0])
	}
}

// ----------------------------------------------------------------------------
// 2. AGENTD MCP SERVER PROTOCOL & SECURITY INVARIANTS
// ----------------------------------------------------------------------------

func TestMCPServer_HandshakeAndToolsListing(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := setupVerificationEnv(t)
	mcpServer := api.NewMCPServer(svc)

	// 1. initialize handshake
	initRaw := []byte(`{
		"jsonrpc": "2.0",
		"id": "agent-handshake-1",
		"method": "initialize",
		"params": {
			"protocolVersion": "2024-11-05",
			"capabilities": {},
			"clientInfo": {"name": "autonomous-verifier", "version": "2.0.0"}
		}
	}`)
	initRespBytes, err := mcpServer.HandleMessage(ctx, initRaw)
	if err != nil {
		t.Fatalf("HandleMessage initialize: %v", err)
	}
	var initResp mcpResp
	if err := json.Unmarshal(initRespBytes, &initResp); err != nil {
		t.Fatalf("Unmarshal initialize: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("initialize returned error: %+v", initResp.Error)
	}
	if initResp.ID != "agent-handshake-1" {
		t.Errorf("ID = %v, want agent-handshake-1", initResp.ID)
	}

	var initResult struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
		t.Fatalf("Unmarshal init result: %v", err)
	}
	if initResult.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want '2024-11-05'", initResult.ProtocolVersion)
	}
	if _, ok := initResult.Capabilities["tools"]; !ok {
		t.Errorf("capabilities missing tools: %+v", initResult.Capabilities)
	}
	if initResult.ServerInfo.Name != "agentd" {
		t.Errorf("serverInfo.name = %q, want 'agentd'", initResult.ServerInfo.Name)
	}

	// 2. notifications/initialized (notification -> nil response)
	notifRaw := []byte(`{"jsonrpc": "2.0", "method": "notifications/initialized"}`)
	notifResp, err := mcpServer.HandleMessage(ctx, notifRaw)
	if err != nil {
		t.Fatalf("notifications/initialized error: %v", err)
	}
	if len(notifResp) != 0 {
		t.Errorf("expected empty response for notification, got: %s", string(notifResp))
	}

	// 3. tools/list verification
	listRaw := []byte(`{"jsonrpc": "2.0", "id": 100, "method": "tools/list"}`)
	listRespBytes, err := mcpServer.HandleMessage(ctx, listRaw)
	if err != nil {
		t.Fatalf("tools/list error: %v", err)
	}
	var listResp mcpResp
	if err := json.Unmarshal(listRespBytes, &listResp); err != nil {
		t.Fatalf("Unmarshal tools/list: %v", err)
	}

	var toolListResult struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listResp.Result, &toolListResult); err != nil {
		t.Fatalf("Unmarshal tool list result: %v", err)
	}

	if len(toolListResult.Tools) != 7 {
		t.Fatalf("expected exactly 7 read tools, got %d", len(toolListResult.Tools))
	}

	expected7ReadTools := map[string]string{
		"list_checks":      "List all configured and active checks",
		"inspect_check":    "Get detailed information about a specific check",
		"list_runs":        "List recent execution runs for a check",
		"inspect_run":      "Get detailed outcome",
		"list_incidents":   "List all open breakage incidents",
		"inspect_incident": "Get comprehensive incident details",
		"inspect_status":   "Get the overall health",
	}

	foundTools := make(map[string]bool)
	for _, tool := range toolListResult.Tools {
		foundTools[tool.Name] = true
		descPrefix, ok := expected7ReadTools[tool.Name]
		if !ok {
			t.Errorf("unexpected tool in tools/list: %q", tool.Name)
			continue
		}
		if !strings.Contains(tool.Description, descPrefix) {
			t.Errorf("tool %q description %q does not contain %q", tool.Name, tool.Description, descPrefix)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q inputSchema is nil", tool.Name)
		}
	}

	for expectedName := range expected7ReadTools {
		if !foundTools[expectedName] {
			t.Errorf("missing required read tool: %q", expectedName)
		}
	}

	// Verify STRICT EXCLUSION of write operations
	forbiddenWriteTools := []string{
		"approve_repair",
		"reject_repair",
		"add_check",
		"delete_check",
		"run_check",
		"gc",
		"backup",
	}
	for _, forbidden := range forbiddenWriteTools {
		if foundTools[forbidden] {
			t.Fatalf("CRITICAL SECURITY VIOLATION: write tool %q was exposed in MCP tools/list", forbidden)
		}
	}
}

func TestMCPServer_ToolExecutionAndMissingParameters(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := setupVerificationEnv(t)
	mcpServer := api.NewMCPServer(svc)

	call := func(toolName string, args map[string]any) toolCallResult {
		params := map[string]any{"name": toolName}
		if args != nil {
			params["arguments"] = args
		}
		reqBytes, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      "call-1",
			"method":  "tools/call",
			"params":  params,
		})
		respBytes, err := mcpServer.HandleMessage(ctx, reqBytes)
		if err != nil {
			t.Fatalf("HandleMessage failed: %v", err)
		}
		var resp mcpResp
		if err := json.Unmarshal(respBytes, &resp); err != nil {
			t.Fatalf("Unmarshal resp: %v", err)
		}
		if resp.Error != nil {
			t.Fatalf("call %s returned JSON-RPC error: %+v", toolName, resp.Error)
		}
		var res toolCallResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatalf("Unmarshal toolResult: %v", err)
		}
		return res
	}

	// 1. inspect_status
	res := call("inspect_status", nil)
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, `"healthy": true`) {
		t.Errorf("inspect_status failed: %+v", res)
	}

	// 2. list_checks
	res = call("list_checks", nil)
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "check-pricing-v1") {
		t.Errorf("list_checks failed: %+v", res)
	}

	// 3. inspect_check: valid & missing check_id
	res = call("inspect_check", map[string]any{"check_id": "check-pricing-v1"})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "Pricing Monitor") {
		t.Errorf("inspect_check valid failed: %+v", res)
	}
	res = call("inspect_check", map[string]any{})
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "argument 'check_id' is required") {
		t.Errorf("inspect_check without check_id expected error, got: %+v", res)
	}

	// 4. list_runs: valid & missing check_id
	res = call("list_runs", map[string]any{"check_id": "check-pricing-v1", "limit": 10})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "run-verify-1") {
		t.Errorf("list_runs valid failed: %+v", res)
	}
	res = call("list_runs", map[string]any{})
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "argument 'check_id' is required") {
		t.Errorf("list_runs without check_id expected error, got: %+v", res)
	}

	// 5. inspect_run: valid & missing run_id
	res = call("inspect_run", map[string]any{"run_id": "run-verify-1"})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "hash-12345") {
		t.Errorf("inspect_run valid failed: %+v", res)
	}
	res = call("inspect_run", map[string]any{})
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "argument 'run_id' is required") {
		t.Errorf("inspect_run without run_id expected error, got: %+v", res)
	}

	// 6. list_incidents
	res = call("list_incidents", nil)
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "inc-pricing-1") {
		t.Errorf("list_incidents failed: %+v", res)
	}

	// 7. inspect_incident: valid & missing incident_id
	res = call("inspect_incident", map[string]any{"incident_id": "inc-pricing-1"})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "div.pricing-tag") {
		t.Errorf("inspect_incident valid failed: %+v", res)
	}
	res = call("inspect_incident", map[string]any{})
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "argument 'incident_id' is required") {
		t.Errorf("inspect_incident without incident_id expected error, got: %+v", res)
	}
}

func TestMCPServer_SecurityBoundaryStrictEnforcement(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := setupVerificationEnv(t)
	mcpServer := api.NewMCPServer(svc)

	writeOperations := []string{
		"approve_repair",
		"reject_repair",
		"add_check",
		"delete_check",
		"run_check",
		"gc",
		"backup",
	}

	for _, writeOp := range writeOperations {
		reqBytes, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      "sec-check-" + writeOp,
			"method":  "tools/call",
			"params": map[string]any{
				"name": writeOp,
				"arguments": map[string]any{
					"incident_id": "inc-pricing-1",
					"by":          "malicious-ai-agent",
				},
			},
		})

		respBytes, err := mcpServer.HandleMessage(ctx, reqBytes)
		if err != nil {
			t.Fatalf("HandleMessage writeOp %s failed: %v", writeOp, err)
		}

		var resp mcpResp
		if err := json.Unmarshal(respBytes, &resp); err != nil {
			t.Fatalf("Unmarshal resp %s: %v", writeOp, err)
		}

		var res toolCallResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatalf("Unmarshal toolResult %s: %v", writeOp, err)
		}

		// Security invariant verification
		if !res.IsError {
			t.Errorf("SECURITY INVARIANT VIOLATION: write operation %q succeeded via MCP!", writeOp)
		}
		if len(res.Content) == 0 {
			t.Errorf("write operation %q returned empty error content", writeOp)
			continue
		}
		expectedInvariant := "security violation: external agents cannot perform write actions or approve repairs via MCP; human approval must remain human approval"
		if res.Content[0].Text != expectedInvariant {
			t.Errorf("write operation %q returned wrong error text: %q, want %q", writeOp, res.Content[0].Text, expectedInvariant)
		}
	}
}

func TestMCPServer_ProtocolRobustnessAndEdgeCases(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := setupVerificationEnv(t)
	mcpServer := api.NewMCPServer(svc)

	// 1. Unknown tool
	reqBytes, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "unknown-tool-test",
		"method":  "tools/call",
		"params":  map[string]any{"name": "rm_rf_root"},
	})
	respBytes, _ := mcpServer.HandleMessage(ctx, reqBytes)
	var resp mcpResp
	_ = json.Unmarshal(respBytes, &resp)
	var res toolCallResult
	_ = json.Unmarshal(resp.Result, &res)
	if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, `unknown MCP tool "rm_rf_root"`) {
		t.Errorf("unknown tool expected isError=true with name, got: %+v", res)
	}

	// 2. Unknown method (-32601)
	reqBytes, _ = json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "unknown-method-test",
		"method":  "resources/list",
	})
	respBytes, _ = mcpServer.HandleMessage(ctx, reqBytes)
	_ = json.Unmarshal(respBytes, &resp)
	if resp.Error == nil || resp.Error.Code != -32601 || !strings.Contains(resp.Error.Message, "Method not found") {
		t.Errorf("expected -32601 Method not found, got: %+v", resp.Error)
	}

	// 3. Malformed JSON payload (-32700)
	respBytes, _ = mcpServer.HandleMessage(ctx, []byte(`{"jsonrpc": "2.0", "method": invalid`))
	_ = json.Unmarshal(respBytes, &resp)
	if resp.Error == nil || resp.Error.Code != -32700 || !strings.Contains(resp.Error.Message, "Parse error") {
		t.Errorf("expected -32700 Parse error, got: %+v", resp.Error)
	}

	// 4. Invalid params for tools/call (-32602)
	reqBytes, _ = json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "bad-params-test",
		"method":  "tools/call",
		"params":  "should-be-object-not-string",
	})
	respBytes, _ = mcpServer.HandleMessage(ctx, reqBytes)
	_ = json.Unmarshal(respBytes, &resp)
	if resp.Error == nil || resp.Error.Code != -32602 || !strings.Contains(resp.Error.Message, "Invalid params") {
		t.Errorf("expected -32602 Invalid params, got: %+v", resp.Error)
	}

	// 5. Notifications with no response
	// Notification with ping (no ID)
	respBytes, err := mcpServer.HandleMessage(ctx, []byte(`{"jsonrpc": "2.0", "method": "ping"}`))
	if err != nil || len(respBytes) != 0 {
		t.Errorf("expected nil response for notification ping, got: %s", string(respBytes))
	}
	// Notification with unknown method (no ID)
	respBytes, err = mcpServer.HandleMessage(ctx, []byte(`{"jsonrpc": "2.0", "method": "unknown_notif"}`))
	if err != nil || len(respBytes) != 0 {
		t.Errorf("expected nil response for unknown notification, got: %s", string(respBytes))
	}

	// 6. Large payloads up to 4MB
	// Generate large valid ping request (~2MB)
	padding := strings.Repeat("A", 2*1024*1024)
	largePingReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "large-ping",
		"method":  "ping",
		"params":  map[string]any{"padding": padding},
	})

	largeRespBytes, err := mcpServer.HandleMessage(ctx, largePingReq)
	if err != nil {
		t.Fatalf("HandleMessage large payload failed: %v", err)
	}
	var largeResp mcpResp
	if err := json.Unmarshal(largeRespBytes, &largeResp); err != nil || largeResp.Error != nil {
		t.Errorf("large payload processing failed: %+v", largeResp)
	}

	// Test large payload over stdio
	stdioIn := bytes.NewBuffer(append(largePingReq, '\n'))
	stdioOut := &bytes.Buffer{}
	if err := mcpServer.ServeStdio(ctx, stdioIn, stdioOut); err != nil {
		t.Fatalf("ServeStdio large payload failed: %v", err)
	}
	if !strings.Contains(stdioOut.String(), `"id":"large-ping"`) {
		t.Errorf("ServeStdio output does not contain expected response: %s", stdioOut.String())
	}
}
