package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/champion19007/agentd/internal/adapters/notify"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/repair"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

// Deps holds the dependencies for the Application Service layer.
type Deps struct {
	Store      *sqlite.Store
	DBPath     string
	Clock      ports.Clock
	IDs        ports.IDs
	RunOrch    *run.Orchestrator
	RepairOrch *repair.Orchestrator
	Notifier   ports.Notifier
	Spool      *notify.Spool
	Metrics    ports.Metrics
}

// Service implements Operations against the Agentd core domain and store.
type Service struct {
	deps Deps
}

var _ Operations = (*Service)(nil)

// NewService constructs an Application Service.
func NewService(deps Deps) *Service {
	if deps.Clock == nil {
		deps.Clock = clockAdapter{now: func() time.Time { return time.Now().UTC() }}
	}
	if deps.IDs == nil {
		deps.IDs = defaultIDs{}
	}
	if deps.Metrics == nil {
		deps.Metrics = ports.NoopMetrics{}
	}
	return &Service{deps: deps}
}

// Init initializes the database and directory.
func (s *Service) Init(ctx context.Context, req InitRequest) (InitResponse, error) {
	path := req.Path
	if path == "" {
		path = s.deps.DBPath
	}
	if path == "" {
		return InitResponse{}, errors.New("init: database path must not be empty")
	}

	alreadyExisted := false
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		alreadyExisted = true
	}

	st, err := sqlite.Open(ctx, sqlite.Options{Path: path})
	if err != nil {
		return InitResponse{}, fmt.Errorf("init: failed to initialize database: %w", err)
	}
	_ = st.Close()

	msg := fmt.Sprintf("Agentd database initialized at %s", path)
	if alreadyExisted {
		msg = fmt.Sprintf("Agentd database already initialized at %s", path)
	}

	return InitResponse{
		Path:    path,
		Created: !alreadyExisted,
		Message: msg,
	}, nil
}

// Status returns system health, database state, check counts, and open incidents.
func (s *Service) Status(ctx context.Context) (StatusResponse, error) {
	if s.deps.Store == nil {
		return StatusResponse{Healthy: false, Message: "database not opened"}, errors.New("database not opened")
	}

	version, err := s.deps.Store.SchemaVersion(ctx)
	if err != nil {
		return StatusResponse{Healthy: false, Message: err.Error()}, err
	}

	checks, err := s.deps.Store.EnabledChecks(ctx)
	if err != nil {
		return StatusResponse{Healthy: false, Message: err.Error()}, err
	}

	openIncidents, err := s.deps.Store.OpenIncidents(ctx)
	if err != nil {
		return StatusResponse{Healthy: false, Message: err.Error()}, err
	}

	awaiting := 0
	for _, inc := range openIncidents {
		if inc.State() == domain.IncidentAwaitingApproval {
			awaiting++
		}
	}

	recentRunsCount := 0
	for _, c := range checks {
		runs, err := s.deps.Store.RecentRuns(ctx, c.ID(), 20)
		if err == nil {
			recentRunsCount += len(runs)
		}
	}

	undelivered := 0
	if s.deps.Spool != nil {
		undelivered = s.deps.Spool.Depth()
	}

	maxStaleness := 0.0
	staleCount := 0
	now := s.deps.Clock.Now()
	for _, c := range checks {
		var lastRun *domain.Run
		if runs, err := s.deps.Store.RecentRuns(ctx, c.ID(), 1); err == nil && len(runs) > 0 {
			lastRun = runs[0]
		}
		st := domain.CheckStaleness(c, lastRun, now, 1.0)
		staleness := 0.0
		if st.Elapsed > st.ExpectedInterval {
			staleness = (st.Elapsed - st.ExpectedInterval).Seconds()
		}
		if s.deps.Metrics != nil {
			s.deps.Metrics.SetCheckStaleness(string(c.ID()), staleness)
		}
		if staleness > 0 {
			staleCount++
			if staleness > maxStaleness {
				maxStaleness = staleness
			}
		}
	}

	freshnessSummary := "all checks are fresh within cadence"
	if staleCount > 0 {
		freshnessSummary = fmt.Sprintf("%d check(s) overdue beyond expected cadence", staleCount)
	}

	msg := "Agentd is healthy"
	if len(openIncidents) > 0 {
		msg = fmt.Sprintf("Agentd has %d open incident(s) needing attention", len(openIncidents))
	} else if staleCount > 0 {
		msg = fmt.Sprintf("Agentd has %d stale check(s) exceeding cadence", staleCount)
	}

	return StatusResponse{
		Healthy:             true,
		DBPath:              s.deps.DBPath,
		SchemaVersion:       version,
		Integrity:           "ok",
		ChecksTotal:         len(checks),
		ChecksEnabled:       len(checks),
		OpenIncidents:       len(openIncidents),
		AwaitingApproval:    awaiting,
		RecentRuns:          recentRunsCount,
		UndeliveredNotifs:   undelivered,
		FreshnessSummary:    freshnessSummary,
		MaxStalenessSeconds: maxStaleness,
		Message:             msg,
	}, nil
}

