package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// applyFakeTerraform puts a `terraform` shell script on PATH for apply runs:
// `plan` reports changes (exit 2) and `show -json` returns one created
// resource, so apply proceeds; `apply` appends the
// name of the directory it ran in to the returned log file, one per line.
func applyFakeTerraform(t *testing.T) (applyLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake terraform needs a POSIX shell")
	}
	bin := t.TempDir()
	applyLog = filepath.Join(t.TempDir(), "applied")
	script := `#!/bin/sh
case "$1" in
  version)
    echo '{"terraform_version":"1.9.8","platform":"linux_amd64","provider_selections":{},"terraform_outdated":false}'
    exit 0 ;;
  plan) echo "Plan: 1 to add, 0 to change, 0 to destroy."; exit 2 ;;
  apply) basename "$(pwd)" >> ` + applyLog + `; exit 0 ;;
  show)
    cat <<'JSON'
{"format_version":"1.2","terraform_version":"1.9.8","resource_changes":[{"address":"terraform_data.x","mode":"managed","type":"terraform_data","name":"x","provider_name":"terraform.io/builtin/terraform","change":{"actions":["create"],"before":null,"after":{"input":"v"},"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":{}}}]}
JSON
    exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return applyLog
}

func appliedDirs(t *testing.T, applyLog string) []string {
	t.Helper()
	b, err := os.ReadFile(applyLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// manualStepRepo builds, in apply order:
//
//	services/0ok   no manual step
//	services/a     .manual-before present (so it needs a manual step)
//	services/b     depends on services/a
//	services/c     independent of everything
func manualStepRepo(t *testing.T) string {
	t.Helper()
	root := planFakeRepo(t, "0ok", "a", "b", "c")
	write := func(rel, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(root, "services", filepath.FromSlash(rel)), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a/.manual-before", "create the DNS record by hand\n", 0o644)
	write("b/.deploy-deps", "services/a\n", 0o644)
	return root
}

// The behaviour a CI job relies on, pinned: `apply --non-interactive` with an
// unsatisfied .manual-before stops the WHOLE run at that directory (it does
// not skip just its dependents and carry on), applies nothing after it, and
// exits non-zero. Directories ordered before it were already applied.
func TestNonInteractiveApply_UnsatisfiedManualStepStopsTheRunAndFails(t *testing.T) {
	applyLog := applyFakeTerraform(t)
	root := manualStepRepo(t)

	err := cmdFullRun(context.Background(), noopResolver(t, root), root, "apply", false, true, outputOptions{})
	if err == nil {
		t.Fatal("apply returned nil; an unsatisfied manual step must fail the run")
	}
	if got := exitCodeOf(err); got != 1 {
		t.Errorf("exit code = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "manual step") {
		t.Errorf("error %q should say a manual step is the reason", err)
	}

	// 0ok sorts before a, so it ran. a stopped the run: neither its dependent
	// b nor the unrelated c was applied.
	if got, want := appliedDirs(t, applyLog), []string{"0ok"}; !reflect.DeepEqual(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}

	// The report records what happened, including the stop.
	raw, rerr := os.ReadFile(filepath.Join(root, ".deploy-logs", "report.json"))
	if rerr != nil {
		t.Fatalf("report.json should be written when apply fails: %v", rerr)
	}
	var rep struct {
		Entries []struct{ Dir, Status, Detail string }
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 2 || rep.Entries[0].Dir != "services/0ok" || rep.Entries[0].Status != "OK" ||
		rep.Entries[1].Dir != "services/a" || rep.Entries[1].Status != "ERROR" ||
		!strings.Contains(rep.Entries[1].Detail, "manual step required") {
		t.Errorf("unexpected report entries: %+v", rep.Entries)
	}
}

// A manual step whose .manual-check already passes is not a stop.
func TestNonInteractiveApply_SatisfiedManualCheckProceeds(t *testing.T) {
	applyLog := applyFakeTerraform(t)
	root := manualStepRepo(t)
	if err := os.WriteFile(filepath.Join(root, "services", "a", ".manual-check"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := cmdFullRun(context.Background(), noopResolver(t, root), root, "apply", false, true, outputOptions{}); err != nil {
		t.Fatalf("apply returned %v, want nil", err)
	}
	if got, want := appliedDirs(t, applyLog), []string{"0ok", "a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("applied %v, want %v (dependency order)", got, want)
	}
}

// Same rule for a single directory: refuse, do not apply, non-zero.
func TestNonInteractiveApply_SingleDirWithUnsatisfiedManualStepFails(t *testing.T) {
	applyLog := applyFakeTerraform(t)
	root := manualStepRepo(t)

	err := cmdSingleDir(context.Background(), noopResolver(t, root), root, "services/a", "apply", false, true, outputOptions{})
	if exitCodeOf(err) != 1 || err == nil || !strings.Contains(err.Error(), "manual step") {
		t.Fatalf("got %v (exit %d), want an error about the manual step with exit 1", err, exitCodeOf(err))
	}
	if got := appliedDirs(t, applyLog); len(got) != 0 {
		t.Errorf("applied %v, want nothing", got)
	}
}
