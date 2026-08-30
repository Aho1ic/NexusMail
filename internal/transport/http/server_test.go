//go:build sqlite_fts5

package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/repository/sqlite"
	sessionservice "nexusmail/internal/service/session"

	"github.com/gin-gonic/gin"
)

const testAPIKey = "0123456789abcdef0123456789abcdef0123"

// TestAuthenticateRejectsAnonymous asserts the middleware fails closed. Every
// handler behind it assumes an authenticated caller, so a gap here is not a missing
// check but an open mailbox.
func TestAuthenticateRejectsAnonymous(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, "/probe", nil))
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials returned %d, want 401", method, response.Code)
		}
	}
}

func TestAuthenticateAcceptsAPIKey(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)

	request := httptest.NewRequest(http.MethodPost, "/probe", nil)
	request.Header.Set("X-API-Key", testAPIKey)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid API key returned %d", response.Code)
	}
	// The API key channel is for external clients that cannot hold a cookie, so it
	// deliberately skips CSRF. Recording that here means removing the skip has to be
	// a deliberate decision rather than an accident.
	if got := response.Body.String(); got != "api_key" {
		t.Fatalf("auth_method = %q, want api_key", got)
	}
}

func TestAuthenticateRejectsWrongAPIKey(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)
	request := httptest.NewRequest(http.MethodGet, "/probe", nil)
	request.Header.Set("X-API-Key", strings.Repeat("z", len(testAPIKey)))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong API key returned %d", response.Code)
	}
}

// TestAuthenticateSessionRequiresCSRF covers the cookie channel: the cookie alone
// authenticates a read, but a state change also needs the header, which is what
// stops a cross-site form from acting as the user.
func TestAuthenticateSessionRequiresCSRF(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)
	token, csrf := newSession(t, server)

	get := httptest.NewRequest(http.MethodGet, "/probe", nil)
	get.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, get)
	if response.Code != http.StatusOK {
		t.Fatalf("GET with session cookie returned %d", response.Code)
	}

	post := httptest.NewRequest(http.MethodPost, "/probe", nil)
	post.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, post)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("POST without CSRF token returned %d, want 401", response.Code)
	}

	post = httptest.NewRequest(http.MethodPost, "/probe", nil)
	post.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
	post.Header.Set("X-CSRF-Token", csrf)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, post)
	if response.Code != http.StatusOK {
		t.Fatalf("POST with CSRF token returned %d", response.Code)
	}

	post = httptest.NewRequest(http.MethodPost, "/probe", nil)
	post.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
	post.Header.Set("X-CSRF-Token", "not-the-token")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, post)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("POST with wrong CSRF token returned %d, want 401", response.Code)
	}
}

// TestAuthenticateChecksOrigin is the second CSRF layer: the token can leak, the
// origin check cannot be replayed from another site.
func TestAuthenticateChecksOrigin(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)
	token, csrf := newSession(t, server)

	cases := []struct {
		origin string
		status int
	}{
		{"", http.StatusOK},                               // non-browser client sends no Origin
		{"http://localhost:13737", http.StatusOK},         // exactly the public URL
		{"HTTP://LOCALHOST:13737", http.StatusOK},         // host comparison is case-insensitive
		{"https://localhost:13737", http.StatusForbidden}, // scheme must match
		{"http://evil.example", http.StatusForbidden},
		{"http://localhost:13738", http.StatusForbidden}, // port is part of the host
		{"null", http.StatusForbidden},                   // sandboxed iframe
	}
	for _, item := range cases {
		request := httptest.NewRequest(http.MethodPost, "/probe", nil)
		request.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
		request.Header.Set("X-CSRF-Token", csrf)
		if item.origin != "" {
			request.Header.Set("Origin", item.origin)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != item.status {
			t.Errorf("Origin %q returned %d, want %d", item.origin, response.Code, item.status)
		}
	}
}

func TestAuthenticateRejectsUnknownSessionToken(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)
	request := httptest.NewRequest(http.MethodGet, "/probe", nil)
	request.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: "fabricated"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unknown session token returned %d", response.Code)
	}
}

