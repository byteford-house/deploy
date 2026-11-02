package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Role is what an authenticated user may do. Roles are cumulative: an
// applier can also plan, a planner can also view. Mapping a user to a
// role is by group membership only (see AuthConfig) — never hardcoded.
type Role int

const (
	RoleNone    Role = iota // authenticated, but in no mapped group
	RoleViewer              // dashboard, graph, job pages and output, static report
	RolePlanner             // + plan, init, sync, and cancelling those jobs
	RoleApplier             // + apply, confirming an apply, cancelling an apply
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RolePlanner:
		return "planner"
	case RoleApplier:
		return "applier"
	}
	return "none"
}

// Environment variables read by AuthFromEnv.
const (
	EnvOIDCIssuer           = "HOMELAB_DEPLOY_OIDC_ISSUER"             // the provider's issuer URL; discovery is <issuer>/.well-known/openid-configuration
	EnvOIDCClientID         = "HOMELAB_DEPLOY_OIDC_CLIENT_ID"          //
	EnvOIDCClientSecretFile = "HOMELAB_DEPLOY_OIDC_CLIENT_SECRET_FILE" // path to a file holding the secret; unset = public client (PKCE only)
	EnvOIDCScopes           = "HOMELAB_DEPLOY_OIDC_SCOPES"             // space or comma separated; default "openid profile"
	EnvOIDCGroupsClaim      = "HOMELAB_DEPLOY_OIDC_GROUPS_CLAIM"       // default "groups"
	EnvOIDCUsernameClaim    = "HOMELAB_DEPLOY_OIDC_USERNAME_CLAIM"     // default "preferred_username", then "email", then "sub"
	EnvPublicURL            = "HOMELAB_DEPLOY_PUBLIC_URL"              // where browsers reach this server, e.g. https://deploy.example.com
	EnvSessionTTL           = "HOMELAB_DEPLOY_SESSION_TTL"             // default 8h
	EnvViewerGroups         = "HOMELAB_DEPLOY_VIEWER_GROUPS"           // comma-separated group names
	EnvPlannerGroups        = "HOMELAB_DEPLOY_PLANNER_GROUPS"
	EnvApplierGroups        = "HOMELAB_DEPLOY_APPLIER_GROUPS"
)

const (
	defaultSessionTTL = 8 * time.Hour
	loginStateTTL     = 10 * time.Minute
	maxLiveEntries    = 1000 // cap on pending logins and on sessions, so unauthenticated /auth/login cannot grow memory without bound

	maxSessionsPerSubject = 5 // concurrent sessions one account may hold (a few devices); the oldest is dropped past this

	maxNextLen = 512 // longest post-login return path kept; stops 1000 pending logins retaining ~1 MiB each

	// Cookie base names. Over https they are prefixed "__Host-", which
	// makes a browser refuse any copy set by a sibling subdomain (it
	// requires Secure, Path=/ and no Domain), so a neighbouring service
	// cannot plant its own session or login state in a victim's browser.
	sessionCookie = "hd_session"
	loginCookie   = "hd_login"
	callbackPath  = "/auth/callback"
)

// AuthConfig configures OIDC login. It is provider-neutral: anything
// that publishes standard OpenID Connect discovery and supports the
// authorization code flow works; the claim names are settings, not
// assumptions about one vendor.
type AuthConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string // optional: empty makes this a public client relying on PKCE alone
	PublicURL    string // origin browsers use to reach this server; the redirect URI is PublicURL + /auth/callback

	Scopes        []string // default openid, profile; "openid" is always included
	GroupsClaim   string   // ID-token claim holding group names; default "groups". A JSON array of strings, or one string
	UsernameClaim string   // default "preferred_username", falling back to "email" then "sub"

	ViewerGroups  []string
	PlannerGroups []string
	ApplierGroups []string

	SessionTTL time.Duration // default 8h; roles are read at login, so a group change applies on the next login
	Now        func() time.Time
}

// Auth logs users in through an OIDC provider, keeps their session, and
// decides what role each request has. The zero value is not usable;
// build one with NewAuth or AuthFromEnv. newServer refuses a nil *Auth,
// so serving with no auth decision made is impossible rather than
// merely discouraged.
type Auth struct {
	disabled bool

	oauth         oauth2.Config
	verifier      *oidc.IDTokenVerifier
	httpClient    *http.Client
	origin        string // scheme://host[:port] of PublicURL, compared against Origin on POSTs
	secureCookies bool
	groupsClaim   string
	usernameClaim string
	groups        map[string]Role
	ttl           time.Duration
	now           func() time.Time

	sessions *expiringStore[Identity]
	pending  *expiringStore[pendingLogin]
}

