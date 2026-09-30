package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Snapshot bodies live once per tenant in snapshot_blobs; snapshot_refs points
// at one per check. Several checks watching the same status page share the
// bytes while keeping their own known-good flag and their own retention,
// because those were always per-check properties -- only the bytes are common.
//
// Sharing means deletion has to count. A check dropping its last reference must
// not take the body away from another check still pointing at it.

func (x q) PutSnapshot(ctx context.Context, s domain.Snapshot) error {
	if err := s.Verify(); err != nil {
		return err
	}
	body, compression, err := compressBody(s.Body())
	if err != nil {
		return err
	}

	// The body first. Content addressing makes this deduplicate across every
	// check in the tenant: the same bytes are the same row.
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO snapshot_blobs (tenant_id, id, size_bytes, compression, body)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO NOTHING`,
		x.tenant, string(s.ID()), s.Size(), compression, body,
	); err != nil {
		return fmt.Errorf("sqlite: storing capture body %q: %w", s.ID(), err)
	}

	// Then this check's reference to it. known_good is deliberately not
	// overwritten: a re-capture must not quietly demote a capture that a
	// repair could be verified against.
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO snapshot_refs (tenant_id, check_id, id, content_type,
			fingerprint, captured_at, known_good)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, check_id, id) DO NOTHING`,
		x.tenant, string(s.CheckID()), string(s.ID()), s.ContentType(),
		string(s.Fingerprint()), mustEncodeTime(s.CapturedAt()), boolToInt(s.KnownGood()),
	); err != nil {
		return fmt.Errorf("sqlite: storing capture reference %q: %w", s.ID(), err)
	}
	return nil
}

