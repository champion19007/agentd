package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

// Supported job schema versions.
//
// Forward-Only Schema Migration Policy:
// Agentd supports the current schema version (v2) and exactly one previous version (v1).
//
// 1. Loading: When a job configuration file or payload in schema version 1 is loaded,
//    it is automatically migrated forward in-memory to schema version 2 (migrated=true).
// 2. Writing: If the job definition is subsequently edited, updated, or saved back by
//    user action or CLI/API mutation, it will ALWAYS be serialized and written back
//    exclusively in the current schema version (v2).
// 3. Backward Compatibility: Backward migration from v2 to v1 is intentionally unsupported.
//    Operators should be aware that once an older job file is edited by Agentd, its on-disk
//    format is promoted to v2.
const (
	CurrentJobSchemaVersion      = 2
	PreviousJobSchemaVersion     = 1
	MinSupportedJobSchemaVersion = 1
)

var (
	// ErrUnsupportedVersion indicates a schema version older than supported.
	ErrUnsupportedVersion = errors.New("config: unsupported job schema version")
	// ErrFutureVersion indicates a schema version newer than supported.
	ErrFutureVersion = errors.New("config: schema version is newer than supported current version")
)

// SourceConfig configures source access in a JobDefinition.
type SourceConfig struct {
	Kind          string                       `json:"kind"`
	URL           string                       `json:"url,omitempty"`
	Method        string                       `json:"method,omitempty"`
	Headers       map[string]string            `json:"headers,omitempty"`
	SecretHeaders map[string]domain.SecretRef  `json:"secret_headers,omitempty"`
	Plugin        string                       `json:"plugin,omitempty"`
	PluginArgs    map[string]string            `json:"plugin_args,omitempty"`
}

// ScheduleConfig configures check cadence in a JobDefinition.
type ScheduleConfig struct {
	Interval string  `json:"interval"`
	Jitter   float64 `json:"jitter,omitempty"`
	CatchUp  string  `json:"catch_up,omitempty"`
}

// IntentConfig configures what the check extracts.
type IntentConfig struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// BindingConfig configures locators for extracting data.
type BindingConfig struct {
	Dialect    string `json:"dialect"`
	Target     string `json:"target"`
	Expression string `json:"expression"`
}

// DestinationConfig configures where results and alerts are sent.
type DestinationConfig struct {
	Kind    string           `json:"kind,omitempty"`
	Target  string           `json:"target,omitempty"`
	Secret  domain.SecretRef `json:"secret,omitempty"`
	OnQuiet bool             `json:"on_quiet,omitempty"`
}

// PolicyConfig tunes retry and repair policies.
type PolicyConfig struct {
	Priority          int    `json:"priority,omitempty"`
	MaxRepairAttempts int    `json:"max_repair_attempts,omitempty"`
	RetainSnapshots   int    `json:"retain_snapshots,omitempty"`
	MaxRetries        int    `json:"max_retries,omitempty"`
	RetryBackoff      string `json:"retry_backoff,omitempty"`
}

// JobDefinition is the current (v2) canonical schema for monitored checks.
type JobDefinition struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Source        SourceConfig      `json:"source"`
	Schedule      ScheduleConfig    `json:"schedule"`
	Intent        IntentConfig      `json:"intent"`
	Binding       BindingConfig     `json:"binding"`
	Destination   DestinationConfig `json:"destination,omitempty"`
	Policy        PolicyConfig      `json:"policy,omitempty"`
}