// TestAuthenticateAPIKeyShortCircuitsSession pins the precedence rules
// CLAUDE.md requires: a present X-API-Key is the only thing the cookie and
// CSRF/Origin paths ever see, and a wrong X-API-Key must not fall through to
// the cookie channel.
func TestAuthenticateAPIKeyShortCircuitsSession(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)
	token, _ := newSession(t, server)

	// Both valid: API key wins, and CSRF is intentionally skipped.
	both := httptest.NewRequest(http.MethodPost, "/probe", nil)
	both.Header.Set("X-API-Key", testAPIKey)
	both.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
	// No X-CSRF-Token and no Origin: would fail the cookie path, but the
	// request is API-key authenticated.
	response := httptest.NewRecorder()
	router.ServeHTTP(response, both)
	if response.Code != http.StatusOK {
		t.Fatalf("valid API key + valid session returned %d, want 200 (API key wins, CSRF skipped)", response.Code)
	}
	if got := response.Body.String(); got != "api_key" {
		t.Fatalf("auth_method = %q, want api_key", got)
	}

	// Wrong API key with a valid session must be 401: the cookie channel is
	// not consulted when the X-API-Key header is present.
	wrong := httptest.NewRequest(http.MethodGet, "/probe", nil)
	wrong.Header.Set("X-API-Key", strings.Repeat("z", len(testAPIKey)))
	wrong.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, wrong)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong API key + valid session returned %d, want 401 (no fallthrough)", response.Code)
	}

	// An empty X-API-Key header is the same as no header: the cookie path
	// must be tried. Use a wrong API key with CSRF enforced but no header set.
	empty := httptest.NewRequest(http.MethodPost, "/probe", nil)
	empty.Header.Set("X-API-Key", "")
	empty.AddCookie(&http.Cookie{Name: sessionservice.CookieName, Value: token})
	empty.Header.Set("X-CSRF-Token", "anything")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, empty)
	// Cookie path then fails on missing CSRF (because the request was POST),
	// not on a fake key; that proves the empty header was treated as absent
	// rather than as a wrong key.
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("empty X-API-Key returned %d, want 401 (cookie path tried)", response.Code)
	}
}

// TestLoginRateLimit bounds guessing on the one endpoint that takes the API key as
// a body parameter.
func TestLoginRateLimit(t *testing.T) {
	server := newTestServer(t)
	router := server.routes()

	for attempt := 1; attempt <= loginRateLimit; attempt++ {
		if code := postLogin(router, "wrong-key", ""); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d returned %d, want 401", attempt, code)
		}
	}
	if code := postLogin(router, "wrong-key", ""); code != http.StatusTooManyRequests {
		t.Fatalf("attempt past the limit returned %d, want 429", code)
	}
	// The limit is on attempts, not on failures, so a correct key offered after the
	// budget is spent is still refused. That is the intended trade: the alternative
	// lets an attacker probe indefinitely as long as they occasionally succeed.
	if code := postLogin(router, testAPIKey, ""); code != http.StatusTooManyRequests {
		t.Fatalf("valid key after limit returned %d, want 429", code)
	}
}

// TestLoginRateLimitIgnoresForwardedFor is the reason trusted proxies are no longer
// wide open: with the default Gin configuration the bucket key came from an
// attacker-supplied header, so every request could claim a fresh budget.
func TestLoginRateLimitIgnoresForwardedFor(t *testing.T) {
	server := newTestServer(t)
	router := server.routes()

	for attempt := 1; attempt <= loginRateLimit; attempt++ {
		if code := postLogin(router, "wrong-key", "10.0.0."+string(rune('1'+attempt))); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d returned %d, want 401", attempt, code)
		}
	}
	if code := postLogin(router, "wrong-key", "203.0.113.9"); code != http.StatusTooManyRequests {
		t.Fatalf("forged X-Forwarded-For bought a new budget: got %d", code)
	}
}

// TestAPIKeyRateLimit closes the asymmetry the review found: the header channel was
// unlimited while the login endpoint was capped, so guessing simply moved.
func TestAPIKeyRateLimit(t *testing.T) {
	server := newTestServer(t)
	router := probeRouter(server)

	probe := func(key string) int {
		request := httptest.NewRequest(http.MethodGet, "/probe", nil)
		request.Header.Set("X-API-Key", key)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}
	for attempt := 1; attempt <= apiKeyRateLimit; attempt++ {
		if code := probe("wrong-key"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d returned %d, want 401", attempt, code)
		}
	}
	if code := probe("wrong-key"); code != http.StatusTooManyRequests {
		t.Fatalf("attempt past the limit returned %d, want 429", code)
	}
	// Only failures are counted, so a working integration is never throttled by
	// another client's guessing on the same address.
	if code := probe(testAPIKey); code != http.StatusOK {
		t.Fatalf("valid key throttled: got %d", code)
	}
}

// TestRateBucketsAreReclaimed pins the memory behaviour. The key is caller
// controlled, so buckets that are never revisited have to be dropped or the map is
// an unbounded allocation an anonymous client can drive.
func TestRateBucketsAreReclaimed(t *testing.T) {
	server := newTestServer(t)

	for index := 0; index < 50; index++ {
		server.allowAttempt("login:198.51.100."+string(rune('0'+index%10))+string(rune('a'+index/10)), loginRateLimit)
	}
	server.rateMu.Lock()
	seeded := len(server.rate)
	// Age every bucket past the window, and move the last sweep back far enough that
	// the next call is due for one: the sweep is rate limited so the common path does
	// not walk the map on every request.
	stale := time.Now().Add(-2 * rateSweepEvery)
	for key := range server.rate {
		server.rate[key] = []time.Time{stale}
	}
	server.rateSwept = stale
	server.rateMu.Unlock()

	server.allowAttempt("login:203.0.113.1", loginRateLimit)

	server.rateMu.Lock()
	defer server.rateMu.Unlock()
	if seeded < 10 {
		t.Fatalf("seeded only %d buckets, test is not measuring anything", seeded)
	}
	if len(server.rate) != 1 {
		t.Fatalf("stale buckets survived: %d entries remain", len(server.rate))
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	server := newTestServer(t)
	router := server.routes()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := response.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Error("X-Request-ID missing")
	}
}