func (x q) MarkSnapshotKnownGood(ctx context.Context, check domain.CheckID, id domain.SnapshotID) error {
	res, err := x.db.ExecContext(ctx,
		`UPDATE snapshot_refs SET known_good = 1
		  WHERE tenant_id = ? AND check_id = ? AND id = ?`,
		x.tenant, string(check), string(id))
	if err != nil {
		return fmt.Errorf("sqlite: marking capture %q known-good: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ports.ErrNotFound
	}
	return nil
}

const snapshotColumns = `r.id, r.check_id, r.content_type, b.size_bytes, r.fingerprint,
	r.captured_at, r.known_good, b.compression, b.body`

const snapshotFrom = ` FROM snapshot_refs r
	JOIN snapshot_blobs b ON b.tenant_id = r.tenant_id AND b.id = r.id`

func (x q) scanSnapshot(scan func(...any) error) (domain.Snapshot, error) {
	var (
		id, checkID, ctype   string
		fp, capturedAt, comp string
		knownGood, sizeBytes int
		stored               []byte
	)
	if err := scan(&id, &checkID, &ctype, &sizeBytes, &fp, &capturedAt, &knownGood, &comp, &stored); err != nil {
		return domain.Snapshot{}, err
	}

	body, err := decompressBody(stored, comp)
	if err != nil {
		return domain.Snapshot{}, err
	}
	at, err := decodeTime(sql.NullString{String: capturedAt, Valid: true})
	if err != nil {
		return domain.Snapshot{}, err
	}

	return domain.RestoreSnapshot(domain.RestoredSnapshot{
		CheckID:     domain.CheckID(checkID),
		ContentType: ctype,
		Body:        body,
		Fingerprint: domain.SourceFingerprint(fp),
		CapturedAt:  at,
		KnownGood:   knownGood == 1,
		ID:          domain.SnapshotID(id),
	})
}

func (x q) Snapshot(ctx context.Context, id domain.SnapshotID) (domain.Snapshot, error) {
	row := x.db.QueryRowContext(ctx,
		`SELECT `+snapshotColumns+snapshotFrom+` WHERE r.tenant_id = ? AND r.id = ? LIMIT 1`,
		x.tenant, string(id))
	s, err := x.scanSnapshot(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Snapshot{}, ports.ErrNotFound
	}
	return s, err
}

func (x q) Snapshots(ctx context.Context, id domain.CheckID) (*domain.SnapshotIndex, error) {
	rows, err := x.db.QueryContext(ctx,
		`SELECT `+snapshotColumns+snapshotFrom+
			` WHERE r.tenant_id = ? AND r.check_id = ? ORDER BY r.captured_at ASC`,
		x.tenant, string(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading captures of check %q: %w", id, err)
	}
	defer rows.Close()

	var snaps []domain.Snapshot
	for rows.Next() {
		s, err := x.scanSnapshot(rows.Scan)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return domain.RestoreSnapshotIndex(id, snaps)
}

// DeleteSnapshots drops a check's references to captures, and removes any body
// nothing points at any more.
//
// It refuses to drop a known-good reference. The domain already protects the
// last one while pruning, but the store is the thing that actually destroys
// data, so it does not take the caller's word for it.
//
// Reference counting happens here rather than in a trigger so that deleting a
// shared body stays a visible operation someone can read in the code, rather
// than a side effect discovered when it misfires.
func (x q) DeleteSnapshots(ctx context.Context, check domain.CheckID, ids []domain.SnapshotID) error {
	for _, id := range ids {
		var knownGood int
		err := x.db.QueryRowContext(ctx,
			`SELECT known_good FROM snapshot_refs
			  WHERE tenant_id = ? AND check_id = ? AND id = ?`,
			x.tenant, string(check), string(id)).Scan(&knownGood)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sqlite: reading capture %q: %w", id, err)
		}
		if knownGood == 1 {
			return fmt.Errorf("sqlite: refusing to delete known-good capture %q; "+
				"without it no future repair for this check could be verified", id)
		}

		if _, err := x.db.ExecContext(ctx,
			`DELETE FROM snapshot_refs
			  WHERE tenant_id = ? AND check_id = ? AND id = ? AND known_good = 0`,
			x.tenant, string(check), string(id)); err != nil {
			return fmt.Errorf("sqlite: deleting capture reference %q: %w", id, err)
		}

		// The body goes only when the last reference to it has gone: another
		// check may still be pointing at these exact bytes.
		var remaining int
		if err := x.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM snapshot_refs WHERE tenant_id = ? AND id = ?`,
			x.tenant, string(id)).Scan(&remaining); err != nil {
			return fmt.Errorf("sqlite: counting references to capture %q: %w", id, err)
		}
		if remaining == 0 {
			if _, err := x.db.ExecContext(ctx,
				`DELETE FROM snapshot_blobs WHERE tenant_id = ? AND id = ?`,
				x.tenant, string(id)); err != nil {
				return fmt.Errorf("sqlite: deleting capture body %q: %w", id, err)
			}
		}
	}
	return nil
}

// --- audit ------------------------------------------------------------------

// AppendAudit records an event in the same transaction as whatever it
// describes. A trail written separately could be left lying by a crash between
// the two writes, and a trail that can lie is worse than none.
func (x q) AppendAudit(ctx context.Context, e domain.AuditEvent) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO audit_events (tenant_id, id, at, actor, action, subject_kind, subject_id, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO NOTHING`,
		x.tenant, e.ID, mustEncodeTime(e.At), e.Actor, string(e.Action),
		string(e.SubjectKind), e.SubjectID, e.Detail,
	); err != nil {
		return fmt.Errorf("sqlite: recording audit event %q: %w", e.ID, err)
	}
	return nil
}

func (x q) AuditTrail(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 10000 {
		limit = 10000
	}
	rows, err := x.db.QueryContext(ctx, `
		SELECT id, at, actor, action, subject_kind, subject_id, detail
		  FROM audit_events WHERE tenant_id = ? ORDER BY at DESC, id DESC LIMIT ?`,
		x.tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading audit trail: %w", err)
	}
	defer rows.Close()

	var out []domain.AuditEvent
	for rows.Next() {
		var (
			e            domain.AuditEvent
			at           string
			action, kind string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &action, &kind, &e.SubjectID, &e.Detail); err != nil {
			return nil, err
		}
		t, err := decodeTime(sql.NullString{String: at, Valid: true})
		if err != nil {
			return nil, err
		}
		e.At, e.Action, e.SubjectKind = t, domain.AuditAction(action), domain.SubjectKind(kind)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditTrail on the Store reads through the reader pool.
func (s *Store) AuditTrail(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	return s.reads().AuditTrail(ctx, limit)
}
