package run

import (
	"context"
	"fmt"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// RecoverCrashedRuns finds any non-terminal runs (runs left in StatePending or
// StateRunning after an ungraceful process termination) and marks them as
// StateInterrupted.
//
// This is called at daemon startup before the scheduler begins dispatching.
// It ensures that interrupted runs leave an explicit, honest record in the
// database rather than remaining pending forever, allowing catch-up policies
// to schedule future observations.
func RecoverCrashedRuns(ctx context.Context, store ports.Store, now time.Time) ([]*domain.Run, error) {
	nonTerminal, err := store.NonTerminalRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("recovering crashed runs: %w", err)
	}
	if len(nonTerminal) == 0 {
		return nil, nil
	}

	var recovered []*domain.Run
	for _, r := range nonTerminal {
		if err := r.Interrupt(now, "process restarted while run was in flight"); err != nil {
			// If already terminal (unlikely), continue
			continue
		}
		if err := store.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
			if err := tx.UpdateRun(ctx, r); err != nil {
				return err
			}
			return tx.AppendAudit(ctx, domain.Audit(
				"recovery:"+string(r.ID()),
				now,
				domain.ActionRunRecorded,
				domain.SubjectRun,
				string(r.ID()),
				"interrupted: process restarted while run was in flight",
			))
		}); err != nil {
			return nil, fmt.Errorf("persisting recovery for run %s: %w", r.ID(), err)
		}
		recovered = append(recovered, r)
	}
	return recovered, nil
}
