// Command homelab-deploy is the Go rewrite of deploy.sh: dependency-
// ordered terraform init/plan/apply across services/, config/, infra/,
// with structured (not grepped) plan summaries and an HTML+JSON report.
//
// The CLI is a thin layer here — all the actual plan/apply/init
// orchestration lives in internal/orchestrate, shared with the `serve`
// web server (internal/server) so the two never implement "how to run a
// dependency graph" twice.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"homelab-deploy/internal/graph"
	"homelab-deploy/internal/manifest"
	"homelab-deploy/internal/orchestrate"
	"homelab-deploy/internal/report"
	"homelab-deploy/internal/server"
	"homelab-deploy/internal/tf"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// globalFlags may appear anywhere on the command line — before or after
// the subcommand and its own arguments — since pflag parses flags and
// positional arguments in an interspersed fashion (`homelab-deploy plan
// --verbose` and `homelab-deploy --verbose plan` both work).
type globalFlags struct {
	workingDir     string
	envFile        string
	sharedEnvFile  string
	verbose        bool
	timeout        time.Duration
	migrateBackend bool
	nonInteractive bool
	insecureNoAuth bool
}

func parseFlags(args []string) (globalFlags, []string, error) {
	fs := pflag.NewFlagSet("homelab-deploy", pflag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usageText)
		fs.PrintDefaults()
	}

	var gf globalFlags
	fs.StringVarP(&gf.workingDir, "working-dir", "C", "", "root directory to operate in (services/, config/, infra/ live under it) — default: current directory")
	fs.StringVar(&gf.envFile, "env-file", "", "path to the secrets env file to load — default: <working-dir>/.env")
	fs.StringVar(&gf.sharedEnvFile, "shared-env-file", "", "path to the committed, non-secret env file to load — default: <working-dir>/.env.shared")
	fs.BoolVarP(&gf.verbose, "verbose", "v", false, "stream live terraform output during plan/apply instead of only a one-line summary per directory")
	fs.DurationVar(&gf.timeout, "timeout", 0, "maximum time a plan/apply/init run may take, e.g. 10m (0 = no timeout); not used by serve, which runs indefinitely by design")
	fs.BoolVar(&gf.migrateBackend, "migrate-backend", false, "with init: migrate to the [backend] configured in the shared/secrets env file, after an explicit confirmation")

	fs.BoolVar(&gf.nonInteractive, "non-interactive", false, "never prompt (for CI): apply refuses to continue past an unsatisfied .manual-before and skips directories whose backend cannot save a plan, instead of waiting on stdin")

	fs.BoolVar(&gf.insecureNoAuth, "insecure-no-auth", false, "serve only: run the web dashboard with NO authentication, so anyone who can reach the port can plan and apply (otherwise serve refuses to start without the HOMELAB_DEPLOY_OIDC_* settings, see README)")

	if err := fs.Parse(args); err != nil {
		return globalFlags{}, nil, err
	}
	return gf, fs.Args(), nil
}

const usageText = `usage: homelab-deploy [flags] <command> [command args]

commands:
  plan                          plan every directory (concurrent, read-only)
  apply                         apply every directory (sequential, dependency order)
  <dir> [plan|apply]            target one directory directly (default: plan)
  init                          terraform init every directory
  sync                          clone/update every git-managed repo in .deploy-repos (multi-repo mode)
  init --migrate-backend        migrate every not-yet-migrated directory to the configured [backend]
  <dir> init                    terraform init just one directory
  <dir> init --migrate-backend  migrate just one directory
  graph [text|dot]              print the dependency graph
  open                          open the last plan's HTML report in a browser
  serve [addr]                  start the live web dashboard (default addr :8080); needs auth settings, see README

flags (may appear before or after the command):`

// interruptContext returns a context that is cancelled by the first SIGINT or
// SIGTERM. It then restores the default signal behaviour, so a second signal
// terminates the process immediately (the escape hatch when terraform is slow
// to stop; terraform-exec itself force-kills it after its wait delay).
// Call the returned func when done to release the signal handler.
func interruptContext(parent context.Context, w io.Writer) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			fmt.Fprintln(w, "interrupt received: asking terraform to stop and save state; press Ctrl-C again to force quit")
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigs)
	}()
	return ctx, func() {
		cancel()
		signal.Stop(sigs)
	}
}

