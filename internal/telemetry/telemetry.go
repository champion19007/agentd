package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// EnvVar is the environment variable that must be set to "1" to enable telemetry.
// Agentd strictly defaults to NO telemetry.
const EnvVar = "AGENTD_TELEMETRY"

// IsEnabled returns true if telemetry has been explicitly opted into by the operator.
func IsEnabled() bool {
	return os.Getenv(EnvVar) == "1"
}

// Event represents an anonymized, privacy-preserving telemetry record.
// Invariants enforced by architecture:
// - Strictly opt-in only.
// - Never contains URLs, domain names, payload contents, secrets, or identifiers.
// - Only captures structural SHA-256 hashes, high-level outcome states, and coarse durations.
type Event struct {
	SchemaVersion  int    `json:"schema_version"`
	Timestamp      string `json:"timestamp"`
	Category       string `json:"category"`                  // e.g. "run", "repair", "status"
	Outcome        string `json:"outcome"`                   // e.g. "changed", "quiet", "failed", "healed"
	DurationMs     int64  `json:"duration_ms,omitempty"`     // coarse duration
	StructuralHash string `json:"structural_hash,omitempty"` // 64-character hex SHA-256 of schema/fingerprint structure only
}

// Collector collects and exports telemetry events when explicitly enabled.
type Collector struct {
	mu      sync.Mutex
	enabled bool
	writer  io.Writer
	events  []Event
}

// New creates a new Collector. If enabled is false (the default), all operations are no-ops.
func New(enabled bool, writer io.Writer) *Collector {
	return &Collector{
		enabled: enabled,
		writer:  writer,
	}
}

// DefaultCollector creates a Collector respecting the AGENTD_TELEMETRY environment variable.
func DefaultCollector() *Collector {
	return New(IsEnabled(), os.Stderr)
}

// Record emits an event if telemetry is enabled. It enforces structural-only privacy guarantees.
func (c *Collector) Record(category, outcome string, duration time.Duration, structuralInput []byte) {
	if c == nil || !c.enabled {
		return
	}

	var hashStr string
	if len(structuralInput) > 0 {
		h := sha256.Sum256(structuralInput)
		hashStr = hex.EncodeToString(h[:])
	}

	evt := Event{
		SchemaVersion:  1,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Category:       category,
		Outcome:        outcome,
		DurationMs:     duration.Milliseconds(),
		StructuralHash: hashStr,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.events = append(c.events, evt)
	if c.writer != nil {
		_ = json.NewEncoder(c.writer).Encode(evt)
	}
}

// Events returns a copy of captured events (useful for tests and inspection).
func (c *Collector) Events() []Event {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := make([]Event, len(c.events))
	copy(copied, c.events)
	return copied
}
