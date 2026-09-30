package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
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

// runCLI is a helper to run cli.Main with a real store/API environment
func runCLI(t *testing.T, dbPath string, args ...string) (int, string, string) {
	t.Helper()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}

	ctx := context.Background()
	env := cli.DefaultEnv()
	env.Out = outBuf
	env.Err = errBuf
	if dbPath != "" {
		// Insert -db flag if not present in args
		hasDB := false
		for _, a := range args {
			if a == "-db" || strings.HasPrefix(a, "-db=") || a == "--db" || strings.HasPrefix(a, "--db=") {
				hasDB = true
				break
			}
		}
		if !hasDB {
			args = append(args, "-db", dbPath)
		}
	}

	code := cli.Main(ctx, env, args)
	return code, outBuf.String(), errBuf.String()
}

// 1. Configuration & Setup: agentd init
func TestOperatorWorkflow_Init(t *testing.T) {
	tempDir := t.TempDir()

	// 1.1 Clean directory creation: nested path that does not exist yet
	nestedDB := filepath.Join(tempDir, "deeply", "nested", "storage", "agentd.db")
	code, out, errOut := runCLI(t, nestedDB, "init")
	if code != 0 {
		t.Fatalf("init on nested directory failed (code %d): %s", code, errOut)
	}
	if !strings.Contains(out, "Agentd database initialized at") {
		t.Errorf("expected initialization confirmation, got: %s", out)
	}
	if _, err := os.Stat(nestedDB); os.IsNotExist(err) {
		t.Fatalf("database file was not created on disk at %s", nestedDB)
	}

	// 1.2 Idempotency: running init a second time on the same path
	code, out, errOut = runCLI(t, nestedDB, "init")
	if code != 0 {
		t.Fatalf("second init failed (code %d): %s", code, errOut)
	}
	if !strings.Contains(out, "already initialized") {
		t.Errorf("expected 'already initialized' message on second run, got: %s", out)
	}

	// 1.3 Init with --json flag
	jsonDB := filepath.Join(tempDir, "json_init", "agentd.db")
	code, out, errOut = runCLI(t, jsonDB, "init", "--json")
	if code != 0 {
		t.Fatalf("init --json failed (code %d): %s", code, errOut)
	}
	var resp api.InitResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("failed to parse init JSON response: %v, output: %s", err, out)
	}
	if !resp.Created {
		t.Errorf("expected Created=true on first run, got false")
	}

	// Second run with --json: Created should be false
	code, out, errOut = runCLI(t, jsonDB, "init", "--json")
	if code != 0 {
		t.Fatalf("second init --json failed: %s", errOut)
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("failed to parse second init JSON response: %v", err)
	}
	if resp.Created {
		t.Errorf("expected Created=false on second run, got true")
	}
}