// classifyDirArg decides how remaining[0] should be treated once it's
// matched none of the known subcommands in run's switch: info/statErr
// are the result of os.Stat'ing it as a directory under repoRoot. Nil
// means it's a genuine <dir> argument and the caller should proceed
// accordingly. A name containing a path separator was clearly intended
// as a directory, so its stat error is surfaced directly rather than
// masked behind the same generic "unknown command" text a typo'd
// subcommand gets — the two used to be indistinguishable, which meant
// a typo'd command name silently got the same wall of usage text as a
// typo'd directory name.
func classifyDirArg(arg string, info os.FileInfo, statErr error) error {
	if statErr == nil && info.IsDir() {
		return nil
	}
	if statErr == nil {
		return fmt.Errorf("%q is a file, not a directory", arg)
	}
	if strings.ContainsAny(arg, `/\`) {
		return fmt.Errorf("no such directory %q: %w", arg, statErr)
	}
	return fmt.Errorf("unknown command %q\n\n%s", arg, usageText)
}

func run(args []string) error {
	gf, remaining, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil // -h/--help already printed usage to stdout/stderr
		}
		return err
	}

	// Chdir once, up front, rather than threading a separate "root
	// directory" override through every function that currently assumes
	// repoRoot == the process's actual working directory (e.g. `open`'s
	// report path, the <dir> existence check below). This keeps that
	// assumption true for the rest of the process's lifetime instead of
	// auditing every place it's made — including future code that might
	// make it without realizing --working-dir exists.
	if gf.workingDir != "" {
		if err := os.Chdir(gf.workingDir); err != nil {
			return fmt.Errorf("--working-dir %s: %w", gf.workingDir, err)
		}
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}

	// Load the committed, non-secret file first, then the gitignored
	// secrets file — a value present in both is a local override of the
	// shared default, and [backend] settings merge the same way (the
	// secrets file can add or override one setting without restating
	// the whole section).
	sharedPath := gf.sharedEnvFile
	if sharedPath == "" {
		sharedPath = filepath.Join(repoRoot, tf.SharedEnvFileName)
	}
	_, backend, err := tf.LoadEnvFileAt(sharedPath)
	if err != nil {
		return fmt.Errorf("loading %s: %w", sharedPath, err)
	}

	envPath := gf.envFile
	if envPath == "" {
		envPath = filepath.Join(repoRoot, tf.EnvFileName)
	}
	found, secretBackend, err := tf.LoadEnvFileAt(envPath)
	if err != nil {
		return fmt.Errorf("loading %s: %w", envPath, err)
	}
	backend.Merge(secretBackend)
	cliBackend = backend
	if !found {
		w := bufio.NewWriter(os.Stderr)
		tf.WarnMissingEnv(w, envPath)
	}

	// Constructed once, threaded through every command below — nil (its
	// zero value's methods fall back to single-repo behavior) unless a
	// .deploy-repos manifest exists at repoRoot. See internal/manifest.
	resolver, err := manifest.NewResolver(repoRoot)
	if err != nil {
		return fmt.Errorf("loading %s: %w", manifest.FileName, err)
	}

	if len(remaining) == 0 {
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(1)
	}

	// Ctrl-C / SIGTERM cancel ctx instead of killing this process: terraform
	// runs in its own process group (terraform-exec sets Setpgid), so the
	// terminal's SIGINT never reaches it. A process that just dies takes
	// terraform down with SIGKILL (Pdeathsig), which leaves the state lock
	// behind and loses what an apply had already created. A cancelled ctx
	// makes terraform-exec send terraform SIGINT, so it saves state and
	// unlocks before exiting.
	ctx, stopInterrupt := interruptContext(context.Background(), os.Stderr)
	defer stopInterrupt()
	if gf.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, gf.timeout)
		defer cancel()
	}

	switch remaining[0] {
	case "init":
		if gf.migrateBackend {
			if backend.Type == "" {
				return fmt.Errorf("--migrate-backend needs a [backend] section in %s or %s", sharedPath, envPath)
			}
			return cmdMigrateBackendAll(ctx, resolver, repoRoot, backend)
		}
		return cmdInitAll(ctx, resolver, repoRoot)
	case "sync":
		return cmdSync(ctx, repoRoot)
	case "plan", "apply":
		return cmdFullRun(ctx, resolver, repoRoot, remaining[0], gf.verbose, gf.nonInteractive)
	case "graph":
		format := "text"
		if len(remaining) > 1 {
			format = remaining[1]
		}
		return cmdGraph(resolver, repoRoot, format)
	case "open":
		return report.Open(filepath.Join(repoRoot, orchestrate.LogDir, "report.html"))
	case "serve":
		addr := ":8080"
		if len(remaining) > 1 {
			addr = remaining[1]
		}
		auth, err := server.AuthFromEnv(context.Background(), os.Getenv, gf.insecureNoAuth)
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return server.Serve(repoRoot, addr, auth, backend)
	default:
		info, statErr := os.Stat(filepath.Join(repoRoot, remaining[0]))
		if err := classifyDirArg(remaining[0], info, statErr); err != nil {
			return err
		}
		cmd := "plan"
		if len(remaining) > 1 {
			cmd = remaining[1]
		}
		if cmd == "init" && gf.migrateBackend {
			if backend.Type == "" {
				return fmt.Errorf("--migrate-backend needs a [backend] section in %s or %s", sharedPath, envPath)
			}
			return cmdMigrateBackendOne(ctx, resolver, repoRoot, remaining[0], backend)
		}
		return cmdSingleDir(ctx, resolver, repoRoot, remaining[0], cmd, gf.verbose, gf.nonInteractive)
	}
}

// newRunner builds a *tf.Runner and wires resolver into it (a no-op in
// single-repo mode, where resolver's methods just fall back to
// filepath.Join(repoRoot, dir)) — every command below goes through this
// instead of calling tf.NewRunner directly, so none of them can forget
// the SetResolver step.
func newRunner(resolver *manifest.Resolver) (*tf.Runner, error) {
	runner, err := tf.NewRunner()
	if err != nil {
		return nil, err
	}
	runner.SetResolver(resolver)
	runner.SetBackend(cliBackend)
	return runner, nil
}

// cliBackend is the merged [backend] section from the env files, set once
// by run() before any command constructs a Runner. Runners use it so that
// `init` can pass -backend-config to already-migrated directories (see
// tf.Runner.Init). Zero value (the default, and what tests see) means
// "pass nothing", which is the previous behavior. The web server builds
// its own Runner; run() hands it the same settings (see the "serve" case).
var cliBackend tf.BackendConfig

// cmdSync clones any not-yet-checked-out git entry in .deploy-repos and
// fast-forward-updates any that's already checked out — see
// internal/manifest.Sync for the actual mechanism (never discards
// anything: a dirty tree or a diverged local branch both come back as
// per-entry errors instead). Only meaningful in multi-repo mode.
func cmdSync(ctx context.Context, repoRoot string) error {
	if !manifest.Exists(repoRoot) {
		return fmt.Errorf("no %s found at %s — sync only applies in multi-repo mode", manifest.FileName, repoRoot)
	}
	m, err := manifest.Load(repoRoot)
	if err != nil {
		return err
	}
	if m == nil || len(m.Order) == 0 {
		fmt.Println("No entries in", manifest.FileName)
		return nil
	}

	var syncMu sync.Mutex
	results := manifest.Sync(ctx, m, func(name string) io.Writer {
		return newVerboseWriter(name, &syncMu)
	})
	var failed int
	for _, r := range results {
		switch {
		case r.Err != nil:
			fmt.Printf("[ERROR] %-30s %v\n", r.Name, r.Err)
			failed++
		default:
			fmt.Printf("[%s] %s\n", r.Action, r.Name)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d repos failed to sync — see above", failed, len(results))
	}
	return nil
}

func cmdInitAll(ctx context.Context, resolver *manifest.Resolver, repoRoot string) error {
	g, err := graph.Discover(repoRoot)
	if err != nil {
		return err
	}
	runner, err := newRunner(resolver)
	if err != nil {
		return err
	}
	fmt.Println("Initializing all directories...")
	dirs := make([]string, 0, len(g.Nodes))
	for dir := range g.Nodes {
		dirs = append(dirs, dir)
	}
	err = orchestrate.InitAll(ctx, runner, repoRoot, dirs, func(dir string, initErr error) {
		fmt.Printf("==> terraform init in %s\n", dir)
		if initErr != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] init failed in %s: %v\n", dir, initErr)
		}
	})
	if err != nil {
		return err
	}
	fmt.Println("All directories initialized.")
	return nil
}

func cmdGraph(resolver *manifest.Resolver, repoRoot, format string) error {
	g, err := graph.Discover(repoRoot)
	if err != nil {
		return err
	}
	if err := g.Sort(); err != nil {
		return err
	}
	switch format {
	case "dot":
		fmt.Print(graph.RenderDot(g, resolver, repoRoot))
	default:
		fmt.Print(graph.RenderText(g, resolver, repoRoot))
	}
	return nil
}

// confirmUnsavedPlanCLI returns a tf.UnsavedPlanConfirm that shows the
// given diff and blocks on stdin for an explicit "yes" — the CLI's
// interactive counterpart to orchestrate.DefaultUnsavedPlanHandler's
// safe default (always skip), usable here because a terminal actually
// has someone there to ask, unlike the web server.
func confirmUnsavedPlanCLI(stdin *bufio.Reader) tf.UnsavedPlanConfirm {
	return func(dir, planText string) bool {
		fmt.Printf("\n%s's backend can't save a plan for review — here's what applying it right now would do:\n\n", dir)
		fmt.Println(planText)
		fmt.Printf("Apply these changes to %s? Type \"yes\" to confirm: ", dir)
		line, _ := stdin.ReadString('\n')
		return strings.TrimSpace(line) == "yes"
	}
}

func cmdSingleDir(ctx context.Context, resolver *manifest.Resolver, repoRoot, dir, cmd string, verbose, nonInteractive bool) error {
	runner, err := newRunner(resolver)
	if err != nil {
		return err
	}

	if cmd == "init" {
		fmt.Printf("==> terraform init in %s\n", dir)
		return runner.Init(ctx, repoRoot, dir)
	}

	if cmd == "apply" {
		ms, err := graph.Evaluate(resolver, repoRoot, dir)
		if err != nil {
			return err
		}
		if nonInteractive {
			// Same rule the web server uses: proceed only if there is
			// no manual step or its .manual-check already passes. Never
			// read stdin — in CI it is closed, and PromptIfNeeded would
			// return at once and silently step past the manual step.
			if !orchestrate.DefaultManualStepHandler(dir, ms) {
				return fmt.Errorf("%s needs a manual step that is not satisfied (see .manual-before); refusing to continue with --non-interactive", dir)
			}
		} else {
			graph.PromptIfNeeded(ms, bufio.NewReader(os.Stdin))
		}
	}

	if !runner.IsInitialized(repoRoot, dir) {
		if hint := resolver.NotSyncedHint(repoRoot, dir); hint != "" {
			return errors.New(hint)
		}
		return fmt.Errorf("%s has not been terraform-initialized yet — run: homelab-deploy init", dir)
	}

	fmt.Printf("==> terraform %s in %s\n", cmd, dir)
	planFile := orchestrate.PlanFilePath(repoRoot, dir)

	var live io.Writer
	if verbose {
		live = newVerboseWriter(dir, &sync.Mutex{})
	}

	if cmd == "apply" {
		// A nil confirm means "skip": the safe answer when nobody can be asked.
		var confirm tf.UnsavedPlanConfirm
		if !nonInteractive {
			confirm = confirmUnsavedPlanCLI(bufio.NewReader(os.Stdin))
		}
		if live != nil {
			return runner.Apply(ctx, repoRoot, dir, planFile, confirm, live)
		}
		return runner.Apply(ctx, repoRoot, dir, planFile, confirm)
	}

	var res tf.PlanResult
	if live != nil {
		res = runner.Plan(ctx, repoRoot, dir, planFile, live)
	} else {
		res = runner.Plan(ctx, repoRoot, dir, planFile)
	}
	if !verbose {
		// Already streamed live above otherwise — printing it again
		// here would just duplicate the same output.
		fmt.Print(res.RawOutput)
	}
	return res.Err
}

func cmdFullRun(ctx context.Context, resolver *manifest.Resolver, repoRoot, cmd string, verbose, nonInteractive bool) error {
	if cmd == "plan" {
		// Wipe old logs/reports/plan-files so this run starts clean.
		// Deliberately NOT done for "apply": a prior "plan" run's saved
		// plan files live here too now, and apply needs to consume them
		// (see orchestrate.PlanFilePath) — wiping this directory first
		// would delete them before Apply ever gets to use them.
		if err := os.RemoveAll(filepath.Join(repoRoot, orchestrate.LogDir)); err != nil {
			return fmt.Errorf("clearing old %s: %w", orchestrate.LogDir, err)
		}
	}

	g, err := graph.Discover(repoRoot)
	if err != nil {
		return err
	}
	if err := g.Sort(); err != nil {
		return err
	}
	runner, err := newRunner(resolver)
	if err != nil {
		return err
	}

	rep := report.Report{GeneratedAt: time.Now(), Command: cmd, Graph: graph.Layout(g, resolver, repoRoot)}

	// One shared mutex across every directory's verboseWriter, since
	// plan runs directories concurrently — without it, interleaved
	// terraform output from different directories could garble mid-line
	// on a shared stdout.
	var verboseMu sync.Mutex
	liveWriter := func(dir string) io.Writer {
		if !verbose {
			return nil
		}
		return newVerboseWriter(dir, &verboseMu)
	}

	if cmd == "apply" {
		stdin := bufio.NewReader(os.Stdin)
		hooks := orchestrate.Hooks{
			LiveWriter: liveWriter,
			OnDirDone: func(dir string, entry report.Entry) {
				fmt.Printf("==> terraform apply in %s\n", dir)
			},
			HandleManualStep: func(dir string, ms graph.ManualStep) bool {
				graph.PromptIfNeeded(ms, stdin) // blocks on Enter if a pause is needed
				return true                     // CLI always proceeds once the person confirms
			},
			HandleUnsavedPlanApply: confirmUnsavedPlanCLI(stdin),
			Resolver:               resolver,
		}
		if nonInteractive {
			// nil handlers fall back to orchestrate's defaults: stop at an
			// unsatisfied manual step, skip a directory that needs a live
			// confirmation. Neither ever reads stdin.
			hooks.HandleManualStep = nil
			hooks.HandleUnsavedPlanApply = nil
		}
		entries, applyErr := orchestrate.ApplySequential(ctx, runner, repoRoot, g.Order, hooks)
		rep.Entries = entries
		if applyErr != nil {
			printSummary(rep.Entries)
			writeReports(repoRoot, rep) // best-effort, even on failure
			return fmt.Errorf("apply failed — stopping: %w", applyErr)
		}
	} else {
		// Unlike apply, plan is read-only — it never mutates real
		// infrastructure, so directories don't need to wait on each
		// other the way apply waits on the dependency graph. See
		// orchestrate.PlanAll's doc comment.
		hooks := orchestrate.Hooks{
			LiveWriter: liveWriter,
			OnDirDone:  func(dir string, entry report.Entry) { printPlanLine(entry) },
		}
		rep.Entries = orchestrate.PlanAll(ctx, runner, repoRoot, g.Order, hooks)
	}

	printSummary(rep.Entries)

	if cmd == "plan" {
		return writeReports(repoRoot, rep)
	}
	return nil
}

// verboseWriter prefixes each complete line of terraform output with
// its directory name and prints it to stdout as it arrives, guarded by
// a shared mutex so concurrent directories (plan runs several at once)
// can't interleave mid-line on the same stdout. Partial lines are
// buffered until a newline arrives; any trailing partial content left
// when a command finishes is dropped rather than flushed — terraform's
// own output reliably ends in a newline in practice, so this is a
// minor, accepted imperfection for what's a nice-to-have flag rather
// than something worth a Close()/flush contract for.
type verboseWriter struct {
	dir string
	mu  *sync.Mutex
	buf []byte
}

func newVerboseWriter(dir string, mu *sync.Mutex) *verboseWriter {
	return &verboseWriter{dir: dir, mu: mu}
}

func (w *verboseWriter) Write(p []byte) (int, error) {
	// The whole body is locked, not just the printing — tfexec reads a
	// command's stdout and stderr on separate goroutines, both of which
	// call this same writer's Write for a given directory, so the
	// buffer append itself needs the same protection as the print, not
	// just the print.
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		fmt.Printf("[%s] %s\n", w.dir, line)
	}
	return len(p), nil
}

func printPlanLine(e report.Entry) {
	switch e.Status {
	case report.StatusOK:
		fmt.Printf("[no changes] %s\n", e.Dir)
	case report.StatusChanges:
		fmt.Printf("[CHANGES]    %s — %s\n", e.Dir, e.Detail)
	case report.StatusError:
		fmt.Printf("[ERROR]      %s — %s\n", e.Dir, e.Detail)
	}
}

func printSummary(entries []report.Entry) {
	fmt.Println()
	fmt.Println("==================== summary ====================")
	var anyChanges, anyError bool
	for _, e := range entries {
		switch e.Status {
		case report.StatusChanges:
			fmt.Printf("[CHANGES] %s — %s\n", e.Dir, e.Detail)
			anyChanges = true
		case report.StatusError:
			fmt.Printf("[ERROR]   %s — %s\n", e.Dir, e.Detail)
			anyError = true
		}
	}
	if !anyChanges && !anyError {
		fmt.Println("Nothing changed, nothing failed.")
	}
	fmt.Println("===================================================")
}

func writeReports(repoRoot string, rep report.Report) error {
	dir := filepath.Join(repoRoot, orchestrate.LogDir)
	htmlPath, err := report.WriteHTML(dir, rep)
	if err != nil {
		return err
	}
	if _, err := report.WriteJSON(dir, rep); err != nil {
		return err
	}
	fmt.Println("HTML report:", htmlPath)
	return nil
}

// cmdMigrateBackendAll migrates every directory that doesn't already
// have a backend stub (HasBackendStub) — already-migrated directories
// are silently skipped, which is what makes re-running this safe after
// adding new directories later. One consolidated preview and one
// confirmation cover the whole batch; migration itself is sequential
// and stops at the first failure, same as apply, so a partial failure
// leaves a clearly-defined "these are done, this one failed, the rest
// weren't touched" state rather than several failing at once.
func cmdMigrateBackendAll(ctx context.Context, resolver *manifest.Resolver, repoRoot string, backend tf.BackendConfig) error {
	if !tf.HasDirPlaceholder(backend.Settings) {
		return fmt.Errorf(`[backend] settings need "{dir}" in whichever setting represents the state's location (e.g. key={dir}.tfstate) — without it every directory would collide on the same state`)
	}

	g, err := graph.Discover(repoRoot)
	if err != nil {
		return err
	}
	if err := g.Sort(); err != nil {
		return err
	}

	var toMigrate []string
	for _, dir := range g.Order {
		if !tf.HasBackendStub(resolver.WorkDir(repoRoot, dir)) {
			toMigrate = append(toMigrate, dir)
		}
	}
	if len(toMigrate) == 0 {
		fmt.Println("Every directory already has a backend stub — nothing to migrate.")
		return nil
	}

	confirmed, err := confirmMigration(os.Stdout, os.Stdin, backend, toMigrate)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Println("Aborted — nothing changed.")
		return nil
	}

	runner, err := newRunner(resolver)
	if err != nil {
		return err
	}
	for _, dir := range toMigrate {
		if err := migrateOneDir(ctx, runner, resolver, repoRoot, dir, backend); err != nil {
			return fmt.Errorf("migrating %s: %w", dir, err)
		}
	}
	fmt.Printf("All %d directories migrated.\n", len(toMigrate))
	return nil
}

