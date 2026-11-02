package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"homelab-deploy/internal/tf"
)

const publicURL = "http://127.0.0.1:8080" // never listened on: tests call the handler directly

type harness struct {
	t     *testing.T
	clock *testClock
	op    *fakeOP
	auth  *Auth
	srv   *Server
}

func baseConfig(op *fakeOP, clock *testClock) AuthConfig {
	return AuthConfig{
		Issuer: op.URL(), ClientID: "homelab", ClientSecret: "s3cret", PublicURL: publicURL,
		ViewerGroups:  []string{"deploy-viewers"},
		PlannerGroups: []string{"deploy-planners"},
		ApplierGroups: []string{"deploy-appliers", "admins"},
		Now:           clock.Now,
	}
}

// newHarness wires a real Auth to a fake provider. A zero tf.Runner is
// fine: no test here lets a request reach terraform. "Allowed" means
// "got past the auth gate", i.e. a handler status (404 unknown job, 409
// unknown dir / no manifest, 303 redirect, 200) rather than 401/403.
func newHarness(t *testing.T, mutate func(*AuthConfig)) *harness {
	t.Helper()
	clock := newTestClock()
	op := newFakeOP(t, clock, "homelab", "s3cret")
	cfg := baseConfig(op, clock)
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := NewAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t.TempDir(), &tf.Runner{}, a)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, clock: clock, op: op, auth: a, srv: s}
}

type req struct {
	method, path string
	form         url.Values
	cookies      []*http.Cookie
	header       map[string]string
	noOrigin     bool // POSTs get the right Origin unless this is set
}

func (h *harness) do(r req) *httptest.ResponseRecorder {
	h.t.Helper()
	body := strings.NewReader("")
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	hr.RemoteAddr = "192.0.2.1:1234"
	if r.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	if !safeMethod(r.method) && !r.noOrigin && hr.Header.Get("Origin") == "" {
		hr.Header.Set("Origin", publicURL)
	}
	for _, c := range r.cookies {
		hr.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, hr)
	return rec
}

// sessionFor plants a session directly, for tests about what a role may
// do (the login flow itself is tested separately).
func (h *harness) sessionFor(user string, role Role) *http.Cookie {
	tok := randToken()
	h.auth.sessions.put(tok, Identity{User: user, Subject: "sub-" + user, Role: role}, time.Hour)
	return &http.Cookie{Name: h.auth.cookieName(sessionCookie), Value: tok}
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			found = c
		}
	}
	return found
}

type loginResult struct {
	rec     *httptest.ResponseRecorder
	session *http.Cookie // nil unless a session was issued
}

// startLogin performs GET /auth/login and returns the provider URL the
// browser is sent to, the state and the login cookie.
func (h *harness) startLogin(next string) (authURL, state string, loginC *http.Cookie) {
	h.t.Helper()
	path := "/auth/login"
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	rec := h.do(req{method: "GET", path: path})
	if rec.Code != http.StatusFound {
		h.t.Fatalf("GET %s = %d, want 302", path, rec.Code)
	}
	authURL = rec.Header().Get("Location")
	u, err := url.Parse(authURL)
	if err != nil {
		h.t.Fatal(err)
	}
	return authURL, u.Query().Get("state"), cookieNamed(rec, h.auth.cookieName(loginCookie))
}

func (h *harness) callback(code, state string, cookies ...*http.Cookie) loginResult {
	h.t.Helper()
	rec := h.do(req{method: "GET", path: callbackPath + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state), cookies: cookies})
	r := loginResult{rec: rec}
	if c := cookieNamed(rec, h.auth.cookieName(sessionCookie)); c != nil && c.Value != "" && c.MaxAge >= 0 {
		r.session = c
	}
	return r
}

// login runs the whole flow: /auth/login, the provider approving with
// (optionally tampered) claims, then the callback.
func (h *harness) login(next string, tamper func(map[string]any), sign string) loginResult {
	h.t.Helper()
	authURL, state, lc := h.startLogin(next)
	code := h.op.approve(authURL, tamper, sign)
	return h.callback(code, state, lc)
}

func denied(code int) bool { return code == http.StatusUnauthorized || code == http.StatusForbidden }

func runForm(kind, dir string) url.Values {
	v := url.Values{"kind": {kind}}
	if dir != "" {
		v.Set("dir", dir)
	}
	return v
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// ---- the login flow -------------------------------------------------

func TestLogin_HappyPath(t *testing.T) {
	h := newHarness(t, nil)

	// Unauthenticated browser navigation is sent to log in, remembering where it was going.
	rec := h.do(req{method: "GET", path: "/graph", header: map[string]string{"Accept": "text/html"}})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=%2Fgraph" {
		t.Fatalf("got %d -> %q, want 302 -> /auth/login?next=%%2Fgraph", rec.Code, rec.Header().Get("Location"))
	}

	authURL, state, lc := h.startLogin("/graph")
	u, _ := url.Parse(authURL)
	q := u.Query()
	if !strings.HasPrefix(authURL, h.op.URL()+"/authorize?") {
		t.Errorf("redirected to %q, want the discovered authorization endpoint", authURL)
	}
	for k, want := range map[string]string{
		"response_type": "code", "client_id": "homelab", "redirect_uri": publicURL + "/auth/callback",
		"code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			t.Errorf("authorize param %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		t.Errorf("scope %q lacks openid", q.Get("scope"))
	}
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		t.Errorf("state/nonce/code_challenge must all be set: %v", q)
	}
	if lc == nil || lc.Value != state || !lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode {
		t.Errorf("login cookie = %+v, want HttpOnly SameSite=Lax holding the state", lc)
	}

	code := h.op.approve(authURL, nil, "")
	res := h.callback(code, state, lc)
	if res.rec.Code != http.StatusSeeOther || res.rec.Header().Get("Location") != "/graph" {
		t.Fatalf("callback = %d -> %q, want 303 -> /graph (body %q)", res.rec.Code, res.rec.Header().Get("Location"), res.rec.Body.String())
	}
	if res.session == nil || !res.session.HttpOnly || res.session.SameSite != http.SameSiteLaxMode || res.session.Secure {
		t.Fatalf("session cookie = %+v, want HttpOnly SameSite=Lax, not Secure on a loopback http URL", res.session)
	}

	if rec := h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{res.session}}); denied(rec.Code) {
		t.Errorf("logged-in GET /graph = %d, want it served", rec.Code)
	}
	if rec := h.do(req{method: "POST", path: "/run", form: runForm("apply", "nope"), cookies: []*http.Cookie{res.session}}); denied(rec.Code) {
		t.Errorf("applier POST /run apply = %d, want past the gate", rec.Code)
	}
}

func TestLogin_SessionCookieIsSecureOnHTTPS(t *testing.T) {
	h := newHarness(t, func(c *AuthConfig) { c.PublicURL = "https://deploy.example.com" })
	authURL, state, lc := h.startLogin("")
	if !lc.Secure {
		t.Error("login cookie not Secure with an https public URL")
	}
	res := h.callback(h.op.approve(authURL, nil, ""), state, lc)
	if res.session == nil || !res.session.Secure {
		t.Errorf("session cookie = %+v, want Secure with an https public URL", res.session)
	}
}

func TestLogin_PublicClientWithoutSecret(t *testing.T) {
	clock := newTestClock()
	op := newFakeOP(t, clock, "homelab", "") // provider requires no secret, PKCE only
	cfg := baseConfig(op, clock)
	cfg.ClientSecret = ""
	a, err := NewAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(t.TempDir(), &tf.Runner{}, a)
	h := &harness{t: t, clock: clock, op: op, auth: a, srv: s}
	if res := h.login("", nil, ""); res.session == nil {
		t.Errorf("public-client login failed: %d %s", res.rec.Code, res.rec.Body.String())
	}
}

