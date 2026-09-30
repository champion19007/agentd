package repair_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/repair"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

func TestOrchestratorApprove_WithSQLiteStore(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fixedTime := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	clk := stubClock{now: fixedTime}

	orch := repair.New(repair.Deps{
		Store: st,
		Clock: clk,
	}, "css")

	// 1. Save check
	chk, err := domain.NewCheck("chk-test", domain.Definition{
		Intent: domain.ScalarIntent{
			Label:   "price",
			Purpose: "price tracking",
			Type:    domain.TypeNumber,
		},
		Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
		Schedule:  domain.Schedule{Interval: time.Hour},
		CreatedAt: fixedTime,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveCheck(ctx, chk)
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Open incident
	inc, err := domain.NewIncident("inc-test", "chk-test", domain.Failure{
		Class:   domain.ClassStructural,
		Code:    "element_missing",
		Summary: "element not found",
	}, 3, fixedTime)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Propose candidate
	cand := domain.Binding{
		ID:                "bnd-repaired",
		CheckID:           "chk-test",
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Fingerprint:       "fp-new",
		Version:           2,
		Origin:            domain.OriginRepaired,
		Locators: []domain.Locator{
			{Target: "price", Dialect: "css", Expression: ".repaired-price"},
		},
		DerivedAt:   fixedTime,
		DerivedFrom: "snap-1",
	}

	snap, err := domain.NewSnapshot("chk-test", "text/html", []byte("<html><span>$42</span></html>"), "fp-new", fixedTime)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.PutSnapshot(ctx, snap)
	}); err != nil {
		t.Fatal(err)
	}

	prop := domain.RepairProposal{
		Binding:         cand,
		Rationale:       "the price moved from div.price to span.repaired-price",
		VerifiedAgainst: snap.ID(),
		VerifiedAt:      fixedTime,
		Gates: []domain.GateResult{
			{Gate: domain.GateG1Structural, Passed: true, Detail: "ok", At: fixedTime},
			{Gate: domain.GateG2Shape, Passed: true, Detail: "ok", At: fixedTime},
			{Gate: domain.GateG3Stability, Passed: true, Detail: "ok", At: fixedTime},
			{Gate: domain.GateG4Semantic, Passed: true, Detail: "ok", At: fixedTime},
			{Gate: domain.GateG5Continuity, Passed: true, Detail: "ok", At: fixedTime},
		},
		ProposedLocators: cand.Locators,
	}

	if _, err := inc.RecordAttempt(domain.AttemptProposed, "verified", fixedTime); err != nil {
		t.Fatal(err)
	}
	if err := inc.Propose(prop, fixedTime); err != nil {
		t.Fatal(err)
	}

	if err := st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, cand); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, inc)
	}); err != nil {
		t.Fatal(err)
	}

	// 4. Approve via Orchestrator
	approvedBinding, err := orch.Approve(ctx, chk, inc.ID(), "alice")
	if err != nil {
		t.Fatalf("Orchestrator.Approve failed unexpectedly: %v", err)
	}
	if approvedBinding == nil || approvedBinding.Version != 2 {
		t.Fatalf("unexpected approved binding: %+v", approvedBinding)
	}

	// 5. Verify the binding is now active in SQLite store
	active, err := st.ActiveBinding(ctx, chk.ID())
	if err != nil {
		t.Fatalf("ActiveBinding failed: %v", err)
	}
	if active.Version != 2 || active.Origin != domain.OriginRepaired {
		t.Errorf("active binding mismatch: version=%d origin=%s", active.Version, active.Origin)
	}
}
