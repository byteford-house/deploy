package server

import (
	"log"
	"net/http"
	"path/filepath"
	"time"

	"homelab-deploy/internal/graph"
	"homelab-deploy/internal/orchestrate"
	"homelab-deploy/internal/report"
)

// reportFiles are the only files under .deploy-logs served over HTTP. The
// directory also holds saved terraform plan files (<dir>.tfplan), which can
// contain unmasked secret values that the rendered reports redact.
var reportFiles = map[string]bool{"report.html": true, "report.json": true}

func (s *Server) handleReportFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !reportFiles[name] {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.repoRoot, orchestrate.LogDir, name))
}

// streamRecheck is how often an open job stream re-checks that the viewer's
// session is still valid. A var so tests can shorten it.
var streamRecheck = 5 * time.Second

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	g, err := s.discoverGraph()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	last := s.current // the most recently started job, running or not
	s.mu.Unlock()

	// Current (used to disable buttons and show the "already running"
	// banner) must ONLY be set while a job is actually still in flight —
	// s.current itself is never cleared after a job finishes (it doubles
	// as "last job" for the link below), so checking presence alone here
	// would leave every button disabled forever after the very first
	// job, regardless of whether it's still running.
	var currentSnap, lastSnap *Snapshot
	lastEntries := map[string]report.Entry{}
	if last != nil {
		snap := last.Snapshot()
		lastSnap = &snap
		for _, e := range snap.Entries {
			lastEntries[e.Dir] = e
		}
		if last.Running() {
			runningSnap := snap
			currentSnap = &runningSnap
		}
	}

	data := dashboardData{
		Dirs:        g.Order,
		Current:     currentSnap,
		Last:        lastSnap,
		LastEntries: lastEntries,
		MultiRepo:   s.resolver.Manifest() != nil,
		Counts:      countByStatus(g.Order, lastEntries),
	}
	if err := dashboardTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleGraph renders the dependency graph on its own, straight from
