package tf

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBlockingTerraform puts a `terraform` shell script on PATH that
// answers terraform-exec's `version -json` probe, then for any other
// subcommand records that it started and blocks until it receives SIGINT
// (what a real terraform treats as "stop gracefully": finish in-flight
// work, save state, release the state lock). On SIGINT it writes
// "interrupted" to the returned marker file and exits. A SIGKILL leaves
// the marker absent, which is exactly the failure being pinned.
//
// The paths are baked into the script text rather than passed through the
// environment because terraform-exec rebuilds the child's environment.
func fakeBlockingTerraform(t *testing.T) (started, interrupted string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake terraform needs a POSIX shell")
	}
	bin := t.TempDir()
	state := t.TempDir()
	started = filepath.Join(state, "started")
	interrupted = filepath.Join(state, "interrupted")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  echo '{"terraform_version":"1.9.8","platform":"linux_amd64","provider_selections":{},"terraform_outdated":false}'
  exit 0
fi
trap 'echo interrupted > '` + shQuote(interrupted) + `'; exit 130' INT
echo started > ` + shQuote(started) + `
while true; do sleep 0.1; done
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return started, interrupted
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func waitForFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %s", path, d)
}

// Cancelling the context of a running apply must interrupt terraform
// (SIGINT) rather than kill it outright, so terraform can write the state
// for what it already created and release its state lock. A SIGKILL leaves
// the lock behind and loses the in-flight resources from state.
func TestApply_CancelInterruptsTerraformInsteadOfKillingIt(t *testing.T) {
	started, interrupted := fakeBlockingTerraform(t)

	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An existing saved plan makes Apply run `terraform apply <planfile>`
	// directly, the command whose cancellation matters.
	planFile := filepath.Join(repoRoot, ".deploy-logs", "svc.tfplan")
	if err := os.MkdirAll(filepath.Dir(planFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planFile, []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := NewRunner()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Apply(ctx, repoRoot, "svc", planFile, nil) }()

	waitForFile(t, started, 10*time.Second)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Apply returned nil after its context was cancelled")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Apply did not return after cancel")
	}

	got, err := os.ReadFile(interrupted)
	if err != nil {
		t.Fatalf("terraform was not sent SIGINT on cancel (it was killed instead): %v", err)
	}
	if strings.TrimSpace(string(got)) != "interrupted" {
		t.Errorf("unexpected marker content %q", got)
	}
}

// lineWatcher signals once the live output contains want.
type lineWatcher struct {
	mu   sync.Mutex
	buf  strings.Builder
	want string
	hit  chan struct{}
	done bool
}

func (w *lineWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if !w.done && strings.Contains(w.buf.String(), w.want) {
		w.done = true
		close(w.hit)
	}
	return len(p), nil
}

// Against real terraform: cancelling an apply that is part-way through
// creating a resource must leave no state lock behind and must record the
// resource in the state file (it is tainted, because its provisioner was
// interrupted). Killing terraform instead leaves the lock info file and a
// state without the resource. Uses only terraform_data and local-exec, so
// no provider download is needed.
func TestApply_CancelReleasesStateLockAndSavesStateWithRealTerraform(t *testing.T) {
	requireTerraform(t)
	repoRoot := t.TempDir()
	dir := "svc"
	writeTFConfig(t, repoRoot, dir, `resource "terraform_data" "slow" {
  provisioner "local-exec" {
    command = "sleep 30"
  }
}
`)
	r, err := NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Init(context.Background(), repoRoot, dir); err != nil {
		t.Fatalf("init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &lineWatcher{want: "local-exec", hit: make(chan struct{})}
	planFile := filepath.Join(repoRoot, ".deploy-logs", "svc.tfplan")
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- r.Apply(ctx, repoRoot, dir, planFile, nil, w) }()

	select {
	case <-w.hit:
	case err := <-done:
		t.Fatalf("apply finished before the provisioner started: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("provisioner never started")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Apply returned nil after cancel")
		}
	case <-time.After(45 * time.Second):
		t.Fatal("Apply did not return after cancel")
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("cancel took %v; the 30s provisioner does not look interrupted", elapsed)
	}

	work := filepath.Join(repoRoot, dir)
	if _, err := os.Stat(filepath.Join(work, ".terraform.tfstate.lock.info")); err == nil {
		t.Error("state lock was left behind after cancel")
	}
	state, err := os.ReadFile(filepath.Join(work, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("no state file written on cancel: %v", err)
	}
	if !strings.Contains(string(state), "terraform_data") {
		t.Errorf("state does not record the in-flight resource:\n%s", state)
	}
}
