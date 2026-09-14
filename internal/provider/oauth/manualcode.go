package oauth

import (
	"net/url"
	"strings"

	"nexusmail/internal/ports"
)

// ParseManualCode extracts the authorization code from what a user pasted into the
// manual completion field.
//
// The manual flow exists for deployments the provider cannot redirect back to: the
// browser is left on an error page whose address bar still holds
// `…/callback?code=…&state=…`, and pasting that back is how the handshake
// completes. Asking the user to isolate the code from a 600-character URL is where
// that goes wrong, so both forms are accepted — a bare code, or the whole
// redirect.
//
// A pasted redirect may also carry `error=access_denied`, which is a refused
// consent rather than a malformed paste and is reported as such: without this the
// user is told their code is invalid when in fact they pressed "No".
//
// The state embedded in a pasted URL is returned so the caller can prefer it over
// the one the page was holding. They agree in the normal case; when they do not,
// the URL is the authorization the user actually completed.
func ParseManualCode(input string) (code, state string, err error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", ports.Invalidf("authorization code is required")
	}
	// Only something that looks like a URL or a bare query string is parsed as one.
	// An authorization code is opaque and Microsoft's contains dots, dashes and
	// underscores; treating every input as a URL would let a code that happens to
	// contain "=" be shredded into query parameters.
	if !strings.Contains(trimmed, "code=") && !strings.Contains(trimmed, "error=") {
		return trimmed, "", nil
	}
	query := trimmed
	if index := strings.IndexAny(trimmed, "?#"); index >= 0 {
		query = trimmed[index+1:]
	}
	values, parseErr := url.ParseQuery(query)
	if parseErr != nil {
		return "", "", ports.Invalidf("could not read the authorization code from the pasted address")
	}
	if refusal := values.Get("error"); refusal != "" {
		return "", "", ports.Invalidf("provider refused the authorization: %s", refusal)
	}
	code = strings.TrimSpace(values.Get("code"))
	if code == "" {
		return "", "", ports.Invalidf("the pasted address carries no authorization code")
	}
	return code, strings.TrimSpace(values.Get("state")), nil
}
