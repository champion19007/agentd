package api_test

import (
	"bytes"
	"context"
	"encoding/json"
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

type mcpReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func TestMCPInitializeAndPing(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestService(t)
	mcpServer := api.NewMCPServer(svc)

	// 1. initialize
	initReq, _ := json.Marshal(mcpReq{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test-agent", "version": "1.0"},
		},
	})

	initRespBytes, err := mcpServer.HandleMessage(ctx, initReq)
	if err != nil {
		t.Fatalf("HandleMessage initialize: %v", err)
	}

	var initResp mcpResp
	if err := json.Unmarshal(initRespBytes, &initResp); err != nil {
		t.Fatalf("Unmarshal initialize response: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("initialize returned error: %+v", initResp.Error)
	}

	var initResult map[string]any
	if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
		t.Fatalf("Unmarshal init result: %v", err)
	}
	if initResult["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want 2024-11-05", initResult["protocolVersion"])
	}

	// 2. notifications/initialized (notification -> no response)
	notifReq, _ := json.Marshal(mcpReq{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	notifRespBytes, err := mcpServer.HandleMessage(ctx, notifReq)
	if err != nil {
		t.Fatalf("HandleMessage notification: %v", err)
	}
	if len(notifRespBytes) != 0 {
		t.Errorf("expected empty response for notification, got %s", string(notifRespBytes))
	}

	// 3. ping
	pingReq, _ := json.Marshal(mcpReq{
		JSONRPC: "2.0",
		ID:      "ping-1",
		Method:  "ping",
	})
	pingRespBytes, err := mcpServer.HandleMessage(ctx, pingReq)
	if err != nil {
		t.Fatalf("HandleMessage ping: %v", err)
	}
	var pingResp mcpResp
	if err := json.Unmarshal(pingRespBytes, &pingResp); err != nil {
		t.Fatalf("Unmarshal ping response: %v", err)
	}
	if pingResp.ID != "ping-1" {
		t.Errorf("ping ID = %v, want ping-1", pingResp.ID)
	}
}

func TestMCPToolsList(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestService(t)
	mcpServer := api.NewMCPServer(svc)

	listReq, _ := json.Marshal(mcpReq{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
	})
	respBytes, err := mcpServer.HandleMessage(ctx, listReq)
	if err != nil {
		t.Fatalf("HandleMessage tools/list: %v", err)
	}

	var resp mcpResp
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}

	var res struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("Unmarshal tools list result: %v", err)
	}

	toolNames := make(map[string]bool)
	for _, tool := range res.Tools {
		toolNames[tool.Name] = true
	}

	expectedTools := []string{
		"list_checks",
		"inspect_check",
		"list_runs",
		"inspect_run",
		"list_incidents",
		"inspect_incident",
		"inspect_status",
	}

	for _, expected := range expectedTools {
		if !toolNames[expected] {
			t.Errorf("missing expected MCP tool %q", expected)
		}
	}

	// Verify NO write tools are exposed
	forbiddenTools := []string{
		"approve_repair",
		"reject_repair",
		"add_check",
		"delete_check",
		"run_check",
		"gc",
		"backup",
	}
	for _, forbidden := range forbiddenTools {
		if toolNames[forbidden] {
			t.Errorf("MCP tools/list MUST NOT expose write tool %q", forbidden)
		}
	}
}

func setupTestServiceWithStore(t *testing.T) (*api.Service, *sqlite.Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_mcp.db")
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
	return svc, st, dbPath
}

