// Tests run inside the package so that they can reach the raw connections and
// assert things about the schema itself, which is as much the deliverable here
// as the Go code over it.
//
// Every test builds its own database in t.TempDir(). Nothing external is
// needed, nothing is shared between tests, and CI never has to provision a
// server.
package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "agentd.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// --- fixtures ---------------------------------------------------------------

func scalarCheck(t *testing.T, id domain.CheckID) *domain.Check {
	t.Helper()
	c, err := domain.NewCheck(id, domain.Definition{
		Intent: domain.ScalarIntent{
			Label:   "price",
			Purpose: "the advertised price of the standard plan",
			Type:    domain.TypeNumber,
		},
		Source: domain.SourceSpec{
			Kind:          domain.SourceHTTP,
			URL:           "https://example.test/pricing",
			SecretHeaders: map[string]domain.SecretRef{"Authorization": "api-token"},
		},
		Schedule:    domain.Schedule{Interval: time.Hour, Jitter: 0.1},
		Destination: domain.Destination{Kind: domain.DestinationNotify, Target: "ops@example.test"},
		Policy:      domain.Policy{MaxRepairAttempts: 2, RetainSnapshots: 5},
		CreatedAt:   base,
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}
	return c
}

func recordCheck(t *testing.T, id domain.CheckID) *domain.Check {
	t.Helper()
	c, err := domain.NewCheck(id, domain.Definition{
		Intent: domain.RecordIntent{
			Label:   "plan",
			Purpose: "the standard plan",
			Fields: []domain.Field{
				{Name: "price", Description: "monthly price", Type: domain.TypeNumber, Required: true},
				{Name: "seats", Description: "included seats", Type: domain.TypeNumber},
			},
		},
		Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
		Schedule:  domain.Schedule{Interval: time.Hour},
		CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}
	return c
}

func saveCheck(t *testing.T, s *Store, c *domain.Check) {
	t.Helper()
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveCheck(ctx, c)
	}); err != nil {
		t.Fatalf("SaveCheck: %v", err)
	}
}

func binding(checkID domain.CheckID, version int, origin domain.BindingOrigin) domain.Binding {
	return domain.Binding{
		ID:                domain.BindingID(fmt.Sprintf("bnd-%s-%d", checkID, version)),
		CheckID:           checkID,
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Fingerprint:       domain.SourceFingerprint(fmt.Sprintf("fp-v%d", version)),
		Version:           version,
		Origin:            origin,
		Locators:          []domain.Locator{{Target: "price", Dialect: "css", Expression: ".price"}},
		DerivedAt:         base,
		DerivedFrom:       "sha256:evidence",
	}
}

func snapshot(t *testing.T, checkID domain.CheckID, body string, offset time.Duration) domain.Snapshot {
	t.Helper()
	s, err := domain.NewSnapshot(checkID, "text/html", []byte(body), "fp-v1", base.Add(offset))
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	return s
}

func scalarResult(text string) domain.Extraction {
	return domain.Extraction{Kind: domain.IntentScalar, Scalar: domain.Value{Text: text, Type: domain.TypeNumber}}
}

// finishedRun builds a run that ran and ended quietly.
func finishedRun(t *testing.T, id domain.RunID, checkID domain.CheckID, slot domain.Slot, snap domain.SnapshotID) *domain.Run {
	t.Helper()
	r, err := domain.NewRun(id, checkID, slot, 1, base)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if err := r.Start(base.Add(time.Second), 1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Quiet(base.Add(2*time.Second), snap, scalarResult("49")); err != nil {
		t.Fatalf("Quiet: %v", err)
	}
	return r
}

// --- schema, migrations, WAL ------------------------------------------------

func TestOpenCreatesSchema(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	want := []string{
		"checks", "check_versions", "bindings", "runs", "snapshots", "incidents",
		"repair_candidates", "verification_results", "repair_decisions",
		"repair_diffs", "audit_events", "schema_migrations",
	}
	for _, table := range want {
		var name string
		err := s.reader.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q is missing: %v", table, err)
		}
	}

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version < 1 {
		t.Errorf("SchemaVersion = %d, want at least 1", version)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentd.db")

	first, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	saveCheck(t, first, scalarCheck(t, "chk-1"))
	v1, _ := first.SchemaVersion(ctx)
	first.Close()

	// Reopening must apply nothing and destroy nothing.
	second, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()

	v2, err := second.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v1 != v2 {
		t.Errorf("schema version moved from %d to %d on reopen", v1, v2)
	}
	if _, err := second.Check(ctx, "chk-1"); err != nil {
		t.Errorf("data did not survive reopening: %v", err)
	}

	var applied int
	if err := second.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("counting migrations: %v", err)
	}
	if applied != v2 {
		t.Errorf("%d migrations recorded but version is %d; one was applied twice", applied, v2)
	}
}

func TestMigrationFilenamesParse(t *testing.T) {
	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations are embedded; the binary would create an empty database")
	}
	for i, m := range all {
		if m.version != i+1 {
			t.Errorf("migration %d has version %d; versions must be contiguous from 1", i, m.version)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %04d is empty", m.version)
		}
	}
}

func TestWALIsEnabled(t *testing.T) {
	s := newStore(t)

	mode, err := s.JournalMode(context.Background())
	if err != nil {
		t.Fatalf("JournalMode: %v", err)
	}
	// Without WAL, readers and the writer serialise against each other and the
	// whole concurrency design collapses into a queue.
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal mode = %q, want wal", mode)
	}
}

