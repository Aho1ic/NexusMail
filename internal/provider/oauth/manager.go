package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	"nexusmail/internal/provider/auth"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type stateEntry struct {
	Provider    string
	Verifier    string
	DisplayName string
	ExpiresAt   time.Time
}

type cachedToken struct {
	Token *oauth2.Token
	mu    sync.Mutex
}

type Manager struct {
	cfg    config.Config
	mu     sync.Mutex
	states map[string]stateEntry
	tokens map[int64]*cachedToken
}

func New(cfg config.Config) *Manager {
	return &Manager{cfg: cfg, states: make(map[string]stateEntry), tokens: make(map[int64]*cachedToken)}
}

func (m *Manager) Start(provider, displayName string) (string, error) {
	oauthConfig, err := m.providerConfig(provider)
	if err != nil {
		return "", err
	}
	state, err := randomToken(32)
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()
	m.mu.Lock()
	for key, entry := range m.states {
		if time.Now().After(entry.ExpiresAt) {
			delete(m.states, key)
		}
	}
	m.states[state] = stateEntry{Provider: provider, Verifier: verifier, DisplayName: displayName, ExpiresAt: time.Now().Add(10 * time.Minute)}
	m.mu.Unlock()
	options := []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier)}
	if provider == "gmail" {
		options = append(options, oauth2.SetAuthURLParam("prompt", "consent"))
	}
	return oauthConfig.AuthCodeURL(state, options...), nil
}

func (m *Manager) Exchange(ctx context.Context, provider, state, code string) (email, displayName, refreshToken string, err error) {
	m.mu.Lock()
	entry, ok := m.states[state]
	delete(m.states, state)
	m.mu.Unlock()
	if !ok || entry.Provider != provider || time.Now().After(entry.ExpiresAt) {
		return "", "", "", errors.New("invalid or expired OAuth state")
	}
	oauthConfig, err := m.providerConfig(provider)
	if err != nil {
		return "", "", "", err
	}
	token, err := oauthConfig.Exchange(ctx, code, oauth2.VerifierOption(entry.Verifier))
	if err != nil {
		return "", "", "", fmt.Errorf("exchange OAuth code: %w", err)
	}
	if token.RefreshToken == "" {
		return "", "", "", errors.New("provider did not return a refresh token; revoke consent and try again")
	}
	if err := verifyMailScope(provider, token); err != nil {
		return "", "", "", err
	}
	email, err = fetchEmail(ctx, oauthConfig.Client(ctx, token), provider)
	if err != nil {
		return "", "", "", err
	}
	return email, entry.DisplayName, token.RefreshToken, nil
}

func (m *Manager) AccessToken(ctx context.Context, account domain.Account, refreshToken string) (string, error) {
	m.mu.Lock()
	entry := m.tokens[account.ID]
	if entry == nil {
		entry = &cachedToken{}
		m.tokens[account.ID] = entry
	}
	m.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.Token != nil && entry.Token.Valid() && time.Until(entry.Token.Expiry) > time.Minute {
		return entry.Token.AccessToken, nil
	}
	oauthConfig, err := m.providerConfig(account.Provider)
	if err != nil {
		return "", err
	}
	token, err := oauthConfig.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return "", classifyRefreshError(err)
	}
	if err := verifyMailScope(account.Provider, token); err != nil {
		return "", err
	}
	entry.Token = token
	return token.AccessToken, nil
}

// rejectedGrantCodes are the RFC 6749 error codes that mean the stored refresh
// token or the OAuth client will never work again. Everything else the token
// endpoint can answer with — 5xx, a throttle, a truncated response — clears on
// its own and must stay on the caller's retry ladder.
var rejectedGrantCodes = map[string]bool{
	"invalid_grant":          true,
	"invalid_client":         true,
	"unauthorized_client":    true,
	"invalid_scope":          true,
	"unsupported_grant_type": true,
}