// 2. Check Management: add, list, show, run, delete
func TestOperatorWorkflow_Checks(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "checks_test.db")

	// Initialize DB
	if code, _, errOut := runCLI(t, dbPath, "init"); code != 0 {
		t.Fatalf("init failed: %s", errOut)
	}

	// 2.1 Initial empty state
	code, out, _ := runCLI(t, dbPath, "check", "list")
	if code != 0 || !strings.Contains(out, "no checks are configured") {
		t.Errorf("expected 'no checks are configured', got: %s", out)
	}

	// 2.2 Input validation errors on check add:
	// Missing --name
	code, _, errOut := runCLI(t, dbPath, "check", "add", "--url", "https://example.com")
	if code == 0 || !strings.Contains(errOut, "--name is required") {
		t.Errorf("expected '--name is required', got code %d, err: %s", code, errOut)
	}

	// Missing --url
	code, _, errOut = runCLI(t, dbPath, "check", "add", "--name", "Test Check")
	if code == 0 || !strings.Contains(errOut, "--url is required") {
		t.Errorf("expected '--url is required', got code %d, err: %s", code, errOut)
	}

	// Invalid interval: text string
	code, _, errOut = runCLI(t, dbPath, "check", "add", "--name", "Test Check", "--url", "https://example.com", "--interval", "notaninterval")
	if code == 0 || !strings.Contains(errOut, "invalid interval") {
		t.Errorf("expected 'invalid interval' error, got code %d, err: %s", code, errOut)
	}

	// Invalid interval: negative duration
	code, _, errOut = runCLI(t, dbPath, "check", "add", "--name", "Test Check", "--url", "https://example.com", "--interval", "-10m")
	if code == 0 || !strings.Contains(errOut, "schedule needs a positive interval") {
		t.Errorf("expected 'needs a positive interval' error, got code %d, err: %s", code, errOut)
	}

	// Invalid interval: zero duration
	code, _, errOut = runCLI(t, dbPath, "check", "add", "--name", "Test Check", "--url", "https://example.com", "--interval", "0s")
	if code == 0 || !strings.Contains(errOut, "schedule needs a positive interval") {
		t.Errorf("expected 'needs a positive interval' error, got code %d, err: %s", code, errOut)
	}

	// 2.3 Add Check with CSS selector
	code, out, errOut = runCLI(t, dbPath, "check", "add",
		"--id", "check-css",
		"--name", "CSS Price Check",
		"--url", "https://example.com/products/widget",
		"--interval", "15m",
		"--target", "price",
		"--dialect", "css",
		"--expr", "div.product-card span.price",
	)
	if code != 0 {
		t.Fatalf("check add CSS failed: %s", errOut)
	}
	if !strings.Contains(out, "Added check check-css") {
		t.Errorf("expected check add success, got: %s", out)
	}

	// 2.4 Add Check with JSONPath selector
	code, out, errOut = runCLI(t, dbPath, "check", "add",
		"--id", "check-json",
		"--name", "API Health Status",
		"--url", "https://api.example.com/v1/health",
		"--interval", "1m",
		"--target", "status",
		"--dialect", "jsonpath",
		"--expr", "$.components.database.status",
		"--notify", "none",
	)
	if code != 0 {
		t.Fatalf("check add JSONPath failed: %s", errOut)
	}
	if !strings.Contains(out, "Added check check-json") {
		t.Errorf("expected check add JSONPath success, got: %s", out)
	}

	// 2.5 Duplicate Check ID detection
	code, _, errOut = runCLI(t, dbPath, "check", "add",
		"--id", "check-css",
		"--name", "Duplicate Check",
		"--url", "https://example.com/duplicate",
	)
	if code == 0 || !strings.Contains(errOut, `check "check-css" already exists`) {
		t.Errorf("expected duplicate check error, got code %d, err: %s", code, errOut)
	}

	// 2.6 List checks (Human readable table)
	code, out, errOut = runCLI(t, dbPath, "check", "list")
	if code != 0 {
		t.Fatalf("check list failed: %s", errOut)
	}
	if !strings.Contains(out, "CHECK") || !strings.Contains(out, "WATCHING") {
		t.Errorf("table headers missing in check list: %s", out)
	}
	if !strings.Contains(out, "check-css") || !strings.Contains(out, "check-json") {
		t.Errorf("expected check-css and check-json in list: %s", out)
	}

	// 2.7 List checks (--json)
	code, out, errOut = runCLI(t, dbPath, "check", "list", "--json")
	if code != 0 {
		t.Fatalf("check list --json failed: %s", errOut)
	}
	var checkList []api.CheckSummary
	if err := json.Unmarshal([]byte(out), &checkList); err != nil {
		t.Fatalf("unmarshal check list JSON: %v", err)
	}
	if len(checkList) != 2 {
		t.Errorf("expected 2 checks in JSON list, got %d", len(checkList))
	}

	// 2.8 Show check (Human readable)
	code, out, errOut = runCLI(t, dbPath, "check", "show", "check-css")
	if code != 0 {
		t.Fatalf("check show failed: %s", errOut)
	}
	if !strings.Contains(out, "Check: check-css (active)") ||
		!strings.Contains(out, "Intent: CSS Price Check") ||
		!strings.Contains(out, "URL: https://example.com/products/widget") ||
		!strings.Contains(out, "Schedule: every 15m") ||
		!strings.Contains(out, "price (css): div.product-card span.price") {
		t.Errorf("unexpected check show output: %s", out)
	}

	// 2.9 Show check (--json)
	code, out, errOut = runCLI(t, dbPath, "check", "show", "check-css", "--json")
	if code != 0 {
		t.Fatalf("check show --json failed: %s", errOut)
	}
	var checkDetail api.CheckDetail
	if err := json.Unmarshal([]byte(out), &checkDetail); err != nil {
		t.Fatalf("unmarshal check show JSON: %v", err)
	}
	if checkDetail.ID != "check-css" || len(checkDetail.Locators) != 1 || checkDetail.Locators[0].Dialect != "css" {
		t.Errorf("unexpected check detail: %+v", checkDetail)
	}

	// 2.10 Show non-existent check
	code, _, errOut = runCLI(t, dbPath, "check", "show", "check-nonexistent")
	if code == 0 || !strings.Contains(errOut, "not found") {
		t.Errorf("expected not found error for nonexistent check, got code %d, err: %s", code, errOut)
	}

	// 2.11 Missing argument to check show
	code, _, errOut = runCLI(t, dbPath, "check", "show")
	if code == 0 || !strings.Contains(errOut, "usage: agentd check show <check-id>") {
		t.Errorf("expected usage error, got code %d, err: %s", code, errOut)
	}

	// 2.12 Run check manually
	code, out, errOut = runCLI(t, dbPath, "check", "run", "check-css")
	if code != 0 {
		t.Fatalf("check run failed: %s", errOut)
	}
	if !strings.Contains(out, "Triggered check check-css") {
		t.Errorf("expected triggered confirmation in check run, got: %s", out)
	}

	// 2.13 Run check (--json)
	code, out, errOut = runCLI(t, dbPath, "check", "run", "check-css", "--json")
	if code != 0 {
		t.Fatalf("check run --json failed: %s", errOut)
	}
	var runSum api.RunSummary
	if err := json.Unmarshal([]byte(out), &runSum); err != nil {
		t.Fatalf("unmarshal check run JSON: %v", err)
	}
	if runSum.CheckID != "check-css" {
		t.Errorf("unexpected run check ID: %s", runSum.CheckID)
	}

	// 2.14 Delete check (disables check)
	code, out, errOut = runCLI(t, dbPath, "check", "delete", "check-json")
	if code != 0 {
		t.Fatalf("check delete failed: %s", errOut)
	}
	if !strings.Contains(out, "Disabled check check-json") {
		t.Errorf("expected disabled confirmation, got: %s", out)
	}

	// 2.15 Verify deleted check is no longer in check list
	code, out, _ = runCLI(t, dbPath, "check", "list")
	if strings.Contains(out, "check-json") {
		t.Errorf("disabled check-json should not appear in check list: %s", out)
	}

	// 2.16 Show deleted check shows disabled status
	code, out, _ = runCLI(t, dbPath, "check", "show", "check-json")
	if code != 0 || !strings.Contains(out, "Check: check-json (disabled)") {
		t.Errorf("expected disabled status in check show, got: %s", out)
	}

	// 2.17 Deleting non-existent check fails cleanly
	code, _, errOut = runCLI(t, dbPath, "check", "delete", "check-nonexistent")
	if code == 0 || !strings.Contains(errOut, "not found") {
		t.Errorf("expected not found error, got code %d, err: %s", code, errOut)
	}

	// 2.18 Unknown check subcommand
	code, _, errOut = runCLI(t, dbPath, "check", "invalidsubcommand")
	if code == 0 || !strings.Contains(errOut, "unknown check subcommand") {
		t.Errorf("expected unknown subcommand error, got code %d, err: %s", code, errOut)
	}
}

