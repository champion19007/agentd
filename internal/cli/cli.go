// Package cli is a driving adapter: the command line surface.
//
// Commands translate arguments into API / Application layer calls and format
// what comes back. No business logic or database queries live here.
//
// Architecture flow: CLI -> API/application layer -> Core
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/champion19007/agentd/internal/api"
	"github.com/champion19007/agentd/internal/config"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

// Build information, populated at link time via -ldflags.
var (
	Version = "v1.0.0"
	Commit  = "none"
	Date    = "unknown"
)

// Env is everything a command needs from outside itself. Passing it rather
// than reaching for globals is what makes commands testable.
type Env struct {
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Now     func() time.Time
	Open    func(ctx context.Context, path string) (*sqlite.Store, error)
	OpenAPI func(ctx context.Context, path string) (api.Operations, func() error, error)
	API     api.Operations
}

// DefaultEnv returns an Env wired to the real world.
func DefaultEnv() Env {
	return Env{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
		Now: func() time.Time { return time.Now().UTC() },
		Open: func(ctx context.Context, path string) (*sqlite.Store, error) {
			return sqlite.Open(ctx, sqlite.Options{Path: path})
		},
		OpenAPI: func(ctx context.Context, path string) (api.Operations, func() error, error) {
			st, err := sqlite.Open(ctx, sqlite.Options{Path: path})
			if err != nil {
				return nil, nil, err
			}
			svc := api.NewService(api.Deps{
				Store:  st,
				DBPath: path,
			})
			return svc, st.Close, nil
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

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, env Env, args []string) error
}

func commands() []command {
	return []command{
		{"init", "initialize a new agentd database and configuration", runInit},
		{"status", "display health, database integrity, checks, and incidents", runStatus},
		{"check", "manage scheduled checks (add, list, show, run, delete)", runCheck},
		{"run", "inspect check execution history and outcomes", runRun},
		{"incident", "inspect and manage breakage incidents", runIncident},
		{"repair", "decide on proposed repairs (approve, reject)", runRepair},
		{"gc", "apply retention policy and report removed records", runGC},
		{"backup", "write a consistent, verified database backup", runBackup},
		{"audit", "show the immutable audit trail", runAudit},
		{"serve", "start the local HTTP API server", runServe},
		{"mcp", "run the Agentd Model Context Protocol (MCP) server over stdio", runMCP},
		{"version", "print version information", runVersion},

		// Top-level aliases for compatibility
		{"checks", "alias for 'check list'", runChecksAlias},
		{"runs", "alias for 'run list'", runRunsAlias},
		{"incidents", "alias for 'incident list'", runIncidentsAlias},
		{"approve", "alias for 'repair approve'", runApproveAlias},
		{"reject", "alias for 'repair reject'", runRejectAlias},
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
		if strings.HasPrefix(c.summary, "alias for") {
			continue
		}
		fmt.Fprintf(tw, "    %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every command takes -db to point at a database file.")
	fmt.Fprintln(w, "Pass --json to any command for machine-readable JSON output.")
}

func baseFlags(name string, env Env) (*flag.FlagSet, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Err)
	db := fs.String("db", DefaultDBPath(), "path to the agentd database")
	asJSON := fs.Bool("json", false, "output result as JSON")
	return fs, db, asJSON
}

func parseArgs(fs *flag.FlagSet, args []string) error {
	var flagArgs []string
	var posArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") {
				clean := strings.TrimLeft(a, "-")
				fl := fs.Lookup(clean)
				if fl != nil {
					if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
						continue
					}
					if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
						i++
						flagArgs = append(flagArgs, args[i])
					}
				}
			}
		} else {
			posArgs = append(posArgs, a)
		}
	}
	return fs.Parse(append(flagArgs, posArgs...))
}

