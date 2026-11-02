// Package server is the live web dashboard: click Plan/Apply instead of
// going back and forth to a terminal, with terraform's output streamed
// to the browser in real time over SSE as it runs. It's a thin HTTP
// layer over the exact same internal/orchestrate functions the CLI
// uses — see orchestrate's package doc for why that matters.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sync"

	"homelab-deploy/internal/graph"
	"homelab-deploy/internal/manifest"
	"homelab-deploy/internal/tf"
)

// Server holds the one thing that actually needs protecting: only one
// job may run at a time. Terraform's own state locking protects a
// single directory from two concurrent applies, but nothing stops two
// browser tabs from starting two different *full runs* that both touch
// overlapping directories, or two people clicking Apply on the same
// directory a second apart. A single current-job slot, guarded by a
// mutex, is the simplest correct answer for a tool one person (or a
// small household) runs against their own homelab — not a general
// multi-tenant job queue, deliberately.
type Server struct {
	repoRoot string
	runner   *tf.Runner
	resolver *manifest.Resolver // nil = single-repo mode
	auth     *Auth              // never nil — see newServer

	mu      sync.Mutex
	current *Job
	jobs    map[string]*Job
	jobIDs  []string // insertion order, for capping history
}

// maxJobHistory caps how many finished jobs the server remembers (so a
// long-running process doesn't accumulate unbounded memory — each Job
// holds its full output history for replay).
const maxJobHistory = 50

// Serve starts the web dashboard, blocking until the server stops or
// fails. repoRoot is the same directory the CLI would be run from
// (services/, config/, infra/ live under it, or a .deploy-repos
// manifest — see internal/manifest — for multi-repo mode). auth is
// required: see Auth for how a request's identity and role are decided.
func Serve(repoRoot, addr string, auth *Auth) error {
	runner, err := tf.NewRunner()
	if err != nil {
		return err
	}
	s, err := newServer(repoRoot, runner, auth)
	if err != nil {
		return err
	}

	fmt.Printf("homelab-deploy web dashboard on http://%s\n", displayAddr(addr))
	log.Println("request logging enabled — every request and job action is logged below")
	if auth.disabled {
		log.Println("WARNING: running with --insecure-no-auth — anyone who can reach this port can plan AND apply")
	}
	return http.ListenAndServe(addr, s.Handler())
}

// newServer builds a Server without starting to listen — split out from
// Serve so tests can drive the routes via httptest without binding a
// real port. Loads the multi-repo manifest if one exists at repoRoot
// (nil resolver otherwise, i.e. single-repo mode) and wires it into
// runner once here, so every subsequent terraform invocation resolves
// paths correctly without any other code needing to know which mode
// it's in. auth must not be nil: a Server with no auth decision would
// serve apply to anyone, so that is an error rather than a default.
func newServer(repoRoot string, runner *tf.Runner, auth *Auth) (*Server, error) {
	if auth == nil {
		return nil, fmt.Errorf("server needs an auth configuration (use insecure no-auth explicitly if that is intended)")
	}
	resolver, err := manifest.NewResolver(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", manifest.FileName, err)
	}
	runner.SetResolver(resolver)
	s := &Server{repoRoot: repoRoot, runner: runner, resolver: resolver, auth: auth, jobs: map[string]*Job{}}

	// Seed "Last" from whatever report.json is already on disk (the CLI's
	// own last plan/apply, or an earlier server process's) — see
	// loadLastReportJob's doc comment. current stays nil (Running() is
	// false for it) so this never disables the toolbar buttons, it only
	// makes the dashboard's table and "Last job" line reflect real state
	// instead of starting blank every restart.
	if job := loadLastReportJob(repoRoot); job != nil {
		s.jobs[job.ID] = job
		s.jobIDs = append(s.jobIDs, job.ID)
		s.current = job
	}
	return s, nil
}

// Handler returns the routed http.Handler — used by Serve for the real
// listener and directly by tests via httptest.NewServer. Every request
// passes through loggingMiddleware first, then authentication (which
// also serves the /auth/* login routes); each route then states the
// minimum role it needs. Routes that start or
// steer work (POST) need the planner role at least, and handlers that
// depend on what is being run (kind=apply, cancelling an apply job)
// check the applier role themselves.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", require(RoleViewer, s.handleDashboard))
	mux.HandleFunc("GET /graph", require(RoleViewer, s.handleGraph))
	mux.HandleFunc("POST /run", require(RolePlanner, s.handleStartRun))
	mux.HandleFunc("GET /jobs/{id}", require(RoleViewer, s.handleJobPage))
	mux.HandleFunc("GET /jobs/{id}/stream", require(RoleViewer, s.handleJobStream))
	mux.HandleFunc("POST /jobs/{id}/cancel", require(RolePlanner, s.handleCancelJob))
	mux.HandleFunc("POST /jobs/{id}/confirm", require(RoleApplier, s.handleConfirmJob))
	mux.HandleFunc("GET /logs/{name}", require(RoleViewer, s.handleReportFile))
	return loggingMiddleware(securityHeaders(s.auth.wrap(mux)))
}

// securityHeaders is applied to every response, including the login pages
// and errors. Pages here carry an authenticated user's data and forms whose
// actions apply infrastructure changes, so they must not be framed by another
// page (clickjacking; a SameSite=Lax session cookie is still sent to a
// same-site iframe) and must not be kept by a shared cache.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func displayAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "localhost" + addr
	}
	return addr
}

func (s *Server) discoverGraph() (*graph.Graph, error) {
	g, err := graph.Discover(s.repoRoot)
	if err != nil {
		return nil, err
	}
	if err := g.Sort(); err != nil {
		return nil, err
	}
	return g, nil
}

// startJob starts a new job if none is currently running. dirs=nil means
// every directory in the current graph (a full run); a single-element
// slice means one directory's own button was clicked.
func (s *Server) startJob(kind string, dirs []string) (*Job, error) {
	isFullRun := dirs == nil
	var targetDirs []string

	if kind == "sync" {
		// sync operates on the manifest's own entries, not the
		// terraform dependency graph — sync has no dependency order to
		// respect, and a directory that doesn't parse as a Terraform
		// root yet (nothing cloned) wouldn't even show up via
		// discoverGraph in a meaningful way.
		m := s.resolver.Manifest()
		if m == nil {
			return nil, fmt.Errorf("no %s found — sync only applies in multi-repo mode", manifest.FileName)
		}
		targetDirs = m.Order
	} else {
		g, err := s.discoverGraph()
		if err != nil {
			return nil, err
		}
		if isFullRun {
			targetDirs = g.Order
		} else {
			// Validate the requested directory actually exists — never
			// trust a path straight from an HTTP request body without
			// checking it against the real, discovered graph first.
			found := false
			for _, d := range g.Order {
				if d == dirs[0] {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("unknown directory: %s", dirs[0])
			}
			targetDirs = dirs
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.Running() {
		return nil, fmt.Errorf("a %s job is already running (%s) — wait for it to finish first", s.current.Kind, s.current.ID)
	}

	id := newJobID()
	job := newJob(id, kind, targetDirs, isFullRun)
	s.current = job
	s.jobs[id] = job
	s.jobIDs = append(s.jobIDs, id)
	if len(s.jobIDs) > maxJobHistory {
		delete(s.jobs, s.jobIDs[0])
		s.jobIDs = s.jobIDs[1:]
	}

	go job.run(s.runner, s.resolver, s.repoRoot)
	return job, nil
}

func (s *Server) getJob(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