// TestEveryTableCarriesTenantID and the index test below are the ones that
// keep multi-tenancy possible later. Adding the column afterwards would mean
// rebuilding every index in every deployed database.
func TestEveryTableCarriesTenantID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	rows, err := s.reader.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		if table == "schema_migrations" {
			// Bookkeeping about the database itself, not tenant data.
			continue
		}
		checked++

		cols, err := columnsOf(ctx, s, table)
		if err != nil {
			t.Fatalf("reading columns of %q: %v", table, err)
		}
		if !cols["tenant_id"] {
			t.Errorf("table %q has no tenant_id column", table)
		}
	}
	if checked < 11 {
		t.Errorf("only %d tenant tables were checked; the schema should have more", checked)
	}
}

func TestCompositeIndexesLeadWithTenantID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	rows, err := s.reader.QueryContext(ctx,
		`SELECT name, tbl_name FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("listing indexes: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			t.Fatal(err)
		}
		if table == "schema_migrations" {
			continue
		}

		first, err := firstIndexColumn(ctx, s, name)
		if err != nil {
			t.Fatalf("reading index %q: %v", name, err)
		}
		if first != "tenant_id" {
			t.Errorf("index %q on %q leads with %q, want tenant_id", name, table, first)
		}
	}
}

func columnsOf(ctx context.Context, s *Store, table string) (map[string]bool, error) {
	rows, err := s.reader.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func firstIndexColumn(ctx context.Context, s *Store, index string) (string, error) {
	rows, err := s.reader.QueryContext(ctx, fmt.Sprintf("PRAGMA index_info(%q)", index))
	if err != nil {
		return "", err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			seqno, cid int
			name       *string
		)
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return "", err
		}
		if seqno == 0 && name != nil {
			return *name, nil
		}
	}
	return "", rows.Err()
}

// --- transactions -----------------------------------------------------------

func TestTransactionRollsBackOnError(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := scalarCheck(t, "chk-1")

	boom := errors.New("something went wrong halfway through")
	err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveCheck(ctx, c); err != nil {
			return err
		}
		if err := tx.SaveBinding(ctx, binding("chk-1", 1, domain.OriginInferred)); err != nil {
			return err
		}
		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("WithTx returned %v, want the callers error", err)
	}
	// Both writes must be gone, not just the one after the failure.
	if _, err := s.Check(ctx, "chk-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("the check survived a rolled-back transaction: %v", err)
	}
	if _, err := s.ActiveBinding(ctx, "chk-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("the binding survived a rolled-back transaction: %v", err)
	}
}

func TestTransactionRollsBackOnPanic(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not propagate")
			}
		}()
		_ = s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
			if err := tx.SaveCheck(ctx, scalarCheck(t, "chk-1")); err != nil {
				return err
			}
			panic("mid-transaction")
		})
	}()

	if _, err := s.Check(ctx, "chk-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("a panicking transaction left data behind: %v", err)
	}
	// The single writer connection must still work; a transaction left open on
	// it would deadlock every later write.
	saveCheck(t, s, scalarCheck(t, "chk-2"))
}

func TestTransactionCommits(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	saveCheck(t, s, scalarCheck(t, "chk-1"))

	got, err := s.Check(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.ID() != "chk-1" {
		t.Errorf("ID = %q, want chk-1", got.ID())
	}
}

// --- run uniqueness ---------------------------------------------------------

// TestOneRunPerSlotIsEnforcedByTheDatabase is the point of the unique index.
// An application-level check can only report what was true a moment ago.
func TestOneRunPerSlotIsEnforcedByTheDatabase(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	first, err := domain.NewRun("run-1", "chk-1", 42, 1, base)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.CreateRun(ctx, first)
	}); err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}

	// A different run id, same slot. This is the shape of two runners racing.
	second, err := domain.NewRun("run-2", "chk-1", 42, 1, base)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	err = s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.CreateRun(ctx, second)
	})
	if !errors.Is(err, domain.ErrDuplicateRun) {
		t.Fatalf("second CreateRun returned %v, want ErrDuplicateRun", err)
	}

	var count int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE tenant_id = ? AND check_id = 'chk-1' AND slot = 42`,
		s.tenant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d rows for one slot, want 1", count)
	}
}

func TestConcurrentRunnersRacingForOneSlotProduceOneRun(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	const racers = 8
	var (
		wg        sync.WaitGroup
		succeeded atomic.Int32
		duplicate atomic.Int32
	)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r, err := domain.NewRun(domain.RunID(fmt.Sprintf("run-%d", n)), "chk-1", 7, 1, base)
			if err != nil {
				t.Errorf("NewRun: %v", err)
				return
			}
			<-start
			err = s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
				return tx.CreateRun(ctx, r)
			})
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, domain.ErrDuplicateRun):
				duplicate.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := succeeded.Load(); got != 1 {
		t.Errorf("%d runners claimed the slot, want exactly 1", got)
	}
	if got := duplicate.Load(); got != racers-1 {
		t.Errorf("%d runners were told the slot was taken, want %d", got, racers-1)
	}
}

func TestUpdateRunRefusesATerminalRun(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))
	snap := snapshot(t, "chk-1", "<html>49</html>", 0)

	r := finishedRun(t, "run-1", "chk-1", 1, snap.ID())
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.PutSnapshot(ctx, snap); err != nil {
			return err
		}
		return tx.CreateRun(ctx, r)
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// The row is already terminal, so an update from a stale copy -- including
	// one held by another process -- must be refused.
	err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.UpdateRun(ctx, r)
	})
	if !errors.Is(err, domain.ErrTerminal) {
		t.Fatalf("UpdateRun on a terminal run returned %v, want ErrTerminal", err)
	}
}

// --- snapshots --------------------------------------------------------------

