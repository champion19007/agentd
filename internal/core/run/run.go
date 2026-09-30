// Package run holds run orchestration: driving a single check through fetch,
// extract and outcome classification, entirely through ports.
//
// This is the busiest part of the core and still performs no I/O of its own.
// It asks a Source for bytes, an Extractor for meaning and a Store for
// memory, and its whole job is deciding what the answers add up to.
package run

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/policy"
	"github.com/champion19007/agentd/internal/ports"
)

// Deps are the capabilities an Orchestrator needs. Gathering them into one
// struct keeps the constructor readable and makes it obvious at a glance
// exactly how much of the outside world this package can reach.
type Deps struct {
	Clock       ports.Clock
	IDs         ports.IDs
	Store       ports.Store
	Source      ports.Source
	Extract     ports.Extractor
	Fingerprint ports.Fingerprinter
	Secrets     ports.SecretResolver
	Model       ports.Model
	Notifier    ports.Notifier
	Metrics     ports.Metrics
}

// Orchestrator runs one check in one slot.
type Orchestrator struct {
	deps        Deps
	degradation domain.DegradationLevel
	mu          sync.RWMutex
}

// New builds an Orchestrator.
func New(d Deps) *Orchestrator {
	if d.Metrics == nil {
		d.Metrics = ports.NoopMetrics{}
	}
	return &Orchestrator{deps: d}
}

// SetDegradation updates the operational load shedding tier.
func (o *Orchestrator) SetDegradation(level domain.DegradationLevel) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.degradation = level
}

// Degradation reports the current operational load shedding tier.
func (o *Orchestrator) Degradation() domain.DegradationLevel {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.degradation
}

// Outcome is what a single run amounted to, handed back so that the caller
// can feed it to the policy layer without re-reading the store.
type Outcome struct {
	// Run is the finished run, always terminal.
	Run *domain.Run

	// Snapshot is what was captured, if the fetch got that far.
	Snapshot domain.Snapshot

	// Captured reports whether Snapshot is populated.
	Captured bool
}

// Run executes one check for one slot and records the result.
//
// It always produces a terminal run, including when something goes wrong:
// a check that fails leaves a record saying so, because an absent run and a
// failed run mean very different things to someone reading the history, and
// only one of them is honest.
func (o *Orchestrator) Run(ctx context.Context, c *domain.Check, slot domain.Slot) (Outcome, error) {
	def := c.ActiveDefinition()
	started := o.deps.Clock.Now()

	r, err := domain.NewRun(o.deps.IDs.NewRunID(), c.ID(), slot, def.Version, started)
	if err != nil {
		return Outcome{}, err
	}

	// Claim the slot before doing any work. If another runner already has it,
	// stop here rather than fetching the source twice.
	if err := o.deps.Store.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.CreateRun(ctx, r)
	}); err != nil {
		return Outcome{}, err
	}

	attempt := 1
	var out Outcome
	for {
		out = o.execute(ctx, c, r)

		// Check if a retryable failure occurred and whether policy asks for a retry
		if out.Run.State() == domain.StateFailed || out.Run.State() == domain.StateDegraded {
			sit := policy.Situation{
				Check:       c,
				Run:         out.Run,
				Attempt:     attempt,
				Degradation: o.Degradation(),
			}
			dec := policy.Decide(sit)
			if dec.Retry && ctx.Err() == nil {
				if err := o.deps.Clock.Sleep(ctx, dec.RetryAfter); err == nil {
					attempt++
					r, _ = domain.NewRun(r.ID(), c.ID(), slot, def.Version, started)
					continue
				}
			}
		}
		break
	}

	// Deliver notification if policy requires it and a notifier is wired
	if o.deps.Notifier != nil && out.Run.Terminal() {
		sit := policy.Situation{
			Check:       c,
			Run:         out.Run,
			Attempt:     attempt,
			Degradation: o.Degradation(),
		}
		dec := policy.Decide(sit)
		if notif, ok := policy.Notification(sit, dec, o.deps.Clock.Now()); ok {
			_ = o.deps.Notifier.Deliver(ctx, notif)
		}
	}

	tPersistStart := o.deps.Clock.Now()
	if err := o.persist(ctx, out); err != nil {
		return out, err
	}
	o.deps.Metrics.RecordRunDuration(string(c.ID()), "persist", o.deps.Clock.Now().Sub(tPersistStart).Seconds())

	// Record terminal run state and total execution duration
	if out.Run != nil && out.Run.Terminal() {
		o.deps.Metrics.RecordRun(string(c.ID()), string(out.Run.State()))
		o.deps.Metrics.RecordRunDuration(string(c.ID()), "total", o.deps.Clock.Now().Sub(started).Seconds())
	}
	return out, nil
}

