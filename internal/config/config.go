package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
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
	// TrustedProxies lists the hops allowed to set X-Forwarded-For. Empty means
	// the peer address is the client address, which is what a direct bind wants:
	// otherwise the login throttle can be sidestepped by forging the header.
	TrustedProxies []string
}

func Load() (Config, error) {
	dataDir := env("NEXUSMAIL_DATA_DIR", "./data")
	secrets, err := secretEnvs(
		"NEXUSMAIL_API_KEY", "NEXUSMAIL_MASTER_KEY",
		"NEXUSMAIL_GOOGLE_CLIENT_ID", "NEXUSMAIL_GOOGLE_CLIENT_SECRET",
		"NEXUSMAIL_MICROSOFT_CLIENT_ID", "NEXUSMAIL_MICROSOFT_CLIENT_SECRET",
	)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:       env("NEXUSMAIL_LISTEN_ADDR", ":8080"),
		DataDir:          dataDir,
		DatabasePath:     env("NEXUSMAIL_DATABASE_PATH", filepath.Join(dataDir, "mail.db")),
		PublicURL:        strings.TrimRight(env("NEXUSMAIL_PUBLIC_URL", "http://localhost:8080"), "/"),
		APIKey:           secrets["NEXUSMAIL_API_KEY"],
		BlobCacheBytes:   defaultCacheBytes,
		MaxOutboundBytes: defaultMaxOutboundBytes,
		SessionIdleTTL:   12 * time.Hour,
		SessionMaxTTL:    7 * 24 * time.Hour,
		ShutdownTimeout:  15 * time.Second,
		LogLevel:         env("NEXUSMAIL_LOG_LEVEL", "info"),
		TrustedProxies:   splitList(os.Getenv("NEXUSMAIL_TRUSTED_PROXIES")),
		Google: OAuthProvider{
			ClientID:     secrets["NEXUSMAIL_GOOGLE_CLIENT_ID"],
			ClientSecret: secrets["NEXUSMAIL_GOOGLE_CLIENT_SECRET"],
		},
		Microsoft: OAuthProvider{
			ClientID:     secrets["NEXUSMAIL_MICROSOFT_CLIENT_ID"],
			ClientSecret: secrets["NEXUSMAIL_MICROSOFT_CLIENT_SECRET"],
		},
	}
	if cfg.BlobCacheBytes, err = int64Env("NEXUSMAIL_BLOB_CACHE_BYTES", defaultCacheBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxOutboundBytes, err = int64Env("NEXUSMAIL_MAX_OUTBOUND_BYTES", defaultMaxOutboundBytes); err != nil {
		return Config{}, err
	}
	if len(cfg.APIKey) < 32 {
		return Config{}, errors.New("NEXUSMAIL_API_KEY must contain at least 32 characters")
	}
	key, err := base64.StdEncoding.DecodeString(secrets["NEXUSMAIL_MASTER_KEY"])
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("NEXUSMAIL_MASTER_KEY must be a base64 encoded 32-byte key")
	}
	cfg.MasterKey = key
	if cfg.BlobCacheBytes < 0 || cfg.MaxOutboundBytes <= 0 {
		return Config{}, errors.New("cache and outbound size limits must be positive")
	}
	if err := validatePublicURL(cfg.PublicURL); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validatePublicURL rejects a public URL that cannot serve the two jobs it has:
// deciding same-origin for cookie-authenticated mutations, and forming the OAuth
// redirect URI by concatenation. Both silently misbehave on a malformed value —
// every state-changing request answers 403, or the provider rejects the redirect —
// so this is checked at startup instead of surfacing much later as a runtime denial.
// A path is refused because no route is served under a base path; accepting one
// would build a redirect URI that does not resolve.
func validatePublicURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("NEXUSMAIL_PUBLIC_URL is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("NEXUSMAIL_PUBLIC_URL must use http or https")
	}
	if parsed.Hostname() == "" {
		return errors.New("NEXUSMAIL_PUBLIC_URL must include a host")
	}
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("NEXUSMAIL_PUBLIC_URL must be a scheme, host and optional port only")
	}
	if port := parsed.Port(); port != "" {
		number, convErr := strconv.Atoi(port)
		if convErr != nil || number < 1 || number > 65535 {
			return errors.New("NEXUSMAIL_PUBLIC_URL has an invalid port")
		}
	}
	return nil
}

// splitList reads a comma separated setting, dropping blanks so a trailing comma
// or a value of " " cannot turn into an entry that matches nothing.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// secretEnv reads a secret from KEY_FILE when that is set, else from KEY. A
// _FILE that names an unreadable path is an error rather than a fall back to the
// plain variable: the file is the deployment's real source, so a failed read means
// the mount that was meant to supply it is broken. Falling back hid that — an
// unreadable OAuth secret left the process up with OAuth silently unconfigured,
// and the user only saw "not configured" much later, from the authorize button.
func secretEnv(key string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(key + "_FILE")); path != "" {
		value, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", key, err)
		}
		return strings.TrimSpace(string(value)), nil
	}
	return strings.TrimSpace(os.Getenv(key)), nil
}

// secretEnvs resolves several secrets, stopping at the first unreadable _FILE so
// the message names the setting the operator has to fix.
func secretEnvs(keys ...string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		value, err := secretEnv(key)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
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