func TestSnapshotsAreDeduplicatedByContent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	first := snapshot(t, "chk-1", "<html>the same bytes</html>", 0)
	later := snapshot(t, "chk-1", "<html>the same bytes</html>", time.Hour)
	other := snapshot(t, "chk-1", "<html>different bytes</html>", 2*time.Hour)

	if first.ID() != later.ID() {
		t.Fatal("identical bodies produced different content addresses")
	}

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		for _, snap := range []domain.Snapshot{first, later, other} {
			if err := tx.PutSnapshot(ctx, snap); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	var count int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM snapshots WHERE tenant_id = ? AND check_id = 'chk-1'`, s.tenant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("%d rows stored, want 2: the same capture twice should be one row", count)
	}
}

func TestSnapshotBodiesAreCompressedAndRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	// Repetitive, like a real page.
	body := strings.Repeat("<div class=\"row\"><span>49.00</span></div>\n", 400)
	snap := snapshot(t, "chk-1", body, 0)

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.PutSnapshot(ctx, snap)
	}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	var (
		compression string
		storedSize  int
		declared    int
	)
	if err := s.reader.QueryRowContext(ctx,
		`SELECT compression, LENGTH(body), size_bytes FROM snapshots WHERE tenant_id = ? AND id = ?`,
		s.tenant, string(snap.ID())).Scan(&compression, &storedSize, &declared); err != nil {
		t.Fatal(err)
	}

	if compression != compressionZstd {
		t.Errorf("compression = %q, want %q", compression, compressionZstd)
	}
	if storedSize >= len(body) {
		t.Errorf("stored %d bytes for a %d byte body; compression did nothing", storedSize, len(body))
	}
	if declared != len(body) {
		t.Errorf("size_bytes = %d, want the uncompressed %d", declared, len(body))
	}

	// The round trip has to be exact: the content address is a hash of these
	// bytes, and a capture that no longer matches its address is useless as
	// evidence.
	got, err := s.Snapshot(ctx, snap.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if string(got.Body()) != body {
		t.Error("the capture did not survive the round trip byte for byte")
	}
	if err := got.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestIncompressibleBodiesAreStoredAsIs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	// Short and high-entropy: zstd framing would make this bigger.
	snap := snapshot(t, "chk-1", "x7fQ", 0)
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.PutSnapshot(ctx, snap)
	}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	var compression string
	if err := s.reader.QueryRowContext(ctx,
		`SELECT compression FROM snapshots WHERE tenant_id = ? AND id = ?`,
		s.tenant, string(snap.ID())).Scan(&compression); err != nil {
		t.Fatal(err)
	}
	if compression != compressionNone {
		t.Errorf("compression = %q, want %q: storing a larger body would be silly", compression, compressionNone)
	}

	got, err := s.Snapshot(ctx, snap.ID())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if string(got.Body()) != "x7fQ" {
		t.Error("an uncompressed capture did not round trip")
	}
}

func TestReCapturingDoesNotDemoteAKnownGoodSnapshot(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))
	snap := snapshot(t, "chk-1", "<html>steady</html>", 0)

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.PutSnapshot(ctx, snap); err != nil {
			return err
		}
		return tx.MarkSnapshotKnownGood(ctx, snap.ID())
	}); err != nil {
		t.Fatalf("storing: %v", err)
	}

	// The same page again, from a run that has not yet succeeded. The insert
	// is a no-op, and must not reset known_good.
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.PutSnapshot(ctx, snapshot(t, "chk-1", "<html>steady</html>", time.Hour))
	}); err != nil {
		t.Fatalf("re-storing: %v", err)
	}

	index, err := s.Snapshots(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if _, err := index.LatestKnownGood(); err != nil {
		t.Errorf("the known-good flag was lost on re-capture: %v", err)
	}
}

func TestDeleteSnapshotsRefusesKnownGood(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))
	snap := snapshot(t, "chk-1", "<html>the only good one</html>", 0)

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.PutSnapshot(ctx, snap); err != nil {
			return err
		}
		return tx.MarkSnapshotKnownGood(ctx, snap.ID())
	}); err != nil {
		t.Fatalf("storing: %v", err)
	}

	// The store destroys data, so it does not take a caller's word for it.
	err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.DeleteSnapshots(ctx, []domain.SnapshotID{snap.ID()})
	})
	if err == nil {
		t.Fatal("DeleteSnapshots removed a known-good capture")
	}
	if _, err := s.Snapshot(ctx, snap.ID()); err != nil {
		t.Errorf("the known-good capture is gone: %v", err)
	}
}

// --- retention --------------------------------------------------------------

func TestGCKeepsTheLastKnownGoodSnapshot(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	good := snapshot(t, "chk-1", "<html>the good one</html>", 0)
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.PutSnapshot(ctx, good); err != nil {
			return err
		}
		return tx.MarkSnapshotKnownGood(ctx, good.ID())
	}); err != nil {
		t.Fatalf("storing: %v", err)
	}

	// Six newer, broken captures. Keeping only the newest two would evict the
	// one capture that makes a future repair verifiable.
	for i := 1; i <= 6; i++ {
		snap := snapshot(t, "chk-1", fmt.Sprintf("<html>broken %d</html>", i), time.Duration(i)*time.Hour)
		if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
			return tx.PutSnapshot(ctx, snap)
		}); err != nil {
			t.Fatalf("storing: %v", err)
		}
	}

	policy := domain.DefaultRetention()
	policy.SnapshotsPerCheck = 2

	sweep, err := s.GC(ctx, policy, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if sweep.Snapshots == 0 {
		t.Error("GC removed no captures")
	}

	index, err := s.Snapshots(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if _, err := index.LatestKnownGood(); err != nil {
		t.Fatalf("GC deleted the last known-good capture: %v", err)
	}
	if got := index.Len(); got != 3 {
		t.Errorf("Len = %d, want 3: two retained plus the protected known-good", got)
	}
}

func TestGCRemovesOldRunsButNotUnfinishedOnes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))
	snap := snapshot(t, "chk-1", "<html>49</html>", 0)

	old := finishedRun(t, "run-old", "chk-1", 1, snap.ID())
	recent, err := domain.NewRun("run-pending", "chk-1", 2, 1, base)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.PutSnapshot(ctx, snap); err != nil {
			return err
		}
		if err := tx.CreateRun(ctx, old); err != nil {
			return err
		}
		return tx.CreateRun(ctx, recent)
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	sweep, err := s.GC(ctx, domain.DefaultRetention(), base.Add(200*24*time.Hour))
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if sweep.Runs != 1 {
		t.Errorf("GC removed %d runs, want 1", sweep.Runs)
	}

	if _, err := s.Run(ctx, "run-old"); !errors.Is(err, ports.ErrNotFound) {
		t.Error("the expired run survived")
	}
	// A pending run that is two hundred days old is a bug worth seeing, not
	// litter to sweep away.
	if _, err := s.Run(ctx, "run-pending"); err != nil {
		t.Errorf("GC deleted an unfinished run: %v", err)
	}
}

func TestGCRemovesClosedIncidentsButNotOpenOnes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))
	saveCheck(t, s, scalarCheck(t, "chk-2"))

	closed := openIncident(t, "inc-closed", "chk-1")
	if err := closed.Abandon("gave up", base.Add(time.Hour)); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	open := openIncident(t, "inc-open", "chk-2")

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveIncident(ctx, closed); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, open)
	}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}

	sweep, err := s.GC(ctx, domain.DefaultRetention(), base.Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if sweep.Incidents != 1 {
		t.Errorf("GC removed %d incidents, want 1", sweep.Incidents)
	}

	// Deleting an open incident would silently withdraw a question Agentd has
	// put to a human, which from outside looks just like an answered one.
	log, err := s.Incidents(ctx, "chk-2")
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if _, stillOpen := log.Current(); !stillOpen {
		t.Error("GC deleted an open incident")
	}
}

func TestGCRefusesAnAbsurdPolicy(t *testing.T) {
	s := newStore(t)

	// "Keep nothing" is never what anyone meant to configure.
	if _, err := s.GC(context.Background(), domain.Retention{}, base); err == nil {
		t.Error("GC accepted a zero retention policy")
	}
}

// --- concurrency ------------------------------------------------------------

// TestConcurrentReadsDoNotBlock exercises the reader pool. In WAL mode these
// should all proceed together; the assertion is simply that they all succeed
// with correct data and nothing deadlocks.
func TestConcurrentReadsDoNotBlock(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers)

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				c, err := s.Check(ctx, "chk-1")
				if err != nil {
					errs <- err
					return
				}
				if c.ID() != "chk-1" {
					errs <- fmt.Errorf("read the wrong check: %q", c.ID())
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent read failed: %v", err)
	}
}

// TestReadsProceedDuringWrites is the property the two-pool design exists for:
// a long write must not stop readers. Without WAL this test would hang or fail.
func TestReadsProceedDuringWrites(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	writing := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
			if err := tx.SaveCheck(ctx, scalarCheck(t, "chk-2")); err != nil {
				return err
			}
			close(writing)
			<-release // hold the write transaction open
			return nil
		})
	}()

	<-writing
	// The writer is mid-transaction. Reads must still work.
	for i := 0; i < 5; i++ {
		if _, err := s.Check(ctx, "chk-1"); err != nil {
			close(release)
			t.Fatalf("a read blocked behind an open write transaction: %v", err)
		}
	}
	// And must not see the uncommitted write.
	if _, err := s.Check(ctx, "chk-2"); !errors.Is(err, ports.ErrNotFound) {
		close(release)
		t.Fatalf("a read saw an uncommitted write: %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	if _, err := s.Check(ctx, "chk-2"); err != nil {
		t.Errorf("the committed write is not visible: %v", err)
	}
}

// TestOnlyOneWriterAtATime checks the discipline rather than its consequences:
// the writer pool has one connection, so two transactions can never overlap.
// That is what makes SQLITE_BUSY an architectural impossibility here rather
// than something to retry around.
func TestOnlyOneWriterAtATime(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	var (
		inFlight atomic.Int32
		maxSeen  atomic.Int32
		wg       sync.WaitGroup
	)

	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
				now := inFlight.Add(1)
				for {
					seen := maxSeen.Load()
					if now <= seen || maxSeen.CompareAndSwap(seen, now) {
						break
					}
				}
				defer inFlight.Add(-1)

				// Hold the transaction long enough that an overlap would be
				// observed if one were possible.
				time.Sleep(2 * time.Millisecond)
				return tx.SaveCheck(ctx, scalarCheck(t, domain.CheckID(fmt.Sprintf("chk-%d", n))))
			})
			if err != nil {
				t.Errorf("write %d failed: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	if got := maxSeen.Load(); got != 1 {
		t.Errorf("%d write transactions were in flight at once, want 1", got)
	}

	var count int
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM checks WHERE tenant_id = ?`, s.tenant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 12 {
		t.Errorf("%d checks stored, want 12; writes were lost", count)
	}
}

