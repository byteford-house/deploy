package report

import (
	"bytes"
	"strings"
	"testing"

	"homelab-deploy/internal/tf"
)

func summaryFixture() Report {
	return Report{
		Command: "plan",
		Entries: []Entry{
			{Dir: "services/clean", Status: StatusOK, Detail: "no changes"},
			{
				Dir: "services/web", Status: StatusChanges, Detail: "1 to add, 1 to change, 0 to destroy",
				Adds: 1, Changes: 1,
				ResourceChanges: []tf.ResourceChange{
					{Address: "nomad_job.web", Action: "update", Attributes: []tf.AttributeChange{
						{Path: "env.DB_PASSWORD", Before: "hunter2-old", After: "hunter2-new"},
					}},
					{Address: "random_id.x", Action: "create"},
				},
				RawOutput: "  ~ env.DB_PASSWORD = \"hunter2-old\" -> \"hunter2-new\"",
				Warnings:  []tf.Warning{{Summary: "Argument is deprecated", Detail: "token=abc123 in the detail"}},
			},
			{
				Dir: "services/broken", Status: StatusError,
				Detail:    "exit status 1\nError: provider said password=s3cret is wrong",
				RawOutput: "Error: provider said password=s3cret is wrong",
			},
		},
	}
}

func TestRenderSummaryMarkdown_ListsDirsStatusesAndResources(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSummaryMarkdown(&buf, summaryFixture()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"homelab-deploy plan",
		"1 failed", "1 with changes", "1 unchanged",
		"services/broken", "services/web", "services/clean",
		"1 to add, 1 to change, 0 to destroy",
		"`nomad_job.web`", "update",
		"`random_id.x`", "create",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	// Errors come first so they are visible at the top of a PR comment.
	if strings.Index(out, "services/broken") > strings.Index(out, "services/web") ||
		strings.Index(out, "services/web") > strings.Index(out, "services/clean") {
		t.Errorf("entries should be ordered errors, changes, clean:\n%s", out)
	}
}

// The summary is meant for a PR comment, so it must never carry attribute
// values, raw terraform output, warning detail, or anything past the first
// line of an error: any of those can contain a secret.
func TestRenderSummaryMarkdown_RedactsValuesAndRawOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSummaryMarkdown(&buf, summaryFixture()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, secret := range []string{"hunter2", "s3cret", "abc123", "DB_PASSWORD"} {
		if strings.Contains(out, secret) {
			t.Errorf("summary leaks %q:\n%s", secret, out)
		}
	}
}

func TestRenderSummaryMarkdown_EscapesMarkdownInDetail(t *testing.T) {
	rep := Report{Command: "plan", Entries: []Entry{{
		Dir: "services/a", Status: StatusError, Detail: "bad | thing <script>alert(1)</script>\nsecond line",
	}}}
	var buf bytes.Buffer
	if err := RenderSummaryMarkdown(&buf, rep); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "<script>") {
		t.Errorf("raw HTML in an error detail must be neutralised:\n%s", out)
	}
	if strings.Contains(out, "second line") {
		t.Errorf("only the first line of an error detail belongs in the summary:\n%s", out)
	}
}

func TestRenderSummaryMarkdown_CapsResourcesPerDirectory(t *testing.T) {
	var rcs []tf.ResourceChange
	for i := 0; i < 200; i++ {
		rcs = append(rcs, tf.ResourceChange{Address: "terraform_data.r" + strings.Repeat("x", i%5) + string(rune('a'+i%26)), Action: "create"})
	}
	rep := Report{Command: "plan", Entries: []Entry{{Dir: "services/big", Status: StatusChanges, Adds: 200, ResourceChanges: rcs}}}
	var buf bytes.Buffer
	if err := RenderSummaryMarkdown(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "more") {
		t.Errorf("a very long resource list should be truncated with a \"... more\" note")
	}
	if buf.Len() > 60000 {
		t.Errorf("summary is %d bytes; GitHub comments are limited to 65536", buf.Len())
	}
}

func TestRenderSummaryMarkdown_NothingToReport(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSummaryMarkdown(&buf, Report{Command: "plan", Entries: []Entry{{Dir: "a", Status: StatusOK, Detail: "no changes"}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No changes") {
		t.Errorf("expected a clear no-changes line:\n%s", buf.String())
	}
}