// AddCheck creates a new check and its initial binding.
func (s *Service) AddCheck(ctx context.Context, req AddCheckRequest) (CheckSummary, error) {
	if strings.TrimSpace(req.Name) == "" {
		return CheckSummary{}, errors.New("check name is required")
	}
	if strings.TrimSpace(req.URL) == "" {
		return CheckSummary{}, errors.New("check URL is required")
	}

	checkID := domain.CheckID(strings.TrimSpace(req.ID))
	if checkID == "" {
		slug := strings.ToLower(strings.ReplaceAll(req.Name, " ", "-"))
		checkID = domain.CheckID(fmt.Sprintf("check-%s-%d", slug, s.deps.Clock.Now().Unix()%10000))
	}

	if _, err := s.deps.Store.Check(ctx, checkID); err == nil {
		return CheckSummary{}, fmt.Errorf("check %q already exists", checkID)
	}

	interval := 10 * time.Minute
	if req.Interval != "" {
		d, err := time.ParseDuration(req.Interval)
		if err != nil {
			return CheckSummary{}, fmt.Errorf("invalid interval %q: %w", req.Interval, err)
		}
		interval = d
	}

	target := req.Target
	if target == "" {
		target = "value"
	}
	expr := req.Expression
	if expr == "" {
		expr = "body"
	}
	dialect := req.Dialect
	if dialect == "" {
		dialect = "css"
	}

	destKind := domain.DestinationNotify
	destTarget := req.DestTarget
	if req.Destination == "none" {
		destKind = domain.DestinationNone
		destTarget = ""
	} else if destTarget == "" {
		destTarget = "stdout"
	}

	intent := domain.ScalarIntent{
		Label:   req.Name,
		Purpose: fmt.Sprintf("Monitor %s from %s", req.Name, req.URL),
		Type:    domain.TypeString,
	}

	now := s.deps.Clock.Now()
	def := domain.Definition{
		Version: 1,
		Intent:  intent,
		Source: domain.SourceSpec{
			Kind: domain.SourceHTTP,
			URL:  req.URL,
		},
		Schedule: domain.Schedule{
			Interval: interval,
			CatchUp:  domain.CatchUpOnce,
		},
		Destination: domain.Destination{
			Kind:    destKind,
			Target:  destTarget,
			OnQuiet: req.OnQuiet,
		},
		Policy: domain.Policy{
			MaxRetries:   3,
			RetryBackoff: 5 * time.Second,
		},
		CreatedAt: now,
	}

	c, err := domain.NewCheck(checkID, def)
	if err != nil {
		return CheckSummary{}, err
	}

	locators := []domain.Locator{
		{Target: target, Dialect: dialect, Expression: expr},
	}
	b := domain.Binding{
		ID:                s.deps.IDs.NewBindingID(),
		CheckID:           checkID,
		DefinitionVersion: 1,
		IntentKind:        intent.Kind(),
		Fingerprint:       domain.SourceFingerprint("init"),
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators:          locators,
		DerivedAt:         now,
	}

	err = s.deps.Store.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveCheck(ctx, c); err != nil {
			return err
		}
		if err := tx.SaveBinding(ctx, b); err != nil {
			return err
		}
		if err := tx.ActivateBinding(ctx, checkID, 1); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, domain.AuditBy(
			"check:create:"+string(checkID), now, "operator",
			domain.ActionCheckCreated, domain.SubjectCheck, string(checkID),
			fmt.Sprintf("created check %q watching %s", req.Name, req.URL),
		))
	})
	if err != nil {
		return CheckSummary{}, err
	}

	return CheckSummary{
		ID:       string(checkID),
		Name:     req.Name,
		URL:      req.URL,
		Interval: interval.String(),
		Enabled:  true,
	}, nil
}

