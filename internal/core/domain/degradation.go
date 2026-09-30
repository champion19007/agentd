package domain

// DegradationLevel represents the system's operational shedding tier under resource pressure.
//
// The architectural order prioritizes core observation above all secondary capabilities:
//  1. Healing generation: paused first to protect model budget and CPU.
//  2. Model evaluation: paused second; changes classified directly from diffs.
//  3. Snapshot retention: reduced third to conserve disk and memory.
//  4. Scheduled runs: sacrificed last, only under physical dispatch queue overflow.
//
// Observation is the last capability to be sacrificed.
type DegradationLevel int

const (
	// DegradationNormal operates with all capabilities fully enabled.
	DegradationNormal DegradationLevel = iota

	// DegradationShedHealing pauses automated repair proposal generation.
	DegradationShedHealing

	// DegradationShedModel pauses LLM semantic change evaluation, marking runs Changed directly.
	DegradationShedModel

	// DegradationReducedRetention restricts snapshot retention to baseline and latest only.
	DegradationReducedRetention

	// DegradationShedRuns sheds scheduled runs via skipped_overload when queue saturates.
	DegradationShedRuns
)

// ShedsHealing reports whether automated repair generation should be paused.
func (d DegradationLevel) ShedsHealing() bool { return d >= DegradationShedHealing }

// ShedsModel reports whether LLM semantic change evaluation should be bypassed.
func (d DegradationLevel) ShedsModel() bool { return d >= DegradationShedModel }

// ReducesRetention reports whether snapshot retention should be minimized.
func (d DegradationLevel) ReducesRetention() bool { return d >= DegradationReducedRetention }

// ShedsRuns reports whether scheduled runs are subject to overload shedding.
func (d DegradationLevel) ShedsRuns() bool { return d >= DegradationShedRuns }

func (d DegradationLevel) String() string {
	switch d {
	case DegradationNormal:
		return "normal"
	case DegradationShedHealing:
		return "shed_healing"
	case DegradationShedModel:
		return "shed_model"
	case DegradationReducedRetention:
		return "reduced_retention"
	case DegradationShedRuns:
		return "shed_runs"
	default:
		return "unknown"
	}
}
