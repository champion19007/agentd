package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// QueuedNotification represents a notification that failed initial delivery
// and was persisted to the spool so a crash does not lose it.
type QueuedNotification struct {
	ID           string              `json:"id"`
	Notification domain.Notification `json:"notification"`
	Attempts     int                 `json:"attempts"`
	LastError    string              `json:"last_error,omitempty"`
	QueuedAt     time.Time           `json:"queued_at"`
}

// Spool manages persisted undelivered notifications.
// If Path is non-empty, the spool writes to and reads from disk.
type Spool struct {
	mu    sync.Mutex
	path  string
	items []QueuedNotification
}

// NewSpool creates or loads a Spool. If path is non-empty and exists on disk,
// pending items are loaded.
func NewSpool(path string) (*Spool, error) {
	s := &Spool{
		path:  path,
		items: make([]QueuedNotification, 0),
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			var loaded []QueuedNotification
			if err := json.Unmarshal(data, &loaded); err == nil {
				s.items = loaded
			}
		}
	}
	return s, nil
}

// Enqueue stores an undelivered notification in the spool.
func (s *Spool) Enqueue(n domain.Notification, deliveryErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	errMsg := ""
	if deliveryErr != nil {
		errMsg = deliveryErr.Error()
	}

	item := QueuedNotification{
		ID:           fmt.Sprintf("notif_%s_%d", n.ContentHash()[:12], time.Now().UTC().UnixNano()),
		Notification: n,
		Attempts:     1,
		LastError:    errMsg,
		QueuedAt:     time.Now().UTC(),
	}
	s.items = append(s.items, item)
	return s.saveLocked()
}

// Pending returns a copy of all undelivered notifications.
func (s *Spool) Pending() []QueuedNotification {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]QueuedNotification, len(s.items))
	copy(out, s.items)
	return out
}

// Depth reports the number of pending notifications waiting in the spool.
func (s *Spool) Depth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// RetryUndelivered attempts to re-deliver all pending notifications via target.
// Successfully delivered notifications are removed from the spool.
func (s *Spool) RetryUndelivered(ctx context.Context, target ports.Notifier) (delivered int, failed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return 0, 0, nil
	}

	remaining := make([]QueuedNotification, 0, len(s.items))
	for _, item := range s.items {
		if ctx.Err() != nil {
			remaining = append(remaining, item)
			continue
		}
		delivErr := target.Deliver(ctx, item.Notification)
		if delivErr == nil {
			delivered++
		} else {
			failed++
			item.Attempts++
			item.LastError = delivErr.Error()
			remaining = append(remaining, item)
		}
	}

	s.items = remaining
	_ = s.saveLocked()
	return delivered, failed, nil
}

func (s *Spool) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SpoolingNotifier wraps a Notifier and persists notifications that fail delivery.
type SpoolingNotifier struct {
	wrapped ports.Notifier
	spool   *Spool
}

var _ ports.Notifier = (*SpoolingNotifier)(nil)

// NewSpoolingNotifier creates a Notifier that automatically buffers failed deliveries.
func NewSpoolingNotifier(downstream ports.Notifier, spool *Spool) *SpoolingNotifier {
	return &SpoolingNotifier{
		wrapped: downstream,
		spool:   spool,
	}
}

// Kinds returns destination kinds supported by downstream.
func (s *SpoolingNotifier) Kinds() []domain.DestinationKind {
	if s.wrapped == nil {
		return nil
	}
	return s.wrapped.Kinds()
}

// Deliver sends to downstream; if delivery fails, the notification is spooled.
func (s *SpoolingNotifier) Deliver(ctx context.Context, n domain.Notification) error {
	if s.wrapped == nil {
		return nil
	}
	err := s.wrapped.Deliver(ctx, n)
	if err != nil {
		if s.spool != nil {
			_ = s.spool.Enqueue(n, err)
		}
		return err
	}
	return nil
}

// Spool returns the underlying Spool instance.
func (s *SpoolingNotifier) Spool() *Spool {
	return s.spool
}
