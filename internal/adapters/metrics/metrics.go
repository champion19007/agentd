package metrics

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/champion19007/agentd/internal/ports"
)

// DefaultHistogramBuckets provides bucket boundaries in seconds for run durations.
var DefaultRunDurationBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0}

// DefaultIncidentResolutionBuckets provides bucket boundaries in seconds for incident resolution.
var DefaultIncidentResolutionBuckets = []float64{1.0, 5.0, 15.0, 60.0, 300.0, 900.0, 1800.0, 3600.0, 86400.0}

// histogram holds internal counts, sum and cumulative bucket counts.
type histogram struct {
	buckets []float64
	counts  []uint64
	count   uint64
	sum     float64
}

func newHistogram(boundaries []float64) *histogram {
	b := make([]float64, len(boundaries))
	copy(b, boundaries)
	sort.Float64s(b)
	return &histogram{
		buckets: b,
		counts:  make([]uint64, len(b)),
	}
}

func (h *histogram) observe(v float64) {
	h.count++
	h.sum += v
	for i, upper := range h.buckets {
		if v <= upper {
			h.counts[i]++
		}
	}
}

// Registry is a thread-safe in-memory metrics collector and Prometheus text exporter.
type Registry struct {
	mu sync.RWMutex

	// agentd_runs_total{check,state}
	runsTotal map[string]map[string]uint64

	// agentd_run_duration_seconds{check,stage}
	runDurations map[string]map[string]*histogram

	// agentd_check_staleness_seconds{check}
	checkStaleness map[string]float64

	// agentd_model_tokens_total{model,purpose}
	modelTokens map[string]map[string]uint64

	// agentd_model_cost_micros_total{model,purpose}
	modelCostMicros map[string]map[string]int64

	// agentd_incidents_total{check,outcome}
	incidentsTotal map[string]map[string]uint64

	// agentd_incident_resolution_seconds
	incidentResolution *histogram

	// agentd_queue_depth
	queueDepth int

	// agentd_plugin_calls_total{plugin,result}
	pluginCallsTotal map[string]map[string]uint64
}

var _ ports.Metrics = (*Registry)(nil)

// NewRegistry constructs a new metrics registry.
func NewRegistry() *Registry {
	return &Registry{
		runsTotal:          make(map[string]map[string]uint64),
		runDurations:       make(map[string]map[string]*histogram),
		checkStaleness:     make(map[string]float64),
		modelTokens:        make(map[string]map[string]uint64),
		modelCostMicros:    make(map[string]map[string]int64),
		incidentsTotal:     make(map[string]map[string]uint64),
		incidentResolution: newHistogram(DefaultIncidentResolutionBuckets),
		pluginCallsTotal:   make(map[string]map[string]uint64),
	}
}