// ListChecks lists all enabled checks.
func (s *Service) ListChecks(ctx context.Context) ([]CheckSummary, error) {
	checks, err := s.deps.Store.EnabledChecks(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]CheckSummary, 0, len(checks))
	now := s.deps.Clock.Now()
	for _, c := range checks {
		def := c.ActiveDefinition()
		lastState := "never"
		lastTime := ""
		var lastRun *domain.Run

		if runs, err := s.deps.Store.RecentRuns(ctx, c.ID(), 1); err == nil && len(runs) > 0 {
			lastRun = runs[0]
			lastState = string(lastRun.State())
			ended := lastRun.EndedAt()
			if ended.IsZero() {
				ended = lastRun.CreatedAt()
			}
			lastTime = ended.Format(time.RFC3339)
		}

		st := domain.CheckStaleness(c, lastRun, now, 1.0)
		staleness := 0.0
		if st.Elapsed > st.ExpectedInterval {
			staleness = (st.Elapsed - st.ExpectedInterval).Seconds()
		}
		freshness := st.Elapsed.Seconds()

		out = append(out, CheckSummary{
			ID:               string(c.ID()),
			Name:             def.Intent.Name(),
			URL:              def.Source.URL,
			Interval:         def.Schedule.Interval.String(),
			Enabled:          c.Enabled(),
			LastRunState:     lastState,
			LastRunTime:      lastTime,
			FreshnessSeconds: freshness,
			StalenessSeconds: staleness,
		})
	}
	return out, nil
}

// GetCheck returns detailed configuration and state for a check.
func (s *Service) GetCheck(ctx context.Context, id domain.CheckID) (CheckDetail, error) {
	c, err := s.deps.Store.Check(ctx, id)
	if err != nil {
		return CheckDetail{}, fmt.Errorf("check %q not found: %w", id, err)
	}

	def := c.ActiveDefinition()
	b, err := s.deps.Store.ActiveBinding(ctx, id)
	var locators []domain.Locator
	if err == nil {
		locators = b.Locators
	}

	runs, _ := s.deps.Store.RecentRuns(ctx, id, 5)
	runSummaries := make([]RunSummary, 0, len(runs))
	for _, r := range runs {
		when := r.EndedAt()
		if when.IsZero() {
			when = r.CreatedAt()
		}
		runSummaries = append(runSummaries, RunSummary{
			ID:          string(r.ID()),
			CheckID:     string(r.CheckID()),
			Slot:        int64(r.Slot()),
			State:       string(r.State()),
			Explanation: r.Explanation(),
			When:        when.Format(time.RFC3339),
			TraceID:     r.TraceID(),
		})
	}

	lastState := "never"
	lastTime := ""
	if len(runSummaries) > 0 {
		lastState = runSummaries[0].State
		lastTime = runSummaries[0].When
	}

	var lastRun *domain.Run
	if len(runs) > 0 {
		lastRun = runs[0]
	}
	now := s.deps.Clock.Now()
	st := domain.CheckStaleness(c, lastRun, now, 1.0)
	staleness := 0.0
	if st.Elapsed > st.ExpectedInterval {
		staleness = (st.Elapsed - st.ExpectedInterval).Seconds()
	}
	freshness := st.Elapsed.Seconds()

	return CheckDetail{
		CheckSummary: CheckSummary{
			ID:               string(c.ID()),
			Name:             def.Intent.Name(),
			URL:              def.Source.URL,
			Interval:         def.Schedule.Interval.String(),
			Enabled:          c.Enabled(),
			LastRunState:     lastState,
			LastRunTime:      lastTime,
			FreshnessSeconds: freshness,
			StalenessSeconds: staleness,
		},
		Version:    def.Version,
		Shape:      string(def.Intent.Kind()),
		Locators:   locators,
		RecentRuns: runSummaries,
	}, nil
}