func TestLogin_WrongClientSecretIsRejected(t *testing.T) {
	h := newHarness(t, func(c *AuthConfig) { c.ClientSecret = "not-the-secret" })
	if res := h.login("", nil, ""); res.session != nil || res.rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d session=%v, want 401 and no session", res.rec.Code, res.session)
	}
}

func TestLogin_NextCannotLeaveTheSite(t *testing.T) {
	h := newHarness(t, nil)
	for next, want := range map[string]string{
		"/jobs/abc":                             "/jobs/abc",
		"/?filter=changes":                      "/?filter=changes",
		"":                                      "/",
		"https://evil.example":                  "/",
		"//evil.example":                        "/",
		"/\\evil.example":                       "/",
		"evil":                                  "/",
		"/auth/login":                           "/", // no loops back into the flow
		"/ok\r\nSet-Cookie: x":                  "/",
		"/a/../\\evil.example":                  "/", // http.Redirect cleans this to /\evil.example, which browsers read as //evil.example
		"/a\\b":                                 "/",
		"/" + strings.Repeat("a", maxNextLen):   "/", // longer than maxNextLen
		"/" + strings.Repeat("a", maxNextLen-1): "/" + strings.Repeat("a", maxNextLen-1),
	} {
		res := h.login(next, nil, "")
		if got := res.rec.Header().Get("Location"); got != want {
			t.Errorf("next %q -> %q, want %q", next, got, want)
		}
	}
}

// ---- the callback must reject everything that is not a clean login ----

func TestCallback_RejectsBadTokens(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(map[string]any)
		sign   string
		want   int
	}{
		{"wrong nonce", func(c map[string]any) { c["nonce"] = "someone-elses" }, "", 401},
		{"missing nonce", func(c map[string]any) { delete(c, "nonce") }, "", 401},
		{"wrong audience", func(c map[string]any) { c["aud"] = "another-app" }, "", 401},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example" }, "", 401},
		{"expired", func(c map[string]any) { c["exp"] = time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC).Unix() }, "", 401},
		{"signed by an unpublished key", nil, "other", 401},
		{"alg none", nil, "none", 401},
		{"alg HS256 (symmetric)", nil, "hs256", 401},
		{"no groups claim", func(c map[string]any) { delete(c, "groups") }, "", 403},
		{"groups in no mapped role", func(c map[string]any) { c["groups"] = []string{"random"} }, "", 403},
		{"groups claim wrong type", func(c map[string]any) { c["groups"] = 42 }, "", 403},
		{"group name differs in case", func(c map[string]any) { c["groups"] = []string{"DEPLOY-APPLIERS"} }, "", 403},
		{"empty groups", func(c map[string]any) { c["groups"] = []string{} }, "", 403},
		{"group name padded with spaces is a different group", func(c map[string]any) { c["groups"] = []string{" deploy-appliers "} }, "", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil)
			res := h.login("", c.tamper, c.sign)
			if res.session != nil {
				t.Fatalf("a session was issued for a bad login")
			}
			if res.rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %q)", res.rec.Code, c.want, res.rec.Body.String())
			}
		})
	}
}