// execute drives the run to a terminal state. It never returns an error for
// anything the source or the extractor did: those become classified failures
// on the run itself, which is the whole point of the taxonomy.
func (o *Orchestrator) execute(ctx context.Context, c *domain.Check, r *domain.Run) Outcome {
	def := c.ActiveDefinition()

	binding, err := o.deps.Store.ActiveBinding(ctx, c.ID())
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return o.fail(r, domain.Classify(err))
	}
	if errors.Is(err, ports.ErrNotFound) {
		// A check with no binding has never been set up properly. That is a
		// configuration problem, not a source problem, so it is fatal rather
		// than structural: there is no earlier binding to repair.
		return o.fail(r, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "no_binding",
			Summary: "this check has no way of locating what it is looking for yet",
		})
	}

	if err := r.Start(o.deps.Clock.Now(), binding.Version); err != nil {
		// Only reachable if the run was already terminal, which means this
		// run was handled twice. Record it rather than returning a run that
		// never ended.
		return o.fail(r, domain.Failure{
			Class:   domain.ClassFatal,
			Code:    "bad_state",
			Summary: "this run could not be started; this is a bug in Agentd, not a problem with the source",
			Detail:  err.Error(),
		})
	}

	var secrets domain.SecretBundle
	if o.deps.Secrets != nil {
		var err error
		secrets, err = o.deps.Secrets.Resolve(ctx, def.Source.SecretRefs())
		if err != nil {
			// A credential that will not resolve cannot be retried into
			// existence, so classify it as auth regardless of what the resolver
			// reported.
			f := domain.Classify(err)
			f.Class = domain.ClassAuth
			return o.fail(r, f)
		}
	} else if len(def.Source.SecretRefs()) > 0 {
		return o.fail(r, domain.Failure{
			Class:   domain.ClassAuth,
			Code:    "secret_resolver_missing",
			Summary: "this check requires secrets but no secret resolver is configured",
		})
	}

	tFetchStart := o.deps.Clock.Now()
	raw, err := o.deps.Source.Fetch(ctx, def.Source, secrets)
	o.deps.Metrics.RecordRunDuration(string(c.ID()), "fetch", o.deps.Clock.Now().Sub(tFetchStart).Seconds())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return o.interrupt(r, "Agentd was shutting down while this check was running")
		}
		return o.fail(r, domain.Classify(err))
	}

	// Fingerprint before extracting. It has to work on a source whose binding
	// is already broken, because that is exactly when knowing the shape
	// changed is worth something.
	tFPStart := o.deps.Clock.Now()
	fp, err := o.deps.Fingerprint.Fingerprint(ctx, raw)
	o.deps.Metrics.RecordRunDuration(string(c.ID()), "fingerprint", o.deps.Clock.Now().Sub(tFPStart).Seconds())
	if err != nil {
		return o.fail(r, domain.Classify(err))
	}
	raw.Fingerprint = fp

	snap, err := domain.NewSnapshot(c.ID(), raw.ContentType, raw.Body, fp, o.deps.Clock.Now())
	if err != nil {
		return o.fail(r, domain.Failure{
			Class:   domain.ClassTransient,
			Code:    "empty_response",
			Summary: "the source returned nothing to look at",
			Detail:  err.Error(),
		})
	}
	out := Outcome{Run: r, Snapshot: snap, Captured: true}

	tExtStart := o.deps.Clock.Now()
	extraction, err := o.deps.Extract.Extract(ctx, raw, binding)
	o.deps.Metrics.RecordRunDuration(string(c.ID()), "extract", o.deps.Clock.Now().Sub(tExtStart).Seconds())
	if err != nil {
		out.Run = o.fail(r, domain.Classify(err)).Run
		return out
	}

	// Judging the extraction against the intent is what separates "the
	// binding broke" from "there was less here than usual". Only the domain
	// knows which fields were required, so only the domain can tell them
	// apart.
	if f := extraction.Satisfies(def.Intent); f != nil {
		if f.Class.Degrades() {
			_ = r.Degrade(o.deps.Clock.Now(), snap.ID(), extraction, *f)
		} else {
			_ = r.Fail(o.deps.Clock.Now(), *f)
		}
		return out
	}

	previous, err := o.deps.Store.LastResult(ctx, c.ID())
	switch {
	case errors.Is(err, ports.ErrNotFound):
		// Nothing to compare against. A first observation is reported as a
		// change rather than as quiet, because calling it quiet would claim
		// a comparison that never happened.
		explanation := "this is the first time this check has run"
		if o.deps.Model != nil {
			if resp, mErr := o.deps.Model.Complete(ctx, ports.ModelRequest{
				Purpose:       ports.PurposeExplain,
				Prompt:        fmt.Sprintf("Summarize initial observation for check %q: %+v", c.ID(), extraction),
				Deterministic: true,
			}); mErr == nil && resp.Text != "" {
				explanation = resp.Text
			}
		}
		_ = r.Changed(o.deps.Clock.Now(), snap.ID(), extraction, explanation)
	case err != nil:
		out.Run = o.fail(r, domain.Classify(err)).Run
		return out
	case extraction.Equal(previous):
		// Hash gate: if normalized payload is unchanged, finish quiet.
		// Crucial: Model evaluation is NEVER invoked when hash/payload is unchanged,
		// honoring the cost architecture.
		_ = r.Quiet(o.deps.Clock.Now(), snap.ID(), extraction)
	default:
		// Graceful degradation: if model evaluation is shed under pressure, mark changed directly
		if o.Degradation().ShedsModel() {
			_ = r.Changed(o.deps.Clock.Now(), snap.ID(), extraction, describe(def.Intent)+" (model evaluation bypassed under load shedding)")
			return out
		}

		// Extraction differs: invoke structured change evaluation.
		tEvalStart := o.deps.Clock.Now()
		eval, evalErr := EvaluateChange(ctx, o.deps.Model, def.Intent, previous, extraction, DefaultMaxExcerptBytes)
		o.deps.Metrics.RecordRunDuration(string(c.ID()), "evaluate", o.deps.Clock.Now().Sub(tEvalStart).Seconds())
		if evalErr != nil {
			out.Run = o.fail(r, domain.Classify(evalErr)).Run
			return out
		}
		switch eval.Verdict {
		case VerdictUnchanged:
			_ = r.Quiet(o.deps.Clock.Now(), snap.ID(), extraction)
		case VerdictUncertain:
			_ = r.Degrade(o.deps.Clock.Now(), snap.ID(), extraction, domain.Failure{
				Class:   domain.ClassSemantic,
				Code:    "uncertain_evaluation",
				Summary: "the change could not be verified with confidence",
				Detail:  eval.Explanation,
			})
		default: // VerdictChanged
			_ = r.Changed(o.deps.Clock.Now(), snap.ID(), extraction, eval.Explanation)
		}
	}
	return out
}