// RunCheck executes an immediate run for a check.
func (s *Service) RunCheck(ctx context.Context, id domain.CheckID) (RunSummary, error) {
	c, err := s.deps.Store.Check(ctx, id)
	if err != nil {
		return RunSummary{}, fmt.Errorf("check %q: %w", id, err)
	}

	now := s.deps.Clock.Now()
	slot := c.ActiveDefinition().Schedule.SlotAt(now)

	if s.deps.RunOrch != nil {
		outcome, err := s.deps.RunOrch.Run(ctx, c, slot)
		if err != nil {
			return RunSummary{}, err
		}
		r := outcome.Run
		when := r.EndedAt()
		if when.IsZero() {
			when = r.CreatedAt()
		}
		return RunSummary{
			ID:          string(r.ID()),
			CheckID:     string(r.CheckID()),
			Slot:        int64(r.Slot()),
			State:       string(r.State()),
			Explanation: r.Explanation(),
			When:        when.Format(time.RFC3339),
			TraceID:     r.TraceID(),
		}, nil
	}

	// Fallback when orchestrator is not wired: query most recent run
	runs, err := s.deps.Store.RecentRuns(ctx, id, 1)
	if err != nil || len(runs) == 0 {
		return RunSummary{
			CheckID:     string(id),
			Slot:        int64(slot),
			State:       string(domain.StateQuiet),
			Explanation: "check triggered",
			When:        now.Format(time.RFC3339),
		}, nil
	}
	r := runs[0]
	return RunSummary{
		ID:          string(r.ID()),
		CheckID:     string(r.CheckID()),
		Slot:        int64(r.Slot()),
		State:       string(r.State()),
		Explanation: r.Explanation(),
		When:        now.Format(time.RFC3339),
		TraceID:     r.TraceID(),
	}, nil
}

// DeleteCheck disables the specified check.
func (s *Service) DeleteCheck(ctx context.Context, id domain.CheckID) error {
	c, err := s.deps.Store.Check(ctx, id)
	if err != nil {
		return fmt.Errorf("check %q: %w", id, err)
	}

	now := s.deps.Clock.Now()
	c.Disable()

	return s.deps.Store.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveCheck(ctx, c); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, domain.AuditBy(
			"check:disable:"+string(id), now, "operator",
			domain.ActionCheckDisabled, domain.SubjectCheck, string(id),
			"disabled check",
		))
	})
}

// ListRuns returns recent runs for a check.
func (s *Service) ListRuns(ctx context.Context, checkID domain.CheckID, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	runs, err := s.deps.Store.RecentRuns(ctx, checkID, limit)
	if err != nil {
		return nil, err
	}

	out := make([]RunSummary, 0, len(runs))
	for _, r := range runs {
		when := r.EndedAt()
		if when.IsZero() {
			when = r.CreatedAt()
		}
		out = append(out, RunSummary{
			ID:          string(r.ID()),
			CheckID:     string(r.CheckID()),
			Slot:        int64(r.Slot()),
			State:       string(r.State()),
			Explanation: r.Explanation(),
			When:        when.Format(time.RFC3339),
			TraceID:     r.TraceID(),
		})
	}
	return out, nil
}