func TestCallback_StateHandling(t *testing.T) {
	t.Run("no login cookie (state prepared by someone else)", func(t *testing.T) {
		h := newHarness(t, nil)
		authURL, state, _ := h.startLogin("")
		res := h.callback(h.op.approve(authURL, nil, ""), state) // no cookie
		if res.session != nil || res.rec.Code != http.StatusBadRequest {
			t.Errorf("got %d session=%v, want 400 and no session", res.rec.Code, res.session)
		}
	})
	t.Run("cookie holds a different state", func(t *testing.T) {
		h := newHarness(t, nil)
		authURL, state, _ := h.startLogin("")
		_, _, otherCookie := h.startLogin("")
		res := h.callback(h.op.approve(authURL, nil, ""), state, otherCookie)
		if res.session != nil || res.rec.Code != http.StatusBadRequest {
			t.Errorf("got %d session=%v, want 400 and no session", res.rec.Code, res.session)
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		h := newHarness(t, nil)
		authURL, _, _ := h.startLogin("")
		forged := &http.Cookie{Name: loginCookie, Value: "forged"}
		res := h.callback(h.op.approve(authURL, nil, ""), "forged", forged)
		if res.session != nil || res.rec.Code != http.StatusBadRequest {
			t.Errorf("got %d session=%v, want 400 and no session", res.rec.Code, res.session)
		}
	})
	t.Run("replayed callback", func(t *testing.T) {
		h := newHarness(t, nil)
		authURL, state, lc := h.startLogin("")
		code := h.op.approve(authURL, nil, "")
		if first := h.callback(code, state, lc); first.session == nil {
			t.Fatal("first callback should have logged in")
		}
		if second := h.callback(code, state, lc); second.session != nil {
			t.Error("a replayed callback issued a second session")
		}
		// Even with a fresh, valid code the used-up state must not work
		// again: that is the server's own single-use rule, not just the
		// provider refusing to redeem a code twice.
		if third := h.callback(h.op.approve(authURL, nil, ""), state, lc); third.session != nil {
			t.Error("a used login state was accepted a second time")
		}
	})
	t.Run("state expired", func(t *testing.T) {
		h := newHarness(t, nil)
		authURL, state, lc := h.startLogin("")
		code := h.op.approve(authURL, nil, "")
		h.clock.Advance(loginStateTTL + time.Second)
		if res := h.callback(code, state, lc); res.session != nil {
			t.Error("a login that took longer than the state lifetime was accepted")
		}
	})
	t.Run("provider reports an error", func(t *testing.T) {
		h := newHarness(t, nil)
		_, state, lc := h.startLogin("")
		rec := h.do(req{method: "GET", path: callbackPath + "?error=access_denied&state=" + state, cookies: []*http.Cookie{lc}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("code the provider never issued", func(t *testing.T) {
		h := newHarness(t, nil)
		_, state, lc := h.startLogin("")
		if res := h.callback("bogus", state, lc); res.session != nil || res.rec.Code != http.StatusUnauthorized {
			t.Errorf("got %d session=%v, want 401 and no session", res.rec.Code, res.session)
		}
	})
	t.Run("no code", func(t *testing.T) {
		h := newHarness(t, nil)
		_, state, lc := h.startLogin("")
		rec := h.do(req{method: "GET", path: callbackPath + "?state=" + state, cookies: []*http.Cookie{lc}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

func TestCallback_CodeAndStateAreNotLogged(t *testing.T) {
	h := newHarness(t, nil)
	buf := captureLog(t)
	authURL, state, lc := h.startLogin("")
	code := h.op.approve(authURL, nil, "")
	h.callback(code, state, lc)
	logged := buf.String()
	if strings.Contains(logged, code) || strings.Contains(logged, state) {
		t.Errorf("the one-time code or state appears in the log:\n%s", logged)
	}
	if !strings.Contains(logged, callbackPath+"?(redacted)") {
		t.Errorf("access log should record the callback with a redacted query:\n%s", logged)
	}
}

func TestLogin_PendingLoginsAreBoundedButNeverLockOutNewLogins(t *testing.T) {
	h := newHarness(t, nil)
	var firstState string
	var firstCookie *http.Cookie
	for i := 0; i < maxLiveEntries; i++ {
		rec := h.do(req{method: "GET", path: "/auth/login"})
		if rec.Code != http.StatusFound {
			t.Fatalf("login %d = %d, want 302", i, rec.Code)
		}
		if i == 0 {
			firstCookie = cookieNamed(rec, h.auth.cookieName(loginCookie))
			firstState = firstCookie.Value
		}
		h.clock.Advance(time.Millisecond) // distinct ages, so "oldest" is well defined
	}
	// A flood of abandoned logins must not make the next real login fail...
	authURL, state, lc := h.startLogin("")
	if res := h.callback(h.op.approve(authURL, nil, ""), state, lc); res.session == nil {
		t.Errorf("a login after the store filled up failed: %d", res.rec.Code)
	}
	// ...at the price of the oldest abandoned one, and memory stays bounded.
	if n := len(h.auth.pending.m); n > maxLiveEntries {
		t.Errorf("pending store holds %d entries, cap is %d", n, maxLiveEntries)
	}
	if _, ok := h.auth.pending.get(firstState); ok {
		t.Error("the oldest pending login should have been evicted to make room")
	}
}

// ---- claims → identity and role ------------------------------------------

func TestClaims_RoleAndUsername(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*AuthConfig)
		tamper   func(map[string]any)
		wantUser string
		wantRole Role
	}{
		{"groups array, highest role wins", nil, func(c map[string]any) { c["groups"] = []string{"deploy-planners", "deploy-viewers", "other"} }, "alice", RolePlanner},
		{"groups as a single string", nil, func(c map[string]any) { c["groups"] = "deploy-viewers" }, "alice", RoleViewer},
		{"custom groups claim", func(c *AuthConfig) { c.GroupsClaim = "roles" }, func(c map[string]any) { c["roles"] = []string{"admins"}; delete(c, "groups") }, "alice", RoleApplier},
		{"custom claim ignores the default one", func(c *AuthConfig) { c.GroupsClaim = "roles" }, nil, "", RoleNone},
		{"username claim wins over email", nil, func(c map[string]any) { c["email"] = "alice@example.com" }, "alice", RoleApplier},
		{"username falls back to email", nil, func(c map[string]any) { delete(c, "preferred_username"); c["email"] = "alice@example.com" }, "alice@example.com", RoleApplier},
		{"username falls back to sub", nil, func(c map[string]any) { delete(c, "preferred_username") }, "sub-alice", RoleApplier},
		{"custom username claim", func(c *AuthConfig) { c.UsernameClaim = "name" }, func(c map[string]any) { c["name"] = "Alice A" }, "Alice A", RoleApplier},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.mutate)
			res := h.login("", c.tamper, "")
			if c.wantRole == RoleNone {
				if res.session != nil {
					t.Fatal("expected no session")
				}
				return
			}
			if res.session == nil {
				t.Fatalf("login failed: %d %s", res.rec.Code, res.rec.Body.String())
			}
			hr := httptest.NewRequest("GET", "/", nil)
			hr.AddCookie(res.session)
			id, ok := h.auth.identify(hr)
			if !ok || id.User != c.wantUser || id.Role != c.wantRole || id.Subject != "sub-alice" {
				t.Errorf("identity = %+v, want user %q role %s sub sub-alice", id, c.wantUser, c.wantRole)
			}
		})
	}
}

func TestLogin_RoleFromGroupsEnforcedOnRoutes(t *testing.T) {
	h := newHarness(t, nil)
	res := h.login("", func(c map[string]any) { c["groups"] = []string{"deploy-planners"} }, "")
	cs := []*http.Cookie{res.session}
	if rec := h.do(req{method: "POST", path: "/run", form: runForm("plan", "nope"), cookies: cs}); denied(rec.Code) {
		t.Errorf("planner plan = %d, want allowed", rec.Code)
	}
	if rec := h.do(req{method: "POST", path: "/run", form: runForm("apply", "nope"), cookies: cs}); rec.Code != http.StatusForbidden {
		t.Errorf("planner apply = %d, want 403", rec.Code)
	}
}

// ---- sessions --------------------------------------------------------------

func TestSession_UnauthenticatedRequests(t *testing.T) {
	h := newHarness(t, nil)
	for _, r := range []req{
		{method: "GET", path: "/"},
		{method: "GET", path: "/graph", header: map[string]string{"Accept": "*/*"}},
		{method: "POST", path: "/run", form: runForm("plan", "")},
		{method: "POST", path: "/run", form: runForm("plan", ""), header: map[string]string{"Accept": "text/html,application/xhtml+xml"}}, // a browser form post: must not be bounced to a login page that drops the action
		{method: "GET", path: "/jobs/x/stream", header: map[string]string{"Accept": "text/event-stream"}},                                 // an EventSource cannot follow an HTML login redirect
		{method: "GET", path: "/logs/report.html"},
		{method: "POST", path: "/jobs/x/confirm", form: url.Values{"dir": {"a"}}},
		{method: "GET", path: "/", cookies: []*http.Cookie{{Name: sessionCookie, Value: "forged"}}},
		{method: "GET", path: "/", cookies: []*http.Cookie{{Name: sessionCookie, Value: ""}}},
	} {
		if rec := h.do(r); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", r.method, r.path, rec.Code)
		}
	}
	// The login entry point itself needs no session.
	if rec := h.do(req{method: "GET", path: "/auth/login"}); rec.Code != http.StatusFound {
		t.Errorf("GET /auth/login = %d, want 302", rec.Code)
	}
}

func TestSession_Expires(t *testing.T) {
	h := newHarness(t, func(c *AuthConfig) { c.SessionTTL = time.Hour })
	res := h.login("", nil, "")
	cs := []*http.Cookie{res.session}
	if rec := h.do(req{method: "GET", path: "/graph", cookies: cs}); denied(rec.Code) {
		t.Fatalf("fresh session = %d", rec.Code)
	}
	h.clock.Advance(time.Hour + time.Second)
	if rec := h.do(req{method: "GET", path: "/graph", cookies: cs}); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired session = %d, want 401", rec.Code)
	}
}

func TestSession_Logout(t *testing.T) {
	h := newHarness(t, nil)
	res := h.login("", nil, "")
	cs := []*http.Cookie{res.session}

	// Logging out is a POST: it must be same-origin too.
	if rec := h.do(req{method: "POST", path: "/auth/logout", cookies: cs, noOrigin: true}); rec.Code != http.StatusForbidden {
		t.Errorf("logout without Origin = %d, want 403", rec.Code)
	}
	if rec := h.do(req{method: "GET", path: "/graph", cookies: cs}); denied(rec.Code) {
		t.Fatal("a refused logout must leave the session alone")
	}

	rec := h.do(req{method: "POST", path: "/auth/logout", cookies: cs})
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d, want 200", rec.Code)
	}
	if c := cookieNamed(rec, sessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout should clear the cookie, got %+v", c)
	}
	if rec := h.do(req{method: "GET", path: "/graph", cookies: cs}); rec.Code != http.StatusUnauthorized {
		t.Errorf("old session after logout = %d, want 401 (server-side revoked, not just the cookie)", rec.Code)
	}
}

// ---- CSRF ------------------------------------------------------------------

func TestCSRF_StateChangingRequestsMustBeSameOrigin(t *testing.T) {
	h := newHarness(t, nil)
	cs := []*http.Cookie{h.sessionFor("alice", RoleApplier)}
	post := func(header map[string]string, noOrigin bool) int {
		return h.do(req{method: "POST", path: "/run", form: runForm("apply", "nope"), cookies: cs, header: header, noOrigin: noOrigin}).Code
	}
	cases := []struct {
		name     string
		header   map[string]string
		noOrigin bool
		allowed  bool
	}{
		{"matching Origin", nil, false, true},
		{"foreign Origin", map[string]string{"Origin": "https://evil.example"}, false, false},
		{"Origin null (sandboxed iframe / redirect)", map[string]string{"Origin": "null"}, false, false},
		{"same host, other port", map[string]string{"Origin": "http://127.0.0.1:9999"}, false, false},
		{"same host, other scheme", map[string]string{"Origin": "https://127.0.0.1:8080"}, false, false},
		{"Origin that merely starts with ours", map[string]string{"Origin": "http://127.0.0.1:80801"}, false, false},
		{"Origin that is a prefix of ours", map[string]string{"Origin": "http://127.0.0.1:808"}, false, false},
		{"Origin with ours as a host prefix", map[string]string{"Origin": "http://127.0.0.1:8080.evil.example"}, false, false},
		{"neither header", nil, true, false},
		{"Sec-Fetch-Site same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, true, true},
		{"Sec-Fetch-Site cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, false, false},
		{"Sec-Fetch-Site same-site (a sibling subdomain)", map[string]string{"Sec-Fetch-Site": "same-site"}, false, false},
		{"Sec-Fetch-Site none", map[string]string{"Sec-Fetch-Site": "none"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := post(c.header, c.noOrigin)
			if c.allowed && denied(got) {
				t.Errorf("status = %d, want past the gate", got)
			}
			if !c.allowed && got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}

	// Reads stay navigable from other sites (links); only writes are checked.
	if rec := h.do(req{method: "GET", path: "/graph", cookies: cs, header: map[string]string{"Sec-Fetch-Site": "cross-site"}}); denied(rec.Code) {
		t.Errorf("cross-site GET = %d, want served", rec.Code)
	}
	// And confirm/cancel are covered like /run.
	if rec := h.do(req{method: "POST", path: "/jobs/x/confirm", form: url.Values{"dir": {"a"}}, cookies: cs, noOrigin: true}); rec.Code != http.StatusForbidden {
		t.Errorf("confirm without Origin = %d, want 403", rec.Code)
	}
}

// ---- RBAC on routes (identity is planted; the flow is tested above) ----------

func TestRBAC_RoleMatrix(t *testing.T) {
	h := newHarness(t, nil)
	// Jobs to cancel/confirm. Never started, so nothing runs.
	h.srv.jobs["plan1"] = newJob("plan1", "plan", nil, true)
	h.srv.jobs["apply1"] = newJob("apply1", "apply", nil, true)

	cookies := map[string]*http.Cookie{
		"viewer":  h.sessionFor("v", RoleViewer),
		"planner": h.sessionFor("p", RolePlanner),
		"applier": h.sessionFor("a", RoleApplier),
	}
	cases := []struct {
		role, name, method, path string
		form                     url.Values
		allow                    bool
	}{
		// viewer: read-only pages only
		{"viewer", "dashboard", "GET", "/", nil, true},
		{"viewer", "graph", "GET", "/graph", nil, true},
		{"viewer", "job page", "GET", "/jobs/plan1", nil, true},
		{"viewer", "job stream", "GET", "/jobs/nope/stream", nil, true}, // 404: past the gate, no such job
		{"viewer", "logs", "GET", "/logs/report.html", nil, true},       // 404: past the gate, no file
		{"viewer", "plan", "POST", "/run", runForm("plan", "nope"), false},
		{"viewer", "init", "POST", "/run", runForm("init", "nope"), false},
		{"viewer", "sync", "POST", "/run", runForm("sync", ""), false},
		{"viewer", "apply", "POST", "/run", runForm("apply", "nope"), false},
		{"viewer", "cancel plan", "POST", "/jobs/plan1/cancel", nil, false},
		{"viewer", "confirm", "POST", "/jobs/apply1/confirm", url.Values{"dir": {"a"}}, false},

		// planner: plan/init/sync, never apply or confirm
		{"planner", "dashboard", "GET", "/", nil, true},
		{"planner", "plan", "POST", "/run", runForm("plan", "nope"), true},
		{"planner", "init", "POST", "/run", runForm("init", "nope"), true},
		{"planner", "sync", "POST", "/run", runForm("sync", ""), true},
		{"planner", "apply", "POST", "/run", runForm("apply", "nope"), false},
		{"planner", "apply all", "POST", "/run", runForm("apply", ""), false},
		{"planner", "cancel plan job", "POST", "/jobs/plan1/cancel", nil, true},
		{"planner", "cancel apply job", "POST", "/jobs/apply1/cancel", nil, false},
		{"planner", "confirm", "POST", "/jobs/apply1/confirm", url.Values{"dir": {"a"}}, false},

		// applier: everything
		{"applier", "dashboard", "GET", "/", nil, true},
		{"applier", "plan", "POST", "/run", runForm("plan", "nope"), true},
		{"applier", "apply", "POST", "/run", runForm("apply", "nope"), true},
		{"applier", "cancel apply job", "POST", "/jobs/apply1/cancel", nil, true},
		{"applier", "confirm", "POST", "/jobs/apply1/confirm", url.Values{"dir": {"a"}}, true},
	}
	for _, c := range cases {
		t.Run(c.role+" "+c.name, func(t *testing.T) {
			rec := h.do(req{method: c.method, path: c.path, form: c.form, cookies: []*http.Cookie{cookies[c.role]}})
			if c.allow && denied(rec.Code) {
				t.Errorf("status = %d, want to get past the auth gate", rec.Code)
			}
			if !c.allow && rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestRBAC_KindIsAuthorizedAsExecuted(t *testing.T) {
	// r.FormValue prefers the body over the query string. The role
	// check must use the same value the run uses, so a planner cannot
	// show one kind to the gate and another to the runner.
	h := newHarness(t, nil)
	rec := h.do(req{method: "POST", path: "/run?kind=plan", form: runForm("apply", "nope"), cookies: []*http.Cookie{h.sessionFor("mallory", RolePlanner)}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body kind=apply wins over query kind=plan)", rec.Code)
	}
}

// ---- audit log ----------------------------------------------------------------

func TestAudit_NamesUserKindDirAndOutcome(t *testing.T) {
	h := newHarness(t, nil)
	buf := captureLog(t)
	h.do(req{method: "POST", path: "/run", form: runForm("apply", "services/a"), cookies: []*http.Cookie{h.sessionFor("alice", RolePlanner)}})
	h.do(req{method: "POST", path: "/run", form: runForm("plan", "nope"), cookies: []*http.Cookie{h.sessionFor("bob", RolePlanner)}})
	logged := buf.String()
	for _, want := range []string{
		`user="alice"`, `sub="sub-alice"`, `kind=apply`, `dir="services/a"`, `denied`,
		`user="bob"`, `sub="sub-bob"`, `kind=plan`, `dir="nope"`, `rejected`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("audit log missing %q:\n%s", want, logged)
		}
	}
}

func TestAudit_CannotBeForgedThroughDir(t *testing.T) {
	h := newHarness(t, nil)
	buf := captureLog(t)
	h.do(req{method: "POST", path: "/run", form: runForm("plan", "x\n2026/01/01 job started: id=fake"), cookies: []*http.Cookie{h.sessionFor("alice", RolePlanner)}})
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "job started: id=fake") && !strings.Contains(line, `\n`) {
			t.Errorf("a client-supplied newline started a fresh log line: %q", line)
		}
	}
}

func TestAudit_LoginAndDenialsAreLogged(t *testing.T) {
	h := newHarness(t, nil)
	buf := captureLog(t)
	h.login("", nil, "")
	h.login("", func(c map[string]any) { c["groups"] = []string{"random"} }, "")
	h.do(req{method: "POST", path: "/run", form: runForm("apply", ""), cookies: []*http.Cookie{h.sessionFor("v", RoleViewer)}})
	logged := buf.String()
	for _, want := range []string{`login: user="alice" sub="sub-alice" role=applier`, "login denied", "no group mapped", `auth denied: user="v" sub="sub-v" role=viewer needs planner`} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
}

// ---- configuration: fail closed -------------------------------------------------

func TestNewAuth_FailsClosedOnIncompleteOrUnsafeConfig(t *testing.T) {
	clock := newTestClock()
	op := newFakeOP(t, clock, "homelab", "s3cret")
	for name, mutate := range map[string]func(*AuthConfig){
		"no issuer":                    func(c *AuthConfig) { c.Issuer = "" },
		"http issuer off loopback":     func(c *AuthConfig) { c.Issuer = "http://auth.example.com" },
		"issuer not a URL":             func(c *AuthConfig) { c.Issuer = "auth" },
		"issuer unreachable":           func(c *AuthConfig) { c.Issuer = "http://127.0.0.1:1" },
		"issuer without discovery":     func(c *AuthConfig) { c.Issuer = op.URL() + "/nothing-here" },
		"no client id":                 func(c *AuthConfig) { c.ClientID = "" },
		"no public url":                func(c *AuthConfig) { c.PublicURL = "" },
		"http public url off loopback": func(c *AuthConfig) { c.PublicURL = "http://deploy.example.com" },
		"public url with a path":       func(c *AuthConfig) { c.PublicURL = "https://example.com/deploy" },
		"no groups mapped":             func(c *AuthConfig) { c.ViewerGroups, c.PlannerGroups, c.ApplierGroups = nil, nil, nil },
		"only blank groups mapped": func(c *AuthConfig) {
			c.ViewerGroups, c.PlannerGroups, c.ApplierGroups = []string{" "}, []string{""}, nil
		},
		"negative session ttl": func(c *AuthConfig) { c.SessionTTL = -time.Hour },
	} {
		cfg := baseConfig(op, clock)
		mutate(&cfg)
		if _, err := NewAuth(context.Background(), cfg); err == nil {
			t.Errorf("%s: NewAuth succeeded; want an error (fail closed)", name)
		}
	}
	if _, err := NewAuth(context.Background(), baseConfig(op, clock)); err != nil {
		t.Fatalf("baseline config rejected: %v", err)
	}
}

func TestNewServer_RequiresAnAuthChoice(t *testing.T) {
	if _, err := newServer(t.TempDir(), &tf.Runner{}, nil); err == nil {
		t.Fatal("newServer with nil auth succeeded; want an error so a forgotten setting cannot serve open")
	}
}

func TestAuthFromEnv(t *testing.T) {
	clock := newTestClock()
	op := newFakeOP(t, clock, "homelab", "s3cret")
	secretFile := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secretFile, []byte("s3cret\n"), 0o600)
	emptyFile := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(emptyFile, []byte(" \n"), 0o600)

	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	good := func() map[string]string {
		return map[string]string{
			EnvOIDCIssuer: op.URL(), EnvOIDCClientID: "homelab", EnvOIDCClientSecretFile: secretFile,
			EnvPublicURL: publicURL, EnvOIDCScopes: "openid, profile groups",
			EnvViewerGroups: "v1,v2", EnvPlannerGroups: "p1", EnvApplierGroups: "a1, a2",
			EnvSessionTTL: "2h",
		}
	}

	a, err := AuthFromEnv(context.Background(), env(good()), false)
	if err != nil {
		t.Fatal(err)
	}
	if a.ttl != 2*time.Hour || a.groups["a2"] != RoleApplier || a.groups["v1"] != RoleViewer || a.groups["p1"] != RolePlanner {
		t.Errorf("env not applied: ttl=%v groups=%v", a.ttl, a.groups)
	}
	if got := strings.Join(a.oauth.Scopes, " "); got != "openid profile groups" {
		t.Errorf("scopes = %q, want %q", got, "openid profile groups")
	}
	// The secret read from the file really is what is sent: the provider insists on it.
	s, _ := newServer(t.TempDir(), &tf.Runner{}, a)
	h := &harness{t: t, clock: clock, op: op, auth: a, srv: s}
	op.clock = &testClock{t: time.Now()} // this Auth uses the real clock, so the provider must too
	if res := h.login("", func(c map[string]any) { c["groups"] = []string{"a1"} }, ""); res.session == nil {
		t.Errorf("login with env-built auth failed: %d %s", res.rec.Code, res.rec.Body.String())
	}

	// Nothing set: refuse to start, and say how.
	if _, err := AuthFromEnv(context.Background(), env(nil), false); err == nil {
		t.Error("AuthFromEnv with no config succeeded")
	} else if !strings.Contains(err.Error(), EnvOIDCIssuer) || !strings.Contains(err.Error(), "--insecure-no-auth") {
		t.Errorf("error should tell the operator what to set: %v", err)
	}

	for name, mutate := range map[string]func(map[string]string){
		"secret file missing": func(m map[string]string) { m[EnvOIDCClientSecretFile] = secretFile + ".nope" },
		"secret file empty":   func(m map[string]string) { m[EnvOIDCClientSecretFile] = emptyFile },
		"bad ttl":             func(m map[string]string) { m[EnvSessionTTL] = "soon" },
		"zero ttl":            func(m map[string]string) { m[EnvSessionTTL] = "0s" },
		"no issuer":           func(m map[string]string) { delete(m, EnvOIDCIssuer) },
		"no client id":        func(m map[string]string) { delete(m, EnvOIDCClientID) },
		"no public url":       func(m map[string]string) { delete(m, EnvPublicURL) },
		"no groups": func(m map[string]string) {
			delete(m, EnvViewerGroups)
			delete(m, EnvPlannerGroups)
			delete(m, EnvApplierGroups)
		},
	} {
		m := good()
		mutate(m)
		if _, err := AuthFromEnv(context.Background(), env(m), false); err == nil {
			t.Errorf("%s: accepted; want an error", name)
		}
	}
}

func TestAuthFromEnv_InsecureNoAuth(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	a, err := AuthFromEnv(context.Background(), env(nil), true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t.TempDir(), &tf.Runner{}, a)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, auth: a, srv: s}
	// Explicit opt-out: no session, no Origin, full access (the old behavior).
	rec := h.do(req{method: "POST", path: "/run", form: runForm("apply", "nope"), noOrigin: true})
	if denied(rec.Code) {
		t.Errorf("--insecure-no-auth: status = %d, want requests let through", rec.Code)
	}

	// Setting both is ambiguous: refuse, so a leftover flag cannot
	// silently disable auth that the operator configured.
	if _, err = AuthFromEnv(context.Background(), env(map[string]string{EnvOIDCIssuer: "https://auth.example.com"}), true); err == nil {
		t.Error("--insecure-no-auth together with auth settings was accepted; want an error")
	}
}

// TestBrowserFlow_RealHTTP drives the whole login the way a browser
// does: a real listener, a cookie jar, and real redirects through the
// provider and back. It exists to catch what handler-level tests cannot,
// such as a cookie whose Path or SameSite stops it surviving the
// round trip.
func TestBrowserFlow_RealHTTP(t *testing.T) {
	clock := newTestClock()
	op := newFakeOP(t, clock, "homelab", "s3cret")

	front := httptest.NewUnstartedServer(nil)
	pub := "http://" + front.Listener.Addr().String()
	cfg := baseConfig(op, clock)
	cfg.PublicURL = pub
	a, err := NewAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t.TempDir(), &tf.Runner{}, a)
	if err != nil {
		t.Fatal(err)
	}
	front.Config.Handler = s.Handler()
	front.Start()
	t.Cleanup(front.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	get := func(path string) (*http.Response, string) {
		r, _ := http.NewRequest("GET", pub+path, nil)
		r.Header.Set("Accept", "text/html")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// Straight to a protected page: login happens in the middle, and we land back on it.
	resp, body := get("/graph")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/graph" {
		t.Fatalf("after login landed on %s with %d (%q), want 200 at /graph", resp.Request.URL, resp.StatusCode, body)
	}
	// The session now carries a POST, provided it is same-origin.
	post := func(origin string) int {
		r, _ := http.NewRequest("POST", pub+"/run", strings.NewReader(runForm("apply", "nope").Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(pub); denied(code) {
		t.Errorf("same-origin POST = %d, want past the gate", code)
	}
	if code := post("https://evil.example"); code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", code)
	}

	// Logging out really ends it: the next navigation is sent back to log in.
	r, _ := http.NewRequest("POST", pub+"/auth/logout", nil)
	r.Header.Set("Origin", pub)
	lr, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	lr.Body.Close()
	if lr.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d, want 200", lr.StatusCode)
	}
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ = http.NewRequest("GET", pub+"/graph", nil)
	r.Header.Set("Accept", "text/html")
	resp, err = noFollow.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/auth/login") {
		t.Errorf("after logout GET /graph = %d -> %q, want 302 to /auth/login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// ---- review round: hardening ---------------------------------------------------

func TestNewAuth_RejectsInsecureDiscoveredEndpoints(t *testing.T) {
	for _, field := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri"} {
		t.Run(field, func(t *testing.T) {
			clock := newTestClock()
			op := newFakeOP(t, clock, "homelab", "s3cret")
			op.setDiscovery(field, "http://auth.example.com/"+field) // a https issuer can still advertise cleartext endpoints
			if _, err := NewAuth(context.Background(), baseConfig(op, clock)); err == nil {
				t.Errorf("NewAuth accepted a cleartext %s", field)
			} else if !strings.Contains(err.Error(), field) {
				t.Errorf("error should name the offending endpoint: %v", err)
			}
		})
	}
}

func TestHTTPClient_RefusesInsecureRedirects(t *testing.T) {
	h := newHarness(t, nil)
	check := h.auth.httpClient.CheckRedirect
	if check == nil {
		t.Fatal("the provider HTTP client follows redirects unchecked")
	}
	for target, ok := range map[string]bool{
		"https://auth.example.com/token": true,
		"http://127.0.0.1:9/token":       true, // loopback, as for the issuer itself
		"http://auth.example.com/token":  false,
		"http://10.0.0.5/token":          false,
		"ftp://auth.example.com/token":   false,
	} {
		u, _ := url.Parse(target)
		err := check(&http.Request{URL: u}, nil)
		if (err == nil) != ok {
			t.Errorf("redirect to %s: err=%v, want ok=%v", target, err, ok)
		}
	}
	if err := check(&http.Request{URL: &url.URL{Scheme: "https", Host: "a.example"}}, make([]*http.Request, 10)); err == nil {
		t.Error("an endless redirect chain was followed")
	}
}

func TestResponses_AreNotFrameableOrCacheable(t *testing.T) {
	h := newHarness(t, nil)
	os.MkdirAll(filepath.Join(h.srv.repoRoot, ".deploy-logs"), 0o755)
	os.WriteFile(filepath.Join(h.srv.repoRoot, ".deploy-logs", "report.html"), []byte("<html>r</html>"), 0o644)
	cs := []*http.Cookie{h.sessionFor("v", RoleViewer)}
	for _, r := range []req{
		{method: "GET", path: "/graph", cookies: cs},
		{method: "GET", path: "/logs/report.html", cookies: cs},
		{method: "GET", path: "/graph"},                                         // 401
		{method: "GET", path: "/auth/login"},                                    // redirect
		{method: "POST", path: "/run", form: runForm("apply", ""), cookies: cs}, // 403
	} {
		rec := h.do(r)
		hd := rec.Header()
		if hd.Get("X-Frame-Options") != "DENY" || !strings.Contains(hd.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s %s (%d): framing not denied: XFO=%q CSP=%q", r.method, r.path, rec.Code, hd.Get("X-Frame-Options"), hd.Get("Content-Security-Policy"))
		}
		if !strings.Contains(hd.Get("Cache-Control"), "no-store") {
			t.Errorf("%s %s (%d): Cache-Control = %q, want no-store", r.method, r.path, rec.Code, hd.Get("Cache-Control"))
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s (%d): missing nosniff", r.method, r.path, rec.Code)
		}
	}
}

func TestLogs_OnlyTheReportsAreServed(t *testing.T) {
	h := newHarness(t, nil)
	dir := filepath.Join(h.srv.repoRoot, ".deploy-logs")
	os.MkdirAll(dir, 0o755)
	for name, body := range map[string]string{
		"report.html":       "<html>report</html>",
		"report.json":       "{}",
		"services-a.tfplan": "PLAN WITH SECRET VALUES",
		"notes.txt":         "other",
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	cs := []*http.Cookie{h.sessionFor("v", RoleViewer)}
	for path, want := range map[string]int{
		"/logs/report.html":            200,
		"/logs/report.json":            200,
		"/logs/services-a.tfplan":      404, // saved plans can hold unmasked secrets
		"/logs/notes.txt":              404,
		"/logs/":                       404, // no directory listing
		"/logs":                        404,
		"/logs/..%2Fservices-a.tfplan": 404,
		"/logs/report.html/..%2Fservices-a.tfplan": 404,
	} {
		rec := h.do(req{method: "GET", path: path, cookies: cs})
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if strings.Contains(rec.Body.String(), "SECRET") || strings.Contains(rec.Body.String(), "services-a") {
			t.Errorf("GET %s leaked plan data or a listing: %q", path, rec.Body.String())
		}
	}
	if rec := h.do(req{method: "GET", path: "/logs/report.html"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated report = %d, want 401", rec.Code)
	}
}

func TestScopes_ExplicitOpenidOnlyIsHonoured(t *testing.T) {
	for name, tc := range map[string]struct {
		scopes []string
		want   string
	}{
		"nothing configured":      {nil, "openid profile"},
		"blank entries":           {[]string{"", " "}, "openid profile"},
		"explicit openid only":    {[]string{"openid"}, "openid"},
		"openid plus groups":      {[]string{"groups"}, "openid groups"},
		"openid listed with more": {[]string{"openid", "email"}, "openid email"},
	} {
		h := newHarness(t, func(c *AuthConfig) { c.Scopes = tc.scopes })
		if got := strings.Join(h.auth.oauth.Scopes, " "); got != tc.want {
			t.Errorf("%s: scopes = %q, want %q", name, got, tc.want)
		}
	}
}

func TestAudit_PathsAreQuotedSoTheyCannotForgeLines(t *testing.T) {
	h := newHarness(t, nil)
	buf := captureLog(t)
	evil := "/jobs/x%0A2026%2F01%2F01%20job%20started:%20id=fake/confirm"
	h.do(req{method: "POST", path: evil, cookies: []*http.Cookie{h.sessionFor("v", RoleViewer)}})                  // role denial
	h.do(req{method: "POST", path: evil, cookies: []*http.Cookie{h.sessionFor("a", RoleApplier)}, noOrigin: true}) // cross-origin denial
	logged := buf.String()
	if !strings.Contains(logged, "auth denied") {
		t.Fatalf("expected denial lines in the log:\n%s", logged)
	}
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "job started: id=fake") && !strings.Contains(line, `\n`) {
			t.Errorf("a decoded newline in the path started a fresh log line: %q", line)
		}
	}
}

func TestOrigin_ComparedCanonically(t *testing.T) {
	h := newHarness(t, func(c *AuthConfig) { c.PublicURL = "HTTPS://Deploy.Example.COM:443" })
	if h.auth.origin != "https://deploy.example.com" {
		t.Fatalf("origin = %q, want the canonical https://deploy.example.com", h.auth.origin)
	}
	authURL, _, _ := h.startLogin("")
	if u, _ := url.Parse(authURL); u.Query().Get("redirect_uri") != "https://deploy.example.com/auth/callback" {
		t.Errorf("redirect_uri = %q, want the canonical origin + /auth/callback", u.Query().Get("redirect_uri"))
	}
	cs := []*http.Cookie{h.sessionFor("a", RoleApplier)}
	for origin, ok := range map[string]bool{
		"https://deploy.example.com":      true, // how browsers serialize it
		"https://DEPLOY.example.com":      true,
		"https://deploy.example.com:443":  true,
		"https://deploy.example.com:8443": false,
		"http://deploy.example.com":       false,
		"https://deploy.example.com/x":    false, // not an origin
		"https://user@deploy.example.com": false,
		"null":                            false,
		"":                                false,
	} {
		rec := h.do(req{method: "POST", path: "/run", form: runForm("apply", "nope"), cookies: cs, header: map[string]string{"Origin": origin}, noOrigin: origin == ""})
		if ok == (rec.Code == http.StatusForbidden) {
			t.Errorf("Origin %q: status %d, want allowed=%v", origin, rec.Code, ok)
		}
	}
}

func TestCookies_HostPrefixedAndPinnedOnHTTPS(t *testing.T) {
	h := newHarness(t, func(c *AuthConfig) { c.PublicURL = "https://deploy.example.com" })
	authURL, state, lc := h.startLogin("")
	if lc == nil || lc.Name != "__Host-"+loginCookie {
		t.Fatalf("login cookie = %+v, want name __Host-%s", lc, loginCookie)
	}
	res := h.callback(h.op.approve(authURL, nil, ""), state, lc)
	if res.session == nil || res.session.Name != "__Host-"+sessionCookie {
		t.Fatalf("session cookie = %+v, want name __Host-%s", res.session, sessionCookie)
	}
	// What the __Host- prefix demands of the browser: Secure, Path=/, no Domain.
	for _, c := range []*http.Cookie{lc, res.session} {
		if !c.Secure || c.Path != "/" || c.Domain != "" || !c.HttpOnly {
			t.Errorf("%s: Secure=%v Path=%q Domain=%q HttpOnly=%v, want Secure, Path=/, no Domain, HttpOnly", c.Name, c.Secure, c.Path, c.Domain, c.HttpOnly)
		}
	}
	// A cookie planted under the plain name by a sibling subdomain means nothing.
	h.auth.sessions.put("planted", Identity{User: "attacker", Role: RoleApplier}, time.Hour)
	if rec := h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{{Name: sessionCookie, Value: "planted"}}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("a plain-named session cookie was honoured on an https deployment: %d", rec.Code)
	}
	// Clearing uses the same attributes, or the browser would ignore it.
	out := h.do(req{method: "POST", path: "/auth/logout", cookies: []*http.Cookie{res.session}, header: map[string]string{"Origin": "https://deploy.example.com"}})
	if c := cookieNamed(out, "__Host-"+sessionCookie); c == nil || c.MaxAge >= 0 || !c.Secure || c.Path != "/" {
		t.Errorf("logout cookie = %+v, want an expiring __Host- cookie with Secure and Path=/", c)
	}
}

func TestCookies_PlainNamesOnLoopbackHTTP(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.auth.cookieName(sessionCookie); got != sessionCookie {
		t.Errorf("cookie name over http = %q, want the plain %q (browsers reject __Host- without Secure)", got, sessionCookie)
	}
}

func TestStore_TakeIsAtomic(t *testing.T) {
	now := time.Now
	for round := 0; round < 200; round++ {
		s := newExpiringStore[int](10, now, false)
		s.put("k", 1, time.Minute)
		var wins int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, ok := s.take("k"); ok {
					atomic.AddInt32(&wins, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d concurrent takes succeeded, want exactly 1 (single-use state)", round, wins)
		}
	}
}

func TestStream_EndsWhenTheSessionEnds(t *testing.T) {
	old := streamRecheck
	streamRecheck = 20 * time.Millisecond
	t.Cleanup(func() { streamRecheck = old })

	for _, how := range []string{"logout", "expiry"} {
		t.Run(how, func(t *testing.T) {
			h := newHarness(t, func(c *AuthConfig) { c.SessionTTL = time.Hour })
			h.srv.jobs["j1"] = newJob("j1", "plan", nil, true) // never finishes, so the stream stays open
			res := h.login("", nil, "")
			ts := httptest.NewServer(h.srv.Handler())
			t.Cleanup(ts.Close)

			r, _ := http.NewRequest("GET", ts.URL+"/jobs/j1/stream", nil)
			r.AddCookie(res.session)
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("stream = %d, want 200", resp.StatusCode)
			}
			if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
				t.Errorf("stream Cache-Control = %q, want no-store", cc)
			}

			done := make(chan struct{})
			go func() { io.Copy(io.Discard, resp.Body); close(done) }()
			select {
			case <-done:
				t.Fatal("stream ended on its own")
			case <-time.After(100 * time.Millisecond):
			}
			if how == "logout" {
				h.do(req{method: "POST", path: "/auth/logout", cookies: []*http.Cookie{res.session}})
			} else {
				h.clock.Advance(2 * time.Hour)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("stream kept delivering after the session ended by %s", how)
			}
		})
	}
}

func TestSessions_FullStoreRefusesRatherThanLoggingSomeoneOut(t *testing.T) {
	h := newHarness(t, nil)
	var first *http.Cookie
	for i := 0; i < maxLiveEntries; i++ {
		c := h.sessionFor("u"+strconv.Itoa(i), RoleViewer)
		if i == 0 {
			first = c
		}
		h.clock.Advance(time.Millisecond)
	}
	// One more login: refused with 503, and nobody already signed in is dropped.
	authURL, state, lc := h.startLogin("")
	res := h.callback(h.op.approve(authURL, nil, ""), state, lc)
	if res.session != nil || res.rec.Code != http.StatusServiceUnavailable {
		t.Errorf("login with a full session store = %d session=%v, want 503 and no session", res.rec.Code, res.session)
	}
	if rec := h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{first}}); denied(rec.Code) {
		t.Errorf("the oldest live session was evicted to make room: %d", rec.Code)
	}
}

// ---- review round 2 -------------------------------------------------------------

func TestAudit_EveryActionNamesTheStableSubject(t *testing.T) {
	// preferred_username is a display claim: not unique, not immutable. The
	// provider's subject is what distinguishes two accounts with one name.
	h := newHarness(t, nil)
	h.srv.jobs["plan1"] = newJob("plan1", "plan", nil, true)
	h.srv.jobs["apply1"] = newJob("apply1", "apply", nil, true)
	buf := captureLog(t)
	planner := []*http.Cookie{h.sessionFor("sam", RolePlanner)}
	applier := []*http.Cookie{h.sessionFor("sam", RoleApplier)}
	// A second account with the same display name but a different subject.
	h.auth.sessions.put("other", Identity{User: "sam", Subject: "sub-OTHER-sam", Role: RoleApplier}, time.Hour)
	other := []*http.Cookie{{Name: h.auth.cookieName(sessionCookie), Value: "other"}}

	h.do(req{method: "POST", path: "/run", form: runForm("apply", "nope"), cookies: planner})                                 // run requested + denied
	h.do(req{method: "POST", path: "/run", form: runForm("plan", "nope"), cookies: planner})                                  // run rejected
	h.do(req{method: "POST", path: "/jobs/apply1/cancel", cookies: planner})                                                  // cancel denied
	h.do(req{method: "POST", path: "/jobs/plan1/cancel", cookies: planner})                                                   // cancel requested
	h.do(req{method: "POST", path: "/jobs/apply1/confirm", form: url.Values{"dir": {"a"}}, cookies: applier})                 // apply confirmation ignored
	h.do(req{method: "POST", path: "/jobs/apply1/confirm", form: url.Values{"dir": {"a"}}, cookies: other})                   // ... by a different "sam"
	h.do(req{method: "POST", path: "/jobs/apply1/confirm", form: url.Values{"dir": {"a"}}, cookies: planner})                 // role denial
	h.do(req{method: "POST", path: "/jobs/apply1/confirm", form: url.Values{"dir": {"a"}}, cookies: applier, noOrigin: true}) // cross-origin denial

	logged := buf.String()
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, `user="sam"`) && !strings.Contains(line, `sub="sub-`) {
			t.Errorf("a line names the user but not the subject: %s", line)
		}
	}
	for _, want := range []string{
		"run requested", "run denied", "run rejected", "cancel denied", "job cancel requested", "apply confirmation ignored", "auth denied", "cross-origin",
		`sub="sub-OTHER-sam"`, // the two accounts called "sam" are told apart
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
}

func TestLogin_ReplacesThePresentedSession(t *testing.T) {
	h := newHarness(t, nil)
	first := h.login("", nil, "")
	if first.session == nil {
		t.Fatal("first login failed")
	}
	// Log in again from the same browser: it presents its current session.
	authURL, state, lc := h.startLogin("")
	second := h.callback(h.op.approve(authURL, nil, ""), state, lc, first.session)
	if second.session == nil {
		t.Fatalf("second login failed: %d", second.rec.Code)
	}
	if rec := h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{first.session}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the replaced session still works: %d, want 401", rec.Code)
	}
	if rec := h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{second.session}}); denied(rec.Code) {
		t.Errorf("the new session does not work: %d", rec.Code)
	}
}

