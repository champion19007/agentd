package plugins_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/plugins"
)

// TestHelperProcess serves as the subprocess implementation for tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("HELPER_MODE")
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 {
			continue
		}

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			caps := map[string]any{"tools": map[string]any{}}
			if mode == "missing_caps" {
				caps = map[string]any{}
			}
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    caps,
					"serverInfo":      map[string]any{"name": "test-plugin", "version": "1.0"},
				},
			}
			b, _ := json.Marshal(resp)
			fmt.Println(string(b))

		case "notifications/initialized":
			// Handshake complete, nothing to reply

		case "tools/call":
			switch mode {
			case "crash":
				os.Stderr.WriteString("fatal error: plugin crashed unexpectedly\n")
				os.Exit(1)

			case "timeout":
				time.Sleep(5 * time.Second)

			case "leak_secret_stderr":
				secretVal := os.Getenv("PLUGIN_API_KEY")
				os.Stderr.WriteString(fmt.Sprintf("debug connection failed with token: %s\n", secretVal))
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"isError": true,
						"content": []map[string]any{
							{"type": "text", "text": fmt.Sprintf("failed authenticating with secret token %s", secretVal)},
						},
					},
				}
				b, _ := json.Marshal(resp)
				fmt.Println(string(b))

			case "rate_limit":
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"isError": true,
						"content": []map[string]any{
							{"type": "text", "text": "HTTP 429: Too Many Requests; rate limit exceeded"},
						},
					},
				}
				b, _ := json.Marshal(resp)
				fmt.Println(string(b))

			case "auth_failure":
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"isError": true,
						"content": []map[string]any{
							{"type": "text", "text": "HTTP 401: Unauthorized access to source"},
						},
					},
				}
				b, _ := json.Marshal(resp)
				fmt.Println(string(b))

			default: // normal
				cwd, _ := os.Getwd()
				apiKey := os.Getenv("PLUGIN_API_KEY")
				hostSecret := os.Getenv("AGENTD_HOST_SECRET")

				payload := fmt.Sprintf(`{"status":"ok","cwd":%q,"api_key":%q,"host_secret":%q}`, cwd, apiKey, hostSecret)
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"isError": false,
						"content": []map[string]any{
							{"type": "text", "text": payload},
						},
					},
				}
				b, _ := json.Marshal(resp)
				fmt.Println(string(b))
			}
		}
	}
	os.Exit(0)
}

func helperSpec(t *testing.T, name, mode string) plugins.Spec {
	t.Helper()
	return plugins.Spec{
		Name:                 name,
		Command:              os.Args[0],
		Args:                 []string{"-test.run=TestHelperProcess", "--"},
		Env:                  []string{"GO_WANT_HELPER_PROCESS=1", "HELPER_MODE=" + mode},
		RequiredCapabilities: []string{"tools"},
		Timeout:              2 * time.Second,
		IdleTimeout:          10 * time.Minute,
		BackoffBase:          50 * time.Millisecond,
		MaxBackoff:           200 * time.Millisecond,
	}
}

func TestLazySpawnAndFetch(t *testing.T) {
	ctx := context.Background()
	host := plugins.NewHost()
	defer host.Close()

	spec := helperSpec(t, "test-scraper", "normal")
	if err := host.Register(spec); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Fetch triggers lazy spawn and handshake
	resp, err := host.Fetch(ctx, domain.SourceSpec{
		Kind:   domain.SourcePlugin,
		Plugin: "test-scraper",
		URL:    "https://example.com/items",
	}, domain.SecretBundle{})

	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200", resp.Status)
	}
	if !strings.Contains(string(resp.Body), `"status":"ok"`) {
		t.Errorf("Unexpected body: %s", string(resp.Body))
	}
}

func TestHandshakeAndCapabilityNegotiation(t *testing.T) {
	ctx := context.Background()
	host := plugins.NewHost()
	defer host.Close()

	// 1. Compatible plugin
	specOK := helperSpec(t, "plugin-ok", "normal")
	specOK.RequiredCapabilities = []string{"tools"}
	_ = host.Register(specOK)

	if err := host.VerifyCompatibility(ctx, "plugin-ok"); err != nil {
		t.Fatalf("VerifyCompatibility plugin-ok: %v", err)
	}

	// 2. Incompatible plugin (missing required capability)
	specBad := helperSpec(t, "plugin-bad", "missing_caps")
	specBad.RequiredCapabilities = []string{"tools"}
	_ = host.Register(specBad)

	err := host.VerifyCompatibility(ctx, "plugin-bad")
	if err == nil {
		t.Fatal("expected capability negotiation error, got nil")
	}

	if !strings.Contains(err.Error(), "capability") {
		t.Errorf("expected capability error message, got: %v", err)
	}
	// Fetching incompatible plugin must return ClassFatal
	_, fetchErr := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "plugin-bad"}, domain.SecretBundle{})
	if fetchErr == nil {
		t.Fatal("expected fetch error for incompatible plugin, got nil")
	}
	if fail, ok := fetchErr.(domain.Failure); ok {
		if fail.Class != domain.ClassFatal {
			t.Errorf("fail.Class = %v, want ClassFatal", fail.Class)
		}
	} else if dFail, ok := fetchErr.(*domain.Failure); ok {
		if dFail.Class != domain.ClassFatal {
			t.Errorf("dFail.Class = %v, want ClassFatal", dFail.Class)
		}
	}
}

