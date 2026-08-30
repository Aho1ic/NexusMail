package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validKey = "0123456789abcdef0123456789abcdef"

var validMasterKey = base64.StdEncoding.EncodeToString(make([]byte, 32))

// clearEnv removes every setting this package reads so one test cannot inherit
// another's environment, and restores nothing: t.Setenv already unwinds per test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"NEXUSMAIL_LISTEN_ADDR", "NEXUSMAIL_DATA_DIR", "NEXUSMAIL_DATABASE_PATH", "NEXUSMAIL_PUBLIC_URL",
		"NEXUSMAIL_API_KEY", "NEXUSMAIL_API_KEY_FILE", "NEXUSMAIL_MASTER_KEY", "NEXUSMAIL_MASTER_KEY_FILE",
		"NEXUSMAIL_BLOB_CACHE_BYTES", "NEXUSMAIL_MAX_OUTBOUND_BYTES", "NEXUSMAIL_LOG_LEVEL",
		"NEXUSMAIL_TRUSTED_PROXIES", "NEXUSMAIL_GOOGLE_CLIENT_ID", "NEXUSMAIL_GOOGLE_CLIENT_ID_FILE",
		"NEXUSMAIL_GOOGLE_CLIENT_SECRET", "NEXUSMAIL_GOOGLE_CLIENT_SECRET_FILE",
		"NEXUSMAIL_MICROSOFT_CLIENT_ID", "NEXUSMAIL_MICROSOFT_CLIENT_ID_FILE",
		"NEXUSMAIL_MICROSOFT_CLIENT_SECRET", "NEXUSMAIL_MICROSOFT_CLIENT_SECRET_FILE",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func withRequiredSecrets(t *testing.T) {
	t.Helper()
	clearEnv(t)
	t.Setenv("NEXUSMAIL_API_KEY", validKey)
	t.Setenv("NEXUSMAIL_MASTER_KEY", validMasterKey)
}

func TestLoadDefaults(t *testing.T) {
	withRequiredSecrets(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.DataDir != "./data" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	// The database path defaults inside the data dir, so moving the data dir moves
	// the database with it.
	if cfg.DatabasePath != filepath.Join("./data", "mail.db") {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.PublicURL != "http://localhost:8080" {
		t.Fatalf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.BlobCacheBytes != defaultCacheBytes || cfg.MaxOutboundBytes != defaultMaxOutboundBytes {
		t.Fatalf("limits = %d / %d", cfg.BlobCacheBytes, cfg.MaxOutboundBytes)
	}
	if cfg.SessionIdleTTL != 12*time.Hour || cfg.SessionMaxTTL != 7*24*time.Hour || cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("durations = %v / %v / %v", cfg.SessionIdleTTL, cfg.SessionMaxTTL, cfg.ShutdownTimeout)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("LogLevel = %q", cfg.LogLevel)
	}
	// No declared proxy means the peer address is the client address, which is what
	// the login throttle needs when the process is bound directly.
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("TrustedProxies = %v, want empty", cfg.TrustedProxies)
	}
	if len(cfg.MasterKey) != 32 {
		t.Fatalf("MasterKey length = %d", len(cfg.MasterKey))
	}
}

func TestLoadDatabasePathFollowsTheDataDir(t *testing.T) {
	withRequiredSecrets(t)
	t.Setenv("NEXUSMAIL_DATA_DIR", "/srv/mail")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != filepath.Join("/srv/mail", "mail.db") {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
}

func TestLoadExplicitDatabasePathWins(t *testing.T) {
	withRequiredSecrets(t)
	t.Setenv("NEXUSMAIL_DATA_DIR", "/srv/mail")
	t.Setenv("NEXUSMAIL_DATABASE_PATH", "/var/db/other.db")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != "/var/db/other.db" {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath)
	}
}

// The public URL is compared against the request Origin, so a trailing slash would
// make every same-origin check fail.
func TestLoadTrimsTrailingSlashesFromPublicURL(t *testing.T) {
	withRequiredSecrets(t)
	t.Setenv("NEXUSMAIL_PUBLIC_URL", "https://mail.example.com///")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://mail.example.com" {
		t.Fatalf("PublicURL = %q", cfg.PublicURL)
	}
}

