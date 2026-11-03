package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"homelab-deploy/internal/tf"
)

// installFakeTerraform puts a `terraform` shell script first on PATH that
// answers `version -json` and appends every invocation's arguments (one
// per line, invocations separated by "--") to the returned log file. It
// lets these tests see exactly what the dashboard's runner would hand
// terraform without needing a real terraform binary or network access.
func installFakeTerraform(t *testing.T) (logFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake terraform is a shell script")
	}
	bin := t.TempDir()
	logFile = filepath.Join(bin, "calls.log")
	script := `#!/bin/sh
{ echo "--"; for a in "$@"; do echo "$a"; done; } >> '` + logFile + `'
if [ "$1" = "version" ]; then
  echo '{"terraform_version":"1.9.8","platform":"linux_amd64","provider_selections":{},"terraform_outdated":false}'
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

// A directory that carries the generated backend stub (deploy_backend.tf)
// declares `backend "s3" {}` with nothing inside; bucket, key and region
// only come from the [backend] env settings, as -backend-config at init.
// The dashboard's runner must receive those settings, or init on a
// directory with no local .terraform/ fails with "bucket/key/region
// required".
func TestBuildServer_InitPassesBackendConfigForStubbedDirectory(t *testing.T) {
	logFile := installFakeTerraform(t)

	repoRoot := t.TempDir()
	dir := filepath.Join("services", "newsvc")
	full := filepath.Join(repoRoot, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tf.WriteBackendStub(full, "s3"); err != nil {
		t.Fatal(err)
	}

	backend := tf.BackendConfig{Type: "s3", Settings: map[string]string{
		"bucket": "state-bucket",
		"key":    "{dir}.tfstate",
		"region": "eu-west-2",
	}}
	s, err := buildServer(repoRoot, &Auth{disabled: true}, backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.runner.Init(context.Background(), repoRoot, dir); err != nil {
		t.Fatalf("init: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(data)
	for _, want := range []string{
		"-backend-config=bucket=state-bucket",
		"-backend-config=key=" + dir + ".tfstate",
		"-backend-config=region=eu-west-2",
	} {
		if !strings.Contains(calls, want+"\n") {
			t.Errorf("terraform was never given %q; calls:\n%s", want, calls)
		}
	}
}

// With no [backend] configured (the default), the dashboard behaves as it
// did before: nothing is passed to init.
func TestBuildServer_InitPassesNoBackendConfigWhenUnconfigured(t *testing.T) {
	logFile := installFakeTerraform(t)

	repoRoot := t.TempDir()
	dir := filepath.Join("services", "newsvc")
	full := filepath.Join(repoRoot, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tf.WriteBackendStub(full, "s3"); err != nil {
		t.Fatal(err)
	}

	s, err := buildServer(repoRoot, &Auth{disabled: true}, tf.BackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.runner.Init(context.Background(), repoRoot, dir); err != nil {
		t.Fatalf("init: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "-backend-config") {
		t.Errorf("no [backend] configured, but terraform got -backend-config:\n%s", data)
	}
}
