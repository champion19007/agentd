package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Transaction boundaries.
//
// A transaction is a closure, not a pair of handles. WithTx begins, runs fn,
// and commits or rolls back; there is no way for a caller to hold an open
// transaction past the call, forget to close one, or leak one through a panic.
// Every mutation goes through it, so "a run state transition is transactional"
// is a property of the only available API rather than a convention.
//
// Reads outside a transaction go to the reader pool and are individually
// consistent. A caller needing several reads to agree with each other uses
// View, which takes a read transaction so they see one snapshot of the
// database.

// execer is the part of *sql.DB and *sql.Tx that queries need. Writing against
// it is what lets the same query code serve both a pooled connection and a
// transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// q carries an executor and the tenant every statement is scoped to.
type q struct {
	db     execer
	tenant string
}

// tx satisfies ports.Tx. It is q with both halves of the interface.
type tx struct{ q }

var (
	_ ports.Store = (*Store)(nil)
	_ ports.Tx    = tx{}
)

// WithTx runs fn inside a write transaction on the single writer connection.
//
// It commits when fn returns nil and rolls back otherwise, including on panic.
// The rollback on panic matters more than it looks: a panic mid-transaction
// that left the connection with an open transaction would poison the one
// writer connection for the life of the process.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx ports.Tx) error) (err error) {
	sqlTx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: beginning transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			// Rollback's error is deliberately dropped: the caller's error, or
			// the panic, is the thing they need to see.
			_ = sqlTx.Rollback()
		}
	}()

	if err := fn(ctx, tx{q{db: sqlTx, tenant: s.tenant}}); err != nil {
		return err
	}

	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("sqlite: committing: %w", err)
	}
	committed = true
	return nil
}

// Update satisfies ports.Store. It is WithTx under the name the core knows.
func (s *Store) Update(ctx context.Context, fn func(ctx context.Context, tx ports.Tx) error) error {
	return s.WithTx(ctx, fn)
}

// View runs fn in a read-only transaction on the reader pool, for reads that
// must agree with one another.
func (s *Store) View(ctx context.Context, fn func(ctx context.Context, r ports.Reader) error) error {
	sqlTx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("sqlite: beginning read transaction: %w", err)
	}
	defer sqlTx.Rollback()

	return fn(ctx, q{db: sqlTx, tenant: s.tenant})
}

// reads returns a query helper over the reader pool.
func (s *Store) reads() q { return q{db: s.reader, tenant: s.tenant} }

// --- Store's Reader methods -------------------------------------------------
//
// These forward to the reader pool so that an ordinary read never touches the
// writer connection and therefore never waits behind a write.

func (s *Store) Check(ctx context.Context, id domain.CheckID) (*domain.Check, error) {
	return s.reads().Check(ctx, id)
}

func (s *Store) EnabledChecks(ctx context.Context) ([]*domain.Check, error) {
	return s.reads().EnabledChecks(ctx)
}

func (s *Store) Run(ctx context.Context, id domain.RunID) (*domain.Run, error) {
	return s.reads().Run(ctx, id)
}

func (s *Store) RunForSlot(ctx context.Context, id domain.CheckID, slot domain.Slot) (*domain.Run, error) {
	return s.reads().RunForSlot(ctx, id, slot)
}

func (s *Store) RecentRuns(ctx context.Context, id domain.CheckID, limit int) ([]*domain.Run, error) {
	return s.reads().RecentRuns(ctx, id, limit)
}

func (s *Store) LastResult(ctx context.Context, id domain.CheckID) (domain.Extraction, error) {
	return s.reads().LastResult(ctx, id)
}

func (s *Store) ActiveBinding(ctx context.Context, id domain.CheckID) (domain.Binding, error) {
	return s.reads().ActiveBinding(ctx, id)
}

func (s *Store) Snapshot(ctx context.Context, id domain.SnapshotID) (domain.Snapshot, error) {
	return s.reads().Snapshot(ctx, id)
}

func (s *Store) Snapshots(ctx context.Context, id domain.CheckID) (*domain.SnapshotIndex, error) {
	return s.reads().Snapshots(ctx, id)
}

