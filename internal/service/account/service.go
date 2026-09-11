package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"

	"nexusmail/internal/domain"
	"nexusmail/internal/platform/cryptobox"
	"nexusmail/internal/ports"
	"nexusmail/internal/provider"
)

type Credential struct {
	Password     string `json:"password,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// Store is the slice of persistence this service uses.
type Store interface {
	CreateAccount(context.Context, *domain.Account) error
	GetAccount(context.Context, int64) (domain.Account, error)
	ListAccounts(context.Context) ([]domain.Account, error)
	UpdateAccountSecret(context.Context, int64, []byte) error
	DeleteAccount(context.Context, int64) error
}

type Service struct {
	repo Store
	box  *cryptobox.Box
}

func New(repo Store, box *cryptobox.Box) *Service { return &Service{repo: repo, box: box} }

func (s *Service) AddPassword(ctx context.Context, providerName, email, displayName, username, password string) (domain.Account, error) {
	preset, err := provider.Get(providerName)
	if err != nil {
		return domain.Account{}, err
	}
	if preset.AuthType != "password" {
		return domain.Account{}, ports.Invalidf("provider requires OAuth2")
	}
	address, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil || address.Address != strings.TrimSpace(email) {
		return domain.Account{}, ports.Invalidf("invalid email address")
	}
	if username == "" {
		username = address.Address
	}
	if password == "" {
		return domain.Account{}, ports.Invalidf("authorization code is required")
	}
	return s.create(ctx, preset, address.Address, displayName, username, Credential{Password: password})
}

func (s *Service) AddOAuth(ctx context.Context, providerName, email, displayName, refreshToken string) (domain.Account, error) {
	preset, err := provider.Get(providerName)
	if err != nil {
		return domain.Account{}, err
	}
	if preset.AuthType != "oauth2" || refreshToken == "" {
		return domain.Account{}, ports.Invalidf("invalid OAuth account")
	}
	return s.create(ctx, preset, email, displayName, email, Credential{RefreshToken: refreshToken})
}

func (s *Service) create(ctx context.Context, preset provider.Preset, email, displayName, username string, credential Credential) (domain.Account, error) {
	ciphertext, err := s.seal(credential)
	if err != nil {
		return domain.Account{}, err
	}
	now := time.Now().UnixMilli()
	account := domain.Account{
		Email: email, DisplayName: displayName, Provider: string(preset.Provider), AuthType: preset.AuthType,
		Username: username, IMAPHost: preset.IMAPHost, IMAPPort: preset.IMAPPort, IMAPTLSMode: preset.IMAPTLSMode,
		SMTPHost: preset.SMTPHost, SMTPPort: preset.SMTPPort, SMTPTLSMode: preset.SMTPTLSMode,
		SecretCiphertext: ciphertext, Status: "disconnected", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.CreateAccount(ctx, &account); err != nil {
		return domain.Account{}, err
	}
	return account, nil
}

func (s *Service) Credential(account domain.Account) (Credential, error) {
	plaintext, err := s.box.Open(account.SecretCiphertext)
	if err != nil {
		return Credential{}, err
	}
	var credential Credential
	if err := json.Unmarshal(plaintext, &credential); err != nil {
		return Credential{}, errors.New("invalid stored credential")
	}
	return credential, nil
}

// seal is the one place a Credential becomes ciphertext, so create and
// UpdateRefreshToken cannot drift into two different envelopes for the same blob.
func (s *Service) seal(credential Credential) ([]byte, error) {
	plaintext, err := json.Marshal(credential)
	if err != nil {
		return nil, err
	}
	return s.box.Seal(plaintext)
}

// UpdateRefreshToken reseals the account's credential around a rotated refresh
// token. The credential is a sealed JSON blob, so the only way to change one field
// is to open it, replace the field and seal the whole thing again; the password (a
// password account never reaches here, but a hybrid one could) is carried through
// untouched rather than being dropped.
func (s *Service) UpdateRefreshToken(ctx context.Context, accountID int64, refreshToken string) error {
	if refreshToken == "" {
		return ports.Invalidf("refresh token is required")
	}
	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	credential, err := s.Credential(account)
	if err != nil {
		return err
	}
	if credential.RefreshToken == refreshToken {
		return nil
	}
	credential.RefreshToken = refreshToken
	ciphertext, err := s.seal(credential)
	if err != nil {
		return err
	}
	return s.repo.UpdateAccountSecret(ctx, accountID, ciphertext)
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	return s.repo.DeleteAccount(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]domain.Account, error) {
	return s.repo.ListAccounts(ctx)
}
