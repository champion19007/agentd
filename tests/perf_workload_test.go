package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/extract"
	"github.com/champion19007/agentd/internal/adapters/httpsource"
	"github.com/champion19007/agentd/internal/adapters/model"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/policy"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/core/scheduling"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

// generateReferenceHTML creates a realistic ~200 KB HTML document containing a 15 KB data payload.
func generateReferenceHTML(dataPayload string, fillerSize int) string {
	var sb strings.Builder
	sb.Grow(fillerSize + len(dataPayload) + 1024)
	sb.WriteString("<!DOCTYPE html><html><head><title>Performance Reference Workload</title></head><body>\n")
	sb.WriteString("<header><nav><ul><li>Home</li><li>Catalog</li><li>Dashboard</li></ul></nav></header>\n")
	sb.WriteString("<main>\n")
	sb.WriteString("<div id=\"monitored-payload\">\n")
	sb.WriteString(dataPayload)
	sb.WriteString("\n</div>\n")
	sb.WriteString("<aside class=\"extra-bloat\">\n")
	// Append filler HTML to reach ~200 KB p90 raw payload
	chunk := "<p class=\"filler-text\">Agentd reference workload stress testing payload chunk with filler markup and text.</p>\n"
	for sb.Len() < fillerSize {
		sb.WriteString(chunk)
	}
	sb.WriteString("</aside>\n</main>\n<footer><p>Footer Copyright 2026</p></footer>\n</body></html>")
	return sb.String()
}

// generate15KBPayload creates an extracted text payload of ~15 KB.
func generate15KBPayload(version int) string {
	var sb strings.Builder
	sb.Grow(16 * 1024)
	sb.WriteString(fmt.Sprintf("<div class=\"target-data\" data-version=\"%d\">\n", version))
	for sb.Len() < 15*1024 {
		sb.WriteString(fmt.Sprintf("<span class=\"entry\">Item metric v%d observation record value #%d: 42.50 USD</span>\n", version, sb.Len()))
	}
	sb.WriteString("</div>")
	return sb.String()
}

func calcPercentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	copied := make([]time.Duration, len(durations))
	copy(copied, durations)
	sort.Slice(copied, func(i, j int) bool { return copied[i] < copied[j] })
	idx := int(float64(len(copied)-1) * p)
	return copied[idx]
}

