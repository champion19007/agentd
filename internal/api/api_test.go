package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/api"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

type stubClock struct {
	now time.Time
}

func (s stubClock) Now() time.Time { return s.now }

func (s stubClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func setupTestService(t *testing.T) (*api.Service, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ctx := context.Background()

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := api.NewService(api.Deps{
		Store:  st,
		DBPath: dbPath,
		Clock:  stubClock{now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)},
	})
	return svc, dbPath
}

func TestAPIServiceLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestService(t)

	// 1. Status
	status, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Healthy {
		t.Errorf("Healthy = %v, want true", status.Healthy)
	}
	if status.ChecksTotal != 0 {
		t.Errorf("ChecksTotal = %d, want 0", status.ChecksTotal)
	}

	// 2. AddCheck
	addReq := api.AddCheckRequest{
		ID:         "check-pricing",
		Name:       "Pro Plan Price",
		URL:        "https://example.com/pricing",
		Interval:   "15m",
		Target:     "price",
		Expression: "span.price",
		Dialect:    "css",
	}
	summary, err := svc.AddCheck(ctx, addReq)
	if err != nil {
		t.Fatalf("AddCheck: %v", err)
	}
	if summary.ID != "check-pricing" || summary.Name != "Pro Plan Price" {
		t.Errorf("unexpected CheckSummary: %+v", summary)
	}

	// 3. ListChecks
	checks, err := svc.ListChecks(ctx)
	if err != nil {
		t.Fatalf("ListChecks: %v", err)
	}
	if len(checks) != 1 || checks[0].ID != "check-pricing" {
		t.Errorf("ListChecks = %+v, want 1 check", checks)
	}

	// 4. GetCheck
	detail, err := svc.GetCheck(ctx, "check-pricing")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if detail.ID != "check-pricing" || len(detail.Locators) != 1 {
		t.Errorf("GetCheck = %+v", detail)
	}

	// 5. RunCheck
	runSum, err := svc.RunCheck(ctx, "check-pricing")
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if runSum.CheckID != "check-pricing" {
		t.Errorf("RunCheck = %+v", runSum)
	}

	// 6. DeleteCheck
	if err := svc.DeleteCheck(ctx, "check-pricing"); err != nil {
		t.Fatalf("DeleteCheck: %v", err)
	}

	// Verify check is now disabled
	checksAfter, err := svc.ListChecks(ctx)
	if err != nil {
		t.Fatalf("ListChecks after delete: %v", err)
	}
	if len(checksAfter) != 0 {
		t.Errorf("ListChecks after delete = %d, want 0", len(checksAfter))
	}

	// 7. GC
	sweep, err := svc.GC(ctx, api.GCRequest{DryRun: true})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	_ = sweep

	// 8. Backup
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	bRes, err := svc.Backup(ctx, api.BackupRequest{To: backupPath, Verify: true})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !bRes.Verified || bRes.SizeBytes <= 0 {
		t.Errorf("Backup result: %+v", bRes)
	}
}

func TestHTTPClientAndServer(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestService(t)

	handler := api.NewHandler(svc)
	server := httptest.NewServer(handler)
	defer server.Close()

	client := api.NewClient(server.URL, server.Client())

	// Status via HTTP client
	st, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("client.Status: %v", err)
	}
	if !st.Healthy {
		t.Errorf("client.Status Healthy = %v, want true", st.Healthy)
	}

	// AddCheck via HTTP client
	cSum, err := client.AddCheck(ctx, api.AddCheckRequest{
		ID:       "check-seats",
		Name:     "Available Seats",
		URL:      "https://example.com/flights",
		Interval: "5m",
	})
	if err != nil {
		t.Fatalf("client.AddCheck: %v", err)
	}
	if cSum.ID != "check-seats" {
		t.Errorf("client.AddCheck ID = %s, want check-seats", cSum.ID)
	}

	// ListChecks via HTTP client
	list, err := client.ListChecks(ctx)
	if err != nil {
		t.Fatalf("client.ListChecks: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("client.ListChecks count = %d, want 1", len(list))
	}

	// GetCheck via HTTP client
	detail, err := client.GetCheck(ctx, "check-seats")
	if err != nil {
		t.Fatalf("client.GetCheck: %v", err)
	}
	if detail.Name != "Available Seats" {
		t.Errorf("client.GetCheck Name = %s, want Available Seats", detail.Name)
	}
}

