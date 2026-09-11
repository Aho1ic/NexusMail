package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexusmail/internal/domain"
	"nexusmail/internal/platform/cryptobox"
	"nexusmail/internal/ports"
)

type fakeStore struct {
	created   *domain.Account
	createErr error
	list      []domain.Account
	listErr   error

	account   domain.Account
	getErr    error
	secret    []byte
	secretID  int64
	secretErr error
	deletedID int64
	deletes   int
	deleteErr error
}

func (f *fakeStore) CreateAccount(_ context.Context, account *domain.Account) error {
	if f.createErr != nil {
		return f.createErr
	}
	account.ID = 1
	copied := *account
	f.created = &copied
	return nil
}
func (f *fakeStore) ListAccounts(context.Context) ([]domain.Account, error) {
	return f.list, f.listErr
}
func (f *fakeStore) GetAccount(context.Context, int64) (domain.Account, error) {
	return f.account, f.getErr
}
func (f *fakeStore) UpdateAccountSecret(_ context.Context, id int64, ciphertext []byte) error {
	if f.secretErr != nil {
		return f.secretErr
	}
	f.secretID, f.secret = id, ciphertext
	return nil
}
func (f *fakeStore) DeleteAccount(_ context.Context, id int64) error {
	f.deletes++
	f.deletedID = id
	return f.deleteErr
}

