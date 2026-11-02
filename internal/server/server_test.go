package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"homelab-deploy/internal/manifest"
	"homelab-deploy/internal/orchestrate"
	"homelab-deploy/internal/report"
	"homelab-deploy/internal/tf"
)

func requireTerraform(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not found on PATH — skipping")
	}
}

// buildFakeRepo creates n service directories, each with a distinct
// terraform_data resource, terraform-inits them, and works around the
// known builtin-provider-only IsInitialized gap (terraform_data creates
// no .terraform/) so the server's IsInitialized checks pass — not what
// any of these tests are actually exercising.
func buildFakeRepo(t *testing.T, n int) (repoRoot string, dirs []string) {
	t.Helper()
	repoRoot = t.TempDir()
	runner, err := tf.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for i := 0; i < n; i++ {
		dir := filepath.Join("services", "svc"+string(rune('a'+i)))
		full := filepath.Join(repoRoot, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := `resource "terraform_data" "example" { input = "` + dir + `" }`
		if err := os.WriteFile(filepath.Join(full, "main.tf"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runner.Init(ctx, repoRoot, dir); err != nil {
			t.Fatalf("init %s: %v", dir, err)
		}
		if err := os.MkdirAll(filepath.Join(full, ".terraform"), 0o755); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	return repoRoot, dirs
}

// buildSlowRepo creates one service directory whose terraform_data
// resource has a local-exec provisioner (built into terraform core — no
// plugin/network fetch needed) that sleeps, giving cancellation tests a
// real, actually-running `terraform apply` to interrupt, rather than one
// that would finish before a cancel request could ever land.
func buildSlowRepo(t *testing.T, sleepSeconds int) (repoRoot, dir string) {
	t.Helper()
	repoRoot = t.TempDir()
	runner, err := tf.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join("services", "slow")
	full := filepath.Join(repoRoot, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `resource "terraform_data" "example" {
  input = "slow"
  provisioner "local-exec" {
    command = "sleep ` + strconv.Itoa(sleepSeconds) + `"
  }
}`
	if err := os.WriteFile(filepath.Join(full, "main.tf"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runner.Init(context.Background(), repoRoot, dir); err != nil {
		t.Fatalf("init %s: %v", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(full, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repoRoot, dir
}

func newTestServer(t *testing.T, repoRoot string) *httptest.Server {
	t.Helper()
	runner, err := tf.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(repoRoot, runner, &Auth{disabled: true}) // these tests exercise behavior, not auth: see auth_test.go
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHandleDashboard_ListsDirectories(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 2)
	ts := newTestServer(t, repoRoot)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readAll(t, resp)
	for _, d := range dirs {
		if !strings.Contains(body, d) {
			t.Errorf("dashboard missing directory %q", d)
		}
	}
}

// TestHandleGraph_ShowsDependencyGraphWithoutAnyPlan is the regression
// test for the whole point of the /graph route: the dependency graph
// must render straight from the discovered directories, with no plan or
// apply ever having run — buildFakeRepo here only inits each directory,
// it never plans or applies anything.
func TestHandleGraph_ShowsDependencyGraphWithoutAnyPlan(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 2)
	ts := newTestServer(t, repoRoot)

	resp, err := http.Get(ts.URL + "/graph")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "<svg") {
		t.Errorf("graph page missing <svg>: %s", body)
	}
	for _, d := range dirs {
		if !strings.Contains(body, d) {
			t.Errorf("graph page missing directory %q", d)
		}
	}
	if strings.Contains(body, "No dependency graph available") {
		t.Errorf("graph page shows the empty state even though directories exist")
	}
}

func TestHandleDashboard_LinksToGraphPage(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	if !strings.Contains(body, `href="/graph"`) {
		t.Errorf("dashboard missing a link to /graph: %s", body)
	}
}

func TestHandleStartRun_UnknownDirectoryRejected(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)

	resp, err := http.PostForm(ts.URL+"/run", urlValues("kind", "plan", "dir", "services/does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 for an unknown directory", resp.StatusCode)
	}
}

func TestHandleStartRun_InvalidKindRejected(t *testing.T) {
	repoRoot := t.TempDir()
	ts := newTestServer(t, repoRoot)

	resp, err := http.PostForm(ts.URL+"/run", urlValues("kind", "destroy-everything"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid kind", resp.StatusCode)
	}
}

func TestHandleStartRun_RedirectsToDashboardWithJobIdHeader(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	// Redirects to the dashboard, not the job page — starting a run no
	// longer navigates away. The job id is still available via a
	// response header for any programmatic caller that wants it.
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want / (stay on the dashboard)", loc)
	}
	if id := resp.Header.Get("X-Job-Id"); id == "" {
		t.Error("expected an X-Job-Id header on the redirect")
	}
}

func TestHandleStartRun_SecondJobRejectedWhileOneRunning(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	resp1, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first job: status = %d, want 303", resp1.StatusCode)
	}

	// Immediately try a second one — the first is (very likely) still
	// running given a real terraform plan takes at least a few ms.
	resp2, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second job: status = %d, want 409 (a job was already running)", resp2.StatusCode)
	}
}

func TestJobStream_DeliversRealOutputAndCompletes(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp.Header.Get("X-Job-Id")
	resp.Body.Close()
	if jobID == "" {
		t.Fatal("expected an X-Job-Id header")
	}

	streamResp, err := http.Get(ts.URL + "/jobs/" + jobID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()

	var sawOutput, sawStatus, sawJobDone bool
	sc := bufio.NewScanner(streamResp.Body)
	deadline := time.Now().Add(10 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: output"):
			sawOutput = true
		case strings.HasPrefix(line, "event: status"):
			sawStatus = true
		case strings.HasPrefix(line, "event: job_done"):
			sawJobDone = true
		}
		if sawOutput && sawStatus && sawJobDone {
			break
		}
	}

	if !sawOutput {
		t.Error("never saw an 'output' SSE event")
	}
	if !sawStatus {
		t.Error("never saw a 'status' SSE event")
	}
	if !sawJobDone {
		t.Error("never saw a 'job_done' SSE event")
	}
}

func TestJobStream_LateSubscriberGetsReplay(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp.Header.Get("X-Job-Id")
	resp.Body.Close()
	if jobID == "" {
		t.Fatal("expected an X-Job-Id header")
	}

	// Give the job a moment to actually finish before subscribing —
	// this is the "refreshed the page after it was already done" case.
	time.Sleep(1500 * time.Millisecond)

	streamResp, err := http.Get(ts.URL + "/jobs/" + jobID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()

	body := readAll(t, streamResp)
	if !strings.Contains(body, "event: job_done") {
		t.Error("a subscriber joining after the job finished should still get the replayed job_done event")
	}
}

func TestHandleJobPage_UnknownIDIs404(t *testing.T) {
	repoRoot := t.TempDir()
	ts := newTestServer(t, repoRoot)

	resp, err := http.Get(ts.URL + "/jobs/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}

func urlValues(kv ...string) map[string][]string {
	v := map[string][]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		v[kv[i]] = []string{kv[i+1]}
	}
	return v
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestHandleDashboard_ButtonsReenableAfterJobFinishes is a regression
// test for a real bug: the dashboard template was gating "disable the
// buttons" / "show the already-running banner" on whether ANY job had
// ever run (Server.current being non-nil), not on whether one was
// actually still running — Server.current is never reset to nil after
// a job finishes (it doubles as "last job" for the dashboard's link
// back to it), so every button stayed permanently disabled after the
// very first job, forever, regardless of outcome.
func TestHandleDashboard_ButtonsReenableAfterJobFinishes(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("starting the job: status = %d, want 303", resp.StatusCode)
	}

	// Wait for the job to actually finish (a single-directory plan
	// against terraform_data is fast) rather than racing it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dashResp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, dashResp)
		dashResp.Body.Close()
		if !strings.Contains(body, `id="progress-notice"`) {
			// Job has finished — this is the assertion that matters:
			// buttons must not carry the disabled attribute anymore.
			if strings.Contains(body, "disabled") {
				t.Errorf("dashboard still has disabled buttons after the job finished:\n%s", body)
			}
			if !strings.Contains(body, "/jobs/") {
				t.Error("expected a link back to the finished job (the \"Last job\" link)")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("job never finished within the timeout")
}

// TestHandleDashboard_ShowsLastPlanResultsInline verifies the dashboard
// embeds the actual plan results (status, change counts, and the
// attribute-level diff) after a job finishes, not just a link out to a
// separate report page.
func TestHandleDashboard_ShowsLastPlanResultsInline(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 1)
	dir := dirs[0]
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	// Change the resource so the next plan shows a real "update" with a
	// real attribute diff, not just a fresh "create".
	full := filepath.Join(repoRoot, dir, "main.tf")
	applyForm := urlValues("kind", "apply")
	resp, err := client.PostForm(ts.URL+"/run", applyForm)
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp.Header.Get("X-Job-Id")
	// This is a fresh resource's first apply — a real "create" change,
	// which now pauses for confirmation before the new plan-then-confirm
	// gate lets it actually apply.
	confirmPendingApply(t, ts, jobID)
	waitForJob(t, ts, resp)

	if err := os.WriteFile(full, []byte(`resource "terraform_data" "example" { input = "changed" }`), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err = client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, ts, resp)

	dashResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer dashResp.Body.Close()
	body := readAll(t, dashResp)

	for _, want := range []string{"badge-CHANGES", dir, "input", "changed"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q in the inline results:\n%s", want, body)
		}
	}
}

// waitForJob polls the dashboard until the job started by resp (a 303
// redirect to /jobs/{id}) is no longer running, or fails the test after
// a timeout.
func waitForJob(t *testing.T, ts *httptest.Server, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("starting job: status = %d, want 303", resp.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dashResp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, dashResp)
		dashResp.Body.Close()
		if !strings.Contains(body, `id="progress-notice"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("job never finished within the timeout")
}

// confirmPendingApply polls jobID's own page until it renders a directory
// awaiting confirmation (the new plan-then-confirm gate an apply job with
// real changes now always pauses at), then POSTs the confirm form for that
// directory so the apply can actually proceed. Needed by any test that
// starts an apply against a directory with real changes, now that doing so
// no longer completes unattended.
func confirmPendingApply(t *testing.T, ts *httptest.Server, jobID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/jobs/" + jobID)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		resp.Body.Close()
		// Not "badge-awaiting" alone — that's also the CSS class name,
		// present in every page's <style> block from the very first
		// byte. The confirm box itself renders with no "hidden"
		// attribute only once a directory is genuinely paused; find its
		// own dir field after that point, in case another directory's
		// (still-hidden) confirm box appears earlier in the page.
		if boxIdx := strings.Index(body, `<div class="confirm-box">`); boxIdx != -1 {
			const marker = `name="dir" value="`
			idx := strings.Index(body[boxIdx:], marker)
			if idx == -1 {
				t.Fatal("found an unhidden confirm-box but no confirm form dir field in it")
			}
			rest := body[boxIdx+idx+len(marker):]
			dir := rest[:strings.Index(rest, `"`)]
			confirmResp, err := http.PostForm(ts.URL+"/jobs/"+jobID+"/confirm", urlValues("dir", dir))
			if err != nil {
				t.Fatal(err)
			}
			confirmResp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("apply never reached an awaiting-confirmation state within the timeout")
}

// TestHandleDashboard_DependsOnColumnRemoved confirms the "Depends on"
// column is gone — dependency info now lives only in the static
// report's graph tab, not duplicated here.
func TestHandleDashboard_DependsOnColumnRemoved(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	if strings.Contains(body, "Depends on") {
		t.Error("dashboard should no longer show a \"Depends on\" column")
	}
}

// TestHandleDashboard_RowsCarryStatusClassForFiltering verifies each
// directory row is tagged with the CSS class the status filter depends
// on: status-OK/CHANGES/ERROR for a real result, status-NONE for a
// directory the last job didn't cover.
func TestHandleDashboard_RowsCarryStatusClassForFiltering(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 2)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	// Plan only the first directory, leaving the second with no result.
	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan", "dir", dirs[0]))
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, ts, resp)

	dashResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer dashResp.Body.Close()
	body := readAll(t, dashResp)

	if !strings.Contains(body, `class="status-row status-CHANGES"`) {
		t.Errorf("expected the planned directory's row to carry status-CHANGES:\n%s", body)
	}
	if !strings.Contains(body, `class="status-row status-NONE"`) {
		t.Errorf("expected the un-planned directory's row to carry status-NONE:\n%s", body)
	}
	for _, id := range []string{"filter-all", "filter-OK", "filter-CHANGES", "filter-ERROR", "filter-NONE"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("missing status filter radio %q", id)
		}
	}
}

// TestHandleDashboard_StaysOnDashboardWithProgressBar verifies clicking
// Plan doesn't navigate away to the job page — a normal (redirect-
// following) client posting the form ends up back on the dashboard,
// which shows a progress notice with a link to the job page for detail,
// not the job page itself.
func TestHandleDashboard_StaysOnDashboardWithProgressBar(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)

	// Default client: follows the 303 redirect automatically, exactly
	// like a browser submitting the form would.
	resp, err := http.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Request.URL.Path != "/" {
		t.Errorf("ended up on %q after submitting the form, want to stay on the dashboard (/)", resp.Request.URL.Path)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, `id="progress-notice"`) {
		t.Error("expected a progress notice on the dashboard while the job runs")
	}
	if !strings.Contains(body, `id="progress-fill"`) {
		t.Error("expected a progress bar element")
	}
	if !strings.Contains(body, "view live output") {
		t.Error("expected a link to the job page for full detail")
	}
}

// TestJobStream_ReportExistsBeforeJobDoneFires is a regression test for
// a real race: writeJobReport used to run AFTER finish() published the
// job_done SSE event, so a client watching the stream (or, as it
// happened, a test's own t.TempDir() cleanup racing the still-running
// report write) could observe "done" before report.html/report.json
// actually existed on disk. Asserts the actual invariant directly —
// the moment job_done arrives over SSE, the report file must already
// be there — rather than relying on incidental timing to catch it.
func TestJobStream_ReportExistsBeforeJobDoneFires(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp.Header.Get("X-Job-Id")
	resp.Body.Close()
	if jobID == "" {
		t.Fatal("expected an X-Job-Id header")
	}

	streamResp, err := http.Get(ts.URL + "/jobs/" + jobID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()

	sc := bufio.NewScanner(streamResp.Body)
	deadline := time.Now().Add(10 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		if strings.HasPrefix(sc.Text(), "event: job_done") {
			// The instant job_done arrives — not "a moment later" — the
			// report must already exist.
			reportPath := filepath.Join(repoRoot, orchestrate.LogDir, "report.html")
			if _, err := os.Stat(reportPath); err != nil {
				t.Fatalf("report.html doesn't exist yet when job_done fired: %v", err)
			}
			return
		}
	}
	t.Fatal("never saw a job_done event")
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH — skipping")
	}
}

// newMultiRepoServer builds a real bare git repo, a manifest pointing
// at it, and a test server rooted at the manifest's directory — for
// exercising the "sync" job kind against real git, the same way
// buildFakeRepo exercises plan/apply against real terraform.
func newMultiRepoServer(t *testing.T) (ts *httptest.Server, repoRoot string) {
	t.Helper()
	requireGit(t)

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitT(t, "", "init", "--quiet", "--bare", "--initial-branch=main", remote)

	work := t.TempDir()
	runGitT(t, work, "init", "--quiet", "--initial-branch=main")
	runGitT(t, work, "config", "user.email", "test@example.com")
	runGitT(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "main.tf"), []byte(`resource "terraform_data" "x" {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, work, "add", ".")
	runGitT(t, work, "commit", "--quiet", "-m", "initial")
	runGitT(t, work, "remote", "add", "origin", remote)
	runGitT(t, work, "push", "--quiet", "origin", "main")

	repoRoot = t.TempDir()
	manifestContent := "vault=file://" + remote + "\n"
	if err := os.WriteFile(filepath.Join(repoRoot, ".deploy-repos"), []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}

	return newTestServer(t, repoRoot), repoRoot
}

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	fullArgs := args
	if dir != "" {
		fullArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestHandleStartRun_SyncClonesRealRepo(t *testing.T) {
	ts, repoRoot := newMultiRepoServer(t)
	client := noRedirectClient()

	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "sync"))
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, ts, resp)

	if _, err := os.Stat(filepath.Join(repoRoot, manifest.CheckoutsDirName, "vault", "main.tf")); err != nil {
		t.Fatalf("expected vault to be cloned into the checkouts subfolder, got: %v", err)
	}
}

func TestHandleStartRun_SyncRejectedInSingleRepoMode(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1) // no .deploy-repos here
	ts := newTestServer(t, repoRoot)

	resp, err := http.PostForm(ts.URL+"/run", urlValues("kind", "sync"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 (no manifest, sync doesn't apply)", resp.StatusCode)
	}
}

func TestHandleStartRun_SyncRejectsDirParam(t *testing.T) {
	ts, _ := newMultiRepoServer(t)

	resp, err := http.PostForm(ts.URL+"/run", urlValues("kind", "sync", "dir", "vault"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (sync has no per-repo targeting)", resp.StatusCode)
	}
}

func TestHandleDashboard_SyncButtonOnlyInMultiRepoMode(t *testing.T) {
	ts, _ := newMultiRepoServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	if !strings.Contains(body, `value="sync"`) {
		t.Error("expected a Sync button in multi-repo mode")
	}
}

func TestHandleDashboard_NoSyncButtonInSingleRepoMode(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildFakeRepo(t, 1)
	ts := newTestServer(t, repoRoot)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	if strings.Contains(body, `value="sync"`) {
		t.Error("should not show a Sync button in single-repo mode — there's nothing to sync")
	}
}

// TestDashboardTmpl_RendersWarnings executes dashboardTmpl directly
// against a synthetic dashboardData rather than going through a real
// terraform run — terraform's own diagnostics engine doesn't emit a
// genuine "Warning:" for any of this project's trivial fixture
// resources, so this pins down the template's rendering of
// Entry.Warnings without needing one.
// TestHandleCancelJob_StopsARunningApplyAndFreesTheJobSlot is an
// end-to-end regression test for real cancellation: it starts an apply
// against a directory whose local-exec provisioner sleeps for 8 seconds,
// waits for actual output (proof terraform is genuinely running, not
// just queued), cancels it, and asserts the job_done event reports
// canceled:true well before the 8-second sleep could have finished on
// its own — i.e. the context cancellation actually killed the terraform
// process rather than merely marking the job as canceled in our own
// bookkeeping while the real command kept running underneath. Also
// verifies the job slot is freed immediately after, since a job stuck
// forever with no way to cancel was the whole point of adding this.
func TestHandleCancelJob_StopsARunningApplyAndFreesTheJobSlot(t *testing.T) {
	requireTerraform(t)
	repoRoot, _ := buildSlowRepo(t, 8)
	ts := newTestServer(t, repoRoot)
	client := noRedirectClient()

	resp, err := client.PostForm(ts.URL+"/run", urlValues("kind", "apply"))
	if err != nil {
		t.Fatal(err)
	}
	jobID := resp.Header.Get("X-Job-Id")
	resp.Body.Close()
	if jobID == "" {
		t.Fatal("expected an X-Job-Id header")
	}

	streamResp, err := http.Get(ts.URL + "/jobs/" + jobID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	sc := bufio.NewScanner(streamResp.Body)

	// The directory's own plan runs first (fast — the local-exec
	// provisioner only fires on apply, never on plan) and, since
	// creating the resource is a real change, now pauses for
	// confirmation before ever starting the slow apply.
	var pendingDir string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: awaiting_confirmation") {
			if sc.Scan() {
				var d struct {
					Dir string `json:"dir"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &d); err != nil {
					t.Fatal(err)
				}
				pendingDir = d.Dir
			}
			break
		}
	}
	if pendingDir == "" {
		t.Fatal("never saw an awaiting_confirmation event — can't confirm the apply to let the slow provisioner actually start")
	}

	confirmResp, err := client.PostForm(ts.URL+"/jobs/"+jobID+"/confirm", urlValues("dir", pendingDir))
	if err != nil {
		t.Fatal(err)
	}
	confirmResp.Body.Close()

	sawOutput := false
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: output") {
			sawOutput = true
			break
		}
	}
	if !sawOutput {
		t.Fatal("never saw any output after confirming — can't prove cancellation interrupted a running command")
	}

	start := time.Now()
	cancelResp, err := client.PostForm(ts.URL+"/jobs/"+jobID+"/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancel status = %d, want 303", cancelResp.StatusCode)
	}

	var dataLine string
	var sawJobDone bool
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: job_done") {
			sawJobDone = true
			if sc.Scan() {
				dataLine = sc.Text()
			}
			break
		}
	}
	elapsed := time.Since(start)
	if !sawJobDone {
		t.Fatal("never saw a job_done event after canceling")
	}
	if !strings.Contains(dataLine, `"canceled":true`) {
		t.Errorf("job_done data should report canceled:true, got: %s", dataLine)
	}
	if elapsed > 5*time.Second {
		t.Errorf("cancellation took %v to take effect — the 8s sleep doesn't seem to have actually been interrupted", elapsed)
	}

	// The job slot must be free again immediately.
	resp2, err := client.PostForm(ts.URL+"/run", urlValues("kind", "plan"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("starting a new job right after cancel: status = %d, want 303 (job slot should be free)", resp2.StatusCode)
	}
}