func TestMCPToolsCallReadOperations(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := setupTestServiceWithStore(t)
	mcpServer := api.NewMCPServer(svc)

	// Add a check first
	_, err := svc.AddCheck(ctx, api.AddCheckRequest{
		ID:         "check-api",
		Name:       "API Health",
		URL:        "https://example.com/health",
		Interval:   "1m",
		Target:     "status",
		Expression: "div.ok",
	})
	if err != nil {
		t.Fatalf("AddCheck: %v", err)
	}

	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	// Save a run in the store for check-api
	runID := domain.RunID("run-101")
	run, err := domain.NewRun(runID, "check-api", domain.Slot(1), 1, base)
	if err != nil {
		t.Fatalf("domain.NewRun: %v", err)
	}
	_ = run.Start(base, 1)
	_ = run.Quiet(base, "", domain.Extraction{})
	_ = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.CreateRun(ctx, run)
	})

	// Also open and save an incident
	incLog := domain.NewIncidentLog("check-api")
	inc, _ := incLog.Open("inc-101", domain.Failure{
		Class:   domain.ClassStructural,
		Summary: "selector div.ok missing",
	}, 3, base)
	_ = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveIncident(ctx, inc)
	})

	callTool := func(name string, args map[string]any) toolCallResult {
		rawReq, _ := json.Marshal(mcpReq{
			JSONRPC: "2.0",
			ID:      10,
			Method:  "tools/call",
			Params: map[string]any{
				"name":      name,
				"arguments": args,
			},
		})
		respBytes, err := mcpServer.HandleMessage(ctx, rawReq)
		if err != nil {
			t.Fatalf("HandleMessage tools/call %s: %v", name, err)
		}
		var resp mcpResp
		if err := json.Unmarshal(respBytes, &resp); err != nil {
			t.Fatalf("Unmarshal resp %s: %v", name, err)
		}
		if resp.Error != nil {
			t.Fatalf("tools/call %s returned error: %+v", name, resp.Error)
		}
		var res toolCallResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatalf("Unmarshal toolResult %s: %v", name, err)
		}
		return res
	}

	// 1. list_checks
	res := callTool("list_checks", nil)
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "check-api") {
		t.Errorf("list_checks failed: %+v", res)
	}

	// 2. inspect_check
	res = callTool("inspect_check", map[string]any{"check_id": "check-api"})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "API Health") {
		t.Errorf("inspect_check failed: %+v", res)
	}

	// 3. list_runs
	res = callTool("list_runs", map[string]any{"check_id": "check-api", "limit": 10})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "run-101") {
		t.Errorf("list_runs failed: %+v", res)
	}

	// 4. inspect_run
	res = callTool("inspect_run", map[string]any{"run_id": "run-101"})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "run-101") {
		t.Errorf("inspect_run failed: %+v", res)
	}

	// 5. list_incidents
	res = callTool("list_incidents", nil)
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "inc-101") {
		t.Errorf("list_incidents failed: %+v", res)
	}

	// 6. inspect_incident
	res = callTool("inspect_incident", map[string]any{"incident_id": "inc-101"})
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "selector div.ok missing") {
		t.Errorf("inspect_incident failed: %+v", res)
	}

	// 7. inspect_status
	res = callTool("inspect_status", nil)
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, `"healthy": true`) {
		t.Errorf("inspect_status failed: %+v", res)
	}

	// 8. current_status alias
	res = callTool("current_status", nil)
	if res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, `"healthy": true`) {
		t.Errorf("current_status failed: %+v", res)
	}
}

func TestMCPToolsCallWriteOperationsBlocked(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestService(t)
	mcpServer := api.NewMCPServer(svc)

	writeTools := []string{
		"approve_repair",
		"reject_repair",
		"add_check",
		"delete_check",
		"run_check",
		"gc",
		"backup",
	}

	for _, tool := range writeTools {
		rawReq, _ := json.Marshal(mcpReq{
			JSONRPC: "2.0",
			ID:      99,
			Method:  "tools/call",
			Params: map[string]any{
				"name":      tool,
				"arguments": map[string]any{"incident_id": "inc-1"},
			},
		})
		respBytes, err := mcpServer.HandleMessage(ctx, rawReq)
		if err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		var resp mcpResp
		if err := json.Unmarshal(respBytes, &resp); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		var res toolCallResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatalf("Unmarshal toolResult: %v", err)
		}

		if !res.IsError {
			t.Errorf("tool %q MUST return isError=true for write operation", tool)
		}
		if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "security violation") {
			t.Errorf("tool %q error text must mention security violation, got: %+v", tool, res)
		}
	}
}