// cmdMigrateBackendOne is the single-directory version — the same
// mechanism as cmdMigrateBackendAll applied to one directory, useful for
// trying the process on one low-stakes directory before doing everything
// else.
func cmdMigrateBackendOne(ctx context.Context, resolver *manifest.Resolver, repoRoot, dir string, backend tf.BackendConfig) error {
	if !tf.HasDirPlaceholder(backend.Settings) {
		return fmt.Errorf(`[backend] settings need "{dir}" in whichever setting represents the state's location (e.g. key={dir}.tfstate) — without it every directory would collide on the same state`)
	}
	if tf.HasBackendStub(resolver.WorkDir(repoRoot, dir)) {
		fmt.Printf("%s already has a backend stub — nothing to do.\n", dir)
		return nil
	}

	confirmed, err := confirmMigration(os.Stdout, os.Stdin, backend, []string{dir})
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Println("Aborted — nothing changed.")
		return nil
	}

	runner, err := newRunner(resolver)
	if err != nil {
		return err
	}
	return migrateOneDir(ctx, runner, resolver, repoRoot, dir, backend)
}

// migrateOneDir writes the backend stub then runs the actual migration.
// If the migration itself fails, the stub is rolled back — Terraform
// needs the stub to exist before init can even attempt the migration,
// but if that migration doesn't actually succeed, leaving the stub in
// place would make a retry think this directory was already handled
// (HasBackendStub is what idempotency is based on) when it wasn't.
func migrateOneDir(ctx context.Context, runner *tf.Runner, resolver *manifest.Resolver, repoRoot, dir string, backend tf.BackendConfig) error {
	workDir := resolver.WorkDir(repoRoot, dir)
	fmt.Printf("==> migrating %s to %s backend\n", dir, backend.Type)
	if err := tf.WriteBackendStub(workDir, backend.Type); err != nil {
		return fmt.Errorf("writing backend stub: %w", err)
	}
	if err := runner.MigrateBackend(ctx, repoRoot, dir, backend); err != nil {
		if rmErr := tf.RemoveBackendStub(workDir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warning: also failed to roll back the backend stub for %s: %v\n", dir, rmErr)
		}
		return err
	}
	return nil
}

