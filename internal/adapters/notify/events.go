package notify

import (
	"fmt"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
)

// Changed constructs a notification for a check whose extracted value changed.
func Changed(checkID domain.CheckID, dest domain.Destination, name string, explanation string, at time.Time) domain.Notification {
	subject := fmt.Sprintf("%s changed", name)
	body := explanation
	if body == "" {
		body = fmt.Sprintf("Agentd detected a new value for %s.", name)
	}
	return domain.Notification{
		CheckID:     checkID,
		Destination: dest,
		Severity:    domain.SeverityInfo,
		Subject:     subject,
		Body:        body,
		OccurredAt:  at,
	}
}

// Degraded constructs a notification for a check that succeeded with reduced confidence.
func Degraded(checkID domain.CheckID, dest domain.Destination, name string, explanation string, at time.Time) domain.Notification {
	subject := fmt.Sprintf("%s was checked with reduced confidence", name)
	body := explanation
	if body == "" {
		body = fmt.Sprintf("Agentd extracted data for %s, but encountered partial issues or retryable degradation.", name)
	}
	return domain.Notification{
		CheckID:     checkID,
		Destination: dest,
		Severity:    domain.SeverityWarning,
		Subject:     subject,
		Body:        body,
		OccurredAt:  at,
	}
}

// Failed constructs a human-readable notification for a failed check.
// It explains what happened in user terms rather than raw exception text.
func Failed(checkID domain.CheckID, dest domain.Destination, name string, f domain.Failure, at time.Time) domain.Notification {
	subject := fmt.Sprintf("%s check failed", name)
	var body string

	switch f.Class {
	case domain.ClassStructural:
		body = fmt.Sprintf("Agentd could not find %s. The source structure appears to have changed.\n(Technical detail: %s)", name, f.Summary)
	case domain.ClassAuth:
		body = fmt.Sprintf("Agentd could not access the source for %s: access was denied or credentials were rejected.\n(Technical detail: %s)", name, f.Summary)
	case domain.ClassRateLimited:
		body = fmt.Sprintf("The source for %s rate-limited Agentd's request. Agentd will back off automatically.\n(Technical detail: %s)", name, f.Summary)
	case domain.ClassTransient:
		body = fmt.Sprintf("A temporary network or server problem occurred while checking %s. Agentd will retry.\n(Technical detail: %s)", name, f.Summary)
	default:
		body = fmt.Sprintf("Agentd encountered an issue checking %s: %s", name, f.Summary)
	}

	if f.Detail != "" {
		body += fmt.Sprintf("\nDetail: %s", f.Detail)
	}

	return domain.Notification{
		CheckID:     checkID,
		Destination: dest,
		Severity:    domain.SeverityAlert,
		Subject:     subject,
		Body:        body,
		OccurredAt:  at,
	}
}

// RepairProposal constructs the notification asking an operator to decide on a repair.
func RepairProposal(checkID domain.CheckID, dest domain.Destination, name string, incidentID domain.IncidentID, rationale string, at time.Time) domain.Notification {
	subject := fmt.Sprintf("Approve a repair for %s?", name)
	body := fmt.Sprintf("%s\n\nAgentd has verified this candidate against historical data and confirmed it reproduces your intent. Nothing has been changed. An operator must approve it before it is applied.", rationale)

	return domain.Notification{
		CheckID:       checkID,
		Destination:   dest,
		Severity:      domain.SeverityAlert,
		Subject:       subject,
		Body:          body,
		NeedsDecision: true,
		IncidentID:    incidentID,
		OccurredAt:    at,
	}
}

// Unhealable constructs a notification when repair attempts are exhausted.
func Unhealable(checkID domain.CheckID, dest domain.Destination, name string, incidentID domain.IncidentID, attempts int, at time.Time) domain.Notification {
	subject := fmt.Sprintf("Repair failed for %s: manual intervention required", name)
	body := fmt.Sprintf("Agentd could not automatically repair %s. All %d candidate generation attempts were exhausted without passing verification gates.\n\nAutomatic healing is paused for this check. An operator must update the check configuration or locators manually.\nIncident ID: %s", name, attempts, incidentID)

	return domain.Notification{
		CheckID:     checkID,
		Destination: dest,
		Severity:    domain.SeverityAlert,
		Subject:     subject,
		Body:        body,
		IncidentID:  incidentID,
		OccurredAt:  at,
	}
}

// Staleness constructs a watchdog notification when a check has missed its schedule.
func Staleness(checkID domain.CheckID, dest domain.Destination, name string, expected time.Duration, overdue time.Duration, at time.Time) domain.Notification {
	subject := fmt.Sprintf("%s is stale", name)
	body := fmt.Sprintf("Agentd expected %s to run every %s, but no completed run has been recorded in %s.\n\nPlease verify that the agentd daemon is running and has network connectivity.", name, expected, overdue)

	return domain.Notification{
		CheckID:     checkID,
		Destination: dest,
		Severity:    domain.SeverityAlert,
		Subject:     subject,
		Body:        body,
		OccurredAt:  at,
	}
}

// HealingStorm constructs a notification when the global circuit breaker triggers.
func HealingStorm(openIncidents int, at time.Time) domain.Notification {
	subject := "Healing paused: circuit breaker triggered"
	body := fmt.Sprintf("%d checks are currently broken simultaneously. Automatic healing has been paused across all checks to prevent burning model budget during a widespread site outage or network disruption.\n\nManual review of external upstream systems is recommended.", openIncidents)

	return domain.Notification{
		CheckID:    domain.CheckID("system"),
		Severity:   domain.SeverityAlert,
		Subject:    subject,
		Body:       body,
		OccurredAt: at,
	}
}

// BudgetExhaustion constructs a notification when the candidate generation budget is reached.
func BudgetExhaustion(checkID domain.CheckID, dest domain.Destination, name string, reason string, at time.Time) domain.Notification {
	subject := fmt.Sprintf("Model budget limit reached for %s", name)
	body := fmt.Sprintf("Automatic candidate generation was paused for %s: %s.\n\nTo conserve model token budget, further repair attempts will require operator action.", name, reason)

	return domain.Notification{
		CheckID:     checkID,
		Destination: dest,
		Severity:    domain.SeverityAlert,
		Subject:     subject,
		Body:        body,
		OccurredAt:  at,
	}
}

// StoreUnhealthy constructs a notification when the underlying database is corrupt or failing.
func StoreUnhealthy(err error, at time.Time) domain.Notification {
	subject := "Agentd database unhealthy"
	body := fmt.Sprintf("Agentd encountered a database storage error: %v.\n\nPlease check disk availability, permissions, and database integrity (e.g. agentd status or sqlite3 integrity_check).", err)

	return domain.Notification{
		CheckID:    domain.CheckID("system"),
		Severity:   domain.SeverityAlert,
		Subject:    subject,
		Body:       body,
		OccurredAt: at,
	}
}