// 3. Incident and Repair Workflow
func TestOperatorWorkflow_IncidentsAndRepairs(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "incident_workflow.db")
	ctx := context.Background()

	// Initialize DB
	if code, _, errOut := runCLI(t, dbPath, "init"); code != 0 {
		t.Fatalf("init failed: %s", errOut)
	}

	// Add check
	_, out, errOut := runCLI(t, dbPath, "check", "add",
		"--id", "chk-billing",
		"--name", "Monthly Billing Total",
		"--url", "https://billing.example.com/invoice",
	)
	if !strings.Contains(out, "Added check chk-billing") {
		t.Fatalf("check add failed: %s", errOut)
	}

	// Initially no incidents
	code, out, _ := runCLI(t, dbPath, "incident", "list")
	if code != 0 || !strings.Contains(out, "nothing is waiting for you") {
		t.Errorf("expected 'nothing is waiting for you', got: %s", out)
	}

	// Open an incident directly in the store to test full operator workflow
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	now := time.Now().UTC()
	c, _ := st.Check(ctx, "chk-billing")
	incLog := domain.NewIncidentLog(c.ID())
	inc, err := incLog.Open("inc-billing-01", domain.Failure{
		Class:   domain.ClassStructural,
		Summary: "selector div.total-amount matched 0 elements",
	}, 3, now)
	if err != nil {
		t.Fatalf("incLog.Open: %v", err)
	}

	proposal := domain.RepairProposal{
		Binding: domain.Binding{
			ID:                "bin-billing-v2",
			CheckID:           "chk-billing",
			DefinitionVersion: 1,
			IntentKind:        domain.IntentScalar,
			Fingerprint:       "fp-v2",
			Version:           2,
			Origin:            domain.OriginRepaired,
			Locators: []domain.Locator{
				{Target: "value", Dialect: "css", Expression: "div.invoice-total span.amount"},
			},
			DerivedAt: now,
		},
		Rationale:       "Billing portal layout was updated; invoice total moved inside modern container div.invoice-total",
		VerifiedAgainst: "snap-billing-999",
		VerifiedAt:      now,
		Diff:            "- div.total-amount\n+ div.invoice-total span.amount",
		Gates: []domain.GateResult{
			{Gate: domain.GateG1Structural, Passed: true, Detail: "single DOM element extracted cleanly", At: now},
			{Gate: domain.GateG2Shape, Passed: true, Detail: "matches scalar format $XX.YY", At: now},
			{Gate: domain.GateG3Stability, Passed: true, Detail: "stable across 2 simulated re-fetches", At: now},
			{Gate: domain.GateG4Semantic, Passed: true, Detail: "AI semantic verification confirmed invoice total intent", At: now},
			{Gate: domain.GateG5Continuity, Passed: true, Detail: "matches prior billing magnitude ($149.00)", At: now},
		},
	}
	_, _ = inc.RecordAttempt(domain.AttemptProposed, "candidate 1 proposed", now)
	_ = inc.Propose(proposal, now)

	_ = st.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, proposal.Binding); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, inc)
	})
	_ = st.Close()

	// 3.1 List incidents
	code, out, errOut = runCLI(t, dbPath, "incident", "list")
	if code != 0 {
		t.Fatalf("incident list failed: %s", errOut)
	}
	if !strings.Contains(out, "inc-billing-01") ||
		!strings.Contains(out, "State: awaiting_approval") ||
		!strings.Contains(out, "A proposed repair is ready for review.") {
		t.Errorf("unexpected incident list output: %s", out)
	}

	// 3.2 Show incident: Human-readable terms test
	code, out, errOut = runCLI(t, dbPath, "incident", "show", "inc-billing-01")
	if code != 0 {
		t.Fatalf("incident show failed: %s", errOut)
	}

	// Verify breakage explanation is in operator terms NOT raw code/nil errors
	if !strings.Contains(out, "WHAT BROKE:") ||
		!strings.Contains(out, "Agentd could not find the target data. The source structure appears to have changed.") {
		t.Errorf("breakage explanation missing operator terms: %s", out)
	}
	if !strings.Contains(out, "WHAT AGENTD FOUND:") ||
		!strings.Contains(out, "located candidate elements reproducing the intent") {
		t.Errorf("WHAT AGENTD FOUND missing: %s", out)
	}
	if !strings.Contains(out, "WHAT IT PROPOSES:") ||
		!strings.Contains(out, "value (css) -> div.invoice-total span.amount") {
		t.Errorf("proposed locators missing: %s", out)
	}
	if !strings.Contains(out, "VERIFICATION RESULTS:") ||
		!strings.Contains(out, "[PASS] g1_structural") ||
		!strings.Contains(out, "[PASS] g4_semantic") {
		t.Errorf("verification gates missing: %s", out)
	}
	if !strings.Contains(out, "WHAT ACTION IS REQUIRED:") ||
		!strings.Contains(out, "agentd repair approve inc-billing-01 --by <your-name>") {
		t.Errorf("action required missing: %s", out)
	}

	// 3.3 Show non-existent incident
	code, _, errOut = runCLI(t, dbPath, "incident", "show", "inc-nonexistent")
	if code == 0 || !strings.Contains(errOut, "not found") {
		t.Errorf("expected not found for nonexistent incident, got code %d, err: %s", code, errOut)
	}

	// 3.4 Repair approve WITHOUT --by MUST fail (attribution requirement)
	code, _, errOut = runCLI(t, dbPath, "repair", "approve", "inc-billing-01")
	if code == 0 || !strings.Contains(errOut, "--by is required") {
		t.Errorf("approval without --by must fail with attribution error, got: %s", errOut)
	}

	// 3.5 Repair reject WITHOUT --by MUST fail
	code, _, errOut = runCLI(t, dbPath, "repair", "reject", "inc-billing-01")
	if code == 0 || !strings.Contains(errOut, "--by is required") {
		t.Errorf("rejection without --by must fail with attribution error, got: %s", errOut)
	}

	// 3.6 Approve nonexistent incident MUST fail
	code, _, errOut = runCLI(t, dbPath, "repair", "approve", "inc-ghost", "--by", "alex")
	if code == 0 || !strings.Contains(errOut, "no open incident called") {
		t.Errorf("expected 'no open incident' error, got code %d, err: %s", code, errOut)
	}

	// 3.7 Reject nonexistent incident MUST fail
	code, _, errOut = runCLI(t, dbPath, "repair", "reject", "inc-ghost", "--by", "alex")
	if code == 0 || !strings.Contains(errOut, "no open incident called") {
		t.Errorf("expected 'no open incident' error, got code %d, err: %s", code, errOut)
	}

	// 3.8 Repair approve WITH --by succeeds
	code, out, errOut = runCLI(t, dbPath, "repair", "approve", "inc-billing-01", "--by", "alex", "--note", "Verified in browser dev tools")
	if code != 0 {
		t.Fatalf("repair approve failed: %s", errOut)
	}
	if !strings.Contains(out, "Applied repair to chk-billing (version 2), approved by alex") {
		t.Errorf("unexpected approval message: %s", out)
	}

	// 3.9 Approving already-closed incident MUST fail cleanly with clear message
	code, _, errOut = runCLI(t, dbPath, "repair", "approve", "inc-billing-01", "--by", "alex")
	if code == 0 || !strings.Contains(errOut, "already closed") {
		t.Errorf("expected 'already closed' error when approving closed incident, got: %s", errOut)
	}

	// 3.10 Rejecting already-closed incident MUST fail cleanly
	code, _, errOut = runCLI(t, dbPath, "repair", "reject", "inc-billing-01", "--by", "alex")
	if code == 0 || !strings.Contains(errOut, "already closed") {
		t.Errorf("expected 'already closed' error when rejecting closed incident, got: %s", errOut)
	}

	// 3.11 Inspect closed incident with incident show
	code, out, errOut = runCLI(t, dbPath, "incident", "show", "inc-billing-01")
	if code != 0 {
		t.Fatalf("incident show on closed incident failed: %s", errOut)
	}
	if !strings.Contains(out, "Incident: inc-billing-01 (check: chk-billing)") || !strings.Contains(out, "State:    resolved") {
		t.Errorf("unexpected closed incident show output: %s", out)
	}

	// 3.12 Check binding activated
	code, out, _ = runCLI(t, dbPath, "check", "show", "chk-billing")
	if !strings.Contains(out, "div.invoice-total span.amount") {
		t.Errorf("new binding locator not active on check: %s", out)
	}
}