// GetRun returns full details of a specific run.
func (s *Service) GetRun(ctx context.Context, id domain.RunID) (RunDetail, error) {
	r, err := s.deps.Store.Run(ctx, id)
	if err != nil {
		return RunDetail{}, fmt.Errorf("run %q not found: %w", id, err)
	}

	failSummary := ""
	failClass := ""
	if f := r.Failure(); f != nil {
		failSummary = f.Summary
		failClass = string(f.Class)
	}

	when := r.EndedAt()
	if when.IsZero() {
		when = r.CreatedAt()
	}

	return RunDetail{
		RunSummary: RunSummary{
			ID:          string(r.ID()),
			CheckID:     string(r.CheckID()),
			Slot:        int64(r.Slot()),
			State:       string(r.State()),
			Explanation: r.Explanation(),
			When:        when.Format(time.RFC3339),
			TraceID:     r.TraceID(),
		},
		StartedAt:      r.CreatedAt().Format(time.RFC3339),
		EndedAt:        r.EndedAt().Format(time.RFC3339),
		PayloadHash:    string(r.SnapshotID()),
		FailureSummary: failSummary,
		FailureClass:   failClass,
	}, nil
}

// ListIncidents returns open incidents.
func (s *Service) ListIncidents(ctx context.Context) ([]IncidentSummary, error) {
	incidents, err := s.deps.Store.OpenIncidents(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]IncidentSummary, 0, len(incidents))
	for _, inc := range incidents {
		whatBroke := formatUserFriendlyBreakage(inc.Cause())
		out = append(out, IncidentSummary{
			ID:           string(inc.ID()),
			CheckID:      string(inc.CheckID()),
			State:        string(inc.State()),
			WhatBroke:    whatBroke,
			OpenedAt:     inc.OpenedAt().Format(time.RFC3339),
			AttemptsLeft: inc.AttemptsRemaining(),
			HasProposal:  inc.Proposal() != nil,
		})
	}
	return out, nil
}

// GetIncident returns detailed, user-explained information about an incident.
func (s *Service) GetIncident(ctx context.Context, id domain.IncidentID) (IncidentDetail, error) {
	incidents, err := s.deps.Store.OpenIncidents(ctx)
	if err != nil {
		return IncidentDetail{}, err
	}

	var target *domain.Incident
	for _, inc := range incidents {
		if inc.ID() == id {
			target = inc
			break
		}
	}

	if target == nil {
		// Also search checks including historical incidents
		checks, _ := s.deps.Store.EnabledChecks(ctx)
		for _, c := range checks {
			log, err := s.deps.Store.Incidents(ctx, c.ID())
			if err == nil {
				for _, inc := range log.All() {
					if inc.ID() == id {
						target = inc
						break
					}
				}
				if target != nil {
					break
				}
			}
		}
	}

	if target == nil {
		return IncidentDetail{}, fmt.Errorf("incident %q not found", id)
	}

	whatBroke := formatUserFriendlyBreakage(target.Cause())
	whatFound := ""
	whatProposes := ""
	var proposedLocators []domain.Locator
	var oldLocators []domain.Locator
	diff := ""
	var gateResults []GateResultSummary
	actionRequired := "No action required at this time."

	p := target.Proposal()
	if p != nil {
		whatFound = fmt.Sprintf("Agentd inspected current page capture %s and located candidate elements reproducing the intent.", p.VerifiedAgainst)
		whatProposes = p.Rationale
		proposedLocators = p.Binding.Locators
		diff = p.Diff

		// Lookup old locators from active binding
		if b, err := s.deps.Store.ActiveBinding(ctx, target.CheckID()); err == nil {
			oldLocators = b.Locators
		}

		for _, g := range p.Gates {
			gateResults = append(gateResults, GateResultSummary{
				Gate:   string(g.Gate),
				Passed: g.Passed,
				Detail: g.Detail,
			})
		}

		actionRequired = fmt.Sprintf("Nothing has been changed. Agentd never applies repairs automatically.\nTo approve: agentd repair approve %s --by <your-name>\nTo reject:  agentd repair reject  %s --by <your-name>", target.ID(), target.ID())
	} else if target.AttemptsRemaining() == 0 {
		actionRequired = "All candidate generation attempts were exhausted. Manual intervention is required to update check locators."
	}

	return IncidentDetail{
		IncidentSummary: IncidentSummary{
			ID:           string(target.ID()),
			CheckID:      string(target.CheckID()),
			State:        string(target.State()),
			WhatBroke:    whatBroke,
			OpenedAt:     target.OpenedAt().Format(time.RFC3339),
			AttemptsLeft: target.AttemptsRemaining(),
			HasProposal:  p != nil,
		},
		WhatAgentdFound:     whatFound,
		WhatItProposes:      whatProposes,
		ProposedLocators:    proposedLocators,
		OldLocators:         oldLocators,
		Diff:                diff,
		VerificationResults: gateResults,
		ActionRequired:      actionRequired,
	}, nil
}

