package provider

import (
	"errors"
	"testing"

	"nexusmail/internal/ports"
)

// AuthType and ServerSavesSent are the two preset fields other code branches on,
// and both are silent when wrong.
//
// AuthType decides which of AddPassword/AddOAuth the transport routes to, and the
// wrong value answers a valid request with the misleading "provider requires
// OAuth2" 400 (or seals a refresh token nothing can use).
//
// ServerSavesSent decides whether the send worker APPENDs the delivered message to
// Sent. Marked true for a provider that does not file sent mail itself, every
// message the user sends vanishes from their Sent folder; marked false for one that
// does, every message appears in it twice. Neither shows up as an error.
//
// Hosts and ports are pinned in TestCreateAccountAcceptsEveryPasswordPreset, where
// they are observable as stored account rows.
func TestPresetAuthAndSentFiling(t *testing.T) {
	for _, tc := range []struct {
		provider        string
		authType        string
		serverSavesSent bool
	}{
		{"qq", "password", false},
		{"163", "password", false},
		{"126", "password", false},
		// Apple publishes no OAuth endpoint for IMAP or SMTP, so iCloud is a
		// password provider despite being a platform vendor, and its SMTP does not
		// file sent mail — this client has to APPEND.
		{"icloud", "password", false},
		{"gmail", "oauth2", true},
		{"outlook", "oauth2", true},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			preset, err := Get(tc.provider)
			if err != nil {
				t.Fatalf("Get(%q): %v", tc.provider, err)
			}
			if string(preset.Provider) != tc.provider {
				t.Errorf("Provider = %q, want %q: the map key and the value disagree", preset.Provider, tc.provider)
			}
			if preset.AuthType != tc.authType {
				t.Errorf("AuthType = %q, want %q", preset.AuthType, tc.authType)
			}
			if preset.ServerSavesSent != tc.serverSavesSent {
				t.Errorf("ServerSavesSent = %v, want %v", preset.ServerSavesSent, tc.serverSavesSent)
			}
		})
	}
}

// The name is lowercased before lookup, so a client that posts "126" or "iCloud"
// as the brand writes it resolves to the same preset. iCloud is the case that
// matters: it is the only provider whose own spelling is not all lower case.
func TestGetIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"iCloud", "ICLOUD", "icloud"} {
		preset, err := Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if preset.Provider != "icloud" {
			t.Errorf("Get(%q) resolved to %q", name, preset.Provider)
		}
	}
}

// An unknown name must be refused as invalid input rather than falling through as a
// zero preset, which would build an account pointed at an empty host.
func TestGetRejectsUnknownProvider(t *testing.T) {
	preset, err := Get("fastmail")
	if !errors.Is(err, ports.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if preset != (Preset{}) {
		t.Errorf("a preset was returned for an unknown provider: %+v", preset)
	}
}
