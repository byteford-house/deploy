package graph

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func sortedGraph(t *testing.T, deps map[string][]string) *Graph {
	t.Helper()
	g := &Graph{Nodes: map[string]*Node{}}
	for name, d := range deps {
		g.Nodes[name] = &Node{Dir: name, DependsOn: d}
	}
	if err := g.Sort(); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestWithDependents_FollowsDependsOnBackwardsTransitively(t *testing.T) {
	// infra/net <- services/a <- services/b ; services/c is unrelated.
	g := sortedGraph(t, map[string][]string{
		"infra/net":  nil,
		"services/a": {"infra/net"},
		"services/b": {"services/a"},
		"services/c": nil,
	})
	got := g.WithDependents([]string{"infra/net"})
	want := []string{"infra/net", "services/a", "services/b"} // dependency order
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := g.WithDependents([]string{"services/b"}); !reflect.DeepEqual(got, []string{"services/b"}) {
		t.Errorf("a leaf has no dependents: got %v", got)
	}
	if got := g.WithDependents([]string{"services/c", "nope"}); !reflect.DeepEqual(got, []string{"services/c"}) {
		t.Errorf("unknown names are dropped: got %v", got)
	}
}

func TestWithDependents_DiamondHasNoDuplicates(t *testing.T) {
	g := sortedGraph(t, map[string][]string{
		"infra/base": nil,
		"services/l": {"infra/base"},
		"services/r": {"infra/base"},
		"services/z": {"services/l", "services/r"},
	})
	got := g.WithDependents([]string{"infra/base"})
	if len(got) != 4 {
		t.Errorf("want 4 distinct dirs, got %v", got)
	}
}

// writeTF writes repoRoot/rel with content.
func writeTF(t *testing.T, repoRoot, rel, content string) {
	t.Helper()
	p := filepath.Join(repoRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStacksForPaths(t *testing.T) {
	root := t.TempDir()
	writeTF(t, root, "services/a/main.tf", `module "x" { source = "../../modules/shared" }`)
	writeTF(t, root, "services/b/main.tf", `module "y" { source = "../../modules/outer" }`)
	writeTF(t, root, "services/c/main.tf", `
terraform {
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}
# source = "../../modules/shared"
module "r" { source = "git::https://example.com/m.git" }
`)
	writeTF(t, root, "services/ab/main.tf", `resource "terraform_data" "x" {}`)
	writeTF(t, root, "modules/shared/main.tf", `resource "terraform_data" "x" {}`)
	// outer calls inner: a change to inner must reach services/b.
	writeTF(t, root, "modules/outer/main.tf", `module "i" { source = "../inner" }`)
	writeTF(t, root, "modules/inner/main.tf", `resource "terraform_data" "x" {}`)
	writeTF(t, root, "modules/unused/main.tf", `resource "terraform_data" "x" {}`)

	g := sortedGraph(t, map[string][]string{
		"services/a": nil, "services/b": nil, "services/c": nil, "services/ab": nil,
	})

	cases := []struct {
		name  string
		paths []string
		want  []string
	}{
		{"file in a stack", []string{"services/a/variables.tf"}, []string{"services/a"}},
		{"stack name prefix must not match a sibling", []string{"services/ab/main.tf"}, []string{"services/ab"}},
		{"module used by a stack", []string{"modules/shared/main.tf"}, []string{"services/a"}},
		{"module reached through another module", []string{"modules/inner/main.tf"}, []string{"services/b"}},
		{"module nobody uses", []string{"modules/unused/main.tf"}, nil},
		{"commented / registry / provider sources are not modules", []string{"modules/shared/x.tf"}, []string{"services/a"}},
		{"unrelated root file", []string{"README.md", ".github/workflows/ci.yml"}, nil},
		{"several paths, result in dependency order", []string{"modules/inner/a.tf", "services/a/b.tf"}, []string{"services/a", "services/b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := g.StacksForPaths(root, c.paths)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestStacksForPaths_ModuleOutsideRepoIsIgnored(t *testing.T) {
	root := t.TempDir()
	writeTF(t, root, "services/a/main.tf", `module "x" { source = "../../../elsewhere" }`)
	g := sortedGraph(t, map[string][]string{"services/a": nil})
	if got := g.StacksForPaths(root, []string{"elsewhere/main.tf"}); len(got) != 0 {
		t.Errorf("a module path outside the repo must not match: %v", got)
	}
}
