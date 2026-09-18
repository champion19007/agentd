package ports

import (
	"context"
	"time"
)

// Clock is the core's only source of time. The core never calls time.Now and
// never sleeps; it asks the Clock what time it is and asks to be woken.
type Clock interface {
	// Now returns the current instant. Always UTC.
	Now() time.Time

	// Sleep blocks until d has elapsed or ctx is done, whichever comes first.
	// It returns ctx.Err() if the context ended first, and nil otherwise.
	//
	// The core calls this rather than time.Sleep so that tests can drive the
	// scheduler through weeks of simulated time in microseconds.
	Sleep(ctx context.Context, d time.Duration) error
}

// Random is the core's only source of randomness, used for schedule jitter so
// that many checks due at the same instant do not stampede a source.
type Random interface {
	// Float64 returns a pseudo-random number in [0.0, 1.0).
	Float64() float64
}
