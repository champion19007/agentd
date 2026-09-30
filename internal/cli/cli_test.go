package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/api"
	"github.com/champion19007/agentd/internal/cli"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

type stubClock struct {
	now time.Time
}

func (s stubClock) Now() time.Time { return s.now }

func (s stubClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func setupTestEnv(t *testing.T) (cli.Env, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentd_cli.db")
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	ctx := context.Background()

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	svc := api.NewService(api.Deps{
		Store:  st,
		DBPath: dbPath,
		Clock:  stubClock{now: base},
	})

	env := cli.Env{
		Out: out,
		Err: errOut,
		Now: func() time.Time { return base },
		API: svc,
	}

	return env, out, errOut, dbPath
}

func TestCommandParsingAndHelp(t *testing.T) {
	env, out, _, _ := setupTestEnv(t)
	ctx := context.Background()

	// 1. Help flag
	code := cli.Main(ctx, env, []string{"--help"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Commands:") {
		t.Errorf("help output missing Commands: %s", out.String())
	}

	// 2. Unknown command
	out.Reset()
	code = cli.Main(ctx, env, []string{"foobar"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestCheckAddListShowRunDelete(t *testing.T) {
	env, out, errOut, _ := setupTestEnv(t)
	ctx := context.Background()

	// 1. Check Add
	code := cli.Main(ctx, env, []string{
		"check", "add",
		"--id", "check-api",
		"--name", "API Latency",
		"--url", "https://example.com/health",
		"--interval", "5m",
		"--target", "status",
		"--expr", "div.status",
	})
	if code != 0 {
		t.Fatalf("check add failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "Added check check-api") {
		t.Errorf("check add output = %s", out.String())
	}

	// 2. Check Add JSON
	out.Reset()
	code = cli.Main(ctx, env, []string{
		"check", "add",
		"--id", "check-pricing",
		"--name", "Pricing Tier",
		"--url", "https://example.com/pricing",
		"--json",
	})
	if code != 0 {
		t.Fatalf("check add --json failed: %s", errOut.String())
	}
	var checkSum api.CheckSummary
	if err := json.Unmarshal(out.Bytes(), &checkSum); err != nil {
		t.Fatalf("unmarshal check add JSON: %v", err)
	}
	if checkSum.ID != "check-pricing" {
		t.Errorf("checkSum.ID = %s, want check-pricing", checkSum.ID)
	}

	// 3. Check List
	out.Reset()
	code = cli.Main(ctx, env, []string{"check", "list"})
	if code != 0 {
		t.Fatalf("check list failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "check-api") || !strings.Contains(out.String(), "check-pricing") {
		t.Errorf("check list output = %s", out.String())
	}

	// 4. Check List JSON
	out.Reset()
	code = cli.Main(ctx, env, []string{"check", "list", "--json"})
	if code != 0 {
		t.Fatalf("check list --json failed: %s", errOut.String())
	}
	var list []api.CheckSummary
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal check list JSON: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("check list count = %d, want 2", len(list))
	}

	// 5. Check Show
	out.Reset()
	code = cli.Main(ctx, env, []string{"check", "show", "check-api"})
	if code != 0 {
		t.Fatalf("check show failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "Check: check-api") || !strings.Contains(out.String(), "div.status") {
		t.Errorf("check show output = %s", out.String())
	}

	// 6. Check Run
	out.Reset()
	code = cli.Main(ctx, env, []string{"check", "run", "check-api"})
	if code != 0 {
		t.Fatalf("check run failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "Triggered check check-api") {
		t.Errorf("check run output = %s", out.String())
	}

	// 7. Check Delete
	out.Reset()
	code = cli.Main(ctx, env, []string{"check", "delete", "check-pricing"})
	if code != 0 {
		t.Fatalf("check delete failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "Disabled check check-pricing") {
		t.Errorf("check delete output = %s", out.String())
	}
}

func TestStatusOutputHumanAndJSON(t *testing.T) {
	env, out, errOut, _ := setupTestEnv(t)
	ctx := context.Background()

	// Human output
	code := cli.Main(ctx, env, []string{"status"})
	if code != 0 {
		t.Fatalf("status failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "Agentd Status: HEALTHY") {
		t.Errorf("status human output: %s", out.String())
	}
	if !strings.Contains(out.String(), "Integrity: ok") {
		t.Errorf("status human output missing integrity: %s", out.String())
	}

	// JSON output
	out.Reset()
	code = cli.Main(ctx, env, []string{"status", "--json"})
	if code != 0 {
		t.Fatalf("status --json failed: %s", errOut.String())
	}
	var st api.StatusResponse
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal status JSON: %v", err)
	}
	if !st.Healthy || st.Integrity != "ok" {
		t.Errorf("st = %+v", st)
	}
}

func TestIncidentProposalAndDecisions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentd_inc.db")
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	ctx := context.Background()

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	svc := api.NewService(api.Deps{
		Store:  st,
		DBPath: dbPath,
		Clock:  stubClock{now: base},
	})

	env := cli.Env{
		Out: out,
		Err: errOut,
		Now: func() time.Time { return base },
		API: svc,
	}

	// Add check
	_, err = svc.AddCheck(ctx, api.AddCheckRequest{
		ID:   "check-table",
		Name: "Pricing Table",
		URL:  "https://example.com/plans",
	})
	if err != nil {
		t.Fatalf("AddCheck: %v", err)
	}

	// Open incident with verified proposal
	c, _ := st.Check(ctx, "check-table")
	incLog := domain.NewIncidentLog(c.ID())
	inc, _ := incLog.Open("inc-pricing", domain.Failure{
		Class:   domain.ClassStructural,
		Summary: "selector table.pricing matched no elements",
	}, 3, base)

	prop := domain.RepairProposal{
		Binding: domain.Binding{
			ID:                "bin-2",
			CheckID:           "check-table",
			DefinitionVersion: 1,
			IntentKind:        domain.IntentScalar,
			Fingerprint:       "fp2",
			Version:           2,
			Origin:            domain.OriginRepaired,
			Locators: []domain.Locator{
				{Target: "value", Dialect: "css", Expression: "div.pricing-card span.price"},
			},
			DerivedAt: base,
		},
		Rationale:       "The pricing table moved into modern card containers. Found price element under div.pricing-card.",
		VerifiedAgainst: "snap-abc123",
		VerifiedAt:      base,
		Diff:            "- table.pricing td\n+ div.pricing-card span.price",
		Gates: []domain.GateResult{
			{Gate: domain.GateG1Structural, Passed: true, Detail: "valid output within size bounds", At: base},
			{Gate: domain.GateG2Shape, Passed: true, Detail: "matches scalar shape", At: base},
			{Gate: domain.GateG3Stability, Passed: true, Detail: "stable across re-fetch", At: base},
			{Gate: domain.GateG4Semantic, Passed: true, Detail: "semantic AI verification approved", At: base},
			{Gate: domain.GateG5Continuity, Passed: true, Detail: "numeric continuity consistent ($49 vs $49)", At: base},
		},
	}
	_, _ = inc.RecordAttempt(domain.AttemptProposed, "candidate 1 proposed", base)
	_ = inc.Propose(prop, base)

	_ = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, prop.Binding); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, inc)
	})

	// 1. Incident List
	out.Reset()
	code := cli.Main(ctx, env, []string{"incident", "list"})
	if code != 0 {
		t.Fatalf("incident list failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "inc-pricing") || !strings.Contains(out.String(), "check-table") {
		t.Errorf("incident list: %s", out.String())
	}

	// 2. Incident Show (Human readable output verification)
	out.Reset()
	code = cli.Main(ctx, env, []string{"incident", "show", "inc-pricing"})
	if code != 0 {
		t.Fatalf("incident show failed: %s", errOut.String())
	}
	humanOut := out.String()

	// Verify all required sections are present
	if !strings.Contains(humanOut, "WHAT BROKE:") {
		t.Errorf("missing WHAT BROKE section: %s", humanOut)
	}
	if !strings.Contains(humanOut, "source structure appears to have changed") {
		t.Errorf("error not explained in user terms: %s", humanOut)
	}
	if !strings.Contains(humanOut, "WHAT AGENTD FOUND:") {
		t.Errorf("missing WHAT AGENTD FOUND section: %s", humanOut)
	}
	if !strings.Contains(humanOut, "WHAT IT PROPOSES:") {
		t.Errorf("missing WHAT IT PROPOSES section: %s", humanOut)
	}
	if !strings.Contains(humanOut, "VERIFICATION RESULTS:") {
		t.Errorf("missing VERIFICATION RESULTS section: %s", humanOut)
	}
	if !strings.Contains(humanOut, "[PASS] g1_structural") || !strings.Contains(humanOut, "[PASS] g4_semantic") {
		t.Errorf("missing gate verification verdicts: %s", humanOut)
	}
	if !strings.Contains(humanOut, "WHAT ACTION IS REQUIRED:") {
		t.Errorf("missing WHAT ACTION IS REQUIRED section: %s", humanOut)
	}
	if !strings.Contains(humanOut, "agentd repair approve inc-pricing --by <your-name>") {
		t.Errorf("missing approval instruction: %s", humanOut)
	}

	// 3. Incident Show JSON
	out.Reset()
	code = cli.Main(ctx, env, []string{"incident", "show", "inc-pricing", "--json"})
	if code != 0 {
		t.Fatalf("incident show --json failed: %s", errOut.String())
	}
	var incDetail api.IncidentDetail
	if err := json.Unmarshal(out.Bytes(), &incDetail); err != nil {
		t.Fatalf("unmarshal incident detail JSON: %v", err)
	}
	if incDetail.ID != "inc-pricing" || len(incDetail.VerificationResults) != 5 {
		t.Errorf("incDetail = %+v", incDetail)
	}

	// 4. Repair approve without --by must fail
	out.Reset()
	errOut.Reset()
	code = cli.Main(ctx, env, []string{"repair", "approve", "inc-pricing"})
	if code == 0 {
		t.Fatal("repair approve without --by should fail")
	}
	if !strings.Contains(errOut.String(), "--by is required") {
		t.Errorf("expected '--by is required' error, got %s", errOut.String())
	}

	// 5. Repair approve with --by succeeds
	out.Reset()
	errOut.Reset()
	code = cli.Main(ctx, env, []string{"repair", "approve", "inc-pricing", "--by", "dana"})
	if code != 0 {
		t.Fatalf("repair approve failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "approved by dana") {
		t.Errorf("approval output: %s", out.String())
	}

	// 6. Verify check binding activated
	b, err := st.ActiveBinding(ctx, "check-table")
	if err != nil {
		t.Fatalf("ActiveBinding: %v", err)
	}
	if b.Version != 2 || b.Origin != domain.OriginRepaired {
		t.Errorf("ActiveBinding version = %d, origin = %s", b.Version, b.Origin)
	}
}

func TestGCAndBackupCommands(t *testing.T) {
	env, out, errOut, _ := setupTestEnv(t)
	ctx := context.Background()

	// 1. GC Dry Run
	code := cli.Main(ctx, env, []string{"gc", "--dry-run"})
	if code != 0 {
		t.Fatalf("gc --dry-run failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "nothing to remove") && !strings.Contains(out.String(), "would remove") {
		t.Errorf("gc output: %s", out.String())
	}

	// 2. Backup
	out.Reset()
	backupFile := filepath.Join(t.TempDir(), "agentd_bk.db")
	code = cli.Main(ctx, env, []string{"backup", "--to", backupFile, "--verify"})
	if code != 0 {
		t.Fatalf("backup failed: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "verified: schema version") {
		t.Errorf("backup output: %s", out.String())
	}
}

func TestServeAndMCPCommands(t *testing.T) {
	env, out, errOut, _ := setupTestEnv(t)
	ctx := context.Background()

	// 1. Test MCP command over stdio
	env.In = strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n")
	code := cli.Main(ctx, env, []string{"mcp"})
	if code != 0 {
		t.Fatalf("agentd mcp failed with code %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"id":1`) {
		t.Errorf("agentd mcp output: %s", out.String())
	}

	// 2. Test serve command with unsafe remote address without allow-remote (must fail)
	out.Reset()
	errOut.Reset()
	code = cli.Main(ctx, env, []string{"serve", "--addr", "0.0.0.0:8080"})
	if code == 0 {
		t.Errorf("expected failure when binding serve to non-loopback 0.0.0.0 without --allow-remote")
	}

	// 3. Test serve command on loopback with context cancellation
	out.Reset()
	errOut.Reset()
	serveCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	code = cli.Main(serveCtx, env, []string{"serve", "--addr", "127.0.0.1:0"})
	if code != 0 {
		t.Fatalf("agentd serve failed with code %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Agentd HTTP API listening on") {
		t.Errorf("expected listening message in serve output: %s", out.String())
	}
}