// --- persistence round trips ------------------------------------------------

func TestCheckRoundTripsWithEveryVersion(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	c := scalarCheck(t, "chk-1")
	if _, err := c.Revise(recordCheck(t, "chk-1").ActiveDefinition()); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	if _, err := c.Revise(scalarCheck(t, "chk-1").ActiveDefinition()); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	if err := c.Activate(2); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	c.Disable()
	saveCheck(t, s, c)

	got, err := s.Check(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if got.ActiveVersion() != 2 {
		t.Errorf("ActiveVersion = %d, want 2", got.ActiveVersion())
	}
	if got.Enabled() {
		t.Error("the check came back enabled")
	}
	if n := len(got.Versions()); n != 3 {
		t.Errorf("%d versions survived, want 3; a past run's explanation depends on them", n)
	}
	if got.Intent().Kind() != domain.IntentRecord {
		t.Errorf("active intent kind = %q, want %q", got.Intent().Kind(), domain.IntentRecord)
	}
	if err := got.CheckInvariants(); err != nil {
		t.Errorf("reloaded check violates its invariants: %v", err)
	}

	// The source spec, including which secrets it needs, has to survive on
	// every version and not just the active one -- but note the secret values
	// themselves were never here to survive.
	//
	// Version 1 is checked rather than the active version, which is the record
	// definition and deliberately has no secrets: an old version has to come
	// back intact or a past run cannot be explained.
	first, err := got.Definition(1)
	if err != nil {
		t.Fatalf("Definition(1): %v", err)
	}
	if ref := first.Source.SecretHeaders["Authorization"]; ref != "api-token" {
		t.Errorf("secret reference on version 1 = %q, want api-token", ref)
	}
	if first.Source.URL != "https://example.test/pricing" {
		t.Errorf("version 1 URL = %q, want the stored one", first.Source.URL)
	}
	if first.Schedule.Jitter != 0.1 {
		t.Errorf("version 1 jitter = %v, want 0.1", first.Schedule.Jitter)
	}
}

func TestEveryIntentKindRoundTrips(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	intents := []domain.Intent{
		domain.ScalarIntent{Label: "price", Purpose: "the price", Type: domain.TypeNumber},
		domain.RecordIntent{Label: "plan", Purpose: "the plan", Fields: []domain.Field{
			{Name: "price", Description: "monthly", Type: domain.TypeNumber, Required: true},
		}},
		domain.CollectionIntent{Label: "plans", Purpose: "every plan", MinItems: 1,
			Element: domain.RecordIntent{Label: "plan", Purpose: "one plan", Fields: []domain.Field{
				{Name: "price", Description: "monthly", Type: domain.TypeNumber, Required: true},
			}}},
	}

	for i, in := range intents {
		t.Run(string(in.Kind()), func(t *testing.T) {
			id := domain.CheckID(fmt.Sprintf("chk-%d", i))
			c, err := domain.NewCheck(id, domain.Definition{
				Intent:    in,
				Source:    domain.SourceSpec{Kind: domain.SourceHTTP, URL: "https://example.test"},
				Schedule:  domain.Schedule{Interval: time.Hour},
				CreatedAt: base,
			})
			if err != nil {
				t.Fatalf("NewCheck: %v", err)
			}
			saveCheck(t, s, c)

			got, err := s.Check(ctx, id)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			// Intent is an interface, so the kind column is what tells the
			// decoder which concrete type to build. Getting this wrong would
			// silently turn a collection into a record.
			if got.Intent().Kind() != in.Kind() {
				t.Fatalf("kind = %q, want %q", got.Intent().Kind(), in.Kind())
			}
			if got.Intent().Description() != in.Description() {
				t.Errorf("description = %q, want %q", got.Intent().Description(), in.Description())
			}
			if err := got.Intent().Validate(); err != nil {
				t.Errorf("the reloaded intent is invalid: %v", err)
			}
		})
	}
}

func TestRunRoundTripsInEveryTerminalState(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))
	snap := snapshot(t, "chk-1", "<html>49</html>", 0)
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.PutSnapshot(ctx, snap)
	}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	failure := domain.Failure{Class: domain.ClassStructural, Code: "missing_required", Summary: "the price moved"}

	cases := []struct {
		state domain.RunState
		drive func(*domain.Run) error
	}{
		{domain.StateQuiet, func(r *domain.Run) error {
			return r.Quiet(base.Add(2*time.Second), snap.ID(), scalarResult("49"))
		}},
		{domain.StateChanged, func(r *domain.Run) error {
			return r.Changed(base.Add(2*time.Second), snap.ID(), scalarResult("59"), "the price went up")
		}},
		{domain.StateDegraded, func(r *domain.Run) error {
			return r.Degrade(base.Add(2*time.Second), snap.ID(), scalarResult("49"),
				domain.Failure{Class: domain.ClassSemantic, Summary: "the seat count was missing"})
		}},
		{domain.StateFailed, func(r *domain.Run) error { return r.Fail(base.Add(2*time.Second), failure) }},
		{domain.StateInterrupted, func(r *domain.Run) error {
			return r.Interrupt(base.Add(2*time.Second), "agentd was shutting down")
		}},
		{domain.StateSkippedOverload, func(r *domain.Run) error {
			return r.SkipOverloaded(base.Add(2*time.Second), "the runner was at capacity")
		}},
	}

	for i, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			id := domain.RunID(fmt.Sprintf("run-%d", i))
			r, err := domain.NewRun(id, "chk-1", domain.Slot(100+i), 1, base)
			if err != nil {
				t.Fatalf("NewRun: %v", err)
			}
			if tc.state != domain.StateSkippedOverload && tc.state != domain.StateInterrupted {
				if err := r.Start(base.Add(time.Second), 1); err != nil {
					t.Fatalf("Start: %v", err)
				}
			}
			if err := tc.drive(r); err != nil {
				t.Fatalf("driving to %q: %v", tc.state, err)
			}

			if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
				return tx.CreateRun(ctx, r)
			}); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}

			got, err := s.Run(ctx, id)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.State() != tc.state {
				t.Errorf("State = %q, want %q", got.State(), tc.state)
			}
			if !got.EndedAt().Equal(r.EndedAt()) {
				t.Errorf("EndedAt = %v, want %v", got.EndedAt(), r.EndedAt())
			}
			if got.Explanation() != r.Explanation() {
				t.Errorf("Explanation = %q, want %q", got.Explanation(), r.Explanation())
			}

			// Degraded and failed must come back distinguishable, with their
			// classification intact: the whole failure taxonomy depends on it.
			switch tc.state {
			case domain.StateDegraded, domain.StateFailed:
				f := got.Failure()
				if f == nil {
					t.Fatal("the classified failure did not survive")
				}
				if f.Class != r.Failure().Class {
					t.Errorf("Class = %q, want %q", f.Class, r.Failure().Class)
				}
			}
			if err := got.CheckInvariants(); err != nil {
				t.Errorf("the reloaded run violates its invariants: %v", err)
			}
		})
	}
}

