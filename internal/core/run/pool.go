package run

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// ErrPoolClosed is returned when submitting work to a stopped worker pool.
var ErrPoolClosed = errors.New("worker pool is closed")

// Job represents a single check scheduled to run in a slot.
type Job struct {
	Check    *domain.Check
	Slot     domain.Slot
	Priority int
	Ctx      context.Context
}

// JobRunner executes a single check in a slot.
type JobRunner interface {
	Run(ctx context.Context, c *domain.Check, slot domain.Slot) error
}

// JobRunnerFunc adapts a plain function to JobRunner.
type JobRunnerFunc func(ctx context.Context, c *domain.Check, slot domain.Slot) error

// Run executes the function.
func (f JobRunnerFunc) Run(ctx context.Context, c *domain.Check, slot domain.Slot) error {
	return f(ctx, c, slot)
}

// OverloadHandler is called when a job cannot be enqueued due to capacity.
type OverloadHandler interface {
	SkipOverloaded(ctx context.Context, c *domain.Check, slot domain.Slot, reason string) (*domain.Run, error)
}

// WorkerPool manages a bounded set of worker goroutines executing checks from
// a bounded dispatch queue.
type WorkerPool struct {
	workers       int
	capacity      int
	clock         ports.Clock
	runner        JobRunner
	overload      OverloadHandler
	queue         chan Job
	activeWorkers int64
	wg            sync.WaitGroup
	mu            sync.Mutex
	closed        bool
	metrics       ports.Metrics
}

// WithMetrics attaches a metrics collector to the worker pool for queue depth tracking.
func (p *WorkerPool) WithMetrics(m ports.Metrics) *WorkerPool {
	p.metrics = m
	return p
}

// NewWorkerPool constructs and starts a bounded worker pool.
func NewWorkerPool(workers, queueCapacity int, clk ports.Clock, runner JobRunner, overload OverloadHandler) *WorkerPool {
	if workers <= 0 {
		workers = 4
	}
	if queueCapacity <= 0 {
		queueCapacity = 100
	}
	p := &WorkerPool{
		workers:  workers,
		capacity: queueCapacity,
		clock:    clk,
		runner:   runner,
		overload: overload,
		queue:    make(chan Job, queueCapacity),
	}

	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *WorkerPool) worker() {
	defer p.wg.Done()
	for job := range p.queue {
		if p.metrics != nil {
			p.metrics.SetQueueDepth(len(p.queue))
		}
		atomic.AddInt64(&p.activeWorkers, 1)
		ctx := job.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		_ = p.runner.Run(ctx, job.Check, job.Slot)
		atomic.AddInt64(&p.activeWorkers, -1)
	}
}

// Submit tries to enqueue a job. If the queue is full, it records the check as
// skipped_overload through the overload handler rather than silently dropping work.
// Returns (true, nil) if enqueued, or (false, nil) if shed due to overload.
func (p *WorkerPool) Submit(ctx context.Context, job Job) (bool, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false, ErrPoolClosed
	}

	select {
	case p.queue <- job:
		depth := len(p.queue)
		p.mu.Unlock()
		if p.metrics != nil {
			p.metrics.SetQueueDepth(depth)
		}
		return true, nil
	default:
		p.mu.Unlock()
		// Saturated! Record skipped_overload rather than dropping work silently.
		if p.overload != nil {
			_, _ = p.overload.SkipOverloaded(ctx, job.Check, job.Slot, "worker pool dispatch queue capacity exceeded")
		}
		return false, nil
	}
}

// QueueDepth reports the current number of jobs waiting in the queue.
func (p *WorkerPool) QueueDepth() int {
	return len(p.queue)
}

// Capacity returns the maximum queue capacity.
func (p *WorkerPool) Capacity() int {
	return p.capacity
}

// ActiveWorkers reports how many worker goroutines are currently executing a job.
func (p *WorkerPool) ActiveWorkers() int {
	return int(atomic.LoadInt64(&p.activeWorkers))
}

// Workers returns the number of worker goroutines.
func (p *WorkerPool) Workers() int {
	return p.workers
}

// Shutdown gracefully drains in-flight runs and queued jobs, waiting until all
// workers have exited or until ctx is done.
func (p *WorkerPool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.queue)
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