// TestHandleCancelJob_UnknownIDIs404 mirrors handleJobPage's own
// behavior for a bogus id, for consistency between the two /jobs/{id}...
// routes.
func TestHandleCancelJob_UnknownIDIs404(t *testing.T) {
	repoRoot := t.TempDir()
	ts := newTestServer(t, repoRoot)

	resp, err := http.PostForm(ts.URL+"/jobs/does-not-exist/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestNewServer_SeedsLastFromExistingReport is the regression test for
// `serve` picking up a report.json already on disk (written by a prior
// `homelab-deploy plan`/`apply` CLI run, or an earlier server process)
// instead of starting with a blank "Last" every restart.
func TestNewServer_SeedsLastFromExistingReport(t *testing.T) {
	requireTerraform(t)
	repoRoot, dirs := buildFakeRepo(t, 1)
	dir := dirs[0]

	rep := report.Report{
		GeneratedAt: time.Now().Add(-time.Hour),
		Command:     "plan",
		Entries: []report.Entry{
			{Dir: dir, Status: report.StatusChanges, Detail: "1 to add, 0 to change, 0 to destroy"},
		},
	}
	logDir := filepath.Join(repoRoot, orchestrate.LogDir)
	if _, err := report.WriteJSON(logDir, rep); err != nil {
		t.Fatal(err)
	}

	ts := newTestServer(t, repoRoot)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	for _, want := range []string{"badge-CHANGES", dir, "Last job: plan", "(done)"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q after seeding from an existing report.json:\n%s", want, body)
		}
	}

	// Buttons must stay enabled — a seeded "Last" is not a running job.
	if strings.Contains(body, `id="progress-notice"`) {
		t.Error("dashboard shows a running-job progress bar for a job seeded from disk, not actually running")
	}

	// The "view it" link's job page must also render, and its SSE stream
	// must end immediately rather than hang (no live output was ever
	// captured for a run that predates this process).
	const marker = `/jobs/`
	idx := strings.Index(body, marker)
	if idx == -1 {
		t.Fatal(`dashboard missing a "view it" link to the seeded job`)
	}
	rest := body[idx+len(marker):]
	id := rest[:strings.IndexAny(rest, `"'`)]

	jobResp, err := http.Get(ts.URL + "/jobs/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer jobResp.Body.Close()
	if jobResp.StatusCode != http.StatusOK {
		t.Fatalf("job page status = %d, want 200", jobResp.StatusCode)
	}

	streamResp, err := http.Get(ts.URL + "/jobs/" + id + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	streamResp.Body.Close() // must not hang even if not explicitly drained
}

func TestDashboardTmpl_RendersWarnings(t *testing.T) {
	data := dashboardData{
		Dirs: []string{"services/a"},
		LastEntries: map[string]report.Entry{
			"services/a": {
				Dir: "services/a", Status: report.StatusChanges, Detail: "1 to add",
				Warnings: []tf.Warning{
					{Summary: "Argument is deprecated", Detail: "line 1 of detail\nline 2 of detail"},
					{Summary: `<script>alert(1)</script>`, Detail: `<script>alert(2)</script>`},
				},
			},
		},
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if !strings.Contains(html, "Argument is deprecated") {
		t.Error("dashboard missing the warning summary text")
	}
	if !strings.Contains(html, "line 1 of detail") || !strings.Contains(html, "line 2 of detail") {
		t.Error("dashboard missing the multi-line warning detail")
	}
	if !strings.Contains(html, `class="warnings"`) || !strings.Contains(html, `class="warning-detail"`) {
		t.Error("dashboard missing the warnings/warning-detail markup")
	}
	if strings.Contains(html, "<script>alert(1)</script>") || strings.Contains(html, "<script>alert(2)</script>") {
		t.Error("unescaped <script> tag made it into a rendered warning")
	}
}
