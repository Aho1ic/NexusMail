package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	"nexusmail/internal/provider/auth"

	"golang.org/x/oauth2"
)

// The provider endpoints are compiled in, which is correct for production — a
// configurable token URL is a phishing surface — so the tests inject a transport
// through the context instead. oauth2 honours oauth2.HTTPClient, and both the token
// exchange and the userinfo fetch run through the client it builds, so the real
// code path is exercised with no network and no test-only hook in the manager.
type stubTransport struct {
	mu sync.Mutex

	tokenStatus  int
	tokenBody    string
	userStatus   int
	userBody     string
	tokenForms   []url.Values
	authHeaders  []string
	requests     []string
	transportErr error
}

func (s *stubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request.URL.String())
	if s.transportErr != nil {
		return nil, s.transportErr
	}
	switch {
	case strings.Contains(request.URL.Host, "oauth2.googleapis.com"), strings.Contains(request.URL.Path, "/token"):
		if request.Body != nil {
			raw, _ := io.ReadAll(request.Body)
			if form, err := url.ParseQuery(string(raw)); err == nil {
				s.tokenForms = append(s.tokenForms, form)
			}
		}
		status, body := s.tokenStatus, s.tokenBody
		if status == 0 {
			status = http.StatusOK
		}
		return jsonResponse(request, status, body), nil
	default:
		s.authHeaders = append(s.authHeaders, request.Header.Get("Authorization"))
		status, body := s.userStatus, s.userBody
		if status == 0 {
			status = http.StatusOK
		}
		return jsonResponse(request, status, body), nil
	}
}

func (s *stubTransport) snapshot() ([]url.Values, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.tokenForms...), append([]string(nil), s.authHeaders...), append([]string(nil), s.requests...)
}

func jsonResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func stubContext(transport *stubTransport) context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: transport})
}

func configuredManager() *Manager {
	cfg := config.Config{PublicURL: "http://localhost:13737"}
	cfg.Google.ClientID, cfg.Google.ClientSecret = "google-client", "google-secret"
	cfg.Microsoft.ClientID, cfg.Microsoft.ClientSecret = "ms-client", "ms-secret"
	return New(cfg)
}