func TestLogin_ReplacementWorksEvenWhenTheStoreIsFull(t *testing.T) {
	h := newHarness(t, nil)
	mine := h.sessionFor("me", RoleApplier)
	for i := 0; i < maxLiveEntries-1; i++ {
		h.sessionFor("u"+strconv.Itoa(i), RoleViewer)
	}
	authURL, state, lc := h.startLogin("")
	res := h.callback(h.op.approve(authURL, nil, ""), state, lc, mine)
	if res.session == nil {
		t.Errorf("re-login at capacity = %d, want the presented session replaced, not 503", res.rec.Code)
	}
	if n := len(h.auth.sessions.m); n > maxLiveEntries {
		t.Errorf("%d sessions, cap is %d", n, maxLiveEntries)
	}
}

func TestSessions_OneAccountCannotFillTheStore(t *testing.T) {
	h := newHarness(t, nil)
	var cookies []*http.Cookie
	for i := 0; i < maxSessionsPerSubject+4; i++ {
		res := h.login("", nil, "") // alice, never presenting an old session
		if res.session == nil {
			t.Fatalf("login %d failed: %d", i, res.rec.Code)
		}
		cookies = append(cookies, res.session)
		h.clock.Advance(time.Second)
	}
	live := 0
	for i, c := range cookies {
		ok := !denied(h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{c}}).Code)
		if ok {
			live++
		}
		if i < 4 && ok {
			t.Errorf("session %d (older than the newest %d) still works", i, maxSessionsPerSubject)
		}
	}
	if live != maxSessionsPerSubject {
		t.Errorf("%d live sessions for one account, want exactly %d (the newest)", live, maxSessionsPerSubject)
	}
	// And other people are untouched.
	bob := h.sessionFor("bob", RoleViewer)
	if rec := h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{bob}}); denied(rec.Code) {
		t.Errorf("another account's session was affected: %d", rec.Code)
	}
}

