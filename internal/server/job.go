package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"homelab-deploy/internal/graph"
	"homelab-deploy/internal/manifest"
	"homelab-deploy/internal/orchestrate"
	"homelab-deploy/internal/report"
	"homelab-deploy/internal/tf"
)

// Job is one plan/apply/init run, trackable and streamable over SSE.
// Every event (a chunk of terraform's live output, a directory
// finishing, the whole job finishing) is a single pre-formatted SSE
// frame published to Job's Broadcaster, which a browser subscribes to
// at /jobs/{id}/stream.
type Job struct {
	ID   string
	Kind string // "plan", "apply", "init"
	Dirs []string
	// IsFullRun marks a job started against every discovered directory
	// (the "Plan All"/"Apply All" buttons) rather than one directory's
	// own button. Only a full run updates the shared static
	// .deploy-logs/report.html/json — a single-directory job would
	// otherwise clobber that report with a view missing every other
	// directory.
	IsFullRun bool
	StartedAt time.Time

	broadcaster *Broadcaster

	ctx        context.Context
	cancelFunc context.CancelFunc

	mu         sync.Mutex
	finishedAt time.Time // zero until done
	entries    []report.Entry
	err        error
	canceled   bool // true once Cancel has been called, regardless of how the job actually ended

	// pendingConfirmDir/pendingConfirmCh track a directory currently
	// paused inside awaitConfirm, waiting for a person to approve
	// applying it (or for Cancel/ctx to decline it) — see awaitConfirm.
	// Empty/nil whenever nothing is paused, which is true almost all the
	// time: ApplySequential only reaches this state once per directory,
	// and only for one directory at a time (it's a strictly sequential
	// loop), so a single pair of fields is enough.
	pendingConfirmDir     string
	pendingConfirmPreview confirmPreview
	pendingConfirmCh      chan bool
}

// confirmPreview is what a person needs to see to decide whether to
// approve applying a directory — either a structured diff (the normal
// case, backend saved a real plan file) or raw plan text plus a link to
// view it in Terraform Cloud (a remote-execution backend, which can't
// save a plan file at all — see tf.PlanResult.UnsavedPlan). Exactly one
// of ResourceChanges or RawPlanText is populated for a given preview.
type confirmPreview struct {
	Detail          string
	ResourceChanges []tf.ResourceChange
	Warnings        []tf.Warning
	RawPlanText     string
	RemotePlanURL   string
}

func newJob(id, kind string, dirs []string, isFullRun bool) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	return &Job{
		ID:          id,
		Kind:        kind,
		Dirs:        append([]string(nil), dirs...),
		IsFullRun:   isFullRun,
		StartedAt:   time.Now(),
		broadcaster: NewBroadcaster(gapFrame),
		ctx:         ctx,
		cancelFunc:  cancel,
	}
}

// Cancel requests that the job stop as soon as possible — its context is
// canceled, which terraform-exec propagates to the running `terraform`
// process as an interrupt (SIGINT), the same way the CLI's own --timeout
// and Ctrl-C do. Terraform treats that as a request to stop gracefully:
// it saves state for what it already created and releases its state
// lock, and terraform-exec only force-kills it if it has not exited after
// its wait delay. Safe to call more than once, or after the job has
// already finished on its own (a no-op then — reports false as "nothing
// to cancel").
func (j *Job) Cancel() bool {
	j.mu.Lock()
	if !j.finishedAt.IsZero() {
		j.mu.Unlock()
		return false
	}
	j.canceled = true
	j.mu.Unlock()
	j.cancelFunc()
	return true
}

// awaitConfirm pauses dir's apply, publishing an "awaiting_confirmation"
// SSE event with preview so a browser can show exactly what's about to be
// applied, then blocks until either Confirm(dir) is called (returns true)
// or the job's context is canceled — via the top-level Cancel button, same
// as declining terraform's own interactive "yes" prompt (returns false).
// Called synchronously from orchestrate.ApplySequential's own goroutine
// (the job's), so blocking here is exactly right — it's what actually
// pauses the sequence.
func (j *Job) awaitConfirm(dir string, preview confirmPreview) bool {
	ch := make(chan bool, 1)
	j.mu.Lock()
	j.pendingConfirmDir = dir
	j.pendingConfirmPreview = preview
	j.pendingConfirmCh = ch
	j.mu.Unlock()

	j.publish("awaiting_confirmation", struct {
		Dir             string              `json:"dir"`
		Detail          string              `json:"detail,omitempty"`
		ResourceChanges []tf.ResourceChange `json:"resourceChanges,omitempty"`
		Warnings        []tf.Warning        `json:"warnings,omitempty"`
		RawPlanText     string              `json:"rawPlanText,omitempty"`
		RemotePlanURL   string              `json:"remotePlanUrl,omitempty"`
	}{dir, preview.Detail, preview.ResourceChanges, preview.Warnings, preview.RawPlanText, preview.RemotePlanURL})

	var confirmed bool
	select {
	case confirmed = <-ch:
	case <-j.ctx.Done():
		confirmed = false
	}

	j.mu.Lock()
	j.pendingConfirmDir = ""
	j.pendingConfirmPreview = confirmPreview{}
	j.pendingConfirmCh = nil
	j.mu.Unlock()

	j.publish("confirmation_resolved", struct {
		Dir string `json:"dir"`
	}{dir})
	return confirmed
}

