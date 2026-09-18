package domain

import "time"

// Rehydration.
//
// Aggregates keep their fields unexported so that the only way to reach an
// invalid state is through a method that refuses. That protection has to be
// reconciled with the fact that a store must be able to put an aggregate back
// together from rows.
//
// The functions here are that reconciliation, and they are written so that
// loading is not a way around the rules:
//
//   - Each one validates before returning, so a row that has rotted -- by a
//     hand-edited database, a partial restore, a bug in an older version --
//     is refused at the boundary rather than becoming a Run that is somehow
//     both terminal and unfinished.
//   - None of them accepts a state the aggregate could not have reached
//     through its own API.
//
// They are exported because the store is a different package. That is the
// price of keeping adapters out of the core, and it is worth paying: the
// alternative is either a reflection-based mapper or moving persistence into
// the domain, and both are worse.

// RestoredRun is the stored form of a Run.
type RestoredRun struct {
	ID                RunID
	CheckID           CheckID
	Slot              Slot
	DefinitionVersion int
	BindingVersion    int
	State             RunState
	CreatedAt         time.Time
	StartedAt         time.Time
	EndedAt           time.Time
	SnapshotID        SnapshotID
	Result            Extraction
	Failure           *Failure
	Explanation       string
}

// RestoreRun rebuilds a Run from storage.
func RestoreRun(r RestoredRun) (*Run, error) {
	if !nonEmpty(string(r.ID)) {
		return nil, invalidf("stored run has no id")
	}
	if !r.State.Valid() {
		return nil, invalidf("stored run %q has unknown state %q", r.ID, r.State)
	}

	run := &Run{
		id:                r.ID,
		checkID:           r.CheckID,
		slot:              r.Slot,
		definitionVersion: r.DefinitionVersion,
		bindingVersion:    r.BindingVersion,
		state:             r.State,
		createdAt:         r.CreatedAt.UTC(),
		startedAt:         utcOrZero(r.StartedAt),
		endedAt:           utcOrZero(r.EndedAt),
		snapshotID:        r.SnapshotID,
		result:            r.Result,
		explanation:       r.Explanation,
	}
	if r.Failure != nil {
		f := *r.Failure
		run.failure = &f
	}

	if err := run.CheckInvariants(); err != nil {
		return nil, err
	}
	return run, nil
}

// RestoredCheck is the stored form of a Check.
type RestoredCheck struct {
	ID          CheckID
	Definitions []Definition
	Active      int
	Enabled     bool
}

// RestoreCheck rebuilds a Check from storage. Definitions must be ordered
// oldest first, which is how they are read back.
func RestoreCheck(c RestoredCheck) (*Check, error) {
	if !nonEmpty(string(c.ID)) {
		return nil, invalidf("stored check has no id")
	}
	if len(c.Definitions) == 0 {
		return nil, invalidf("stored check %q has no definitions", c.ID)
	}
	for _, d := range c.Definitions {
		if err := d.Validate(); err != nil {
			return nil, err
		}
	}

	check := &Check{
		id:          c.ID,
		definitions: append([]Definition(nil), c.Definitions...),
		active:      c.Active,
		enabled:     c.Enabled,
	}
	if err := check.CheckInvariants(); err != nil {
		return nil, err
	}
	return check, nil
}

// RestoredSnapshot is the stored form of a Snapshot. Body is the original
// uncompressed bytes; the store is responsible for having decompressed it.
type RestoredSnapshot struct {
	CheckID     CheckID
	ContentType string
	Body        []byte
	Fingerprint SourceFingerprint
	CapturedAt  time.Time
	KnownGood   bool

	// ID is the content address as stored. It is checked against a fresh
	// digest of Body, so a capture that was corrupted on disk or mangled in
	// transit is refused here rather than being used to verify a repair.
	ID SnapshotID
}

// RestoreSnapshot rebuilds a Snapshot from storage and verifies its content
// address.
func RestoreSnapshot(s RestoredSnapshot) (Snapshot, error) {
	snap, err := NewSnapshot(s.CheckID, s.ContentType, s.Body, s.Fingerprint, s.CapturedAt)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.id != s.ID {
		return Snapshot{}, invalidf("stored snapshot %q hashes to %q; the capture on disk is not the one that was recorded", s.ID, snap.id)
	}
	snap.knownGood = s.KnownGood
	return snap, nil
}

// RestoredIncident is the stored form of an Incident.
type RestoredIncident struct {
	ID          IncidentID
	CheckID     CheckID
	State       IncidentState
	Cause       Failure
	OpenedAt    time.Time
	ClosedAt    time.Time
	MaxAttempts int
	Attempts    []RepairAttempt
	Proposal    *RepairProposal
	Resolution  string
}

// RestoreIncident rebuilds an Incident from storage.
func RestoreIncident(i RestoredIncident) (*Incident, error) {
	if !nonEmpty(string(i.ID)) {
		return nil, invalidf("stored incident has no id")
	}
	if !i.State.Valid() {
		return nil, invalidf("stored incident %q has unknown state %q", i.ID, i.State)
	}

	inc := &Incident{
		id:          i.ID,
		checkID:     i.CheckID,
		state:       i.State,
		cause:       i.Cause,
		openedAt:    i.OpenedAt.UTC(),
		closedAt:    utcOrZero(i.ClosedAt),
		maxAttempts: i.MaxAttempts,
		attempts:    append([]RepairAttempt(nil), i.Attempts...),
		resolution:  i.Resolution,
	}
	if i.Proposal != nil {
		p := *i.Proposal
		inc.proposal = &p
	}

	if err := inc.CheckInvariants(); err != nil {
		return nil, err
	}
	return inc, nil
}

// RestoreIncidentLog rebuilds a check's incident log. Incidents must be
// ordered oldest first.
func RestoreIncidentLog(checkID CheckID, incidents []*Incident) (*IncidentLog, error) {
	log := &IncidentLog{checkID: checkID, incidents: append([]*Incident(nil), incidents...)}
	if err := log.CheckInvariants(); err != nil {
		return nil, err
	}
	return log, nil
}

// RestoreSnapshotIndex rebuilds a check's snapshot index.
func RestoreSnapshotIndex(checkID CheckID, snaps []Snapshot) (*SnapshotIndex, error) {
	index := NewSnapshotIndex(checkID)
	for _, s := range snaps {
		if err := index.Add(s); err != nil {
			return nil, err
		}
		if s.knownGood {
			if err := index.MarkKnownGood(s.id); err != nil {
				return nil, err
			}
		}
	}
	return index, nil
}

// utcOrZero normalises an instant to UTC, leaving the zero time alone so that
// "this never happened" survives a round trip.
func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
