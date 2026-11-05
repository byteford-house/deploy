package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// changedFixture builds a git repo with
//
//	infra/net            (no deps)
//	services/a           (depends on infra/net, uses modules/shared)
//	services/b           (depends on services/a)
//	services/c           (independent)
//
// commits it on main, and returns the repo root. Tests then commit changes
// on top and ask which stacks `changed --since main` reports.
func changedFixture(t *testing.T) string {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("infra/net/main.tf", `resource "terraform_data" "x" {}`)
	write("services/a/main.tf", "module \"s\" {\n  source = \"../../modules/shared\"\n}\n")
	write("services/a/.deploy-deps", "infra/net\n")
	write("services/b/main.tf", `resource "terraform_data" "x" {}`)
	write("services/b/.deploy-deps", "services/a\n")
	write("services/c/main.tf", `resource "terraform_data" "x" {}`)
	write("modules/shared/main.tf", `resource "terraform_data" "x" {}`)
	write("README.md", "hi")

	runGitT(t, root, "init", "--quiet", "--initial-branch=main")
	runGitT(t, root, "config", "user.email", "test@example.com")
	runGitT(t, root, "config", "user.name", "test")
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "--quiet", "-m", "base")
	runGitT(t, root, "checkout", "--quiet", "-b", "change")
	return root
}

func commitChange(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("# changed "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "--quiet", "-m", "change "+rel)
}

func runChanged(t *testing.T, root, since string, withDependents bool, format string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := cmdChanged(&buf, root, since, withDependents, format)
	return buf.String(), err
}

func TestCmdChanged_ReportsStacksWithChangedFiles(t *testing.T) {
	root := changedFixture(t)
	commitChange(t, root, "services/c/extra.tf")

	out, err := runChanged(t, root, "main", false, "lines")
	if err != nil {
		t.Fatal(err)
	}
	if out != "services/c\n" {
		t.Errorf("got %q, want just services/c", out)
	}
}

func TestCmdChanged_ModuleChangeReachesTheStacksUsingIt(t *testing.T) {
	root := changedFixture(t)
	commitChange(t, root, "modules/shared/main.tf")

	out, err := runChanged(t, root, "main", false, "lines")
	if err != nil {
		t.Fatal(err)
	}
	if out != "services/a\n" {
		t.Errorf("got %q, want services/a (the only user of modules/shared)", out)
	}
}

func TestCmdChanged_WithDependentsAddsThemInApplyOrder(t *testing.T) {
	root := changedFixture(t)
	commitChange(t, root, "infra/net/main.tf")

	without, _ := runChanged(t, root, "main", false, "lines")
	if without != "infra/net\n" {
		t.Errorf("without --with-dependents: got %q", without)
	}
	with, err := runChanged(t, root, "main", true, "lines")
	if err != nil {
		t.Fatal(err)
	}
	if want := "infra/net\nservices/a\nservices/b\n"; with != want {
		t.Errorf("with --with-dependents: got %q, want %q", with, want)
	}
}

// `--since main` is a merge-base comparison: work that landed on main after
// this branch forked is not this branch's change and must not be reported.
func TestCmdChanged_IgnoresWorkThatLandedOnTheBaseAfterBranching(t *testing.T) {
	root := changedFixture(t)
	commitChange(t, root, "services/a/extra.tf")
	runGitT(t, root, "checkout", "--quiet", "main")
	commitChange(t, root, "services/c/landed-on-main.tf")
	runGitT(t, root, "checkout", "--quiet", "change")

	out, err := runChanged(t, root, "main", false, "lines")
	if err != nil {
		t.Fatal(err)
	}
	if out != "services/a\n" {
		t.Errorf("got %q, want only services/a (services/c changed on main, not here)", out)
	}
}

// A file moved from one stack to another is a change to BOTH. With git's
// rename detection on, --name-only lists only the destination, so the stack
// that lost the file would be skipped.
func TestCmdChanged_MovedFileFlagsBothTheSourceAndDestinationStack(t *testing.T) {
	root := changedFixture(t)
	runGitT(t, root, "mv", "services/c/main.tf", "services/b/moved.tf")
	runGitT(t, root, "commit", "--quiet", "-m", "move c/main.tf into b")

	out, err := runChanged(t, root, "main", false, "lines")
	if err != nil {
		t.Fatal(err)
	}
	if out != "services/b\nservices/c\n" {
		t.Errorf("got %q, want both services/b and services/c", out)
	}
}

func TestCmdChanged_NothingChangedPrintsNothing(t *testing.T) {
	root := changedFixture(t)
	commitChange(t, root, "README.md")

	out, err := runChanged(t, root, "main", true, "lines")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("a change outside every stack must print nothing, got %q", out)
	}
}

func TestCmdChanged_JSONIsAnArrayEvenWhenEmpty(t *testing.T) {
	root := changedFixture(t)
	commitChange(t, root, "services/c/extra.tf")
	out, err := runChanged(t, root, "main", false, "json")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil || !reflect.DeepEqual(got, []string{"services/c"}) {
		t.Errorf("got %q (%v), want [\"services/c\"]", out, err)
	}

	commitChange(t, root, "README.md")
	runGitT(t, root, "checkout", "--quiet", "-B", "empty", "main")
	out, err = runChanged(t, root, "main", false, "json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty result must be [] so a workflow matrix parses it: got %q (%v)", out, err)
	}
}

func TestCmdChanged_Errors(t *testing.T) {
	root := changedFixture(t)
	if _, err := runChanged(t, root, "", false, "lines"); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Errorf("missing --since must be an error mentioning it, got %v", err)
	}
	if _, err := runChanged(t, root, "no-such-ref", false, "lines"); err == nil {
		t.Error("an unknown git ref must be an error, not an empty result (CI would silently skip everything)")
	}
	if _, err := runChanged(t, root, "main", false, "yaml"); err == nil {
		t.Error("unknown format must be rejected")
	}
	if err := os.WriteFile(filepath.Join(root, ".deploy-repos"), []byte("a=./a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runChanged(t, root, "main", false, "lines"); err == nil || !strings.Contains(err.Error(), "single-repo") {
		t.Errorf("a .deploy-repos manifest must be rejected explicitly, got %v", err)
	}
}

func TestParseFlags_SinceAndWithDependents(t *testing.T) {
	gf, rest, err := parseFlags([]string{"changed", "--since", "origin/main", "--with-dependents", "json"})
	if err != nil {
		t.Fatal(err)
	}
	if gf.since != "origin/main" || !gf.withDependents {
		t.Errorf("flags not parsed: %+v", gf)
	}
	if !reflect.DeepEqual(rest, []string{"changed", "json"}) {
		t.Errorf("remaining = %v", rest)
	}
}
