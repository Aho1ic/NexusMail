package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

type oauthClientRow struct {
	Provider           string `json:"provider"`
	Configured         bool   `json:"configured"`
	Source             string `json:"source"`
	ClientID           string `json:"client_id"`
	RedirectURI        string `json:"redirect_uri"`
	EnvClientIDKey     string `json:"env_client_id_key"`
	EnvClientSecretKey string `json:"env_client_secret_key"`
	UpdatedAt          *int64 `json:"updated_at"`
}

func listClients(t *testing.T, h *harness) map[string]oauthClientRow {
	t.Helper()
	response := h.do(http.MethodGet, "/api/v1/oauth/clients", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	payload := decodeBody[struct {
		Items []oauthClientRow `json:"items"`
	}](t, response)
	byProvider := make(map[string]oauthClientRow, len(payload.Items))
	for _, item := range payload.Items {
		byProvider[item.Provider] = item
	}
	return byProvider
}

// The page decides between the authorize button and the credential form from this
// payload, so an unconfigured provider must still be listed: a provider missing
// from the response is indistinguishable from one this build does not support. The
// environment variable names travel with it because the deployer's alternative is
// writing them into the compose file's .env, which is not guessable from the page.
func TestListOAuthClientsReportsUnconfiguredProviders(t *testing.T) {
	h := newHarness(t)

	clients := listClients(t, h)
	if len(clients) != 2 {
		t.Fatalf("providers = %v, want gmail and outlook", clients)
	}
	outlook, ok := clients["outlook"]
	if !ok {
		t.Fatal("outlook is missing from the response")
	}
	if outlook.Configured || outlook.Source != "none" || outlook.ClientID != "" {
		t.Fatalf("outlook = %+v, want an unconfigured row", outlook)
	}
	if outlook.EnvClientIDKey != "NEXUSMAIL_MICROSOFT_CLIENT_ID" || outlook.EnvClientSecretKey != "NEXUSMAIL_MICROSOFT_CLIENT_SECRET" {
		t.Fatalf("outlook env keys = %q/%q", outlook.EnvClientIDKey, outlook.EnvClientSecretKey)
	}
	if outlook.RedirectURI != "http://localhost:13737/api/v1/oauth/outlook/callback" {
		t.Fatalf("redirect_uri = %q", outlook.RedirectURI)
	}
	if outlook.UpdatedAt != nil {
		t.Fatal("an unconfigured provider reported an update time")
	}
}

// This is the failure the feature exists for: an Outlook mailbox could not be
// connected at all without editing .env and restarting. Saving the client on the
// page has to make the same authorization work immediately, in the running process.
func TestSavingAnOAuthClientMakesAuthorizationWork(t *testing.T) {
	h := newHarness(t)

	h.expectError(h.do(http.MethodPost, "/api/v1/oauth/outlook/authorize", map[string]any{"display_name": "工作邮箱"}), 400, "oauth_not_configured")

	response := h.do(http.MethodPut, "/api/v1/oauth/clients/outlook", map[string]any{"client_id": "page-client", "client_secret": "page-secret"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	saved := decodeBody[oauthClientRow](t, response)
	if !saved.Configured || saved.Source != "database" || saved.ClientID != "page-client" {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.UpdatedAt == nil || *saved.UpdatedAt == 0 {
		t.Fatal("a stored client reported no update time")
	}
	// The secret is write-only: it is returned by no endpoint, so a compromised
	// session cannot read back what the deployment configured.
	if strings.Contains(response.Body.String(), "page-secret") {
		t.Fatal("the client secret was echoed back to the caller")
	}

	authorize := h.do(http.MethodPost, "/api/v1/oauth/outlook/authorize", map[string]any{"display_name": "工作邮箱"})
	if authorize.Code != http.StatusOK {
		t.Fatalf("authorize status = %d: %s", authorize.Code, authorize.Body.String())
	}
	payload := decodeBody[struct {
		AuthorizationURL string `json:"authorization_url"`
		State            string `json:"state"`
		RedirectURI      string `json:"redirect_uri"`
	}](t, authorize)
	if !strings.Contains(payload.AuthorizationURL, "login.microsoftonline.com") {
		t.Fatalf("authorization_url = %q", payload.AuthorizationURL)
	}
	if !strings.Contains(payload.AuthorizationURL, "client_id=page-client") {
		t.Fatalf("authorization_url does not carry the saved client: %q", payload.AuthorizationURL)
	}
	// The state is returned because the manual completion has to present it back.
	if payload.State == "" || !strings.Contains(payload.AuthorizationURL, "state="+payload.State) {
		t.Fatalf("state = %q, url = %q", payload.State, payload.AuthorizationURL)
	}
	if payload.RedirectURI != "http://localhost:13737/api/v1/oauth/outlook/callback" {
		t.Fatalf("redirect_uri = %q", payload.RedirectURI)
	}
}

// Clearing the stored pair has to fall back to the environment rather than leave
// the provider dead: a deployment that configured both is asking for the container
// value back.
func TestClearingAnOAuthClientFallsBackToTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.Google.ClientID, h.server.cfg.Google.ClientSecret = "env-client", "env-secret"
	// The services hold the config by value, so both have to be rebuilt for the
	// change to be visible — this mirrors what a restart does.
	h.rebuildOAuth()

	if got := listClients(t, h)["gmail"]; got.Source != "environment" || got.ClientID != "env-client" {
		t.Fatalf("gmail = %+v, want the environment client", got)
	}

	if response := h.do(http.MethodPut, "/api/v1/oauth/clients/gmail", map[string]any{"client_id": "page-client", "client_secret": "page-secret"}); response.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", response.Code, response.Body.String())
	}
	if got := listClients(t, h)["gmail"]; got.Source != "database" || got.ClientID != "page-client" {
		t.Fatalf("gmail = %+v, want the stored client to win", got)
	}

	if response := h.do(http.MethodDelete, "/api/v1/oauth/clients/gmail", nil); response.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", response.Code, response.Body.String())
	}
	if got := listClients(t, h)["gmail"]; got.Source != "environment" || got.ClientID != "env-client" {
		t.Fatalf("gmail = %+v, want the environment client back", got)
	}
}

func TestOAuthClientRejectsBadInput(t *testing.T) {
	h := newHarness(t)

	// A password provider has no OAuth client, and writing a row for one would be
	// dead configuration no code path reads.
	h.expectError(h.do(http.MethodPut, "/api/v1/oauth/clients/qq", map[string]any{"client_id": "a", "client_secret": "b"}), 400, "invalid_request")
	h.expectError(h.do(http.MethodPut, "/api/v1/oauth/clients/nope", map[string]any{"client_id": "a", "client_secret": "b"}), 400, "invalid_request")
	// A half-filled pair cannot authorize, so it is refused rather than stored as a
	// row that presents the provider as configured.
	h.expectError(h.do(http.MethodPut, "/api/v1/oauth/clients/gmail", map[string]any{"client_id": "a", "client_secret": "  "}), 400, "invalid_request")
	if got := listClients(t, h)["gmail"]; got.Configured {
		t.Fatalf("gmail = %+v, want it left unconfigured", got)
	}
	h.expectError(h.do(http.MethodDelete, "/api/v1/oauth/clients/qq", nil), 400, "invalid_request")
	h.expectError(h.do(http.MethodPost, "/api/v1/oauth/qq/authorize", nil), 400, "invalid_request")
}

// The manual path exists for a deployment the provider cannot redirect back to.
// It must complete the same handshake the callback does — same PKCE verifier, same
// scope check — and leave the account syncing, or the mailbox sits inert until the
// process restarts.
func TestManualCodeCompletesTheHandshake(t *testing.T) {
	h := newHarness(t)
	state := configureOAuth(t, h)
	transport := &oauthStubTransport{
		tokenBody: `{"access_token":"access-token-value","token_type":"Bearer","refresh_token":"refresh-token-value","scope":"openid email https://mail.google.com/","expires_in":3600}`,
		userBody:  `{"email":"user@gmail.com"}`,
	}

	before, _, _, _ := h.provider.counts()
	// The whole redirect is pasted, which is what the address bar actually holds.
	response := h.doOAuthCode(transport, "gmail", map[string]any{
		"state": state,
		"code":  "http://192.168.1.9:13737/api/v1/oauth/gmail/callback?code=auth-code&state=" + state,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	account := decodeBody[struct {
		Email    string `json:"email"`
		AuthType string `json:"auth_type"`
	}](t, response)
	if account.Email != "user@gmail.com" || account.AuthType != "oauth2" {
		t.Fatalf("account = %+v", account)
	}
	if started, _, _, _ := h.provider.counts(); started != before+1 {
		t.Fatal("the manually connected account would not sync until a restart")
	}
	if strings.Contains(response.Body.String(), "refresh-token-value") {
		t.Fatal("the refresh token reached the client")
	}
}

// State is single use and expires. Replaying it — the user pressing the button
// twice, or finishing an authorization the callback already consumed — has to be a
// 400 the page can explain, not a 500.
func TestManualCodeRejectsAConsumedState(t *testing.T) {
	h := newHarness(t)
	state := configureOAuth(t, h)
	transport := &oauthStubTransport{
		tokenBody: `{"access_token":"access-token-value","token_type":"Bearer","refresh_token":"refresh-token-value","scope":"openid email https://mail.google.com/","expires_in":3600}`,
		userBody:  `{"email":"user@gmail.com"}`,
	}
	if response := h.doOAuthCode(transport, "gmail", map[string]any{"state": state, "code": "auth-code"}); response.Code != http.StatusCreated {
		t.Fatalf("first attempt status = %d: %s", response.Code, response.Body.String())
	}

	replay := h.doOAuthCode(transport, "gmail", map[string]any{"state": state, "code": "auth-code"})
	envelope := h.expectError(replay, 400, "invalid_request")
	if !strings.Contains(envelope.Error.Message, "expired") {
		t.Fatalf("message = %q, want it to name the state", envelope.Error.Message)
	}
	if emails := listAccountEmails(t, h); len(emails) != 1 {
		t.Fatalf("accounts = %v, want one from one consent", emails)
	}
}

// A refused consent arrives as a pasted redirect carrying error=access_denied.
// Reporting that as an invalid code sends the user looking for a typo in something
// they never mistyped.
func TestManualCodeReportsARefusedConsent(t *testing.T) {
	h := newHarness(t)
	state := configureOAuth(t, h)

	envelope := h.expectError(h.doOAuthCode(nil, "gmail", map[string]any{
		"state": state,
		"code":  "http://localhost:13737/api/v1/oauth/gmail/callback?error=access_denied&state=" + state,
	}), 400, "invalid_request")
	if !strings.Contains(envelope.Error.Message, "access_denied") {
		t.Fatalf("message = %q, want the provider's refusal", envelope.Error.Message)
	}
}

func TestManualCodeRequiresStateAndCode(t *testing.T) {
	h := newHarness(t)
	configureOAuth(t, h)

	h.expectError(h.doOAuthCode(nil, "gmail", map[string]any{"state": "abc", "code": "  "}), 400, "invalid_request")
	// A bare code with no state cannot be redeemed: the PKCE verifier is keyed on
	// the state, and guessing one would defeat the CSRF binding it exists for.
	h.expectError(h.doOAuthCode(nil, "gmail", map[string]any{"code": "auth-code"}), 400, "invalid_request")
}

// doOAuthCode posts to the manual completion endpoint with the HTTP client the
// exchange will use, injected the way oauth2 expects.
func (h *harness) doOAuthCode(transport http.RoundTripper, provider string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	request := jsonRequest(h.t, http.MethodPost, "/api/v1/oauth/"+provider+"/code", body)
	if transport != nil {
		request = request.WithContext(context.WithValue(
			request.Context(), oauth2.HTTPClient, &http.Client{Transport: transport},
		))
	}
	return h.doRaw(request)
}
