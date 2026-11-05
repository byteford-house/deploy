package report

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"homelab-deploy/internal/graph"
	"homelab-deploy/internal/tf"
)

// report.json is the CI integration point, and the README documents its
// field names. This pins them: renaming a Go field would silently rename
// the JSON key (there are no struct tags) and break every consumer.
func TestWriteJSON_FieldNamesAreTheDocumentedSchema(t *testing.T) {
	rep := Report{
		GeneratedAt: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		Command:     "plan",
		Graph:       []graph.LayoutNode{{Dir: "services/web"}},
		Entries: []Entry{{
			Dir: "services/web", Status: StatusChanges,
			ResourceChanges: []tf.ResourceChange{{Address: "a.b", Action: "update", Attributes: []tf.AttributeChange{{Path: "p", Before: "x", After: "y"}}}},
			Warnings:        []tf.Warning{{Summary: "s", Detail: "d"}},
		}},
	}
	path, err := WriteJSON(t.TempDir(), rep)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	keys := func(m map[string]any) []string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	same := func(label string, got []string, want ...string) {
		t.Helper()
		sort.Strings(want)
		if len(got) != len(want) {
			t.Errorf("%s keys = %v, want %v", label, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s keys = %v, want %v", label, got, want)
				return
			}
		}
	}

	same("report", keys(doc), "GeneratedAt", "Command", "Entries", "Graph")
	entry := doc["Entries"].([]any)[0].(map[string]any)
	same("entry", keys(entry), "Dir", "Status", "Detail", "ResourceChanges", "Adds", "Changes", "Destroys",
		"RawOutput", "UnsavedPlan", "RemotePlanURL", "Warnings")
	rc := entry["ResourceChanges"].([]any)[0].(map[string]any)
	same("resource change", keys(rc), "Address", "Action", "Attributes")
	same("attribute", keys(rc["Attributes"].([]any)[0].(map[string]any)), "Path", "Before", "After")
	same("warning", keys(entry["Warnings"].([]any)[0].(map[string]any)), "summary", "detail")
	same("graph node", keys(doc["Graph"].([]any)[0].(map[string]any)), "Dir", "DependsOn", "Depth", "ManualStep", "NotSynced")
}
