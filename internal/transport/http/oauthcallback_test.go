package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nexusmail/internal/config"
	"nexusmail/internal/provider/oauth"

	"golang.org/x/oauth2"
)

// oauthStubTransport answers the two calls an exchange makes: the token endpoint and
// the provider's identity endpoint. It is deliberately minimal — the exchange itself
// is covered in internal/provider/oauth, and what is under test here is what the
// handler does once the exchange has succeeded.
type oauthStubTransport struct {
	tokenBody  string
	userBody   string
	tokenCalls int
}

func (s *oauthStubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body := s.userBody
	if strings.Contains(request.URL.Path, "/token") || strings.Contains(request.URL.Host, "oauth2.googleapis.com") {
		s.tokenCalls++
		body = s.tokenBody
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

// configureOAuth points the server at a configured manager and returns a state that
// manager will accept. State is minted by Start and is single use, so it is the only
// way a caller ever obtains one.
func configureOAuth(t *testing.T, h *harness) string {
	t.Helper()
	cfg := config.Config{PublicURL: "http://localhost:13737"}
	cfg.Google.ClientID, cfg.Google.ClientSecret = "google-client", "google-secret"
	h.server.cfg.Google = cfg.Google
	h.server.oauth = oauth.New(cfg)

	raw, err := h.server.oauth.Start("gmail", "Personal")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := parsed.URL.Query().Get("state")
	if state == "" {
		t.Fatal("Start produced no state")
	}
	return state
}

// callbackWith drives the callback with an HTTP client the exchange will use, which
// is injected the way oauth2 expects: as a value on the request context.
func callbackWith(h *harness, transport http.RoundTripper, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request = request.WithContext(context.WithValue(
		request.Context(), oauth2.HTTPClient, &http.Client{Transport: transport},
	))
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, request)
	return response
}

// A consent that completes has to leave the account syncing. The provider redirects
// the browser here once and never again, so an account stored without StartAccount
// sits inert until the process restarts: no mail, no IDLE, and nothing in the UI to
// explain why. The redirect target matters too — it is the only signal the SPA gets
// that the popup succeeded.
func TestOAuthCallbackStartsSyncingTheNewAccount(t *testing.T) {
	h := newHarness(t)
	state := configureOAuth(t, h)
	transport := &oauthStubTransport{
		tokenBody: `{"access_token":"access-token-value","token_type":"Bearer","refresh_token":"refresh-token-value","scope":"openid email https://mail.google.com/","expires_in":3600}`,
		userBody:  `{"email":"user@gmail.com"}`,
	}

	before, _, _, _ := h.provider.counts()
	response := callbackWith(h, transport, "/api/v1/oauth/gmail/callback?state="+state+"&code=auth-code")

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "/?oauth=success" {
		t.Errorf("Location = %q, want %q", location, "/?oauth=success")
	}
	if started, _, _, _ := h.provider.counts(); started != before+1 {
		t.Errorf("StartAccount ran %d times, want %d: the new account would not sync until a restart", started-before, 1)
	}

	accounts, err := h.repo.ListAccounts(h.server.appCtx)
	if err != nil {
		t.Fatal(err)
	}
	var stored bool
	for _, account := range accounts {
		if account.Email == "user@gmail.com" {
			stored = true
			if account.AuthType != "oauth2" {
				t.Errorf("auth_type = %q, want oauth2: the supervisor picks its authenticator from this column", account.AuthType)
			}
		}
	}
	if !stored {
		t.Error("the account from a completed consent was not stored")
	}
}

// State is single use, so the same callback replayed — a refreshed tab, a provider
// retry — must not produce a second account from one consent.
func TestOAuthCallbackReplayCreatesNoSecondAccount(t *testing.T) {
	h := newHarness(t)
	state := configureOAuth(t, h)
	transport := &oauthStubTransport{
		tokenBody: `{"access_token":"access-token-value","token_type":"Bearer","refresh_token":"refresh-token-value","scope":"openid email https://mail.google.com/","expires_in":3600}`,
		userBody:  `{"email":"user@gmail.com"}`,
	}
	target := "/api/v1/oauth/gmail/callback?state=" + state + "&code=auth-code"

	if first := callbackWith(h, transport, target); first.Code != http.StatusFound {
		t.Fatalf("first callback status = %d, want 302; body = %s", first.Code, first.Body.String())
	}
	countAfterFirst := len(listAccountEmails(t, h))

	second := callbackWith(h, transport, target)
	if second.Code == http.StatusFound {
		t.Error("a replayed callback succeeded, so one consent can create two accounts")
	}
	if after := len(listAccountEmails(t, h)); after != countAfterFirst {
		t.Errorf("accounts went from %d to %d on a replay", countAfterFirst, after)
	}
}

func listAccountEmails(t *testing.T, h *harness) []string {
	t.Helper()
	accounts, err := h.repo.ListAccounts(h.server.appCtx)
	if err != nil {
		t.Fatal(err)
	}
	emails := make([]string, 0, len(accounts))
	for _, account := range accounts {
		emails = append(emails, account.Email)
	}
	return emails
}
