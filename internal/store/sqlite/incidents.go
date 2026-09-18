package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// An incident is stored across four tables rather than one, because the four
// things it records answer different questions and are asked about separately:
//
//	incidents            what broke, when, and how it ended
//	repair_candidates    what Agentd tried, including what did not work
//	verification_results which gate a candidate passed or failed, and why
//	repair_decisions     what a human said, and who they were
//
// Reassembly reads them back into one domain.Incident. The failures are kept
// deliberately: an operator deciding whether to trust a proposal wants to see
// what else was attempted on their behalf.

// candidateID derives a stable identifier for one attempt. Deriving it rather
// than minting one keeps the store free of an ID generator and makes saving
// the same incident twice idempotent.
func candidateID(incident domain.IncidentID, attempt int) string {
	return string(incident) + "#" + strconv.Itoa(attempt)
}

func (x q) SaveIncident(ctx context.Context, i *domain.Incident) error {
	if i == nil {
		return errors.New("sqlite: SaveIncident was given no incident")
	}
	if err := i.CheckInvariants(); err != nil {
		return err
	}

	cause, err := encodeJSON(i.Cause())
	if err != nil {
		return err
	}

	// The partial unique index refuses a second open incident for a check, so
	// a race between two processes noticing the same breakage ends with one
	// winner and one clear error rather than two incidents.
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO incidents (tenant_id, id, check_id, state, cause_json,
			opened_at, closed_at, max_attempts, resolution)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			state      = excluded.state,
			closed_at  = excluded.closed_at,
			resolution = excluded.resolution`,
		x.tenant, string(i.ID()), string(i.CheckID()), string(i.State()), cause,
		mustEncodeTime(i.OpenedAt()), encodeTime(i.ClosedAt()),
		i.AttemptsRemaining()+len(i.Attempts()), i.Resolution(),
	); err != nil {
		if isUniqueViolation(err) {
			return domain.ErrIncidentOpen
		}
		return fmt.Errorf("sqlite: saving incident %q: %w", i.ID(), err)
	}

	proposal := i.Proposal()

	for _, a := range i.Attempts() {
		id := candidateID(i.ID(), a.Number)

		var (
			bindingID    sql.NullString
			locatorsJSON = "[]"
			rationale    string
			verifiedAt   sql.NullString
			verifiedFrom sql.NullString
		)
		// Only the attempt that produced the standing proposal carries the
		// candidate binding. The others are a record that something was tried
		// and did not work.
		if proposal != nil && a.Outcome == domain.AttemptProposed {
			bindingID = nullString(string(proposal.Binding.ID))
			if locatorsJSON, err = encodeJSON(proposal.Binding.Locators); err != nil {
				return err
			}
			rationale = proposal.Rationale
			verifiedAt = encodeTime(proposal.VerifiedAt)
			verifiedFrom = nullString(string(proposal.VerifiedAgainst))
		}

		if _, err := x.db.ExecContext(ctx, `
			INSERT INTO repair_candidates (tenant_id, id, incident_id, check_id,
				attempt_number, outcome, binding_id, locators_json, rationale, note,
				verified_against, verified_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				outcome          = excluded.outcome,
				binding_id       = excluded.binding_id,
				locators_json    = excluded.locators_json,
				rationale        = excluded.rationale,
				note             = excluded.note,
				verified_against = excluded.verified_against,
				verified_at      = excluded.verified_at`,
			x.tenant, id, string(i.ID()), string(i.CheckID()), a.Number, string(a.Outcome),
			bindingID, locatorsJSON, rationale, a.Note, verifiedFrom, verifiedAt,
			mustEncodeTime(a.At),
		); err != nil {
			return fmt.Errorf("sqlite: saving repair attempt %d of incident %q: %w", a.Number, i.ID(), err)
		}
	}

	// A decision only exists once a human has made one.
	if proposal != nil && proposal.Approval != domain.ApprovalPending {
		attempt := proposedAttempt(i)
		if attempt == 0 {
			return fmt.Errorf("sqlite: incident %q has a decided proposal but no proposing attempt", i.ID())
		}
		cid := candidateID(i.ID(), attempt)
		if _, err := x.db.ExecContext(ctx, `
			INSERT INTO repair_decisions (tenant_id, id, incident_id, candidate_id,
				decision, decided_by, decided_at, note)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				decision   = excluded.decision,
				decided_by = excluded.decided_by,
				decided_at = excluded.decided_at,
				note       = excluded.note`,
			x.tenant, cid+"!decision", string(i.ID()), cid,
			string(proposal.Approval), proposal.ApprovedBy,
			mustEncodeTime(proposal.DecidedAt), i.Resolution(),
		); err != nil {
			return fmt.Errorf("sqlite: saving decision on incident %q: %w", i.ID(), err)
		}
	}
	return nil
}

// proposedAttempt returns the attempt number that produced the standing
// proposal, or zero.
func proposedAttempt(i *domain.Incident) int {
	for _, a := range i.Attempts() {
		if a.Outcome == domain.AttemptProposed {
			return a.Number
		}
	}
	return 0
}

func (x q) Incidents(ctx context.Context, id domain.CheckID) (*domain.IncidentLog, error) {
	rows, err := x.db.QueryContext(ctx, `
		SELECT id, state, cause_json, opened_at, closed_at, max_attempts, resolution
		  FROM incidents
		 WHERE tenant_id = ? AND check_id = ?
		 ORDER BY opened_at ASC`, x.tenant, string(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading incidents of check %q: %w", id, err)
	}

	type row struct {
		id, state, cause, openedAt, resolution string
		closedAt                               sql.NullString
		maxAttempts                            int
	}
	var raw []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.state, &r.cause, &r.openedAt, &r.closedAt,
			&r.maxAttempts, &r.resolution); err != nil {
			rows.Close()
			return nil, err
		}
		raw = append(raw, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	incidents := make([]*domain.Incident, 0, len(raw))
	for _, r := range raw {
		incidentID := domain.IncidentID(r.id)

		var cause domain.Failure
		if err := decodeJSON(r.cause, &cause); err != nil {
			return nil, err
		}
		openedAt, err := decodeTime(sql.NullString{String: r.openedAt, Valid: true})
		if err != nil {
			return nil, err
		}
		closedAt, err := decodeTime(r.closedAt)
		if err != nil {
			return nil, err
		}

		attempts, proposal, err := x.repairHistory(ctx, incidentID)
		if err != nil {
			return nil, err
		}

		inc, err := domain.RestoreIncident(domain.RestoredIncident{
			ID:          incidentID,
			CheckID:     id,
			State:       domain.IncidentState(r.state),
			Cause:       cause,
			OpenedAt:    openedAt,
			ClosedAt:    closedAt,
			MaxAttempts: r.maxAttempts,
			Attempts:    attempts,
			Proposal:    proposal,
			Resolution:  r.resolution,
		})
		if err != nil {
			return nil, err
		}
		incidents = append(incidents, inc)
	}

	return domain.RestoreIncidentLog(id, incidents)
}

// repairHistory reads an incident's attempts and rebuilds its standing
// proposal, if it has one.
func (x q) repairHistory(ctx context.Context, id domain.IncidentID) ([]domain.RepairAttempt, *domain.RepairProposal, error) {
	rows, err := x.db.QueryContext(ctx, `
		SELECT attempt_number, outcome, note, created_at, binding_id, rationale,
		       verified_against, verified_at
		  FROM repair_candidates
		 WHERE tenant_id = ? AND incident_id = ?
		 ORDER BY attempt_number ASC`, x.tenant, string(id))
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: reading repair attempts of incident %q: %w", id, err)
	}

	var (
		attempts     []domain.RepairAttempt
		bindingID    domain.BindingID
		rationale    string
		verifiedFrom domain.SnapshotID
		verifiedAt   time.Time
		hasCandidate bool
	)
	for rows.Next() {
		var (
			a          domain.RepairAttempt
			outcome    string
			createdAt  string
			bID, rat   sql.NullString
			vFrom, vAt sql.NullString
		)
		if err := rows.Scan(&a.Number, &outcome, &a.Note, &createdAt, &bID, &rat, &vFrom, &vAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		a.Outcome = domain.AttemptOutcome(outcome)
		if a.At, err = decodeTime(sql.NullString{String: createdAt, Valid: true}); err != nil {
			rows.Close()
			return nil, nil, err
		}
		attempts = append(attempts, a)

		if bID.Valid && bID.String != "" {
			hasCandidate = true
			bindingID = domain.BindingID(bID.String)
			rationale = rat.String
			verifiedFrom = domain.SnapshotID(vFrom.String)
			if verifiedAt, err = decodeTime(vAt); err != nil {
				rows.Close()
				return nil, nil, err
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if !hasCandidate {
		return attempts, nil, nil
	}

	binding, err := x.binding(ctx, bindingID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			// The candidate row survived but its binding did not. Returning
			// the attempts without a proposal is the honest reading: there is
			// nothing left to approve.
			return attempts, nil, nil
		}
		return nil, nil, err
	}

	proposal := &domain.RepairProposal{
		Binding:         binding,
		Rationale:       rationale,
		VerifiedAgainst: verifiedFrom,
		VerifiedAt:      verifiedAt,
		Approval:        domain.ApprovalPending,
	}

	if err := x.decision(ctx, id, proposal); err != nil {
		return nil, nil, err
	}
	return attempts, proposal, nil
}

// decision applies a recorded human decision to a proposal, if there is one.
func (x q) decision(ctx context.Context, id domain.IncidentID, p *domain.RepairProposal) error {
	var (
		decision, by string
		at           sql.NullString
	)
	err := x.db.QueryRowContext(ctx, `
		SELECT decision, decided_by, decided_at
		  FROM repair_decisions
		 WHERE tenant_id = ? AND incident_id = ?
		 ORDER BY decided_at DESC LIMIT 1`, x.tenant, string(id)).Scan(&decision, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlite: reading decision on incident %q: %w", id, err)
	}

	decidedAt, err := decodeTime(at)
	if err != nil {
		return err
	}
	p.Approval = domain.ApprovalState(decision)
	p.ApprovedBy = by
	p.DecidedAt = decidedAt
	return nil
}

// --- records the domain does not yet model ----------------------------------

// Verification is one gate a candidate had to pass. The repair orchestrator
// records a single pass or fail today; this exists so that gate-level detail
// can be written as soon as it produces any, without a schema change.
type Verification struct {
	ID              string
	IncidentID      domain.IncidentID
	AttemptNumber   int
	Gate            string
	Passed          bool
	Detail          string
	VerifiedAgainst domain.SnapshotID
	VerifiedAt      time.Time
}

// RecordVerification stores one gate result.
func (x q) RecordVerification(ctx context.Context, v Verification) error {
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO verification_results (tenant_id, id, incident_id, candidate_id,
			gate, passed, detail, verified_against, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			passed = excluded.passed, detail = excluded.detail`,
		x.tenant, v.ID, string(v.IncidentID), candidateID(v.IncidentID, v.AttemptNumber),
		v.Gate, boolToInt(v.Passed), v.Detail, string(v.VerifiedAgainst),
		mustEncodeTime(v.VerifiedAt),
	); err != nil {
		return fmt.Errorf("sqlite: recording verification %q: %w", v.ID, err)
	}
	return nil
}

