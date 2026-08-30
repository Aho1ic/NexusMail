package config

import (
	"strings"
	"testing"
)

// TestLoadRejectsPublicURLsThatCannotBeOrigins covers the configuration boundary
// shared by OAuth callback construction and the cookie-session same-origin check.
// A bad value used to let the process start and fail only on a later login or write
// request; failing at boot tells the operator which setting needs correcting.
func TestLoadRejectsPublicURLsThatCannotBeOrigins(t *testing.T) {
	for _, value := range []string{
		"mail.example", "ftp://mail.example", "https:///missing-host",
		"https://user@mail.example", "https://mail.example/nexusmail", "https://mail.example?x=1",
		"https://mail.example#fragment", "https://mail.example:0", "https://mail.example:65536", "https://mail.example:not-a-port",
	} {
		t.Run(value, func(t *testing.T) {
			withRequiredSecrets(t)
			t.Setenv("NEXUSMAIL_PUBLIC_URL", value)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted public URL %q", value)
			}
			if !strings.Contains(err.Error(), "NEXUSMAIL_PUBLIC_URL") {
				t.Errorf("error %q does not identify the invalid setting", err)
			}
		})
	}
}

// TestLoadAcceptsOriginShapedPublicURLs protects legitimate direct and reverse
// proxy deployments, including explicit default ports. Port normalization happens
// later in sameOrigin; config only needs to reject values that cannot be origins.
func TestLoadAcceptsOriginShapedPublicURLs(t *testing.T) {
	for _, value := range []string{
		"http://localhost:8080", "https://mail.example", "https://mail.example:443", "http://[::1]:8080",
	} {
		t.Run(value, func(t *testing.T) {
			withRequiredSecrets(t)
			t.Setenv("NEXUSMAIL_PUBLIC_URL", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(%q): %v", value, err)
			}
			if cfg.PublicURL != value {
				t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, value)
			}
		})
	}
}
