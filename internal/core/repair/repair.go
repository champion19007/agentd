// Package repair holds repair orchestration: turning a breakage into a
// candidate binding, verifying that candidate against stored evidence, and
// recording it as a proposal.
//
// Applying a proposal is not this package's job and there is no function here
// that does it. The only way a proposed binding becomes an active one runs
// through a human approval recorded on the incident. That is not an accident
// of the implementation; it is the product.
package repair

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

var (
	// ErrNotVerified reports a candidate that did not survive verification.
	// Such a candidate is discarded and never shown to an operator as a
	// suggestion: an unverified repair is exactly the guess Agentd promises
	// not to make.
	ErrNotVerified = errors.New("the candidate binding did not reproduce the intent")

	// ErrNoEvidence reports that there is not enough stored evidence to
	// verify a repair: either no known-good capture to compare against, or no
	// current capture to derive from.
	ErrNoEvidence = errors.New("not enough stored evidence to verify a repair")
)

// Deps are the capabilities an Orchestrator needs.
type Deps struct {
	Clock   ports.Clock
	IDs     ports.IDs
	Store   ports.Store
	Model   ports.Model
	Extract ports.Extractor
}

// Orchestrator proposes repairs.
type Orchestrator struct {
	deps    Deps
	dialect string
}

// New builds an Orchestrator. The dialect is the locator language candidates
// will be expressed in, chosen by the composition root from whatever the
// wired Extractor understands.
func New(d Deps, dialect string) *Orchestrator {
	return &Orchestrator{deps: d, dialect: dialect}
}

// Propose attempts one repair for an open incident.
//
// The sequence is deliberately ask, verify, record, then ask a human. Every
// failure path leaves the incident either still open with an attempt spent or
// closed -- never with a binding applied.
func (o *Orchestrator) Propose(ctx context.Context, c *domain.Check, i *domain.Incident) (*domain.RepairProposal, error) {
	if !i.Open() {
		return nil, domain.ErrIncidentClosed
	}
	if i.AttemptsRemaining() == 0 {
		return nil, domain.ErrRepairBudgetExhausted
	}

	ev, err := o.evidence(ctx, c.ID())
	if err != nil {
		return nil, err
	}

	def := c.ActiveDefinition()
	now := o.deps.Clock.Now()

	locators, rationale, err := o.ask(ctx, def.Intent, ev.current)
	if err != nil {
		if _, rerr := i.RecordAttempt(domain.AttemptModelError, explainModelFailure(err), now); rerr != nil {
			return nil, rerr
		}
		return nil, err
	}

	candidate := domain.Binding{
		ID:                o.deps.IDs.NewBindingID(),
		CheckID:           c.ID(),
		DefinitionVersion: def.Version,
		IntentKind:        def.Intent.Kind(),
		// Derived against the shape the source has now, which is what makes
		// it a repair rather than a restatement of the broken binding.
		Fingerprint: ev.current.Fingerprint(),
		Version:     ev.nextVersion,
		Origin:      domain.OriginRepaired,
		Locators:    locators,
		DerivedAt:   now,
		DerivedFrom: ev.current.ID(),
	}

	if err := o.verify(ctx, def.Intent, candidate, ev); err != nil {
		if _, rerr := i.RecordAttempt(domain.AttemptUnverified, explainVerifyFailure(err), now); rerr != nil {
			return nil, rerr
		}
		return nil, err
	}

	if _, err := i.RecordAttempt(domain.AttemptProposed, "a candidate was found and checked against a stored capture", now); err != nil {
		return nil, err
	}

	proposal := domain.RepairProposal{
		Binding:   candidate,
		Rationale: rationale,
		// The capture the candidate was actually replayed against.
		VerifiedAgainst: ev.current.ID(),
		VerifiedAt:      now,
	}
	if err := i.Propose(proposal, now); err != nil {
		return nil, err
	}

	// The candidate is stored but not activated. ActivateBinding is not
	// called here and is not called anywhere in this package.
	if err := o.deps.Store.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveBinding(ctx, candidate); err != nil {
			return err
		}
		return tx.SaveIncident(ctx, i)
	}); err != nil {
		return nil, err
	}

	return i.Proposal(), nil
}

