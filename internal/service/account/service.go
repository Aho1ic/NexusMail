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

type Service struct {
	repo ports.Repository
	box  *cryptobox.Box
}

func New(repo ports.Repository, box *cryptobox.Box) *Service { return &Service{repo: repo, box: box} }

func (s *Service) AddPassword(ctx context.Context, providerName, email, displayName, username, password string) (domain.Account, error) {
	preset, err := provider.Get(providerName)
	if err != nil {
		return domain.Account{}, err
	}
	if preset.AuthType != "password" {
		return domain.Account{}, errors.New("provider requires OAuth2")
	}
	address, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil || address.Address != strings.TrimSpace(email) {
		return domain.Account{}, errors.New("invalid email address")
	}
	if username == "" {
		username = address.Address
	}
	if password == "" {
		return domain.Account{}, errors.New("authorization code is required")
	}
	return s.create(ctx, preset, address.Address, displayName, username, Credential{Password: password})
}

func (s *Service) AddOAuth(ctx context.Context, providerName, email, displayName, refreshToken string) (domain.Account, error) {
	preset, err := provider.Get(providerName)
	if err != nil {
		return domain.Account{}, err
	}
	if preset.AuthType != "oauth2" || refreshToken == "" {
		return domain.Account{}, errors.New("invalid OAuth account")
	}
	return s.create(ctx, preset, email, displayName, email, Credential{RefreshToken: refreshToken})
}

func (s *Service) create(ctx context.Context, preset provider.Preset, email, displayName, username string, credential Credential) (domain.Account, error) {
	plaintext, _ := json.Marshal(credential)
	ciphertext, err := s.box.Seal(plaintext)
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

func (s *Service) List(ctx context.Context) ([]domain.Account, error) {
	return s.repo.ListAccounts(ctx)
}
