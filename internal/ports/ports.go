package ports

import (
	"context"

	"github.com/champion19007/agentd/internal/core/domain"
)

// Source fetches the raw content of an external source.
//
// Implementations must classify what they know. An adapter that sees a 401
// returns an error wrapping domain.Failure{Class: domain.ClassAuth}; one that
// sees a 429 returns ClassRateLimited. Anything it does not recognise it may
// return unwrapped, and domain.Classify will treat it as transient. An
// adapter must never report ClassStructural: whether a source changed shape
// is a judgement about intent, which only the Extractor and the core can make.
type Source interface {
	Fetch(ctx context.Context, spec domain.SourceSpec, secrets domain.SecretBundle) (domain.RawResponse, error)
}

// Extractor applies a Binding to a fetched response.
//
// An extractor owns one or more dialects and is the only component that knows
// what a Locator's Expression means. It reports a missing value by setting
// domain.Value.Missing rather than by returning an error; deciding whether a
// missing value is structural, merely degrading or fine is the core's job,
// because only the core knows which fields the intent marked required.
type Extractor interface {
	// Dialects returns the Locator dialects this extractor understands, so
	// that the composition root can route bindings without trial and error.
	Dialects() []string

	Extract(ctx context.Context, raw domain.RawResponse, binding domain.Binding) (domain.Extraction, error)
}

// Fingerprinter computes the structural fingerprint of a response.
//
// It is separate from Extractor because fingerprinting must work on a source
// whose binding is already broken -- that is precisely when it is needed.
type Fingerprinter interface {
	Fingerprint(ctx context.Context, raw domain.RawResponse) (domain.SourceFingerprint, error)
}

// ModelPurpose says what a completion is for. It lets an operator route
// different work to different models, and lets Agentd refuse to spend a large
// model's budget on a small job.
type ModelPurpose string

const (
	// PurposeBind derives a binding for a check being set up.
	PurposeBind ModelPurpose = "bind"
	// PurposeRepair proposes a replacement binding after a structural break.
	PurposeRepair ModelPurpose = "repair"
	// PurposeExplain turns a technical failure into something a human can
	// act on.
	PurposeExplain ModelPurpose = "explain"
)

// ModelRequest is a provider-neutral completion request. It carries no
// provider identifiers, no model names and no API shapes, so that adding a
// provider never changes the core. Keys are the operator's, resolved through
// a SecretResolver and never held here.
type ModelRequest struct {
	// Purpose says what this completion is for.
	Purpose ModelPurpose

	// System is the standing instruction.
	System string

	// Prompt is the request itself.
	Prompt string

	// MaxOutputTokens bounds the response. Zero means the adapter's default.
	MaxOutputTokens int

	// Deterministic asks for the least random sampling the provider offers.
	// Repair proposals set it: the same break should produce the same
	// proposal, so that a human reviewing one twice sees the same thing.
	Deterministic bool
}

// ModelResponse is what came back.
type ModelResponse struct {
	// Text is the completion.
	Text string

	// InputTokens and OutputTokens are for the operator's own accounting.
	InputTokens  int
	OutputTokens int

	// Truncated reports that the response hit the output limit. A truncated
	// repair proposal is discarded rather than parsed optimistically.
	Truncated bool
}

// Model is the BYOK completion port. The core calls it only to propose and
// explain; nothing a Model returns is ever applied without passing through
// verification against stored evidence and then a human approval.
type Model interface {
	Complete(ctx context.Context, req ModelRequest) (ModelResponse, error)
}

// Notifier delivers a notification to a human.
//
// Delivery is at-least-once from the core's point of view: the core records
// its intent to notify inside the same transaction as the run, then delivers.
// A Notifier that cannot deliver returns a classified error and the core
// retries according to class.
type Notifier interface {
	// Kinds returns the destination kinds this notifier serves.
	Kinds() []domain.DestinationKind

	Deliver(ctx context.Context, n domain.Notification) error
}

// SecretResolver turns secret references into values.
//
// Implementations read from the operator's chosen place -- environment,
// keyring, file -- and the core never learns which. A reference that cannot
// be resolved is an auth-class failure, not a transient one: no amount of
// retrying will conjure a credential.
type SecretResolver interface {
	Resolve(ctx context.Context, refs []domain.SecretRef) (domain.SecretBundle, error)
}