// classifyRefreshError separates a refresh the provider refused from one that
// merely failed to complete. Only the former is a credential problem: reporting a
// dropped connection to the token endpoint as "invalid credentials" parks a
// healthy account and tells the user to re-authorize for no reason.
func classifyRefreshError(err error) error {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		// A provider that answers 400/401 with no parseable error code has still
		// refused the grant; Google returns exactly that for a revoked token when
		// the body is not JSON.
		status := 0
		if retrieve.Response != nil {
			status = retrieve.Response.StatusCode
		}
		if rejectedGrantCodes[retrieve.ErrorCode] || ((status == http.StatusBadRequest || status == http.StatusUnauthorized) && retrieve.ErrorCode == "") {
			return fmt.Errorf("%w: refresh OAuth token: %w", auth.ErrCredentialRejected, err)
		}
	}
	return fmt.Errorf("refresh OAuth token: %w", err)
}

func (m *Manager) providerConfig(provider string) (*oauth2.Config, error) {
	switch provider {
	case "gmail":
		if m.cfg.Google.ClientID == "" || m.cfg.Google.ClientSecret == "" {
			return nil, errors.New("missing Google OAuth client credentials")
		}
		return &oauth2.Config{
			ClientID: m.cfg.Google.ClientID, ClientSecret: m.cfg.Google.ClientSecret,
			RedirectURL: m.cfg.PublicURL + "/api/v1/oauth/gmail/callback", Endpoint: google.Endpoint,
			Scopes: []string{"openid", "email", "https://mail.google.com/"},
		}, nil
	case "outlook":
		if m.cfg.Microsoft.ClientID == "" || m.cfg.Microsoft.ClientSecret == "" {
			return nil, errors.New("missing Microsoft OAuth client credentials")
		}
		return &oauth2.Config{
			ClientID: m.cfg.Microsoft.ClientID, ClientSecret: m.cfg.Microsoft.ClientSecret,
			RedirectURL: m.cfg.PublicURL + "/api/v1/oauth/outlook/callback",
			Endpoint:    oauth2.Endpoint{AuthURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize", TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token"},
			Scopes:      []string{"openid", "email", "profile", "offline_access", "https://outlook.office.com/IMAP.AccessAsUser.All", "https://outlook.office.com/SMTP.Send"},
		}, nil
	default:
		return nil, errors.New("provider does not support OAuth")
	}
}

// mailScopes lists the scope each provider must grant for IMAP and SMTP access.
var mailScopes = map[string]string{
	"gmail":   "https://mail.google.com/",
	"outlook": "https://outlook.office.com/IMAP.AccessAsUser.All",
}

// verifyMailScope rejects tokens that carry no mailbox access. Providers silently drop
// restricted scopes that the OAuth client is not cleared for and still return a usable
// token, which would otherwise surface much later as an opaque IMAP credential failure.
func verifyMailScope(provider string, token *oauth2.Token) error {
	required, ok := mailScopes[provider]
	if !ok {
		return nil
	}
	granted, _ := token.Extra("scope").(string)
	if strings.TrimSpace(granted) == "" {
		return nil
	}
	if scopeGranted(granted, required) {
		return nil
	}
	// Marked as a credential rejection so the sync loops park the account and ask
	// the user to authorize again instead of retrying a consent screen that will
	// keep granting the same insufficient scopes.
	return fmt.Errorf("%w: %s granted [%s] but not the required %s scope; enable the provider mail API and add that scope to the OAuth consent screen, then authorize again", auth.ErrCredentialRejected, provider, strings.Join(strings.Fields(granted), " "), required)
}

func scopeGranted(granted, required string) bool {
	required = strings.ToLower(required)
	short := required
	if trimmed := strings.TrimSuffix(required, "/"); trimmed != "" {
		if index := strings.LastIndex(trimmed, "/"); index >= 0 {
			short = trimmed[index+1:]
		}
	}
	for _, value := range strings.Fields(strings.ToLower(granted)) {
		if value == required || value == short {
			return true
		}
	}
	return false
}

func fetchEmail(ctx context.Context, client *http.Client, provider string) (string, error) {
	url := "https://openidconnect.googleapis.com/v1/userinfo"
	if provider == "outlook" {
		url = "https://graph.microsoft.com/oidc/userinfo"
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch OAuth identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch OAuth identity: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload.Email == "" {
		return "", errors.New("OAuth provider did not return an email address")
	}
	return payload.Email, nil
}

func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