// confirmMigration prints exactly what's about to happen — backend
// type and every directory's fully resolved settings (with {dir}
// substituted, so you see the actual value, not the template) — then
// requires the person to type the literal word "yes" (not Enter, not
// "y") to proceed. terraform-exec always passes -input=false to
// `terraform init` (it's hardcoded, not something any option can turn
// off), so Terraform's own interactive "do you want to copy state?"
// prompt can never actually appear when we're driving it — this
// confirmation is the only one that will ever happen, so it has to be a
// real one, not a rubber stamp.
func confirmMigration(w io.Writer, r io.Reader, backend tf.BackendConfig, dirs []string) (bool, error) {
	fmt.Fprintln(w, "This will migrate the following to a remote backend, copying each")
	fmt.Fprintln(w, "directory's EXISTING state into it:")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  backend type: %s\n", backend.Type)
	for _, dir := range dirs {
		fmt.Fprintf(w, "\n  %s:\n", dir)
		resolved := tf.ResolveSettings(backend.Settings, dir)
		for _, k := range sortedKeys(resolved) {
			fmt.Fprintf(w, "    %s = %s\n", k, resolved[k])
		}
	}
	fmt.Fprintln(w)
	fmt.Fprint(w, `Type "yes" to proceed: `)

	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false, nil // e.g. stdin closed/EOF — treat as "no", not an error
	}
	return strings.TrimSpace(line) == "yes", nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
