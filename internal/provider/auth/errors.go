package auth

import "errors"

// ErrCredentialRejected marks a failure the provider has definitively attributed
// to the credential itself: a revoked or expired refresh token, a client the
// provider no longer recognises, a consent screen missing the mailbox scope.
// Retrying any of these produces the same answer, so the account is parked and
// the user is asked to authorize again.
//
// It exists because the only signal the IMAP loops had was error text, and the
// token refresh path wraps every failure — including a TCP reset on the way to
// the provider's token endpoint — in the same "refresh OAuth token" prefix. That
// prefix was on the auth-failure marker list, so a transient network fault
// reaching oauth2.googleapis.com was reported to the user as "Invalid
// credentials" and parked the account for the full auth window. Whether the
// credential was rejected is knowable only where the HTTP response is, so the
// token source states it here instead of leaving it to be guessed from a string.
var ErrCredentialRejected = errors.New("credential rejected by provider")