func TestCallback_MismatchedStateDoesNotDestroyAnotherLoginInProgress(t *testing.T) {
	h := newHarness(t, nil)
	authA, stateA, _ := h.startLogin("")       // tab 1 starts...
	authB, stateB, cookieB := h.startLogin("") // ...tab 2 starts; the browser now holds tab 2's cookie
	codeA, codeB := h.op.approve(authA, nil, ""), h.op.approve(authB, nil, "")

	// Tab 1's callback arrives carrying tab 2's cookie: it must fail without clearing it.
	stale := h.callback(codeA, stateA, cookieB)
	if stale.session != nil || stale.rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched callback = %d session=%v, want 400 and no session", stale.rec.Code, stale.session)
	}
	if c := cookieNamed(stale.rec, h.auth.cookieName(loginCookie)); c != nil {
		t.Errorf("a mismatched callback touched the login cookie: %+v", c)
	}
	// Tab 2's login still completes.
	if res := h.callback(codeB, stateB, cookieB); res.session == nil {
		t.Errorf("tab 2's login was destroyed by tab 1's stale callback: %d", res.rec.Code)
	}
	// A matching callback does consume the cookie.
	authC, stateC, cookieC := h.startLogin("")
	done := h.callback(h.op.approve(authC, nil, ""), stateC, cookieC)
	if c := cookieNamed(done.rec, h.auth.cookieName(loginCookie)); c == nil || c.MaxAge >= 0 {
		t.Errorf("a completed login should clear the login cookie, got %+v", c)
	}
}