func extractSubcommand(args []string) (string, []string) {
	var sub string
	var rest []string
	found := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !found && !strings.HasPrefix(a, "-") {
			sub = a
			found = true
			continue
		}
		rest = append(rest, a)
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			clean := strings.TrimLeft(a, "-")
			if clean == "db" || clean == "addr" || clean == "to" || clean == "n" || clean == "by" || clean == "note" || clean == "locator" || clean == "runs" || clean == "incidents" || clean == "snapshots" {
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					rest = append(rest, args[i])
				}
			}
		}
	}
	return sub, rest
}

func withAPI(ctx context.Context, env Env, path string, fn func(api.Operations) error) error {
	if env.API != nil {
		return fn(env.API)
	}
	if env.OpenAPI != nil {
		ops, closeFn, err := env.OpenAPI(ctx, path)
		if err != nil {
			return err
		}
		if closeFn != nil {
			defer closeFn()
		}
		return fn(ops)
	}
	if env.Open != nil {
		st, err := env.Open(ctx, path)
		if err != nil {
			return err
		}
		defer st.Close()
		svc := api.NewService(api.Deps{
			Store:  st,
			DBPath: path,
		})
		return fn(svc)
	}
	return errors.New("no api or store opener configured in environment")
}

func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- init -------------------------------------------------------------------

func runInit(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("init", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	// For init, invoke the service directly without pre-opening the database
	svc := api.NewService(api.Deps{
		DBPath: *db,
	})
	resp, err := svc.Init(ctx, api.InitRequest{Path: *db})
	if err != nil {
		return err
	}
	if *asJSON {
		return emitJSON(env.Out, resp)
	}
	fmt.Fprintln(env.Out, resp.Message)
	return nil
}

// --- status -----------------------------------------------------------------

func runStatus(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("status", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		resp, err := ops.Status(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, resp)
		}

		healthStr := "HEALTHY"
		if !resp.Healthy || resp.OpenIncidents > 0 {
			healthStr = "ATTENTION NEEDED"
		}
		fmt.Fprintf(env.Out, "Agentd Status: %s\n", healthStr)
		fmt.Fprintf(env.Out, "Database: %s (Schema v%d, Integrity: %s)\n", resp.DBPath, resp.SchemaVersion, resp.Integrity)
		fmt.Fprintf(env.Out, "Checks: %d active (%d total)\n", resp.ChecksEnabled, resp.ChecksTotal)
		fmt.Fprintf(env.Out, "Incidents: %d open (%d awaiting approval)\n", resp.OpenIncidents, resp.AwaitingApproval)
		fmt.Fprintf(env.Out, "Recent Runs: %d\n", resp.RecentRuns)
		if resp.FreshnessSummary != "" {
			fmt.Fprintf(env.Out, "Freshness: %s\n", resp.FreshnessSummary)
		}
		if resp.MaxStalenessSeconds > 0 {
			fmt.Fprintf(env.Out, "Max Staleness: %.1fs\n", resp.MaxStalenessSeconds)
		}
		if resp.UndeliveredNotifs > 0 {
			fmt.Fprintf(env.Out, "Spool: %d undelivered notification(s)\n", resp.UndeliveredNotifs)
		}
		return nil
	})
}

// --- check ------------------------------------------------------------------

func runCheck(ctx context.Context, env Env, args []string) error {
	sub, rest := extractSubcommand(args)
	if sub == "" {
		return runCheckList(ctx, env, rest)
	}
	switch sub {
	case "add":
		return runCheckAdd(ctx, env, rest)
	case "list":
		return runCheckList(ctx, env, rest)
	case "show":
		return runCheckShow(ctx, env, rest)
	case "run":
		return runCheckRun(ctx, env, rest)
	case "export":
		return runCheckExport(ctx, env, rest)
	case "delete", "del", "rm":
		return runCheckDelete(ctx, env, rest)
	default:
		return fmt.Errorf("unknown check subcommand %q; available: add, list, show, run, export, delete", sub)
	}
}