// Confirm approves the currently-pending directory's apply — a no-op
// (returns false) unless dir matches whatever awaitConfirm is actually
// blocked on right now, which guards against a stale confirm click (an
// old page, a double-click) resolving the wrong — possibly later —
// directory's pause.
func (j *Job) Confirm(dir string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.pendingConfirmCh == nil || j.pendingConfirmDir != dir {
		return false
	}
	ch := j.pendingConfirmCh
	j.pendingConfirmCh = nil // a second click on the same dir is then a no-op too
	ch <- true
	return true
}

// gapFrame formats a "gap" SSE event telling a client that some number
// of output frames never reached it — either trimmed from history before
// it could ever replay them, or dropped live because its own connection
// was too slow to keep up. Passed into Broadcaster (rather than
// hardcoded there) so Broadcaster itself stays ignorant of SSE's wire
// format — see its package doc.
func gapFrame(dropped int) []byte {
	data, err := json.Marshal(struct {
		Dropped int `json:"dropped"`
	}{dropped})
	if err != nil {
		return nil
	}
	return []byte(fmt.Sprintf("event: gap\ndata: %s\n\n", data))
}

// Snapshot is a point-in-time, lock-safe view of a Job for rendering.
type Snapshot struct {
	ID         string
	Kind       string
	Dirs       []string
	StartedAt  time.Time
	FinishedAt time.Time // zero if still running
	Entries    []report.Entry
	Err        error
	Canceled   bool // true if Cancel was called, whether or not it beat the job to finishing naturally

	// AwaitingConfirmDir is non-empty exactly when the job is currently
	// paused inside awaitConfirm for this directory — lets a fresh page
	// load (or the dashboard) render the pause immediately, without
	// waiting on an SSE event to arrive first.
	AwaitingConfirmDir     string
	AwaitingConfirmPreview confirmPreview
}

func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Snapshot{
		ID: j.ID, Kind: j.Kind, Dirs: j.Dirs, StartedAt: j.StartedAt,
		FinishedAt:             j.finishedAt,
		Entries:                append([]report.Entry(nil), j.entries...),
		Err:                    j.err,
		Canceled:               j.canceled,
		AwaitingConfirmDir:     j.pendingConfirmDir,
		AwaitingConfirmPreview: j.pendingConfirmPreview,
	}
}

// Running reports whether the job this snapshot was taken from was
// still in progress at that moment.
func (s Snapshot) Running() bool {
	return s.FinishedAt.IsZero()
}

func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finishedAt.IsZero()
}

func (j *Job) finish(entries []report.Entry, err error) {
	j.mu.Lock()
	j.finishedAt = time.Now()
	j.entries = entries
	j.err = err
	canceled := j.canceled
	j.mu.Unlock()

	// Releases the context's resources now that nothing will select on
	// it again — a no-op if Cancel already called this, since
	// CancelFuncs are idempotent. Every job reaches finish exactly once
	// on every path (see run's switch), so this is the one place that
	// needs to do it rather than every case duplicating a defer.
	j.cancelFunc()

	switch {
	case canceled:
		log.Printf("job finished: id=%s kind=%s CANCELED", j.ID, j.Kind)
	case err != nil:
		log.Printf("job finished: id=%s kind=%s FAILED: %v", j.ID, j.Kind, err)
	default:
		log.Printf("job finished: id=%s kind=%s ok (%d %s)", j.ID, j.Kind, len(entries), pluralize(len(entries), "entry", "entries"))
	}

	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	j.publish("job_done", struct {
		Err      string `json:"err,omitempty"`
		Canceled bool   `json:"canceled,omitempty"`
	}{errStr, canceled})
	j.broadcaster.Close()
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// publish formats v as JSON and sends it as one SSE frame under the
// given event name.
func (j *Job) publish(event string, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return // shouldn't happen for the plain structs used here
	}
	j.broadcaster.Publish([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)))
}

