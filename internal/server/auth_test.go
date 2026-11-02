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
	"strings"
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
	return &http.Cookie{Name: sessionCookie, Value: tok}
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
	return authURL, u.Query().Get("state"), cookieNamed(rec, loginCookie)
}

func (h *harness) callback(code, state string, cookies ...*http.Cookie) loginResult {
	h.t.Helper()
	rec := h.do(req{method: "GET", path: callbackPath + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state), cookies: cookies})
	r := loginResult{rec: rec}
	if c := cookieNamed(rec, sessionCookie); c != nil && c.Value != "" && c.MaxAge >= 0 {
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
		"/jobs/abc":            "/jobs/abc",
		"/?filter=changes":     "/?filter=changes",
		"":                     "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"evil":                 "/",
		"/auth/login":          "/", // no loops back into the flow
		"/ok\r\nSet-Cookie: x": "/",
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

func TestLogin_PendingLoginsAreBounded(t *testing.T) {
	h := newHarness(t, nil)
	for i := 0; i < maxLiveEntries; i++ {
		if rec := h.do(req{method: "GET", path: "/auth/login"}); rec.Code != http.StatusFound {
			t.Fatalf("login %d = %d, want 302", i, rec.Code)
		}
	}
	if rec := h.do(req{method: "GET", path: "/auth/login"}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("login past the cap = %d, want 503", rec.Code)
	}
	h.clock.Advance(loginStateTTL + time.Second) // lapsed entries free the space
	if rec := h.do(req{method: "GET", path: "/auth/login"}); rec.Code != http.StatusFound {
		t.Errorf("login after entries lapsed = %d, want 302", rec.Code)
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
		`user="alice"`, `kind=apply`, `dir="services/a"`, `denied`,
		`user="bob"`, `kind=plan`, `dir="nope"`, `rejected`,
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
	for _, want := range []string{`login: user="alice" sub="sub-alice" role=applier`, "login denied", "no group mapped", `auth denied: user="v" role=viewer needs planner`} {
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
