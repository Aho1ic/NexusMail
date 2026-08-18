package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCacheBytes       = int64(2 << 30)
	defaultMaxOutboundBytes = int64(20 << 20)
)

type OAuthProvider struct {
	ClientID     string
	ClientSecret string
}

type Config struct {
	ListenAddr       string
	DataDir          string
	DatabasePath     string
	PublicURL        string
	APIKey           string
	MasterKey        []byte
	Google           OAuthProvider
	Microsoft        OAuthProvider
	BlobCacheBytes   int64
	MaxOutboundBytes int64
	SessionIdleTTL   time.Duration
	SessionMaxTTL    time.Duration
	ShutdownTimeout  time.Duration
	LogLevel         string
}

func Load() (Config, error) {
	dataDir := env("NEXUSMAIL_DATA_DIR", "./data")
	cfg := Config{
		ListenAddr:       env("NEXUSMAIL_LISTEN_ADDR", ":8080"),
		DataDir:          dataDir,
		DatabasePath:     env("NEXUSMAIL_DATABASE_PATH", filepath.Join(dataDir, "mail.db")),
		PublicURL:        strings.TrimRight(env("NEXUSMAIL_PUBLIC_URL", "http://localhost:8080"), "/"),
		APIKey:           secretEnv("NEXUSMAIL_API_KEY"),
		BlobCacheBytes:   defaultCacheBytes,
		MaxOutboundBytes: defaultMaxOutboundBytes,
		SessionIdleTTL:   12 * time.Hour,
		SessionMaxTTL:    7 * 24 * time.Hour,
		ShutdownTimeout:  15 * time.Second,
		LogLevel:         env("NEXUSMAIL_LOG_LEVEL", "info"),
		Google: OAuthProvider{
			ClientID:     secretEnv("NEXUSMAIL_GOOGLE_CLIENT_ID"),
			ClientSecret: secretEnv("NEXUSMAIL_GOOGLE_CLIENT_SECRET"),
		},
		Microsoft: OAuthProvider{
			ClientID:     secretEnv("NEXUSMAIL_MICROSOFT_CLIENT_ID"),
			ClientSecret: secretEnv("NEXUSMAIL_MICROSOFT_CLIENT_SECRET"),
		},
	}
	var err error
	if cfg.BlobCacheBytes, err = int64Env("NEXUSMAIL_BLOB_CACHE_BYTES", defaultCacheBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxOutboundBytes, err = int64Env("NEXUSMAIL_MAX_OUTBOUND_BYTES", defaultMaxOutboundBytes); err != nil {
		return Config{}, err
	}
	if len(cfg.APIKey) < 32 {
		return Config{}, errors.New("NEXUSMAIL_API_KEY must contain at least 32 characters")
	}
	masterKeyText := secretEnv("NEXUSMAIL_MASTER_KEY")
	key, err := base64.StdEncoding.DecodeString(masterKeyText)
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("NEXUSMAIL_MASTER_KEY must be a base64 encoded 32-byte key")
	}
	cfg.MasterKey = key
	if cfg.BlobCacheBytes < 0 || cfg.MaxOutboundBytes <= 0 {
		return Config{}, errors.New("cache and outbound size limits must be positive")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func secretEnv(key string) string {
	if path := strings.TrimSpace(os.Getenv(key + "_FILE")); path != "" {
		value, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(value))
		}
	}
	return strings.TrimSpace(os.Getenv(key))
}

func int64Env(key string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.New(key + " must be an integer")
	}
	return n, nil
}