func TestIdleTimeoutReapingAndRespawn(t *testing.T) {
	ctx := context.Background()
	host := plugins.NewHost()
	defer host.Close()

	spec := helperSpec(t, "idle-test", "normal")
	spec.IdleTimeout = 60 * time.Millisecond // fast idle timeout for test
	_ = host.Register(spec)

	// First call spawns the process
	resp1, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "idle-test"}, domain.SecretBundle{})
	if err != nil {
		t.Fatalf("Fetch 1: %v", err)
	}
	if resp1.Status != 200 {
		t.Errorf("resp1.Status = %d", resp1.Status)
	}

	// Wait for idle timer to fire and reap the process
	time.Sleep(100 * time.Millisecond)

	// Second call must lazily re-spawn a fresh process
	resp2, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "idle-test"}, domain.SecretBundle{})
	if err != nil {
		t.Fatalf("Fetch 2 (after idle reap): %v", err)
	}
	if resp2.Status != 200 {
		t.Errorf("resp2.Status = %d", resp2.Status)
	}
}

func TestRestartAfterCrashWithBackoff(t *testing.T) {
	ctx := context.Background()
	host := plugins.NewHost()
	defer host.Close()

	specCrash := helperSpec(t, "crasher", "crash")
	specCrash.BackoffBase = 80 * time.Millisecond
	specCrash.MaxBackoff = 300 * time.Millisecond
	_ = host.Register(specCrash)

	specHealthy := helperSpec(t, "healthy", "normal")
	_ = host.Register(specHealthy)

	// 1. Call crashing plugin -> fails
	_, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "crasher"}, domain.SecretBundle{})
	if err == nil {
		t.Fatal("expected crasher to fail, got nil")
	}

	// 2. Immediate second call must be blocked by backoff delay
	_, backoffErr := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "crasher"}, domain.SecretBundle{})
	if backoffErr == nil {
		t.Fatal("expected backoff rejection, got nil")
	}
	if !strings.Contains(backoffErr.Error(), "backing off") {
		t.Errorf("expected backoff error, got: %v", backoffErr)
	}

	// 3. Invariant: A broken plugin degrades only the check depending on it.
	// Healthy plugin must run without issue!
	respH, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "healthy"}, domain.SecretBundle{})
	if err != nil {
		t.Fatalf("healthy plugin should not be degraded by crashed plugin: %v", err)
	}
	if respH.Status != 200 {
		t.Errorf("healthy plugin status = %d", respH.Status)
	}

	// 4. Wait for backoff window to expire
	time.Sleep(100 * time.Millisecond)

	// Switch crasher spec to normal to verify recovery after backoff
	specRecovered := helperSpec(t, "crasher", "normal")
	_ = host.Register(specRecovered)

	respRec, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "crasher"}, domain.SecretBundle{})
	if err != nil {
		t.Fatalf("recovered plugin failed after backoff: %v", err)
	}
	if respRec.Status != 200 {
		t.Errorf("recovered plugin status = %d", respRec.Status)
	}
}

func TestPerCallTimeoutAndProcessTermination(t *testing.T) {
	ctx := context.Background()
	host := plugins.NewHost()
	defer host.Close()

	spec := helperSpec(t, "timeout-plugin", "timeout")
	spec.Timeout = 60 * time.Millisecond // very short per-call timeout
	_ = host.Register(spec)

	start := time.Now()
	_, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "timeout-plugin"}, domain.SecretBundle{})
	duration := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if duration > 1*time.Second {
		t.Errorf("call took %v, expected termination close to 60ms", duration)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout message, got: %v", err)
	}
}

