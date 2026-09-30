package tests

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/scheduling"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

type fakeRandom struct{}

func (fakeRandom) Float64() float64 { return 0.5 }

// TestReleaseGate_MemoryScale500Checks verifies that Agentd scales smoothly to
// 500 configured checks while remaining strictly bounded in memory consumption.
func TestReleaseGate_MemoryScale500Checks(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agentd-scale-500.db")

	store, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("failed to open sqlite store: %v", err)
	}
	defer store.Close()

	// Force initial GC to establish accurate memory baseline
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	baseTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const numChecks = 500

	// Step 1: Bulk populate 500 checks with definitions and bindings in transactions
	checks := make([]*domain.Check, numChecks)
	err = store.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		for i := 0; i < numChecks; i++ {
			chkID := domain.CheckID(fmt.Sprintf("chk-scale-%04d", i+1))
			def := domain.Definition{
				Version: 1,
				Intent: domain.ScalarIntent{
					Label:   fmt.Sprintf("metric_%04d", i+1),
					Purpose: fmt.Sprintf("Scale test metric for check #%04d", i+1),
					Type:    domain.TypeNumber,
				},
				Source: domain.SourceSpec{
					Kind: domain.SourceHTTP,
					URL:  fmt.Sprintf("https://monitored.internal/metric/%04d", i+1),
				},
				Schedule: domain.Schedule{
					Interval: 5 * time.Minute,
					CatchUp:  domain.CatchUpOnce,
				},
				Destination: domain.Destination{
					Kind:   domain.DestinationNotify,
					Target: "operator",
				},
				CreatedAt: baseTime,
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
				ID:                domain.BindingID(fmt.Sprintf("bnd-scale-%04d", i+1)),
				CheckID:           chkID,
				DefinitionVersion: 1,
				IntentKind:        domain.IntentScalar,
				Fingerprint:       "fp-initial-scale",
				Version:           1,
				Origin:            domain.OriginInferred,
				Locators: []domain.Locator{
					{Target: fmt.Sprintf("metric_%04d", i+1), Dialect: "css", Expression: ".metric-value"},
				},
				DerivedAt: baseTime,
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
		t.Fatalf("populating 500 checks failed: %v", err)
	}

	// Step 2: Test scheduling and catch-up resolution at 500-check scale
	tScanStart := time.Now()
	enabled, err := store.EnabledChecks(ctx)
	if err != nil {
		t.Fatalf("store.EnabledChecks failed: %v", err)
	}
	if len(enabled) != numChecks {
		t.Fatalf("expected %d enabled checks, got %d", numChecks, len(enabled))
	}

	evalTime := baseTime.Add(24 * time.Hour)
	clk := &e2eClock{now: evalTime}
	sched := scheduling.New(clk, fakeRandom{}, store, scheduling.DefaultOptions())
	due, err := sched.DueChecks(ctx, evalTime)
	if err != nil {
		t.Fatalf("sched.DueChecks failed: %v", err)
	}
	totalSlots := len(due)
	scanDuration := time.Since(tScanStart)

	// Step 3: Measure heap footprint after 500-check population & operations
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	var heapAllocDelta int64
	if memAfter.HeapAlloc > memBefore.HeapAlloc {
		heapAllocDelta = int64(memAfter.HeapAlloc - memBefore.HeapAlloc)
	}
	heapInUseMB := float64(memAfter.HeapInuse) / (1024 * 1024)
	heapAllocDeltaMB := float64(heapAllocDelta) / (1024 * 1024)
	bytesPerCheck := float64(heapAllocDelta) / float64(numChecks)

	t.Logf("=== 500-Check Memory & Scale Verification ===")
	t.Logf("Checks Populated:        %d", numChecks)
	t.Logf("Total Scheduling Slots:  %d", totalSlots)
	t.Logf("Full Scan Duration:      %v", scanDuration)
	t.Logf("Heap In-Use:             %.2f MB", heapInUseMB)
	t.Logf("Heap Alloc Delta:        %.2f MB", heapAllocDeltaMB)
	t.Logf("Average Memory / Check:  %.2f KB", bytesPerCheck/1024)

	// Assertions for release candidate:
	// - Full 500-check scan duration under 2 seconds
	// - Total heap in-use under 50 MB
	// - Average memory per check under 100 KB
	if scanDuration > 2*time.Second {
		t.Errorf("500-check scan duration too slow: %v (target < 2s)", scanDuration)
	}
	if heapInUseMB > 50.0 {
		t.Errorf("heap in-use exceeds 50MB release ceiling: %.2f MB", heapInUseMB)
	}
	if bytesPerCheck > 100*1024 {
		t.Errorf("average memory per check exceeds 100KB: %.2f KB", bytesPerCheck/1024)
	}
}