func TestLoadRejectsAShortAPIKey(t *testing.T) {
	for _, key := range []string{"", "short", strings.Repeat("a", 31)} {
		t.Run(key, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NEXUSMAIL_API_KEY", key)
			t.Setenv("NEXUSMAIL_MASTER_KEY", validMasterKey)
			if _, err := Load(); err == nil {
				t.Fatalf("accepted a %d-character API key", len(key))
			}
		})
	}
}

func TestLoadAcceptsExactlyThirtyTwoCharacters(t *testing.T) {
	clearEnv(t)
	t.Setenv("NEXUSMAIL_API_KEY", strings.Repeat("a", 32))
	t.Setenv("NEXUSMAIL_MASTER_KEY", validMasterKey)
	if _, err := Load(); err != nil {
		t.Fatalf("a 32-character key was refused: %v", err)
	}
}

func TestLoadRejectsABadMasterKey(t *testing.T) {
	for name, value := range map[string]string{
		"empty":      "",
		"not base64": "!!!not base64!!!",
		"wrong size": base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"one byte短":  base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"one byte 长": base64.StdEncoding.EncodeToString(make([]byte, 33)),
	} {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NEXUSMAIL_API_KEY", validKey)
			t.Setenv("NEXUSMAIL_MASTER_KEY", value)
			if _, err := Load(); err == nil {
				t.Fatalf("accepted master key %q", value)
			}
		})
	}
}

func TestLoadParsesSizeLimits(t *testing.T) {
	withRequiredSecrets(t)
	t.Setenv("NEXUSMAIL_BLOB_CACHE_BYTES", "1048576")
	t.Setenv("NEXUSMAIL_MAX_OUTBOUND_BYTES", "2097152")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlobCacheBytes != 1048576 || cfg.MaxOutboundBytes != 2097152 {
		t.Fatalf("limits = %d / %d", cfg.BlobCacheBytes, cfg.MaxOutboundBytes)
	}
}

func TestLoadRejectsNonNumericSizeLimits(t *testing.T) {
	for _, key := range []string{"NEXUSMAIL_BLOB_CACHE_BYTES", "NEXUSMAIL_MAX_OUTBOUND_BYTES"} {
		t.Run(key, func(t *testing.T) {
			withRequiredSecrets(t)
			t.Setenv(key, "2GB")
			_, err := Load()
			if err == nil {
				t.Fatal("expected a non-numeric size to be refused")
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error %q does not name the offending setting", err)
			}
		})
	}
}

// A zero cache is legal — it means never keep a body on disk — but a zero outbound
// limit would reject every message, and a negative cache is meaningless.
func TestLoadRejectsImpossibleLimits(t *testing.T) {
	for name, env := range map[string][2]string{
		"negative cache":    {"NEXUSMAIL_BLOB_CACHE_BYTES", "-1"},
		"zero outbound":     {"NEXUSMAIL_MAX_OUTBOUND_BYTES", "0"},
		"negative outbound": {"NEXUSMAIL_MAX_OUTBOUND_BYTES", "-5"},
	} {
		t.Run(name, func(t *testing.T) {
			withRequiredSecrets(t)
			t.Setenv(env[0], env[1])
			if _, err := Load(); err == nil {
				t.Fatalf("accepted %s=%s", env[0], env[1])
			}
		})
	}
}

