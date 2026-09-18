package domain

import "time"

// Retention is domain policy, not a database chore.
//
// What Agentd is allowed to forget is a product decision with consequences:
// throw away the wrong capture and a future repair cannot be verified; throw
// away the wrong run and an operator asking "was this checked last Tuesday"
// gets no answer. So the rules live here, in terms the product understands,
// and a store is handed the decision rather than making one.
//
// Nothing here runs on a timer. Retention happens when maintenance asks for
// it, which means it is visible, interruptible, and cannot quietly delete
// things at three in the morning while nobody is looking.

const (
	// DefaultRunTTL is how long finished runs are kept. Ninety days covers a
	// quarter, which is the window in which someone asks what a source was
	// doing at the time of some other event.
	DefaultRunTTL = 90 * 24 * time.Hour

	// DefaultIncidentTTL is how long a closed incident is kept after it
	// closed. Twice the run window, because an incident is the record of
	// Agentd asking a human for something, and those are worth keeping
	// longer than routine observations.
	DefaultIncidentTTL = 180 * 24 * time.Hour

	// DefaultSnapshotsPerCheck is how many captures are kept per check on top
	// of the known-good ones.
	DefaultSnapshotsPerCheck = 10
)

// Retention says what may be forgotten.
type Retention struct {
	// RunTTL is the age past which a finished run may be deleted. Runs that
	// have not finished are never deleted by age: an unfinished run that is
	// ninety days old is a bug worth seeing, not litter.
	RunTTL time.Duration

	// IncidentTTL is the age past which a closed incident may be deleted,
	// measured from when it closed. An open incident is never deleted, however
	// old, because deleting it would silently withdraw a question Agentd has
	// asked a human.
	IncidentTTL time.Duration

	// SnapshotsPerCheck is how many captures to keep per check. Known-good
	// captures are retained on top of this count, never counted against it.
	SnapshotsPerCheck int
}

// DefaultRetention returns the standard policy.
func DefaultRetention() Retention {
	return Retention{
		RunTTL:            DefaultRunTTL,
		IncidentTTL:       DefaultIncidentTTL,
		SnapshotsPerCheck: DefaultSnapshotsPerCheck,
	}
}

// Validate reports whether r is usable. A zero or negative window would mean
// "delete everything immediately", which is never what someone meant to
// configure, so it is refused rather than obeyed.
func (r Retention) Validate() error {
	if r.RunTTL <= 0 {
		return invalidf("retention needs a positive run window")
	}
	if r.IncidentTTL <= 0 {
		return invalidf("retention needs a positive incident window")
	}
	if r.SnapshotsPerCheck < 1 {
		return invalidf("retention must keep at least one capture per check")
	}
	return nil
}

// RunCutoff returns the instant before which finished runs may be deleted.
func (r Retention) RunCutoff(now time.Time) time.Time {
	return now.UTC().Add(-r.RunTTL)
}

// IncidentCutoff returns the instant before which closed incidents may be
// deleted, compared against when they closed.
func (r Retention) IncidentCutoff(now time.Time) time.Time {
	return now.UTC().Add(-r.IncidentTTL)
}

// RunIsExpired reports whether a run may be forgotten.
func (r Retention) RunIsExpired(run *Run, now time.Time) bool {
	if run == nil || !run.Terminal() {
		return false
	}
	return run.EndedAt().Before(r.RunCutoff(now))
}

// IncidentIsExpired reports whether a closed incident may be forgotten.
func (r Retention) IncidentIsExpired(i *Incident, now time.Time) bool {
	if i == nil || i.Open() || i.ClosedAt().IsZero() {
		return false
	}
	return i.ClosedAt().Before(r.IncidentCutoff(now))
}

// ExpiredSnapshots returns the captures in an index that may be forgotten,
// and applies the decision to the index.
//
// It delegates to SnapshotIndex.Prune, which will exceed the retention count
// rather than drop the last known-good capture. That asymmetry is the whole
// reason retention is domain policy: a store implementing "keep the newest
// ten" from first principles would get it wrong in exactly the way that
// disables repair forever.
func (r Retention) ExpiredSnapshots(index *SnapshotIndex) []SnapshotID {
	if index == nil {
		return nil
	}
	return index.Prune(r.SnapshotsPerCheck)
}

// Sweep is what one retention pass removed, reported so that maintenance can
// tell an operator what it did rather than deleting in silence.
type Sweep struct {
	Runs      int
	Snapshots int
	Incidents int
}

// Empty reports whether the sweep removed nothing.
func (s Sweep) Empty() bool {
	return s.Runs == 0 && s.Snapshots == 0 && s.Incidents == 0
}
