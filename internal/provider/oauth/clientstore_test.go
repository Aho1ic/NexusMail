package oauth

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"nexusmail/internal/config"
	"nexusmail/internal/ports"
)

type stubClientStore struct {
	id     string
	secret string
	calls  int
}

func (s *stubClientStore) ClientCredentials(string) (string, string, bool) {
	s.calls++
	if s.id == "" || s.secret == "" {
		return "", "", false
	}
	return s.id, s.secret, true
}

// A stored client is what the deployment configured through the settings page, and
// it has to beat the environment: the environment is the value the container booted
// with and changing it costs a restart, so an operator who saves a client on the
// page has said which one they mean.
func TestStoredClientOverridesEnvironment(t *testing.T) {
	manager := configuredManager()
	manager.SetClientStore(&stubClientStore{id: "page-client", secret: "page-secret"})

	raw, _, err := manager.Start("outlook", "Work")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("client_id"); got != "page-client" {
		t.Fatalf("client_id = %q, want the stored client", got)
	}
}

// A store that holds nothing for the provider is the normal case: most deployments
// configure the environment only, and the manager must not treat an empty answer as
// a refusal.
func TestEmptyStoreFallsBackToEnvironment(t *testing.T) {
	manager := configuredManager()
	store := &stubClientStore{}
	manager.SetClientStore(store)

	raw, _, err := manager.Start("gmail", "")
	if err != nil {
		t.Fatal(err)
	}
	if store.calls == 0 {
		t.Fatal("the store was never consulted")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("client_id"); got != "google-client" {
		t.Fatalf("client_id = %q, want the environment client", got)
	}
}

// A provider with no credentials from either source is a setup problem, and the
// transport keys its own error code off this sentinel to offer the credential form
// instead of reporting a failed authorization. It must also still classify as bad
// input so any caller that only knows the ports sentinels answers 400.
func TestMissingClientIsReportedAsNotConfigured(t *testing.T) {
	manager := New(config.Config{PublicURL: "http://localhost:13737"})
	manager.SetClientStore(&stubClientStore{})

	_, _, err := manager.Start("outlook", "")
	if !errors.Is(err, ErrClientNotConfigured) {
		t.Fatalf("err = %v, want ErrClientNotConfigured", err)
	}
	if !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want it to classify as invalid input", err)
	}
	if manager.Configured("outlook") {
		t.Fatal("Configured reported an unconfigured provider as ready")
	}
	if !configuredManager().Configured("outlook") {
		t.Fatal("Configured reported a configured provider as unready")
	}
}

// A store holding only half a pair cannot build a working client. Falling through
// to the environment is what keeps a partially written row from taking a provider
// offline that the environment could still serve.
func TestHalfStoredClientFallsBackToEnvironment(t *testing.T) {
	manager := configuredManager()
	manager.SetClientStore(&stubClientStore{id: "page-client"})

	raw, _, err := manager.Start("gmail", "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("client_id"); got != "google-client" {
		t.Fatalf("client_id = %q, want the environment client", got)
	}
}

// The redirect URI the settings page shows must be the one the request carries:
// a mismatch is refused by the provider with an error the user cannot map back to
// anything, so the two cannot be allowed to drift.
func TestRedirectURIMatchesTheAuthorizationRequest(t *testing.T) {
	manager := configuredManager()

	raw, _, err := manager.Start("outlook", "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://localhost:13737/api/v1/oauth/outlook/callback"
	if got := manager.RedirectURI("outlook"); got != want {
		t.Fatalf("RedirectURI = %q, want %q", got, want)
	}
	if got := parsed.Query().Get("redirect_uri"); got != want {
		t.Fatalf("authorization redirect_uri = %q, want %q", got, want)
	}
}

// Start now hands the state back so a user whose deployment the provider cannot
// redirect to can finish by hand. The value it returns has to be the one bound
// into the URL, or the manual completion presents a state Exchange never stored.
func TestStartReturnsTheStateBoundIntoTheURL(t *testing.T) {
	manager := configuredManager()

	raw, state, err := manager.Start("gmail", "Personal")
	if err != nil {
		t.Fatal(err)
	}
	if state == "" {
		t.Fatal("Start returned no state")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("state"); got != state {
		t.Fatalf("URL state = %q, want the returned %q", got, state)
	}
}

func TestParseManualCode(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantCode  string
		wantState string
		wantErr   string
	}{
		{
			name:     "bare code",
			input:    "  M.C123_BAY.2.U.-Ck9s8x  ",
			wantCode: "M.C123_BAY.2.U.-Ck9s8x",
		},
		{
			// What the address bar actually holds when the callback is unreachable.
			name:      "whole redirect",
			input:     "http://192.168.1.9:13737/api/v1/oauth/outlook/callback?code=M.C123&state=abc123",
			wantCode:  "M.C123",
			wantState: "abc123",
		},
		{
			name:      "bare query string",
			input:     "code=M.C123&state=abc123",
			wantCode:  "M.C123",
			wantState: "abc123",
		},
		{
			// A refused consent is not a malformed paste, and saying "invalid code"
			// to someone who pressed "No" sends them looking for a typo.
			name:    "refusal",
			input:   "http://host/callback?error=access_denied&state=abc",
			wantErr: "access_denied",
		},
		{
			name:    "redirect without a code",
			input:   "http://host/callback?state=abc&code=",
			wantErr: "no authorization code",
		},
		{
			name:    "empty",
			input:   "   ",
			wantErr: "required",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			code, state, err := ParseManualCode(item.input)
			if item.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), item.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, item.wantErr)
				}
				if !errors.Is(err, ports.ErrInvalidInput) {
					t.Fatalf("err = %v, want it to classify as invalid input", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if code != item.wantCode || state != item.wantState {
				t.Fatalf("got (%q, %q), want (%q, %q)", code, state, item.wantCode, item.wantState)
			}
		})
	}
}
