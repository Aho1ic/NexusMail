package cryptobox

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestRoundTripAndTamperDetection(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("oauth refresh token")
	envelope, err := box.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope, plain) {
		t.Fatal("ciphertext contains plaintext")
	}
	opened, err := box.Open(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plain) {
		t.Fatalf("round trip = %q, want %q", opened, plain)
	}
	envelope[len(envelope)-1] ^= 1
	if _, err := box.Open(envelope); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestRejectsWrongKeyLength(t *testing.T) {
	for _, size := range []int{0, 1, 15, 16, 24, 31, 33, 64} {
		if _, err := New(make([]byte, size)); err == nil {
			t.Errorf("New accepted a %d-byte key", size)
		}
	}
}

// TestOpenRejectsMalformedEnvelopes covers the envelope validation, which is what
// stands between a corrupted or attacker-supplied database column and the AEAD. Every
// case here has to fail as an error: a panic would be reachable from stored data,
// since these bytes are read straight back out of the accounts table.
func TestOpenRejectsMalformedEnvelopes(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	nonceSize := box.aead.NonceSize()

	shortNonce := append([]byte{}, sealed...)
	binary.BigEndian.PutUint16(shortNonce[1:3], uint16(nonceSize-1))

	longNonce := append([]byte{}, sealed...)
	binary.BigEndian.PutUint16(longNonce[1:3], uint16(nonceSize+1))

	for _, testCase := range []struct {
		name     string
		envelope []byte
	}{
		{"empty", nil},
		{"one byte", []byte{version}},
		{"two bytes, shorter than the header", []byte{version, 0}},
		{"header only, no nonce or ciphertext", []byte{version, 0, byte(nonceSize)}},
		{"unknown version", append([]byte{version + 1}, sealed[1:]...)},
		{"zero version", append([]byte{0}, sealed[1:]...)},
		{"declared nonce shorter than the AEAD's", shortNonce},
		{"declared nonce longer than the AEAD's", longNonce},
		// Truncation is the realistic corruption: a write cut short leaves a
		// well-formed header describing bytes that are no longer there.
		{"truncated inside the nonce", sealed[:3+nonceSize-1]},
		{"truncated to exactly the nonce, no tag", sealed[:3+nonceSize]},
		{"truncated inside the tag", sealed[:len(sealed)-1]},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plaintext, err := box.Open(testCase.envelope)
			if err == nil {
				t.Fatalf("Open accepted a malformed envelope and returned %q", plaintext)
			}
			if plaintext != nil {
				t.Errorf("Open returned %q alongside its error, want nil", plaintext)
			}
		})
	}
}

// TestSealReportsANonceFailure covers the one branch that a caller cannot reach: the
// system entropy source failing. It matters because the alternative to reporting it is
// sealing with a zero nonce, and a repeated nonce in GCM leaks the plaintext.
func TestSealReportsANonceFailure(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	original := randReader
	t.Cleanup(func() { randReader = original })
	randReader = errorReader{}

	sealed, err := box.Seal([]byte("secret"))
	if err == nil {
		t.Fatal("Seal succeeded without entropy")
	}
	if sealed != nil {
		t.Errorf("Seal returned %d bytes alongside its error, want nil", len(sealed))
	}
	if !strings.Contains(err.Error(), "generate nonce") {
		t.Errorf("error is %q, want it to name the nonce", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
