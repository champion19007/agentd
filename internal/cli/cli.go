// Package cli is a driving adapter: the command line surface.
//
// Commands translate arguments into core or store calls and format what comes
// back. No decision lives here. In particular, `agentd approve` does not decide
// whether a repair is good -- it carries a human's decision to the incident,
// which is the only thing that can act on it.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

// Version is the build's version, set at link time.
var Version = "dev"

// Env is everything a command needs from outside itself. Passing it rather
// than reaching for globals is what makes the commands testable.
type Env struct {
	Out  io.Writer
	Err  io.Writer
	Now  func() time.Time
	Open func(ctx context.Context, path string) (*sqlite.Store, error)
}

// DefaultEnv returns an Env wired to the real world.
func DefaultEnv() Env {
	return Env{
		Out: os.Stdout,
		Err: os.Stderr,
		Now: func() time.Time { return time.Now().UTC() },
		Open: func(ctx context.Context, path string) (*sqlite.Store, error) {
			return sqlite.Open(ctx, sqlite.Options{Path: path})
		},
	}
}

// DefaultDBPath is where the database lives when nothing says otherwise.
func DefaultDBPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "agentd", "agentd.db")
	}
	return "agentd.db"
}

// command is one subcommand.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env Env, args []string) error
}

func commands() []command {
	return []command{
		{"gc", "apply the retention policy and report what was removed", runGC},
		{"backup", "write a consistent copy of the database", runBackup},
		{"checks", "list configured checks and their state", runChecks},
		{"runs", "show recent runs for a check", runRuns},
		{"incidents", "show open incidents awaiting a decision", runIncidents},
		{"approve", "approve a proposed repair", runApprove},
		{"reject", "reject a proposed repair", runReject},
		{"audit", "show the audit trail", runAudit},
		{"version", "print the version", runVersion},
	}
}

// Main runs the CLI and returns a process exit code.
func Main(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(env.Out)
		return 0
	}

	for _, c := range commands() {
		if c.name != args[0] {
			continue
		}
		if err := c.run(ctx, env, args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 2
			}
			fmt.Fprintf(env.Err, "agentd: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(env.Err, "agentd: no command called %q\n\n", args[0])
	usage(env.Err)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "agentd %s -- scheduled checks over external sources\n\n", Version)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "    agentd <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands() {
		fmt.Fprintf(tw, "    %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every command takes -db to point at a database file.")
	fmt.Fprintln(w, "Flags come before arguments: agentd approve -by dana -db path inc-1")
}

// flags builds a flag set with the database path every command needs.
func flags(name string, env Env) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Err)
	db := fs.String("db", DefaultDBPath(), "path to the agentd database")
	return fs, db
}

