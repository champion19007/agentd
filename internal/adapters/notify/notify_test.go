package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/notify"
	"github.com/champion19007/agentd/internal/core/domain"
)

type recordNotifier struct {
	mu        sync.Mutex
	delivered []domain.Notification
	failNext  bool
}

func (r *recordNotifier) Kinds() []domain.DestinationKind {
	return []domain.DestinationKind{domain.DestinationNotify}
}

func (r *recordNotifier) Deliver(_ context.Context, n domain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext {
		r.failNext = false
		return errors.New("simulated delivery failure")
	}
	r.delivered = append(r.delivered, n)
	return nil
}

func TestDeduplication(t *testing.T) {
	rec := &recordNotifier{}
	dedup := notify.NewDeduplicator(rec, 10*time.Minute)

	baseTime := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	dedup.SetNow(func() time.Time { return baseTime })

	n1 := domain.Notification{
		CheckID:     "check-1",
		Destination: domain.Destination{Kind: domain.DestinationNotify},
		Severity:    domain.SeverityInfo,
		Subject:     "Product price changed",
		Body:        "Price updated from $50 to $45",
		OccurredAt:  baseTime,
	}

	// First delivery must succeed and pass through to downstream
	if err := dedup.Deliver(context.Background(), n1); err != nil {
		t.Fatalf("first Deliver: %v", err)
	}
	if len(rec.delivered) != 1 {
		t.Fatalf("rec.delivered = %d, want 1", len(rec.delivered))
	}

	// Duplicate delivery within window must be suppressed
	if err := dedup.Deliver(context.Background(), n1); err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if len(rec.delivered) != 1 {
		t.Errorf("rec.delivered = %d after duplicate, want 1 (suppressed)", len(rec.delivered))
	}

	// Different notification must NOT be suppressed
	n2 := n1
	n2.Subject = "Product out of stock"
	if err := dedup.Deliver(context.Background(), n2); err != nil {
		t.Fatalf("Deliver different: %v", err)
	}
	if len(rec.delivered) != 2 {
		t.Errorf("rec.delivered = %d, want 2", len(rec.delivered))
	}

	// Advance time past deduplication window: original notification can be delivered again
	dedup.SetNow(func() time.Time { return baseTime.Add(15 * time.Minute) })
	if err := dedup.Deliver(context.Background(), n1); err != nil {
		t.Fatalf("Deliver after window: %v", err)
	}
	if len(rec.delivered) != 3 {
		t.Errorf("rec.delivered = %d after window expiry, want 3", len(rec.delivered))
	}
}

func TestSpoolAndRetry(t *testing.T) {
	spool, err := notify.NewSpool("") // in-memory spool
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	rec := &recordNotifier{failNext: true}
	spooling := notify.NewSpoolingNotifier(rec, spool)

	n := domain.Notification{
		CheckID:     "check-pricing",
		Destination: domain.Destination{Kind: domain.DestinationNotify},
		Severity:    domain.SeverityAlert,
		Subject:     "Pricing check failed",
		Body:        "Upstream host unreachable",
		OccurredAt:  time.Now().UTC(),
	}

	// Delivery should fail and be buffered in spool
	err = spooling.Deliver(context.Background(), n)
	if err == nil {
		t.Fatal("expected delivery error, got nil")
	}
	if spool.Depth() != 1 {
		t.Fatalf("spool.Depth = %d, want 1", spool.Depth())
	}
	if len(rec.delivered) != 0 {
		t.Fatalf("rec.delivered = %d, want 0", len(rec.delivered))
	}

	// Now retry delivery through downstream (which now succeeds)
	delivered, failed, err := spool.RetryUndelivered(context.Background(), rec)
	if err != nil {
		t.Fatalf("RetryUndelivered: %v", err)
	}
	if delivered != 1 || failed != 0 {
		t.Errorf("delivered=%d, failed=%d, want 1, 0", delivered, failed)
	}
	if spool.Depth() != 0 {
		t.Errorf("spool.Depth = %d after retry, want 0", spool.Depth())
	}
	if len(rec.delivered) != 1 {
		t.Errorf("rec.delivered = %d, want 1", len(rec.delivered))
	}
}