// JobDefinitionV1 is the previous (v1) schema supported for backward compatibility.
type JobDefinitionV1 struct {
	Version   int    `json:"version,omitempty"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Interval  string `json:"interval"`
	Selector  string `json:"selector"`
	Target    string `json:"target,omitempty"`
	NotifyURL string `json:"notify_url,omitempty"`
	Retries   int    `json:"retries,omitempty"`
}

type versionHeader struct {
	SchemaVersion int `json:"schema_version"`
	Version       int `json:"version"`
}

// LoadJob decodes job data. If the data is formatted under the previous schema
// version (v1), it is automatically migrated forward to the current schema (v2).
// Returns the current v2 definition, whether a migration occurred, and any error.
func LoadJob(data []byte) (*JobDefinition, bool, error) {
	var hdr versionHeader
	if err := json.Unmarshal(data, &hdr); err != nil {
		return nil, false, fmt.Errorf("config: parsing job header: %w", err)
	}

	ver := hdr.SchemaVersion
	if ver == 0 {
		ver = hdr.Version
	}
	if ver == 0 {
		// Default unversioned payload to v1 (legacy format)
		ver = PreviousJobSchemaVersion
	}

	if ver < MinSupportedJobSchemaVersion {
		return nil, false, fmt.Errorf("%w: version %d (minimum is %d)", ErrUnsupportedVersion, ver, MinSupportedJobSchemaVersion)
	}
	if ver > CurrentJobSchemaVersion {
		return nil, false, fmt.Errorf("%w: version %d (current is %d)", ErrFutureVersion, ver, CurrentJobSchemaVersion)
	}

	switch ver {
	case PreviousJobSchemaVersion:
		var v1 JobDefinitionV1
		if err := json.Unmarshal(data, &v1); err != nil {
			return nil, false, fmt.Errorf("config: parsing v1 job: %w", err)
		}
		migrated := migrateV1ToV2(v1)
		return migrated, true, nil

	case CurrentJobSchemaVersion:
		var v2 JobDefinition
		if err := json.Unmarshal(data, &v2); err != nil {
			return nil, false, fmt.Errorf("config: parsing v2 job: %w", err)
		}
		if v2.SchemaVersion != CurrentJobSchemaVersion {
			v2.SchemaVersion = CurrentJobSchemaVersion
		}
		if err := v2.Validate(); err != nil {
			return nil, false, err
		}
		return &v2, false, nil

	default:
		return nil, false, fmt.Errorf("%w: %d", ErrUnsupportedVersion, ver)
	}
}

// LoadJobFile reads and parses a job configuration file, migrating forward if v1.
func LoadJobFile(path string) (*JobDefinition, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("config: reading job file %q: %w", path, err)
	}
	return LoadJob(data)
}

// migrateV1ToV2 upgrades a v1 job definition to current v2 format.
func migrateV1ToV2(v1 JobDefinitionV1) *JobDefinition {
	target := v1.Target
	if target == "" {
		target = "value"
	}
	interval := v1.Interval
	if interval == "" {
		interval = "10m"
	}

	var dest DestinationConfig
	if v1.NotifyURL != "" {
		dest = DestinationConfig{
			Kind:   "notify",
			Target: v1.NotifyURL,
		}
	} else {
		dest = DestinationConfig{Kind: "none"}
	}

	retries := v1.Retries
	if retries <= 0 {
		retries = domain.DefaultMaxRetries
	}

	return &JobDefinition{
		SchemaVersion: CurrentJobSchemaVersion,
		ID:            v1.ID,
		Name:          v1.Name,
		Source: SourceConfig{
			Kind:   "http",
			URL:    v1.URL,
			Method: "GET",
		},
		Schedule: ScheduleConfig{
			Interval: interval,
			CatchUp:  "once",
		},
		Intent: IntentConfig{
			Kind: "scalar",
			Name: v1.Name,
		},
		Binding: BindingConfig{
			Dialect:    "css",
			Target:     target,
			Expression: v1.Selector,
		},
		Destination: dest,
		Policy: PolicyConfig{
			MaxRetries:        retries,
			MaxRepairAttempts: domain.DefaultMaxRepairAttempts,
			RetainSnapshots:   5,
		},
	}
}

// Validate checks that the v2 job definition is valid.
func (j *JobDefinition) Validate() error {
	if strings.TrimSpace(j.Name) == "" {
		return errors.New("config: job name is required")
	}
	if strings.TrimSpace(j.Source.URL) == "" && strings.TrimSpace(j.Source.Plugin) == "" {
		return errors.New("config: job source must specify either URL or Plugin")
	}
	if strings.TrimSpace(j.Schedule.Interval) == "" {
		return errors.New("config: job schedule interval is required")
	}
	if _, err := time.ParseDuration(j.Schedule.Interval); err != nil {
		return fmt.Errorf("config: invalid schedule interval %q: %w", j.Schedule.Interval, err)
	}
	if strings.TrimSpace(j.Binding.Expression) == "" {
		return errors.New("config: job binding expression is required")
	}
	return nil
}

// Save marshals the job definition in its current migrated (v2) form.
func (j *JobDefinition) Save() ([]byte, error) {
	j.SchemaVersion = CurrentJobSchemaVersion
	if err := j.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(j, "", "  ")
}

// SaveFile writes the job definition in current v2 format to the given path.
func (j *JobDefinition) SaveFile(path string) error {
	data, err := j.Save()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// ToDomain converts a JobDefinition into a domain Check and initial Binding.
func (j *JobDefinition) ToDomain(now time.Time) (*domain.Check, domain.Binding, error) {
	if err := j.Validate(); err != nil {
		return nil, domain.Binding{}, err
	}

	interval, _ := time.ParseDuration(j.Schedule.Interval)
	var catchUp domain.CatchUpPolicy
	switch strings.ToLower(j.Schedule.CatchUp) {
	case "skip":
		catchUp = domain.CatchUpSkip
	case "backfill":
		catchUp = domain.CatchUpBackfill
	default:
		catchUp = domain.CatchUpOnce
	}

	sched := domain.Schedule{
		Interval: interval,
		Jitter:   j.Schedule.Jitter,
		CatchUp:  catchUp,
	}

	var srcKind domain.SourceKind
	if j.Source.Plugin != "" {
		srcKind = domain.SourcePlugin
	} else {
		srcKind = domain.SourceHTTP
	}

	method := j.Source.Method
	if method == "" {
		method = "GET"
	}

	source := domain.SourceSpec{
		Kind:          srcKind,
		URL:           j.Source.URL,
		Method:        method,
		Headers:       j.Source.Headers,
		SecretHeaders: j.Source.SecretHeaders,
		Plugin:        j.Source.Plugin,
		PluginArgs:    j.Source.PluginArgs,
	}

	var dest domain.Destination
	if strings.ToLower(j.Destination.Kind) == "none" || (j.Destination.Kind == "" && j.Destination.Target == "") {
		dest = domain.Destination{Kind: domain.DestinationNone}
	} else {
		dest = domain.Destination{
			Kind:    domain.DestinationNotify,
			Target:  j.Destination.Target,
			Secret:  j.Destination.Secret,
			OnQuiet: j.Destination.OnQuiet,
		}
	}

	var retryBackoff time.Duration
	if j.Policy.RetryBackoff != "" {
		retryBackoff, _ = time.ParseDuration(j.Policy.RetryBackoff)
	}

	policy := domain.Policy{
		Priority:          j.Policy.Priority,
		MaxRepairAttempts: j.Policy.MaxRepairAttempts,
		RetainSnapshots:   j.Policy.RetainSnapshots,
		MaxRetries:        j.Policy.MaxRetries,
		RetryBackoff:      retryBackoff,
	}

	intent := domain.ScalarIntent{
		Label:   j.Name,
		Purpose: "Monitored scalar intent for " + j.Name,
		Type:    domain.TypeString,
	}

	def := domain.Definition{
		Version:     1,
		Intent:      intent,
		Source:      source,
		Schedule:    sched,
		Destination: dest,
		Policy:      policy,
		CreatedAt:   now.UTC(),
	}

	checkID := domain.CheckID(j.ID)
	if checkID == "" {
		checkID = domain.CheckID(strings.ToLower(strings.ReplaceAll(j.Name, " ", "-")))
	}

	check, err := domain.NewCheck(checkID, def)
	if err != nil {
		return nil, domain.Binding{}, fmt.Errorf("config: building check: %w", err)
	}

	dialect := j.Binding.Dialect
	if dialect == "" {
		dialect = "css"
	}
	target := j.Binding.Target
	if target == "" {
		target = "value"
	}

	binding := domain.Binding{
		ID:                domain.BindingID(fmt.Sprintf("%s-b1", checkID)),
		CheckID:           checkID,
		DefinitionVersion: 1,
		IntentKind:        domain.IntentScalar,
		Version:           1,
		Origin:            domain.OriginInferred,
		Locators: []domain.Locator{
			{
				Target:     target,
				Dialect:    dialect,
				Expression: j.Binding.Expression,
			},
		},
		DerivedAt: now.UTC(),
	}

	return check, binding, nil
}

// FromDomain creates a current v2 JobDefinition from a Check and its active Binding.
func FromDomain(c *domain.Check, b domain.Binding) *JobDefinition {
	def := c.ActiveDefinition()
	expr := ""
	dialect := "css"
	target := "value"
	if len(b.Locators) > 0 {
		expr = b.Locators[0].Expression
		dialect = b.Locators[0].Dialect
		target = b.Locators[0].Target
	}

	return &JobDefinition{
		SchemaVersion: CurrentJobSchemaVersion,
		ID:            string(c.ID()),
		Name:          def.Intent.Name(),
		Source: SourceConfig{
			Kind:          string(def.Source.Kind),
			URL:           def.Source.URL,
			Method:        def.Source.Method,
			Headers:       def.Source.Headers,
			SecretHeaders: def.Source.SecretHeaders,
			Plugin:        def.Source.Plugin,
			PluginArgs:    def.Source.PluginArgs,
		},
		Schedule: ScheduleConfig{
			Interval: def.Schedule.Interval.String(),
			Jitter:   def.Schedule.Jitter,
			CatchUp:  string(def.Schedule.CatchUpPolicy()),
		},
		Intent: IntentConfig{
			Kind: string(def.Intent.Kind()),
			Name: def.Intent.Name(),
		},
		Binding: BindingConfig{
			Dialect:    dialect,
			Target:     target,
			Expression: expr,
		},
		Destination: DestinationConfig{
			Kind:    string(def.Destination.Kind),
			Target:  def.Destination.Target,
			Secret:  def.Destination.Secret,
			OnQuiet: def.Destination.OnQuiet,
		},
		Policy: PolicyConfig{
			Priority:          def.Policy.Priority,
			MaxRepairAttempts: def.Policy.Attempts(),
			RetainSnapshots:   def.Policy.RetainSnapshots,
			MaxRetries:        def.Policy.Retries(),
			RetryBackoff:      def.Policy.RetryBackoff.String(),
		},
	}
}
