package domain

import (
	"errors"
	"time"
)

// ErrNoSuchVersion reports a reference to a definition version a Check does
// not have.
var ErrNoSuchVersion = errors.New("no such definition version")

// SourceKind names how a source is reached. The core does not implement any
// of them; it only records which adapter a Check needs.
type SourceKind string

const (
	// SourceHTTP is fetched over HTTP by the built-in adapter.
	SourceHTTP SourceKind = "http"
	// SourcePlugin is fetched by an MCP plugin over stdio.
	SourcePlugin SourceKind = "plugin"
)

// SecretRef names a secret without containing it. The core passes references
// around; only a SecretResolver ever holds a value.
type SecretRef string

// SourceSpec says where to look. It says nothing about what to look for --
// that is the Intent -- and nothing about where in the response to look --
// that is the Binding.
type SourceSpec struct {
	Kind SourceKind

	// URL is the address to fetch for SourceHTTP.
	URL string

	// Method defaults to GET when empty.
	Method string

	// Headers are sent verbatim. Values that must stay secret are named in
	// SecretHeaders instead.
	Headers map[string]string

	// SecretHeaders maps a header name to the secret supplying its value.
	SecretHeaders map[string]SecretRef

	// Plugin names the MCP plugin for SourcePlugin.
	Plugin string

	// PluginArgs are passed to the plugin verbatim.
	PluginArgs map[string]string
}

// Validate reports whether s is well formed.
func (s SourceSpec) Validate() error {
	switch s.Kind {
	case SourceHTTP:
		if !nonEmpty(s.URL) {
			return invalidf("http source needs a URL")
		}
	case SourcePlugin:
		if !nonEmpty(s.Plugin) {
			return invalidf("plugin source needs a plugin name")
		}
	default:
		return invalidf("unknown source kind %q", s.Kind)
	}
	return nil
}

// SecretRefs returns every secret this spec needs resolved before a fetch.
func (s SourceSpec) SecretRefs() []SecretRef {
	out := make([]SecretRef, 0, len(s.SecretHeaders))
	for _, ref := range s.SecretHeaders {
		out = append(out, ref)
	}
	return out
}

// Slot is a scheduling slot: the index of one interval-sized window since the
// Unix epoch. Slots exist so that "one run per (check, slot)" is a statement
// about a value rather than about wall-clock proximity, which makes duplicate
// suppression exact and testable.
type Slot int64

// CatchUpPolicy controls how missed slots are handled when the scheduler runs.
type CatchUpPolicy string

const (
	// CatchUpSkip ignores all missed slots and only schedules the current slot.
	CatchUpSkip CatchUpPolicy = "skip"

	// CatchUpOnce schedules exactly one run covering the missed period (the default).
	CatchUpOnce CatchUpPolicy = "once"

	// CatchUpBackfill schedules runs for all missed slots in chronological order.
	CatchUpBackfill CatchUpPolicy = "backfill"
)

// Schedule says how often a Check runs.
type Schedule struct {
	// Interval is the width of one slot.
	Interval time.Duration

	// Jitter is the fraction of the interval, in [0, 1], by which a run may
	// be delayed within its slot so that many checks do not stampede.
	Jitter float64

	// CatchUp determines how missed slots are handled after daemon downtime.
	// Empty means CatchUpOnce.
	CatchUp CatchUpPolicy
}

// CatchUpPolicy returns the configured policy, defaulting to CatchUpOnce.
func (s Schedule) CatchUpPolicy() CatchUpPolicy {
	if s.CatchUp == "" {
		return CatchUpOnce
	}
	return s.CatchUp
}

// Validate reports whether s is well formed.
func (s Schedule) Validate() error {
	if s.Interval <= 0 {
		return invalidf("schedule needs a positive interval")
	}
	if s.Jitter < 0 || s.Jitter > 1 {
		return invalidf("schedule jitter must be between 0 and 1, got %v", s.Jitter)
	}
	if s.CatchUp != "" && s.CatchUp != CatchUpSkip && s.CatchUp != CatchUpOnce && s.CatchUp != CatchUpBackfill {
		return invalidf("unknown catch-up policy %q", s.CatchUp)
	}
	return nil
}

// SlotAt returns the slot containing t. It is pure arithmetic on a supplied
// instant; nothing here reads a clock.
func (s Schedule) SlotAt(t time.Time) Slot {
	if s.Interval <= 0 {
		return 0
	}
	return Slot(t.UnixNano() / int64(s.Interval))
}

// SlotStart returns the instant at which slot begins, in UTC.
func (s Schedule) SlotStart(slot Slot) time.Time {
	return time.Unix(0, int64(slot)*int64(s.Interval)).UTC()
}

// DestinationKind names where a result is delivered.
type DestinationKind string

const (
	// DestinationNone records the result and tells nobody. Silence is a
	// feature, so this is a legitimate choice.
	DestinationNone DestinationKind = "none"
	// DestinationNotify delivers through a Notifier.
	DestinationNotify DestinationKind = "notify"
)