// dirOutputWriter adapts a Job+directory into an io.Writer suitable for
// orchestrate.Hooks.LiveWriter — every Write becomes one "output" SSE
// frame tagged with which directory it came from, so the browser can
// route it to the right pane.
type dirOutputWriter struct {
	job *Job
	dir string
}

func (w dirOutputWriter) Write(p []byte) (int, error) {
	w.job.publish("output", struct {
		Dir  string `json:"dir"`
		Text string `json:"text"`
	}{w.dir, string(p)})
	return len(p), nil
}

var _ io.Writer = dirOutputWriter{}

func (j *Job) hooks(resolver *manifest.Resolver) orchestrate.Hooks {
	return orchestrate.Hooks{
		LiveWriter: func(dir string) io.Writer {
			return dirOutputWriter{job: j, dir: dir}
		},
		OnDirDone: func(dir string, entry report.Entry) {
			j.publish("status", struct {
				Dir           string       `json:"dir"`
				Status        string       `json:"status"`
				Detail        string       `json:"detail"`
				RemotePlanURL string       `json:"remotePlanUrl,omitempty"`
				Warnings      []tf.Warning `json:"warnings,omitempty"`
			}{dir, string(entry.Status), entry.Detail, entry.RemotePlanURL, entry.Warnings})
		},
		// Non-interactive by design — see DefaultManualStepHandler's
		// doc comment. The one addition here: publish an explanatory
		// output line first, so a blocked directory's live pane says
		// *why* instead of just going straight to an ERROR badge with
		// no context.
		HandleManualStep: func(dir string, ms graph.ManualStep) bool {
			if ms.Instructions != "" && !ms.Satisfied {
				dirOutputWriter{job: j, dir: dir}.Write([]byte(
					"manual step required before this directory can be applied from the web UI:\n" +
						ms.Instructions + "\n\nRun `homelab-deploy " + dir + " apply` from the CLI once this is done, or add a .manual-check script so this is detected automatically.\n"))
			}
			return orchestrate.DefaultManualStepHandler(dir, ms)
		},
		// Pauses the job right here, showing the exact diff about to be
		// applied — see awaitConfirm's doc comment. This is the gate
		// that makes clicking Apply behave like terraform's own
		// interactive prompt instead of applying unreviewed.
		HandleApplyConfirm: func(dir string, entry report.Entry) bool {
			return j.awaitConfirm(dir, confirmPreview{
				Detail:          entry.Detail,
				ResourceChanges: entry.ResourceChanges,
				Warnings:        entry.Warnings,
			})
		},
		// Without this, a Terraform Cloud remote-execution workspace
		// (the only backend that can't save a plan file at all — see
		// tf.PlanResult.UnsavedPlan) would fall back to
		// DefaultUnsavedPlanHandler and could never be applied from the
		// web UI at all. Reuses the exact same pause mechanism, just
		// with the raw plan text Terraform printed (there's no
		// structured diff for this backend) plus a link to view the
		// run in Terraform Cloud, when Terraform printed one.
		HandleUnsavedPlanApply: func(dir, planText string) bool {
			return j.awaitConfirm(dir, confirmPreview{
				RawPlanText:   planText,
				RemotePlanURL: tf.ParseRemotePlanURL(planText),
			})
		},
		Resolver: resolver,
	}
}

