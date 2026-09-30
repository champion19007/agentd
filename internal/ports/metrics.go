package ports

// Metrics defines the monitoring and observability port for Agentd.
//
// The core emits metrics through this port without knowing how they are stored
// or exported. Implementations must avoid high-cardinality labels and must
// never record sensitive payload data or secret credentials.
type Metrics interface {
	// RecordRun records a completed terminal run for a check in a given state.
	RecordRun(check string, state string)

	// RecordRunDuration records the execution duration in seconds for a specific
	// stage of a check's run (e.g. fetch, extract, evaluate, persist, total).
	RecordRunDuration(check string, stage string, durationSeconds float64)

	// SetCheckStaleness sets the observation staleness in seconds for a check.
	// Staleness is the primary SLI, measuring whether checks produce terminal
	// runs within their expected cadence.
	SetCheckStaleness(check string, stalenessSeconds float64)

	// RecordModelUsage records token counts and estimated cost in micros
	// for model completions by model name and purpose.
	RecordModelUsage(model string, purpose string, tokens int, costMicros int64)

	// RecordIncident records an incident lifecycle outcome for a check
	// (e.g. opened, proposed, approved, rejected, unhealable).
	RecordIncident(check string, outcome string)

	// RecordIncidentResolution records the time taken to resolve an incident in seconds.
	RecordIncidentResolution(durationSeconds float64)

	// SetQueueDepth sets the current number of pending/queued runs in the worker pool.
	SetQueueDepth(depth int)

	// RecordPluginCall records an MCP plugin execution call result (e.g. ok, error, timeout).
	RecordPluginCall(plugin string, result string)
}

// NoopMetrics is a null object implementation of Metrics that drops all signals.
type NoopMetrics struct{}

func (NoopMetrics) RecordRun(string, string)                    {}
func (NoopMetrics) RecordRunDuration(string, string, float64)   {}
func (NoopMetrics) SetCheckStaleness(string, float64)           {}
func (NoopMetrics) RecordModelUsage(string, string, int, int64) {}
func (NoopMetrics) RecordIncident(string, string)               {}
func (NoopMetrics) RecordIncidentResolution(float64)            {}
func (NoopMetrics) SetQueueDepth(int)                           {}
func (NoopMetrics) RecordPluginCall(string, string)             {}

var _ Metrics = NoopMetrics{}