// Destination says who hears about a result, and when.
type Destination struct {
	Kind DestinationKind

	// Target is the address the Notifier understands.
	Target string

	// Secret names the credential the Notifier needs, if any.
	Secret SecretRef

	// OnQuiet, when true, delivers even when nothing changed. Off by
	// default: a trustworthy silence is the product.
	OnQuiet bool
}

// Validate reports whether d is well formed.
func (d Destination) Validate() error {
	switch d.Kind {
	case DestinationNone, "":
		// The zero value means nobody is told, which is a legitimate and
		// common choice: a recorded result an operator can go and look at.
		return nil
	case DestinationNotify:
		if !nonEmpty(d.Target) {
			return invalidf("notify destination needs a target")
		}
		return nil
	default:
		return invalidf("unknown destination kind %q", d.Kind)
	}
}

// DefaultMaxRepairAttempts bounds repair work when a Policy does not.
const DefaultMaxRepairAttempts = 3

// DefaultMaxRetries bounds in-slot retries when a Policy does not. It is small
// on purpose: a check that fails three times in a row is telling you
// something, and hammering the source past that point is rude to it and
// useless to the operator.
const DefaultMaxRetries = 3

// Policy is the per-check tuning the core consults when deciding what to do
// about a failure.
type Policy struct {
	// Priority ranks checks for execution under capacity constraints.
	// Higher numbers have higher priority; lower numbers are shed first.
	Priority int

	// MaxRepairAttempts bounds how many times Agentd will try to propose a
	// repair for one incident before giving up and saying so. Zero means
	// DefaultMaxRepairAttempts.
	MaxRepairAttempts int

	// RetainSnapshots is how many snapshots to keep per check. The most
	// recent known-good snapshot is retained regardless of this number.
	RetainSnapshots int

	// MaxRetries bounds how many times a retryable failure is retried within
	// one slot. Zero means DefaultMaxRetries.
	MaxRetries int

	// RetryBackoff is the first retry delay; each subsequent retry doubles
	// it. Zero means DefaultRetryBackoff.
	RetryBackoff time.Duration
}

// DefaultRetryBackoff is the first retry delay when a Policy does not set one.
const DefaultRetryBackoff = 30 * time.Second

// Attempts returns the effective repair budget.
func (p Policy) Attempts() int {
	if p.MaxRepairAttempts <= 0 {
		return DefaultMaxRepairAttempts
	}
	return p.MaxRepairAttempts
}

// Retries returns the effective in-slot retry budget.
func (p Policy) Retries() int {
	if p.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return p.MaxRetries
}

// Backoff returns the delay before the given retry attempt, counting from 1.
// It doubles each time and is capped so that a long-running interval cannot
// produce a retry scheduled past the end of the universe.
func (p Policy) Backoff(attempt int) time.Duration {
	base := p.RetryBackoff
	if base <= 0 {
		base = DefaultRetryBackoff
	}
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		return time.Hour
	}
	return d
}

// Definition is one version of a Check's configuration. Definitions are
// immutable; changing a Check appends a new one.
//
// Note what is absent: there is no selector, path, query or dialect anywhere
// in this struct or in anything it contains. Locating the intent inside a
// particular source version is a Binding's job, and a Binding is derived, not
// configured. TestCheckCannotCarryABinding enforces that by inspection.
type Definition struct {
	// Version is the definition's number within its Check, starting at 1.
	Version int

	// Intent is what the operator wants to know.
	Intent Intent

	// Source is where to look for it.
	Source SourceSpec

	// Schedule is how often to look.
	Schedule Schedule

	// Destination is who hears about it.
	Destination Destination

	// Policy tunes failure handling.
	Policy Policy

	// CreatedAt is when this version was authored, supplied by the caller.
	CreatedAt time.Time
}

// Validate reports whether d is well formed.
func (d Definition) Validate() error {
	if d.Version < 1 {
		return invalidf("definition version must be 1 or greater, got %d", d.Version)
	}
	if d.Intent == nil {
		return invalidf("definition %d has no intent", d.Version)
	}
	if err := d.Intent.Validate(); err != nil {
		return err
	}
	if err := d.Source.Validate(); err != nil {
		return err
	}
	if err := d.Schedule.Validate(); err != nil {
		return err
	}
	if err := d.Destination.Validate(); err != nil {
		return err
	}
	if d.Policy.RetainSnapshots < 0 {
		return invalidf("definition %d cannot retain a negative number of snapshots", d.Version)
	}
	return nil
}

// Check is the aggregate holding a user's durable intent to watch something.
//
// Invariant: a Check has exactly one active definition version at any moment.
// Fields are unexported so that the only way to change which version is active
// is through a method that keeps that true.
type Check struct {
	id          CheckID
	definitions []Definition
	active      int
	enabled     bool
}

