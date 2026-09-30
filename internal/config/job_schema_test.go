package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/config"
)

func TestJobSchema_LoadV1MigratesForward(t *testing.T) {
	v1JSON := `{
		"version": 1,
		"id": "pricing-check",
		"name": "Acme Pricing Table",
		"url": "https://example.com/pricing",
		"interval": "15m",
		"selector": ".pricing-card",
		"target": "plan_price",
		"notify_url": "https://hooks.slack.com/services/test",
		"retries": 4
	}`

	job, migrated, err := config.LoadJob([]byte(v1JSON))
	if err != nil {
		t.Fatalf("unexpected error loading v1 job: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true when loading v1 job, got false")
	}

	// Verify migrated to v2 schema fields
	if job.SchemaVersion != config.CurrentJobSchemaVersion {
		t.Errorf("expected schema_version=%d, got %d", config.CurrentJobSchemaVersion, job.SchemaVersion)
	}
	if job.ID != "pricing-check" {
		t.Errorf("expected ID='pricing-check', got %q", job.ID)
	}
	if job.Source.Kind != "http" || job.Source.URL != "https://example.com/pricing" {
		t.Errorf("unexpected migrated source: %+v", job.Source)
	}
	if job.Schedule.Interval != "15m" || job.Schedule.CatchUp != "once" {
		t.Errorf("unexpected migrated schedule: %+v", job.Schedule)
	}
	if job.Binding.Dialect != "css" || job.Binding.Expression != ".pricing-card" || job.Binding.Target != "plan_price" {
		t.Errorf("unexpected migrated binding: %+v", job.Binding)
	}
	if job.Destination.Kind != "notify" || job.Destination.Target != "https://hooks.slack.com/services/test" {
		t.Errorf("unexpected migrated destination: %+v", job.Destination)
	}
	if job.Policy.MaxRetries != 4 {
		t.Errorf("expected max_retries=4, got %d", job.Policy.MaxRetries)
	}

	// Verify that saving writes out in current v2 format
	saved, err := job.Save()
	if err != nil {
		t.Fatalf("saving migrated job failed: %v", err)
	}
	if !strings.Contains(string(saved), `"schema_version": 2`) {
		t.Errorf("expected saved output to contain schema_version: 2, got:\n%s", string(saved))
	}
}

func TestJobSchema_LoadV2Preserved(t *testing.T) {
	v2JSON := `{
		"schema_version": 2,
		"id": "stock-feed",
		"name": "Inventory Levels",
		"source": {
			"kind": "http",
			"url": "https://example.com/stock",
			"method": "GET"
		},
		"schedule": {
			"interval": "30m",
			"jitter": 0.05,
			"catch_up": "backfill"
		},
		"intent": {
			"kind": "scalar",
			"name": "Inventory Levels"
		},
		"binding": {
			"dialect": "css",
			"target": "quantity",
			"expression": "#stock-count"
		},
		"destination": {
			"kind": "notify",
			"target": "https://alerts.internal.net/webhook",
			"on_quiet": true
		},
		"policy": {
			"priority": 10,
			"max_repair_attempts": 2,
			"retain_snapshots": 8,
			"max_retries": 5
		}
	}`

	job, migrated, err := config.LoadJob([]byte(v2JSON))
	if err != nil {
		t.Fatalf("unexpected error loading v2 job: %v", err)
	}
	if migrated {
		t.Fatal("expected migrated=false when loading native v2 job, got true")
	}

	if job.SchemaVersion != 2 {
		t.Errorf("expected schema_version=2, got %d", job.SchemaVersion)
	}
	if job.Schedule.CatchUp != "backfill" {
		t.Errorf("expected catch_up=backfill, got %q", job.Schedule.CatchUp)
	}
	if !job.Destination.OnQuiet {
		t.Error("expected on_quiet=true")
	}
}

func TestJobSchema_RejectFutureAndInvalidVersions(t *testing.T) {
	futureJSON := `{"schema_version": 99, "name": "Future Check"}`
	_, _, err := config.LoadJob([]byte(futureJSON))
	if err == nil || !errors.Is(err, config.ErrFutureVersion) {
		t.Errorf("expected ErrFutureVersion for version 99, got: %v", err)
	}

	invalidJSON := `{"schema_version": -1, "name": "Invalid Check"}`
	_, _, err = config.LoadJob([]byte(invalidJSON))
	if err == nil || !errors.Is(err, config.ErrUnsupportedVersion) {
		t.Errorf("expected ErrUnsupportedVersion for version -1, got: %v", err)
	}
}

func TestJobSchema_DomainRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	orig := &config.JobDefinition{
		SchemaVersion: config.CurrentJobSchemaVersion,
		ID:            "catalog-watch",
		Name:          "Product Catalog",
		Source: config.SourceConfig{
			Kind: "http",
			URL:  "https://shop.example.com/items",
		},
		Schedule: config.ScheduleConfig{
			Interval: "1h",
			CatchUp:  "once",
		},
		Intent: config.IntentConfig{
			Kind: "scalar",
			Name: "Product Catalog",
		},
		Binding: config.BindingConfig{
			Dialect:    "css",
			Target:     "count",
			Expression: ".item-row",
		},
		Destination: config.DestinationConfig{
			Kind:   "notify",
			Target: "https://notify.org/webhook",
		},
		Policy: config.PolicyConfig{
			Priority:          5,
			MaxRepairAttempts: 3,
			RetainSnapshots:   5,
			MaxRetries:        3,
		},
	}

	check, binding, err := orig.ToDomain(now)
	if err != nil {
		t.Fatalf("ToDomain failed: %v", err)
	}

	if check.ID() != "catalog-watch" {
		t.Errorf("expected check ID 'catalog-watch', got %s", check.ID())
	}
	if len(binding.Locators) != 1 || binding.Locators[0].Expression != ".item-row" {
		t.Errorf("unexpected binding: %+v", binding)
	}

	rebuilt := config.FromDomain(check, binding)
	if rebuilt.SchemaVersion != config.CurrentJobSchemaVersion {
		t.Errorf("expected rebuilt schema version %d, got %d", config.CurrentJobSchemaVersion, rebuilt.SchemaVersion)
	}
	if rebuilt.ID != orig.ID || rebuilt.Name != orig.Name {
		t.Errorf("mismatch in rebuilt job: %+v", rebuilt)
	}
}