// discoverGraph (a filesystem walk of .deploy-deps files) rather than
// from a report.html written after some past plan/apply run — so it's
// viewable even if a plan has never been run.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	g, err := s.discoverGraph()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nodes := graph.Layout(g, s.resolver, s.repoRoot)
	if err := report.RenderGraph(w, nodes); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kind := r.FormValue("kind")
	if kind != "plan" && kind != "apply" && kind != "init" && kind != "sync" {
		http.Error(w, "kind must be plan, apply, init, or sync", http.StatusBadRequest)
		return
	}

	dirParam := r.FormValue("dir")
	if kind == "sync" && dirParam != "" {
		// manifest.Sync always covers every entry — there's no
		// per-repo sync, matching the CLI (`homelab-deploy sync` has no
		// directory-targeting option either).
		http.Error(w, "sync doesn't support targeting a single repo", http.StatusBadRequest)
		return
	}
	var dirs []string // nil = full run
	if dirParam != "" {
		dirs = []string{dirParam}
	}

	// Audit trail: user, kind and dir on every run request, then its
	// outcome (denied / rejected / started with the job id, which the
	// "job finished" line repeats). dir is client-supplied, so it is
	// logged quoted (%q) — a newline in it must not forge a log line.
	id := identityFrom(r.Context())
	dirLog := dirParam
	if dirLog == "" {
		dirLog = "(all)"
	}
	log.Printf("run requested: user=%q kind=%s dir=%q from=%s", id.User, kind, dirLog, r.RemoteAddr)

	// The kind checked here is the very value used below: one read of
	// the form, so the gate and the runner cannot disagree about it.
	if need := roleForKind(kind); id.Role < need {
		log.Printf("run denied: user=%q role=%s kind=%s dir=%q needs %s", id.User, id.Role, kind, dirLog, need)
		http.Error(w, "forbidden: "+kind+" requires the "+need.String()+" role", http.StatusForbidden)
		return
	}

	job, err := s.startJob(kind, dirs)
	if err != nil {
		// A run is already in progress, or an unknown directory was
		// requested — either way this is the person's next useful
		// signal, not a 500.
		// err can embed the client-supplied dir ("unknown directory: ..."), so it is quoted too.
		log.Printf("run rejected: user=%q kind=%s dir=%q: %q", id.User, kind, dirLog, err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	log.Printf("job started: id=%s user=%q kind=%s dirs=%q", job.ID, id.User, job.Kind, job.Dirs)

	// Redirect back to the dashboard, not the job page — the dashboard
	// itself shows a live progress bar (via the same SSE stream the job
	// page uses) plus a link to the job page for full output, so there's
	// no need to navigate away just to start a run. X-Job-Id is set for
	// any programmatic caller that wants the id without scraping HTML;
	// the dashboard itself doesn't need it — it reads the running job
	// straight from server-side state.
	w.Header().Set("X-Job-Id", job.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleJobPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.getJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := jobTmpl.Execute(w, job.Snapshot()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleCancelJob requests that a running job stop. Redirects back to
// the job page either way (there's no useful distinct response for
// "already finished" — the page itself will simply show the job as
// done, not canceled) rather than a bare 200/409, since this is only
// ever hit from a browser form, not a programmatic caller.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.getJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Stopping an apply mid-step can leave state inconsistent, so it
	// takes the same role as starting one.
	who := identityFrom(r.Context())
	if need := roleForKind(job.Kind); who.Role < need {
		log.Printf("cancel denied: user=%q role=%s id=%s kind=%s needs %s", who.User, who.Role, id, job.Kind, need)
		http.Error(w, "forbidden: cancelling a "+job.Kind+" job requires the "+need.String()+" role", http.StatusForbidden)
		return
	}
	if job.Cancel() {
		log.Printf("job cancel requested: id=%s user=%q kind=%s from=%s", id, who.User, job.Kind, r.RemoteAddr)
	} else {
		log.Printf("job cancel requested but already finished: id=%s user=%q from=%s", id, who.User, r.RemoteAddr)
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handleConfirmJob approves applying the directory currently paused inside
// the job's own awaitConfirm call — see Job.Confirm's doc comment for why
// dir must match. Redirects back to the job page either way (like
// handleCancelJob, there's no useful distinct response for "nothing
// pending" from a plain browser form submit).
func (s *Server) handleConfirmJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.getJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dir := r.FormValue("dir")
	who := identityFrom(r.Context()).User
	if job.Confirm(dir) {
		log.Printf("apply confirmed: id=%s user=%q dir=%q from=%s", id, who, dir, r.RemoteAddr)
	} else {
		log.Printf("apply confirmation ignored (nothing pending for dir): id=%s user=%q dir=%q from=%s", id, who, dir, r.RemoteAddr)
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func (s *Server) handleJobStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.getJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// The generic access-log line for this request only prints once the
	// (potentially long-lived) connection closes — log the connect/
	// disconnect separately so a stream that's open for minutes doesn't
	// look like nothing is happening.
	who := identityFrom(r.Context()).User
	log.Printf("stream connected: job=%s user=%q from=%s", id, who, r.RemoteAddr)
	defer log.Printf("stream disconnected: job=%s user=%q from=%s", id, who, r.RemoteAddr)

	replay, live := job.broadcaster.Subscribe()
	defer job.broadcaster.Unsubscribe(live)

	for _, frame := range replay {
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	flusher.Flush()

	ctx := r.Context()
	recheck := time.NewTicker(streamRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-recheck.C:
			// Authentication happened when the stream opened; a long job (or
			// one paused awaiting confirmation) must not keep streaming to
			// someone who has since logged out or whose session expired.
			if !sessionStillValid(ctx) {
				log.Printf("stream closed: session ended: job=%s", id)
				return
			}
		case frame, ok := <-live:
			if !ok {
				return // broadcaster closed: job finished, stream ends
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// dashboardData feeds the dashboard template: Dirs is the topological
// order (so the table reads in apply order). Dependency info isn't
// included here on purpose — that's what the static report's graph tab
// is for; duplicating it as a table column would just be redundant.
// Current is set only while a job is actually running (disables
// buttons, shows the "already running" banner). Last is set whenever
// there's been any job at all, running or finished — it's what lets the
// dashboard link back to the most recent job's live/replayed output
// after you've navigated away from it, which is otherwise nowhere to
// find again. LastEntries indexes Last's Entries by directory so the
// template can look up each row's own result directly (a zero-value
// report.Entry — empty Status — for a directory not covered by the last
// job, e.g. it only ran a single-directory job for something else)
// instead of the template needing to search a slice.
type dashboardData struct {
	Dirs        []string
	Current     *Snapshot
	Last        *Snapshot
	LastEntries map[string]report.Entry
	MultiRepo   bool // whether a .deploy-repos manifest is in play — gates the "Sync All" button
	Counts      statusCounts
}

// statusCounts tallies Dirs by LastEntries' status, so the dashboard can
// show an at-a-glance summary (and per-filter counts) without the
// template itself doing the counting — text/template has no clean way
// to accumulate across a range.
type statusCounts struct {
	OK, Changes, Error, None int
}

func countByStatus(dirs []string, entries map[string]report.Entry) statusCounts {
	var c statusCounts
	for _, d := range dirs {
		switch entries[d].Status {
		case report.StatusOK:
			c.OK++
		case report.StatusChanges:
			c.Changes++
		case report.StatusError:
			c.Error++
		default:
			c.None++
		}
	}
	return c
}