// 4. Maintenance & Reliability: status, gc, backup, audit
func TestOperatorWorkflow_MaintenanceAndReliability(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "maint_test.db")

	// Init
	if code, _, errOut := runCLI(t, dbPath, "init"); code != 0 {
		t.Fatalf("init failed: %s", errOut)
	}

	// Add check
	_, _, _ = runCLI(t, dbPath, "check", "add",
		"--id", "chk-maint",
		"--name", "Maint Check",
		"--url", "https://example.com/status",
	)

	// 4.1 Status human readable
	code, out, errOut := runCLI(t, dbPath, "status")
	if code != 0 {
		t.Fatalf("status failed: %s", errOut)
	}
	if !strings.Contains(out, "Agentd Status: HEALTHY") ||
		!strings.Contains(out, "Integrity: ok") ||
		!strings.Contains(out, "Checks: 1 active") {
		t.Errorf("unexpected status output: %s", out)
	}

	// 4.2 Status --json
	code, out, errOut = runCLI(t, dbPath, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json failed: %s", errOut)
	}
	var stResp api.StatusResponse
	if err := json.Unmarshal([]byte(out), &stResp); err != nil {
		t.Fatalf("unmarshal status JSON: %v", err)
	}
	if !stResp.Healthy || stResp.Integrity != "ok" || stResp.ChecksEnabled != 1 {
		t.Errorf("unexpected status JSON: %+v", stResp)
	}

	// 4.3 GC dry-run
	code, out, errOut = runCLI(t, dbPath, "gc", "--dry-run")
	if code != 0 {
		t.Fatalf("gc --dry-run failed: %s", errOut)
	}
	if !strings.Contains(out, "nothing to remove") && !strings.Contains(out, "would remove") {
		t.Errorf("unexpected gc dry-run output: %s", out)
	}

	// 4.4 GC live sweep
	code, out, errOut = runCLI(t, dbPath, "gc", "--runs", "30", "--incidents", "90", "--snapshots", "5")
	if code != 0 {
		t.Fatalf("gc live sweep failed: %s", errOut)
	}
	if !strings.Contains(out, "nothing to remove") && !strings.Contains(out, "removed") {
		t.Errorf("unexpected gc live output: %s", out)
	}

	// 4.5 GC --json
	code, out, errOut = runCLI(t, dbPath, "gc", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("gc --json failed: %s", errOut)
	}
	var sweep domain.Sweep
	if err := json.Unmarshal([]byte(out), &sweep); err != nil {
		t.Fatalf("unmarshal gc JSON: %v", err)
	}

	// 4.6 Backup with --verify
	backupPath := filepath.Join(tempDir, "backups", "agentd_verified.db")
	code, out, errOut = runCLI(t, dbPath, "backup", "--to", backupPath, "--verify")
	if code != 0 {
		t.Fatalf("backup failed: %s", errOut)
	}
	if !strings.Contains(out, "verified: schema version") || !strings.Contains(out, "integrity check passed") {
		t.Errorf("expected verification in backup output, got: %s", out)
	}
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		t.Fatalf("backup file was not created on disk: %s", backupPath)
	}

	// 4.7 Backup to already existing destination must fail
	code, _, errOut = runCLI(t, dbPath, "backup", "--to", backupPath)
	if code == 0 || !strings.Contains(errOut, "already exists") {
		t.Errorf("expected backup failure when destination exists, got code %d, err: %s", code, errOut)
	}

	// 4.8 Backup --json
	jsonBackupPath := filepath.Join(tempDir, "backups", "agentd_json.db")
	code, out, errOut = runCLI(t, dbPath, "backup", "--to", jsonBackupPath, "--verify", "--json")
	if code != 0 {
		t.Fatalf("backup --json failed: %s", errOut)
	}
	var bResult api.BackupResult
	if err := json.Unmarshal([]byte(out), &bResult); err != nil {
		t.Fatalf("unmarshal backup JSON: %v", err)
	}
	if !bResult.Verified || bResult.SizeBytes <= 0 {
		t.Errorf("unexpected backup JSON result: %+v", bResult)
	}

	// 4.9 Audit trail (Human table)
	code, out, errOut = runCLI(t, dbPath, "audit")
	if code != 0 {
		t.Fatalf("audit failed: %s", errOut)
	}
	if !strings.Contains(out, "WHEN") || !strings.Contains(out, "WHO") || !strings.Contains(out, "chk-maint") {
		t.Errorf("unexpected audit table output: %s", out)
	}

	// 4.10 Audit trail (--json)
	code, out, errOut = runCLI(t, dbPath, "audit", "--json")
	if code != 0 {
		t.Fatalf("audit --json failed: %s", errOut)
	}
	var events []domain.AuditEvent
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		t.Fatalf("unmarshal audit JSON: %v", err)
	}
	if len(events) == 0 {
		t.Errorf("expected at least 1 audit event, got 0")
	}
}