func TestSandboxingAndEnvironmentIsolation(t *testing.T) {
	ctx := context.Background()
	tempRoot := t.TempDir()

	host := plugins.NewHost()
	host.SetSandboxRoot(tempRoot)
	defer host.Close()

	// Simulate a host-level secret that must NEVER be leaked to the plugin
	os.Setenv("AGENTD_HOST_SECRET", "super-secret-daemon-token-999")
	defer os.Unsetenv("AGENTD_HOST_SECRET")

	spec := helperSpec(t, "sandbox-plugin", "normal")
	spec.SecretEnv = map[string]domain.SecretRef{
		"PLUGIN_API_KEY": domain.SecretRef("api_credential"),
	}
	_ = host.Register(spec)

	// 1. Missing secret must fail with ClassAuth
	_, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "sandbox-plugin"}, domain.SecretBundle{})
	if err == nil {
		t.Fatal("expected secret_missing error, got nil")
	}
	if fail, ok := err.(domain.Failure); ok {
		if fail.Class != domain.ClassAuth || fail.Code != "secret_missing" {
			t.Errorf("unexpected failure for missing secret: %+v", fail)
		}
	}

	// 2. Supply secret bundle with scoped secret
	sec := domain.NewSecret("target-secret-val-42")
	bundle := domain.SecretBundle{domain.SecretRef("api_credential"): sec}

	resp, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "sandbox-plugin"}, bundle)
	if err != nil {
		t.Fatalf("Fetch with valid secret bundle: %v", err)
	}

	var data struct {
		CWD        string `json:"cwd"`
		APIKey     string `json:"api_key"`
		HostSecret string `json:"host_secret"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		t.Fatalf("Unmarshal response body: %v", err)
	}

	// Working directory MUST be the plugin's sandbox directory
	expectedSandbox := filepath.Join(tempRoot, "sandbox-plugin")
	if !strings.EqualFold(data.CWD, expectedSandbox) {
		t.Errorf("CWD = %q, want sandbox directory %q", data.CWD, expectedSandbox)
	}

	// Scoped secret MUST be received
	if data.APIKey != "target-secret-val-42" {
		t.Errorf("APIKey = %q, want target-secret-val-42", data.APIKey)
	}

	// Host daemon environment MUST NOT be inherited!
	if data.HostSecret != "" {
		t.Errorf("HostSecret was leaked to plugin: %q", data.HostSecret)
	}
}

func TestNeverLogSecrets(t *testing.T) {
	ctx := context.Background()
	host := plugins.NewHost()
	defer host.Close()

	spec := helperSpec(t, "leak-test", "leak_secret_stderr")
	spec.SecretEnv = map[string]domain.SecretRef{
		"PLUGIN_API_KEY": domain.SecretRef("leak_cred"),
	}
	_ = host.Register(spec)

	secretVal := "ultra-sensitive-api-token-888"
	sec := domain.NewSecret(secretVal)
	bundle := domain.SecretBundle{domain.SecretRef("leak_cred"): sec}

	_, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "leak-test"}, bundle)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errStr := err.Error()
	if fail, ok := err.(domain.Failure); ok {
		errStr = errStr + " " + fail.Detail
	}
	// Secret value MUST NOT appear anywhere in the error or detail
	if strings.Contains(errStr, secretVal) {
		t.Fatalf("CRITICAL SECURITY VIOLATION: secret %q was leaked in error message: %s", secretVal, errStr)
	}
	// Must be redacted
	if !strings.Contains(errStr, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in error message: %s", errStr)
	}
}

func TestTypedFailures(t *testing.T) {
	ctx := context.Background()
	host := plugins.NewHost()
	defer host.Close()

	// 1. Rate limited plugin (429)
	specRate := helperSpec(t, "rate-limited", "rate_limit")
	_ = host.Register(specRate)

	_, err := host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "rate-limited"}, domain.SecretBundle{})
	if err == nil {
		t.Fatal("expected rate limit error, got nil")
	}
	fail, ok := err.(domain.Failure)
	if !ok {
		t.Fatalf("expected domain.Failure, got %T: %v", err, err)
	}
	if fail.Class != domain.ClassRateLimited {
		t.Errorf("fail.Class = %v, want ClassRateLimited", fail.Class)
	}

	// 2. Auth failed plugin (401)
	specAuth := helperSpec(t, "auth-plugin", "auth_failure")
	_ = host.Register(specAuth)

	_, err = host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "auth-plugin"}, domain.SecretBundle{})
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
	fail, ok = err.(domain.Failure)
	if !ok {
		t.Fatalf("expected domain.Failure, got %T: %v", err, err)
	}
	if fail.Class != domain.ClassAuth {
		t.Errorf("fail.Class = %v, want ClassAuth", fail.Class)
	}

	// 3. Unknown plugin -> ClassFatal
	_, err = host.Fetch(ctx, domain.SourceSpec{Kind: domain.SourcePlugin, Plugin: "does-not-exist"}, domain.SecretBundle{})
	if err == nil {
		t.Fatal("expected unknown plugin error, got nil")
	}
	fail, ok = err.(domain.Failure)
	if !ok {
		t.Fatalf("expected domain.Failure, got %T: %v", err, err)
	}
	if fail.Class != domain.ClassFatal {
		t.Errorf("fail.Class = %v, want ClassFatal", fail.Class)
	}
}