// evidence is everything stored that a repair is derived from and checked
// against.
type evidence struct {
	// current is the most recent capture: the source as it is now, which is
	// what a new binding must work against.
	current domain.Snapshot

	// expected is the last result the check extracted successfully. A
	// candidate has to produce something shaped like this, not merely
	// something.
	expected domain.Extraction

	// hasExpected reports whether expected is populated.
	hasExpected bool

	// nextVersion is the binding version a repair would become.
	nextVersion int
}

// evidence gathers what is needed before a model is asked anything, so that a
// check with nothing to verify against never spends a model call.
func (o *Orchestrator) evidence(ctx context.Context, id domain.CheckID) (evidence, error) {
	index, err := o.deps.Store.Snapshots(ctx, id)
	if err != nil {
		return evidence{}, err
	}
	all := index.All() // newest first
	if len(all) == 0 {
		return evidence{}, ErrNoEvidence
	}
	current := all[0]
	if err := current.Verify(); err != nil {
		// Corrupt evidence must never be used to verify a repair.
		return evidence{}, err
	}

	// A known-good capture must exist, even though it is not what the
	// candidate is replayed against: its absence means Agentd has never seen
	// this check work, so it has no idea what working looks like.
	if _, err := index.LatestKnownGood(); err != nil {
		return evidence{}, ErrNoEvidence
	}

	ev := evidence{current: current, nextVersion: 1}

	if b, err := o.deps.Store.ActiveBinding(ctx, id); err == nil {
		ev.nextVersion = b.Version + 1
	} else if !errors.Is(err, ports.ErrNotFound) {
		return evidence{}, err
	}

	switch expected, err := o.deps.Store.LastResult(ctx, id); {
	case err == nil:
		ev.expected, ev.hasExpected = expected, true
	case errors.Is(err, ports.ErrNotFound):
		// Tolerated: a check can have a known-good capture without a stored
		// result if it was set up before results were kept.
	default:
		return evidence{}, err
	}

	return ev, nil
}

// verify replays the candidate against the current capture and checks what
// comes out. It is the step that makes a proposal worth showing to a human.
func (o *Orchestrator) verify(ctx context.Context, in domain.Intent, candidate domain.Binding, ev evidence) error {
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("%w: %s", ErrNotVerified, err)
	}
	if err := candidate.Covers(in); err != nil {
		return fmt.Errorf("%w: %s", ErrNotVerified, err)
	}

	got, err := o.deps.Extract.Extract(ctx, rawFrom(ev.current), candidate)
	if err != nil {
		return fmt.Errorf("%w: it failed to run against the stored capture", ErrNotVerified)
	}
	if f := got.Satisfies(in); f != nil {
		return fmt.Errorf("%w: %s", ErrNotVerified, f.Summary)
	}

	// Satisfying the intent proves the candidate finds something of the right
	// shape. It does not prove it found the right thing: a locator pointing at
	// the wrong element of the right kind would pass. Comparing against what
	// the check used to see is what catches that.
	if ev.hasExpected {
		if err := sameShape(ev.expected, got); err != nil {
			return fmt.Errorf("%w: %s", ErrNotVerified, err)
		}
	}
	return nil
}

// sameShape reports whether a candidate's result is recognisably the same
// kind of thing the check used to extract.
//
// It deliberately does not require equal values. The source changed; the
// values may have changed with it, and demanding they match would reject
// every honest repair of a page whose content moved on.
func sameShape(expected, got domain.Extraction) error {
	if expected.Kind != got.Kind {
		return fmt.Errorf("it returns a %s where this check used to get a %s", got.Kind, expected.Kind)
	}
	switch expected.Kind {
	case domain.IntentScalar:
		if expected.Scalar.Type != got.Scalar.Type {
			return fmt.Errorf("it returns a %s where this check used to get a %s", got.Scalar.Type, expected.Scalar.Type)
		}
	case domain.IntentRecord:
		return sameFields(expected.Record, got.Record)
	case domain.IntentCollection:
		if len(got.Collection) == 0 && len(expected.Collection) > 0 {
			return errors.New("it finds nothing where this check used to find entries")
		}
		if len(expected.Collection) > 0 && len(got.Collection) > 0 {
			return sameFields(expected.Collection[0], got.Collection[0])
		}
	}
	return nil
}

