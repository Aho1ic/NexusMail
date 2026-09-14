// Package oauthclient owns the deployment's OAuth application per provider.
//
// The Client ID and Secret used to come only from the environment. A deployment
// that had not set NEXUSMAIL_MICROSOFT_CLIENT_ID could therefore not connect an
// Outlook mailbox at all: the authorize button answered "missing Microsoft OAuth
// client credentials" and the only remedy was editing .env and restarting the
// container. This service adds a second, sealed source that the settings page
// writes, and resolves the two: a stored row wins over the environment, because
// the environment is the boot default and the page is the live setting.
package oauthclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	"nexusmail/internal/platform/cryptobox"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider"
)

// Store is the slice of persistence this service uses.
type Store interface {
	UpsertOAuthClient(context.Context, *domain.OAuthClient) error
	ListOAuthClients(context.Context) ([]domain.OAuthClient, error)
	DeleteOAuthClient(context.Context, string) error
}

// sealed is the JSON envelope the client secret is stored in. A struct rather
// than the bare secret so the envelope can carry a second field later without
// making every existing row unreadable.
type sealed struct {
	ClientSecret string `json:"client_secret"`
}

// Status is what the settings page renders for one provider. The secret is
// deliberately absent: it is write-only from the API's point of view.
type Status struct {
	Provider string `json:"provider"`
	// Configured reports whether an authorization URL can be built at all, from
	// either source.
	Configured bool `json:"configured"`
	// Source is "database", "environment" or "none", which is what tells the user
	// whether the page or the container's env is currently in force.
	Source string `json:"source"`
	// ClientID is not a secret — it travels in every authorization URL — and is
	// returned so the page can show which client is in use.
	ClientID string `json:"client_id"`
	// RedirectURI is the callback the provider must have registered. A mismatch
	// here is the most common setup failure and the value is not guessable from
	// the page alone, so it is served rather than reconstructed client-side.
	RedirectURI        string `json:"redirect_uri"`
	EnvClientIDKey     string `json:"env_client_id_key"`
	EnvClientSecretKey string `json:"env_client_secret_key"`
	// UpdatedAt is present only for a stored client, in UTC milliseconds.
	UpdatedAt *int64 `json:"updated_at,omitempty"`
}

// envKeys names the environment variables that supply each provider's fallback.
// They are reported to the client so the setup hint can name the exact variable
// to write into the deployment's .env, which is otherwise guesswork for anyone
// who did not write the compose file.
var envKeys = map[string][2]string{
	string(domain.ProviderGmail):   {"NEXUSMAIL_GOOGLE_CLIENT_ID", "NEXUSMAIL_GOOGLE_CLIENT_SECRET"},
	string(domain.ProviderOutlook): {"NEXUSMAIL_MICROSOFT_CLIENT_ID", "NEXUSMAIL_MICROSOFT_CLIENT_SECRET"},
}

type credential struct {
	ClientID     string
	ClientSecret string
	UpdatedAt    int64
}

// Service resolves and persists OAuth client credentials.
//
// Stored credentials are cached in memory because the resolver is called from
// oauth.Manager on a path that has no context and must not block: an IMAP
// reconnect asking for an access token cannot afford a database read, and the
// only writer is this service. Load populates the cache at startup; every write
// updates it under the same lock.
type Service struct {
	repo Store
	box  *cryptobox.Box
	cfg  config.Config

	mu     sync.RWMutex
	stored map[string]credential
}

func New(repo Store, box *cryptobox.Box, cfg config.Config) *Service {
	return &Service{repo: repo, box: box, cfg: cfg, stored: make(map[string]credential)}
}

// Load reads every stored client into the cache. A row that cannot be decrypted
// fails the load rather than being skipped: it means the master key no longer
// matches what sealed it, and silently continuing would present the provider as
// unconfigured and send the user to re-enter credentials that are already there.
func (s *Service) Load(ctx context.Context) error {
	items, err := s.repo.ListOAuthClients(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]credential, len(items))
	for _, item := range items {
		secret, err := s.open(item.ClientSecretCiphertext)
		if err != nil {
			return err
		}
		next[item.Provider] = credential{ClientID: item.ClientID, ClientSecret: secret, UpdatedAt: item.UpdatedAt}
	}
	s.mu.Lock()
	s.stored = next
	s.mu.Unlock()
	return nil
}