// withStore opens the database, runs fn, and closes it.
func withStore(ctx context.Context, env Env, path string, fn func(*sqlite.Store) error) error {
	store, err := env.Open(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	return fn(store)
}

// --- gc ---------------------------------------------------------------------

func runGC(ctx context.Context, env Env, args []string) error {
	fs, db := flags("gc", env)
	runDays := fs.Int("runs", 0, "days of finished runs to keep (0 uses the default)")
	incidentDays := fs.Int("incidents", 0, "days to keep closed incidents (0 uses the default)")
	keep := fs.Int("snapshots", 0, "captures to keep per check (0 uses the default)")
	dry := fs.Bool("dry-run", false, "report what would be removed without removing it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	policy := domain.DefaultRetention()
	if *runDays > 0 {
		policy.RunTTL = time.Duration(*runDays) * 24 * time.Hour
	}
	if *incidentDays > 0 {
		policy.IncidentTTL = time.Duration(*incidentDays) * 24 * time.Hour
	}
	if *keep > 0 {
		policy.SnapshotsPerCheck = *keep
	}
	if err := policy.Validate(); err != nil {
		return err
	}

	return withStore(ctx, env, *db, func(store *sqlite.Store) error {
		if *dry {
			// Retention destroys data, so there is a way to look first. The
			// numbers come from the same policy the real sweep would use.
			sweep, err := store.PlanGC(ctx, policy, env.Now())
			if err != nil {
				return err
			}
			report(env.Out, "would remove", sweep)
			return nil
		}

		sweep, err := store.GC(ctx, policy, env.Now())
		if err != nil {
			return err
		}
		report(env.Out, "removed", sweep)
		return nil
	})
}

func report(w io.Writer, verb string, sweep domain.Sweep) {
	if sweep.Empty() {
		fmt.Fprintf(w, "nothing to remove\n")
		return
	}
	fmt.Fprintf(w, "%s %d runs, %d captures, %d closed incidents\n",
		verb, sweep.Runs, sweep.Snapshots, sweep.Incidents)
}

// --- backup -----------------------------------------------------------------

func runBackup(ctx context.Context, env Env, args []string) error {
	fs, db := flags("backup", env)
	to := fs.String("to", "", "destination file (default: alongside the database, timestamped)")
	verify := fs.Bool("verify", true, "open the backup afterwards and check it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dest := *to
	if dest == "" {
		stamp := env.Now().Format("20060102T150405Z")
		dest = filepath.Join(filepath.Dir(*db), fmt.Sprintf("agentd-%s.db", stamp))
	}

	return withStore(ctx, env, *db, func(store *sqlite.Store) error {
		size, err := store.Backup(ctx, dest)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "wrote %s (%s)\n", dest, humanBytes(size))

		if *verify {
			// A backup nobody has opened is a hope, not a backup.
			version, err := sqlite.VerifyBackup(ctx, dest)
			if err != nil {
				return err
			}
			fmt.Fprintf(env.Out, "verified: schema version %d, integrity check passed\n", version)
		}
		return nil
	})
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// --- checks -----------------------------------------------------------------

func runChecks(ctx context.Context, env Env, args []string) error {
	fs, db := flags("checks", env)
	if err := fs.Parse(args); err != nil {
		return err
	}

	return withStore(ctx, env, *db, func(store *sqlite.Store) error {
		checks, err := store.EnabledChecks(ctx)
		if err != nil {
			return err
		}
		if len(checks) == 0 {
			fmt.Fprintln(env.Out, "no checks are configured")
			return nil
		}

		tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CHECK\tWATCHING\tEVERY\tLAST RUN")
		for _, c := range checks {
			def := c.ActiveDefinition()
			last := "never"
			if runs, err := store.RecentRuns(ctx, c.ID(), 1); err == nil && len(runs) > 0 {
				last = string(runs[0].State())
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.ID(), def.Intent.Name(), def.Schedule.Interval, last)
		}
		return tw.Flush()
	})
}

func runRuns(ctx context.Context, env Env, args []string) error {
	fs, db := flags("runs", env)
	limit := fs.Int("n", 20, "how many runs to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd runs <check-id>")
	}
	id := domain.CheckID(fs.Arg(0))

	return withStore(ctx, env, *db, func(store *sqlite.Store) error {
		runs, err := store.RecentRuns(ctx, id, *limit)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			fmt.Fprintf(env.Out, "%s has never run\n", id)
			return nil
		}

		tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "WHEN\tOUTCOME\tWHAT HAPPENED")
		for _, r := range runs {
			when := r.EndedAt()
			if when.IsZero() {
				when = r.CreatedAt()
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", when.Format(time.RFC3339), r.State(), r.Explanation())
		}
		return tw.Flush()
	})
}

// --- incidents and decisions ------------------------------------------------

func runIncidents(ctx context.Context, env Env, args []string) error {
	fs, db := flags("incidents", env)
	if err := fs.Parse(args); err != nil {
		return err
	}

	return withStore(ctx, env, *db, func(store *sqlite.Store) error {
		checks, err := store.EnabledChecks(ctx)
		if err != nil {
			return err
		}

		found := 0
		for _, c := range checks {
			log, err := store.Incidents(ctx, c.ID())
			if err != nil {
				return err
			}
			inc, open := log.Current()
			if !open {
				continue
			}
			found++

			fmt.Fprintf(env.Out, "%s  (%s)\n", inc.ID(), c.ID())
			fmt.Fprintf(env.Out, "    %s\n", inc.Cause().Summary)
			fmt.Fprintf(env.Out, "    opened %s, %d repair attempts left\n",
				inc.OpenedAt().Format(time.RFC3339), inc.AttemptsRemaining())

			if p := inc.Proposal(); p != nil {
				fmt.Fprintf(env.Out, "\n    Agentd proposes:\n")
				fmt.Fprintf(env.Out, "        %s\n", p.Rationale)
				for _, l := range p.Binding.Locators {
					fmt.Fprintf(env.Out, "        %s -> %s\n", l.Target, l.Expression)
				}
				fmt.Fprintf(env.Out, "    checked against capture %s:\n", short(string(p.VerifiedAgainst)))
				for _, g := range p.Gates {
					mark := "x"
					if g.Passed {
						mark = "ok"
					}
					fmt.Fprintf(env.Out, "        [%s] %s: %s\n", mark, g.Gate, g.Detail)
				}
				fmt.Fprintf(env.Out, "\n    Nothing has been changed. To decide:\n")
				fmt.Fprintf(env.Out, "        agentd approve %s --by <your name>\n", inc.ID())
				fmt.Fprintf(env.Out, "        agentd reject  %s --by <your name>\n", inc.ID())
			}
			fmt.Fprintln(env.Out)
		}

		if found == 0 {
			fmt.Fprintln(env.Out, "nothing is waiting for you")
		}
		return nil
	})
}

func short(id string) string {
	if len(id) > 19 {
		return id[:19] + "..."
	}
	return id
}

// decide carries a human's decision to an incident. It is shared by approve
// and reject so that the two cannot drift apart in how they identify a person.
func decide(ctx context.Context, env Env, args []string, name string, approve bool) error {
	fs, db := flags(name, env)
	by := fs.String("by", "", "who is making this decision (required)")
	edit := fs.String("locator", "", "replace a locator before approving, as target=expression (repeatable)")
	note := fs.String("note", "", "why")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentd %s <incident-id> --by <your name>", name)
	}
	if strings.TrimSpace(*by) == "" {
		// An approval with nobody attached is indistinguishable from Agentd
		// approving its own work, so the flag is not optional.
		return errors.New("--by is required: a decision has to be attributable to a person")
	}
	target := domain.IncidentID(fs.Arg(0))

	return withStore(ctx, env, *db, func(store *sqlite.Store) error {
		checks, err := store.EnabledChecks(ctx)
		if err != nil {
			return err
		}

		for _, c := range checks {
			log, err := store.Incidents(ctx, c.ID())
			if err != nil {
				return err
			}
			inc, open := log.Current()
			if !open || inc.ID() != target {
				continue
			}
			return apply(ctx, env, store, c, inc, *by, *edit, *note, approve)
		}
		return fmt.Errorf("no open incident called %q", target)
	})
}