// 5. Server Execution & Version
func TestOperatorWorkflow_ServerAndVersion(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "server_test.db")

	// 5.1 Version command
	code, out, _ := runCLI(t, dbPath, "version")
	if code != 0 || !strings.Contains(out, "agentd") {
		t.Errorf("version output unexpected: %s", out)
	}

	// 5.2 Version --json
	code, out, _ = runCLI(t, dbPath, "version", "--json")
	if code != 0 || !strings.Contains(out, `"version"`) {
		t.Errorf("version --json output unexpected: %s", out)
	}

	// 5.3 Serve security guard: binding to 0.0.0.0 without --allow-remote must be rejected
	code, _, errOut := runCLI(t, dbPath, "serve", "--addr", "0.0.0.0:9099")
	if code == 0 || !strings.Contains(errOut, "loopback") {
		t.Errorf("expected rejection of 0.0.0.0 without --allow-remote, got code %d, err: %s", code, errOut)
	}
}

// 6. User Ergonomics & Papercuts: Top level aliases, invalid flags, unknown commands
func TestOperatorWorkflow_ErgonomicsAndAliases(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "aliases_test.db")

	// Init
	if code, _, _ := runCLI(t, dbPath, "init"); code != 0 {
		t.Fatalf("init failed")
	}

	// 6.1 Top level aliases
	// 'checks' -> 'check list'
	code, out, _ := runCLI(t, dbPath, "checks")
	if code != 0 || !strings.Contains(out, "no checks are configured") {
		t.Errorf("checks alias failed: %s", out)
	}

	// 'incidents' -> 'incident list'
	code, out, _ = runCLI(t, dbPath, "incidents")
	if code != 0 || !strings.Contains(out, "nothing is waiting for you") {
		t.Errorf("incidents alias failed: %s", out)
	}

	// 6.2 Unknown commands return exit code 2
	code, _, errOut := runCLI(t, dbPath, "nonexistentcommand")
	if code != 2 || !strings.Contains(errOut, "no command called") {
		t.Errorf("expected code 2 and 'no command called', got code %d, err: %s", code, errOut)
	}

	// 6.3 Flag ordering: flags before or after arguments
	// e.g. check show check-id --json vs check show --json check-id
	code, out, _ = runCLI(t, dbPath, "check", "add", "--name", "Flag Test", "--url", "https://example.com", "--id", "chk-flags")
	if code != 0 {
		t.Fatalf("check add failed: %s", out)
	}

	code1, out1, _ := runCLI(t, dbPath, "check", "show", "chk-flags", "--json")
	code2, out2, _ := runCLI(t, dbPath, "check", "show", "--json", "chk-flags")
	var d1, d2 api.CheckDetail
	if err := json.Unmarshal([]byte(out1), &d1); err != nil {
		t.Fatalf("failed to parse out1: %v\n%s", err, out1)
	}
	if err := json.Unmarshal([]byte(out2), &d2); err != nil {
		t.Fatalf("failed to parse out2: %v\n%s", err, out2)
	}
	// Zero out dynamic elapsed seconds before comparison
	d1.FreshnessSeconds, d2.FreshnessSeconds = 0, 0
	d1.StalenessSeconds, d2.StalenessSeconds = 0, 0
	if code1 != 0 || code2 != 0 || d1.ID != d2.ID || d1.Name != d2.Name || d1.URL != d2.URL {
		t.Errorf("flag ordering inconsistency: code1=%d code2=%d\nd1=%+v\nd2=%+v", code1, code2, d1, d2)
	}
}