// run executes the job's plan/apply/init synchronously — callers launch
// this in its own goroutine. repoRoot/runner come from the Server so
// this file doesn't need to know how those are constructed. Uses j.ctx
// (created alongside j.cancelFunc in newJob) rather than taking a
// context parameter, so Cancel can reach exactly the run this job owns
// without the caller needing to hold onto it separately.
func (j *Job) run(runner *tf.Runner, resolver *manifest.Resolver, repoRoot string) {
	ctx := j.ctx
	switch j.Kind {
	case "plan":
		entries := orchestrate.PlanAll(ctx, runner, repoRoot, j.Dirs, j.hooks(resolver))
		if j.IsFullRun {
			// Written BEFORE finish() deliberately: finish() publishes
			// the job_done SSE event, which is what unblocks anyone
			// watching the stream (including a browser about to follow
			// the "static report" link, or a test reading until
			// job_done). If the report were written after, there'd be a
			// real window where a client sees "done" before
			// report.html/report.json actually exist on disk.
			writeJobReport(repoRoot, resolver, "plan", entries)
		}
		j.finish(entries, nil)
	case "apply":
		entries, err := orchestrate.ApplySequential(ctx, runner, repoRoot, j.Dirs, j.hooks(resolver))
		if j.IsFullRun {
			writeJobReport(repoRoot, resolver, "apply", entries)
		}
		j.finish(entries, err)
	case "init":
		var mu sync.Mutex
		var entries []report.Entry
		err := orchestrate.InitAll(ctx, runner, repoRoot, j.Dirs, func(dir string, initErr error) {
			status, detail := report.StatusOK, "initialized"
			if initErr != nil {
				status, detail = report.StatusError, initErr.Error()
			}
			entry := report.Entry{Dir: dir, Status: status, Detail: detail}
			mu.Lock()
			entries = append(entries, entry)
			mu.Unlock()
			j.publish("status", struct {
				Dir    string `json:"dir"`
				Status string `json:"status"`
				Detail string `json:"detail"`
			}{dir, string(status), detail})
		})
		j.finish(entries, err)
	case "sync":
		var entries []report.Entry
		var failed bool
		if m := resolver.Manifest(); m != nil {
			results := manifest.Sync(ctx, m, func(name string) io.Writer {
				return dirOutputWriter{job: j, dir: name}
			})
			for _, r := range results {
				status, detail := report.StatusOK, r.Action
				if r.Err != nil {
					status, detail = report.StatusError, r.Err.Error()
					failed = true
				}
				entry := report.Entry{Dir: r.Name, Status: status, Detail: detail}
				entries = append(entries, entry)
				j.publish("status", struct {
					Dir    string `json:"dir"`
					Status string `json:"status"`
					Detail string `json:"detail"`
				}{r.Name, string(status), detail})
			}
		}
		var syncErr error
		if failed {
			syncErr = fmt.Errorf("one or more repos failed to sync")
		}
		j.finish(entries, syncErr)
	}
}

// writeJobReport updates the shared static report — the same
// .deploy-logs/report.html + report.json the CLI produces, including
// the dependency-graph tab — so a full plan/apply run from the web UI
// leaves exactly the artifact you'd get from the CLI. Best-effort:
// failures here don't fail the job itself, the live job page already
// showed the person everything that matters.
func writeJobReport(repoRoot string, resolver *manifest.Resolver, kind string, entries []report.Entry) {
	g, err := graph.Discover(repoRoot)
	if err != nil {
		return
	}
	if err := g.Sort(); err != nil {
		return
	}
	rep := report.Report{
		GeneratedAt: time.Now(),
		Command:     kind,
		Entries:     entries,
		Graph:       graph.Layout(g, resolver, repoRoot),
	}
	dir := filepath.Join(repoRoot, orchestrate.LogDir)
	report.WriteHTML(dir, rep)
	report.WriteJSON(dir, rep)
}

// loadLastReportJob reads report.json from a previous plan/apply run (the
// CLI's or an earlier server process's — writeJobReport and the CLI's own
// writeReports both write the same file) and, if one exists, wraps it as a
// finished, read-only Job. newServer seeds Server.current with this so the
// dashboard's "Last" row and job page show that run's results immediately
// on startup, instead of sitting blank until something is run through the
// web UI itself. Best-effort: no report yet, or one that fails to parse,
// just means the dashboard starts blank — never a reason to fail startup.
func loadLastReportJob(repoRoot string) *Job {
	path := filepath.Join(repoRoot, orchestrate.LogDir, "report.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rep report.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		log.Printf("ignoring %s: %v", path, err)
		return nil
	}
	if len(rep.Entries) == 0 {
		return nil
	}
	return newJobFromReport(rep)
}

// newJobFromReport builds the synthetic Job described above. Its
// broadcaster is created pre-closed — no live output was ever captured for
// this run, so a client hitting the job page's SSE stream gets an empty
// replay and an immediately-closed connection rather than one that hangs
// open waiting for frames that will never come.
func newJobFromReport(rep report.Report) *Job {
	dirs := make([]string, len(rep.Entries))
	var jobErr error
	for i, e := range rep.Entries {
		dirs[i] = e.Dir
		if e.Status == report.StatusError && jobErr == nil {
			jobErr = fmt.Errorf("one or more directories failed")
		}
	}
	j := &Job{
		ID:          newJobID(),
		Kind:        rep.Command,
		Dirs:        dirs,
		IsFullRun:   true,
		StartedAt:   rep.GeneratedAt,
		broadcaster: NewBroadcaster(gapFrame),
		finishedAt:  rep.GeneratedAt,
		entries:     rep.Entries,
		err:         jobErr,
	}
	j.broadcaster.Close()
	return j
}