func clamp(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

func sanitize(s string) string {
	// Clamp label length to 128 characters to prevent metric explosion / line buffer inflation
	s = clamp(s, 128)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// RecordRun records a completed run state for a check.
func (r *Registry) RecordRun(check string, state string) {
	if check == "" {
		check = "unknown"
	}
	if state == "" {
		state = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	states, ok := r.runsTotal[check]
	if !ok {
		states = make(map[string]uint64)
		r.runsTotal[check] = states
	}
	states[state]++
}

// RecordRunDuration records the duration of a run stage.
func (r *Registry) RecordRunDuration(check string, stage string, durationSeconds float64) {
	if check == "" {
		check = "unknown"
	}
	if stage == "" {
		stage = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	stages, ok := r.runDurations[check]
	if !ok {
		stages = make(map[string]*histogram)
		r.runDurations[check] = stages
	}
	h, ok := stages[stage]
	if !ok {
		h = newHistogram(DefaultRunDurationBuckets)
		stages[stage] = h
	}
	h.observe(durationSeconds)
}

// SetCheckStaleness updates the staleness gauge for a check.
func (r *Registry) SetCheckStaleness(check string, stalenessSeconds float64) {
	if check == "" {
		check = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkStaleness[check] = stalenessSeconds
}

// RecordModelUsage records token counts and cost in micros for a model completion.
func (r *Registry) RecordModelUsage(model string, purpose string, tokens int, costMicros int64) {
	if model == "" {
		model = "unknown"
	}
	if purpose == "" {
		purpose = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Tokens
	purposes, ok := r.modelTokens[model]
	if !ok {
		purposes = make(map[string]uint64)
		r.modelTokens[model] = purposes
	}
	if tokens > 0 {
		purposes[purpose] += uint64(tokens)
	}

	// Cost
	costs, ok := r.modelCostMicros[model]
	if !ok {
		costs = make(map[string]int64)
		r.modelCostMicros[model] = costs
	}
	if costMicros > 0 {
		costs[purpose] += costMicros
	}
}

// RecordIncident records an incident lifecycle event.
func (r *Registry) RecordIncident(check string, outcome string) {
	if check == "" {
		check = "unknown"
	}
	if outcome == "" {
		outcome = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	outcomes, ok := r.incidentsTotal[check]
	if !ok {
		outcomes = make(map[string]uint64)
		r.incidentsTotal[check] = outcomes
	}
	outcomes[outcome]++
}

// RecordIncidentResolution records the elapsed time in seconds to resolve an incident.
func (r *Registry) RecordIncidentResolution(durationSeconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.incidentResolution.observe(durationSeconds)
}

// SetQueueDepth updates the worker pool queue depth gauge.
func (r *Registry) SetQueueDepth(depth int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queueDepth = depth
}

// RecordPluginCall records an MCP plugin invocation result.
func (r *Registry) RecordPluginCall(plugin string, result string) {
	if plugin == "" {
		plugin = "unknown"
	}
	if result == "" {
		result = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	results, ok := r.pluginCallsTotal[plugin]
	if !ok {
		results = make(map[string]uint64)
		r.pluginCallsTotal[plugin] = results
	}
	results[result]++
}

// Export returns the current metrics formatted in Prometheus text format (version 0.0.4).
func (r *Registry) Export() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var buf bytes.Buffer

	// 1. agentd_runs_total{check,state}
	buf.WriteString("# HELP agentd_runs_total Total number of runs completed by check and state.\n")
	buf.WriteString("# TYPE agentd_runs_total counter\n")
	var checks []string
	for c := range r.runsTotal {
		checks = append(checks, c)
	}
	sort.Strings(checks)
	for _, c := range checks {
		states := r.runsTotal[c]
		var stateNames []string
		for s := range states {
			stateNames = append(stateNames, s)
		}
		sort.Strings(stateNames)
		for _, s := range stateNames {
			fmt.Fprintf(&buf, "agentd_runs_total{check=\"%s\",state=\"%s\"} %d\n", sanitize(c), sanitize(s), states[s])
		}
	}

	// 2. agentd_run_duration_seconds{check,stage}
	buf.WriteString("# HELP agentd_run_duration_seconds Execution duration of run stages in seconds.\n")
	buf.WriteString("# TYPE agentd_run_duration_seconds histogram\n")
	var durationChecks []string
	for c := range r.runDurations {
		durationChecks = append(durationChecks, c)
	}
	sort.Strings(durationChecks)
	for _, c := range durationChecks {
		stages := r.runDurations[c]
		var stageNames []string
		for s := range stages {
			stageNames = append(stageNames, s)
		}
		sort.Strings(stageNames)
		for _, s := range stageNames {
			h := stages[s]
			for i, b := range h.buckets {
				fmt.Fprintf(&buf, "agentd_run_duration_seconds_bucket{check=\"%s\",stage=\"%s\",le=\"%g\"} %d\n",
					sanitize(c), sanitize(s), b, h.counts[i])
			}
			fmt.Fprintf(&buf, "agentd_run_duration_seconds_bucket{check=\"%s\",stage=\"%s\",le=\"+Inf\"} %d\n",
				sanitize(c), sanitize(s), h.count)
			fmt.Fprintf(&buf, "agentd_run_duration_seconds_sum{check=\"%s\",stage=\"%s\"} %g\n",
				sanitize(c), sanitize(s), h.sum)
			fmt.Fprintf(&buf, "agentd_run_duration_seconds_count{check=\"%s\",stage=\"%s\"} %d\n",
				sanitize(c), sanitize(s), h.count)
		}
	}

	// 3. agentd_check_staleness_seconds{check}
	buf.WriteString("# HELP agentd_check_staleness_seconds Observation staleness in seconds beyond expected cadence.\n")
	buf.WriteString("# TYPE agentd_check_staleness_seconds gauge\n")
	var stalenessChecks []string
	for c := range r.checkStaleness {
		stalenessChecks = append(stalenessChecks, c)
	}
	sort.Strings(stalenessChecks)
	for _, c := range stalenessChecks {
		fmt.Fprintf(&buf, "agentd_check_staleness_seconds{check=\"%s\"} %g\n", sanitize(c), r.checkStaleness[c])
	}

	// 4. agentd_model_tokens_total{model,purpose}
	buf.WriteString("# HELP agentd_model_tokens_total Total tokens used by model and purpose.\n")
	buf.WriteString("# TYPE agentd_model_tokens_total counter\n")
	var models []string
	for m := range r.modelTokens {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		purposes := r.modelTokens[m]
		var pNames []string
		for p := range purposes {
			pNames = append(pNames, p)
		}
		sort.Strings(pNames)
		for _, p := range pNames {
			fmt.Fprintf(&buf, "agentd_model_tokens_total{model=\"%s\",purpose=\"%s\"} %d\n", sanitize(m), sanitize(p), purposes[p])
		}
	}

	// 5. agentd_model_cost_micros_total{model,purpose}
	buf.WriteString("# HELP agentd_model_cost_micros_total Estimated total model cost in micros.\n")
	buf.WriteString("# TYPE agentd_model_cost_micros_total counter\n")
	var costModels []string
	for m := range r.modelCostMicros {
		costModels = append(costModels, m)
	}
	sort.Strings(costModels)
	for _, m := range costModels {
		purposes := r.modelCostMicros[m]
		var pNames []string
		for p := range purposes {
			pNames = append(pNames, p)
		}
		sort.Strings(pNames)
		for _, p := range pNames {
			fmt.Fprintf(&buf, "agentd_model_cost_micros_total{model=\"%s\",purpose=\"%s\"} %d\n", sanitize(m), sanitize(p), purposes[p])
		}
	}

	// 6. agentd_incidents_total{check,outcome}
	buf.WriteString("# HELP agentd_incidents_total Total number of incidents by check and outcome.\n")
	buf.WriteString("# TYPE agentd_incidents_total counter\n")
	var incidentChecks []string
	for c := range r.incidentsTotal {
		incidentChecks = append(incidentChecks, c)
	}
	sort.Strings(incidentChecks)
	for _, c := range incidentChecks {
		outcomes := r.incidentsTotal[c]
		var oNames []string
		for o := range outcomes {
			oNames = append(oNames, o)
		}
		sort.Strings(oNames)
		for _, o := range oNames {
			fmt.Fprintf(&buf, "agentd_incidents_total{check=\"%s\",outcome=\"%s\"} %d\n", sanitize(c), sanitize(o), outcomes[o])
		}
	}

	// 7. agentd_incident_resolution_seconds
	buf.WriteString("# HELP agentd_incident_resolution_seconds Time taken to resolve an incident in seconds.\n")
	buf.WriteString("# TYPE agentd_incident_resolution_seconds histogram\n")
	for i, b := range r.incidentResolution.buckets {
		fmt.Fprintf(&buf, "agentd_incident_resolution_seconds_bucket{le=\"%g\"} %d\n", b, r.incidentResolution.counts[i])
	}
	fmt.Fprintf(&buf, "agentd_incident_resolution_seconds_bucket{le=\"+Inf\"} %d\n", r.incidentResolution.count)
	fmt.Fprintf(&buf, "agentd_incident_resolution_seconds_sum %g\n", r.incidentResolution.sum)
	fmt.Fprintf(&buf, "agentd_incident_resolution_seconds_count %d\n", r.incidentResolution.count)

	// 8. agentd_queue_depth
	buf.WriteString("# HELP agentd_queue_depth Current number of runs pending or queued in the worker pool.\n")
	buf.WriteString("# TYPE agentd_queue_depth gauge\n")
	fmt.Fprintf(&buf, "agentd_queue_depth %d\n", r.queueDepth)

	// 9. agentd_plugin_calls_total{plugin,result}
	buf.WriteString("# HELP agentd_plugin_calls_total Total plugin execution calls by plugin and result.\n")
	buf.WriteString("# TYPE agentd_plugin_calls_total counter\n")
	var plugins []string
	for p := range r.pluginCallsTotal {
		plugins = append(plugins, p)
	}
	sort.Strings(plugins)
	for _, p := range plugins {
		results := r.pluginCallsTotal[p]
		var rNames []string
		for res := range results {
			rNames = append(rNames, res)
		}
		sort.Strings(rNames)
		for _, res := range rNames {
			fmt.Fprintf(&buf, "agentd_plugin_calls_total{plugin=\"%s\",result=\"%s\"} %d\n", sanitize(p), sanitize(res), results[res])
		}
	}

	return buf.String()
}

// HTTPHandler returns an http.Handler that serves metrics at /metrics.
func (r *Registry) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed: /metrics requires GET", http.StatusMethodNotAllowed)
			return
		}
		text := r.Export()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(text))
	})
}