func runCheckAdd(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("check add", env)
	file := fs.String("file", "", "path to job definition file (migrates forward if v1)")
	id := fs.String("id", "", "optional explicit check id")
	name := fs.String("name", "", "intent name/label")
	urlStr := fs.String("url", "", "source URL to monitor")
	interval := fs.String("interval", "10m", "monitoring schedule interval (e.g. 5m, 1h)")
	target := fs.String("target", "value", "target name for extracted data")
	expr := fs.String("expr", "body", "locator expression")
	dialect := fs.String("dialect", "css", "locator dialect (css, jsonpath, xpath, regex)")
	notifyDest := fs.String("notify", "notify", "destination kind ('notify' or 'none')")
	destTarget := fs.String("dest-target", "", "target webhook URL or notify channel")
	onQuiet := fs.Bool("on-quiet", false, "deliver notifications even when value is unchanged")

	if err := parseArgs(fs, args); err != nil {
		return err
	}

	if *file != "" {
		jobDef, migrated, err := config.LoadJobFile(*file)
		if err != nil {
			return fmt.Errorf("loading job file %q: %w", *file, err)
		}
		if migrated {
			fmt.Fprintln(env.Err, "notice: job file was in schema v1 and was migrated forward to schema v2")
		}
		if *id == "" {
			*id = jobDef.ID
		}
		if *name == "" {
			*name = jobDef.Name
		}
		if *urlStr == "" {
			*urlStr = jobDef.Source.URL
		}
		if jobDef.Schedule.Interval != "" {
			*interval = jobDef.Schedule.Interval
		}
		if jobDef.Binding.Expression != "" {
			*expr = jobDef.Binding.Expression
		}
		if jobDef.Binding.Dialect != "" {
			*dialect = jobDef.Binding.Dialect
		}
		if jobDef.Binding.Target != "" {
			*target = jobDef.Binding.Target
		}
		if jobDef.Destination.Kind != "" {
			*notifyDest = jobDef.Destination.Kind
		}
		if jobDef.Destination.Target != "" {
			*destTarget = jobDef.Destination.Target
		}
		if jobDef.Destination.OnQuiet {
			*onQuiet = true
		}
	}

	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required (or supply via --file)")
	}
	if strings.TrimSpace(*urlStr) == "" {
		return errors.New("--url is required (or supply via --file)")
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		sum, err := ops.AddCheck(ctx, api.AddCheckRequest{
			ID:          *id,
			Name:        *name,
			URL:         *urlStr,
			Interval:    *interval,
			Target:      *target,
			Expression:  *expr,
			Dialect:     *dialect,
			Destination: *notifyDest,
			DestTarget:  *destTarget,
			OnQuiet:     *onQuiet,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, sum)
		}
		fmt.Fprintf(env.Out, "Added check %s (%q) watching %s every %s\n", sum.ID, sum.Name, sum.URL, sum.Interval)
		return nil
	})
}

func runCheckExport(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("check export", env)
	outFile := fs.String("out", "", "write exported job to file instead of stdout")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd check export <check-id> [--out <file>]")
	}
	checkID := domain.CheckID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		detail, err := ops.GetCheck(ctx, checkID)
		if err != nil {
			return err
		}
		expr := ""
		dialect := "css"
		target := "value"
		if len(detail.Locators) > 0 {
			expr = detail.Locators[0].Expression
			dialect = detail.Locators[0].Dialect
			target = detail.Locators[0].Target
		}
		jobDef := &config.JobDefinition{
			SchemaVersion: config.CurrentJobSchemaVersion,
			ID:            detail.ID,
			Name:          detail.Name,
			Source: config.SourceConfig{
				Kind:   "http",
				URL:    detail.URL,
				Method: "GET",
			},
			Schedule: config.ScheduleConfig{
				Interval: detail.Interval,
				CatchUp:  "once",
			},
			Intent: config.IntentConfig{
				Kind: "scalar",
				Name: detail.Name,
			},
			Binding: config.BindingConfig{
				Dialect:    dialect,
				Target:     target,
				Expression: expr,
			},
		}
		if *asJSON || *outFile == "" {
			data, err := jobDef.Save()
			if err != nil {
				return err
			}
			if *outFile != "" {
				if err := os.WriteFile(*outFile, data, 0600); err != nil {
					return err
				}
				fmt.Fprintf(env.Out, "exported check %q to %s (schema v%d)\n", checkID, *outFile, config.CurrentJobSchemaVersion)
				return nil
			}
			_, err = env.Out.Write(append(data, '\n'))
			return err
		}
		return emitJSON(env.Out, jobDef)
	})
}

