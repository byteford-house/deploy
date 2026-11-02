package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
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

// The identity headers set by the Authentik proxy outpost (forward auth
// through Traefik). They are only believed when the request arrives from
// a trusted proxy address — see Auth.authenticate.
const (
	headerUser   = "X-authentik-username"
	headerGroups = "X-authentik-groups" // pipe-separated: "foo|bar|baz"
)

// Environment variables read by AuthFromEnv.
const (
	EnvTrustedProxies = "HOMELAB_DEPLOY_TRUSTED_PROXIES" // comma-separated CIDRs or IPs
	EnvViewerGroups   = "HOMELAB_DEPLOY_VIEWER_GROUPS"   // comma-separated group names
	EnvPlannerGroups  = "HOMELAB_DEPLOY_PLANNER_GROUPS"
	EnvApplierGroups  = "HOMELAB_DEPLOY_APPLIER_GROUPS"
)

// AuthConfig is the header-trust configuration: who the proxy is, and
// which Authentik groups map to which role.
type AuthConfig struct {
	TrustedProxies []string // CIDRs or bare IPs the identity headers are believed from
	ViewerGroups   []string
	PlannerGroups  []string
	ApplierGroups  []string
}

// Auth decides who a request is and what role they have. The zero value
// is not usable; build one with NewAuth, AuthFromEnv or insecureNoAuth.
// newServer refuses a nil *Auth, so serving with no auth decision made
// is impossible rather than merely discouraged.
type Auth struct {
	disabled bool
	proxies  []netip.Prefix
	groups   map[string]Role
}

// NewAuth validates cfg and fails closed: no trusted proxy, a
// trust-everyone range, or no group mapped to any role is an error, not
// a server that quietly lets everyone in (or no one).
func NewAuth(cfg AuthConfig) (*Auth, error) {
	a := &Auth{groups: map[string]Role{}}

	for _, raw := range cfg.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		p, err := parseProxy(raw)
		if err != nil {
			return nil, err
		}
		a.proxies = append(a.proxies, p)
	}
	if len(a.proxies) == 0 {
		return nil, errors.New("no trusted proxy configured: the identity headers are only safe to believe from the proxy, so set " + EnvTrustedProxies + " to the proxy's address or CIDR")
	}

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
	return a, nil
}

func parseProxy(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return p, fmt.Errorf("trusted proxy %q: %w", s, err)
		}
	} else {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return p, fmt.Errorf("trusted proxy %q: not an IP address or CIDR", s)
		}
		p = netip.PrefixFrom(ip, ip.BitLen())
	}
	p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()) // never compare a v4-mapped v6 prefix
	if p.Bits() == 0 {
		return p, fmt.Errorf("trusted proxy %q trusts every address, which defeats the check", s)
	}
	return p.Masked(), nil
}

// AuthFromEnv builds the Auth for `serve` from the HOMELAB_DEPLOY_*
// variables. insecure is the explicit --insecure-no-auth opt-out; with
// nothing configured and no opt-out it returns an error, so `serve`
// does not start open by omission.
func AuthFromEnv(getenv func(string) string, insecure bool) (*Auth, error) {
	list := func(k string) []string { return strings.Split(getenv(k), ",") }
	cfg := AuthConfig{
		TrustedProxies: list(EnvTrustedProxies),
		ViewerGroups:   list(EnvViewerGroups),
		PlannerGroups:  list(EnvPlannerGroups),
		ApplierGroups:  list(EnvApplierGroups),
	}

	if insecure {
		for _, k := range []string{EnvTrustedProxies, EnvViewerGroups, EnvPlannerGroups, EnvApplierGroups} {
			if strings.TrimSpace(getenv(k)) != "" {
				return nil, fmt.Errorf("--insecure-no-auth conflicts with %s: remove one so it is clear whether auth is meant to be on", k)
			}
		}
		return &Auth{disabled: true}, nil
	}

	a, err := NewAuth(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w\n(to run with no authentication at all, pass --insecure-no-auth)", err)
	}
	return a, nil
}

// Identity is who the proxy says made the request, and their role.
type Identity struct {
	User string
	Role Role
}

type identityKey struct{}

func identityFrom(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id // zero value: no user, RoleNone — denies everything
}

// authenticate resolves the caller's identity before any route runs. It
// fails closed at each step:
//
//  1. The TCP peer must be a trusted proxy. Anyone else can send
//     whatever headers they like, so their headers (and the request) are
//     refused outright. X-Forwarded-For is deliberately not consulted.
//  2. Exactly one non-empty username header must be present. Zero is an
//     unauthenticated request that got past the proxy (a misrouted
//     request); two means a client-supplied value sits next to the real
//     one and there is no telling which is which.
//  3. The groups must map to at least one role.
func (a *Auth) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.disabled {
			ctx := context.WithValue(r.Context(), identityKey{}, Identity{User: "(auth disabled)", Role: RoleApplier})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		if !a.fromTrustedProxy(r.RemoteAddr) {
			log.Printf("auth denied: untrusted source %q for %s %s", r.RemoteAddr, r.Method, r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		users := r.Header.Values(headerUser)
		if len(users) != 1 || strings.TrimSpace(users[0]) == "" {
			log.Printf("auth denied: missing identity (%d %s values) from %q for %s %s", len(users), headerUser, r.RemoteAddr, r.Method, r.URL.Path)
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		user := strings.TrimSpace(users[0])

		role := a.roleFor(r.Header.Values(headerGroups))
		if role == RoleNone {
			log.Printf("auth denied: user=%q is in no group mapped to a role (%s %s)", user, r.Method, r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), identityKey{}, Identity{User: user, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Auth) fromTrustedProxy(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range a.proxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// roleFor returns the highest role any of the user's groups maps to.
// Groups are matched exactly (case-sensitive), split on Authentik's pipe.
func (a *Auth) roleFor(groupHeaders []string) Role {
	best := RoleNone
	for _, h := range groupHeaders {
		for _, g := range strings.Split(h, "|") {
			if r := a.groups[strings.TrimSpace(g)]; r > best {
				best = r
			}
		}
	}
	return best
}

// require gates a route on a minimum role. authenticate has already
// run, so a request reaching here has an identity; this is the RBAC step.
func require(min Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id := identityFrom(r.Context()); id.Role < min {
			log.Printf("auth denied: user=%q role=%s needs %s for %s %s", id.User, id.Role, min, r.Method, r.URL.Path)
			http.Error(w, "forbidden: requires the "+min.String()+" role", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// roleForKind is the role a run of this kind needs. Only apply changes
// real infrastructure; plan is read-only, init and sync touch local
// state/checkouts only.
func roleForKind(kind string) Role {
	if kind == "apply" {
		return RoleApplier
	}
	return RolePlanner
}