func TestIncidentApprovalAndRejection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_inc.db")

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	svc := api.NewService(api.Deps{
		Store:  st,
		DBPath: dbPath,
		Clock:  stubClock{now: base},
	})

	// Add a check
	_, err = svc.AddCheck(ctx, api.AddCheckRequest{
		ID:   "check-app",
		Name: "App Check",
		URL:  "https://example.com",
	})
	if err != nil {
		t.Fatalf("AddCheck: %v", err)
	}

	// Open an incident in the store
	c, _ := st.Check(ctx, "check-app")
	incLog := domain.NewIncidentLog(c.ID())
	inc, err := incLog.Open("inc-100", domain.Failure{
		Class:   domain.ClassStructural,
		Summary: "selector .button matched nothing",
	}, 3, base)
	if err != nil {
		t.Fatalf("incLog.Open: %v", err)
	}

	// Propose candidate
	prop := domain.RepairProposal{
		Binding: domain.Binding{
			ID:                "bin-2",
			CheckID:           "check-app",
			DefinitionVersion: 1,
			IntentKind:        domain.IntentScalar,
			Fingerprint:       "fp2",
			Version:           2,
			Origin:            domain.OriginRepaired,
			Locators: []domain.Locator{
				{Target: "value", Dialect: "css", Expression: "button.submit"},
			},
			DerivedAt: base,
		},
		Rationale:       "Button moved to class .submit",
		VerifiedAgainst: "snap-1",
		VerifiedAt:      base,
		Gates: []domain.GateResult{
			{Gate: domain.GateG1Structural, Passed: true, Detail: "ok", At: base},
			{Gate: domain.GateG2Shape, Passed: true, Detail: "ok", At: base},
			{Gate: domain.GateG3Stability, Passed: true, Detail: "ok", At: base},
			{Gate: domain.GateG4Semantic, Passed: true, Detail: "ok", At: base},
			{Gate: domain.GateG5Continuity, Passed: true, Detail: "ok", At: base},
		},
	}
	// Record attempt and attach proposal
	_, _ = inc.RecordAttempt(domain.AttemptProposed, "candidate proposed", base)
	_ = inc.Propose(prop, base)

	_ = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, prop.Binding); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, inc)
	})

	// 1. GetIncident
	detail, err := svc.GetIncident(ctx, "inc-100")
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if !detail.HasProposal || detail.CheckID != "check-app" {
		t.Errorf("GetIncident: %+v", detail)
	}
	if detail.WhatItProposes != "Button moved to class .submit" {
		t.Errorf("WhatItProposes = %q", detail.WhatItProposes)
	}

	// 2. RejectRepair without --by must fail
	_, err = svc.RejectRepair(ctx, "inc-100", api.RejectRequest{})
	if err == nil {
		t.Error("expected error approving/rejecting without --by")
	}

	// 3. ApproveRepair with valid --by
	appRes, err := svc.ApproveRepair(ctx, "inc-100", api.ApproveRequest{
		By: "operator-alice",
	})
	if err != nil {
		t.Fatalf("ApproveRepair: %v", err)
	}
	if appRes.Action != string(domain.ActionRepairApproved) {
		t.Errorf("Action = %s, want repair_approved", appRes.Action)
	}

	// Verify active binding updated to version 2
	b, err := st.ActiveBinding(ctx, "check-app")
	if err != nil {
		t.Fatalf("ActiveBinding: %v", err)
	}
	if b.Version != 2 || b.Origin != domain.OriginRepaired {
		t.Errorf("ActiveBinding: %+v", b)
	}
}