func runCheckList(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("check list", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		checks, err := ops.ListChecks(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, checks)
		}
		if len(checks) == 0 {
			fmt.Fprintln(env.Out, "no checks are configured")
			return nil
		}

		tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CHECK\tWATCHING\tEVERY\tLAST RUN\tSTATUS")
		for _, c := range checks {
			last := c.LastRunState
			if last == "" {
				last = "never"
			}
			stale := "fresh"
			if c.StalenessSeconds > 0 {
				stale = fmt.Sprintf("%.0fs overdue", c.StalenessSeconds)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, c.Interval, last, stale)
		}
		return tw.Flush()
	})
}

func runCheckShow(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("check show", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd check show <check-id>")
	}
	checkID := domain.CheckID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		detail, err := ops.GetCheck(ctx, checkID)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, detail)
		}

		status := "active"
		if !detail.Enabled {
			status = "disabled"
		}
		fmt.Fprintf(env.Out, "Check: %s (%s)\n", detail.ID, status)
		fmt.Fprintf(env.Out, "Intent: %s (%s)\n", detail.Name, detail.Shape)
		fmt.Fprintf(env.Out, "URL: %s\n", detail.URL)
		fmt.Fprintf(env.Out, "Schedule: every %s\n", detail.Interval)
		if detail.StalenessSeconds > 0 {
			fmt.Fprintf(env.Out, "Freshness: overdue by %.1fs\n", detail.StalenessSeconds)
		} else if detail.FreshnessSeconds > 0 {
			fmt.Fprintf(env.Out, "Freshness: last observed %.1fs ago (fresh)\n", detail.FreshnessSeconds)
		}
		fmt.Fprintf(env.Out, "Definition Version: %d\n", detail.Version)
		if len(detail.Locators) > 0 {
			fmt.Fprintf(env.Out, "Active Locators:\n")
			for _, l := range detail.Locators {
				fmt.Fprintf(env.Out, "    %s (%s): %s\n", l.Target, l.Dialect, l.Expression)
			}
		}
		if len(detail.RecentRuns) > 0 {
			fmt.Fprintf(env.Out, "Recent Runs:\n")
			for _, r := range detail.RecentRuns {
				fmt.Fprintf(env.Out, "    %s: %s (%s)\n", r.When, r.State, r.Explanation)
			}
		}
		return nil
	})
}

func runCheckRun(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("check run", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd check run <check-id>")
	}
	checkID := domain.CheckID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		sum, err := ops.RunCheck(ctx, checkID)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, sum)
		}
		fmt.Fprintf(env.Out, "Triggered check %s (slot %d):\n", sum.CheckID, sum.Slot)
		fmt.Fprintf(env.Out, "Outcome: %s\n", sum.State)
		fmt.Fprintf(env.Out, "Details: %s\n", sum.Explanation)
		return nil
	})
}

func runCheckDelete(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("check delete", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd check delete <check-id>")
	}
	checkID := domain.CheckID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		if err := ops.DeleteCheck(ctx, checkID); err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, map[string]string{"message": fmt.Sprintf("disabled check %s", checkID)})
		}
		fmt.Fprintf(env.Out, "Disabled check %s\n", checkID)
		return nil
	})
}

// --- run --------------------------------------------------------------------