func newBox(t *testing.T) *cryptobox.Box {
	t.Helper()
	box, err := cryptobox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestAddPasswordRejectsUnknownProvider(t *testing.T) {
	store := &fakeStore{}
	service := New(store, newBox(t))
	if _, err := service.AddPassword(context.Background(), "nope", "a@b.com", "", "", "secret"); err == nil {
		t.Fatal("expected an unknown provider to be refused")
	}
	if store.created != nil {
		t.Fatal("an account was stored for an unknown provider")
	}
}

// A provider that only speaks OAuth must not accept a password: the credential
// would be sealed and stored, then fail on every connection attempt.
func TestAddPasswordRefusesOAuthOnlyProvider(t *testing.T) {
	store := &fakeStore{}
	service := New(store, newBox(t))
	if _, err := service.AddPassword(context.Background(), "gmail", "a@gmail.com", "", "", "secret"); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if store.created != nil {
		t.Fatal("stored a password account for an OAuth-only provider")
	}
}

func TestAddPasswordValidatesTheAddress(t *testing.T) {
	for _, email := range []string{"", "not-an-address", "a@b.com, c@d.com", "Someone <a@b.com>", " a@b.com "} {
		t.Run(email, func(t *testing.T) {
			store := &fakeStore{}
			service := New(store, newBox(t))
			_, err := service.AddPassword(context.Background(), "qq", email, "", "", "secret")
			if email == " a@b.com " {
				// Surrounding space is trimmed before parsing, so this one is accepted.
				if err != nil {
					t.Fatalf("err = %v, want the trimmed address to be accepted", err)
				}
				if store.created.Email != "a@b.com" {
					t.Fatalf("stored email = %q, want the trimmed form", store.created.Email)
				}
				return
			}
			if !errors.Is(err, ports.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if store.created != nil {
				t.Fatal("stored an account with an unusable address")
			}
		})
	}
}

func TestAddPasswordRequiresAPassword(t *testing.T) {
	store := &fakeStore{}
	service := New(store, newBox(t))
	if _, err := service.AddPassword(context.Background(), "qq", "a@qq.com", "", "", ""); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if store.created != nil {
		t.Fatal("stored an account with no credential")
	}
}

func TestAddPasswordSealsTheCredentialAndFillsThePreset(t *testing.T) {
	store := &fakeStore{}
	box := newBox(t)
	service := New(store, box)

	account, err := service.AddPassword(context.Background(), "qq", "user@qq.com", "Work", "", "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	if account.Provider != "qq" || account.AuthType != "password" {
		t.Fatalf("account = %+v", account)
	}
	// A missing username defaults to the address, since that is what QQ and 163
	// expect at LOGIN.
	if account.Username != "user@qq.com" {
		t.Fatalf("username = %q", account.Username)
	}
	if account.IMAPHost == "" || account.IMAPPort == 0 || account.SMTPHost == "" || account.SMTPPort == 0 {
		t.Fatalf("preset was not applied: %+v", account)
	}
	if account.Status != "disconnected" || account.CreatedAt == 0 || account.UpdatedAt == 0 {
		t.Fatalf("account = %+v", account)
	}
	// The password must never be recoverable from the row without the master key.
	if strings.Contains(string(account.SecretCiphertext), "auth-code") {
		t.Fatal("the credential was stored in the clear")
	}
	credential, err := service.Credential(account)
	if err != nil || credential.Password != "auth-code" {
		t.Fatalf("Credential = %+v, %v", credential, err)
	}
}

func TestAddPasswordKeepsAnExplicitUsername(t *testing.T) {
	store := &fakeStore{}
	service := New(store, newBox(t))
	if _, err := service.AddPassword(context.Background(), "163", "user@163.com", "", "login-name", "code"); err != nil {
		t.Fatal(err)
	}
	if store.created.Username != "login-name" {
		t.Fatalf("username = %q, want the explicit one", store.created.Username)
	}
}

func TestAddPasswordPropagatesStoreError(t *testing.T) {
	service := New(&fakeStore{createErr: errors.New("unique constraint")}, newBox(t))
	if _, err := service.AddPassword(context.Background(), "qq", "a@qq.com", "", "", "code"); err == nil {
		t.Fatal("expected the store error")
	}
}

func TestAddOAuthRequiresARefreshToken(t *testing.T) {
	store := &fakeStore{}
	service := New(store, newBox(t))
	if _, err := service.AddOAuth(context.Background(), "gmail", "a@gmail.com", "", ""); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if store.created != nil {
		t.Fatal("stored an OAuth account with no refresh token")
	}
}

// The mirror of the password case: a password provider has no OAuth flow, so
// accepting a refresh token for it would store a credential nothing can use.
func TestAddOAuthRefusesPasswordProvider(t *testing.T) {
	service := New(&fakeStore{}, newBox(t))
	if _, err := service.AddOAuth(context.Background(), "qq", "a@qq.com", "", "token"); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestAddOAuthRejectsUnknownProvider(t *testing.T) {
	service := New(&fakeStore{}, newBox(t))
	if _, err := service.AddOAuth(context.Background(), "nope", "a@b.com", "", "token"); err == nil {
		t.Fatal("expected an unknown provider to be refused")
	}
}

func TestAddOAuthSealsTheRefreshToken(t *testing.T) {
	store := &fakeStore{}
	service := New(store, newBox(t))

	account, err := service.AddOAuth(context.Background(), "gmail", "a@gmail.com", "Personal", "refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	if account.AuthType != "oauth2" || account.Username != "a@gmail.com" {
		t.Fatalf("account = %+v", account)
	}
	if strings.Contains(string(account.SecretCiphertext), "refresh-token") {
		t.Fatal("the refresh token was stored in the clear")
	}
	credential, err := service.Credential(account)
	if err != nil || credential.RefreshToken != "refresh-token" {
		t.Fatalf("Credential = %+v, %v", credential, err)
	}
	if credential.Password != "" {
		t.Fatalf("an OAuth credential carried a password: %+v", credential)
	}
}

func TestAddOAuthPropagatesStoreError(t *testing.T) {
	service := New(&fakeStore{createErr: errors.New("busy")}, newBox(t))
	if _, err := service.AddOAuth(context.Background(), "outlook", "a@outlook.com", "", "token"); err == nil {
		t.Fatal("expected the store error")
	}
}

// A row sealed under a different master key must fail closed rather than yield an
// empty credential that would be sent to the provider as a blank password.
func TestCredentialRejectsForeignCiphertext(t *testing.T) {
	store := &fakeStore{}
	original := New(store, newBox(t))
	account, err := original.AddPassword(context.Background(), "qq", "a@qq.com", "", "", "code")
	if err != nil {
		t.Fatal(err)
	}

	otherKey := make([]byte, 32)
	otherKey[0] = 1
	otherBox, err := cryptobox.New(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(store, otherBox).Credential(account); err == nil {
		t.Fatal("a credential sealed under another key was accepted")
	}
}

func TestCredentialRejectsNonJSONPlaintext(t *testing.T) {
	box := newBox(t)
	ciphertext, err := box.Seal([]byte("not json"))
	if err != nil {
		t.Fatal(err)
	}
	service := New(&fakeStore{}, box)
	if _, err := service.Credential(domain.Account{SecretCiphertext: ciphertext}); err == nil {
		t.Fatal("expected the malformed credential to be refused")
	}
}

func TestListPassesThrough(t *testing.T) {
	store := &fakeStore{list: []domain.Account{{ID: 1}, {ID: 2}}}
	service := New(store, newBox(t))
	items, err := service.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("List = %+v, %v", items, err)
	}

	failing := New(&fakeStore{listErr: errors.New("boom")}, newBox(t))
	if _, err := failing.List(context.Background()); err == nil {
		t.Fatal("expected the store error")
	}
}

// A rotated refresh token has to survive a restart, so it goes back into the sealed
// credential rather than living only in the token cache.
func TestUpdateRefreshTokenReseals(t *testing.T) {
	box := newBox(t)
	store := &fakeStore{}
	service := New(store, box)
	account, err := service.AddOAuth(context.Background(), "outlook", "a@outlook.com", "", "first-token")
	if err != nil {
		t.Fatal(err)
	}
	store.account = account

	if err := service.UpdateRefreshToken(context.Background(), account.ID, "second-token"); err != nil {
		t.Fatal(err)
	}
	if store.secretID != account.ID || store.secret == nil {
		t.Fatalf("secret written for id=%d (%v)", store.secretID, store.secret)
	}
	credential, err := service.Credential(domain.Account{SecretCiphertext: store.secret})
	if err != nil {
		t.Fatal(err)
	}
	if credential.RefreshToken != "second-token" {
		t.Fatalf("refresh token = %q, want second-token", credential.RefreshToken)
	}
}

// An unchanged token must not cost a write: AccessToken calls this on every refresh
// and most providers return the same refresh token every time.
func TestUpdateRefreshTokenSkipsAnUnchangedToken(t *testing.T) {
	box := newBox(t)
	store := &fakeStore{}
	service := New(store, box)
	account, err := service.AddOAuth(context.Background(), "gmail", "a@gmail.com", "", "same-token")
	if err != nil {
		t.Fatal(err)
	}
	store.account = account

	if err := service.UpdateRefreshToken(context.Background(), account.ID, "same-token"); err != nil {
		t.Fatal(err)
	}
	if store.secret != nil {
		t.Fatal("resealed an unchanged credential")
	}
}

func TestUpdateRefreshTokenRequiresAToken(t *testing.T) {
	service := New(&fakeStore{}, newBox(t))
	if err := service.UpdateRefreshToken(context.Background(), 1, ""); !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestDeletePassesThrough(t *testing.T) {
	store := &fakeStore{}
	service := New(store, newBox(t))
	if err := service.Delete(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if store.deletes != 1 || store.deletedID != 7 {
		t.Fatalf("delete calls=%d id=%d", store.deletes, store.deletedID)
	}

	failing := New(&fakeStore{deleteErr: errors.New("busy")}, newBox(t))
	if err := failing.Delete(context.Background(), 7); err == nil {
		t.Fatal("expected the store error")
	}
}