func TestCrossOriginRejection(t *testing.T) {
	svc, _ := setupTestService(t)
	handler := api.NewHandler(svc)

	tests := []struct {
		name       string
		host       string
		origin     string
		fetchSite  string
		wantStatus int
	}{
		{
			name:       "legitimate loopback request succeeds",
			host:       "127.0.0.1:8080",
			origin:     "http://localhost:3000",
			wantStatus: http.StatusOK,
		},
		{
			name:       "same-origin request succeeds",
			host:       "127.0.0.1:8080",
			origin:     "http://127.0.0.1:8080",
			fetchSite:  "same-origin",
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid localhost without origin succeeds",
			host:       "localhost:8080",
			wantStatus: http.StatusOK,
		},
		{
			name:       "malicious cross-origin request fails",
			host:       "127.0.0.1:8080",
			origin:     "https://evil.attacker.com",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "Sec-Fetch-Site cross-site fails",
			host:       "127.0.0.1:8080",
			fetchSite:  "cross-site",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "spoofed non-loopback Origin fails",
			host:       "127.0.0.1:8080",
			origin:     "http://127.0.0.1.attacker.com",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "opaque null Origin fails",
			host:       "127.0.0.1:8080",
			origin:     "null",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "existing Host validation still works (DNS rebinding)",
			host:       "evil.attacker.com",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/status", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status code = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestCrossOriginRejection_MutationsBlocked(t *testing.T) {
	svc, _ := setupTestService(t)
	handler := api.NewHandler(svc)

	mutationCases := []struct {
		name       string
		method     string
		path       string
		body       string
		origin     string
		fetchSite  string
		wantStatus int
	}{
		{
			name:       "cross-origin POST /v1/checks is blocked",
			method:     "POST",
			path:       "/v1/checks",
			body:       `{"id":"hack-check","name":"Hack","url":"http://evil.com","interval":"1h"}`,
			origin:     "https://malicious-website.com",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross-site Sec-Fetch-Site POST /v1/checks is blocked",
			method:     "POST",
			path:       "/v1/checks",
			body:       `{"id":"hack-check","name":"Hack","url":"http://evil.com","interval":"1h"}`,
			fetchSite:  "cross-site",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross-origin POST /v1/incidents/inc-1/approve is blocked",
			method:     "POST",
			path:       "/v1/incidents/inc-1/approve",
			body:       `{"by":"attacker"}`,
			origin:     "https://attacker.org",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross-site Sec-Fetch-Site POST /v1/incidents/inc-1/approve is blocked",
			method:     "POST",
			path:       "/v1/incidents/inc-1/approve",
			body:       `{"by":"attacker"}`,
			fetchSite:  "cross-site",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "legitimate loopback POST /v1/checks succeeds",
			method:     "POST",
			path:       "/v1/checks",
			body:       `{"id":"safe-check","name":"Safe Check","url":"http://example.com","interval":"1h","target":"price","expression":".price","dialect":"css"}`,
			origin:     "http://127.0.0.1:8080",
			fetchSite:  "same-origin",
			wantStatus: http.StatusCreated,
		},
	}

	for _, tc := range mutationCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Host = "127.0.0.1:8080"
			req.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status code = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestRequestBodyLimit_OversizedRejected(t *testing.T) {
	svc, _ := setupTestService(t)
	handler := api.NewHandler(svc)

	// Create an oversized payload > 4 MiB (4.5 MiB)
	oversizedBody := `{"id":"large-check","name":"` + strings.Repeat("A", 4500000) + `"}`
	req := httptest.NewRequest("POST", "/v1/checks", strings.NewReader(oversizedBody))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// MaxBytesReader should fail reading the payload, returning 413 or 400
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("expected 413 Request Entity Too Large or 400 Bad Request, got %d", rec.Code)
	}
}

func TestAuditLimit_Clamped(t *testing.T) {
	svc, _ := setupTestService(t)
	handler := api.NewHandler(svc)

	// Request with excessive limit (50000)
	req := httptest.NewRequest("GET", "/v1/audit?limit=50000", nil)
	req.Host = "127.0.0.1:8080"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d; body = %s", rec.Code, rec.Body.String())
	}
}