// describe says what changed, in the operator's terms.
func describe(in domain.Intent) string {
	return in.Name() + " is different from the last time Agentd looked"
}

// fail ends a run with a classified failure.
func (o *Orchestrator) fail(r *domain.Run, f domain.Failure) Outcome {
	if !f.Class.Valid() {
		f.Class = domain.ClassTransient
	}
	if f.Summary == "" {
		f.Summary = "the check could not complete"
	}
	_ = r.Fail(o.deps.Clock.Now(), f)
	return Outcome{Run: r}
}

// interrupt ends a run that was cut short by shutdown.
func (o *Orchestrator) interrupt(r *domain.Run, reason string) Outcome {
	_ = r.Interrupt(o.deps.Clock.Now(), reason)
	return Outcome{Run: r}
}

// persist writes the run, its snapshot and any retention changes in one
// transaction, so that a crash cannot leave a run pointing at a snapshot that
// was never stored.
func (o *Orchestrator) persist(ctx context.Context, out Outcome) error {
	if out.Run == nil {
		return nil
	}
	// Every path through execute is supposed to reach a terminal state. If one
	// did not, a state transition was refused and the refusal went unnoticed,
	// which would leave a run stuck pending forever and a gap in the record
	// with no explanation. Fail loudly rather than storing that.
	if !out.Run.Terminal() {
		return fmt.Errorf("agentd: run %s for check %s finished in non-terminal state %q; this is a bug in Agentd",
			out.Run.ID(), out.Run.CheckID(), out.Run.State())
	}
	return o.deps.Store.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
		if out.Captured {
			// Graceful degradation: under reduced retention, only persist snapshots that succeeded
			if !o.Degradation().ReducesRetention() || out.Run.State().Succeeded() {
				if err := tx.PutSnapshot(ctx, out.Snapshot); err != nil {
					return err
				}
			}
			// A run that extracted cleanly proves this capture is one a
			// repair can later be verified against.
			if out.Run.State().Succeeded() {
				if err := tx.MarkSnapshotKnownGood(ctx, out.Snapshot.CheckID(), out.Snapshot.ID()); err != nil {
					return err
				}
			}
		}
		if err := tx.UpdateRun(ctx, out.Run); err != nil {
			return err
		}
		// In the same transaction as the run itself. A trail written
		// separately could be left lying by a crash between the two writes.
		return tx.AppendAudit(ctx, domain.Audit(
			"run:"+string(out.Run.ID()),
			out.Run.EndedAt(),
			domain.ActionRunRecorded,
			domain.SubjectRun,
			string(out.Run.ID()),
			string(out.Run.State())+": "+out.Run.Explanation(),
		))
	})
}

// SkipOverloaded records a slot that was never attempted because the runner
// was saturated. Recording the skip rather than dropping it is what keeps a
// gap in the history explicable.
func (o *Orchestrator) SkipOverloaded(ctx context.Context, c *domain.Check, slot domain.Slot, reason string) (*domain.Run, error) {
	now := o.deps.Clock.Now()
	r, err := domain.NewRun(o.deps.IDs.NewRunID(), c.ID(), slot, c.ActiveVersion(), now)
	if err != nil {
		return nil, err
	}
	if err := r.SkipOverloaded(now, reason); err != nil {
		return nil, err
	}
	if err := o.deps.Store.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
		return tx.CreateRun(ctx, r)
	}); err != nil {
		return nil, err
	}
	return r, nil
}