type pendingLogin struct {
	nonce, verifier, next string
}

// Identity is who the provider says made the request, and their role.
type Identity struct {
	User    string // display/audit name from the username claim
	Subject string // the provider's stable id for the user
	Role    Role
}

type identityKey struct{}
type sessionCheckKey struct{}

// sessionStillValid reports whether the request's session is still live.
// With auth disabled there is no session to end.
func sessionStillValid(ctx context.Context) bool {
	if f, ok := ctx.Value(sessionCheckKey{}).(func() bool); ok {
		return f()
	}
	return true
}

func identityFrom(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id // zero value: no user, RoleNone — denies everything
}

// signingAlgs is every asymmetric algorithm go-jose verifies. Listing
// them (rather than trusting whatever the provider advertises) keeps a
// symmetric algorithm out: a token "signed" HS256 with a key an attacker
// can derive from public material must never verify.
var signingAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "EdDSA"}

// NewAuth validates cfg, contacts the provider's discovery endpoint, and
// fails closed: a missing setting, an insecure URL, an unreachable
// provider, or no group mapped to any role is an error, not a server that
// quietly lets everyone in (or no one).
func NewAuth(ctx context.Context, cfg AuthConfig) (*Auth, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("no OIDC issuer configured: set " + EnvOIDCIssuer)
	}
	if err := requireSecureURL("OIDC issuer", cfg.Issuer); err != nil {
		return nil, err
	}
	if cfg.ClientID == "" {
		return nil, errors.New("no OIDC client id configured: set " + EnvOIDCClientID)
	}
	if cfg.PublicURL == "" {
		return nil, errors.New("no public URL configured: set " + EnvPublicURL + " to the address browsers use to reach this server")
	}
	if err := requireSecureURL("public URL", cfg.PublicURL); err != nil {
		return nil, err
	}
	pub, _ := url.Parse(cfg.PublicURL) // validated just above
	if (pub.Path != "" && pub.Path != "/") || pub.RawQuery != "" || pub.Fragment != "" {
		return nil, fmt.Errorf("public URL %q must be just scheme://host[:port]; serving under a path prefix is not supported", cfg.PublicURL)
	}

	a := &Auth{
		groups:        map[string]Role{},
		groupsClaim:   orDefault(cfg.GroupsClaim, "groups"),
		usernameClaim: orDefault(cfg.UsernameClaim, "preferred_username"),
		ttl:           cfg.SessionTTL,
		now:           cfg.Now,
		origin:        canonicalOrigin(pub),
		secureCookies: strings.EqualFold(pub.Scheme, "https"),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			// Discovery, JWKS and the token exchange carry the code,
			// PKCE verifier and client secret: never follow a redirect to cleartext.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return requireSecureURL("redirect target", req.URL.String())
			},
		},
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.ttl < 0 {
		return nil, errors.New("session TTL must be positive")
	}
	if a.ttl == 0 {
		a.ttl = defaultSessionTTL
	}
	// Sessions never evict: dropping a live user's session to make room would
	// be a logout. Pending logins do: they are cheap, anyone can create them,
	// and refusing new ones when full would let a flood lock everyone out.
	a.sessions = newExpiringStore[Identity](maxLiveEntries, a.now, false)
	a.pending = newExpiringStore[pendingLogin](maxLiveEntries, a.now, true)

	// Lowest role first, so a group listed under two roles keeps the higher one.
	for _, m := range []struct {
		role   Role
		groups []string
	}{{RoleViewer, cfg.ViewerGroups}, {RolePlanner, cfg.PlannerGroups}, {RoleApplier, cfg.ApplierGroups}} {
		for _, g := range m.groups {
			if g = strings.TrimSpace(g); g != "" {
				a.groups[g] = m.role
			}
		}
	}
	if len(a.groups) == 0 {
		return nil, errors.New("no groups mapped to a role: set at least one of " + EnvViewerGroups + ", " + EnvPlannerGroups + ", " + EnvApplierGroups)
	}

	ctx = oidc.ClientContext(ctx, a.httpClient)
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s failed: %w", cfg.Issuer, err)
	}

	// A https issuer may still advertise cleartext endpoints (a proxy that
	// hides the scheme from the provider is the usual cause). The code
	// exchange would then send the code, verifier and secret in the clear.
	var disc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := provider.Claims(&disc); err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s: %w", cfg.Issuer, err)
	}
	ep := provider.Endpoint()
	for _, e := range []struct{ name, u string }{{"authorization_endpoint", ep.AuthURL}, {"token_endpoint", ep.TokenURL}, {"jwks_uri", disc.JWKSURI}} {
		if err := requireSecureURL("discovered "+e.name, e.u); err != nil {
			return nil, fmt.Errorf("OIDC discovery for %s: %w", cfg.Issuer, err)
		}
	}
	a.verifier = provider.VerifierContext(ctx, &oidc.Config{ClientID: cfg.ClientID, SupportedSigningAlgs: signingAlgs, Now: a.now})

	scopes := []string{"openid"}
	configured := false
	for _, s := range cfg.Scopes {
		if s = strings.TrimSpace(s); s != "" {
			configured = true
			if s != "openid" {
				scopes = append(scopes, s)
			}
		}
	}
	if !configured { // the default applies only when nothing was configured
		scopes = append(scopes, "profile")
	}
	a.oauth = oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  a.origin + callbackPath,
		Scopes:       scopes,
	}
	return a, nil
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}

