package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// planFakeTerraform puts a `terraform` shell script on PATH that behaves per
// directory name (the directory terraform is run in):
//
//	clean   plan exits 0 (no changes)
//	changed plan exits 2 (changes) and `show -json` returns one created resource
//	broken  plan exits 1 with an error
//
// Plan output deliberately contains a secret-looking value so tests can check
// it never reaches a summary.
func planFakeTerraform(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake terraform needs a POSIX shell")
	}
	bin := t.TempDir()
	script := `#!/bin/sh
name="$(basename "$(pwd)")"
case "$1" in
  version)
    echo '{"terraform_version":"1.9.8","platform":"linux_amd64","provider_selections":{},"terraform_outdated":false}'
    exit 0 ;;
  plan)
    case "$name" in
      clean) echo "No changes."; exit 0 ;;
      changed) echo "Plan: 1 to add, 0 to change, 0 to destroy."; exit 2 ;;
      broken) echo "Error: provider said password=s3cret is wrong" >&2; exit 1 ;;
    esac
    exit 0 ;;
  show)
    cat <<'JSON'
{"format_version":"1.2","terraform_version":"1.9.8","resource_changes":[{"address":"terraform_data.x","mode":"managed","type":"terraform_data","name":"x","provider_name":"terraform.io/builtin/terraform","change":{"actions":["create"],"before":null,"after":{"input":"hunter2"},"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":{}}}]}
JSON
    exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// planFakeRepo makes services/<name> for each name, marked initialized.
func planFakeRepo(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		dir := filepath.Join(root, "services", n)
		if err := os.MkdirAll(filepath.Join(dir, ".terraform"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "terraform_data" "x" {}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(nil); got != 0 {
		t.Errorf("nil error: exit %d, want 0", got)
	}
	if got := exitCodeOf(errors.New("boom")); got != 1 {
		t.Errorf("plain error: exit %d, want 1", got)
	}
	if got := exitCodeOf(&exitError{code: 2}); got != 2 {
		t.Errorf("exitError{2}: exit %d, want 2", got)
	}
}

func TestParseFlags_DetailedExitcodeAndSummaryMD(t *testing.T) {
	gf, rest, err := parseFlags([]string{"--detailed-exitcode", "--summary-md", "out/summary.md", "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if !gf.detailedExitcode || gf.summaryMD != "out/summary.md" {
		t.Errorf("flags not parsed: %+v", gf)
	}
	if len(rest) != 1 || rest[0] != "plan" {
		t.Errorf("remaining = %v", rest)
	}
	gf, _, _ = parseFlags([]string{"plan"})
	if gf.detailedExitcode || gf.summaryMD != "" {
		t.Errorf("both must default off: %+v", gf)
	}
}

func TestCmdFullRun_PlanExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		dirs     []string
		detailed bool
		want     int
	}{
		{"no changes", []string{"clean"}, true, 0},
		{"changes without the flag still exit 0", []string{"clean", "changed"}, false, 0},
		{"changes with --detailed-exitcode exit 2", []string{"clean", "changed"}, true, 2},
		{"an error exits 1 without the flag", []string{"clean", "broken"}, false, 1},
		{"an error beats changes", []string{"changed", "broken"}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			planFakeTerraform(t)
			root := planFakeRepo(t, c.dirs...)
			err := cmdFullRun(context.Background(), noopResolver(t, root), root, "plan", false, false, outputOptions{detailedExitcode: c.detailed})
			if got := exitCodeOf(err); got != c.want {
				t.Errorf("exit code = %d (err=%v), want %d", got, err, c.want)
			}
		})
	}
}

func TestCmdSingleDir_PlanDetailedExitcode(t *testing.T) {
	planFakeTerraform(t)
	root := planFakeRepo(t, "clean", "changed", "broken")
	r := noopResolver(t, root)
	for dir, want := range map[string]int{"services/clean": 0, "services/changed": 2, "services/broken": 1} {
		err := cmdSingleDir(context.Background(), r, root, dir, "plan", false, false, outputOptions{detailedExitcode: true})
		if got := exitCodeOf(err); got != want {
			t.Errorf("%s: exit code = %d (err=%v), want %d", dir, got, err, want)
		}
	}
	// Without the flag, changes are still success.
	if err := cmdSingleDir(context.Background(), r, root, "services/changed", "plan", false, false, outputOptions{}); err != nil {
		t.Errorf("changes without --detailed-exitcode returned %v, want nil", err)
	}
}

func TestCmdFullRun_SummaryMDIsWrittenAndRedacted(t *testing.T) {
	planFakeTerraform(t)
	root := planFakeRepo(t, "clean", "changed", "broken")
	summary := filepath.Join(t.TempDir(), "nested", "summary.md")
	// The run fails (broken), but the summary must still be written.
	err := cmdFullRun(context.Background(), noopResolver(t, root), root, "plan", false, false, outputOptions{summaryMD: summary})
	if exitCodeOf(err) != 1 {
		t.Fatalf("exit code = %d, want 1", exitCodeOf(err))
	}
	b, rerr := os.ReadFile(summary)
	if rerr != nil {
		t.Fatalf("summary not written: %v", rerr)
	}
	out := string(b)
	for _, want := range []string{"services/broken", "services/changed", "terraform_data.x", "services/clean"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"hunter2", "s3cret"} {
		if strings.Contains(out, secret) {
			t.Errorf("summary leaks %q:\n%s", secret, out)
		}
	}
}

func TestCmdSingleDir_SummaryMDForPlan(t *testing.T) {
	planFakeTerraform(t)
	root := planFakeRepo(t, "changed")
	summary := filepath.Join(t.TempDir(), "s.md")
	if err := cmdSingleDir(context.Background(), noopResolver(t, root), root, "services/changed", "plan", false, false, outputOptions{summaryMD: summary}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(summary)
	if err != nil || !strings.Contains(string(b), "terraform_data.x") {
		t.Errorf("single-directory plan summary missing or wrong (err=%v):\n%s", err, b)
	}
}

func TestCheckOutputOptions_SummaryMDNeedsPlanOrApply(t *testing.T) {
	if err := checkOutputOptions("graph", false, outputOptions{summaryMD: "x.md"}); err == nil {
		t.Error("--summary-md with graph should be rejected")
	}
	if err := checkOutputOptions("init", false, outputOptions{summaryMD: "x.md"}); err == nil {
		t.Error("--summary-md with init should be rejected")
	}
	// A single-directory apply produces no report entry to summarise.
	if err := checkOutputOptions("apply", true, outputOptions{summaryMD: "x.md"}); err == nil {
		t.Error("--summary-md with '<dir> apply' should be rejected")
	}
	if err := checkOutputOptions("plan", true, outputOptions{summaryMD: "x.md"}); err != nil {
		t.Errorf("--summary-md with '<dir> plan' should be allowed: %v", err)
	}
	if err := checkOutputOptions("apply", false, outputOptions{summaryMD: "x.md"}); err != nil {
		t.Errorf("--summary-md with a full apply should be allowed: %v", err)
	}
	if err := checkOutputOptions("graph", false, outputOptions{}); err != nil {
		t.Errorf("no output options must never be rejected: %v", err)
	}
}