func TestLoadAcceptsAZeroCache(t *testing.T) {
	withRequiredSecrets(t)
	t.Setenv("NEXUSMAIL_BLOB_CACHE_BYTES", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlobCacheBytes != 0 {
		t.Fatalf("BlobCacheBytes = %d", cfg.BlobCacheBytes)
	}
}

func TestLoadReadsSecretsFromFiles(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "api-key")
	// A trailing newline is what a here-doc, an editor and a Docker secret all
	// produce, so it has to be trimmed or the key never matches.
	if err := os.WriteFile(keyPath, []byte(validKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	masterPath := filepath.Join(dir, "master-key")
	if err := os.WriteFile(masterPath, []byte("  "+validMasterKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXUSMAIL_API_KEY_FILE", keyPath)
	t.Setenv("NEXUSMAIL_MASTER_KEY_FILE", masterPath)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != validKey {
		t.Fatalf("APIKey = %q, want the file contents trimmed", cfg.APIKey)
	}
	if len(cfg.MasterKey) != 32 {
		t.Fatalf("MasterKey length = %d", len(cfg.MasterKey))
	}
}

// _FILE wins over the plain variable: the file is the deployment's real source and
// the variable is often a leftover from a compose default.
func TestSecretFileTakesPrecedence(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	fromFile := strings.Repeat("f", 32)
	if err := os.WriteFile(path, []byte(fromFile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXUSMAIL_API_KEY", strings.Repeat("e", 32))
	t.Setenv("NEXUSMAIL_API_KEY_FILE", path)
	t.Setenv("NEXUSMAIL_MASTER_KEY", validMasterKey)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != fromFile {
		t.Fatalf("APIKey = %q, want the file contents", cfg.APIKey)
	}
}

// A _FILE that names a path nothing can read is a broken deployment, and the mount
// that was supposed to supply the secret is the usual cause. Falling back to the
// plain variable used to hide that: an unreadable OAuth secret file left the
// process running with OAuth silently unconfigured.
func TestUnreadableSecretFileIsAnError(t *testing.T) {
	clearEnv(t)
	t.Setenv("NEXUSMAIL_API_KEY", validKey)
	t.Setenv("NEXUSMAIL_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("NEXUSMAIL_MASTER_KEY", validMasterKey)

	_, err := Load()
	if err == nil {
		t.Fatal("an unreadable secret file was ignored")
	}
	if !strings.Contains(err.Error(), "NEXUSMAIL_API_KEY_FILE") {
		t.Fatalf("error %q does not name the unreadable setting", err)
	}
}

func TestLoadReadsOAuthCredentials(t *testing.T) {
	withRequiredSecrets(t)
	t.Setenv("NEXUSMAIL_GOOGLE_CLIENT_ID", "google-id")
	t.Setenv("NEXUSMAIL_GOOGLE_CLIENT_SECRET", "google-secret")
	t.Setenv("NEXUSMAIL_MICROSOFT_CLIENT_ID", "ms-id")
	t.Setenv("NEXUSMAIL_MICROSOFT_CLIENT_SECRET", "ms-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Google.ClientID != "google-id" || cfg.Google.ClientSecret != "google-secret" {
		t.Fatalf("Google = %+v", cfg.Google)
	}
	if cfg.Microsoft.ClientID != "ms-id" || cfg.Microsoft.ClientSecret != "ms-secret" {
		t.Fatalf("Microsoft = %+v", cfg.Microsoft)
	}
}

func TestLoadTrustedProxies(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"10.0.0.1", []string{"10.0.0.1"}},
		{"10.0.0.1,172.16.0.0/12", []string{"10.0.0.1", "172.16.0.0/12"}},
		// A trailing comma or padded entry must not become a blank that matches
		// nothing yet still counts as a declared proxy.
		{" 10.0.0.1 , 192.168.0.0/16 ,", []string{"10.0.0.1", "192.168.0.0/16"}},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			withRequiredSecrets(t)
			t.Setenv("NEXUSMAIL_TRUSTED_PROXIES", tc.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.TrustedProxies) != len(tc.want) {
				t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, tc.want)
			}
			for i, want := range tc.want {
				if cfg.TrustedProxies[i] != want {
					t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, tc.want)
				}
			}
		})
	}
}

// A setting that is present but blank must fall back rather than produce an empty
// listen address, which would bind every interface on a random port.
func TestBlankSettingsFallBack(t *testing.T) {
	withRequiredSecrets(t)
	t.Setenv("NEXUSMAIL_LISTEN_ADDR", "   ")
	t.Setenv("NEXUSMAIL_LOG_LEVEL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" || cfg.LogLevel != "info" {
		t.Fatalf("ListenAddr=%q LogLevel=%q", cfg.ListenAddr, cfg.LogLevel)
	}
}