// ApproveRepair records a human's approval of a proposed repair.
func (s *Service) ApproveRepair(ctx context.Context, id domain.IncidentID, req ApproveRequest) (DecisionResult, error) {
	if strings.TrimSpace(req.By) == "" {
		return DecisionResult{}, errors.New("--by is required: a repair approval must be attributable to a person")
	}

	checks, err := s.deps.Store.EnabledChecks(ctx)
	if err != nil {
		return DecisionResult{}, err
	}

	now := s.deps.Clock.Now()
	var closedIncident *domain.Incident
	for _, c := range checks {
		log, err := s.deps.Store.Incidents(ctx, c.ID())
		if err != nil {
			continue
		}
		for _, inc := range log.All() {
			if inc.ID() == id && !inc.State().Open() {
				closedIncident = inc
			}
		}
		inc, open := log.Current()
		if !open || inc.ID() != id {
			continue
		}

		action := domain.ActionRepairApproved
		if req.Locators != "" {
			locators, err := parseLocators(inc, req.Locators)
			if err != nil {
				return DecisionResult{}, err
			}
			if err := inc.ApproveWithEdits(req.By, locators, now); err != nil {
				return DecisionResult{}, err
			}
			action = domain.ActionRepairEdited
		} else {
			if err := inc.Approve(req.By, now); err != nil {
				return DecisionResult{}, err
			}
		}

		binding, err := inc.ApprovedBinding()
		if err != nil {
			return DecisionResult{}, err
		}

		err = s.deps.Store.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
			if err := tx.SaveBinding(ctx, binding); err != nil {
				return err
			}
			if err := tx.SaveIncident(ctx, inc); err != nil {
				return err
			}
			if err := tx.ActivateBinding(ctx, c.ID(), binding.Version); err != nil {
				return err
			}
			if err := inc.ResolveWithRepair(now); err != nil {
				return err
			}
			if err := tx.SaveIncident(ctx, inc); err != nil {
				return err
			}
			return tx.AppendAudit(ctx, domain.AuditBy(
				"decision:"+string(inc.ID()), now, req.By, action,
				domain.SubjectIncident, string(inc.ID()),
				fmt.Sprintf("approved repair for check %s", c.ID()),
			))
		})
		if err != nil {
			return DecisionResult{}, err
		}

		return DecisionResult{
			IncidentID: string(id),
			CheckID:    string(c.ID()),
			Action:     string(action),
			By:         req.By,
			Message:    fmt.Sprintf("Applied repair to %s (version %d), approved by %s", c.ID(), binding.Version, req.By),
		}, nil
	}

	if closedIncident != nil {
		return DecisionResult{}, fmt.Errorf("incident %q is already closed (%s)", id, closedIncident.State())
	}
	return DecisionResult{}, fmt.Errorf("no open incident called %q", id)
}

