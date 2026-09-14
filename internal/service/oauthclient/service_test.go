package oauthclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexusmail/internal/config"
	"nexusmail/internal/domain"
	"nexusmail/internal/platform/cryptobox"
	"nexusmail/internal/ports"
)

type fakeStore struct {
	rows      map[string]domain.OAuthClient
	upserts   int
	deletes   []string
	upsertErr error
	listErr   error
	deleteErr error
}

func newStore() *fakeStore { return &fakeStore{rows: make(map[string]domain.OAuthClient)} }

func (f *fakeStore) UpsertOAuthClient(_ context.Context, client *domain.OAuthClient) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts++
	f.rows[client.Provider] = *client
	return nil
}

func (f *fakeStore) ListOAuthClients(context.Context) ([]domain.OAuthClient, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	items := make([]domain.OAuthClient, 0, len(f.rows))
	for _, row := range f.rows {
		items = append(items, row)
	}
	return items, nil
}

func (f *fakeStore) DeleteOAuthClient(_ context.Context, provider string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, provider)
	delete(f.rows, provider)
	return nil
}

func newBox(t *testing.T) *cryptobox.Box {
	t.Helper()
	box, err := cryptobox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func envConfig() config.Config {
	cfg := config.Config{PublicURL: "http://localhost:13737"}
	cfg.Google.ClientID, cfg.Google.ClientSecret = "env-google-id", "env-google-secret"
	return cfg
}

// The secret is the only value here worth protecting, and it is the one thing a
// row must never hold in the clear: the database file travels in the deployment's
// backups, which the master key does not.
func TestSaveSealsTheSecret(t *testing.T) {
	store := newStore()
	box := newBox(t)
	service := New(store, box, config.Config{PublicURL: "http://localhost:13737"})

	status, err := service.Save(context.Background(), "outlook", "  app-id  ", "  app-secret  ")
	if err != nil {
		t.Fatal(err)
	}
	// Trimmed: a pasted client id carries trailing whitespace often enough, and it
	// would otherwise be sent to the provider verbatim and rejected.
	if status.ClientID != "app-id" || status.Source != "database" || !status.Configured {
		t.Fatalf("status = %+v", status)
	}
	row, ok := store.rows["outlook"]
	if !ok {
		t.Fatal("nothing was written")
	}
	if strings.Contains(string(row.ClientSecretCiphertext), "app-secret") {
		t.Fatal("the client secret was stored in the clear")
	}
	if row.CreatedAt == 0 || row.UpdatedAt == 0 {
		t.Fatalf("row timestamps = %d/%d", row.CreatedAt, row.UpdatedAt)
	}

	// The sealed value has to survive a cold start, which is the only path that
	// reads it back.
	fresh := New(store, box, config.Config{PublicURL: "http://localhost:13737"})
	if err := fresh.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, secret, configured := fresh.ClientCredentials("outlook")
	if !configured || id != "app-id" || secret != "app-secret" {
		t.Fatalf("resolved (%q, %q, %v)", id, secret, configured)
	}
}

// A save has to be effective without a restart: the failure this feature exists
// for is a deployment that cannot authorize at all, and telling the user to
// restart the container to apply what they just typed reproduces it.
func TestSaveIsEffectiveImmediately(t *testing.T) {
	service := New(newStore(), newBox(t), config.Config{PublicURL: "http://localhost:13737"})

	if _, _, configured := service.ClientCredentials("gmail"); configured {
		t.Fatal("an empty service reported a client")
	}
	if _, err := service.Save(context.Background(), "gmail", "app-id", "app-secret"); err != nil {
		t.Fatal(err)
	}
	id, secret, configured := service.ClientCredentials("gmail")
	if !configured || id != "app-id" || secret != "app-secret" {
		t.Fatalf("resolved (%q, %q, %v) straight after the save", id, secret, configured)
	}
}

// A write that fails must not leave the process serving credentials the database
// does not have: the next restart would silently authorize against a different
// client than the running process does.
func TestFailedSaveLeavesTheCacheAlone(t *testing.T) {
	store := newStore()
	store.upsertErr = errors.New("disk full")
	service := New(store, newBox(t), config.Config{PublicURL: "http://localhost:13737"})

	if _, err := service.Save(context.Background(), "gmail", "app-id", "app-secret"); err == nil {
		t.Fatal("a failed write was reported as a success")
	}
	if _, _, configured := service.ClientCredentials("gmail"); configured {
		t.Fatal("the cache holds a client the store rejected")
	}
}

// Clearing drops the row so the environment takes over again. Reporting the
// environment as the source afterwards is what tells the user which client is now
// in force — the alternative is a page that says "not configured" about a gateway
// that authorizes fine.
func TestClearFallsBackToTheEnvironment(t *testing.T) {
	store := newStore()
	service := New(store, newBox(t), envConfig())

	if _, err := service.Save(context.Background(), "gmail", "app-id", "app-secret"); err != nil {
		t.Fatal(err)
	}
	if err := service.Clear(context.Background(), "gmail"); err != nil {
		t.Fatal(err)
	}
	if _, _, configured := service.ClientCredentials("gmail"); configured {
		t.Fatal("the cleared client is still resolved")
	}
	status, err := service.Status("gmail")
	if err != nil {
		t.Fatal(err)
	}
	if status.Source != "environment" || status.ClientID != "env-google-id" || !status.Configured {
		t.Fatalf("status = %+v, want the environment client", status)
	}
	if status.UpdatedAt != nil {
		t.Fatal("an environment-backed provider reported an update time")
	}
}

// Every OAuth provider is listed whether or not it is configured: the page renders
// one row each way, and a provider missing from the list is indistinguishable from
// one the build does not support.
func TestListCoversEveryOAuthProvider(t *testing.T) {
	service := New(newStore(), newBox(t), envConfig())

	items := service.List()
	if len(items) != 2 {
		t.Fatalf("items = %+v, want gmail and outlook", items)
	}
	byProvider := map[string]Status{}
	for _, item := range items {
		byProvider[item.Provider] = item
	}
	gmail, outlook := byProvider["gmail"], byProvider["outlook"]
	if !gmail.Configured || gmail.Source != "environment" {
		t.Fatalf("gmail = %+v", gmail)
	}
	if outlook.Configured || outlook.Source != "none" {
		t.Fatalf("outlook = %+v", outlook)
	}
	// The redirect URI and the env keys are served because neither is guessable
	// from the page, and both are what a setup failure turns on.
	if outlook.RedirectURI != "http://localhost:13737/api/v1/oauth/outlook/callback" {
		t.Fatalf("outlook redirect = %q", outlook.RedirectURI)
	}
	if outlook.EnvClientIDKey != "NEXUSMAIL_MICROSOFT_CLIENT_ID" || outlook.EnvClientSecretKey != "NEXUSMAIL_MICROSOFT_CLIENT_SECRET" {
		t.Fatalf("outlook env keys = %q/%q", outlook.EnvClientIDKey, outlook.EnvClientSecretKey)
	}
}

func TestSaveRejectsWhatCannotAuthorize(t *testing.T) {
	store := newStore()
	service := New(store, newBox(t), config.Config{PublicURL: "http://localhost:13737"})
	ctx := context.Background()

	cases := []struct{ name, provider, id, secret string }{
		// A password provider has no OAuth client; a row for one would be dead
		// configuration no code path reads.
		{"password provider", "qq", "app-id", "app-secret"},
		{"unknown provider", "nope", "app-id", "app-secret"},
		// Half a pair cannot build a client, and storing it would present the
		// provider as configured.
		{"no secret", "gmail", "app-id", "   "},
		{"no id", "gmail", "", "app-secret"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if _, err := service.Save(ctx, item.provider, item.id, item.secret); !errors.Is(err, ports.ErrInvalidInput) {
				t.Fatalf("err = %v, want invalid input", err)
			}
		})
	}
	if store.upserts != 0 {
		t.Fatalf("%d rows written for refused input", store.upserts)
	}
	if err := service.Clear(ctx, "qq"); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("clear err = %v, want invalid input", err)
	}
	if _, err := service.Status("qq"); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("status err = %v, want invalid input", err)
	}
	// A provider name is normalised rather than refused for case: it arrives from a
	// URL path.
	if _, err := service.Save(ctx, " GMAIL ", "app-id", "app-secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, configured := service.ClientCredentials("gmail"); !configured {
		t.Fatal("a normalised provider name did not resolve")
	}
}

// A row sealed under a different master key cannot be decrypted, and skipping it
// would present the provider as unconfigured — sending the user to re-enter
// credentials that are already there, against a key that will reject them too.
// Failing the load is what surfaces the real problem at startup.
func TestLoadFailsOnAnUnreadableRow(t *testing.T) {
	store := newStore()
	if _, err := New(store, newBox(t), config.Config{}).Save(context.Background(), "gmail", "app-id", "app-secret"); err != nil {
		t.Fatal(err)
	}
	otherKey := make([]byte, 32)
	otherKey[0] = 9
	otherBox, err := cryptobox.New(otherKey)
	if err != nil {
		t.Fatal(err)
	}

	if err := New(store, otherBox, config.Config{}).Load(context.Background()); err == nil {
		t.Fatal("a row sealed under another key loaded without error")
	}
}

func TestLoadReportsAStoreFailure(t *testing.T) {
	store := newStore()
	store.listErr = errors.New("database is locked")

	if err := New(store, newBox(t), config.Config{}).Load(context.Background()); err == nil {
		t.Fatal("a failed read was reported as a success")
	}
}
