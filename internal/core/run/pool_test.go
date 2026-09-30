package run_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/run"
)

type stubOverloader struct {
	mu      sync.Mutex
	skipped []domain.Slot
}

func (s *stubOverloader) SkipOverloaded(_ context.Context, _ *domain.Check, slot domain.Slot, _ string) (*domain.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped = append(s.skipped, slot)
	return nil, nil
}

func TestWorkerPoolExecutionAndQueueDepth(t *testing.T) {
	var processed int64
	blocker := make(chan struct{})

	runner := run.JobRunnerFunc(func(ctx context.Context, c *domain.Check, slot domain.Slot) error {
		<-blocker
		atomic.AddInt64(&processed, 1)
		return nil
	})

	overload := &stubOverloader{}
	clk := &stubClock{now: base}

	// 2 workers, capacity 3
	pool := run.NewWorkerPool(2, 3, clk, runner, overload)

	if pool.Workers() != 2 {
		t.Errorf("Workers = %d, want 2", pool.Workers())
	}
	if pool.Capacity() != 3 {
		t.Errorf("Capacity = %d, want 3", pool.Capacity())
	}

	c := check(t)

	// Submit 2 jobs: these should immediately be picked up by the 2 workers
	for i := 0; i < 2; i++ {
		ok, err := pool.Submit(context.Background(), run.Job{Check: c, Slot: domain.Slot(i)})
		if err != nil || !ok {
			t.Fatalf("Submit(%d) = (%v, %v), want (true, nil)", i, ok, err)
		}
	}

	// Allow workers to pick up jobs
	time.Sleep(10 * time.Millisecond)
	if pool.ActiveWorkers() != 2 {
		t.Errorf("ActiveWorkers = %d, want 2", pool.ActiveWorkers())
	}

	// Submit 3 more jobs: these will sit in the queue (depth = 3)
	for i := 2; i < 5; i++ {
		ok, err := pool.Submit(context.Background(), run.Job{Check: c, Slot: domain.Slot(i)})
		if err != nil || !ok {
			t.Fatalf("Submit(%d) = (%v, %v), want (true, nil)", i, ok, err)
		}
	}

	if depth := pool.QueueDepth(); depth != 3 {
		t.Errorf("QueueDepth = %d, want 3", depth)
	}

	// Now the queue is FULL (2 active + 3 in queue). The 6th submission must overload!
	ok, err := pool.Submit(context.Background(), run.Job{Check: c, Slot: 99})
	if err != nil {
		t.Fatalf("Submit(99) err = %v", err)
	}
	if ok {
		t.Error("Submit(99) succeeded on a saturated queue; expected overload shedding")
	}

	overload.mu.Lock()
	if len(overload.skipped) != 1 || overload.skipped[0] != 99 {
		t.Errorf("expected slot 99 recorded as skipped_overload, got %v", overload.skipped)
	}
	overload.mu.Unlock()

	// Unblock workers
	close(blocker)

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := pool.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if atomic.LoadInt64(&processed) != 5 {
		t.Errorf("processed = %d, want 5", atomic.LoadInt64(&processed))
	}
	if pool.QueueDepth() != 0 {
		t.Errorf("QueueDepth after shutdown = %d, want 0", pool.QueueDepth())
	}
}

func TestWorkerPoolShutdownDeadline(t *testing.T) {
	hang := make(chan struct{})
	runner := run.JobRunnerFunc(func(ctx context.Context, c *domain.Check, slot domain.Slot) error {
		<-hang
		return nil
	})

	clk := &stubClock{now: base}
	pool := run.NewWorkerPool(1, 1, clk, runner, nil)

	_, _ = pool.Submit(context.Background(), run.Job{Check: check(t), Slot: 1})

	// Immediate deadline
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := pool.Shutdown(ctx)
	if err == nil {
		t.Error("expected deadline error on shutdown when workers hang")
	}

	close(hang)
}

func TestWorkerPool_DefaultsClosedAndDoubleShutdown(t *testing.T) {
	runner := run.JobRunnerFunc(func(ctx context.Context, c *domain.Check, slot domain.Slot) error {
		return nil
	})
	clk := &stubClock{now: base}

	// 1. Defaults for 0 workers and 0 capacity
	pDefault := run.NewWorkerPool(0, 0, clk, runner, nil)
	if pDefault.Workers() != 4 {
		t.Errorf("expected 4 default workers, got %d", pDefault.Workers())
	}
	if pDefault.Capacity() != 100 {
		t.Errorf("expected 100 default capacity, got %d", pDefault.Capacity())
	}

	// 2. Double shutdown
	if err := pDefault.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown failed: %v", err)
	}
	if err := pDefault.Shutdown(context.Background()); err != nil {
		t.Errorf("double shutdown should return nil, got: %v", err)
	}

	// 3. Submit after closed
	ok, err := pDefault.Submit(context.Background(), run.Job{Check: check(t), Slot: 1})
	if ok || err != run.ErrPoolClosed {
		t.Errorf("expected (false, ErrPoolClosed), got (%v, %v)", ok, err)
	}
}