func (s *Store) Incidents(ctx context.Context, id domain.CheckID) (*domain.IncidentLog, error) {
	return s.reads().Incidents(ctx, id)
}

// --- checks -----------------------------------------------------------------

func (x q) Check(ctx context.Context, id domain.CheckID) (*domain.Check, error) {
	var (
		enabled int
		active  int
	)
	err := x.db.QueryRowContext(ctx,
		`SELECT enabled, active_version FROM checks WHERE tenant_id = ? AND id = ?`,
		x.tenant, string(id),
	).Scan(&enabled, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading check %q: %w", id, err)
	}

	defs, err := x.definitions(ctx, id)
	if err != nil {
		return nil, err
	}
	return domain.RestoreCheck(domain.RestoredCheck{
		ID:          id,
		Definitions: defs,
		Active:      active,
		Enabled:     enabled == 1,
	})
}

// definitions reads a check's versions, oldest first.
func (x q) definitions(ctx context.Context, id domain.CheckID) ([]domain.Definition, error) {
	rows, err := x.db.QueryContext(ctx, `
		SELECT version, intent_kind, intent_json, source_json, schedule_json,
		       destination_json, policy_json, created_at
		  FROM check_versions
		 WHERE tenant_id = ? AND check_id = ?
		 ORDER BY version ASC`, x.tenant, string(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading versions of check %q: %w", id, err)
	}
	defer rows.Close()

	var out []domain.Definition
	for rows.Next() {
		var (
			d                                       domain.Definition
			kind, intentJSON, sourceJSON, schedJSON string
			destJSON, policyJSON, createdAt         string
		)
		if err := rows.Scan(&d.Version, &kind, &intentJSON, &sourceJSON, &schedJSON,
			&destJSON, &policyJSON, &createdAt); err != nil {
			return nil, err
		}

		intent, err := decodeIntent(kind, intentJSON)
		if err != nil {
			return nil, err
		}
		d.Intent = intent
		if err := decodeJSON(sourceJSON, &d.Source); err != nil {
			return nil, err
		}
		if err := decodeJSON(schedJSON, &d.Schedule); err != nil {
			return nil, err
		}
		if err := decodeJSON(destJSON, &d.Destination); err != nil {
			return nil, err
		}
		if err := decodeJSON(policyJSON, &d.Policy); err != nil {
			return nil, err
		}
		if d.CreatedAt, err = decodeTime(sql.NullString{String: createdAt, Valid: true}); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (x q) EnabledChecks(ctx context.Context) ([]*domain.Check, error) {
	rows, err := x.db.QueryContext(ctx,
		`SELECT id FROM checks WHERE tenant_id = ? AND enabled = 1 ORDER BY id`, x.tenant)
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing enabled checks: %w", err)
	}

	var ids []domain.CheckID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, domain.CheckID(id))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*domain.Check, 0, len(ids))
	for _, id := range ids {
		c, err := x.Check(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (x q) SaveCheck(ctx context.Context, c *domain.Check) error {
	if c == nil {
		return errors.New("sqlite: SaveCheck was given no check")
	}
	if err := c.CheckInvariants(); err != nil {
		return err
	}

	now := mustEncodeTime(c.ActiveDefinition().CreatedAt)
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO checks (tenant_id, id, enabled, active_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			enabled        = excluded.enabled,
			active_version = excluded.active_version,
			updated_at     = excluded.updated_at`,
		x.tenant, string(c.ID()), boolToInt(c.Enabled()), c.ActiveVersion(), now, now,
	); err != nil {
		return fmt.Errorf("sqlite: saving check %q: %w", c.ID(), err)
	}

	// Definitions are immutable, so an existing version is left alone rather
	// than rewritten. A past run's explanation depends on its version saying
	// what it said at the time.
	for _, d := range c.Versions() {
		intentJSON, err := encodeJSON(d.Intent)
		if err != nil {
			return err
		}
		sourceJSON, err := encodeJSON(d.Source)
		if err != nil {
			return err
		}
		schedJSON, err := encodeJSON(d.Schedule)
		if err != nil {
			return err
		}
		destJSON, err := encodeJSON(d.Destination)
		if err != nil {
			return err
		}
		policyJSON, err := encodeJSON(d.Policy)
		if err != nil {
			return err
		}

		if _, err := x.db.ExecContext(ctx, `
			INSERT INTO check_versions (
				tenant_id, check_id, version, intent_kind, intent_json, source_json,
				schedule_json, destination_json, policy_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, check_id, version) DO NOTHING`,
			x.tenant, string(c.ID()), d.Version, string(d.Intent.Kind()), intentJSON,
			sourceJSON, schedJSON, destJSON, policyJSON, mustEncodeTime(d.CreatedAt),
		); err != nil {
			return fmt.Errorf("sqlite: saving version %d of check %q: %w", d.Version, c.ID(), err)
		}
	}
	return nil
}

// --- runs -------------------------------------------------------------------

const runColumns = `id, check_id, slot, definition_version, binding_version, state,
	created_at, started_at, ended_at, snapshot_id, result_json, failure_json, explanation`

func (x q) scanRun(scan func(...any) error) (*domain.Run, error) {
	var (
		r                    domain.RestoredRun
		id, checkID, state   string
		createdAt            string
		startedAt, endedAt   sql.NullString
		snapshotID           sql.NullString
		resultJSON, failJSON sql.NullString
	)
	if err := scan(&id, &checkID, &r.Slot, &r.DefinitionVersion, &r.BindingVersion, &state,
		&createdAt, &startedAt, &endedAt, &snapshotID, &resultJSON, &failJSON, &r.Explanation); err != nil {
		return nil, err
	}

	r.ID, r.CheckID, r.State = domain.RunID(id), domain.CheckID(checkID), domain.RunState(state)
	r.SnapshotID = domain.SnapshotID(snapshotID.String)

	var err error
	if r.CreatedAt, err = decodeTime(sql.NullString{String: createdAt, Valid: true}); err != nil {
		return nil, err
	}
	if r.StartedAt, err = decodeTime(startedAt); err != nil {
		return nil, err
	}
	if r.EndedAt, err = decodeTime(endedAt); err != nil {
		return nil, err
	}
	if r.Result, err = decodeExtraction(resultJSON); err != nil {
		return nil, err
	}
	if r.Failure, err = decodeFailure(failJSON); err != nil {
		return nil, err
	}
	return domain.RestoreRun(r)
}

func (x q) Run(ctx context.Context, id domain.RunID) (*domain.Run, error) {
	row := x.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE tenant_id = ? AND id = ?`, x.tenant, string(id))
	r, err := x.scanRun(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	return r, err
}

func (x q) RunForSlot(ctx context.Context, id domain.CheckID, slot domain.Slot) (*domain.Run, error) {
	row := x.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE tenant_id = ? AND check_id = ? AND slot = ?`,
		x.tenant, string(id), int64(slot))
	r, err := x.scanRun(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	return r, err
}

func (x q) RecentRuns(ctx context.Context, id domain.CheckID, limit int) ([]*domain.Run, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := x.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE tenant_id = ? AND check_id = ?
		 ORDER BY created_at DESC LIMIT ?`, x.tenant, string(id), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading runs of check %q: %w", id, err)
	}
	defer rows.Close()

	var out []*domain.Run
	for rows.Next() {
		r, err := x.scanRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (x q) LastResult(ctx context.Context, id domain.CheckID) (domain.Extraction, error) {
	var resultJSON sql.NullString
	err := x.db.QueryRowContext(ctx, `
		SELECT result_json FROM runs
		 WHERE tenant_id = ? AND check_id = ? AND state IN ('quiet', 'changed')
		   AND result_json IS NOT NULL
		 ORDER BY ended_at DESC LIMIT 1`, x.tenant, string(id)).Scan(&resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Extraction{}, ports.ErrNotFound
	}
	if err != nil {
		return domain.Extraction{}, fmt.Errorf("sqlite: reading last result of check %q: %w", id, err)
	}
	return decodeExtraction(resultJSON)
}

// CreateRun claims a slot. The unique index on (tenant_id, check_id, slot) is
// what actually enforces one run per slot; this translates its complaint into
// the error the core understands.
func (x q) CreateRun(ctx context.Context, r *domain.Run) error {
	if r == nil {
		return errors.New("sqlite: CreateRun was given no run")
	}
	result, err := encodeExtraction(r.Result())
	if err != nil {
		return err
	}
	failure, err := encodeFailure(r.Failure())
	if err != nil {
		return err
	}

	_, err = x.db.ExecContext(ctx, `
		INSERT INTO runs (tenant_id, `+runColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		x.tenant, string(r.ID()), string(r.CheckID()), int64(r.Slot()),
		r.DefinitionVersion(), r.BindingVersion(), string(r.State()),
		mustEncodeTime(r.CreatedAt()), encodeTime(r.StartedAt()), encodeTime(r.EndedAt()),
		nullString(string(r.SnapshotID())), result, failure, r.Explanation())
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrDuplicateRun
		}
		return fmt.Errorf("sqlite: creating run %q: %w", r.ID(), err)
	}
	return nil
}

// UpdateRun stores a change to a run.
//
// The WHERE clause refuses to touch a row that is already terminal, so
// immutability holds even against a caller acting on a stale copy -- including
// one from another process this one knows nothing about.
func (x q) UpdateRun(ctx context.Context, r *domain.Run) error {
	if r == nil {
		return errors.New("sqlite: UpdateRun was given no run")
	}
	result, err := encodeExtraction(r.Result())
	if err != nil {
		return err
	}
	failure, err := encodeFailure(r.Failure())
	if err != nil {
		return err
	}

	res, err := x.db.ExecContext(ctx, `
		UPDATE runs SET
			binding_version = ?, state = ?, started_at = ?, ended_at = ?,
			snapshot_id = ?, result_json = ?, failure_json = ?, explanation = ?
		 WHERE tenant_id = ? AND id = ?
		   AND state IN ('pending', 'running')`,
		r.BindingVersion(), string(r.State()), encodeTime(r.StartedAt()), encodeTime(r.EndedAt()),
		nullString(string(r.SnapshotID())), result, failure, r.Explanation(),
		x.tenant, string(r.ID()))
	if err != nil {
		return fmt.Errorf("sqlite: updating run %q: %w", r.ID(), err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either the run is gone or it has already ended. Both are the same
		// answer to the caller: this run is not yours to change.
		return domain.ErrTerminal
	}
	return nil
}

// --- bindings ---------------------------------------------------------------

const bindingColumns = `id, check_id, definition_version, intent_kind, fingerprint,
	version, origin, locators_json, derived_at, derived_from`

func (x q) scanBinding(scan func(...any) error) (domain.Binding, error) {
	var (
		b                     domain.Binding
		id, checkID, kind, fp string
		origin, locatorsJSON  string
		derivedAt             string
		derivedFrom           sql.NullString
	)
	if err := scan(&id, &checkID, &b.DefinitionVersion, &kind, &fp, &b.Version,
		&origin, &locatorsJSON, &derivedAt, &derivedFrom); err != nil {
		return domain.Binding{}, err
	}

	b.ID, b.CheckID = domain.BindingID(id), domain.CheckID(checkID)
	b.IntentKind, b.Fingerprint = domain.IntentKind(kind), domain.SourceFingerprint(fp)
	b.Origin, b.DerivedFrom = domain.BindingOrigin(origin), domain.SnapshotID(derivedFrom.String)

	if err := decodeJSON(locatorsJSON, &b.Locators); err != nil {
		return domain.Binding{}, err
	}
	t, err := decodeTime(sql.NullString{String: derivedAt, Valid: true})
	if err != nil {
		return domain.Binding{}, err
	}
	b.DerivedAt = t
	return b, b.Validate()
}

func (x q) ActiveBinding(ctx context.Context, id domain.CheckID) (domain.Binding, error) {
	row := x.db.QueryRowContext(ctx,
		`SELECT `+bindingColumns+` FROM bindings
		  WHERE tenant_id = ? AND check_id = ? AND active = 1`, x.tenant, string(id))
	b, err := x.scanBinding(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Binding{}, ports.ErrNotFound
	}
	return b, err
}

func (x q) binding(ctx context.Context, id domain.BindingID) (domain.Binding, error) {
	row := x.db.QueryRowContext(ctx,
		`SELECT `+bindingColumns+` FROM bindings WHERE tenant_id = ? AND id = ?`, x.tenant, string(id))
	b, err := x.scanBinding(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Binding{}, ports.ErrNotFound
	}
	return b, err
}

func (x q) SaveBinding(ctx context.Context, b domain.Binding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	locators, err := encodeJSON(b.Locators)
	if err != nil {
		return err
	}

	// Saving never activates. A stored binding is a candidate until something
	// with an approval in hand says otherwise.
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO bindings (tenant_id, `+bindingColumns+`, active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			locators_json = excluded.locators_json,
			fingerprint   = excluded.fingerprint,
			derived_at    = excluded.derived_at,
			derived_from  = excluded.derived_from`,
		x.tenant, string(b.ID), string(b.CheckID), b.DefinitionVersion, string(b.IntentKind),
		string(b.Fingerprint), b.Version, string(b.Origin), locators,
		mustEncodeTime(b.DerivedAt), nullString(string(b.DerivedFrom)),
	); err != nil {
		return fmt.Errorf("sqlite: saving binding %q: %w", b.ID, err)
	}
	return nil
}

// ErrNotApprovedForActivation reports an attempt to put a repaired binding
// into force without a recorded human approval.
var ErrNotApprovedForActivation = errors.New("sqlite: a repaired binding cannot be activated without a recorded approval")

// ActivateBinding puts a binding version in force.
//
// A binding whose origin is "repaired" is refused unless an approval for it is
// recorded in repair_decisions. The product's central promise is checked in
// the domain, in the repair orchestrator, and again here, because this is the
// last point before the change becomes real and it is the one place that can
// still refuse when the code above it has a bug.
func (x q) ActivateBinding(ctx context.Context, id domain.CheckID, version int) error {
	var (
		bindingID string
		origin    string
	)
	err := x.db.QueryRowContext(ctx,
		`SELECT id, origin FROM bindings WHERE tenant_id = ? AND check_id = ? AND version = ?`,
		x.tenant, string(id), version).Scan(&bindingID, &origin)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("sqlite: reading binding %d of check %q: %w", version, id, err)
	}

	if domain.BindingOrigin(origin) == domain.OriginRepaired {
		var approvals int
		if err := x.db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			  FROM repair_decisions d
			  JOIN repair_candidates c
			    ON c.tenant_id = d.tenant_id AND c.id = d.candidate_id
			 WHERE d.tenant_id = ? AND c.binding_id = ? AND d.decision = ?
			   AND TRIM(d.decided_by) <> ''`,
			x.tenant, bindingID, string(domain.ApprovalApproved)).Scan(&approvals); err != nil {
			return fmt.Errorf("sqlite: checking approval for binding %q: %w", bindingID, err)
		}
		if approvals == 0 {
			return ErrNotApprovedForActivation
		}
	}

	// Clear first: the partial unique index allows only one active row per
	// check, so setting the new one before clearing the old would collide.
	if _, err := x.db.ExecContext(ctx,
		`UPDATE bindings SET active = 0 WHERE tenant_id = ? AND check_id = ?`,
		x.tenant, string(id)); err != nil {
		return fmt.Errorf("sqlite: clearing active binding of check %q: %w", id, err)
	}
	if _, err := x.db.ExecContext(ctx,
		`UPDATE bindings SET active = 1 WHERE tenant_id = ? AND check_id = ? AND version = ?`,
		x.tenant, string(id), version); err != nil {
		return fmt.Errorf("sqlite: activating binding %d of check %q: %w", version, id, err)
	}
	return nil
}

// isUniqueViolation reports whether err is SQLite complaining about a unique
// constraint. modernc's driver does not export a typed error for this, so the
// message is matched -- narrowly, and in exactly one place so that a driver
// that grows a typed error later is a one-line change.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE") ||
		strings.Contains(msg, "constraint failed (2067)")
}
