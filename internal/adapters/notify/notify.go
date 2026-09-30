// Package notify is a driven adapter implementing ports.Notifier.
//
// The core decides whether to speak. This package decides only how. That split
// is why silence is trustworthy: there is no path here that suppresses a
// notification the core asked for, and no path that invents one it did not.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Writer delivers notifications to an io.Writer, which is the default because
// it is the one destination that cannot itself fail in an interesting way.
// Agentd running under systemd or in a terminal writes here and the operator's
// existing log plumbing does the rest.
type Writer struct {
	mu  sync.Mutex
	out io.Writer
}

var _ ports.Notifier = (*Writer)(nil)

// NewWriter builds a Writer. A nil destination means stderr.
func NewWriter(out io.Writer) *Writer {
	if out == nil {
		out = os.Stderr
	}
	return &Writer{out: out}
}

// Kinds reports which destinations this notifier serves.
func (w *Writer) Kinds() []domain.DestinationKind {
	return []domain.DestinationKind{domain.DestinationNotify, domain.DestinationNone}
}

// Deliver writes the notification.
func (w *Writer) Deliver(_ context.Context, n domain.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  [%s] %s\n", n.OccurredAt.Format(time.RFC3339), n.Severity, n.Subject)
	if n.TraceID != "" {
		fmt.Fprintf(&b, "    Trace ID: %s\n", n.TraceID)
	}
	if n.Body != "" {
		for _, line := range strings.Split(strings.TrimRight(n.Body, "\n"), "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	if n.NeedsDecision {
		// The one message that expects an answer says so unmistakably, and
		// says how to give it.
		fmt.Fprintf(&b, "    This is waiting for you. Approve or reject with:\n")
		fmt.Fprintf(&b, "        agentd approve %s --by <your name>\n", n.IncidentID)
		fmt.Fprintf(&b, "        agentd reject  %s --by <your name>\n", n.IncidentID)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := io.WriteString(w.out, b.String()); err != nil {
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "notify_write_failed",
			Summary: "the notification could not be written",
			Detail:  err.Error(),
		}
	}
	return nil
}

// Webhook posts notifications as JSON.
type Webhook struct {
	// URL is the endpoint. A check's Destination.Target overrides it, so one
	// notifier can serve checks that report to different places.
	URL string

	// SecretRef names a credential sent as a bearer token, if the endpoint
	// needs one.
	SecretRef domain.SecretRef

	// Secrets resolves SecretRef.
	Secrets ports.SecretResolver

	// Client overrides the HTTP client, for tests.
	Client *http.Client
}

var _ ports.Notifier = (*Webhook)(nil)

// Kinds reports which destinations this notifier serves.
func (w *Webhook) Kinds() []domain.DestinationKind {
	return []domain.DestinationKind{domain.DestinationNotify}
}

// payload is the JSON a webhook receives. It is a stable, documented shape
// rather than a dump of internal types, because anything an operator writes a
// handler against is an interface Agentd has to keep.
type payload struct {
	Check         string `json:"check"`
	Severity      string `json:"severity"`
	Subject       string `json:"subject"`
	Body          string `json:"body"`
	NeedsDecision bool   `json:"needs_decision"`
	Incident      string `json:"incident,omitempty"`
	OccurredAt    string `json:"occurred_at"`
	TraceID       string `json:"trace_id,omitempty"`
}

// Deliver posts the notification.
func (w *Webhook) Deliver(ctx context.Context, n domain.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}

	target := n.Destination.Target
	if target == "" {
		target = w.URL
	}
	if target == "" {
		return domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "notify_no_target",
			Summary: "this check is set to notify but has nowhere to notify",
		}
	}

	body, err := json.Marshal(payload{
		Check:         string(n.CheckID),
		Severity:      string(n.Severity),
		Subject:       n.Subject,
		Body:          n.Body,
		NeedsDecision: n.NeedsDecision,
		Incident:      string(n.IncidentID),
		OccurredAt:    n.OccurredAt.UTC().Format(time.RFC3339),
		TraceID:       n.TraceID,
	})
	if err != nil {
		return fmt.Errorf("notify: encoding payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "notify_bad_target",
			Summary: "this check's notification target is not a usable address",
			Detail:  err.Error(),
		}
	}
	req.Header.Set("Content-Type", "application/json")

	ref := n.Destination.Secret
	if ref == "" {
		ref = w.SecretRef
	}
	if ref != "" && w.Secrets != nil {
		bundle, err := w.Secrets.Resolve(ctx, []domain.SecretRef{ref})
		if err != nil {
			return err
		}
		if secret, ok := bundle.Get(ref); ok {
			req.Header.Set("Authorization", "Bearer "+secret.Reveal())
		}
	}

	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "notify_unreachable",
			Summary: "the notification endpoint could not be reached",
			Detail:  err.Error(),
		}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "notify_unauthorised",
			Summary: "the notification endpoint rejected Agentd's credentials",
		}
	default:
		return domain.Failure{
			Class:   domain.ClassTransient,
			Code:    fmt.Sprintf("notify_http_%d", resp.StatusCode),
			Summary: fmt.Sprintf("the notification endpoint answered with HTTP %d", resp.StatusCode),
		}
	}
}

// Router picks a notifier by destination kind.
//
// A check whose destination is DestinationNone is delivered to nobody, and
// that is a successful delivery rather than an error: choosing not to be told
// is a legitimate configuration, and the run is still recorded either way.
type Router struct {
	notifiers []ports.Notifier
}

var _ ports.Notifier = (*Router)(nil)

// NewRouter builds a Router.
func NewRouter(notifiers ...ports.Notifier) *Router {
	return &Router{notifiers: notifiers}
}

// Kinds reports every kind the routed notifiers serve.
func (r *Router) Kinds() []domain.DestinationKind {
	seen := map[domain.DestinationKind]bool{}
	var out []domain.DestinationKind
	for _, n := range r.notifiers {
		for _, k := range n.Kinds() {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// Deliver routes to the first notifier that serves the destination's kind.
func (r *Router) Deliver(ctx context.Context, n domain.Notification) error {
	kind := n.Destination.Kind
	if kind == "" {
		kind = domain.DestinationNone
	}
	if kind == domain.DestinationNone {
		return nil
	}

	for _, notifier := range r.notifiers {
		for _, k := range notifier.Kinds() {
			if k == kind {
				return notifier.Deliver(ctx, n)
			}
		}
	}
	return domain.Failure{
		Class:   domain.ClassFatal,
		Code:    "notify_no_route",
		Summary: fmt.Sprintf("nothing is configured to deliver a %q notification", kind),
	}
}