func apply(ctx context.Context, env Env, store *sqlite.Store, c *domain.Check,
	inc *domain.Incident, by, edit, note string, approve bool,
) error {
	now := env.Now()

	if !approve {
		if err := inc.Reject(by, note, now); err != nil {
			return err
		}
	} else if edit != "" {
		locators, err := parseLocators(inc, edit)
		if err != nil {
			return err
		}
		if err := inc.ApproveWithEdits(by, locators, now); err != nil {
			return err
		}
	} else if err := inc.Approve(by, now); err != nil {
		return err
	}

	action := domain.ActionRepairRejected
	detail := "rejected the proposed repair"
	if approve {
		action = domain.ActionRepairApproved
		detail = "approved the proposed repair"
		if edit != "" {
			action = domain.ActionRepairEdited
			detail = "approved the proposed repair after editing it"
		}
	}

	err := store.WithTx(ctx, func(ctx context.Context, tx ports.Tx) error {
		if approve {
			binding, err := inc.ApprovedBinding()
			if err != nil {
				return err
			}
			if err := tx.SaveBinding(ctx, binding); err != nil {
				return err
			}
			if err := tx.SaveIncident(ctx, inc); err != nil {
				return err
			}
			// Only now, with an approval recorded, does anything change.
			if err := tx.ActivateBinding(ctx, c.ID(), binding.Version); err != nil {
				return err
			}
			if err := inc.ResolveWithRepair(ctx2time(now)); err != nil {
				return err
			}
			if err := tx.SaveIncident(ctx, inc); err != nil {
				return err
			}
		} else if err := tx.SaveIncident(ctx, inc); err != nil {
			return err
		}

		return tx.AppendAudit(ctx, domain.AuditBy(
			"decision:"+string(inc.ID()), now, by, action,
			domain.SubjectIncident, string(inc.ID()), detail,
		))
	})
	if err != nil {
		return err
	}

	if approve {
		fmt.Fprintf(env.Out, "applied the repair to %s, approved by %s\n", c.ID(), by)
	} else {
		fmt.Fprintf(env.Out, "rejected the repair for %s. The check is still broken.\n", c.ID())
	}
	return nil
}