// RejectRepair records a human's rejection of a proposed repair.
func (s *Service) RejectRepair(ctx context.Context, id domain.IncidentID, req RejectRequest) (DecisionResult, error) {
	if strings.TrimSpace(req.By) == "" {
		return DecisionResult{}, errors.New("--by is required: a repair rejection must be attributable to a person")
	}

	checks, err := s.deps.Store.EnabledChecks(ctx)
	if err != nil {
		return DecisionResult{}, err
	}

	now := s.deps.Clock.Now()
	var closedIncident *domain.Incident
	for _, c := range checks {
		log, err := s.deps.Store.Incidents(ctx, c.ID())
		if err != nil {
			continue
		}
		for _, inc := range log.All() {
			if inc.ID() == id && !inc.State().Open() {
				closedIncident = inc
			}
		}
		inc, open := log.Current()
		if !open || inc.ID() != id {
			continue
		}

		if err := inc.Reject(req.By, req.Note, now); err != nil {
			return DecisionResult{}, err
		}

		err = s.deps.Store.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
			if err := tx.SaveIncident(ctx, inc); err != nil {
				return err
			}
			return tx.AppendAudit(ctx, domain.AuditBy(
				"decision:"+string(inc.ID()), now, req.By, domain.ActionRepairRejected,
				domain.SubjectIncident, string(inc.ID()),
				fmt.Sprintf("rejected repair for check %s: %s", c.ID(), req.Note),
			))
		})
		if err != nil {
			return DecisionResult{}, err
		}

		return DecisionResult{
			IncidentID: string(id),
			CheckID:    string(c.ID()),
			Action:     string(domain.ActionRepairRejected),
			By:         req.By,
			Message:    fmt.Sprintf("Rejected repair for %s. The check remains broken.", c.ID()),
		}, nil
	}

	if closedIncident != nil {
		return DecisionResult{}, fmt.Errorf("incident %q is already closed (%s)", id, closedIncident.State())
	}
	return DecisionResult{}, fmt.Errorf("no open incident called %q", id)
}

// GC executes retention garbage collection.
func (s *Service) GC(ctx context.Context, req GCRequest) (domain.Sweep, error) {
	pol := domain.DefaultRetention()
	if req.RunDays > 0 {
		pol.RunTTL = time.Duration(req.RunDays) * 24 * time.Hour
	}
	if req.IncidentDays > 0 {
		pol.IncidentTTL = time.Duration(req.IncidentDays) * 24 * time.Hour
	}
	if req.SnapshotsPerCheck > 0 {
		pol.SnapshotsPerCheck = req.SnapshotsPerCheck
	}
	if err := pol.Validate(); err != nil {
		return domain.Sweep{}, err
	}

	now := s.deps.Clock.Now()
	if req.DryRun {
		return s.deps.Store.PlanGC(ctx, pol, now)
	}
	return s.deps.Store.GC(ctx, pol, now)
}

// Backup creates a consistent database backup and optionally verifies it.
func (s *Service) Backup(ctx context.Context, req BackupRequest) (BackupResult, error) {
	dest := req.To
	if dest == "" {
		stamp := s.deps.Clock.Now().Format("20060102T150405Z")
		dest = filepath.Join(filepath.Dir(s.deps.DBPath), fmt.Sprintf("agentd-%s.db", stamp))
	}

	size, err := s.deps.Store.Backup(ctx, dest)
	if err != nil {
		return BackupResult{}, err
	}

	ver := 0
	if req.Verify {
		v, err := sqlite.VerifyBackup(ctx, dest)
		if err != nil {
			return BackupResult{}, fmt.Errorf("backup verification failed: %w", err)
		}
		ver = v
	}

	return BackupResult{
		Path:          dest,
		SizeBytes:     size,
		Verified:      req.Verify,
		SchemaVersion: ver,
	}, nil
}