func TestLastResultFindsTheMostRecentSuccess(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))
	snap := snapshot(t, "chk-1", "<html>49</html>", 0)

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.PutSnapshot(ctx, snap)
	}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	if _, err := s.LastResult(ctx, "chk-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("LastResult on a check that has never run returned %v, want ErrNotFound", err)
	}

	// An older success, then a newer one, then a failure. The failure must not
	// become the comparison baseline.
	older, _ := domain.NewRun("run-1", "chk-1", 1, 1, base)
	older.Start(base.Add(time.Second), 1)
	older.Quiet(base.Add(2*time.Second), snap.ID(), scalarResult("39"))

	newer, _ := domain.NewRun("run-2", "chk-1", 2, 1, base.Add(time.Hour))
	newer.Start(base.Add(time.Hour), 1)
	newer.Changed(base.Add(time.Hour+time.Second), snap.ID(), scalarResult("49"), "went up")

	broken, _ := domain.NewRun("run-3", "chk-1", 3, 1, base.Add(2*time.Hour))
	broken.Start(base.Add(2*time.Hour), 1)
	broken.Fail(base.Add(2*time.Hour+time.Second),
		domain.Failure{Class: domain.ClassStructural, Summary: "the price moved"})

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		for _, r := range []*domain.Run{older, newer, broken} {
			if err := tx.CreateRun(ctx, r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := s.LastResult(ctx, "chk-1")
	if err != nil {
		t.Fatalf("LastResult: %v", err)
	}
	if got.Scalar.Text != "49" {
		t.Errorf("LastResult = %q, want 49 from the most recent successful run", got.Scalar.Text)
	}
}

func TestBindingRoundTripsAndActivation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	b := binding("chk-1", 1, domain.OriginInferred)
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveBinding(ctx, b)
	}); err != nil {
		t.Fatalf("SaveBinding: %v", err)
	}

	// Saving is not activating.
	if _, err := s.ActiveBinding(ctx, "chk-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("a saved binding was active without being activated: %v", err)
	}

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.ActivateBinding(ctx, "chk-1", 1)
	}); err != nil {
		t.Fatalf("ActivateBinding: %v", err)
	}

	got, err := s.ActiveBinding(ctx, "chk-1")
	if err != nil {
		t.Fatalf("ActiveBinding: %v", err)
	}
	if got.ID != b.ID || got.Version != 1 {
		t.Errorf("ActiveBinding = %+v, want %+v", got, b)
	}
	if got.Fingerprint != b.Fingerprint {
		t.Errorf("Fingerprint = %q, want %q", got.Fingerprint, b.Fingerprint)
	}
	if len(got.Locators) != 1 || got.Locators[0].Expression != ".price" {
		t.Errorf("Locators = %+v, want the stored one", got.Locators)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the reloaded binding is invalid: %v", err)
	}
}