func tokenJSON(refresh, scope string, expiresIn int) string {
	payload := map[string]any{
		"access_token": "access-token-value",
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
		"scope":        scope,
	}
	if refresh != "" {
		payload["refresh_token"] = refresh
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

// --- Start ----------------------------------------------------------------

func TestStartBuildsAPKCEAuthorizationURL(t *testing.T) {
	manager := configuredManager()
	raw, err := manager.Start("gmail", "Personal")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	// PKCE is what stops a stolen authorization code from being redeemed by anyone
	// but this process, so the challenge and its method are both required.
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("query = %v", query)
	}
	// Offline access is what makes a refresh token arrive at all, and Gmail only
	// re-issues one when consent is prompted again.
	if query.Get("access_type") != "offline" || query.Get("prompt") != "consent" {
		t.Fatalf("query = %v", query)
	}
	if query.Get("state") == "" {
		t.Fatal("no state parameter")
	}
	if !strings.Contains(query.Get("scope"), "https://mail.google.com/") {
		t.Fatalf("scope = %q", query.Get("scope"))
	}
	if query.Get("redirect_uri") != "http://localhost:13737/api/v1/oauth/gmail/callback" {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
}

// Outlook needs the offline_access scope explicitly and must not carry Gmail's
// consent prompt.
func TestStartForOutlook(t *testing.T) {
	manager := configuredManager()
	raw, err := manager.Start("outlook", "Work")
	if err != nil {
		t.Fatal(err)
	}
	query := mustQuery(t, raw)
	if !strings.Contains(query.Get("scope"), "offline_access") {
		t.Fatalf("scope = %q", query.Get("scope"))
	}
	if !strings.Contains(query.Get("scope"), "IMAP.AccessAsUser.All") || !strings.Contains(query.Get("scope"), "SMTP.Send") {
		t.Fatalf("scope = %q, both IMAP and SMTP access are required", query.Get("scope"))
	}
	if query.Get("prompt") != "" {
		t.Fatalf("prompt = %q, only Gmail needs it", query.Get("prompt"))
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query()
}

// Every Start must produce a distinct state and verifier, or one flow could be
// completed with another's code.
func TestStartIssuesDistinctState(t *testing.T) {
	manager := configuredManager()
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		raw, err := manager.Start("gmail", "")
		if err != nil {
			t.Fatal(err)
		}
		query := mustQuery(t, raw)
		state, challenge := query.Get("state"), query.Get("code_challenge")
		if seen[state] {
			t.Fatalf("state %q was issued twice", state)
		}
		if seen[challenge] {
			t.Fatalf("code challenge %q was issued twice", challenge)
		}
		seen[state], seen[challenge] = true, true
	}
}

func TestStartRequiresConfiguration(t *testing.T) {
	manager := New(config.Config{PublicURL: "http://localhost:13737"})
	for _, provider := range []string{"gmail", "outlook"} {
		if _, err := manager.Start(provider, ""); err == nil {
			t.Fatalf("%s started without client credentials", provider)
		}
	}
	// A provider that has no OAuth support at all is a different failure and must
	// not be silently treated as unconfigured.
	if _, err := configuredManager().Start("qq", ""); err == nil {
		t.Fatal("qq started an OAuth flow")
	}
}

// --- Exchange -------------------------------------------------------------

func TestExchangeStoresTheRefreshTokenAndEmail(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "Personal")
	transport := &stubTransport{
		tokenBody: tokenJSON("refresh-token-value", "openid email https://mail.google.com/", 3600),
		userBody:  `{"email":"user@gmail.com"}`,
	}
	email, displayName, refreshToken, err := manager.Exchange(stubContext(transport), "gmail", state, "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@gmail.com" || displayName != "Personal" || refreshToken != "refresh-token-value" {
		t.Fatalf("email=%q display=%q refresh=%q", email, displayName, refreshToken)
	}
	forms, authHeaders, _ := transport.snapshot()
	if len(forms) != 1 {
		t.Fatalf("token requests = %d", len(forms))
	}
	// The code verifier has to be sent, otherwise PKCE is decoration.
	if forms[0].Get("code_verifier") == "" {
		t.Fatalf("token request = %v, no code_verifier", forms[0])
	}
	if forms[0].Get("code") != "auth-code" || forms[0].Get("grant_type") != "authorization_code" {
		t.Fatalf("token request = %v", forms[0])
	}
	// The identity call is authenticated with the access token, not the refresh one.
	if len(authHeaders) != 1 || authHeaders[0] != "Bearer access-token-value" {
		t.Fatalf("userinfo Authorization = %v", authHeaders)
	}
}

// stateOf runs Start and pulls the state out of the authorization URL, which is
// the only way a caller ever learns it.
func stateOf(t *testing.T, manager *Manager, provider, displayName string) string {
	t.Helper()
	raw, err := manager.Start(provider, displayName)
	if err != nil {
		t.Fatal(err)
	}
	state := mustQuery(t, raw).Get("state")
	if state == "" {
		t.Fatal("Start produced no state")
	}
	return state
}

// State is single use. Without this a replayed callback would create a second
// account from one consent.
func TestExchangeConsumesTheState(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "")
	transport := &stubTransport{
		tokenBody: tokenJSON("refresh", "https://mail.google.com/", 3600),
		userBody:  `{"email":"user@gmail.com"}`,
	}
	if _, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code"); err == nil {
		t.Fatal("the same state was accepted twice")
	}
}

// A state issued for one provider must not complete another's callback.
func TestExchangeRejectsAStateFromAnotherProvider(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "")
	transport := &stubTransport{tokenBody: tokenJSON("refresh", "", 3600), userBody: `{"email":"a@b.com"}`}
	if _, _, _, err := manager.Exchange(stubContext(transport), "outlook", state, "code"); err == nil {
		t.Fatal("a gmail state completed an outlook callback")
	}
	// And the state is gone either way, so it cannot be retried against the right
	// provider afterwards.
	if _, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code"); err == nil {
		t.Fatal("the state survived a mismatched attempt")
	}
}

