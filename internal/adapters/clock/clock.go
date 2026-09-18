// Package clock is a driven adapter implementing ports.Clock and ports.Random
// against the operating system. It is the only place in the binary, outside
// tests, that is allowed to read the wall clock or sleep.
package clock

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/champion19007/agentd/internal/ports"
)

// System is the real clock.
type System struct{}

var _ ports.Clock = System{}

// Now returns the current UTC instant.
func (System) Now() time.Time { return time.Now().UTC() }

// Sleep waits for d, or until ctx is done.
func (System) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Rand is the real source of randomness.
type Rand struct{}

var _ ports.Random = Rand{}

// Float64 returns a pseudo-random number in [0.0, 1.0).
func (Rand) Float64() float64 { return rand.Float64() }