// TestRepairedBindingCannotBeActivatedWithoutAnApproval is the product's
// central promise, checked at the last point before a change becomes real.
func TestRepairedBindingCannotBeActivatedWithoutAnApproval(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	repaired := binding("chk-1", 2, domain.OriginRepaired)
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveBinding(ctx, repaired)
	}); err != nil {
		t.Fatalf("SaveBinding: %v", err)
	}

	err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.ActivateBinding(ctx, "chk-1", 2)
	})
	if !errors.Is(err, ErrNotApprovedForActivation) {
		t.Fatalf("ActivateBinding returned %v, want ErrNotApprovedForActivation", err)
	}
	if _, err := s.ActiveBinding(ctx, "chk-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Error("the repaired binding became active anyway")
	}
}

func TestRepairedBindingActivatesOnceApproved(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	repaired := binding("chk-1", 2, domain.OriginRepaired)
	inc := openIncident(t, "inc-1", "chk-1")
	if _, err := inc.RecordAttempt(domain.AttemptProposed, "found a candidate", base.Add(time.Minute)); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	if err := inc.Propose(domain.RepairProposal{
		Binding:         repaired,
		Rationale:       "the price moved into a new container",
		VerifiedAgainst: "sha256:current",
		VerifiedAt:      base.Add(time.Minute),
	}, base.Add(time.Minute)); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if err := inc.Approve("dana", base.Add(time.Hour)); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, repaired); err != nil {
			return err
		}
		if err := tx.SaveIncident(ctx, inc); err != nil {
			return err
		}
		return tx.ActivateBinding(ctx, "chk-1", 2)
	}); err != nil {
		t.Fatalf("activating an approved repair: %v", err)
	}

	got, err := s.ActiveBinding(ctx, "chk-1")
	if err != nil {
		t.Fatalf("ActiveBinding: %v", err)
	}
	if got.Version != 2 || got.Origin != domain.OriginRepaired {
		t.Errorf("ActiveBinding = %+v, want the approved repair", got)
	}
}