// canonicalOrigin is scheme://host[:port] the way a browser serializes an
// Origin: lower-case, default port dropped. Both the configured public URL
// and an incoming Origin header go through it before being compared.
func canonicalOrigin(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// cookieName is the name a cookie is set and read under (see sessionCookie).
func (a *Auth) cookieName(base string) string {
	if a.secureCookies {
		return "__Host-" + base
	}
	return base
}

// setCookie sets or (maxAge < 0) clears a cookie with the attributes the
// __Host- prefix demands. Clearing must repeat them or the browser ignores it.
func (a *Auth) setCookie(w http.ResponseWriter, base, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(base), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: a.secureCookies, SameSite: http.SameSiteLaxMode})
}

// requireSecureURL insists on https, except for a loopback host (local
// development), so the authorization code and tokens never cross a
// network in clear text.
func requireSecureURL(what, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q is not a valid absolute URL", what, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("%s %q must be https (http is only allowed for localhost)", what, raw)
	}
	return fmt.Errorf("%s %q must be an https URL", what, raw)
}

// AuthFromEnv builds the Auth for `serve` from the HOMELAB_DEPLOY_*
// variables. insecure is the explicit --insecure-no-auth opt-out; with
// nothing configured and no opt-out it returns an error, so `serve`
// does not start open by omission.
func AuthFromEnv(ctx context.Context, getenv func(string) string, insecure bool) (*Auth, error) {
	all := []string{EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecretFile, EnvOIDCScopes, EnvOIDCGroupsClaim,
		EnvOIDCUsernameClaim, EnvPublicURL, EnvSessionTTL, EnvViewerGroups, EnvPlannerGroups, EnvApplierGroups}

	if insecure {
		for _, k := range all {
			if strings.TrimSpace(getenv(k)) != "" {
				return nil, fmt.Errorf("--insecure-no-auth conflicts with %s: remove one so it is clear whether auth is meant to be on", k)
			}
		}
		return &Auth{disabled: true}, nil
	}

	list := func(k string) []string { return strings.Split(getenv(k), ",") }
	cfg := AuthConfig{
		Issuer:        strings.TrimSpace(getenv(EnvOIDCIssuer)),
		ClientID:      strings.TrimSpace(getenv(EnvOIDCClientID)),
		PublicURL:     strings.TrimSpace(getenv(EnvPublicURL)),
		GroupsClaim:   getenv(EnvOIDCGroupsClaim),
		UsernameClaim: getenv(EnvOIDCUsernameClaim),
		Scopes:        strings.FieldsFunc(getenv(EnvOIDCScopes), func(r rune) bool { return r == ' ' || r == ',' }),
		ViewerGroups:  list(EnvViewerGroups),
		PlannerGroups: list(EnvPlannerGroups),
		ApplierGroups: list(EnvApplierGroups),
	}
	if v := strings.TrimSpace(getenv(EnvSessionTTL)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("%s=%q: want a positive duration like 8h", EnvSessionTTL, v)
		}
		cfg.SessionTTL = d
	}
	if f := strings.TrimSpace(getenv(EnvOIDCClientSecretFile)); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvOIDCClientSecretFile, err)
		}
		cfg.ClientSecret = strings.TrimSpace(string(b))
		if cfg.ClientSecret == "" {
			return nil, fmt.Errorf("%s: %s is empty", EnvOIDCClientSecretFile, f)
		}
	}

	a, err := NewAuth(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w\n(to run with no authentication at all, pass --insecure-no-auth)", err)
	}
	return a, nil
}

