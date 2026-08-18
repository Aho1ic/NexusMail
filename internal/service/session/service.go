package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"nexusmail/internal/ports"
)

const CookieName = "nexusmail_session"

type Service struct {
	repo       ports.Repository
	apiKeyHash [32]byte
	idleTTL    time.Duration
	maxTTL     time.Duration
}

func New(repo ports.Repository, apiKey string, idleTTL, maxTTL time.Duration) *Service {
	return &Service{repo: repo, apiKeyHash: sha256.Sum256([]byte(apiKey)), idleTTL: idleTTL, maxTTL: maxTTL}
}

func (s *Service) CheckAPIKey(value string) bool {
	hash := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(hash[:], s.apiKeyHash[:]) == 1
}

func (s *Service) Create(ctx context.Context, apiKey string) (token, csrf string, expiresAt int64, err error) {
	if !s.CheckAPIKey(apiKey) {
		return "", "", 0, errors.New("invalid API key")
	}
	token, err = random(32)
	if err != nil {
		return "", "", 0, err
	}
	csrf, err = random(24)
	if err != nil {
		return "", "", 0, err
	}
	now := time.Now()
	expires := now.Add(s.idleTTL)
	absolute := now.Add(s.maxTTL)
	if expires.After(absolute) {
		expires = absolute
	}
	if err := s.repo.CreateSession(ctx, hash(token), hash(csrf), expires.UnixMilli(), absolute.UnixMilli()); err != nil {
		return "", "", 0, err
	}
	return token, csrf, expires.UnixMilli(), nil
}

func (s *Service) Validate(ctx context.Context, token, csrf string, requireCSRF bool) (bool, error) {
	csrfHash, valid, err := s.repo.ValidateSession(ctx, hash(token), time.Now().UnixMilli())
	if err != nil || !valid {
		return false, err
	}
	if requireCSRF && subtle.ConstantTimeCompare(csrfHash, hash(csrf)) != 1 {
		return false, nil
	}
	return true, nil
}

func (s *Service) Delete(ctx context.Context, token string) error {
	return s.repo.DeleteSession(ctx, hash(token))
}

func random(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func hash(value string) []byte { digest := sha256.Sum256([]byte(value)); return digest[:] }