// ctx2time exists only to keep the call above readable; the domain takes an
// instant and the CLI has one.
func ctx2time(t time.Time) time.Time { return t }

// parseLocators turns repeated target=expression pairs into locators, keeping
// the dialect of whatever the proposal already used.
func parseLocators(inc *domain.Incident, spec string) ([]domain.Locator, error) {
	p := inc.Proposal()
	if p == nil {
		return nil, domain.ErrNoProposal
	}

	dialect := ""
	byTarget := map[string]domain.Locator{}
	for _, l := range p.Binding.Locators {
		byTarget[l.Target] = l
		dialect = l.Dialect
	}

	for _, pair := range strings.Split(spec, ",") {
		target, expr, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return nil, fmt.Errorf("--locator wants target=expression, got %q", pair)
		}
		target, expr = strings.TrimSpace(target), strings.TrimSpace(expr)
		existing, known := byTarget[target]
		d := dialect
		if known {
			d = existing.Dialect
		}
		byTarget[target] = domain.Locator{Target: target, Dialect: d, Expression: expr}
	}

	targets := make([]string, 0, len(byTarget))
	for t := range byTarget {
		targets = append(targets, t)
	}
	sort.Strings(targets)

	out := make([]domain.Locator, 0, len(targets))
	for _, t := range targets {
		out = append(out, byTarget[t])
	}
	return out, nil
}

func runApprove(ctx context.Context, env Env, args []string) error {
	return decide(ctx, env, args, "approve", true)
}

func runReject(ctx context.Context, env Env, args []string) error {
	return decide(ctx, env, args, "reject", false)
}

// --- audit ------------------------------------------------------------------

func runAudit(ctx context.Context, env Env, args []string) error {
	fs, db := flags("audit", env)
	limit := fs.Int("n", 50, "how many events to show")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return withStore(ctx, env, *db, func(store *sqlite.Store) error {
		events, err := store.AuditTrail(ctx, *limit)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			fmt.Fprintln(env.Out, "nothing has happened yet")
			return nil
		}

		tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "WHEN\tWHO\tWHAT\tSUBJECT\tDETAIL")
		for _, e := range events {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s %s\t%s\n",
				e.At.Format(time.RFC3339), e.Actor, e.Action, e.SubjectKind, e.SubjectID, e.Detail)
		}
		return tw.Flush()
	})
}

func runVersion(_ context.Context, env Env, _ []string) error {
	fmt.Fprintf(env.Out, "agentd %s\n", Version)
	return nil
}