// RepairEdit is a proposal a human changed before approving it.
//
// Both sides are kept. What Agentd suggested and what the operator actually
// approved are different facts, and collapsing them would erase the record of
// Agentd having been partly wrong -- which is exactly the record worth having
// when deciding how much to trust the next proposal.
type RepairEdit struct {
	ID            string
	IncidentID    domain.IncidentID
	AttemptNumber int
	Proposed      []domain.Locator
	Approved      []domain.Locator
	EditedBy      string
	EditedAt      time.Time
}

// RecordRepairEdit stores an edited-then-approved repair.
func (x q) RecordRepairEdit(ctx context.Context, e RepairEdit) error {
	if e.EditedBy == "" {
		return errors.New("sqlite: an edited repair must name the person who edited it")
	}
	proposed, err := encodeJSON(e.Proposed)
	if err != nil {
		return err
	}
	approved, err := encodeJSON(e.Approved)
	if err != nil {
		return err
	}

	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO repair_diffs (tenant_id, id, incident_id, candidate_id,
			proposed_locators_json, approved_locators_json, edited_by, edited_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			approved_locators_json = excluded.approved_locators_json,
			edited_by              = excluded.edited_by,
			edited_at              = excluded.edited_at`,
		x.tenant, e.ID, string(e.IncidentID), candidateID(e.IncidentID, e.AttemptNumber),
		proposed, approved, e.EditedBy, mustEncodeTime(e.EditedAt),
	); err != nil {
		return fmt.Errorf("sqlite: recording repair edit %q: %w", e.ID, err)
	}
	return nil
}

// AuditEvent is one thing that happened, kept so that a tool which asks
// permission can show later exactly what it asked and what was answered.
type AuditEvent struct {
	ID          string
	At          time.Time
	Actor       string
	Action      string
	SubjectKind string
	SubjectID   string
	Detail      string
}

// AppendAudit records an event. The audit table is append-only by convention
// and is not swept by retention.
func (x q) AppendAudit(ctx context.Context, e AuditEvent) error {
	if _, err := x.db.ExecContext(ctx, `
		INSERT INTO audit_events (tenant_id, id, at, actor, action, subject_kind, subject_id, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, id) DO NOTHING`,
		x.tenant, e.ID, mustEncodeTime(e.At), e.Actor, e.Action,
		e.SubjectKind, e.SubjectID, e.Detail,
	); err != nil {
		return fmt.Errorf("sqlite: recording audit event %q: %w", e.ID, err)
	}
	return nil
}

// Audit reads the audit trail for a tenant, newest first.
func (s *Store) Audit(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, at, actor, action, subject_kind, subject_id, detail
		  FROM audit_events WHERE tenant_id = ? ORDER BY at DESC LIMIT ?`, s.tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: reading audit trail: %w", err)
	}
	defer rows.Close()

	var out []AuditEvent
	for rows.Next() {
		var (
			e  AuditEvent
			at string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.SubjectKind, &e.SubjectID, &e.Detail); err != nil {
			return nil, err
		}
		t, err := decodeTime(sql.NullString{String: at, Valid: true})
		if err != nil {
			return nil, err
		}
		e.At = t
		out = append(out, e)
	}
	return out, rows.Err()
}
