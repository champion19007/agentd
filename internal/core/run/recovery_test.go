package run_test

import (
	"context"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/ports"
)

type recoveryStore struct {
	ports.Store
	runs      map[domain.RunKey]*domain.Run
	audits    []domain.AuditEvent
	updateErr error
	listErr   error
}

func (s *recoveryStore) NonTerminalRuns(context.Context) ([]*domain.Run, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*domain.Run
	for _, r := range s.runs {
		if !r.Terminal() {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *recoveryStore) Update(ctx context.Context, fn func(context.Context, ports.Tx) error) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return fn(ctx, recoveryTx{s})
}

type recoveryTx struct {
	*recoveryStore
}

func (tx recoveryTx) UpdateRun(_ context.Context, r *domain.Run) error {
	tx.runs[r.Key()] = r
	return nil
}

func (tx recoveryTx) AppendAudit(_ context.Context, e domain.AuditEvent) error {
	tx.audits = append(tx.audits, e)
	return nil
}

func (tx recoveryTx) Check(context.Context, domain.CheckID) (*domain.Check, error) { return nil, nil }
func (tx recoveryTx) EnabledChecks(context.Context) ([]*domain.Check, error)       { return nil, nil }
func (tx recoveryTx) Run(context.Context, domain.RunID) (*domain.Run, error)       { return nil, nil }
func (tx recoveryTx) RunForSlot(context.Context, domain.CheckID, domain.Slot) (*domain.Run, error) {
	return nil, nil
}
func (tx recoveryTx) RecentRuns(context.Context, domain.CheckID, int) ([]*domain.Run, error) {
	return nil, nil
}
func (tx recoveryTx) LastResult(context.Context, domain.CheckID) (domain.Extraction, error) {
	return domain.Extraction{}, nil
}
func (tx recoveryTx) ActiveBinding(context.Context, domain.CheckID) (domain.Binding, error) {
	return domain.Binding{}, nil
}
func (tx recoveryTx) Snapshot(context.Context, domain.SnapshotID) (domain.Snapshot, error) {
	return domain.Snapshot{}, nil
}
func (tx recoveryTx) Snapshots(context.Context, domain.CheckID) (*domain.SnapshotIndex, error) {
	return nil, nil
}
func (tx recoveryTx) Incidents(context.Context, domain.CheckID) (*domain.IncidentLog, error) {
	return nil, nil
}
func (tx recoveryTx) AuditTrail(context.Context, int) ([]domain.AuditEvent, error) { return nil, nil }
func (tx recoveryTx) NonTerminalRuns(context.Context) ([]*domain.Run, error)       { return nil, nil }
func (tx recoveryTx) SaveCheck(context.Context, *domain.Check) error               { return nil }
func (tx recoveryTx) CreateRun(context.Context, *domain.Run) error                 { return nil }
func (tx recoveryTx) PutSnapshot(context.Context, domain.Snapshot) error           { return nil }
func (tx recoveryTx) MarkSnapshotKnownGood(context.Context, domain.CheckID, domain.SnapshotID) error {
	return nil
}
func (tx recoveryTx) DeleteSnapshots(context.Context, domain.CheckID, []domain.SnapshotID) error {
	return nil
}
func (tx recoveryTx) SaveBinding(context.Context, domain.Binding) error          { return nil }
func (tx recoveryTx) ActivateBinding(context.Context, domain.CheckID, int) error { return nil }
func (tx recoveryTx) SaveIncident(context.Context, *domain.Incident) error       { return nil }

func TestRecoverCrashedRuns(t *testing.T) {
	st := &recoveryStore{runs: map[domain.RunKey]*domain.Run{}}

	// 1. Pending run
	rPending, err := domain.NewRun("run-pending", "chk-1", 1, 1, base)
	if err != nil {
		t.Fatal(err)
	}
	st.runs[rPending.Key()] = rPending

	// 2. Running run
	rRunning, err := domain.NewRun("run-running", "chk-1", 2, 1, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := rRunning.Start(base.Add(time.Second), 1); err != nil {
		t.Fatal(err)
	}
	st.runs[rRunning.Key()] = rRunning

	// 3. Already terminal run
	rQuiet, err := domain.NewRun("run-quiet", "chk-1", 3, 1, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := rQuiet.Start(base.Add(time.Second), 1); err != nil {
		t.Fatal(err)
	}
	if err := rQuiet.Quiet(base.Add(2*time.Second), "snap-1", domain.Extraction{Kind: domain.IntentScalar}); err != nil {
		t.Fatal(err)
	}
	st.runs[rQuiet.Key()] = rQuiet

	now := base.Add(time.Hour)
	recovered, err := run.RecoverCrashedRuns(context.Background(), st, now)
	if err != nil {
		t.Fatalf("RecoverCrashedRuns: %v", err)
	}

	if len(recovered) != 2 {
		t.Fatalf("expected 2 recovered runs, got %d", len(recovered))
	}

	for _, r := range recovered {
		if r.State() != domain.StateInterrupted {
			t.Errorf("run %s state = %q, want interrupted", r.ID(), r.State())
		}
		if !r.Terminal() {
			t.Errorf("run %s should be terminal", r.ID())
		}
		if r.EndedAt() != now.UTC() {
			t.Errorf("run %s endedAt = %v, want %v", r.ID(), r.EndedAt(), now.UTC())
		}
	}

	// rQuiet must be untouched
	if st.runs[rQuiet.Key()].State() != domain.StateQuiet {
		t.Errorf("rQuiet state changed to %q", st.runs[rQuiet.Key()].State())
	}

	if len(st.audits) != 2 {
		t.Errorf("expected 2 audit entries, got %d", len(st.audits))
	}
}

func TestRecoverCrashedRuns_ErrorsAndEmpty(t *testing.T) {
	ctx := context.Background()
	now := base.Add(time.Hour)

	// 1. Empty runs returns nil, nil
	stEmpty := &recoveryStore{runs: make(map[domain.RunKey]*domain.Run)}
	recEmpty, err := run.RecoverCrashedRuns(ctx, stEmpty, now)
	if err != nil || len(recEmpty) != 0 {
		t.Errorf("expected (nil, nil) for empty runs, got (%v, %v)", recEmpty, err)
	}

	// 2. List error
	stListErr := &recoveryStore{
		runs:    make(map[domain.RunKey]*domain.Run),
		listErr: context.DeadlineExceeded,
	}
	_, err = run.RecoverCrashedRuns(ctx, stListErr, now)
	if err == nil {
		t.Error("expected error when store.NonTerminalRuns fails")
	}

	// 3. Update error
	rPending, _ := domain.NewRun("run-pend", "chk-1", 1, 1, base)
	stUpdateErr := &recoveryStore{
		runs:      map[domain.RunKey]*domain.Run{rPending.Key(): rPending},
		updateErr: context.Canceled,
	}
	_, err = run.RecoverCrashedRuns(ctx, stUpdateErr, now)
	if err == nil {
		t.Error("expected error when store.Update fails")
	}
}