// Audit returns recent audit trail events.
func (s *Service) Audit(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.deps.Store.AuditTrail(ctx, limit)
}

// formatUserFriendlyBreakage turns technical error codes into user terms.
func formatUserFriendlyBreakage(f domain.Failure) string {
	switch f.Class {
	case domain.ClassStructural:
		return fmt.Sprintf("Agentd could not find the target data. The source structure appears to have changed. (Technical detail: %s)", f.Summary)
	case domain.ClassAuth:
		return fmt.Sprintf("Access was denied: credentials were rejected by the remote host. (Technical detail: %s)", f.Summary)
	case domain.ClassRateLimited:
		return fmt.Sprintf("The remote host rate-limited Agentd's request. (Technical detail: %s)", f.Summary)
	case domain.ClassTransient:
		return fmt.Sprintf("A temporary network or server problem occurred. (Technical detail: %s)", f.Summary)
	default:
		return fmt.Sprintf("Check failed: %s", f.Summary)
	}
}

func parseLocators(inc *domain.Incident, spec string) ([]domain.Locator, error) {
	p := inc.Proposal()
	if p == nil {
		return nil, domain.ErrNoProposal
	}

	dialect := ""
	byTarget := map[string]domain.Locator{}
	for _, l := range p.Binding.Locators {
		byTarget[l.Target] = l
		dialect = l.Dialect
	}

	for _, pair := range strings.Split(spec, ",") {
		target, expr, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return nil, fmt.Errorf("locator wants target=expression, got %q", pair)
		}
		target, expr = strings.TrimSpace(target), strings.TrimSpace(expr)
		existing, known := byTarget[target]
		d := dialect
		if known {
			d = existing.Dialect
		}
		byTarget[target] = domain.Locator{Target: target, Dialect: d, Expression: expr}
	}

	targets := make([]string, 0, len(byTarget))
	for t := range byTarget {
		targets = append(targets, t)
	}
	sort.Strings(targets)

	out := make([]domain.Locator, 0, len(targets))
	for _, t := range targets {
		out = append(out, byTarget[t])
	}
	return out, nil
}

type clockAdapter struct {
	now func() time.Time
}

func (c clockAdapter) Now() time.Time { return c.now() }

func (c clockAdapter) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

type defaultIDs struct{}

func (defaultIDs) NewRunID() domain.RunID           { return domain.RunID(fmt.Sprintf("run_%d", time.Now().UnixNano())) }
func (defaultIDs) NewIncidentID() domain.IncidentID { return domain.IncidentID(fmt.Sprintf("inc_%d", time.Now().UnixNano())) }
func (defaultIDs) NewBindingID() domain.BindingID   { return domain.BindingID(fmt.Sprintf("bin_%d", time.Now().UnixNano())) }

// ExportMetrics refreshes check staleness for all enabled checks and returns Prometheus metrics.
func (s *Service) ExportMetrics() string {
	if s.deps.Store != nil {
		ctx := context.Background()
		checks, err := s.deps.Store.EnabledChecks(ctx)
		if err == nil {
			now := s.deps.Clock.Now()
			for _, c := range checks {
				var lastRun *domain.Run
				if runs, rErr := s.deps.Store.RecentRuns(ctx, c.ID(), 1); rErr == nil && len(runs) > 0 {
					lastRun = runs[0]
				}
				st := domain.CheckStaleness(c, lastRun, now, 1.0)
				staleness := 0.0
				if st.Elapsed > st.ExpectedInterval {
					staleness = (st.Elapsed - st.ExpectedInterval).Seconds()
				}
				if s.deps.Metrics != nil {
					s.deps.Metrics.SetCheckStaleness(string(c.ID()), staleness)
				}
			}
		}
	}

	if exp, ok := s.deps.Metrics.(interface{ Export() string }); ok {
		return exp.Export()
	}
	return ""
}

