package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/model"
	"github.com/champion19007/agentd/internal/adapters/secrets"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// 1. Valid provider response parsing
func TestClientValidResponse(t *testing.T) {
	var gotHeaderKey string
	var gotVersion string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaderKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"content": [{"type": "text", "text": "{\"verdict\": \"changed\", \"explanation\": \"value increased\"}"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 150, "output_tokens": 42}
		}`))
	}))
	defer srv.Close()

	apiKey := "sk-ant-api03-testkey-12345"
	client := model.New(model.Options{
		Endpoint:   srv.URL,
		APIKeyRef:  "anthropic-key",
		Secrets:    secrets.Static{"anthropic-key": apiKey},
		Model:      "claude-3-7-sonnet-20250219",
		MaxTokens:  1024,
		Timeout:    5 * time.Second,
	})

	resp, err := client.Complete(context.Background(), ports.ModelRequest{
		Purpose:         ports.PurposeEvaluate,
		System:          "you evaluate changes",
		Prompt:          "compare these extractions",
		Deterministic:   true,
		MaxOutputTokens: 512,
	})
	if err != nil {
		t.Fatalf("Complete unexpected error: %v", err)
	}

	if gotHeaderKey != apiKey {
		t.Errorf("x-api-key = %q, want %q", gotHeaderKey, apiKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header should not be empty")
	}
	if gotBody["model"] != "claude-3-7-sonnet-20250219" {
		t.Errorf("model = %v, want claude-3-7-sonnet-20250219", gotBody["model"])
	}
	if gotBody["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want 512", gotBody["max_tokens"])
	}
	if temp, ok := gotBody["temperature"]; !ok || temp != 0.0 {
		t.Errorf("temperature = %v, want 0.0 for deterministic request", temp)
	}
	if resp.InputTokens != 150 || resp.OutputTokens != 42 {
		t.Errorf("tokens = (%d, %d), want (150, 42)", resp.InputTokens, resp.OutputTokens)
	}
	if resp.Truncated {
		t.Error("response should not be marked truncated")
	}
	if !strings.Contains(resp.Text, "changed") {
		t.Errorf("Text = %q, want changed verdict", resp.Text)
	}
}

// 2. Truncation detection
func TestClientDetectsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"content": [{"type": "text", "text": "{\"verdict\": \"chan\"}"}],
			"stop_reason": "max_tokens",
			"usage": {"input_tokens": 100, "output_tokens": 10}
		}`))
	}))
	defer srv.Close()

	client := model.New(model.Options{
		Endpoint:  srv.URL,
		APIKeyRef: "k",
		Secrets:   secrets.Static{"k": "val"},
	})

	resp, err := client.Complete(context.Background(), ports.ModelRequest{Prompt: "test"})
	if err != nil {
		t.Fatalf("Complete unexpected error: %v", err)
	}
	if !resp.Truncated {
		t.Error("expected Truncated = true when stop_reason is max_tokens")
	}
}

// 3. Provider failure classification
func TestClientStatusClassifications(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantClass domain.FailureClass
		wantCode  string
	}{
		{
			name:      "rate limited (429)",
			status:    http.StatusTooManyRequests,
			body:      `{"error": {"type": "rate_limit_error", "message": "Number of request tokens has exceeded your quota."}}`,
			wantClass: domain.ClassRateLimited,
			wantCode:  "model_rate_limited",
		},
		{
			name:      "unauthorized (401)",
			status:    http.StatusUnauthorized,
			body:      `{"error": {"type": "authentication_error", "message": "invalid x-api-key"}}`,
			wantClass: domain.ClassAuth,
			wantCode:  "model_unauthorised",
		},
		{
			name:      "forbidden (403)",
			status:    http.StatusForbidden,
			body:      `{"error": {"type": "permission_error", "message": "access forbidden"}}`,
			wantClass: domain.ClassAuth,
			wantCode:  "model_unauthorised",
		},
		{
			name:      "billing problem (402)",
			status:    http.StatusPaymentRequired,
			body:      `{"error": {"type": "billing_error", "message": "credit balance is too low"}}`,
			wantClass: domain.ClassAuth,
			wantCode:  "model_billing",
		},
		{
			name:      "server internal error (500)",
			status:    http.StatusInternalServerError,
			body:      `{"error": {"type": "api_error", "message": "internal server error"}}`,
			wantClass: domain.ClassTransient,
			wantCode:  "model_server_error",
		},
		{
			name:      "bad gateway (502)",
			status:    http.StatusBadGateway,
			body:      `<html>502 Bad Gateway</html>`,
			wantClass: domain.ClassTransient,
			wantCode:  "model_server_error",
		},
		{
			name:      "unhandled status code (418)",
			status:    http.StatusTeapot,
			body:      `I'm a teapot`,
			wantClass: domain.ClassTransient,
			wantCode:  "model_http_418",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			client := model.New(model.Options{
				Endpoint:  srv.URL,
				APIKeyRef: "k",
				Secrets:   secrets.Static{"k": "val"},
			})

			_, err := client.Complete(context.Background(), ports.ModelRequest{Prompt: "test"})
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", tt.status)
			}
			f := domain.Classify(err)
			if f.Class != tt.wantClass {
				t.Errorf("Class = %q, want %q", f.Class, tt.wantClass)
			}
			if f.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", f.Code, tt.wantCode)
			}
		})
	}
}

