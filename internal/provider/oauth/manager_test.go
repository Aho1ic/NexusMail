package oauth

import (
	"testing"

	"golang.org/x/oauth2"
)

func TestScopeGranted(t *testing.T) {
	tests := []struct {
		name     string
		granted  string
		required string
		want     bool
	}{
		{name: "full URL", granted: "openid https://mail.google.com/", required: "https://mail.google.com/", want: true},
		{name: "short provider scope", granted: "openid IMAP.AccessAsUser.All", required: "https://outlook.office.com/IMAP.AccessAsUser.All", want: true},
		{name: "missing scope", granted: "openid email", required: "https://mail.google.com/", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scopeGranted(tt.granted, tt.required); got != tt.want {
				t.Fatalf("scopeGranted(%q, %q) = %v, want %v", tt.granted, tt.required, got, tt.want)
			}
		})
	}
}

func TestVerifyMailScope(t *testing.T) {
	token := (&oauth2.Token{}).WithExtra(map[string]any{"scope": "openid https://mail.google.com/"})
	if err := verifyMailScope("gmail", token); err != nil {
		t.Fatalf("verifyMailScope() with required scope: %v", err)
	}

	withoutScope := (&oauth2.Token{}).WithExtra(map[string]any{"scope": "openid email"})
	if err := verifyMailScope("gmail", withoutScope); err == nil {
		t.Fatal("verifyMailScope() accepted a token without the Gmail scope")
	}

	unknown := (&oauth2.Token{}).WithExtra(map[string]any{"scope": "openid"})
	if err := verifyMailScope("unknown", unknown); err != nil {
		t.Fatalf("verifyMailScope() rejected an unknown provider: %v", err)
	}
}
