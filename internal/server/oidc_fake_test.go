package server

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// testClock is the one clock shared by the fake provider (iat/exp) and
// the Auth under test (expiry checks, session and login-state lifetime),
// so expiry tests advance time instead of sleeping.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)} }
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeOP is a minimal but real OpenID provider: discovery, JWKS, and a
// token endpoint that enforces the client secret, single-use codes, the
// redirect URI and PKCE (S256). It issues RS256 ID tokens that tests can
// tamper with, so the server's checks are exercised against genuine
// protocol messages rather than mocks of its own functions.
type fakeOP struct {
	t        *testing.T
	srv      *httptest.Server
	clock    *testClock
	key      *rsa.PrivateKey
	otherKey *rsa.PrivateKey // a key the JWKS does not publish
	clientID string
	secret   string // "" = public client: no secret required

	mu    sync.Mutex
	codes map[string]*opCode
}

type opCode struct {
	claims      map[string]any
	challenge   string
	redirectURI string
	used        bool
	sign        string // "", "other" (wrong key), "none", "hs256"
}

func newFakeOP(t *testing.T, clock *testClock, clientID, secret string) *fakeOP {
	t.Helper()
	k1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	op := &fakeOP{t: t, clock: clock, key: k1, otherKey: k2, clientID: clientID, secret: secret, codes: map[string]*opCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                op.srv.URL,
			"authorization_endpoint":                op.srv.URL + "/authorize",
			"token_endpoint":                        op.srv.URL + "/token",
			"jwks_uri":                              op.srv.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "k1",
			"n": b64(k1.N.Bytes()), "e": b64(big.NewInt(int64(k1.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", op.handleToken)
	// A user who is already signed in and consents: approve at once and
	// bounce back to the client, like a browser following redirects would see.
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		code := op.approve(op.srv.URL+r.URL.RequestURI(), nil, "")
		back, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		q := back.Query()
		q.Set("code", code)
		q.Set("state", r.URL.Query().Get("state"))
		back.RawQuery = q.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	op.srv = httptest.NewServer(mux) // loopback http: allowed by the server's https rule
	t.Cleanup(op.srv.Close)
	return op
}

func (op *fakeOP) URL() string { return op.srv.URL }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// approve plays "the user logged in and consented": given the
// authorization URL the server redirected the browser to, it records a
// code that the token endpoint will redeem for an ID token carrying
// default claims for alice, after tamper has edited them.
func (op *fakeOP) approve(authURL string, tamper func(claims map[string]any), sign string) string {
	op.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		op.t.Fatal(err)
	}
	q := u.Query()
	now := op.clock.Now()
	claims := map[string]any{
		"iss": op.srv.URL, "aud": op.clientID, "sub": "sub-alice",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"nonce":              q.Get("nonce"),
		"preferred_username": "alice",
		"groups":             []string{"deploy-appliers"},
	}
	if tamper != nil {
		tamper(claims)
	}
	code := b64(randBytes(16))
	op.mu.Lock()
	op.codes[code] = &opCode{claims: claims, challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), sign: sign}
	op.mu.Unlock()
	return code
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func (op *fakeOP) handleToken(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": msg})
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		fail(400, "unsupported_grant_type")
		return
	}
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != op.clientID || (op.secret != "" && secret != op.secret) {
		fail(401, "invalid_client")
		return
	}
	op.mu.Lock()
	c, ok := op.codes[r.PostForm.Get("code")]
	if ok && c.used {
		ok = false
	}
	if ok {
		c.used = true
	}
	op.mu.Unlock()
	if !ok || c.redirectURI != r.PostForm.Get("redirect_uri") {
		fail(400, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if c.challenge == "" || b64(sum[:]) != c.challenge {
		fail(400, "invalid_grant") // PKCE verifier does not match the challenge
		return
	}
	writeJSON(w, map[string]any{
		"access_token": "access-" + b64(randBytes(6)), "token_type": "Bearer", "expires_in": 300,
		"id_token": op.mint(c.claims, c.sign),
	})
}

// mint builds a compact JWT. sign selects how it is (mis)signed.
func (op *fakeOP) mint(claims map[string]any, sign string) string {
	alg := "RS256"
	switch sign {
	case "none":
		alg = "none"
	case "hs256":
		alg = "HS256"
	}
	hdr, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(claims)
	signing := b64(hdr) + "." + b64(body)
	var sig []byte
	switch sign {
	case "none":
	case "hs256":
		m := hmac.New(sha256.New, []byte(op.secret))
		m.Write([]byte(signing))
		sig = m.Sum(nil)
	default:
		key := op.key
		if sign == "other" {
			key = op.otherKey
		}
		sum := sha256.Sum256([]byte(signing))
		var err error
		if sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:]); err != nil {
			op.t.Fatal(err)
		}
	}
	return signing + "." + b64(sig)
}