// 4. Secret resolution failures
func TestClientSecretResolution(t *testing.T) {
	t.Run("missing secret reference", func(t *testing.T) {
		client := model.New(model.Options{
			Endpoint: "http://localhost:12345",
			// APIKeyRef unset
		})
		_, err := client.Complete(context.Background(), ports.ModelRequest{Prompt: "test"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		f := domain.Classify(err)
		if f.Class != domain.ClassAuth || f.Code != "model_key_unset" {
			t.Errorf("unexpected failure: %+v", f)
		}
	})

	t.Run("secret not found in resolver", func(t *testing.T) {
		client := model.New(model.Options{
			Endpoint:  "http://localhost:12345",
			APIKeyRef: "non-existent-key",
			Secrets:   secrets.Static{"other-key": "secret"},
		})
		_, err := client.Complete(context.Background(), ports.ModelRequest{Prompt: "test"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		f := domain.Classify(err)
		if f.Class != domain.ClassAuth || (f.Code != "model_key_missing" && f.Code != "secret_missing") {
			t.Errorf("unexpected failure: %+v", f)
		}
	})
}

// 5. Secret leakage prevention: API key never appears in error messages, details, or logs
func TestClientSecretLeakageScrubbing(t *testing.T) {
	apiKey := "sk-super-confidential-secret-key-12345"

	// Provider echoes back error containing the secret key
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Simulating buggy/misconfigured upstream error echoing back secret
		w.Write([]byte(`{"error": {"type": "authentication_error", "message": "Key ` + apiKey + ` is invalid"}}`))
	}))
	defer srv.Close()

	client := model.New(model.Options{
		Endpoint:  srv.URL,
		APIKeyRef: "secret-key",
		Secrets:   secrets.Static{"secret-key": apiKey},
	})

	_, err := client.Complete(context.Background(), ports.ModelRequest{Prompt: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errStr := err.Error()
	if strings.Contains(errStr, apiKey) {
		t.Fatalf("CRITICAL SECRET LEAK: error string contains secret key: %s", errStr)
	}

	f := domain.Classify(err)
	if strings.Contains(f.Summary, apiKey) {
		t.Fatalf("CRITICAL SECRET LEAK: failure summary contains secret key: %s", f.Summary)
	}
	if strings.Contains(f.Detail, apiKey) {
		t.Fatalf("CRITICAL SECRET LEAK: failure detail contains secret key: %s", f.Detail)
	}
	if !strings.Contains(f.Detail, "[redacted]") {
		t.Errorf("Detail = %q, expected [redacted] marker", f.Detail)
	}
}

// 6. Network timeout handling
func TestClientTimeoutHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte(`{"content": [{"type": "text", "text": "ok"}]}`))
	}))
	defer srv.Close()

	client := model.New(model.Options{
		Endpoint:  srv.URL,
		APIKeyRef: "k",
		Secrets:   secrets.Static{"k": "val"},
		Timeout:   10 * time.Millisecond,
	})

	_, err := client.Complete(context.Background(), ports.ModelRequest{Prompt: "test"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	f := domain.Classify(err)
	if f.Class != domain.ClassTransient {
		t.Errorf("Class = %q, want transient for timeout", f.Class)
	}
	if f.Code != "model_unreachable" {
		t.Errorf("Code = %q, want model_unreachable", f.Code)
	}
}

// 7. ScriptedModel behavior & contract tests
func TestScriptedModel(t *testing.T) {
	m := model.NewScripted()

	t.Run("queued response and request recording", func(t *testing.T) {
		m.WithResponse(ports.ModelResponse{Text: `{"verdict": "changed"}`})

		req := ports.ModelRequest{
			Purpose:       ports.PurposeEvaluate,
			Prompt:        "prompt 1",
			Deterministic: true,
		}
		resp, err := m.Complete(context.Background(), req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Text != `{"verdict": "changed"}` {
			t.Errorf("Text = %q", resp.Text)
		}
		if m.CallCount() != 1 {
			t.Errorf("CallCount = %d, want 1", m.CallCount())
		}
		if m.LastCall().Prompt != "prompt 1" {
			t.Errorf("LastCall Prompt = %q", m.LastCall().Prompt)
		}
	})

	t.Run("queued error", func(t *testing.T) {
		simErr := errors.New("simulated network failure")
		m.WithError(simErr)

		_, err := m.Complete(context.Background(), ports.ModelRequest{Prompt: "prompt 2"})
		if !errors.Is(err, simErr) {
			t.Errorf("got err %v, want %v", err, simErr)
		}
		if m.CallCount() != 2 {
			t.Errorf("CallCount = %d, want 2", m.CallCount())
		}
	})

	t.Run("context cancellation / timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := m.Complete(ctx, ports.ModelRequest{Prompt: "prompt 3"})
		if err == nil {
			t.Fatal("expected error on cancelled context, got nil")
		}
		f := domain.Classify(err)
		if f.Class != domain.ClassTransient {
			t.Errorf("Class = %q, want transient on cancelled context", f.Class)
		}
		if f.Code != "model_timeout" {
			t.Errorf("Code = %q, want model_timeout", f.Code)
		}
	})

	t.Run("dynamic handler", func(t *testing.T) {
		m.Reset()
		m.WithHandler(func(ctx context.Context, req ports.ModelRequest) (ports.ModelResponse, error) {
			return ports.ModelResponse{Text: "handled: " + req.Prompt}, nil
		})

		resp, err := m.Complete(context.Background(), ports.ModelRequest{Prompt: "dynamic"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Text != "handled: dynamic" {
			t.Errorf("Text = %q", resp.Text)
		}
	})
}