// wrap puts authentication in front of app and adds the /auth/* routes.
// With auth disabled it only attaches a full-access identity.
func (a *Auth) wrap(app http.Handler) http.Handler {
	if a.disabled {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), identityKey{}, Identity{User: "(auth disabled)", Role: RoleApplier})
			app.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	root := http.NewServeMux()
	root.HandleFunc("GET /auth/login", a.handleLogin)
	root.HandleFunc("GET "+callbackPath, a.handleCallback)
	root.Handle("POST /auth/logout", a.authenticate(http.HandlerFunc(a.handleLogout)))
	root.Handle("/", a.authenticate(app))
	return root
}

// authenticate resolves the caller's identity from their session before
// any route runs, and fails closed:
//
//  1. No valid session: a browser navigation is sent to log in; anything
//     else (a POST, the SSE stream, a script) gets 401 rather than an HTML
//     redirect it cannot follow.
//  2. A state-changing request must be same-origin (see sameOrigin), since
//     the session cookie is sent automatically with cross-site requests.
func (a *Auth) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.identify(r)
		if !ok {
			a.unauthenticated(w, r)
			return
		}
		if !safeMethod(r.Method) && !a.sameOrigin(r) {
			log.Printf("auth denied: user=%q sub=%q cross-origin %s %q (Origin=%q Sec-Fetch-Site=%q)", id.User, id.Subject, r.Method, r.URL.Path, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), identityKey{}, id)
		// Long-lived responses (the SSE stream) re-check this, so they end
		// when the session does instead of outliving a logout or expiry.
		ctx = context.WithValue(ctx, sessionCheckKey{}, func() bool { _, ok := a.identify(r); return ok })
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// identify maps a request to an Identity. The session cookie is the only
// source today. A bearer-token path (for example an MCP client sending an
// access token) belongs here: verify the token with a.verifier, map its
// groups through a.roleFor, and return the same Identity, so require(),
// the audit log and the roles need no change.
func (a *Auth) identify(r *http.Request) (Identity, bool) {
	c, err := r.Cookie(a.cookieName(sessionCookie))
	if err != nil || c.Value == "" {
		return Identity{}, false
	}
	return a.sessions.get(c.Value)
}

