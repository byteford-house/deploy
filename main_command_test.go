package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"homelab-deploy/internal/manifest"
	"homelab-deploy/internal/orchestrate"
	"homelab-deploy/internal/tf"
)

func requireTerraform(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not found on PATH — skipping")
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH — skipping")
	}
}

// buildFakeRepo creates n service directories, each with a distinct
// terraform_data resource, terraform-inits them, and works around the
// known builtin-provider-only IsInitialized gap (terraform_data creates
// no .terraform/) — mirrors internal/server's own test helper of the
// same name, since both are exercising the same real orchestrate/tf
// machinery end to end, just via the CLI's command functions instead of
// the web server's job machinery.
func buildFakeRepo(t *testing.T, n int) (repoRoot string, dirs []string) {
	t.Helper()
	repoRoot = t.TempDir()
	runner, err := tf.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for i := 0; i < n; i++ {
		dir := filepath.Join("services", "svc"+string(rune('a'+i)))
		full := filepath.Join(repoRoot, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := `resource "terraform_data" "example" { input = "` + dir + `" }`
		if err := os.WriteFile(filepath.Join(full, "main.tf"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runner.Init(ctx, repoRoot, dir); err != nil {
			t.Fatalf("init %s: %v", dir, err)
		}
		if err := os.MkdirAll(filepath.Join(full, ".terraform"), 0o755); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	return repoRoot, dirs
}

// noopResolver is the same resolver a real single-repo run would build
// (manifest.NewResolver returns a working nil-manifest resolver when no
// .deploy-repos exists) — built once per test via this helper purely to
// avoid repeating the two-line construction everywhere.
func noopResolver(t *testing.T, repoRoot string) *manifest.Resolver {
	t.Helper()
	r, err := manifest.NewResolver(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCmdInitAll_InitializesEveryDiscoveredDirectory(t *testing.T) {
	requireTerraform(t)
	// A fresh (never-inited) fixture, built directly rather than via
	// buildFakeRepo — buildFakeRepo already runs Init once per directory
	// (working around the builtin-provider-only IsInitialized gap noted
	// on its doc comment: terraform_data creates no .terraform/, so
	// IsInitialized needs a manually-created marker), which would make
	// this test's own cmdInitAll call a no-op re-init rather than
	// exercising real work.
	repoRoot := t.TempDir()
	dirs := []string{filepath.Join("services", "svca"), filepath.Join("services", "svcb")}
	for _, dir := range dirs {
		full := filepath.Join(repoRoot, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := `resource "terraform_data" "example" { input = "` + dir + `" }`
		if err := os.WriteFile(filepath.Join(full, "main.tf"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resolver := noopResolver(t, repoRoot)
	// terraform_data needs no provider plugins and no backend beyond the
	// local default, so a real `terraform init` against it creates no
	// observable filesystem artifact at all (confirmed manually — no
	// .terraform/, no .terraform.lock.hcl) — the only real signal this
	// test can check is that cmdInitAll actually drives real terraform
	// init calls for both discovered directories without error, wiring
	// graph.Discover + newRunner + orchestrate.InitAll together
	// correctly end to end.
	if err := cmdInitAll(context.Background(), resolver, repoRoot); err != nil {
		t.Fatalf("cmdInitAll returned %v, want nil", err)
	}
}

func TestCmdSingleDir_PlanOnInitializedDirectorySucceeds(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 1)
	resolver := noopResolver(t, repoRoot)

	if err := cmdSingleDir(context.Background(), resolver, repoRoot, dirs[0], "plan", false, false, outputOptions{}); err != nil {
		t.Errorf("cmdSingleDir plan returned %v, want nil", err)
	}
}

func TestCmdSingleDir_PlanOnUninitializedDirectoryErrorsWithHint(t *testing.T) {
	requireTerraform(t)
	repoRoot := t.TempDir()
	dir := filepath.Join("services", "never-initialized")
	if err := os.MkdirAll(filepath.Join(repoRoot, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, dir, "main.tf"), []byte(`resource "terraform_data" "x" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver := noopResolver(t, repoRoot)

	err := cmdSingleDir(context.Background(), resolver, repoRoot, dir, "plan", false, false, outputOptions{})
	if err == nil {
		t.Fatal("expected an error for a never-initialized directory")
	}
	if !strings.Contains(err.Error(), "init") {
		t.Errorf("expected the error to mention running init, got: %v", err)
	}
}

func TestCmdFullRun_PlanWritesReportAndClearsOldLogDir(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 2)
	resolver := noopResolver(t, repoRoot)

	// A stale file from a previous run must not survive — cmdFullRun's
	// "plan" branch wipes LogDir first specifically so this can't happen.
	logDir := filepath.Join(repoRoot, orchestrate.LogDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(logDir, "stale.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdFullRun(context.Background(), resolver, repoRoot, "plan", false, false, outputOptions{}); err != nil {
		t.Fatalf("cmdFullRun plan returned %v, want nil", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("expected the stale log-dir file to be gone after a plan run")
	}
	if _, err := os.Stat(filepath.Join(logDir, "report.html")); err != nil {
		t.Errorf("expected report.html to exist after a plan run, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "report.json")); err != nil {
		t.Errorf("expected report.json to exist after a plan run, got %v", err)
	}
}

func TestCmdFullRun_ApplyRunsEveryDirectoryAndDoesNotWipeLogDirFirst(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 2)
	resolver := noopResolver(t, repoRoot)

	// A plan file placed here beforehand (as "plan" itself would leave
	// behind) must survive an "apply" run's start, unlike "plan"'s own
	// wipe — apply needs to be able to consume a previously-saved plan.
	logDir := filepath.Join(repoRoot, orchestrate.LogDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(logDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdFullRun(context.Background(), resolver, repoRoot, "apply", false, false, outputOptions{}); err != nil {
		t.Fatalf("cmdFullRun apply returned %v, want nil", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Error("apply must not wipe the log dir the way plan does")
	}

	for _, dir := range dirs {
		// terraform_data has no real infra to check against — confirming
		// the apply reported success for each directory (no error
		// returned above) is what this test is actually verifying;
		// state file presence is the closest observable proxy that
		// terraform genuinely ran apply, not just planned.
		if _, err := os.Stat(filepath.Join(repoRoot, dir, "terraform.tfstate")); err != nil {
			t.Errorf("%s: expected a state file after apply, got %v", dir, err)
		}
	}
}

func TestCmdSync_NoManifestErrors(t *testing.T) {
	repoRoot := t.TempDir()
	err := cmdSync(context.Background(), repoRoot)
	if err == nil {
		t.Fatal("expected an error when no .deploy-repos exists")
	}
	if !strings.Contains(err.Error(), "multi-repo mode") {
		t.Errorf("expected the error to explain multi-repo mode is required, got: %v", err)
	}
}

func TestCmdSync_ClonesEveryManifestEntry(t *testing.T) {
	requireGit(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitT(t, "", "init", "--quiet", "--bare", "--initial-branch=main", remote)

	work := t.TempDir()
	runGitT(t, work, "init", "--quiet", "--initial-branch=main")
	runGitT(t, work, "config", "user.email", "test@example.com")
	runGitT(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "main.tf"), []byte(`resource "terraform_data" "x" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, work, "add", ".")
	runGitT(t, work, "commit", "--quiet", "-m", "initial")
	runGitT(t, work, "remote", "add", "origin", remote)
	runGitT(t, work, "push", "--quiet", "origin", "main")

	repoRoot := t.TempDir()
	manifestContent := "vault=file://" + remote + "\n"
	if err := os.WriteFile(filepath.Join(repoRoot, ".deploy-repos"), []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdSync(context.Background(), repoRoot); err != nil {
		t.Fatalf("cmdSync returned %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, manifest.CheckoutsDirName, "vault", "main.tf")); err != nil {
		t.Fatalf("expected vault to be cloned into the checkouts subfolder, got: %v", err)
	}
}

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	fullArgs := args
	if dir != "" {
		fullArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestCmdMigrateBackendOne_RequiresDirPlaceholder(t *testing.T) {
	// The {dir}-placeholder check happens before any real terraform
	// interaction, so this needs no terraform binary and no discovered
	// directories on disk at all.
	repoRoot := t.TempDir()
	resolver := noopResolver(t, repoRoot)
	backend := tf.BackendConfig{Type: "s3", Settings: map[string]string{"bucket": "no-placeholder-here"}}

	err := cmdMigrateBackendOne(context.Background(), resolver, repoRoot, "services/svca", backend)
	if err == nil {
		t.Fatal("expected an error when the backend settings have no {dir} placeholder")
	}
	if !strings.Contains(err.Error(), "{dir}") {
		t.Errorf("expected the error to mention the {dir} placeholder, got: %v", err)
	}
}

func TestCmdMigrateBackendOne_AlreadyMigratedIsANoop(t *testing.T) {
	repoRoot := t.TempDir()
	dir := "services/already-done"
	workDir := filepath.Join(repoRoot, dir)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tf.WriteBackendStub(workDir, "s3"); err != nil {
		t.Fatal(err)
	}
	resolver := noopResolver(t, repoRoot)
	backend := tf.BackendConfig{Type: "s3", Settings: map[string]string{"key": "{dir}.tfstate"}}

	if err := cmdMigrateBackendOne(context.Background(), resolver, repoRoot, dir, backend); err != nil {
		t.Errorf("cmdMigrateBackendOne on an already-migrated directory should be a no-op, got: %v", err)
	}
}

func TestCmdMigrateBackendAll_RequiresDirPlaceholder(t *testing.T) {
	// Same as the single-directory variant: this check happens before
	// graph.Discover even runs, so no real directories or terraform
	// binary are needed.
	repoRoot := t.TempDir()
	resolver := noopResolver(t, repoRoot)
	backend := tf.BackendConfig{Type: "s3", Settings: map[string]string{"bucket": "no-placeholder-here"}}

	err := cmdMigrateBackendAll(context.Background(), resolver, repoRoot, backend)
	if err == nil {
		t.Fatal("expected an error when the backend settings have no {dir} placeholder")
	}
	if !strings.Contains(err.Error(), "{dir}") {
		t.Errorf("expected the error to mention the {dir} placeholder, got: %v", err)
	}
}

func TestCmdMigrateBackendAll_EveryDirectoryAlreadyMigratedIsANoop(t *testing.T) {
	// graph.Discover only reads directory names (services/config/infra),
	// no terraform involved — and since every directory already has a
	// backend stub, cmdMigrateBackendAll returns before ever touching a
	// runner. No terraform binary needed.
	repoRoot := t.TempDir()
	dirs := []string{"services/svca", "services/svcb"}
	for _, dir := range dirs {
		full := filepath.Join(repoRoot, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := tf.WriteBackendStub(full, "s3"); err != nil {
			t.Fatal(err)
		}
	}
	resolver := noopResolver(t, repoRoot)
	backend := tf.BackendConfig{Type: "s3", Settings: map[string]string{"key": "{dir}.tfstate"}}

	if err := cmdMigrateBackendAll(context.Background(), resolver, repoRoot, backend); err != nil {
		t.Errorf("cmdMigrateBackendAll with every directory already migrated should be a no-op, got: %v", err)
	}
}

// verboseWriter tests: pure concurrency/buffering logic, no terraform
// needed at all.

func TestVerboseWriter_BuffersUntilNewlineThenPrintsPrefixedLine(t *testing.T) {
	var out bytes.Buffer
	restore := redirectStdout(t, &out)
	w := newVerboseWriter("services/a", &sync.Mutex{})

	// Deliberately not inspecting `out` yet here — it's being written by
	// redirectStdout's background copy goroutine with no synchronization
	// until restore() joins it below, so reading it mid-flight would be
	// a genuine data race, not just an ugly test. The exact-match
	// assertion after restore() is what actually proves the partial
	// write alone produced no premature output: if it had, the final
	// text wouldn't match this exact single prefixed line.
	w.Write([]byte("partial "))
	w.Write([]byte("line\n"))
	restore()

	if got := out.String(); got != "[services/a] partial line\n" {
		t.Errorf("got %q, want %q", got, "[services/a] partial line\n")
	}
}

func TestVerboseWriter_MultipleLinesInOneWriteEachGetPrefixed(t *testing.T) {
	var out bytes.Buffer
	restore := redirectStdout(t, &out)
	w := newVerboseWriter("services/a", &sync.Mutex{})
	w.Write([]byte("line one\nline two\n"))
	restore()

	want := "[services/a] line one\n[services/a] line two\n"
	if got := out.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestVerboseWriter_TrailingPartialLineWithNoFinalNewlineIsDropped(t *testing.T) {
	var out bytes.Buffer
	restore := redirectStdout(t, &out)
	w := newVerboseWriter("services/a", &sync.Mutex{})
	w.Write([]byte("complete\nincomplete"))
	restore()

	want := "[services/a] complete\n"
	if got := out.String(); got != want {
		t.Errorf("got %q, want %q (trailing partial line is dropped, not flushed)", got, want)
	}
}

// TestVerboseWriter_ConcurrentWritersShareOneMutexWithoutCorruption is a
// regression-style test for the documented reason verboseWriter takes an
// external *sync.Mutex rather than owning its own: tfexec reads a
// command's stdout and stderr on separate goroutines, both calling Write
// on the same *verboseWriter for a given directory. Run with -race to
// actually catch a broken lock, not just a wrong line count.
func TestVerboseWriter_ConcurrentWritersShareOneMutexWithoutCorruption(t *testing.T) {
	var out bytes.Buffer
	restore := redirectStdout(t, &out)
	var mu sync.Mutex
	w := newVerboseWriter("services/a", &mu)

	const n = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			w.Write([]byte("stdout-line\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			w.Write([]byte("stderr-line\n"))
		}
	}()
	wg.Wait()
	restore()

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2*n {
		t.Fatalf("got %d lines, want %d — some writes were lost or lines got merged", len(lines), 2*n)
	}
	for _, line := range lines {
		if line != "[services/a] stdout-line" && line != "[services/a] stderr-line" {
			t.Fatalf("corrupted/interleaved line: %q", line)
		}
	}
}

// TestVerboseWriter_SharedMutexPreventsInterleavingAcrossDirectories
// mirrors cmdFullRun's actual usage during a concurrent "plan": every
// directory's verboseWriter shares the SAME mutex specifically so their
// output can't interleave mid-line on one shared stdout.
func TestVerboseWriter_SharedMutexPreventsInterleavingAcrossDirectories(t *testing.T) {
	var out bytes.Buffer
	restore := redirectStdout(t, &out)
	var sharedMu sync.Mutex
	wa := newVerboseWriter("services/a", &sharedMu)
	wb := newVerboseWriter("services/b", &sharedMu)

	const n = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			wa.Write([]byte("from-a\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			wb.Write([]byte("from-b\n"))
		}
	}()
	wg.Wait()
	restore()

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2*n {
		t.Fatalf("got %d lines, want %d", len(lines), 2*n)
	}
	for _, line := range lines {
		if line != "[services/a] from-a" && line != "[services/b] from-b" {
			t.Fatalf("corrupted/interleaved line across directories: %q", line)
		}
	}
}

func TestCmdSingleDir_NonInteractiveApplyRefusesUnsatisfiedManualStep(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 1)
	resolver := noopResolver(t, repoRoot)
	if err := os.WriteFile(filepath.Join(repoRoot, dirs[0], ".manual-before"), []byte("do the thing by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := cmdSingleDir(context.Background(), resolver, repoRoot, dirs[0], "apply", false, true, outputOptions{})
	if err == nil {
		t.Fatal("expected an error: manual step is not satisfied and --non-interactive must not step past it")
	}
	if !strings.Contains(err.Error(), "manual step") {
		t.Errorf("error %q should mention the manual step", err)
	}
}

func TestCmdSingleDir_InitRunsInitForOneDirectory(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 1)
	resolver := noopResolver(t, repoRoot)

	if err := cmdSingleDir(context.Background(), resolver, repoRoot, dirs[0], "init", false, true, outputOptions{}); err != nil {
		t.Fatalf("cmdSingleDir init returned %v, want nil", err)
	}
}