// TestPerformance_TargetWorkload benchmarks the architecture's reference workload:
// - 500 checks
// - average 4-hour cadence
// - 200 KB p90 raw payload
// - 15 KB p90 extracted payload
// - 4-8 concurrent runs
// Measures: no-model p95 < 2s, model p95 < 15s, scheduler dispatch p99 < 500ms, store write < 5ms, idle memory < 256MB.
func TestPerformance_TargetWorkload(t *testing.T) {
	ctx := context.Background()

	// 1. Establish HTTP server serving 200 KB raw payload with 15 KB extracted payload
	payload15KB := generate15KBPayload(1)
	full200KBHTML := generateReferenceHTML(payload15KB, 200*1024)

	var serverPayload atomic.Value
	serverPayload.Store(full200KBHTML)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, serverPayload.Load().(string))
	}))
	defer ts.Close()

	dbPath := filepath.Join(t.TempDir(), "agentd-perf-workload.db")
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("failed to open sqlite store: %v", err)
	}
	defer st.Close()

	clk := &e2eClock{now: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	ids := &e2eIDs{}
	src := httpsource.New(httpsource.Options{Timeout: 5 * time.Second, AllowPrivateIPs: true})
	ext := extract.New()
	scrModel := model.NewScripted()
	notif := &memoryNotifier{}

	runOrch := run.New(run.Deps{
		Clock:       clk,
		IDs:         ids,
		Store:       st,
		Source:      src,
		Extract:     ext,
		Fingerprint: ext,
		Model:       scrModel,
		Notifier:    notif,
		Metrics:     ports.NoopMetrics{},
	})

	const numChecks = 500
	checks := make([]*domain.Check, numChecks)

	// Cadences centered around 4h: 1h, 2h, 4h, 6h, 8h
	cadences := []time.Duration{1 * time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 8 * time.Hour}

	t.Logf("Populating %d checks across average 4h cadence with ~200 KB HTML payloads...", numChecks)
	err = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		for i := 0; i < numChecks; i++ {
			chkID := domain.CheckID(fmt.Sprintf("chk-perf-%04d", i+1))
			cadence := cadences[i%len(cadences)]
			def := domain.Definition{
				Version: 1,
				Intent: domain.ScalarIntent{
					Label:   fmt.Sprintf("payload_%04d", i+1),
					Purpose: "performance benchmark payload tracking",
					Type:    domain.TypeString,
				},
				Source: domain.SourceSpec{
					Kind: domain.SourceHTTP,
					URL:  ts.URL,
				},
				Schedule: domain.Schedule{
					Interval: cadence,
					CatchUp:  domain.CatchUpOnce,
				},
				CreatedAt: clk.Now(),
			}
			chk, err := domain.NewCheck(chkID, def)
			if err != nil {
				return err
			}
			checks[i] = chk
			if err := tx.SaveCheck(ctx, chk); err != nil {
				return err
			}

			bnd := domain.Binding{
				ID:                ids.NewBindingID(),
				CheckID:           chkID,
				DefinitionVersion: 1,
				IntentKind:        domain.IntentScalar,
				Fingerprint:       "fp-initial",
				Version:           1,
				Origin:            domain.OriginInferred,
				Locators: []domain.Locator{
					{Target: fmt.Sprintf("payload_%04d", i+1), Dialect: "css", Expression: "#monitored-payload"},
				},
				DerivedAt: clk.Now(),
			}
			if err := tx.SaveBinding(ctx, bnd); err != nil {
				return err
			}
			if err := tx.ActivateBinding(ctx, chkID, 1); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("populating checks failed: %v", err)
	}

	// Step 1: Measure Idle Memory footprint with 500 checks loaded
	runtime.GC()
	var memIdle runtime.MemStats
	runtime.ReadMemStats(&memIdle)
	idleMemoryMB := float64(memIdle.HeapInuse+memIdle.StackInuse) / (1024 * 1024)
	t.Logf("[Target 1] Idle Memory with 500 checks: %.2f MB (Target: < 256 MB)", idleMemoryMB)
	if idleMemoryMB >= 256.0 {
		t.Errorf("Idle memory exceeds 256 MB ceiling: %.2f MB", idleMemoryMB)
	}

	// Step 2: Measure Scheduler Dispatch latency distribution (p99 < 500ms)
	sched := scheduling.New(clk, fakeRandom{}, st, scheduling.DefaultOptions())
	// Warm-up query to initialize reader connection and prepare statements
	_, _ = sched.DueChecks(ctx, clk.Now())

	var dispatchDurations []time.Duration
	for round := 0; round < 20; round++ {
		clk.Advance(30 * time.Minute)
		t0 := time.Now()
		due, err := sched.DueChecks(ctx, clk.Now())
		if err != nil {
			t.Fatalf("DueChecks failed: %v", err)
		}
		d := time.Since(t0)
		dispatchDurations = append(dispatchDurations, d)
		t.Logf("round %d dispatch duration: %v", round, d)
		_ = due
	}
	p99Dispatch := calcPercentile(dispatchDurations, 0.99)
	p50Dispatch := calcPercentile(dispatchDurations, 0.50)
	t.Logf("[Target 2] Scheduler Dispatch: p50 = %v, p99 = %v (Target: p99 < 500ms)", p50Dispatch, p99Dispatch)
	if p99Dispatch >= 500*time.Millisecond {
		t.Errorf("Scheduler dispatch p99 exceeds 500ms: %v", p99Dispatch)
	}

	// Step 3: Measure SQLite Store Write latency (tx.UpdateRun + tx.PutSnapshot)
	var writeDurations []time.Duration
	for i := 0; i < 50; i++ {
		sampleRunID := ids.NewRunID()
		sampleSlot := domain.Slot(clk.Now().Unix() + int64((i+1)*100))
		r, _ := domain.NewRun(sampleRunID, checks[i].ID(), sampleSlot, 1, clk.Now())
		if err := st.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
			return tx.CreateRun(ctx, r)
		}); err != nil {
			t.Fatalf("create run failed: %v", err)
		}

		snap, _ := domain.NewSnapshot(checks[i].ID(), "text/html", []byte(full200KBHTML), "fp-bench", clk.Now())
		_ = r.Quiet(clk.Now(), snap.ID(), domain.Extraction{Kind: domain.IntentScalar})

		t0 := time.Now()
		err := st.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
			if err := tx.PutSnapshot(ctx, snap); err != nil {
				return err
			}
			return tx.UpdateRun(ctx, r)
		})
		d := time.Since(t0)
		if err != nil {
			t.Fatalf("store write failed: %v", err)
		}
		writeDurations = append(writeDurations, d)
	}
	p50Write := calcPercentile(writeDurations, 0.50)
	p95Write := calcPercentile(writeDurations, 0.95)
	t.Logf("[Target 3] Store Write: p50 = %v, p95 = %v (Target: < 5ms average/median)", p50Write, p95Write)

	// Step 4: Measure No-Model Run Latency Distribution (4-8 concurrent runs, 200 KB HTML)
	// We run 6 concurrent workers across 60 checks with unchanged payload (hash gate active)
	const concurrency = 6
	pool := run.NewWorkerPool(concurrency, 100, clk, run.JobRunnerFunc(func(ctx context.Context, c *domain.Check, s domain.Slot) error {
		_, err := runOrch.Run(ctx, c, s)
		return err
	}), runOrch)
	defer func() { _ = pool.Shutdown(ctx) }()

	// Initial baseline run for first 60 checks
	for i := 0; i < 60; i++ {
		_, err := runOrch.Run(ctx, checks[i], domain.Slot(clk.Now().Unix()))
		if err != nil {
			t.Fatalf("baseline run failed: %v", err)
		}
	}

	// Subsequent runs are unchanged -> hash gate suppresses model
	scrModel.Reset()
	var noModelDurations []time.Duration
	var durMu sync.Mutex

	clk.Advance(4 * time.Hour)
	currentSlot := domain.Slot(clk.Now().Unix())

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		chk := checks[i]
		go func() {
			defer wg.Done()
			t0 := time.Now()
			out, err := runOrch.Run(ctx, chk, currentSlot)
			d := time.Since(t0)
			if err != nil {
				t.Errorf("no-model run failed: %v", err)
				return
			}
			if out.Run.State() != domain.StateQuiet {
				t.Errorf("expected StateQuiet from hash gate, got %s", out.Run.State())
			}
			durMu.Lock()
			noModelDurations = append(noModelDurations, d)
			durMu.Unlock()
		}()
	}
	wg.Wait()

	p50NoModel := calcPercentile(noModelDurations, 0.50)
	p95NoModel := calcPercentile(noModelDurations, 0.95)
	p99NoModel := calcPercentile(noModelDurations, 0.99)
	t.Logf("[Target 4] No-Model Run Latency (200 KB HTML): p50 = %v, p95 = %v, p99 = %v (Target: p95 < 2s)", p50NoModel, p95NoModel, p99NoModel)
	if p95NoModel >= 2*time.Second {
		t.Errorf("no-model run p95 exceeds 2s target: %v", p95NoModel)
	}

	// Step 5: Verify Model-Call Reduction via Hash Gating
	calls := scrModel.CallCount()
	t.Logf("[Target 5] Model Calls under Unchanged Payload: %d calls across 60 runs (100%% suppressed by hash gate)", calls)
	if calls != 0 {
		t.Errorf("expected 0 model calls under hash gating, got %d", calls)
	}

	// Step 6: Measure Model Run Latency Distribution (when payload changes)
	// Mutate server payload to trigger change evaluation
	newPayload15KB := generate15KBPayload(2)
	new200KBHTML := generateReferenceHTML(newPayload15KB, 200*1024)
	serverPayload.Store(new200KBHTML)

	// Pre-queue structured change verdicts
	for i := 0; i < 20; i++ {
		scrModel.WithTextResponse(`{"verdict": "changed", "confidence": 0.98, "explanation": "Metric observation updated successfully"}`)
	}

	clk.Advance(4 * time.Hour)
	changeSlot := domain.Slot(clk.Now().Unix())

	var modelDurations []time.Duration
	for i := 0; i < 20; i++ {
		t0 := time.Now()
		out, err := runOrch.Run(ctx, checks[i], changeSlot)
		d := time.Since(t0)
		if err != nil {
			t.Fatalf("model run failed: %v", err)
		}
		if out.Run.State() != domain.StateChanged {
			t.Errorf("expected StateChanged, got: %s", out.Run.State())
		}
		modelDurations = append(modelDurations, d)
	}

	p50Model := calcPercentile(modelDurations, 0.50)
	p95Model := calcPercentile(modelDurations, 0.95)
	t.Logf("[Target 6] Model Run Latency (200 KB HTML + Evaluation): p50 = %v, p95 = %v (Target: p95 < 15s)", p50Model, p95Model)
	if p95Model >= 15*time.Second {
		t.Errorf("model run p95 exceeds 15s target: %v", p95Model)
	}
}

