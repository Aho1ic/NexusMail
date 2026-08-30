package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const version byte = 1

// randReader is the entropy source for nonces. It is a variable only so a test can
// make it fail: the alternative to reporting that failure is sealing with a zero
// nonce, and a repeated nonce in GCM discloses the plaintext, so the branch is worth
// covering. Production code never assigns it.
var randReader io.Reader = rand.Reader

type Box struct{ aead cipher.AEAD }

func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("cryptobox key must be 32 bytes")
	}
	// Neither error below is reachable, and both are kept anyway rather than
	// discarded with _: aes.NewCipher rejects only key lengths other than 16, 24 or
	// 32, which the check above has already excluded, and cipher.NewGCM fails only on
	// a block size other than 16, which AES never has. They are the two statements
	// this package does not cover, and they would start mattering the moment the
	// cipher or the key length changes.
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(randReader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	header := make([]byte, 3)
	header[0] = version
	binary.BigEndian.PutUint16(header[1:], uint16(len(nonce)))
	out := append(header, nonce...)
	out = b.aead.Seal(out, nonce, plaintext, header)
	return out, nil
}

func (b *Box) Open(envelope []byte) ([]byte, error) {
	if len(envelope) < 3 || envelope[0] != version {
		return nil, errors.New("invalid encrypted envelope")
	}
	nonceSize := int(binary.BigEndian.Uint16(envelope[1:3]))
	if nonceSize != b.aead.NonceSize() || len(envelope) < 3+nonceSize+b.aead.Overhead() {
		return nil, errors.New("invalid encrypted envelope length")
	}
	header := envelope[:3]
	nonce := envelope[3 : 3+nonceSize]
	plaintext, err := b.aead.Open(nil, nonce, envelope[3+nonceSize:], header)
	if err != nil {
		return nil, errors.New("decrypt secret: authentication failed")
	}
	return plaintext, nil
}