func TestMCPServeStdio(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestService(t)
	mcpServer := api.NewMCPServer(svc)

	in := &bytes.Buffer{}
	out := &bytes.Buffer{}

	// Write two JSON-RPC requests separated by newline
	req1, _ := json.Marshal(mcpReq{JSONRPC: "2.0", ID: 1, Method: "ping"})
	req2, _ := json.Marshal(mcpReq{JSONRPC: "2.0", ID: 2, Method: "tools/list"})
	in.Write(req1)
	in.WriteByte('\n')
	in.Write(req2)
	in.WriteByte('\n')

	err := mcpServer.ServeStdio(ctx, in, out)
	if err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 output lines, got %d: %q", len(lines), out.String())
	}

	var resp1, resp2 mcpResp
	if err := json.Unmarshal([]byte(lines[0]), &resp1); err != nil {
		t.Fatalf("Unmarshal line 1: %v", err)
	}
	if resp1.ID != float64(1) { // JSON unmarshals number as float64 into any
		t.Errorf("resp1.ID = %v, want 1", resp1.ID)
	}

	if err := json.Unmarshal([]byte(lines[1]), &resp2); err != nil {
		t.Fatalf("Unmarshal line 2: %v", err)
	}
	if resp2.ID != float64(2) {
		t.Errorf("resp2.ID = %v, want 2", resp2.ID)
	}
}

func TestMCPOverHTTP(t *testing.T) {
	ctx := context.Background()
	svc, _ := setupTestService(t)
	handler := api.NewHandler(svc)
	server := httptest.NewServer(handler)
	defer server.Close()

	// 1. POST /v1/mcp
	pingReq, _ := json.Marshal(mcpReq{
		JSONRPC: "2.0",
		ID:      "http-ping",
		Method:  "ping",
	})

	resp, err := server.Client().Post(server.URL+"/v1/mcp", "application/json", bytes.NewReader(pingReq))
	if err != nil {
		t.Fatalf("POST /v1/mcp: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %s, want application/json", ct)
	}

	var mResp mcpResp
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		t.Fatalf("Decode response: %v", err)
	}
	if mResp.ID != "http-ping" {
		t.Errorf("ID = %v, want http-ping", mResp.ID)
	}

	// 2. GET /v1/mcp (should return 405 Method Not Allowed)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/mcp", nil)
	getResp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/mcp: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", getResp.StatusCode)
	}
}

func TestValidateBindAddress(t *testing.T) {
	// Valid loopback addresses with allowRemote = false
	validLoopback := []string{
		"127.0.0.1:8080",
		"127.0.0.1:0",
		"localhost:8080",
		"[::1]:8080",
		"::1",
	}
	for _, addr := range validLoopback {
		if err := api.ValidateBindAddress(addr, false); err != nil {
			t.Errorf("ValidateBindAddress(%q, false) unexpected error: %v", addr, err)
		}
	}

	// Invalid remote addresses with allowRemote = false
	invalidRemote := []string{
		"0.0.0.0:8080",
		"0.0.0.0",
		"192.168.1.50:8080",
		"10.0.0.1:8080",
		"public.example.com:8080",
	}
	for _, addr := range invalidRemote {
		if err := api.ValidateBindAddress(addr, false); err == nil {
			t.Errorf("ValidateBindAddress(%q, false) expected error for remote address, got nil", addr)
		}
	}

	// Remote addresses allowed when allowRemote = true
	for _, addr := range invalidRemote {
		if err := api.ValidateBindAddress(addr, true); err != nil {
			t.Errorf("ValidateBindAddress(%q, true) unexpected error: %v", addr, err)
		}
	}
}