// TestSessionCookieIsHardened checks the flags rather than the value: HttpOnly and
// SameSite are what keep the token out of reach of injected script and cross-site
// requests.
func TestSessionCookieIsHardened(t *testing.T) {
	server := newTestServer(t)
	router := server.routes()

	response := httptest.NewRecorder()
	router.ServeHTTP(response, loginRequest(testAPIKey, ""))
	if response.Code != http.StatusCreated {
		t.Fatalf("login returned %d: %s", response.Code, response.Body.String())
	}
	var cookie *http.Cookie
	for _, item := range response.Result().Cookies() {
		if item.Name == sessionservice.CookieName {
			cookie = item
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie issued")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("session cookie path = %q", cookie.Path)
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	// The CSRF token has to travel in the body, not in a cookie: a cookie would be
	// sent by the cross-site request the token exists to stop.
	if payload.CSRFToken == "" {
		t.Error("no CSRF token in the login response")
	}
	if strings.Contains(response.Header().Get("Set-Cookie"), payload.CSRFToken) {
		t.Error("CSRF token was also set as a cookie")
	}
}

func TestSecureCookieFollowsPublicURLScheme(t *testing.T) {
	server := newTestServer(t)
	server.cfg.PublicURL = "https://mail.example.com"
	router := server.routes()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, loginRequest(testAPIKey, ""))
	for _, item := range response.Result().Cookies() {
		if item.Name == sessionservice.CookieName && !item.Secure {
			t.Fatal("cookie is not Secure under an https public URL")
		}
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		origin string
		public string
		want   bool
	}{
		{"", "http://localhost:13737", true},
		{"http://localhost:13737", "http://localhost:13737", true},
		{"http://LocalHost:13737", "http://localhost:13737", true},
		{"http://localhost:13737", "https://localhost:13737", false},
		{"http://localhost", "http://localhost:13737", false},
		// Origin serialisation omits default ports, while PublicURL is commonly
		// configured with one. They still name exactly the same origin.
		{"https://mail.example", "https://mail.example:443", true},
		{"https://mail.example:443", "https://mail.example", true},
		{"http://mail.example", "http://mail.example:80", true},
		{"http://mail.example:443", "http://mail.example", false},
		{"HTTPS://MAIL.EXAMPLE", "https://mail.example:443", true},
		// url.Parse accepts all of these, but none can be an Origin header: accepting
		// one would make our CSRF boundary looser than the browser's own model.
		{"https://mail.example/path", "https://mail.example", false},
		{"https://mail.example?next=https://attacker.example", "https://mail.example", false},
		{"https://user@mail.example", "https://mail.example", false},
		{"http://attacker.example", "http://localhost:13737", false},
		{"not a url at all", "http://localhost:13737", false},
	}
	for _, item := range cases {
		request := httptest.NewRequest(http.MethodPost, "/", nil)
		if item.origin != "" {
			request.Header.Set("Origin", item.origin)
		}
		if got := sameOrigin(request, item.public); got != item.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", item.origin, item.public, got, item.want)
		}
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo, err := sqlite.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	cfg := config.Config{PublicURL: "http://localhost:13737", APIKey: testAPIKey}
	sessions := sessionservice.New(repo, testAPIKey, time.Hour, 24*time.Hour)
	return &Server{cfg: cfg, repo: repo, sessions: sessions, appCtx: context.Background(), rate: make(map[string][]time.Time)}
}

// probeRouter mounts the real middleware in front of a handler that only reports
// how the caller was authenticated, so the tests exercise the authentication path
// without dragging in the services each endpoint needs.
func probeRouter(server *Server) *gin.Engine {
	router := gin.New()
	_ = router.SetTrustedProxies(nil)
	router.Use(gin.Recovery(), requestID(), securityHeaders())
	group := router.Group("/probe")
	group.Use(server.authenticate())
	handler := func(c *gin.Context) { c.String(http.StatusOK, c.GetString("auth_method")) }
	for _, register := range []func(string, ...gin.HandlerFunc) gin.IRoutes{group.GET, group.POST, group.PATCH, group.DELETE} {
		register("", handler)
	}
	return router
}

func newSession(t *testing.T, server *Server) (token, csrf string) {
	t.Helper()
	token, csrf, _, err := server.sessions.Create(context.Background(), testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	return token, csrf
}

func loginRequest(apiKey, forwardedFor string) *http.Request {
	body := strings.NewReader(`{"api_key":"` + apiKey + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session", body)
	request.Header.Set("Content-Type", "application/json")
	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}
	return request
}

func postLogin(router *gin.Engine, apiKey, forwardedFor string) int {
	response := httptest.NewRecorder()
	router.ServeHTTP(response, loginRequest(apiKey, forwardedFor))
	return response.Code
}
