package sqlite

import (
	"context"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

// PlanGC reports what a retention sweep would remove, without removing it.
//
// Retention destroys data, and the product's whole posture is that a person
// gets to look before anything irreversible happens. It runs the same policy
// against the same rows as GC, so the numbers are the ones a real sweep would
// produce rather than an estimate of them.
func (s *Store) PlanGC(ctx context.Context, policy domain.Retention, now time.Time) (domain.Sweep, error) {
	if err := policy.Validate(); err != nil {
		return domain.Sweep{}, err
	}

	var sweep domain.Sweep

	checkIDs, err := s.allCheckIDs(ctx)
	if err != nil {
		return sweep, err
	}
	for _, id := range checkIDs {
		index, err := s.Snapshots(ctx, id)
		if err != nil {
			return sweep, err
		}
		// Prune mutates the index, but this one is a freshly loaded copy and
		// is thrown away, so nothing stored is touched.
		sweep.Snapshots += len(policy.ExpiredSnapshots(index))
	}

	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs
		 WHERE tenant_id = ? AND state NOT IN ('pending', 'running')
		   AND ended_at IS NOT NULL AND ended_at < ?`,
		s.tenant, mustEncodeTime(policy.RunCutoff(now))).Scan(&sweep.Runs); err != nil {
		return sweep, err
	}

	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM incidents
		 WHERE tenant_id = ? AND state NOT IN ('open', 'awaiting_approval')
		   AND closed_at IS NOT NULL AND closed_at < ?`,
		s.tenant, mustEncodeTime(policy.IncidentCutoff(now))).Scan(&sweep.Incidents); err != nil {
		return sweep, err
	}

	return sweep, nil
}
