package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"homelab-deploy/internal/tf"
)

const (
	proxyAddr    = "10.1.2.3:41000"  // inside the trusted 10.0.0.0/8
	strangerAddr = "203.0.113.9:555" // outside it
)

func testAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := NewAuth(AuthConfig{
		TrustedProxies: []string{"10.0.0.0/8", "fd00::/8"},
		ViewerGroups:   []string{"deploy-viewers"},
		PlannerGroups:  []string{"deploy-planners"},
		ApplierGroups:  []string{"deploy-appliers", "admins"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// newAuthServer builds a Server that can serve every route's *auth*
// behavior without terraform on PATH: a zero tf.Runner is fine because
// none of these tests lets a request get as far as running terraform.
// "Allowed" is asserted as "got past the auth gate", i.e. a handler
// status (404 unknown job, 409 unknown dir / no manifest, 303 redirect,
// 200) rather than 401/403.
func newAuthServer(t *testing.T, a *Auth) *Server {
	t.Helper()
	s, err := newServer(t.TempDir(), &tf.Runner{}, a)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type req struct {
	method, path string
	form         url.Values
	remote       string
	user         string
	groups       string
	extraUsers   []string // additional X-authentik-username values
}

func (r req) do(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	} else {
		body = strings.NewReader("")
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	if r.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	hr.RemoteAddr = r.remote
	if r.remote == "" {
		hr.RemoteAddr = proxyAddr
	}
	if r.user != "" {
		hr.Header.Set("X-authentik-username", r.user)
	}
	for _, u := range r.extraUsers {
		hr.Header.Add("X-authentik-username", u)
	}
	if r.groups != "" {
		hr.Header.Set("X-authentik-groups", r.groups)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, hr)
	return rec
}

func runForm(kind, dir string) url.Values {
	v := url.Values{"kind": {kind}}
	if dir != "" {
		v.Set("dir", dir)
	}
	return v
}

func denied(code int) bool { return code == http.StatusUnauthorized || code == http.StatusForbidden }

func TestAuth_RoleMatrix(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	// Jobs to cancel/confirm. Never started, so nothing runs.
	s.jobs["plan1"] = newJob("plan1", "plan", nil, true)
	s.jobs["apply1"] = newJob("apply1", "apply", nil, true)

	const (
		viewer  = "deploy-viewers"
		planner = "deploy-planners"
		applier = "deploy-appliers"
	)
	cases := []struct {
		name   string
		groups string
		method string
		path   string
		form   url.Values
		allow  bool
	}{
		// viewer: read-only pages only
		{"viewer dashboard", viewer, "GET", "/", nil, true},
		{"viewer graph", viewer, "GET", "/graph", nil, true},
		{"viewer job page", viewer, "GET", "/jobs/plan1", nil, true},
		{"viewer job stream", viewer, "GET", "/jobs/nope/stream", nil, true}, // 404: past the gate, no such job
		{"viewer logs", viewer, "GET", "/logs/report.html", nil, true},       // 404: past the gate, no file
		{"viewer plan", viewer, "POST", "/run", runForm("plan", "nope"), false},
		{"viewer init", viewer, "POST", "/run", runForm("init", "nope"), false},
		{"viewer sync", viewer, "POST", "/run", runForm("sync", ""), false},
		{"viewer apply", viewer, "POST", "/run", runForm("apply", "nope"), false},
		{"viewer cancel plan", viewer, "POST", "/jobs/plan1/cancel", nil, false},
		{"viewer confirm", viewer, "POST", "/jobs/apply1/confirm", url.Values{"dir": {"a"}}, false},

		// planner: plan/init/sync, never apply or confirm
		{"planner dashboard", planner, "GET", "/", nil, true},
		{"planner plan", planner, "POST", "/run", runForm("plan", "nope"), true},
		{"planner init", planner, "POST", "/run", runForm("init", "nope"), true},
		{"planner sync", planner, "POST", "/run", runForm("sync", ""), true},
		{"planner apply", planner, "POST", "/run", runForm("apply", "nope"), false},
		{"planner apply all", planner, "POST", "/run", runForm("apply", ""), false},
		{"planner cancel plan job", planner, "POST", "/jobs/plan1/cancel", nil, true},
		{"planner cancel apply job", planner, "POST", "/jobs/apply1/cancel", nil, false},
		{"planner confirm", planner, "POST", "/jobs/apply1/confirm", url.Values{"dir": {"a"}}, false},

		// applier: everything
		{"applier dashboard", applier, "GET", "/", nil, true},
		{"applier plan", applier, "POST", "/run", runForm("plan", "nope"), true},
		{"applier apply", applier, "POST", "/run", runForm("apply", "nope"), true},
		{"applier cancel apply job", applier, "POST", "/jobs/apply1/cancel", nil, true},
		{"applier confirm", applier, "POST", "/jobs/apply1/confirm", url.Values{"dir": {"a"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := req{method: c.method, path: c.path, form: c.form, user: "alice", groups: c.groups}.do(t, s)
			if c.allow && denied(rec.Code) {
				t.Errorf("status = %d, want to get past the auth gate", rec.Code)
			}
			if !c.allow && rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestAuth_UserWithNoMappedGroupIsForbidden(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	for _, groups := range []string{"", "random-group", "deploy-viewers-extra", "DEPLOY-VIEWERS"} {
		rec := req{method: "GET", path: "/", user: "alice", groups: groups}.do(t, s)
		if rec.Code != http.StatusForbidden {
			t.Errorf("groups %q: status = %d, want 403 (exact, case-sensitive group match)", groups, rec.Code)
		}
	}
}

func TestAuth_GroupsHeaderIsPipeSeparated(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	rec := req{method: "POST", path: "/run", form: runForm("apply", "nope"), user: "alice",
		groups: "everyone|deploy-appliers|other"}.do(t, s)
	if denied(rec.Code) {
		t.Errorf("status = %d, want a user in deploy-appliers among others to get through", rec.Code)
	}
	// A comma list is NOT how Authentik sends groups; it must not be
	// split, or a group literally named "a,deploy-appliers" would grant apply.
	rec = req{method: "POST", path: "/run", form: runForm("apply", "nope"), user: "alice",
		groups: "everyone,deploy-appliers"}.do(t, s)
	if rec.Code != http.StatusForbidden {
		t.Errorf("comma-separated groups: status = %d, want 403", rec.Code)
	}
}

func TestAuth_MissingIdentityHeaderIsRejected(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	for _, r := range []req{
		{method: "GET", path: "/", groups: "deploy-appliers"},
		{method: "GET", path: "/graph", groups: "deploy-appliers"},
		{method: "POST", path: "/run", form: runForm("apply", "nope"), groups: "deploy-appliers"},
		{method: "POST", path: "/run", form: runForm("plan", "nope"), groups: "deploy-appliers"},
		{method: "POST", path: "/jobs/x/confirm", form: url.Values{"dir": {"a"}}, groups: "deploy-appliers"},
	} {
		rec := r.do(t, s)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a username header: status = %d, want 401", r.method, r.path, rec.Code)
		}
	}
}

func TestAuth_DuplicateIdentityHeaderIsRejected(t *testing.T) {
	// A proxy that appends instead of replacing leaves the client's own
	// value next to the real one; which one is real is unknowable, so
	// refuse rather than pick.
	s := newAuthServer(t, testAuth(t))
	rec := req{method: "POST", path: "/run", form: runForm("apply", "nope"),
		user: "admin", extraUsers: []string{"mallory"}, groups: "deploy-appliers"}.do(t, s)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for two username values", rec.Code)
	}
}

func TestAuth_ForgedHeadersFromUntrustedSourceAreRejected(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	for _, r := range []req{
		{method: "GET", path: "/", user: "admin", groups: "deploy-appliers", remote: strangerAddr},
		{method: "POST", path: "/run", form: runForm("apply", ""), user: "admin", groups: "deploy-appliers", remote: strangerAddr},
		{method: "POST", path: "/run", form: runForm("apply", ""), user: "admin", groups: "deploy-appliers", remote: "[2001:db8::1]:555"},
		{method: "GET", path: "/", remote: strangerAddr}, // no headers at all
	} {
		rec := r.do(t, s)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from %s: status = %d, want 403", r.method, r.path, r.remote, rec.Code)
		}
	}
}

func TestAuth_TrustedProxyMatching(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	for remote, want := range map[string]bool{
		"10.0.0.1:1":          true,
		"10.255.255.255:1":    true,
		"[::ffff:10.1.2.3]:1": true, // v4-mapped v6 must match the v4 range
		"[fd00::5]:1":         true,
		"11.0.0.1:1":          false,
		"[fe80::1]:1":         false,
		"not-an-address":      false, // unparseable RemoteAddr fails closed
		"":                    false,
	} {
		hr := httptest.NewRequest("GET", "/", nil)
		hr.RemoteAddr = remote
		hr.Header.Set("X-authentik-username", "alice")
		hr.Header.Set("X-authentik-groups", "deploy-viewers")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, hr)
		if got := rec.Code == http.StatusOK; got != want {
			t.Errorf("remote %q: allowed = %v (status %d), want %v", remote, got, rec.Code, want)
		}
	}
}

func TestAuth_KindIsAuthorizedAsExecuted(t *testing.T) {
	// r.FormValue prefers the body over the query string. The role
	// check must use the same value the run uses, so a planner cannot
	// show one kind to the gate and another to the runner.
	s := newAuthServer(t, testAuth(t))
	hr := httptest.NewRequest("POST", "/run?kind=plan", strings.NewReader(runForm("apply", "nope").Encode()))
	hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hr.RemoteAddr = proxyAddr
	hr.Header.Set("X-authentik-username", "mallory")
	hr.Header.Set("X-authentik-groups", "deploy-planners")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, hr)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body kind=apply wins over query kind=plan)", rec.Code)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestAuth_AuditLogNamesUserKindDirAndOutcome(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	buf := captureLog(t)

	// Denied apply.
	req{method: "POST", path: "/run", form: runForm("apply", "services/a"), user: "alice", groups: "deploy-planners"}.do(t, s)
	// Allowed to try, rejected by the run itself (unknown directory).
	req{method: "POST", path: "/run", form: runForm("plan", "nope"), user: "bob", groups: "deploy-planners"}.do(t, s)

	logged := buf.String()
	for _, want := range []string{
		`user="alice"`, `kind=apply`, `dir="services/a"`, `denied`,
		`user="bob"`, `kind=plan`, `dir="nope"`, `rejected`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("audit log missing %q:\n%s", want, logged)
		}
	}
}

func TestAuth_AuditLogCannotBeForgedThroughDirOrUser(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	buf := captureLog(t)
	req{method: "POST", path: "/run", form: runForm("plan", "x\n2026/01/01 job started: id=fake"),
		user: "alice", groups: "deploy-planners"}.do(t, s)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "job started: id=fake") && !strings.Contains(line, `\n`) {
			t.Errorf("a client-supplied newline started a fresh log line: %q", line)
		}
	}
}

