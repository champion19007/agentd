package telemetry_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/telemetry"
)

func TestTelemetry_DefaultDisabled(t *testing.T) {
	_ = os.Unsetenv(telemetry.EnvVar)
	if telemetry.IsEnabled() {
		t.Fatal("expected telemetry to be disabled by default")
	}

	var buf bytes.Buffer
	c := telemetry.DefaultCollector()
	c.Record("run", "changed", 100*time.Millisecond, []byte("sensitive structure"))

	if len(c.Events()) != 0 {
		t.Fatalf("expected 0 events when disabled, got %d", len(c.Events()))
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty buffer when disabled, got %s", buf.String())
	}
}

func TestTelemetry_OptInAndPrivacyGuarantee(t *testing.T) {
	t.Setenv(telemetry.EnvVar, "1")
	if !telemetry.IsEnabled() {
		t.Fatal("expected telemetry to be enabled when AGENTD_TELEMETRY=1")
	}

	var buf bytes.Buffer
	c := telemetry.New(true, &buf)

	sensitiveInput := []byte("https://internal.corp.net/secret-token-12345?auth=bearer")
	c.Record("run", "quiet", 25*time.Millisecond, sensitiveInput)

	events := c.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Outcome != "quiet" || evt.Category != "run" {
		t.Errorf("unexpected event fields: %+v", evt)
	}
	if evt.DurationMs != 25 {
		t.Errorf("expected 25ms, got %d", evt.DurationMs)
	}

	// Verify that the raw sensitive string NEVER appears anywhere in the event or JSON stream
	rawJSON := buf.String()
	if strings.Contains(rawJSON, "internal.corp.net") ||
		strings.Contains(rawJSON, "secret-token") ||
		strings.Contains(rawJSON, "auth=bearer") {
		t.Fatalf("privacy violation! sensitive input found in telemetry stream: %s", rawJSON)
	}

	// Verify structural hash is present and valid sha256 (64 hex characters)
	if len(evt.StructuralHash) != 64 {
		t.Fatalf("expected 64-char hex SHA256, got %q", evt.StructuralHash)
	}

	// Test valid JSON parse
	var parsed telemetry.Event
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("failed to parse emitted telemetry JSON: %v", err)
	}
	if parsed.StructuralHash != evt.StructuralHash {
		t.Errorf("hash mismatch: %s vs %s", parsed.StructuralHash, evt.StructuralHash)
	}
}

func TestTelemetry_ConcurrentSafety(t *testing.T) {
	var buf bytes.Buffer
	c := telemetry.New(true, &buf)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c.Record("run", "changed", time.Duration(id)*time.Millisecond, []byte("data"))
		}(i)
	}
	wg.Wait()

	events := c.Events()
	if len(events) != 20 {
		t.Fatalf("expected 20 events recorded concurrently, got %d", len(events))
	}
}