// NewCheck creates a Check whose first definition is version 1 and active.
// The version number on def is ignored and set to 1.
func NewCheck(id CheckID, def Definition) (*Check, error) {
	if !nonEmpty(string(id)) {
		return nil, invalidf("check needs an id")
	}
	def.Version = 1
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return &Check{
		id:          id,
		definitions: []Definition{def},
		active:      1,
		enabled:     true,
	}, nil
}

// ID returns the check's identifier.
func (c *Check) ID() CheckID { return c.id }

// Enabled reports whether the check is scheduled to run.
func (c *Check) Enabled() bool { return c.enabled }

// Enable schedules the check.
func (c *Check) Enable() { c.enabled = true }

// Disable stops the check from being scheduled. Its history is untouched.
func (c *Check) Disable() { c.enabled = false }

// ActiveVersion returns the version number currently in force.
func (c *Check) ActiveVersion() int { return c.active }

// Definition returns the definition with the given version.
func (c *Check) Definition(version int) (Definition, error) {
	for _, d := range c.definitions {
		if d.Version == version {
			return d, nil
		}
	}
	return Definition{}, ErrNoSuchVersion
}

// ActiveDefinition returns the definition currently in force. A Check built
// through NewCheck always has exactly one.
func (c *Check) ActiveDefinition() Definition {
	d, err := c.Definition(c.active)
	if err != nil {
		// Unreachable while the invariant holds. NewCheck, Revise and
		// Activate are the only ways to construct or change a Check, and all
		// three maintain it.
		panic("agentd: check has no active definition: " + err.Error())
	}
	return d
}

// Versions returns every definition, oldest first. The slice is a copy.
func (c *Check) Versions() []Definition {
	out := make([]Definition, len(c.definitions))
	copy(out, c.definitions)
	return out
}

// Revise appends a new definition version and makes it the active one. The
// version number on def is ignored and assigned by the Check, so a caller
// cannot leave a gap or create a duplicate.
//
// Older versions are kept because every Run records the definition version it
// ran under, and an explanation of a past run must still make sense after the
// check has been edited.
func (c *Check) Revise(def Definition) (int, error) {
	next := c.definitions[len(c.definitions)-1].Version + 1
	def.Version = next
	if err := def.Validate(); err != nil {
		return 0, err
	}
	c.definitions = append(c.definitions, def)
	c.active = next
	return next, nil
}

// Activate makes an existing version the active one, which is how an operator
// rolls a change back.
func (c *Check) Activate(version int) error {
	if _, err := c.Definition(version); err != nil {
		return err
	}
	c.active = version
	return nil
}

// Schedule returns the active schedule, a convenience for the scheduler.
func (c *Check) Schedule() Schedule { return c.ActiveDefinition().Schedule }

// Intent returns the active intent.
func (c *Check) Intent() Intent { return c.ActiveDefinition().Intent }

// CheckInvariants reports whether the aggregate's invariants hold. It exists
// so that tests can assert them directly after any sequence of operations.
func (c *Check) CheckInvariants() error {
	if len(c.definitions) == 0 {
		return invalidf("check %q has no definitions", c.id)
	}
	active := 0
	seen := make(map[int]bool, len(c.definitions))
	for i, d := range c.definitions {
		if seen[d.Version] {
			return invalidf("check %q has duplicate definition version %d", c.id, d.Version)
		}
		seen[d.Version] = true
		if i > 0 && d.Version <= c.definitions[i-1].Version {
			return invalidf("check %q has out-of-order definition versions", c.id)
		}
		if d.Version == c.active {
			active++
		}
	}
	if active != 1 {
		return invalidf("check %q has %d active definitions, want exactly 1", c.id, active)
	}
	return nil
}

// Staleness reports whether a check is overdue for a terminal run.
type Staleness struct {
	CheckID          CheckID
	ExpectedInterval time.Duration
	Elapsed          time.Duration
	Threshold        time.Duration
	IsStale          bool
}

// CheckStaleness evaluates whether a check has produced no terminal run
// materially beyond its expected interval.
// If lastRun is nil, elapsed time is measured from the check's creation time.
// graceFactor defaults to 1.5 if <= 0 (e.g. 50% beyond expected interval).
func CheckStaleness(check *Check, lastRun *Run, now time.Time, graceFactor float64) Staleness {
	if graceFactor <= 0 {
		graceFactor = 1.5
	}
	interval := check.Schedule().Interval
	threshold := time.Duration(float64(interval) * graceFactor)

	var lastTime time.Time
	if lastRun != nil && lastRun.Terminal() && !lastRun.EndedAt().IsZero() {
		lastTime = lastRun.EndedAt()
	} else if lastRun != nil && !lastRun.CreatedAt().IsZero() {
		lastTime = lastRun.CreatedAt()
	} else {
		lastTime = check.ActiveDefinition().CreatedAt
	}

	elapsed := now.Sub(lastTime)
	if elapsed < 0 {
		elapsed = 0
	}

	return Staleness{
		CheckID:          check.ID(),
		ExpectedInterval: interval,
		Elapsed:          elapsed,
		Threshold:        threshold,
		IsStale:          elapsed > threshold,
	}
}
