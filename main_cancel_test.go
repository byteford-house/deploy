package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"homelab-deploy/internal/orchestrate"
)

// writeBlockingFakeTerraform puts a `terraform` script on PATH that answers
// terraform-exec's `version -json` probe and, for any other subcommand,
// records that it started and then blocks until it gets SIGINT, on which it
// writes "interrupted" to the returned marker file and exits.
func writeBlockingFakeTerraform(t *testing.T) (started, interrupted string) {
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
trap 'echo interrupted > ` + interrupted + `; exit 130' INT
echo started > ` + started + `
while true; do sleep 0.1; done
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return started, interrupted
}

// Ctrl-C (SIGINT) on the CLI must reach a running terraform as a graceful
// interrupt. terraform runs in its own process group, so the terminal's
// SIGINT never reaches it directly; without handling in this process the
// CLI just dies and terraform-exec's Pdeathsig SIGKILLs terraform, leaving
// the state lock behind and the state without what was already created.
func TestRun_SIGINTInterruptsRunningTerraform(t *testing.T) {
	started, interrupted := writeBlockingFakeTerraform(t)

	repoRoot := t.TempDir()
	t.Chdir(repoRoot)
	dir := "svc"
	if err := os.MkdirAll(filepath.Join(repoRoot, dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An existing saved plan makes apply run `terraform apply <planfile>`.
	planFile := orchestrate.PlanFilePath(repoRoot, dir)
	if err := os.MkdirAll(filepath.Dir(planFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planFile, []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- run([]string{"--non-interactive", dir, "apply"}) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake terraform never started")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("run returned nil after being interrupted mid-apply")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after SIGINT")
	}

	got, err := os.ReadFile(interrupted)
	if err != nil {
		t.Fatalf("terraform was not sent SIGINT (it was killed instead): %v", err)
	}
	if strings.TrimSpace(string(got)) != "interrupted" {
		t.Errorf("unexpected marker content %q", got)
	}
}
