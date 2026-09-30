package notify

import (
	"context"
	"sync"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Deduplicator wraps a Notifier to provide idempotent deduplication based
// on the notification's deterministic content hash.
//
// A crash must not duplicate notifications, and identical alerts for the same
// condition within the deduplication window are suppressed.
type Deduplicator struct {
	mu      sync.Mutex
	wrapped ports.Notifier
	seen    map[string]time.Time
	window  time.Duration
	now     func() time.Time
}

var _ ports.Notifier = (*Deduplicator)(nil)

// NewDeduplicator builds a Deduplicator wrapping downstream with the given
// deduplication window. A window of 0 means deduplication is retained indefinitely
// for the lifetime of this process.
func NewDeduplicator(downstream ports.Notifier, window time.Duration) *Deduplicator {
	return &Deduplicator{
		wrapped: downstream,
		seen:    make(map[string]time.Time),
		window:  window,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetNow overrides the clock used for window evaluation (primarily for testing).
func (d *Deduplicator) SetNow(now func() time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = now
}

// Kinds delegates destination kinds to the wrapped notifier.
func (d *Deduplicator) Kinds() []domain.DestinationKind {
	if d.wrapped == nil {
		return nil
	}
	return d.wrapped.Kinds()
}

// Deliver checks the notification's content hash against recent deliveries.
// If identical content was already successfully delivered within the deduplication
// window, Deliver returns nil without contacting downstream.
func (d *Deduplicator) Deliver(ctx context.Context, n domain.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}

	hash := n.ContentHash()
	now := d.now()

	d.mu.Lock()
	// Prune expired entries if window is non-zero
	if d.window > 0 {
		for h, t := range d.seen {
			if now.Sub(t) > d.window {
				delete(d.seen, h)
			}
		}
	}

	if last, ok := d.seen[hash]; ok {
		if d.window <= 0 || now.Sub(last) <= d.window {
			d.mu.Unlock()
			// Suppress duplicate notification idempotently
			return nil
		}
	}
	// Optimistically record the hash to prevent concurrent duplicate alerts
	d.seen[hash] = now
	d.mu.Unlock()

	if d.wrapped != nil {
		if err := d.wrapped.Deliver(ctx, n); err != nil {
			// Revert claim on failure so subsequent attempts may retry
			d.mu.Lock()
			delete(d.seen, hash)
			d.mu.Unlock()
			return err
		}
	}
	return nil
}

// HasSeen reports whether a notification with the given content hash has been recorded.
func (d *Deduplicator) HasSeen(hash string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.seen[hash]
	return ok
}

// Count returns the number of active entries currently held in the deduplication cache.
func (d *Deduplicator) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// Clear resets the deduplication cache.
func (d *Deduplicator) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = make(map[string]time.Time)
}