func runRun(ctx context.Context, env Env, args []string) error {
	sub, rest := extractSubcommand(args)
	switch sub {
	case "list":
		return runRunList(ctx, env, rest)
	case "show":
		return runRunShow(ctx, env, rest)
	default:
		if sub != "" {
			return runRunList(ctx, env, append([]string{sub}, rest...))
		}
		return errors.New("usage: agentd run [list|show] ...")
	}
}

func runRunList(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("run list", env)
	limit := fs.Int("n", 20, "how many runs to show")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd run list <check-id>")
	}
	checkID := domain.CheckID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		runs, err := ops.ListRuns(ctx, checkID, *limit)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, runs)
		}
		if len(runs) == 0 {
			fmt.Fprintf(env.Out, "%s has never run\n", checkID)
			return nil
		}

		tw := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "WHEN\tOUTCOME\tWHAT HAPPENED")
		for _, r := range runs {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.When, r.State, r.Explanation)
		}
		return tw.Flush()
	})
}

func runRunShow(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("run show", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd run show <run-id>")
	}
	runID := domain.RunID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		detail, err := ops.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, detail)
		}

		fmt.Fprintf(env.Out, "Run: %s (check: %s, slot %d)\n", detail.ID, detail.CheckID, detail.Slot)
		if detail.TraceID != "" {
			fmt.Fprintf(env.Out, "Trace ID: %s\n", detail.TraceID)
		}
		fmt.Fprintf(env.Out, "State: %s\n", detail.State)
		fmt.Fprintf(env.Out, "Started: %s\n", detail.StartedAt)
		fmt.Fprintf(env.Out, "Ended:   %s\n", detail.EndedAt)
		if detail.PayloadHash != "" {
			fmt.Fprintf(env.Out, "Capture: %s\n", detail.PayloadHash)
		}
		if detail.FailureSummary != "" {
			fmt.Fprintf(env.Out, "Failure [%s]: %s\n", detail.FailureClass, detail.FailureSummary)
		}
		fmt.Fprintf(env.Out, "Explanation: %s\n", detail.Explanation)
		return nil
	})
}

// --- incident ---------------------------------------------------------------

func runIncident(ctx context.Context, env Env, args []string) error {
	sub, rest := extractSubcommand(args)
	if sub == "" {
		return runIncidentList(ctx, env, rest)
	}
	switch sub {
	case "list":
		return runIncidentList(ctx, env, rest)
	case "show":
		return runIncidentShow(ctx, env, rest)
	default:
		// If incident id given directly, treat as show
		return runIncidentShow(ctx, env, append([]string{sub}, rest...))
	}
}

func runIncidentList(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("incident list", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		incidents, err := ops.ListIncidents(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, incidents)
		}
		if len(incidents) == 0 {
			fmt.Fprintln(env.Out, "nothing is waiting for you")
			return nil
		}

		for _, inc := range incidents {
			fmt.Fprintf(env.Out, "%s  (%s)\n", inc.ID, inc.CheckID)
			fmt.Fprintf(env.Out, "    State: %s (opened %s, %d repair attempt(s) remaining)\n", inc.State, inc.OpenedAt, inc.AttemptsLeft)
			fmt.Fprintf(env.Out, "    Breakage: %s\n", inc.WhatBroke)
			if inc.HasProposal {
				fmt.Fprintf(env.Out, "    Status: A proposed repair is ready for review.\n")
				fmt.Fprintf(env.Out, "    Inspect: agentd incident show %s\n", inc.ID)
			}
			fmt.Fprintln(env.Out)
		}
		return nil
	})
}