func (a *Auth) unauthenticated(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	http.Error(w, "unauthenticated: sign in at /auth/login", http.StatusUnauthorized)
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin is the CSRF check. Browsers set Sec-Fetch-Site themselves
// and page script cannot forge it; older ones still send Origin on
// POSTs. A request carrying neither is refused (fail closed): the
// dashboard is only ever driven from a browser.
func (a *Auth) sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin"
	}
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false // absent, "null", or not an origin
	}
	return canonicalOrigin(u) == a.origin
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	p := pendingLogin{nonce: randToken(), verifier: oauth2.GenerateVerifier(), next: safeNext(r.URL.Query().Get("next"))}
	state := randToken()
	if !a.pending.put(state, p, loginStateTTL) {
		http.Error(w, "too many logins in progress, try again shortly", http.StatusServiceUnavailable)
		return
	}
	// The state is also bound to this browser by a cookie, so a callback
	// URL an attacker prepared cannot be completed by a victim (login CSRF).
	a.setCookie(w, loginCookie, state, int(loginStateTTL.Seconds()))
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(p.nonce), oauth2.S256ChallengeOption(p.verifier)), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")

	c, cerr := r.Cookie(a.cookieName(loginCookie))
	if cerr != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		// Left alone on purpose: the cookie belongs to a different login the
		// browser started since (another tab), which this stale callback must not destroy.
		log.Printf("login failed: state cookie missing or mismatched (from %s)", r.RemoteAddr)
		http.Error(w, "login failed: start again at /auth/login", http.StatusBadRequest)
		return
	}
	// This callback owns the cookie: it, and the pending state, are single use whatever happens next.
	a.setCookie(w, loginCookie, "", -1)
	p, ok := a.pending.take(state)
	if !ok {
		log.Printf("login failed: unknown, expired or replayed state (from %s)", r.RemoteAddr)
		http.Error(w, "login failed: start again at /auth/login", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		log.Printf("login refused by provider: error=%q description=%q", e, q.Get("error_description"))
		http.Error(w, "login refused by the identity provider", http.StatusForbidden)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "login failed: no authorization code", http.StatusBadRequest)
		return
	}

	ctx := oidc.ClientContext(r.Context(), a.httpClient)
	tok, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(p.verifier))
	if err != nil {
		log.Printf("login failed: code exchange: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		log.Printf("login failed: provider returned no id_token")
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	idt, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		log.Printf("login failed: id_token rejected: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(p.nonce)) != 1 {
		log.Printf("login failed: id_token nonce mismatch (sub=%q)", idt.Subject)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		log.Printf("login failed: reading claims: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}

	user := a.username(claims, idt.Subject)
	role := a.roleFor(claimStrings(claims[a.groupsClaim]))
	if role == RoleNone {
		// The provider no longer grants this account any role: end every
		// session it holds (this browser's and other devices'), so a group
		// removal takes effect at the next login attempt instead of at
		// each old session's expiry.
		n := a.sessions.deleteWhere(func(i Identity) bool { return i.Subject == idt.Subject })
		a.setCookie(w, sessionCookie, "", -1)
		log.Printf("login denied: user=%q sub=%q is in no group mapped to a role (claim %q); revoked %d session(s)", user, idt.Subject, a.groupsClaim, n)
		http.Error(w, "forbidden: your account is not in any group allowed to use this server", http.StatusForbidden)
		return
	}

	// A new login replaces the session this browser presented (so re-login
	// really drops the old privileges and uses no extra slot), and one account
	// holds at most maxSessionsPerSubject, so it cannot fill the store alone.
	presented := ""
	if pc, err := r.Cookie(a.cookieName(sessionCookie)); err == nil {
		presented = pc.Value
	}
	sid := randToken()
	if !a.sessions.replace(presented, sid, Identity{User: user, Subject: idt.Subject, Role: role}, a.ttl,
		func(i Identity) bool { return i.Subject == idt.Subject }, maxSessionsPerSubject) {
		http.Error(w, "too many sessions, try again shortly", http.StatusServiceUnavailable)
		return
	}
	a.setCookie(w, sessionCookie, sid, int(a.ttl.Seconds()))
	log.Printf("login: user=%q sub=%q role=%s", user, idt.Subject, role)
	http.Redirect(w, r, p.next, http.StatusSeeOther)
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(a.cookieName(sessionCookie)); err == nil {
		a.sessions.delete(c.Value)
	}
	a.setCookie(w, sessionCookie, "", -1)
	who := identityFrom(r.Context())
	log.Printf("logout: user=%q sub=%q", who.User, who.Subject)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><title>Signed out</title><p>Signed out. <a href="/auth/login">Sign in again</a></p>`)
}

// username prefers the configured claim, then email, then the subject.
func (a *Auth) username(claims map[string]any, sub string) string {
	for _, k := range []string{a.usernameClaim, "email"} {
		if s, ok := claims[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return sub
}

// claimStrings reads a groups-style claim: a JSON array of strings, or a
// single string. Anything else yields no groups (and so no role).
func claimStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// roleFor returns the highest role any of the user's groups maps to.
// Groups are matched exactly: case-sensitive and untrimmed, so " admins "
// (a different group to the provider) is not "admins". Only the configured
// names are trimmed, once, when the mapping is built.
func (a *Auth) roleFor(groups []string) Role {
	best := RoleNone
	for _, g := range groups {
		if r := a.groups[g]; r > best {
			best = r
		}
	}
	return best
}

// safeNext only lets a login return to a path on this server, never
// another site (open redirect) or back into the login flow.
//
// Backslashes are refused anywhere: http.Redirect runs path.Clean on the
// target, so "/a/../\\evil.example" would come out as "/\\evil.example", which
// browsers read as "//evil.example". The length cap bounds what each pending
// login retains, and the result is copied so it cannot pin a larger request buffer.
func safeNext(next string) string {
	if next == "" || len(next) > maxNextLen || next[0] != '/' || strings.Contains(next, "\\") ||
		strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/auth/") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	for _, r := range next {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	return strings.Clone(next)
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no entropy: nothing safe to do
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// expiringStore is a small bounded in-memory map whose entries lapse.
type expiringStore[T any] struct {
	mu          sync.Mutex
	m           map[string]storeEntry[T]
	max         int
	now         func() time.Time
	evictOldest bool // when full of live entries: drop the oldest instead of refusing
}

type storeEntry[T any] struct {
	v   T
	exp time.Time
}

func newExpiringStore[T any](max int, now func() time.Time, evictOldest bool) *expiringStore[T] {
	return &expiringStore[T]{m: map[string]storeEntry[T]{}, max: max, now: now, evictOldest: evictOldest}
}

// put stores v, first dropping lapsed entries. When the store is still full
// it evicts the oldest entry (evictOldest) or reports false.
func (s *expiringStore[T]) put(key string, v T, ttl time.Duration) bool {
	return s.replace("", key, v, ttl, nil, 0)
}

// replace atomically removes the entry at old (if any), keeps at most
// perGroup live entries for which same reports true by dropping the oldest of
// them, then stores v as put does. Removing old first means swapping one's own
// entry never needs a free slot.
func (s *expiringStore[T]) replace(old, key string, v T, ttl time.Duration, same func(T) bool, perGroup int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if old != "" {
		delete(s.m, old)
	}
	if same != nil && perGroup > 0 {
		for {
			n, oldest, oldestExp := 0, "", time.Time{}
			for k, e := range s.m {
				if !now.Before(e.exp) {
					delete(s.m, k)
					continue
				}
				if same(e.v) {
					n++
					if oldest == "" || e.exp.Before(oldestExp) {
						oldest, oldestExp = k, e.exp
					}
				}
			}
			if n < perGroup {
				break
			}
			delete(s.m, oldest)
		}
	}
	if len(s.m) >= s.max {
		for k, e := range s.m {
			if !now.Before(e.exp) {
				delete(s.m, k)
			}
		}
	}
	if len(s.m) >= s.max {
		if !s.evictOldest {
			return false
		}
		var oldest string
		var oldestExp time.Time
		for k, e := range s.m {
			if oldest == "" || e.exp.Before(oldestExp) {
				oldest, oldestExp = k, e.exp
			}
		}
		delete(s.m, oldest)
	}
	s.m[key] = storeEntry[T]{v: v, exp: now.Add(ttl)}
	return true
}

// deleteWhere removes every entry for which match is true and returns how many.
func (s *expiringStore[T]) deleteWhere(match func(T) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, e := range s.m {
		if match(e.v) {
			delete(s.m, k)
			n++
		}
	}
	return n
}

func (s *expiringStore[T]) get(key string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if ok && !s.now().Before(e.exp) {
		delete(s.m, key)
		return e.v, false
	}
	return e.v, ok
}

// take is a single-use lookup. Lookup, expiry check and delete happen under
// one lock, so two concurrent callers can never both receive the same entry.
func (s *expiringStore[T]) take(key string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	delete(s.m, key)
	if ok && !s.now().Before(e.exp) {
		ok = false
	}
	return e.v, ok
}

func (s *expiringStore[T]) delete(key string) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// require gates a route on a minimum role. authenticate has already
// run, so a request reaching here has an identity; this is the RBAC step.
func require(min Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id := identityFrom(r.Context()); id.Role < min {
			log.Printf("auth denied: user=%q sub=%q role=%s needs %s for %s %q", id.User, id.Subject, id.Role, min, r.Method, r.URL.Path)
			http.Error(w, "forbidden: requires the "+min.String()+" role", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// roleForKind is the role a run of this kind needs. Only apply changes
// real infrastructure through this tool; plan, init and sync do not. That
// is a statement about what the buttons do, not a sandbox: terraform
// executes provider and data-source code at plan time with the server's own
// credentials, so a planner (or anyone who can change the repository that
// gets planned) is trusted to that extent. See the README.
func roleForKind(kind string) Role {
	if kind == "apply" {
		return RoleApplier
	}
	return RolePlanner
}
