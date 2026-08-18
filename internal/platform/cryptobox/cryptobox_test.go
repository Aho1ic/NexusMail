package cryptobox

import (
	"bytes"
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
	if _, err := New(make([]byte, 31)); err == nil {
		t.Fatal("expected key length error")
	}
}
