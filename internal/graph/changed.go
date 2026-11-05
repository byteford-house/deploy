package graph

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// WithDependents returns dirs plus every directory that depends on one of
// them, directly or transitively (following DependsOn backwards). The result
// is in g.Order when the graph has been sorted, alphabetical otherwise, with
// no duplicates. Names that are not in the graph are dropped.
func (g *Graph) WithDependents(dirs []string) []string {
	rev := map[string][]string{}
	for name, n := range g.Nodes {
		for _, dep := range n.DependsOn {
			rev[dep] = append(rev[dep], name)
		}
	}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		if _, ok := g.Nodes[name]; !ok {
			return
		}
		seen[name] = true
		for _, d := range rev[name] {
			walk(d)
		}
	}
	for _, d := range dirs {
		walk(d)
	}
	return g.inOrder(seen)
}

// inOrder lists the members of set in g.Order (if sorted) or alphabetically.
func (g *Graph) inOrder(set map[string]bool) []string {
	var out []string
	if len(g.Order) > 0 {
		for _, name := range g.Order {
			if set[name] {
				out = append(out, name)
			}
		}
		return out
	}
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// moduleSource matches a `source = "..."` line. Local modules are the ones
// whose value starts with ./ or ../ (comment lines are skipped); registry, git and provider sources
// (e.g. `source = "hashicorp/aws"` in required_providers) are ignored by the
// caller.
var moduleSource = regexp.MustCompile(`\bsource\s*=\s*"([^"]+)"`)

// localModuleDirs returns the repo-relative directories of every local module
// reachable from the Terraform files directly inside dir (repo-relative),
// following modules that themselves call local modules. Paths that leave the
// repository are skipped.
func localModuleDirs(repoRoot, dir string) map[string]bool {
	found := map[string]bool{}
	var visit func(d string)
	visit = func(d string) {
		files, _ := filepath.Glob(filepath.Join(repoRoot, filepath.FromSlash(d), "*.tf"))
		for _, f := range files {
			fh, err := os.Open(f)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(fh)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
					continue
				}
				m := moduleSource.FindStringSubmatch(line)
				if m == nil || !(strings.HasPrefix(m[1], "./") || strings.HasPrefix(m[1], "../")) {
					continue
				}
				mod := path.Clean(path.Join(d, m[1]))
				if mod == "." || mod == ".." || strings.HasPrefix(mod, "../") {
					continue // the repo root itself, or outside the repo
				}
				if !found[mod] {
					found[mod] = true
					visit(mod)
				}
			}
			fh.Close()
		}
	}
	visit(dir)
	return found
}

// StacksForPaths maps changed file paths (slash-separated, relative to
// repoRoot) to the directories in g they affect: a path inside a directory's
// own tree, or inside a local module that directory uses (directly or
// through other local modules). Only paths under a directory or one of its
// local modules count: a change to a shared file at the repo root is not
// attributed to anything. The result is in g.Order if sorted, alphabetical
// otherwise.
func (g *Graph) StacksForPaths(repoRoot string, paths []string) []string {
	hit := map[string]bool{}
	under := func(p, dir string) bool {
		return p == dir || strings.HasPrefix(p, dir+"/")
	}
	for name := range g.Nodes {
		roots := []string{filepath.ToSlash(name)}
		for mod := range localModuleDirs(repoRoot, name) {
			roots = append(roots, mod)
		}
		for _, p := range paths {
			p = path.Clean(filepath.ToSlash(p))
			for _, r := range roots {
				if under(p, r) {
					hit[name] = true
				}
			}
		}
	}
	return g.inOrder(hit)
}