func openIncident(t *testing.T, id domain.IncidentID, checkID domain.CheckID) *domain.Incident {
	t.Helper()
	log := domain.NewIncidentLog(checkID)
	i, err := log.Open(id, domain.Failure{
		Class:   domain.ClassStructural,
		Code:    "missing_required",
		Summary: "the price is no longer where this check expects it",
	}, 2, base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return i
}

func TestIncidentRoundTripsWithItsRepairHistory(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	inc := openIncident(t, "inc-1", "chk-1")
	if _, err := inc.RecordAttempt(domain.AttemptUnverified, "the first candidate did not reproduce the intent", base.Add(time.Minute)); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	if _, err := inc.RecordAttempt(domain.AttemptProposed, "a candidate was found and checked", base.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}

	repaired := binding("chk-1", 2, domain.OriginRepaired)
	if err := inc.Propose(domain.RepairProposal{
		Binding:         repaired,
		Rationale:       "the price moved into a span with a data-price attribute",
		VerifiedAgainst: "sha256:current",
		VerifiedAt:      base.Add(2 * time.Minute),
	}, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, repaired); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, inc)
	}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}

	log, err := s.Incidents(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	got, open := log.Current()
	if !open {
		t.Fatal("the open incident did not come back")
	}

	if got.State() != domain.IncidentAwaitingApproval {
		t.Errorf("State = %q, want %q", got.State(), domain.IncidentAwaitingApproval)
	}
	if got.Cause().Class != domain.ClassStructural {
		t.Errorf("Cause class = %q, want structural", got.Cause().Class)
	}
	// The failed attempt is kept on purpose: it is what an operator reads when
	// deciding how much to trust the proposal that did survive.
	if n := len(got.Attempts()); n != 2 {
		t.Fatalf("%d attempts survived, want 2", n)
	}
	if got.Attempts()[0].Outcome != domain.AttemptUnverified {
		t.Errorf("first attempt outcome = %q, want %q", got.Attempts()[0].Outcome, domain.AttemptUnverified)
	}

	p := got.Proposal()
	if p == nil {
		t.Fatal("the proposal did not survive")
	}
	if p.Approval != domain.ApprovalPending {
		t.Errorf("Approval = %q, want %q: reloading must not approve anything", p.Approval, domain.ApprovalPending)
	}
	if p.Binding.ID != repaired.ID || p.Binding.Origin != domain.OriginRepaired {
		t.Errorf("proposal binding = %+v, want %+v", p.Binding, repaired)
	}
	if p.Rationale == "" || p.VerifiedAgainst != "sha256:current" {
		t.Errorf("verification provenance was lost: %+v", p)
	}
	if _, err := got.ApprovedBinding(); !errors.Is(err, domain.ErrNotApproved) {
		t.Errorf("a reloaded pending proposal yielded a binding: %v", err)
	}
}

func TestApprovalSurvivesReload(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	inc := openIncident(t, "inc-1", "chk-1")
	inc.RecordAttempt(domain.AttemptProposed, "found one", base.Add(time.Minute))
	repaired := binding("chk-1", 2, domain.OriginRepaired)
	if err := inc.Propose(domain.RepairProposal{
		Binding: repaired, Rationale: "it moved",
		VerifiedAgainst: "sha256:current", VerifiedAt: base.Add(time.Minute),
	}, base.Add(time.Minute)); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if err := inc.Approve("dana", base.Add(time.Hour)); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, repaired); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, inc)
	}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}

	log, err := s.Incidents(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	got, _ := log.Current()

	p := got.Proposal()
	if p.Approval != domain.ApprovalApproved {
		t.Fatalf("Approval = %q, want %q", p.Approval, domain.ApprovalApproved)
	}
	// Who approved it is the point. An approval with nobody attached is
	// indistinguishable from Agentd approving its own work.
	if p.ApprovedBy != "dana" {
		t.Errorf("ApprovedBy = %q, want dana", p.ApprovedBy)
	}
	if _, err := got.ApprovedBinding(); err != nil {
		t.Errorf("the approved binding is not available after reload: %v", err)
	}
}

func TestOneOpenIncidentPerCheckIsEnforcedByTheDatabase(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	first := openIncident(t, "inc-1", "chk-1")
	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveIncident(ctx, first)
	}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}

	// A second process that also saw the breakage. The partial unique index is
	// what stops both from succeeding.
	second := openIncident(t, "inc-2", "chk-1")
	err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.SaveIncident(ctx, second)
	})
	if !errors.Is(err, domain.ErrIncidentOpen) {
		t.Fatalf("saving a second open incident returned %v, want ErrIncidentOpen", err)
	}
}

func TestSnapshotIndexRoundTrips(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	good := snapshot(t, "chk-1", "<html>good</html>", 0)
	other := snapshot(t, "chk-1", "<html>later</html>", time.Hour)

	if err := s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.PutSnapshot(ctx, good); err != nil {
			return err
		}
		if err := tx.PutSnapshot(ctx, other); err != nil {
			return err
		}
		return tx.MarkSnapshotKnownGood(ctx, good.ID())
	}); err != nil {
		t.Fatalf("storing: %v", err)
	}

	index, err := s.Snapshots(ctx, "chk-1")
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if index.Len() != 2 {
		t.Errorf("Len = %d, want 2", index.Len())
	}

	kg, err := index.LatestKnownGood()
	if err != nil {
		t.Fatalf("LatestKnownGood: %v", err)
	}
	if kg.ID() != good.ID() {
		t.Errorf("LatestKnownGood = %q, want %q", kg.ID(), good.ID())
	}
	if err := index.CheckInvariants(true); err != nil {
		t.Errorf("the reloaded index violates its invariants: %v", err)
	}
}

// --- audit and repair records ------------------------------------------------

func TestAuditTrailRoundTrips(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.WithTx(ctx, func(ctx context.Context, t2 ports.Tx) error {
		return t2.(tx).AppendAudit(ctx, AuditEvent{
			ID: "evt-1", At: base, Actor: "dana", Action: "approve_repair",
			SubjectKind: "incident", SubjectID: "inc-1", Detail: "approved version 2",
		})
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}

	events, err := s.Audit(ctx, 10)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d events, want 1", len(events))
	}
	if events[0].Actor != "dana" || events[0].Action != "approve_repair" {
		t.Errorf("event = %+v, want the one that was written", events[0])
	}
	if !events[0].At.Equal(base) {
		t.Errorf("At = %v, want %v", events[0].At, base)
	}
}