func TestNotificationRendering(t *testing.T) {
	var buf bytes.Buffer
	w := notify.NewWriter(&buf)

	n := domain.Notification{
		CheckID:       "check-seats",
		Destination:   domain.Destination{Kind: domain.DestinationNotify},
		Severity:      domain.SeverityAlert,
		Subject:       "Approve a repair for Flight Seats?",
		Body:          "The booking table layout moved to section#seats.\nProposed selector: section#seats tr.available",
		NeedsDecision: true,
		IncidentID:    "inc-42",
		OccurredAt:    time.Date(2026, 9, 28, 14, 30, 0, 0, time.UTC),
	}

	if err := w.Deliver(context.Background(), n); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "[alert] Approve a repair for Flight Seats?") {
		t.Errorf("unexpected output header: %s", out)
	}
	if !strings.Contains(out, "agentd approve inc-42 --by <your name>") {
		t.Errorf("missing approval instruction: %s", out)
	}
	if !strings.Contains(out, "agentd reject  inc-42 --by <your name>") {
		t.Errorf("missing rejection instruction: %s", out)
	}
}

func TestWebhookNotifier(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	wh := &notify.Webhook{
		URL:    server.URL,
		Client: server.Client(),
	}

	n := domain.Notification{
		CheckID:     "check-api",
		Destination: domain.Destination{Kind: domain.DestinationNotify},
		Severity:    domain.SeverityInfo,
		Subject:     "API Latency normalized",
		Body:        "P99 latency is now 120ms",
		OccurredAt:  time.Now().UTC(),
	}

	if err := wh.Deliver(context.Background(), n); err != nil {
		t.Fatalf("Deliver to webhook: %v", err)
	}

	var payload struct {
		Check   string `json:"check"`
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("Unmarshal webhook payload: %v", err)
	}
	if payload.Check != "check-api" || payload.Subject != "API Latency normalized" {
		t.Errorf("payload = %+v", payload)
	}
}

func TestEventConstructors(t *testing.T) {
	now := time.Now().UTC()
	dest := domain.Destination{Kind: domain.DestinationNotify}

	// 1. Changed
	n := notify.Changed("c1", dest, "Pricing", "Changed from $10 to $12", now)
	if n.Severity != domain.SeverityInfo || !strings.Contains(n.Subject, "Pricing changed") {
		t.Errorf("Changed: %+v", n)
	}

	// 2. Degraded
	n = notify.Degraded("c1", dest, "Pricing", "Slow response", now)
	if n.Severity != domain.SeverityWarning {
		t.Errorf("Degraded: %+v", n)
	}

	// 3. Failed (Structural)
	fail := domain.Failure{Class: domain.ClassStructural, Summary: "table.pricing not found"}
	n = notify.Failed("c1", dest, "Pricing", fail, now)
	if n.Severity != domain.SeverityAlert || !strings.Contains(n.Body, "source structure appears to have changed") {
		t.Errorf("Failed structural: %+v", n)
	}

	// 4. RepairProposal
	n = notify.RepairProposal("c1", dest, "Pricing", "inc-1", "Found updated table", now)
	if !n.NeedsDecision || n.IncidentID != "inc-1" {
		t.Errorf("RepairProposal: %+v", n)
	}

	// 5. Unhealable
	n = notify.Unhealable("c1", dest, "Pricing", "inc-1", 3, now)
	if !strings.Contains(n.Subject, "Repair failed") || !strings.Contains(n.Body, "manually") {
		t.Errorf("Unhealable: %+v", n)
	}

	// 6. Staleness
	n = notify.Staleness("c1", dest, "Pricing", 5*time.Minute, 25*time.Minute, now)
	if !strings.Contains(n.Subject, "is stale") {
		t.Errorf("Staleness: %+v", n)
	}

	// 7. HealingStorm
	n = notify.HealingStorm(6, now)
	if !strings.Contains(n.Subject, "circuit breaker triggered") || !strings.Contains(n.Body, "6 checks") {
		t.Errorf("HealingStorm: %+v", n)
	}

	// 8. BudgetExhaustion
	n = notify.BudgetExhaustion("c1", dest, "Pricing", "daily token limit reached", now)
	if !strings.Contains(n.Subject, "budget limit reached") {
		t.Errorf("BudgetExhaustion: %+v", n)
	}

	// 9. StoreUnhealthy
	n = notify.StoreUnhealthy(errors.New("disk I/O error"), now)
	if !strings.Contains(n.Subject, "database unhealthy") {
		t.Errorf("StoreUnhealthy: %+v", n)
	}
}