// ClientCredentials implements oauth.ClientStore. It reports only the stored
// pair; the environment fallback stays in the OAuth manager, which already holds
// the config. A half-configured row cannot exist — Save rejects an empty field —
// so a present row is always usable.
func (s *Service) ClientCredentials(providerName string) (id, secret string, configured bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.stored[providerName]
	if !ok {
		return "", "", false
	}
	return entry.ClientID, entry.ClientSecret, true
}

// Save seals a new pair for the provider and makes it effective immediately. The
// cache is written after the row so a failed write cannot leave the process
// serving credentials the database does not have.
func (s *Service) Save(ctx context.Context, providerName, clientID, clientSecret string) (Status, error) {
	name, err := oauthProvider(providerName)
	if err != nil {
		return Status{}, err
	}
	clientID, clientSecret = strings.TrimSpace(clientID), strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return Status{}, ports.Invalidf("client_id and client_secret are required")
	}
	plaintext, err := json.Marshal(sealed{ClientSecret: clientSecret})
	if err != nil {
		return Status{}, err
	}
	ciphertext, err := s.box.Seal(plaintext)
	if err != nil {
		return Status{}, err
	}
	now := time.Now().UnixMilli()
	record := domain.OAuthClient{Provider: name, ClientID: clientID, ClientSecretCiphertext: ciphertext, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.UpsertOAuthClient(ctx, &record); err != nil {
		return Status{}, err
	}
	s.mu.Lock()
	s.stored[name] = credential{ClientID: clientID, ClientSecret: clientSecret, UpdatedAt: now}
	s.mu.Unlock()
	return s.status(name), nil
}

// Clear drops the stored pair so the provider falls back to the environment.
// Existing accounts keep working when the environment supplies the same client;
// when it does not, they park on the next token refresh, which is the honest
// outcome of removing the credentials they authorize against.
func (s *Service) Clear(ctx context.Context, providerName string) error {
	name, err := oauthProvider(providerName)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteOAuthClient(ctx, name); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.stored, name)
	s.mu.Unlock()
	return nil
}

// List reports every OAuth provider, configured or not, in the preset order. The
// page renders one row per provider either way: a provider missing from the
// response would be indistinguishable from one the build does not support.
func (s *Service) List() []Status {
	names := provider.OAuthProviders()
	items := make([]Status, 0, len(names))
	for _, name := range names {
		items = append(items, s.status(string(name)))
	}
	return items
}

// Status reports one provider, rejecting a provider that does not use OAuth so a
// typo cannot come back as a plausible "not configured".
func (s *Service) Status(providerName string) (Status, error) {
	name, err := oauthProvider(providerName)
	if err != nil {
		return Status{}, err
	}
	return s.status(name), nil
}

func (s *Service) status(name string) Status {
	keys := envKeys[name]
	status := Status{
		Provider:           name,
		Source:             "none",
		RedirectURI:        s.cfg.PublicURL + "/api/v1/oauth/" + name + "/callback",
		EnvClientIDKey:     keys[0],
		EnvClientSecretKey: keys[1],
	}
	s.mu.RLock()
	entry, stored := s.stored[name]
	s.mu.RUnlock()
	if stored {
		updatedAt := entry.UpdatedAt
		status.Configured, status.Source, status.ClientID, status.UpdatedAt = true, "database", entry.ClientID, &updatedAt
		return status
	}
	if env := s.cfg.OAuthEnv(name); env.ClientID != "" && env.ClientSecret != "" {
		status.Configured, status.Source, status.ClientID = true, "environment", env.ClientID
	}
	return status
}

func (s *Service) open(ciphertext []byte) (string, error) {
	plaintext, err := s.box.Open(ciphertext)
	if err != nil {
		return "", err
	}
	var envelope sealed
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		return "", errors.New("invalid stored OAuth client secret")
	}
	if envelope.ClientSecret == "" {
		return "", errors.New("stored OAuth client secret is empty")
	}
	return envelope.ClientSecret, nil
}

// oauthProvider normalises a provider name and refuses one that does not
// authenticate with OAuth. Writing a row for a password provider would be dead
// configuration no code path reads, and the migration's CHECK refuses it anyway —
// this turns that into a 400 with a readable cause.
func oauthProvider(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	preset, err := provider.Get(normalized)
	if err != nil {
		return "", err
	}
	if preset.AuthType != "oauth2" {
		return "", ports.Invalidf("provider does not support OAuth")
	}
	return string(preset.Provider), nil
}