func runIncidentShow(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("incident show", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd incident show <incident-id>")
	}
	incID := domain.IncidentID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		detail, err := ops.GetIncident(ctx, incID)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, detail)
		}

		fmt.Fprintf(env.Out, "Incident: %s (check: %s)\n", detail.ID, detail.CheckID)
		fmt.Fprintf(env.Out, "State:    %s\n", detail.State)
		fmt.Fprintf(env.Out, "Opened:   %s (%d attempts remaining)\n\n", detail.OpenedAt, detail.AttemptsLeft)

		fmt.Fprintf(env.Out, "WHAT BROKE:\n    %s\n\n", detail.WhatBroke)

		if detail.WhatAgentdFound != "" {
			fmt.Fprintf(env.Out, "WHAT AGENTD FOUND:\n    %s\n\n", detail.WhatAgentdFound)
		}

		if detail.WhatItProposes != "" {
			fmt.Fprintf(env.Out, "WHAT IT PROPOSES:\n    %s\n", detail.WhatItProposes)
			if len(detail.ProposedLocators) > 0 {
				fmt.Fprintf(env.Out, "    Proposed locators:\n")
				for _, l := range detail.ProposedLocators {
					fmt.Fprintf(env.Out, "        %s (%s) -> %s\n", l.Target, l.Dialect, l.Expression)
				}
			}
			if detail.Diff != "" {
				fmt.Fprintf(env.Out, "    Diff:\n        %s\n", detail.Diff)
			}
			fmt.Fprintln(env.Out)
		}

		if len(detail.VerificationResults) > 0 {
			fmt.Fprintf(env.Out, "VERIFICATION RESULTS:\n")
			for _, g := range detail.VerificationResults {
				mark := "[FAIL]"
				if g.Passed {
					mark = "[PASS]"
				}
				fmt.Fprintf(env.Out, "    %s %s: %s\n", mark, g.Gate, g.Detail)
			}
			fmt.Fprintln(env.Out)
		}

		fmt.Fprintf(env.Out, "WHAT ACTION IS REQUIRED:\n")
		for _, line := range strings.Split(detail.ActionRequired, "\n") {
			fmt.Fprintf(env.Out, "    %s\n", line)
		}
		return nil
	})
}

// --- repair -----------------------------------------------------------------

func runRepair(ctx context.Context, env Env, args []string) error {
	sub, rest := extractSubcommand(args)
	if sub == "" {
		return errors.New("usage: agentd repair [approve|reject] <incident-id> --by <your name>")
	}
	switch sub {
	case "approve":
		return runRepairApprove(ctx, env, rest)
	case "reject":
		return runRepairReject(ctx, env, rest)
	default:
		return fmt.Errorf("unknown repair subcommand %q; available: approve, reject", sub)
	}
}

func runRepairApprove(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("repair approve", env)
	by := fs.String("by", "", "operator approving the repair (required)")
	locator := fs.String("locator", "", "override locator before approving, as target=expr")
	note := fs.String("note", "", "optional rationale note")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd repair approve <incident-id> --by <your name>")
	}
	if strings.TrimSpace(*by) == "" {
		return errors.New("--by is required: an approval must be attributable to a person")
	}
	incID := domain.IncidentID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		res, err := ops.ApproveRepair(ctx, incID, api.ApproveRequest{
			By:       *by,
			Locators: *locator,
			Note:     *note,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, res)
		}
		fmt.Fprintln(env.Out, res.Message)
		return nil
	})
}

func runRepairReject(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("repair reject", env)
	by := fs.String("by", "", "operator rejecting the repair (required)")
	note := fs.String("note", "", "why the repair is being rejected")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentd repair reject <incident-id> --by <your name>")
	}
	if strings.TrimSpace(*by) == "" {
		return errors.New("--by is required: a rejection must be attributable to a person")
	}
	incID := domain.IncidentID(fs.Arg(0))

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		res, err := ops.RejectRepair(ctx, incID, api.RejectRequest{
			By:   *by,
			Note: *note,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, res)
		}
		fmt.Fprintln(env.Out, res.Message)
		return nil
	})
}

// --- gc ---------------------------------------------------------------------

