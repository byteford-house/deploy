package report

import (
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxSummaryResources caps the resources listed under one directory.
	maxSummaryResources = 50
	// maxSummaryBytes keeps the whole summary under GitHub's 65536-character
	// limit on a comment body, with room for whatever a workflow adds around it.
	maxSummaryBytes = 60000
	// maxSummaryDetail caps the one error line shown per failed directory.
	maxSummaryDetail = 200
)

// RenderSummaryMarkdown writes a short Markdown digest of rep, meant to be
// posted as a pull-request comment or job summary.
//
// It is deliberately thin: directory names, statuses, add/change/destroy
// counts, resource addresses with their action, warning headlines and the
// FIRST LINE of an error. It never includes attribute values (before/after),
// raw terraform output, warning detail or the rest of an error message, since
// any of those can carry a secret. The full output stays in report.json and
// the log directory, which a workflow can upload as an artifact instead.
func RenderSummaryMarkdown(w io.Writer, rep Report) error {
	var failed, changed, clean []Entry
	for _, e := range sortedEntries(rep.Entries) {
		switch e.Status {
		case StatusError:
			failed = append(failed, e)
		case StatusChanges:
			changed = append(changed, e)
		default:
			clean = append(clean, e)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "### homelab-deploy %s\n\n", mdText(rep.Command))

	cleanWord := "unchanged"
	if rep.Command == "apply" {
		cleanWord = "applied"
	}
	var counts []string
	if len(failed) > 0 {
		counts = append(counts, fmt.Sprintf("%d failed", len(failed)))
	}
	if len(changed) > 0 {
		counts = append(counts, fmt.Sprintf("%d with changes", len(changed)))
	}
	if len(clean) > 0 {
		counts = append(counts, fmt.Sprintf("%d %s", len(clean), cleanWord))
	}
	if len(counts) == 0 {
		counts = append(counts, "no directories")
	}
	b.WriteString(strings.Join(counts, " · ") + "\n\n")

	if len(failed) == 0 && len(changed) == 0 && len(clean) > 0 && rep.Command != "apply" {
		b.WriteString("No changes.\n\n")
	}

	for _, e := range failed {
		fmt.Fprintf(&b, "- **ERROR** %s — %s\n", code(e.Dir), mdLine(e.Detail))
		writeWarnings(&b, e)
	}
	for _, e := range changed {
		fmt.Fprintf(&b, "- **CHANGES** %s — %s\n", code(e.Dir), mdLine(e.Detail))
		for i, rc := range e.ResourceChanges {
			if i == maxSummaryResources {
				fmt.Fprintf(&b, "  - … and %d more\n", len(e.ResourceChanges)-maxSummaryResources)
				break
			}
			fmt.Fprintf(&b, "  - %s %s\n", mdText(rc.Action), code(rc.Address))
		}
		writeWarnings(&b, e)
	}
	if len(clean) > 0 {
		names := make([]string, len(clean))
		for i, e := range clean {
			names[i] = code(e.Dir)
		}
		fmt.Fprintf(&b, "- %s (%d): %s\n", capitalise(cleanWord), len(clean), strings.Join(names, ", "))
	}

	b.WriteString("\n_Values and full output are omitted on purpose; see the uploaded `.deploy-logs/` artifact._\n")

	out := b.String()
	if len(out) > maxSummaryBytes {
		out = out[:maxSummaryBytes]
		if i := strings.LastIndex(out, "\n"); i > 0 {
			out = out[:i+1]
		}
		out += "\n… summary truncated to fit a GitHub comment; see report.json for everything.\n"
	}
	_, err := io.WriteString(w, out)
	return err
}

// WriteSummaryMarkdown renders rep with RenderSummaryMarkdown into path,
// creating parent directories.
func WriteSummaryMarkdown(path string, rep Report) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := RenderSummaryMarkdown(f, rep); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeWarnings(b *strings.Builder, e Entry) {
	for _, w := range e.Warnings {
		fmt.Fprintf(b, "  - warning: %s\n", mdLine(w.Summary))
	}
}

// code wraps s in an inline code span; backticks inside s are replaced so
// they cannot end the span early.
func code(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

// mdText makes s safe to place in running Markdown text: HTML-escaped, with
// newlines flattened.
func mdText(s string) string {
	s = html.EscapeString(strings.TrimSpace(s))
	return strings.NewReplacer("\r", " ", "\n", " ", "|", "\\|").Replace(s)
}

// mdLine is mdText of only the first non-empty line of s, length-capped.
func mdLine(s string) string {
	first := ""
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			first = line
			break
		}
	}
	if r := []rune(first); len(r) > maxSummaryDetail {
		first = string(r[:maxSummaryDetail]) + "…"
	}
	return mdText(first)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