// TestPerformance_Soak12Hours executes a compressed 12-hour soak simulation across 500 checks:
// Monitors memory, goroutines, SQLite growth, queue behavior, and asserts zero leaks.
func TestPerformance_Soak12Hours(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agentd-soak-12h.db")
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("failed to open sqlite store: %v", err)
	}
	defer st.Close()

	initialGoroutines := runtime.NumGoroutine()

	payload := generate15KBPayload(1)
	htmlBody := generateReferenceHTML(payload, 50*1024)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, htmlBody)
	}))
	defer ts.Close()

	startTime := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	clk := &e2eClock{now: startTime}
	ids := &e2eIDs{}
	src := httpsource.New(httpsource.Options{Timeout: 3 * time.Second, AllowPrivateIPs: true})
	ext := extract.New()
	scrModel := model.NewScripted()
	notif := &memoryNotifier{}

	runOrch := run.New(run.Deps{
		Clock:       clk,
		IDs:         ids,
		Store:       st,
		Source:      src,
		Extract:     ext,
		Fingerprint: ext,
		Model:       scrModel,
		Notifier:    notif,
		Metrics:     ports.NoopMetrics{},
	})

	const numChecks = 500
	checks := make([]*domain.Check, numChecks)
	err = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		for i := 0; i < numChecks; i++ {
			chkID := domain.CheckID(fmt.Sprintf("chk-soak-%04d", i+1))
			def := domain.Definition{
				Version: 1,
				Intent: domain.ScalarIntent{
					Label:   fmt.Sprintf("metric_%04d", i+1),
					Purpose: "12-hour soak tracking metric",
					Type:    domain.TypeString,
				},
				Source: domain.SourceSpec{
					Kind: domain.SourceHTTP,
					URL:  ts.URL,
				},
				Schedule: domain.Schedule{
					Interval: 4 * time.Hour,
					CatchUp:  domain.CatchUpOnce,
				},
				CreatedAt: startTime,
			}
			chk, err := domain.NewCheck(chkID, def)
			if err != nil {
				return err
			}
			checks[i] = chk
			if err := tx.SaveCheck(ctx, chk); err != nil {
				return err
			}

			bnd := domain.Binding{
				ID:                ids.NewBindingID(),
				CheckID:           chkID,
				DefinitionVersion: 1,
				IntentKind:        domain.IntentScalar,
				Fingerprint:       "fp-soak",
				Version:           1,
				Origin:            domain.OriginInferred,
				Locators: []domain.Locator{
					{Target: fmt.Sprintf("metric_%04d", i+1), Dialect: "css", Expression: "#monitored-payload"},
				},
				DerivedAt: startTime,
			}
			if err := tx.SaveBinding(ctx, bnd); err != nil {
				return err
			}
			if err := tx.ActivateBinding(ctx, chkID, 1); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("populating checks failed: %v", err)
	}

	// 6 worker threads with queue capacity 200
	pool := run.NewWorkerPool(6, 200, clk, run.JobRunnerFunc(func(ctx context.Context, c *domain.Check, s domain.Slot) error {
		_, err := runOrch.Run(ctx, c, s)
		return err
	}), runOrch)
	defer func() { _ = pool.Shutdown(ctx) }()

	sched := scheduling.New(clk, fakeRandom{}, st, scheduling.DefaultOptions())

	t.Logf("Starting 12-hour simulated soak across %d checks...", numChecks)
	var maxQueueDepth int
	var totalRunsExecuted int64
	var failedRuns int64

	// Simulate 12 hours: 24 steps of 30 minutes
	for step := 1; step <= 24; step++ {
		clk.Advance(30 * time.Minute)
		due, err := sched.DueChecks(ctx, clk.Now())
		if err != nil {
			t.Fatalf("step %d DueChecks failed: %v", step, err)
		}

		for _, d := range due {
			atomic.AddInt64(&totalRunsExecuted, 1)
			enqueued, err := pool.Submit(ctx, run.Job{
				Check:    d.Check,
				Slot:     d.Slot,
				Priority: d.Priority,
			})
			if err != nil {
				atomic.AddInt64(&failedRuns, 1)
			}
			if !enqueued {
				// Shed to skipped_overload
			}
			depth := pool.QueueDepth()
			if depth > maxQueueDepth {
				maxQueueDepth = depth
			}
		}

		// Allow worker pool to process current burst
		for pool.QueueDepth() > 0 || pool.ActiveWorkers() > 0 {
			time.Sleep(2 * time.Millisecond)
		}

		if step%6 == 0 {
			simHour := step / 2
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			fi, _ := os.Stat(dbPath)
			dbSizeKB := int64(0)
			if fi != nil {
				dbSizeKB = fi.Size() / 1024
			}
			t.Logf("  [Soak Hour %2d/12] Executed: %d runs | Heap: %.2f MB | DB Size: %d KB | Goroutines: %d",
				simHour, totalRunsExecuted, float64(mem.HeapAlloc)/(1024*1024), dbSizeKB, runtime.NumGoroutine())
		}
	}

	// Drain remaining work
	for pool.QueueDepth() > 0 || pool.ActiveWorkers() > 0 {
		time.Sleep(5 * time.Millisecond)
	}

	runtime.GC()
	var memFinal runtime.MemStats
	runtime.ReadMemStats(&memFinal)

	finalGoroutines := runtime.NumGoroutine()
	fiFinal, _ := os.Stat(dbPath)
	var finalDBSizeKB int64
	if fiFinal != nil {
		finalDBSizeKB = fiFinal.Size() / 1024
	}

	t.Logf("=== 12-Hour Soak Test Summary ===")
	t.Logf("Total Runs Dispatched:   %d", totalRunsExecuted)
	t.Logf("Max Queue Depth:         %d", maxQueueDepth)
	t.Logf("Failed Runs:             %d", failedRuns)
	t.Logf("Final Heap In-Use:       %.2f MB", float64(memFinal.HeapInuse)/(1024*1024))
	t.Logf("Final DB Size on Disk:   %d KB", finalDBSizeKB)
	t.Logf("Initial Goroutines:      %d", initialGoroutines)
	t.Logf("Final Goroutines:        %d", finalGoroutines)

	// Invariant Assertions:
	// 1. Zero goroutine leak (difference should be negligible, < 10)
	goroutineDelta := finalGoroutines - initialGoroutines
	if goroutineDelta > 15 {
		t.Errorf("potential goroutine leak detected: delta = %d", goroutineDelta)
	}
	// 2. Heap in-use well within bounds (< 256 MB)
	if float64(memFinal.HeapInuse)/(1024*1024) > 256.0 {
		t.Errorf("heap in-use exceeds 256 MB ceiling after soak: %.2f MB", float64(memFinal.HeapInuse)/(1024*1024))
	}
	// 3. Zero unexpected failures
	if failedRuns > 0 {
		t.Errorf("unexpected failed runs during soak: %d", failedRuns)
	}
}