func TestLogin_NoLongerInAnyGroupRevokesExistingSessions(t *testing.T) {
	h := newHarness(t, nil)
	laptop := h.login("", nil, "") // alice, applier
	phone := h.login("", nil, "")  // a second device
	bob := h.login("", func(c map[string]any) { c["sub"] = "sub-bob"; c["preferred_username"] = "bob" }, "")
	if laptop.session == nil || phone.session == nil || bob.session == nil {
		t.Fatal("setup logins failed")
	}

	// Alice is removed from every mapped group, then tries to sign in again from the laptop.
	authURL, state, lc := h.startLogin("")
	res := h.callback(h.op.approve(authURL, func(c map[string]any) { c["groups"] = []string{} }, ""), state, lc, laptop.session)
	if res.rec.Code != http.StatusForbidden || res.session != nil {
		t.Fatalf("re-login = %d, want 403 and no session", res.rec.Code)
	}
	if c := cookieNamed(res.rec, h.auth.cookieName(sessionCookie)); c == nil || c.MaxAge >= 0 {
		t.Errorf("the browser's session cookie should be cleared, got %+v", c)
	}
	for name, c := range map[string]*http.Cookie{"laptop": laptop.session, "phone": phone.session} {
		if rec := h.do(req{method: "POST", path: "/run", form: runForm("apply", "nope"), cookies: []*http.Cookie{c}}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: alice's old session still authorizes /run after losing her groups: %d", name, rec.Code)
		}
	}
	if rec := h.do(req{method: "GET", path: "/graph", cookies: []*http.Cookie{bob.session}}); denied(rec.Code) {
		t.Errorf("another account's session was revoked: %d", rec.Code)
	}
}