func runGC(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("gc", env)
	runDays := fs.Int("runs", 0, "days of finished runs to keep")
	incidentDays := fs.Int("incidents", 0, "days to keep closed incidents")
	keep := fs.Int("snapshots", 0, "captures to keep per check")
	dry := fs.Bool("dry-run", false, "report what would be removed without removing it")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		sweep, err := ops.GC(ctx, api.GCRequest{
			RunDays:           *runDays,
			IncidentDays:      *incidentDays,
			SnapshotsPerCheck: *keep,
			DryRun:            *dry,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, sweep)
		}
		verb := "removed"
		if *dry {
			verb = "would remove"
		}
		if sweep.Empty() {
			fmt.Fprintln(env.Out, "nothing to remove")
			return nil
		}
		fmt.Fprintf(env.Out, "%s %d runs, %d captures, %d closed incidents\n",
			verb, sweep.Runs, sweep.Snapshots, sweep.Incidents)
		return nil
	})
}

// --- backup -----------------------------------------------------------------

func runBackup(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("backup", env)
	to := fs.String("to", "", "destination backup file")
	verify := fs.Bool("verify", true, "verify backup integrity after write")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		res, err := ops.Backup(ctx, api.BackupRequest{
			To:     *to,
			Verify: *verify,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, res)
		}
		fmt.Fprintf(env.Out, "wrote %s (%s)\n", res.Path, humanBytes(res.SizeBytes))
		if res.Verified {
			fmt.Fprintf(env.Out, "verified: schema version %d, integrity check passed\n", res.SchemaVersion)
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

// --- audit ------------------------------------------------------------------

func runAudit(ctx context.Context, env Env, args []string) error {
	fs, db, asJSON := baseFlags("audit", env)
	limit := fs.Int("n", 50, "how many events to show")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		events, err := ops.Audit(ctx, *limit)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(env.Out, events)
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

func runVersion(_ context.Context, env Env, args []string) error {
	fs, _, asJSON := baseFlags("version", env)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if *asJSON {
		return emitJSON(env.Out, map[string]string{
			"version":  Version,
			"commit":   Commit,
			"date":     Date,
			"go":       runtime.Version(),
			"platform": fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		})
	}
	fmt.Fprintf(env.Out, "agentd %s (%s, %s, %s/%s)\n", Version, Commit, Date, runtime.GOOS, runtime.GOARCH)
	return nil
}

// --- aliases ----------------------------------------------------------------

func runChecksAlias(ctx context.Context, env Env, args []string) error {
	return runCheckList(ctx, env, args)
}

func runRunsAlias(ctx context.Context, env Env, args []string) error {
	return runRunList(ctx, env, args)
}

func runIncidentsAlias(ctx context.Context, env Env, args []string) error {
	return runIncidentList(ctx, env, args)
}

func runApproveAlias(ctx context.Context, env Env, args []string) error {
	return runRepairApprove(ctx, env, args)
}

func runRejectAlias(ctx context.Context, env Env, args []string) error {
	return runRepairReject(ctx, env, args)
}

// --- serve ------------------------------------------------------------------

func runServe(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	db := fs.String("db", DefaultDBPath(), "path to agentd database")
	addr := fs.String("addr", api.DefaultBindAddress, "address:port to bind local HTTP server to")
	allowRemote := fs.Bool("allow-remote", false, "allow binding to remote interfaces (bypasses loopback check)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		srv, err := api.NewServer(*addr, ops, *allowRemote)
		if err != nil {
			return err
		}
		errCh := make(chan error, 1)
		go func() {
			errCh <- srv.Start()
		}()
		fmt.Fprintf(env.Out, "Agentd HTTP API listening on %s (loopback only: %v)\nWeb Dashboard available at http://%s/\n", srv.Addr(), !*allowRemote, srv.Addr())

		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	})
}

// --- mcp --------------------------------------------------------------------

func runMCP(ctx context.Context, env Env, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	db := fs.String("db", DefaultDBPath(), "path to agentd database")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	return withAPI(ctx, env, *db, func(ops api.Operations) error {
		mcpServer := api.NewMCPServer(ops)
		in := env.In
		if in == nil {
			in = os.Stdin
		}
		return mcpServer.ServeStdio(ctx, in, env.Out)
	})
}