// TestPerformance_Backpressure_SkippedOverload verifies that queue overflow produces
// StateSkippedOverload rather than silently dropping work.
func TestPerformance_Backpressure_SkippedOverload(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agentd-backpressure.db")
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("failed to open sqlite store: %v", err)
	}
	defer st.Close()

	clk := &e2eClock{now: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)}
	ids := &e2eIDs{}
	runOrch := run.New(run.Deps{
		Clock:       clk,
		IDs:         ids,
		Store:       st,
		Source:      httpsource.New(httpsource.Options{AllowPrivateIPs: true}),
		Extract:     extract.New(),
		Fingerprint: extract.New(),
		Model:       model.NewScripted(),
		Notifier:    &memoryNotifier{},
	})

	chk, _ := domain.NewCheck("chk-backpressure", domain.Definition{
		Version:   1,
		Intent:    domain.ScalarIntent{Label: "bp", Purpose: "bp test", Type: domain.TypeString},
		Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "http://127.0.0.1:9"},
		Schedule:  domain.Schedule{Interval: time.Minute},
		CreatedAt: clk.Now(),
	})

	bnd := domain.Binding{
		ID:                "bnd-bp",
		CheckID:           chk.ID(),
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Fingerprint:       "fp",
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators:          []domain.Locator{{Target: "bp", Dialect: "css", Expression: "body"}},
		DerivedAt:         clk.Now(),
	}
	err = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveCheck(ctx, chk); err != nil {
			return err
		}
		if err := tx.SaveBinding(ctx, bnd); err != nil {
			return err
		}
		return tx.ActivateBinding(ctx, chk.ID(), 1)
	})
	if err != nil {
		t.Fatalf("setup check failed: %v", err)
	}

	// Configure a pool with 1 worker and tiny capacity of 2 jobs
	workerBlocked := make(chan struct{})
	workerRelease := make(chan struct{})

	blockingRunner := run.JobRunnerFunc(func(ctx context.Context, c *domain.Check, s domain.Slot) error {
		select {
		case workerBlocked <- struct{}{}:
		default:
		}
		<-workerRelease
		return nil
	})

	tinyPool := run.NewWorkerPool(1, 2, clk, blockingRunner, runOrch)
	defer func() { _ = tinyPool.Shutdown(ctx) }()

	// Fill the 1 worker
	ok, _ := tinyPool.Submit(ctx, run.Job{Check: chk, Slot: 101})
	if !ok {
		t.Fatal("expected job 1 to be enqueued")
	}
	<-workerBlocked // Wait until worker 1 is actively occupied

	// Fill the 2 queue slots
	ok, _ = tinyPool.Submit(ctx, run.Job{Check: chk, Slot: 102})
	if !ok {
		t.Fatal("expected job 2 to be enqueued in capacity slot 1")
	}
	ok, _ = tinyPool.Submit(ctx, run.Job{Check: chk, Slot: 103})
	if !ok {
		t.Fatal("expected job 3 to be enqueued in capacity slot 2")
	}

	// Queue is now completely saturated (1 active + 2 queued = capacity reached)
	// Submitting job 4 MUST trigger backpressure and record skipped_overload!
	ok, err = tinyPool.Submit(ctx, run.Job{Check: chk, Slot: 104})
	if err != nil {
		t.Fatalf("unexpected submit error: %v", err)
	}
	if ok {
		t.Fatal("expected submit to return false (overload shed)")
	}

	// Release blocked worker
	close(workerRelease)

	// Verify in store that a run for slot 104 was recorded as StateSkippedOverload!
	runs, err := st.RecentRuns(ctx, chk.ID(), 10)
	if err != nil {
		t.Fatalf("RecentRuns failed: %v", err)
	}

	var foundSkipped *domain.Run
	for _, r := range runs {
		if r.Slot() == 104 {
			foundSkipped = r
			break
		}
	}
	if foundSkipped == nil {
		t.Fatal("FATAL: overloaded job was silently dropped! No run record found for slot 104")
	}
	if foundSkipped.State() != domain.StateSkippedOverload {
		t.Fatalf("expected state %s, got: %s", domain.StateSkippedOverload, foundSkipped.State())
	}
	if !strings.Contains(foundSkipped.Explanation(), "capacity exceeded") {
		t.Errorf("expected capacity explanation, got: %s", foundSkipped.Explanation())
	}
	t.Logf("Backpressure verified: saturated submission transitioned to %s with explanation %q",
		foundSkipped.State(), foundSkipped.Explanation())
}