// sameFields reports whether two records carry the same field names.
func sameFields(expected, got domain.Record) error {
	for name := range expected {
		if _, ok := got[name]; !ok {
			return fmt.Errorf("it no longer finds %q, which this check used to get", name)
		}
	}
	return nil
}

// rawFrom rebuilds a fetch response from a stored capture, so that the
// extractor sees exactly what it would have seen at the time.
func rawFrom(s domain.Snapshot) domain.RawResponse {
	return domain.RawResponse{
		ContentType: s.ContentType(),
		Body:        s.Body(),
		Fingerprint: s.Fingerprint(),
		FetchedAt:   s.CapturedAt(),
	}
}

// ask puts the problem to a model and parses what comes back.
func (o *Orchestrator) ask(ctx context.Context, in domain.Intent, current domain.Snapshot) ([]domain.Locator, string, error) {
	res, err := o.deps.Model.Complete(ctx, ports.ModelRequest{
		Purpose: ports.PurposeRepair,
		System:  systemPrompt,
		Prompt:  prompt(in, o.dialect, current),
		// The same break should produce the same proposal, so that an
		// operator reviewing one twice sees the same thing.
		Deterministic:   true,
		MaxOutputTokens: 1024,
	})
	if err != nil {
		return nil, "", err
	}
	if res.Truncated {
		// A half-written locator may well parse and be wrong.
		return nil, "", errors.New("the model's answer was cut off")
	}
	return parse(res.Text, o.dialect)
}

const systemPrompt = "You locate data in documents. You are given a description of what someone wants to find and the current version of a source. Reply with one line per target in the form TARGET<tab>EXPRESSION, then a final line beginning RATIONALE: followed by one sentence in plain language explaining your choice."

// prompt builds the request. It describes the intent in the operator's own
// words, because those words are what the candidate is verified against.
func prompt(in domain.Intent, dialect string, current domain.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dialect: %s\n", dialect)
	fmt.Fprintf(&b, "What to find: %s\n", in.Description())

	b.WriteString("Targets:\n")
	switch v := in.(type) {
	case domain.ScalarIntent:
		fmt.Fprintf(&b, "  %s (%s)\n", v.Name(), v.Type)
	case domain.RecordIntent:
		for _, f := range v.Fields {
			fmt.Fprintf(&b, "  %s (%s) %s\n", f.Name, f.Type, f.Description)
		}
	case domain.CollectionIntent:
		fmt.Fprintf(&b, "  %s (the repeating element)\n", domain.CollectionRoot)
		for _, f := range v.Element.Fields {
			fmt.Fprintf(&b, "  %s (%s) %s\n", f.Name, f.Type, f.Description)
		}
	}

	fmt.Fprintf(&b, "\nSource (%s):\n%s\n", current.ContentType(), string(current.Body()))
	return b.String()
}

// parse reads the model's answer.
//
// It is strict on purpose: anything it does not understand is discarded
// rather than guessed at, because a misparsed locator becomes a proposal an
// operator is asked to trust.
func parse(text, dialect string) ([]domain.Locator, string, error) {
	var locators []domain.Locator
	var rationale string
	seen := make(map[string]bool)

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "RATIONALE:"); ok {
			rationale = strings.TrimSpace(rest)
			continue
		}
		target, expr, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		target, expr = strings.TrimSpace(target), strings.TrimSpace(expr)
		if target == "" || expr == "" || seen[target] {
			continue
		}
		seen[target] = true
		locators = append(locators, domain.Locator{
			Target: target, Dialect: dialect, Expression: expr,
		})
	}

	if len(locators) == 0 {
		return nil, "", errors.New("the model did not return any way of locating the data")
	}
	if rationale == "" {
		return nil, "", errors.New("the model did not explain its proposal")
	}
	return locators, rationale, nil
}

// explainModelFailure and explainVerifyFailure put an attempt's outcome in
// the operator's terms. The attempt log is something a person reads when
// deciding whether to trust a proposal, so it must not be full of Go errors.
func explainModelFailure(error) string {
	return "Agentd could not get a usable answer from the model while working out a repair"
}

func explainVerifyFailure(err error) string {
	return "a candidate was found but it did not reproduce what this check asks for: " +
		strings.TrimPrefix(strings.TrimPrefix(err.Error(), ErrNotVerified.Error()), ": ")
}
