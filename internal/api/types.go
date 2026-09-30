package api

import (
	"context"

	"github.com/champion19007/agentd/internal/core/domain"
)

// Operations defines the high-level application boundary for Agentd.
// Both the local HTTP API and the CLI drive through this interface.
type Operations interface {
	Init(ctx context.Context, req InitRequest) (InitResponse, error)
	Status(ctx context.Context) (StatusResponse, error)

	AddCheck(ctx context.Context, req AddCheckRequest) (CheckSummary, error)
	ListChecks(ctx context.Context) ([]CheckSummary, error)
	GetCheck(ctx context.Context, id domain.CheckID) (CheckDetail, error)
	RunCheck(ctx context.Context, id domain.CheckID) (RunSummary, error)
	DeleteCheck(ctx context.Context, id domain.CheckID) error

	ListRuns(ctx context.Context, checkID domain.CheckID, limit int) ([]RunSummary, error)
	GetRun(ctx context.Context, id domain.RunID) (RunDetail, error)

	ListIncidents(ctx context.Context) ([]IncidentSummary, error)
	GetIncident(ctx context.Context, id domain.IncidentID) (IncidentDetail, error)

	ApproveRepair(ctx context.Context, id domain.IncidentID, req ApproveRequest) (DecisionResult, error)
	RejectRepair(ctx context.Context, id domain.IncidentID, req RejectRequest) (DecisionResult, error)

	GC(ctx context.Context, req GCRequest) (domain.Sweep, error)
	Backup(ctx context.Context, req BackupRequest) (BackupResult, error)
	Audit(ctx context.Context, limit int) ([]domain.AuditEvent, error)
	ExportMetrics() string
}

// InitRequest configures database initialization.
type InitRequest struct {
	Path string `json:"path,omitempty"`
}

// InitResponse reports the result of database initialization.
type InitResponse struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Message string `json:"message"`
}

// StatusResponse is the system-wide health and metric summary.
type StatusResponse struct {
	Healthy             bool    `json:"healthy"`
	DBPath              string  `json:"db_path"`
	SchemaVersion       int     `json:"schema_version"`
	Integrity           string  `json:"integrity"`
	ChecksTotal         int     `json:"checks_total"`
	ChecksEnabled       int     `json:"checks_enabled"`
	OpenIncidents       int     `json:"open_incidents"`
	AwaitingApproval    int     `json:"awaiting_approval"`
	RecentRuns          int     `json:"recent_runs"`
	UndeliveredNotifs   int     `json:"undelivered_notifications"`
	FreshnessSummary    string  `json:"freshness_summary,omitempty"`
	MaxStalenessSeconds float64 `json:"max_staleness_seconds"`
	Message             string  `json:"message"`
}

// AddCheckRequest creates a new check and initial binding.
type AddCheckRequest struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Interval    string `json:"interval,omitempty"`
	Shape       string `json:"shape,omitempty"`
	Target      string `json:"target,omitempty"`
	Expression  string `json:"expression,omitempty"`
	Dialect     string `json:"dialect,omitempty"`
	Destination string `json:"destination,omitempty"`
	DestTarget  string `json:"dest_target,omitempty"`
	OnQuiet     bool   `json:"on_quiet,omitempty"`
}

// CheckSummary is a concise listing of a check.
type CheckSummary struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	URL              string  `json:"url"`
	Interval         string  `json:"interval"`
	Enabled          bool    `json:"enabled"`
	LastRunState     string  `json:"last_run_state,omitempty"`
	LastRunTime      string  `json:"last_run_time,omitempty"`
	FreshnessSeconds float64 `json:"freshness_seconds"`
	StalenessSeconds float64 `json:"staleness_seconds"`
}

// CheckDetail gives the complete configuration and state of a check.
type CheckDetail struct {
	CheckSummary
	Version     int              `json:"version"`
	Shape       string           `json:"shape"`
	Locators    []domain.Locator `json:"locators"`
	RecentRuns  []RunSummary     `json:"recent_runs,omitempty"`
}

// RunSummary is a concise record of a finished check execution.
type RunSummary struct {
	ID          string `json:"id"`
	CheckID     string `json:"check_id"`
	Slot        int64  `json:"slot"`
	State       string `json:"state"`
	Explanation string `json:"explanation"`
	When        string `json:"when"`
	TraceID     string `json:"trace_id,omitempty"`
}

// RunDetail provides comprehensive information about a single run.
type RunDetail struct {
	RunSummary
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at"`
	PayloadHash    string `json:"payload_hash,omitempty"`
	FailureSummary string `json:"failure_summary,omitempty"`
	FailureClass   string `json:"failure_class,omitempty"`
}

// IncidentSummary describes an open breakage incident.
type IncidentSummary struct {
	ID            string `json:"id"`
	CheckID       string `json:"check_id"`
	State         string `json:"state"`
	WhatBroke     string `json:"what_broke"`
	OpenedAt      string `json:"opened_at"`
	AttemptsLeft  int    `json:"attempts_remaining"`
	HasProposal   bool   `json:"has_proposal"`
}

// GateResultSummary describes the outcome of one verification gate.
type GateResultSummary struct {
	Gate   string `json:"gate"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// IncidentDetail explains what happened, what Agentd found, what it proposes,
// verification results, and what action is required.
type IncidentDetail struct {
	IncidentSummary
	WhatAgentdFound     string              `json:"what_agentd_found,omitempty"`
	WhatItProposes      string              `json:"what_it_proposes,omitempty"`
	ProposedLocators    []domain.Locator    `json:"proposed_locators,omitempty"`
	OldLocators         []domain.Locator    `json:"old_locators,omitempty"`
	Diff                string              `json:"diff,omitempty"`
	VerificationResults []GateResultSummary `json:"verification_results,omitempty"`
	ActionRequired      string              `json:"action_required"`
}

// ApproveRequest records a human operator's approval of a repair proposal.
type ApproveRequest struct {
	By       string `json:"by"`
	Locators string `json:"locators,omitempty"`
	Note     string `json:"note,omitempty"`
}

// RejectRequest records a human operator's rejection of a repair proposal.
type RejectRequest struct {
	By   string `json:"by"`
	Note string `json:"note,omitempty"`
}

// DecisionResult reports the outcome of an approval or rejection.
type DecisionResult struct {
	IncidentID string `json:"incident_id"`
	CheckID    string `json:"check_id"`
	Action     string `json:"action"`
	By         string `json:"by"`
	Message    string `json:"message"`
}

// GCRequest tunes garbage collection.
type GCRequest struct {
	RunDays           int  `json:"run_days,omitempty"`
	IncidentDays      int  `json:"incident_days,omitempty"`
	SnapshotsPerCheck int  `json:"snapshots_per_check,omitempty"`
	DryRun            bool `json:"dry_run,omitempty"`
}

// BackupRequest configures a database backup.
type BackupRequest struct {
	To     string `json:"to,omitempty"`
	Verify bool   `json:"verify,omitempty"`
}

// BackupResult describes a completed backup.
type BackupResult struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"size_bytes"`
	Verified      bool   `json:"verified"`
	SchemaVersion int    `json:"schema_version,omitempty"`
}
