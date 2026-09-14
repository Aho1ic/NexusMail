//go:build sqlite_fts5

package sqlite

import (
	"context"
	"testing"

	"nexusmail/internal/domain"
)

// The row is keyed on the provider, so re-saving one replaces the credentials in
// place. created_at has to survive that: the settings page shows when the pair was
// last changed, and a rewrite is a change to an existing client, not a new one.
func TestUpsertOAuthClientReplacesInPlace(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	first := domain.OAuthClient{Provider: "outlook", ClientID: "first-id", ClientSecretCiphertext: []byte("sealed-one"), CreatedAt: 1000, UpdatedAt: 1000}
	if err := store.UpsertOAuthClient(ctx, &first); err != nil {
		t.Fatal(err)
	}
	second := domain.OAuthClient{Provider: "outlook", ClientID: "second-id", ClientSecretCiphertext: []byte("sealed-two"), CreatedAt: 5000, UpdatedAt: 5000}
	if err := store.UpsertOAuthClient(ctx, &second); err != nil {
		t.Fatal(err)
	}

	items, err := store.ListOAuthClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("rows = %d, want one per provider", len(items))
	}
	row := items[0]
	if row.ClientID != "second-id" || string(row.ClientSecretCiphertext) != "sealed-two" {
		t.Fatalf("row = %+v, want the second save", row)
	}
	if row.CreatedAt != 1000 {
		t.Fatalf("created_at = %d, want the original 1000", row.CreatedAt)
	}
	if row.UpdatedAt != 5000 {
		t.Fatalf("updated_at = %d, want the rewrite's 5000", row.UpdatedAt)
	}
}

// Two providers are configured independently — one Google client and one Microsoft
// client — so a save for one must not disturb the other.
func TestOAuthClientsAreIndependentPerProvider(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"gmail", "outlook"} {
		client := domain.OAuthClient{Provider: name, ClientID: name + "-id", ClientSecretCiphertext: []byte("sealed-" + name), CreatedAt: 1, UpdatedAt: 1}
		if err := store.UpsertOAuthClient(ctx, &client); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteOAuthClient(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}

	items, err := store.ListOAuthClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Provider != "outlook" {
		t.Fatalf("rows = %+v, want only outlook", items)
	}
	// Deleting a provider with no row is the caller asking for an absence that is
	// already the case, which is not a failure.
	if err := store.DeleteOAuthClient(ctx, "gmail"); err != nil {
		t.Fatalf("deleting an absent row = %v", err)
	}
}

// A provider that does not authenticate with OAuth has no client, and the CHECK
// constraint is what keeps a typo or a mis-routed request from writing dead
// configuration no code path reads.
func TestOAuthClientRejectsANonOAuthProvider(t *testing.T) {
	store := openTestStore(t)

	client := domain.OAuthClient{Provider: "qq", ClientID: "id", ClientSecretCiphertext: []byte("sealed"), CreatedAt: 1, UpdatedAt: 1}
	if err := store.UpsertOAuthClient(context.Background(), &client); err == nil {
		t.Fatal("a password provider was accepted into oauth_clients")
	}
}
