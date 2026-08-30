package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

type fakeStore struct {
	tokenHash  []byte
	csrfHash   []byte
	expiresAt  int64
	absoluteAt int64
	createErr  error

	validateHash  []byte
	validateNow   int64
	validateCSRF  []byte
	validateOK    bool
	validateErr   error
	deletedHash   []byte
	deleteErr     error
	createdCalls  int
	validateCalls int
}

func (f *fakeStore) CreateSession(_ context.Context, token, csrf []byte, expires, absolute int64) error {
	f.createdCalls++
	if f.createErr != nil {
		return f.createErr
	}
	f.tokenHash, f.csrfHash, f.expiresAt, f.absoluteAt = token, csrf, expires, absolute
	return nil
}
func (f *fakeStore) ValidateSession(_ context.Context, token []byte, now int64) ([]byte, bool, error) {
	f.validateCalls++
	f.validateHash, f.validateNow = token, now
	return f.validateCSRF, f.validateOK, f.validateErr
}
func (f *fakeStore) DeleteSession(_ context.Context, token []byte) error {
	f.deletedHash = token
	return f.deleteErr
}

const apiKey = "0123456789abcdef0123456789abcdef"

func newService(store Store) *Service {
	return New(store, apiKey, 30*time.Minute, 12*time.Hour)
}

func TestCheckAPIKey(t *testing.T) {
	service := newService(&fakeStore{})
	if !service.CheckAPIKey(apiKey) {
		t.Fatal("the configured key was rejected")
	}
	for _, wrong := range []string{"", apiKey + "x", apiKey[:len(apiKey)-1], "0123456789ABCDEF0123456789abcdef"} {
		if service.CheckAPIKey(wrong) {
			t.Fatalf("accepted %q", wrong)
		}
	}
}

func TestCreateRejectsAWrongKeyWithoutTouchingTheStore(t *testing.T) {
	store := &fakeStore{}
	service := newService(store)
	if _, _, _, err := service.Create(context.Background(), "wrong"); err == nil {
		t.Fatal("expected the wrong key to be refused")
	}
	if store.createdCalls != 0 {
		t.Fatal("a session row was written for a failed login")
	}
}

// Only hashes may reach the database: a stolen table must not yield usable tokens.
func TestCreateStoresOnlyHashes(t *testing.T) {
	store := &fakeStore{}
	service := newService(store)

	token, csrf, expires, err := service.Create(context.Background(), apiKey)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || csrf == "" || token == csrf {
		t.Fatalf("token=%q csrf=%q", token, csrf)
	}
	tokenDigest := sha256.Sum256([]byte(token))
	csrfDigest := sha256.Sum256([]byte(csrf))
	if !bytes.Equal(store.tokenHash, tokenDigest[:]) {
		t.Fatal("the stored token is not the token's SHA-256")
	}
	if !bytes.Equal(store.csrfHash, csrfDigest[:]) {
		t.Fatal("the stored CSRF value is not its SHA-256")
	}
	if bytes.Contains(store.tokenHash, []byte(token)) {
		t.Fatal("the raw token reached the store")
	}
	if expires != store.expiresAt {
		t.Fatalf("returned expiry %d, stored %d", expires, store.expiresAt)
	}
}

