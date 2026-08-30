package auth

import (
	"testing"

	"github.com/emersion/go-sasl"
)

// The wire format is fixed by RFC 7628 / the Google and Microsoft docs: the two
// fields are separated by \x01 and the response ends with \x01\x01. A server
// rejects anything else, so the bytes are asserted exactly.
func TestStartProducesTheXOAUTH2Response(t *testing.T) {
	client := &XOAUTH2{Username: "user@example.com", AccessToken: "ya29.token"}
	mechanism, response, err := client.Start()
	if err != nil {
		t.Fatal(err)
	}
	if mechanism != "XOAUTH2" {
		t.Fatalf("mechanism = %q, want XOAUTH2", mechanism)
	}
	want := "user=user@example.com\x01auth=Bearer ya29.token\x01\x01"
	if string(response) != want {
		t.Fatalf("response = %q, want %q", response, want)
	}
}

func TestStartRequiresBothFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *XOAUTH2
	}{
		{"neither", &XOAUTH2{}},
		{"no username", &XOAUTH2{AccessToken: "token"}},
		{"no token", &XOAUTH2{Username: "user@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mechanism, response, err := tc.client.Start()
			if err == nil {
				t.Fatal("expected empty credentials to be refused")
			}
			// Nothing may be sent when the credentials are unusable: an empty
			// user= line reads as a failed login attempt to the provider, which
			// counts against the account's rate limit.
			if mechanism != "" || response != nil {
				t.Fatalf("Start returned %q / %q alongside the error", mechanism, response)
			}
		})
	}
}

// The server sends one empty challenge after a successful response, and the client
// answers it with an empty string. A second challenge means the exchange went off
// the rails, and answering it again would loop.
func TestNextAnswersOneChallengeOnly(t *testing.T) {
	client := &XOAUTH2{Username: "u", AccessToken: "t"}
	response, err := client.Next([]byte("challenge"))
	if err != nil {
		t.Fatal(err)
	}
	if len(response) != 0 {
		t.Fatalf("first challenge answered with %q, want empty", response)
	}
	if _, err := client.Next([]byte("challenge")); err == nil {
		t.Fatal("a repeated challenge was accepted")
	}
}

// The type is handed to go-imap and go-smtp as a sasl.Client, so it has to satisfy
// that interface — a signature drift would only show up at the call site otherwise.
func TestSatisfiesSASLClient(t *testing.T) {
	var client sasl.Client = &XOAUTH2{Username: "u", AccessToken: "t"}
	if _, _, err := client.Start(); err != nil {
		t.Fatal(err)
	}
}