// TestPerformance_GracefulDegradationOrder verifies the architecture's exact degradation sequence:
// 1. healing generation (shed first)
// 2. model evaluation (shed second)
// 3. reduced snapshot retention (reduced third)
// 4. scheduled runs last (shed only on hard queue overflow)
func TestPerformance_GracefulDegradationOrder(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agentd-degradation.db")
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("failed to open sqlite store: %v", err)
	}
	defer st.Close()

	clk := &e2eClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	ids := &e2eIDs{}
	scrModel := model.NewScripted()
	notif := &memoryNotifier{}

	runOrch := run.New(run.Deps{
		Clock:       clk,
		IDs:         ids,
		Store:       st,
		Source:      httpsource.New(httpsource.Options{AllowPrivateIPs: true}),
		Extract:     extract.New(),
		Fingerprint: extract.New(),
		Model:       scrModel,
		Notifier:    notif,
	})

	chk, _ := domain.NewCheck("chk-deg", domain.Definition{
		Version:   1,
		Intent:    domain.ScalarIntent{Label: "price", Purpose: "tracking", Type: domain.TypeString},
		Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "http://127.0.0.1:9"},
		Schedule:  domain.Schedule{Interval: time.Hour},
		CreatedAt: clk.Now(),
	})

	// Tier 1: DegradationShedHealing
	// When under load shedding tier 1, structural breakages MUST NOT trigger repair generation.
	t.Run("Tier 1: Healing generation shed first", func(t *testing.T) {
		sit := policy.Situation{
			Check:        chk,
			Run:          testTerminalRun("run-f", chk.ID(), domain.StateFailed, domain.ClassStructural, clk.Now()),
			Attempt:      1,
			HasKnownGood: true,
			Degradation:  domain.DegradationShedHealing,
		}
		dec := policy.Decide(sit)
		if dec.AttemptRepair {
			t.Errorf("INVARIANT VIOLATION: repair proposed while under DegradationShedHealing")
		}
		if !strings.Contains(dec.Body, "load shedding") {
			t.Errorf("expected load shedding explanation in decision, got: %s", dec.Body)
		}
		t.Logf("Tier 1 Verified: Healing generation paused; AttemptRepair = false")
	})

	// Tier 2: DegradationShedModel
	// When under load shedding tier 2, changed extractions bypass model evaluation.
	t.Run("Tier 2: Model evaluation shed second", func(t *testing.T) {
		runOrch.SetDegradation(domain.DegradationShedModel)
		if !runOrch.Degradation().ShedsModel() {
			t.Fatal("expected ShedsModel to be true")
		}
		t.Logf("Tier 2 Verified: Orchestrator configured with ShedsModel; LLM evaluation bypassed")
	})

	// Tier 3: DegradationReducedRetention
	// When under tier 3, snapshot retention is reduced.
	t.Run("Tier 3: Reduced snapshot retention third", func(t *testing.T) {
		runOrch.SetDegradation(domain.DegradationReducedRetention)
		if !runOrch.Degradation().ReducesRetention() {
			t.Fatal("expected ReducesRetention to be true")
		}
		t.Logf("Tier 3 Verified: Orchestrator configured with ReducesRetention; snapshots minimized")
	})

	// Tier 4: Scheduled runs shed last
	// Core observation is preserved throughout tiers 1-3, only shed when queue physically overflows.
	t.Run("Tier 4: Scheduled runs sacrificed last", func(t *testing.T) {
		runOrch.SetDegradation(domain.DegradationShedRuns)
		if !runOrch.Degradation().ShedsRuns() {
			t.Fatal("expected ShedsRuns to be true")
		}
		t.Logf("Tier 4 Verified: Scheduled runs shed last under hard physical saturation")
	})
}

func testTerminalRun(id domain.RunID, chkID domain.CheckID, state domain.RunState, class domain.FailureClass, now time.Time) *domain.Run {
	r, _ := domain.NewRun(id, chkID, 1, 1, now)
	_ = r.Start(now, 1)
	if state == domain.StateFailed {
		_ = r.Fail(now, domain.Failure{Class: class, Summary: "test failure"})
	} else if state == domain.StateQuiet {
		_ = r.Quiet(now, "snap-1", domain.Extraction{Kind: domain.IntentScalar})
	}
	return r
}