// A fresh token must be minted per login; a fixed one would let an old cookie live
// forever.
func TestCreateMintsDistinctTokens(t *testing.T) {
	service := newService(&fakeStore{})
	seen := make(map[string]struct{}, 20)
	for i := 0; i < 20; i++ {
		token, csrf, _, err := service.Create(context.Background(), apiKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[token]; dup {
			t.Fatalf("token %q was issued twice", token)
		}
		seen[token] = struct{}{}
		if _, dup := seen[csrf]; dup {
			t.Fatalf("csrf %q collided with an earlier value", csrf)
		}
		seen[csrf] = struct{}{}
	}
}

// The idle window may extend the session repeatedly, but never past the absolute
// deadline: a session that keeps being used must still expire.
func TestCreateClampsIdleExpiryToTheAbsoluteDeadline(t *testing.T) {
	store := &fakeStore{}
	// Idle TTL longer than the max TTL is the case the clamp exists for.
	service := New(store, apiKey, 24*time.Hour, time.Hour)
	before := time.Now()

	_, _, expires, err := service.Create(context.Background(), apiKey)
	if err != nil {
		t.Fatal(err)
	}
	if expires != store.absoluteAt {
		t.Fatalf("expiry %d was not clamped to the absolute deadline %d", expires, store.absoluteAt)
	}
	if limit := before.Add(2 * time.Hour).UnixMilli(); expires > limit {
		t.Fatalf("expiry %d exceeds the one-hour max TTL", expires)
	}
}

func TestCreateKeepsIdleExpiryWhenItIsSooner(t *testing.T) {
	store := &fakeStore{}
	service := New(store, apiKey, time.Minute, 12*time.Hour)
	_, _, expires, err := service.Create(context.Background(), apiKey)
	if err != nil {
		t.Fatal(err)
	}
	if expires >= store.absoluteAt {
		t.Fatalf("idle expiry %d should be before the absolute deadline %d", expires, store.absoluteAt)
	}
}

func TestCreatePropagatesStoreError(t *testing.T) {
	service := newService(&fakeStore{createErr: errors.New("busy")})
	if _, _, _, err := service.Create(context.Background(), apiKey); err == nil {
		t.Fatal("expected the store error")
	}
}

func TestValidateRequiresMatchingCSRFOnWrites(t *testing.T) {
	csrf := "csrf-value"
	digest := sha256.Sum256([]byte(csrf))
	store := &fakeStore{validateCSRF: digest[:], validateOK: true}
	service := newService(store)

	valid, err := service.Validate(context.Background(), "token", csrf, true)
	if err != nil || !valid {
		t.Fatalf("Validate = %v, %v", valid, err)
	}
	// The lookup is by hash, never by the raw token.
	tokenDigest := sha256.Sum256([]byte("token"))
	if !bytes.Equal(store.validateHash, tokenDigest[:]) {
		t.Fatal("looked the session up by something other than the token hash")
	}

	valid, err = service.Validate(context.Background(), "token", "wrong-csrf", true)
	if err != nil || valid {
		t.Fatalf("a mismatched CSRF token was accepted: %v, %v", valid, err)
	}
	valid, err = service.Validate(context.Background(), "token", "", true)
	if err != nil || valid {
		t.Fatalf("an absent CSRF token was accepted on a write: %v, %v", valid, err)
	}
}

// Reads carry no CSRF token, so the check must be skipped rather than failed.
func TestValidateSkipsCSRFOnReads(t *testing.T) {
	digest := sha256.Sum256([]byte("real-csrf"))
	store := &fakeStore{validateCSRF: digest[:], validateOK: true}
	service := newService(store)
	valid, err := service.Validate(context.Background(), "token", "", false)
	if err != nil || !valid {
		t.Fatalf("Validate = %v, %v", valid, err)
	}
}

func TestValidateRejectsAnExpiredOrUnknownSession(t *testing.T) {
	store := &fakeStore{validateOK: false}
	service := newService(store)
	valid, err := service.Validate(context.Background(), "token", "csrf", false)
	if err != nil || valid {
		t.Fatalf("Validate = %v, %v", valid, err)
	}
	// Expiry is decided by the store against the timestamp handed to it.
	if store.validateNow == 0 {
		t.Fatal("no timestamp was passed to the store")
	}
}

func TestValidatePropagatesStoreError(t *testing.T) {
	want := errors.New("read failed")
	service := newService(&fakeStore{validateErr: want})
	valid, err := service.Validate(context.Background(), "token", "", false)
	if !errors.Is(err, want) || valid {
		t.Fatalf("Validate = %v, %v", valid, err)
	}
}

func TestDeleteHashesTheToken(t *testing.T) {
	store := &fakeStore{}
	service := newService(store)
	if err := service.Delete(context.Background(), "token"); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("token"))
	if !bytes.Equal(store.deletedHash, digest[:]) {
		t.Fatal("deleted by something other than the token hash")
	}

	failing := newService(&fakeStore{deleteErr: errors.New("busy")})
	if err := failing.Delete(context.Background(), "token"); err == nil {
		t.Fatal("expected the store error")
	}
}

func TestRandomIsURLSafeAndSized(t *testing.T) {
	value, err := random(32)
	if err != nil {
		t.Fatal(err)
	}
	// RawURLEncoding of 32 bytes is 43 characters with no padding, so the value is
	// safe to put in a Set-Cookie header verbatim.
	if len(value) != 43 {
		t.Fatalf("len = %d, want 43", len(value))
	}
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			t.Fatalf("value %q contains %q, which is not cookie safe", value, c)
		}
	}
}