func TestEditedThenApprovedRepairKeepsBothSides(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	inc := openIncident(t, "inc-1", "chk-1")
	inc.RecordAttempt(domain.AttemptProposed, "found one", base.Add(time.Minute))
	if err := s.WithTx(ctx, func(ctx context.Context, tx2 ports.Tx) error {
		if err := tx2.SaveIncident(ctx, inc); err != nil {
			return err
		}
		return tx2.(tx).RecordRepairEdit(ctx, RepairEdit{
			ID: "diff-1", IncidentID: "inc-1", AttemptNumber: 1,
			Proposed: []domain.Locator{{Target: "price", Dialect: "css", Expression: "[data-price]"}},
			Approved: []domain.Locator{{Target: "price", Dialect: "css", Expression: ".plan .price"}},
			EditedBy: "dana", EditedAt: base.Add(time.Hour),
		})
	}); err != nil {
		t.Fatalf("RecordRepairEdit: %v", err)
	}

	var proposed, approved, by string
	if err := s.reader.QueryRowContext(ctx,
		`SELECT proposed_locators_json, approved_locators_json, edited_by
		   FROM repair_diffs WHERE tenant_id = ? AND id = 'diff-1'`, s.tenant).
		Scan(&proposed, &approved, &by); err != nil {
		t.Fatalf("reading the diff: %v", err)
	}

	// Both sides, because what Agentd suggested and what the human approved
	// are different facts, and the difference is the useful part.
	if !strings.Contains(proposed, "data-price") {
		t.Errorf("the proposal was not kept: %s", proposed)
	}
	if !strings.Contains(approved, ".plan .price") {
		t.Errorf("the approved version was not kept: %s", approved)
	}
	if by != "dana" {
		t.Errorf("edited_by = %q, want dana", by)
	}
}

func TestRepairEditMustNameTheEditor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	err := s.WithTx(ctx, func(ctx context.Context, t2 ports.Tx) error {
		return t2.(tx).RecordRepairEdit(ctx, RepairEdit{ID: "diff-1", IncidentID: "inc-1"})
	})
	if err == nil {
		t.Error("an anonymous edit was recorded")
	}
}

// --- backup -----------------------------------------------------------------

// TestBackupIsConsistentUnderLoad is the reason Backup uses VACUUM INTO rather
// than copying the file. In WAL mode the main file is not the database: recent
// commits live in the -wal sidecar, so a naive copy taken while writes are in
// flight opens fine and is missing data.
func TestBackupIsConsistentUnderLoad(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
				return tx.SaveCheck(ctx, scalarCheck(t, domain.CheckID(fmt.Sprintf("chk-%d", i))))
			})
		}
	}()

	// Let some writes land before taking the backup.
	time.Sleep(20 * time.Millisecond)

	dest := filepath.Join(t.TempDir(), "backup.db")
	size, err := s.Backup(ctx, dest)

	close(stop)
	<-done

	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if size <= 0 {
		t.Errorf("the backup is %d bytes", size)
	}

	version, err := VerifyBackup(ctx, dest)
	if err != nil {
		t.Fatalf("the backup is not a usable database: %v", err)
	}
	current, _ := s.SchemaVersion(ctx)
	if version != current {
		t.Errorf("the backup is at schema %d, the live database at %d", version, current)
	}
}

func TestBackupContainsCommittedData(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	dest := filepath.Join(t.TempDir(), "backup.db")
	if _, err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	restored, err := Open(ctx, Options{Path: dest})
	if err != nil {
		t.Fatalf("opening the backup: %v", err)
	}
	defer restored.Close()

	got, err := restored.Check(ctx, "chk-1")
	if err != nil {
		t.Fatalf("the backup is missing committed data: %v", err)
	}
	if got.ID() != "chk-1" {
		t.Errorf("ID = %q, want chk-1", got.ID())
	}
}

func TestBackupRefusesToOverwrite(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "backup.db")

	if _, err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	if _, err := s.Backup(ctx, dest); !errors.Is(err, ErrBackupExists) {
		t.Errorf("second Backup returned %v, want ErrBackupExists", err)
	}
}

func TestCheckpointRuns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	saveCheck(t, s, scalarCheck(t, "chk-1"))

	if err := s.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// The database must still be usable afterwards.
	if _, err := s.Check(ctx, "chk-1"); err != nil {
		t.Errorf("the database is unusable after a checkpoint: %v", err)
	}
}

// --- misc -------------------------------------------------------------------

func TestOpenRequiresAPath(t *testing.T) {
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Error("Open accepted an empty path")
	}
}

func TestTenantScopingIsolatesRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agentd.db")

	one, err := Open(ctx, Options{Path: path, Tenant: "tenant-a"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer one.Close()
	saveCheck(t, one, scalarCheck(t, "chk-1"))

	two, err := Open(ctx, Options{Path: path, Tenant: "tenant-b"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer two.Close()

	// Same file, same check id, different tenant. v1 only ever uses one
	// tenant, but the scoping has to actually work or the column is decoration.
	if _, err := two.Check(ctx, "chk-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("a check leaked across tenants: %v", err)
	}
	if _, err := one.Check(ctx, "chk-1"); err != nil {
		t.Errorf("the check is missing from its own tenant: %v", err)
	}
}

func TestDefaultTenantIsUsedWhenUnset(t *testing.T) {
	s := newStore(t)
	if s.Tenant() != DefaultTenant {
		t.Errorf("Tenant = %q, want %q", s.Tenant(), DefaultTenant)
	}

	saveCheck(t, s, scalarCheck(t, "chk-1"))
	var tenant string
	if err := s.reader.QueryRowContext(context.Background(),
		`SELECT tenant_id FROM checks WHERE id = 'chk-1'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if tenant != DefaultTenant {
		t.Errorf("stored tenant_id = %q, want %q", tenant, DefaultTenant)
	}
}