// Reader is the read-only view of stored state. Every method is safe to call
// concurrently and none of them changes anything.
type Reader interface {
	// Check returns one check by id.
	Check(ctx context.Context, id domain.CheckID) (*domain.Check, error)

	// EnabledChecks returns every check eligible to be scheduled.
	EnabledChecks(ctx context.Context) ([]*domain.Check, error)

	// Run returns one run by id.
	Run(ctx context.Context, id domain.RunID) (*domain.Run, error)

	// RunForSlot returns the run recorded for a check and slot, which is how
	// the scheduler avoids starting a second one. It returns ErrNotFound
	// when the slot has not been run.
	RunForSlot(ctx context.Context, id domain.CheckID, slot domain.Slot) (*domain.Run, error)

	// RecentRuns returns a check's runs, newest first.
	RecentRuns(ctx context.Context, id domain.CheckID, limit int) ([]*domain.Run, error)

	// ActiveBinding returns the binding currently in force for a check.
	ActiveBinding(ctx context.Context, id domain.CheckID) (domain.Binding, error)

	// Snapshot returns one snapshot by its content address.
	Snapshot(ctx context.Context, id domain.SnapshotID) (domain.Snapshot, error)

	// Snapshots returns the snapshot index for a check, which carries the
	// known-good retention invariant with it.
	Snapshots(ctx context.Context, id domain.CheckID) (*domain.SnapshotIndex, error)

	// Incidents returns the incident log for a check, which carries the
	// at-most-one-open invariant with it.
	Incidents(ctx context.Context, id domain.CheckID) (*domain.IncidentLog, error)
}

// Writer is the mutating half of the store, available only inside a
// transaction.
type Writer interface {
	// SaveCheck stores a check and all of its definition versions.
	SaveCheck(ctx context.Context, c *domain.Check) error

	// CreateRun records a new run. It returns domain.ErrDuplicateRun if the
	// (check, slot) pair already has one, which is how "one run per slot"
	// survives two runners racing.
	CreateRun(ctx context.Context, r *domain.Run) error

	// UpdateRun stores a change to a run that has not yet ended. It returns
	// domain.ErrTerminal if the stored run is already terminal, so that
	// immutability holds even against a caller holding a stale copy.
	UpdateRun(ctx context.Context, r *domain.Run) error

	// PutSnapshot stores a snapshot body under its content address. Storing
	// the same content twice is a no-op.
	PutSnapshot(ctx context.Context, s domain.Snapshot) error

	// MarkSnapshotKnownGood records that extraction succeeded against a
	// snapshot.
	MarkSnapshotKnownGood(ctx context.Context, id domain.SnapshotID) error

	// DeleteSnapshots removes pruned snapshot bodies. The store must refuse
	// to delete the last known-good snapshot for a check.
	DeleteSnapshots(ctx context.Context, ids []domain.SnapshotID) error

	// SaveBinding stores a binding version.
	SaveBinding(ctx context.Context, b domain.Binding) error

	// ActivateBinding makes a binding version the one in force. The store
	// must reject a binding whose Origin is OriginRepaired unless its
	// incident records a human approval.
	ActivateBinding(ctx context.Context, id domain.CheckID, version int) error

	// SaveIncident stores an incident and its attempts and proposal.
	SaveIncident(ctx context.Context, i *domain.Incident) error
}

// Tx is a unit of work: reads and writes that commit or roll back together.
type Tx interface {
	Reader
	Writer
}

// Store is the persistence port.
//
// Transactions are expressed as a callback rather than as Begin/Commit
// handles so that the core cannot leak one. The store commits when fn returns
// nil and rolls back when it returns an error or panics. Nothing in the core
// knows this is SQLite.
type Store interface {
	Reader

	// Update runs fn in a read-write transaction.
	Update(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// View runs fn in a read-only transaction, for reads that must agree
	// with each other.
	View(ctx context.Context, fn func(ctx context.Context, r Reader) error) error
}