func TestAuth_DeniedRequestsAreLoggedWithReason(t *testing.T) {
	s := newAuthServer(t, testAuth(t))
	buf := captureLog(t)
	req{method: "POST", path: "/run", form: runForm("apply", ""), user: "admin", groups: "deploy-appliers", remote: strangerAddr}.do(t, s)
	req{method: "POST", path: "/run", form: runForm("apply", ""), groups: "deploy-appliers"}.do(t, s)
	logged := buf.String()
	for _, want := range []string{"untrusted source", strangerAddr, "missing identity"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
}

func TestNewServer_RequiresAnAuthChoice(t *testing.T) {
	if _, err := newServer(t.TempDir(), &tf.Runner{}, nil); err == nil {
		t.Fatal("newServer with nil auth succeeded; want an error so a forgotten setting cannot serve open")
	}
}

func TestNewAuth_FailsClosedOnIncompleteOrUnsafeConfig(t *testing.T) {
	ok := AuthConfig{
		TrustedProxies: []string{"10.0.0.0/8"},
		ApplierGroups:  []string{"a"},
	}
	if _, err := NewAuth(ok); err != nil {
		t.Fatalf("baseline config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*AuthConfig){
		"no trusted proxies":       func(c *AuthConfig) { c.TrustedProxies = nil },
		"blank trusted proxy":      func(c *AuthConfig) { c.TrustedProxies = []string{" "} },
		"garbage trusted proxy":    func(c *AuthConfig) { c.TrustedProxies = []string{"traefik"} },
		"trust-everyone v4":        func(c *AuthConfig) { c.TrustedProxies = []string{"0.0.0.0/0"} },
		"trust-everyone v6":        func(c *AuthConfig) { c.TrustedProxies = []string{"::/0"} },
		"no groups mapped":         func(c *AuthConfig) { c.ApplierGroups = nil },
		"only blank groups mapped": func(c *AuthConfig) { c.ApplierGroups = []string{"", " "} },
	} {
		c := ok
		mutate(&c)
		if _, err := NewAuth(c); err == nil {
			t.Errorf("%s: NewAuth succeeded; want an error (fail closed)", name)
		}
	}
}

func TestNewAuth_AcceptsBareIPs(t *testing.T) {
	a, err := NewAuth(AuthConfig{TrustedProxies: []string{"172.18.0.5", "::1"}, ViewerGroups: []string{"v"}})
	if err != nil {
		t.Fatal(err)
	}
	s := newAuthServer(t, a)
	hr := httptest.NewRequest("GET", "/", nil)
	hr.RemoteAddr = "172.18.0.5:9"
	hr.Header.Set("X-authentik-username", "alice")
	hr.Header.Set("X-authentik-groups", "v")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, hr)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 from the single trusted IP", rec.Code)
	}
}

func TestAuthFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	good := map[string]string{
		"HOMELAB_DEPLOY_TRUSTED_PROXIES": "10.0.0.0/8, 192.168.1.5",
		"HOMELAB_DEPLOY_VIEWER_GROUPS":   "v1,v2",
		"HOMELAB_DEPLOY_PLANNER_GROUPS":  "p1",
		"HOMELAB_DEPLOY_APPLIER_GROUPS":  "a1, a2",
	}

	a, err := AuthFromEnv(env(good), false)
	if err != nil {
		t.Fatal(err)
	}
	s := newAuthServer(t, a)
	rec := req{method: "POST", path: "/run", form: runForm("apply", "nope"), user: "alice", groups: "a2"}.do(t, s)
	if denied(rec.Code) {
		t.Errorf("applier group a2 from env: status = %d, want to get through", rec.Code)
	}
	rec = req{method: "POST", path: "/run", form: runForm("apply", "nope"), user: "alice", groups: "p1"}.do(t, s)
	if rec.Code != http.StatusForbidden {
		t.Errorf("planner group p1 from env applying: status = %d, want 403", rec.Code)
	}

	// Nothing set at all: refuse to start rather than serve open.
	if _, err := AuthFromEnv(env(nil), false); err == nil {
		t.Error("AuthFromEnv with no config succeeded; want an error naming the missing settings")
	} else if !strings.Contains(err.Error(), "HOMELAB_DEPLOY_TRUSTED_PROXIES") || !strings.Contains(err.Error(), "--insecure-no-auth") {
		t.Errorf("error should tell the operator what to set: %v", err)
	}

	// Groups without proxies, and proxies without groups: both refuse.
	for _, drop := range []string{"HOMELAB_DEPLOY_TRUSTED_PROXIES", "HOMELAB_DEPLOY_APPLIER_GROUPS"} {
		m := map[string]string{}
		for k, v := range good {
			if k != drop && k != "HOMELAB_DEPLOY_VIEWER_GROUPS" && k != "HOMELAB_DEPLOY_PLANNER_GROUPS" {
				m[k] = v
			}
		}
		if _, err := AuthFromEnv(env(m), false); err == nil {
			t.Errorf("config missing %s was accepted", drop)
		}
	}
}

func TestAuthFromEnv_InsecureNoAuth(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	a, err := AuthFromEnv(env(nil), true)
	if err != nil {
		t.Fatal(err)
	}
	s := newAuthServer(t, a)
	// Explicit opt-out: no headers, any source, full access (the old behavior).
	rec := req{method: "POST", path: "/run", form: runForm("apply", "nope"), remote: strangerAddr}.do(t, s)
	if denied(rec.Code) {
		t.Errorf("--insecure-no-auth: status = %d, want requests let through", rec.Code)
	}

	// Setting both is ambiguous: refuse, so a leftover flag cannot
	// silently disable auth that the operator configured.
	_, err = AuthFromEnv(env(map[string]string{"HOMELAB_DEPLOY_TRUSTED_PROXIES": "10.0.0.0/8"}), true)
	if err == nil {
		t.Error("--insecure-no-auth together with auth settings was accepted; want an error")
	}
}