func TestExchangeRejectsAnUnknownState(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{}
	if _, _, _, err := manager.Exchange(stubContext(transport), "gmail", "never-issued", "code"); err == nil {
		t.Fatal("an unknown state was accepted")
	}
	// Nothing was sent to the provider: an unknown state is rejected locally.
	if _, _, requests := transport.snapshot(); len(requests) != 0 {
		t.Fatalf("requests = %v", requests)
	}
}

// An expired state is refused. The window is bounded so an abandoned flow cannot be
// completed hours later from a browser history entry.
func TestExchangeRejectsAnExpiredState(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "")
	manager.mu.Lock()
	entry := manager.states[state]
	entry.ExpiresAt = time.Now().Add(-time.Second)
	manager.states[state] = entry
	manager.mu.Unlock()
	if _, _, _, err := manager.Exchange(stubContext(&stubTransport{}), "gmail", state, "code"); err == nil {
		t.Fatal("an expired state was accepted")
	}
}

// A provider that returns no refresh token leaves an account that can never be
// refreshed, so it has to be refused at the point where the user can still fix it
// by revoking consent.
func TestExchangeRequiresARefreshToken(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "")
	transport := &stubTransport{tokenBody: tokenJSON("", "https://mail.google.com/", 3600), userBody: `{"email":"a@b.com"}`}
	_, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code")
	if err == nil {
		t.Fatal("a token response without a refresh token was accepted")
	}
	if !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("error = %q, it does not say what to do", err)
	}
}

// A token granted without mailbox scope is refused here rather than surfacing much
// later as an opaque IMAP credential failure.
func TestExchangeRequiresTheMailScope(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "")
	transport := &stubTransport{tokenBody: tokenJSON("refresh", "openid email", 3600), userBody: `{"email":"a@b.com"}`}
	_, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code")
	if err == nil {
		t.Fatal("a token without the mail scope was accepted")
	}
	if !strings.Contains(err.Error(), "https://mail.google.com/") {
		t.Fatalf("error = %q, it does not name the missing scope", err)
	}
}

func TestExchangeFailsWhenTheTokenEndpointRejectsTheCode(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "")
	transport := &stubTransport{tokenStatus: http.StatusBadRequest, tokenBody: `{"error":"invalid_grant"}`}
	_, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code")
	if err == nil {
		t.Fatal("a rejected code was accepted")
	}
	if !strings.Contains(err.Error(), "exchange OAuth code") {
		t.Fatalf("error = %q", err)
	}
}

func TestExchangeFailsWhenTheIdentityCallFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http error", http.StatusForbidden, `{}`},
		{"no email", http.StatusOK, `{"sub":"1234"}`},
		{"malformed json", http.StatusOK, `{not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := configuredManager()
			state := stateOf(t, manager, "gmail", "")
			transport := &stubTransport{
				tokenBody:  tokenJSON("refresh", "https://mail.google.com/", 3600),
				userStatus: tc.status, userBody: tc.body,
			}
			if _, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code"); err == nil {
				t.Fatal("the exchange succeeded without an email address")
			}
		})
	}
}

// Outlook uses the Graph userinfo endpoint, not Google's.
func TestExchangeUsesTheProviderIdentityEndpoint(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "outlook", "Work")
	transport := &stubTransport{
		tokenBody: tokenJSON("refresh", "IMAP.AccessAsUser.All", 3600),
		userBody:  `{"email":"user@outlook.com"}`,
	}
	email, _, _, err := manager.Exchange(stubContext(transport), "outlook", state, "code")
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@outlook.com" {
		t.Fatalf("email = %q", email)
	}
	_, _, requests := transport.snapshot()
	found := false
	for _, request := range requests {
		if strings.Contains(request, "graph.microsoft.com") {
			found = true
		}
		if strings.Contains(request, "openidconnect.googleapis.com") {
			t.Fatalf("an outlook exchange called Google: %v", requests)
		}
	}
	if !found {
		t.Fatalf("requests = %v, none reached Graph", requests)
	}
}

// Expired states are swept on the next Start, so an abandoned flow does not leave
// an entry in the map forever.
func TestStartSweepsExpiredStates(t *testing.T) {
	manager := configuredManager()
	stale := stateOf(t, manager, "gmail", "")
	manager.mu.Lock()
	entry := manager.states[stale]
	entry.ExpiresAt = time.Now().Add(-time.Minute)
	manager.states[stale] = entry
	manager.mu.Unlock()

	stateOf(t, manager, "gmail", "")
	manager.mu.Lock()
	_, present := manager.states[stale]
	count := len(manager.states)
	manager.mu.Unlock()
	if present {
		t.Fatal("the expired state was kept")
	}
	if count != 1 {
		t.Fatalf("states = %d, want only the live one", count)
	}
}

// --- AccessToken ----------------------------------------------------------

func oauthAccount(id int64, provider string) domain.Account {
	return domain.Account{ID: id, Provider: provider, AuthType: "oauth2", Email: "user@example.com", Username: "user@example.com"}
}

func TestAccessTokenRefreshesAndCaches(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{tokenBody: tokenJSON("", "https://mail.google.com/", 3600)}
	ctx := stubContext(transport)

	first, err := manager.AccessToken(ctx, oauthAccount(1, "gmail"), "refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	if first != "access-token-value" {
		t.Fatalf("token = %q", first)
	}
	// A second call inside the validity window is served from the cache: a refresh
	// per IMAP reconnect would run into the provider's rate limit.
	second, err := manager.AccessToken(ctx, oauthAccount(1, "gmail"), "refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second token = %q, want the cached %q", second, first)
	}
	if forms, _, _ := transport.snapshot(); len(forms) != 1 {
		t.Fatalf("token endpoint was called %d times, want 1", len(forms))
	}
}

// The cache is per account, so two accounts never share a token.
func TestAccessTokenCacheIsPerAccount(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{tokenBody: tokenJSON("", "https://mail.google.com/", 3600)}
	ctx := stubContext(transport)
	if _, err := manager.AccessToken(ctx, oauthAccount(1, "gmail"), "refresh-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AccessToken(ctx, oauthAccount(2, "gmail"), "refresh-two"); err != nil {
		t.Fatal(err)
	}
	forms, _, _ := transport.snapshot()
	if len(forms) != 2 {
		t.Fatalf("token endpoint was called %d times, want one per account", len(forms))
	}
	if forms[0].Get("refresh_token") == forms[1].Get("refresh_token") {
		t.Fatalf("both accounts refreshed with %q", forms[0].Get("refresh_token"))
	}
}

// A token about to expire is refreshed rather than handed out: the minute of
// headroom is what stops a long IMAP command from starting on a token that dies
// mid-conversation.
func TestAccessTokenRefreshesATokenNearExpiry(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{tokenBody: tokenJSON("", "https://mail.google.com/", 3600)}
	ctx := stubContext(transport)
	account := oauthAccount(1, "gmail")
	if _, err := manager.AccessToken(ctx, account, "refresh-token"); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	entry := manager.tokens[account.ID]
	manager.mu.Unlock()
	entry.mu.Lock()
	entry.Token.Expiry = time.Now().Add(30 * time.Second)
	entry.mu.Unlock()

	if _, err := manager.AccessToken(ctx, account, "refresh-token"); err != nil {
		t.Fatal(err)
	}
	if forms, _, _ := transport.snapshot(); len(forms) != 2 {
		t.Fatalf("token endpoint was called %d times, want a second refresh", len(forms))
	}
}

// Concurrent callers must not stampede the token endpoint: the supervisor asks for
// a token from several goroutines and a provider counts every refresh.
func TestAccessTokenIsRefreshedOnceUnderConcurrency(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{tokenBody: tokenJSON("", "https://mail.google.com/", 3600)}
	ctx := stubContext(transport)
	account := oauthAccount(1, "gmail")

	const callers = 20
	var wg sync.WaitGroup
	errs := make([]error, callers)
	tokens := make([]string, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			tokens[index], errs[index] = manager.AccessToken(ctx, account, "refresh-token")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if tokens[i] != "access-token-value" {
			t.Fatalf("caller %d got %q", i, tokens[i])
		}
	}
	if forms, _, _ := transport.snapshot(); len(forms) != 1 {
		t.Fatalf("token endpoint was called %d times for %d concurrent callers", len(forms), callers)
	}
}

func TestAccessTokenFailsForAnUnconfiguredProvider(t *testing.T) {
	manager := New(config.Config{})
	if _, err := manager.AccessToken(context.Background(), oauthAccount(1, "gmail"), "refresh"); err == nil {
		t.Fatal("an unconfigured provider produced a token")
	}
	if _, err := configuredManager().AccessToken(context.Background(), oauthAccount(1, "qq"), "refresh"); err == nil {
		t.Fatal("a non-OAuth provider produced a token")
	}
}

func TestAccessTokenFailsWhenTheRefreshIsRejected(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{tokenStatus: http.StatusBadRequest, tokenBody: `{"error":"invalid_grant"}`}
	_, err := manager.AccessToken(stubContext(transport), oauthAccount(1, "gmail"), "revoked")
	if err == nil {
		t.Fatal("a rejected refresh produced a token")
	}
	if !strings.Contains(err.Error(), "refresh OAuth token") {
		t.Fatalf("error = %q", err)
	}
	// A failed refresh must not have cached anything.
	manager.mu.Lock()
	entry := manager.tokens[1]
	manager.mu.Unlock()
	entry.mu.Lock()
	cached := entry.Token
	entry.mu.Unlock()
	if cached != nil {
		t.Fatalf("a failed refresh cached %+v", cached)
	}
}

// TestAccessTokenSeparatesARefusalFromAFailedRefresh is the regression for a real
// misreport: a dropped TCP connection to oauth2.googleapis.com was surfaced to the
// user as invalid credentials and parked the account for 15 minutes, because the
// only signal downstream had was the "refresh OAuth token" prefix this path puts on
// every failure alike. Whether the credential was refused is knowable only here,
// where the HTTP response is, so it is stated with a sentinel.
func TestAccessTokenSeparatesARefusalFromAFailedRefresh(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		transport *stubTransport
		rejected  bool
	}{
		// The provider's final answers: the stored refresh token or the client
		// registration will not work again, and only re-authorizing fixes either.
		{"invalid_grant is a refusal", &stubTransport{tokenStatus: http.StatusBadRequest, tokenBody: `{"error":"invalid_grant"}`}, true},
		{"invalid_client is a refusal", &stubTransport{tokenStatus: http.StatusUnauthorized, tokenBody: `{"error":"invalid_client"}`}, true},
		{"unauthorized_client is a refusal", &stubTransport{tokenStatus: http.StatusBadRequest, tokenBody: `{"error":"unauthorized_client"}`}, true},
		// Google answers a revoked token with 400 and a non-JSON body often enough
		// that the status alone has to count.
		{"a 400 with no error code is a refusal", &stubTransport{tokenStatus: http.StatusBadRequest, tokenBody: `not json`}, true},
		// Everything that clears on its own must stay on the caller's ladder.
		{"a transport failure is not a refusal", &stubTransport{transportErr: errors.New("EOF")}, false},
		{"a 500 is not a refusal", &stubTransport{tokenStatus: http.StatusInternalServerError, tokenBody: `{"error":"internal_failure"}`}, false},
		{"a 503 is not a refusal", &stubTransport{tokenStatus: http.StatusServiceUnavailable, tokenBody: ``}, false},
		{"a throttle is not a refusal", &stubTransport{tokenStatus: http.StatusTooManyRequests, tokenBody: `{"error":"rate_limit_exceeded"}`}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := configuredManager().AccessToken(stubContext(testCase.transport), oauthAccount(1, "gmail"), "refresh")
			if err == nil {
				t.Fatal("a failed refresh produced a token")
			}
			if got := errors.Is(err, auth.ErrCredentialRejected); got != testCase.rejected {
				t.Errorf("errors.Is(%q, ErrCredentialRejected) = %v, want %v", err, got, testCase.rejected)
			}
			// The original cause stays reachable either way, so operators still see
			// what the provider actually said.
			if !strings.Contains(err.Error(), "refresh OAuth token") {
				t.Errorf("error lost its context: %q", err)
			}
		})
	}
}

// A refreshed token that lost the mail scope — because consent was narrowed after
// the account was added — must be refused rather than used until IMAP fails. It is
// a credential rejection: retrying cannot widen a consent screen.
func TestAccessTokenVerifiesTheScopeOnRefresh(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{tokenBody: tokenJSON("", "openid email", 3600)}
	_, err := manager.AccessToken(stubContext(transport), oauthAccount(1, "gmail"), "refresh")
	if err == nil {
		t.Fatal("a refreshed token without the mail scope was accepted")
	}
	if !strings.Contains(err.Error(), "https://mail.google.com/") {
		t.Fatalf("error = %q", err)
	}
	if !errors.Is(err, auth.ErrCredentialRejected) {
		t.Errorf("a missing mail scope was not reported as a credential rejection: %q", err)
	}
}

// A provider that returns no scope at all is not treated as a scope failure: many
// refresh responses omit it, and rejecting them would break every reconnect.
func TestAccessTokenAcceptsAResponseWithNoScope(t *testing.T) {
	manager := configuredManager()
	transport := &stubTransport{tokenBody: `{"access_token":"a","token_type":"Bearer","expires_in":3600}`}
	if _, err := manager.AccessToken(stubContext(transport), oauthAccount(1, "gmail"), "refresh"); err != nil {
		t.Fatalf("a scopeless refresh response was rejected: %v", err)
	}
}

func TestProviderConfigRedirectURIMatchesThePublicURL(t *testing.T) {
	cfg := config.Config{PublicURL: "https://mail.example.com"}
	cfg.Google.ClientID, cfg.Google.ClientSecret = "id", "secret"
	cfg.Microsoft.ClientID, cfg.Microsoft.ClientSecret = "id", "secret"
	manager := New(cfg)
	for provider, want := range map[string]string{
		"gmail":   "https://mail.example.com/api/v1/oauth/gmail/callback",
		"outlook": "https://mail.example.com/api/v1/oauth/outlook/callback",
	} {
		config, err := manager.providerConfig(provider)
		if err != nil {
			t.Fatal(err)
		}
		if config.RedirectURL != want {
			t.Fatalf("%s redirect = %q, want %q", provider, config.RedirectURL, want)
		}
	}
}

func TestRandomTokenIsUniqueAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		value, err := randomToken(32)
		if err != nil {
			t.Fatal(err)
		}
		if seen[value] {
			t.Fatalf("randomToken repeated %q", value)
		}
		seen[value] = true
		if value != url.QueryEscape(value) {
			t.Fatalf("randomToken produced %q, which is not URL safe", value)
		}
		if len(value) < 40 {
			t.Fatalf("randomToken produced %d characters from 32 bytes", len(value))
		}
	}
}

func TestFetchEmailReportsATransportFailure(t *testing.T) {
	manager := configuredManager()
	state := stateOf(t, manager, "gmail", "")
	transport := &stubTransport{transportErr: fmt.Errorf("dial tcp: network is unreachable")}
	if _, _, _, err := manager.Exchange(stubContext(transport), "gmail", state, "code"); err == nil {
		t.Fatal("an unreachable provider produced an account")
	}
}
