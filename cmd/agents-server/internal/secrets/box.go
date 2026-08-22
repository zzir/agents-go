// Package secrets seals stored credentials — provider keys, OAuth grants, SSH
// passwords, webhook secrets — with one process-level key, so possession of
// the database is not possession of every upstream credential.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// prefix marks a sealed value. A stored value without it is plaintext from
// before a key was configured and is passed through unchanged — enabling
// encryption later never locks existing rows out; they seal on their next
// write.
const prefix = "enc:v1:"

// IsSealed reports whether a stored value carries the sealed prefix.
func IsSealed(stored string) bool { return strings.HasPrefix(stored, prefix) }

// Box seals and opens with AES-256-GCM. A nil *Box is the no-key mode:
// Seal and Open pass every value through.
type Box struct{ aead cipher.AEAD }

// New returns a Box over a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// ParseKey accepts a key as base64 (standard or URL, padded or not) or hex.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("secrets: empty key")
	}
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString, base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString, base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("secrets: key must decode (base64 or hex) to 32 bytes")
}

// FromEnvOrFile resolves the key: env is the encoded key itself, file names
// a file holding it. Neither set returns a nil Box — plaintext mode.
func FromEnvOrFile(env, file string) (*Box, error) {
	raw := os.Getenv(env)
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("secrets: reading key file: %w", err)
		}
		raw = string(b)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	key, err := ParseKey(raw)
	if err != nil {
		return nil, err
	}
	return New(key)
}

// Seal encrypts plain; "" stays "" (an absent secret is not a secret).
func (b *Box) Seal(plain string) string {
	if b == nil || plain == "" || strings.HasPrefix(plain, prefix) {
		return plain
	}
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce)
	ct := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(ct)
}

// Open decrypts a sealed value; an unsealed one passes through. A sealed
// value with no key, or under the wrong key, is an error — silently reading
// ciphertext as a credential would fail somewhere far less clear.
func (b *Box) Open(stored string) (string, error) {
	if !strings.HasPrefix(stored, prefix) {
		return stored, nil
	}
	if b == nil {
		return "", errors.New("secrets: a sealed value but no key configured (AGENTS_SECRET_KEY)")
	}
	ct, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return "", fmt.Errorf("secrets: malformed sealed value: %w", err)
	}
	n := b.aead.NonceSize()
	if len(ct) < n {
		return "", errors.New("secrets: malformed sealed value")
	}
	plain, err := b.aead.Open(nil, ct[:n], ct[n:], nil)
	if err != nil {
		return "", errors.New("secrets: cannot open a sealed value — wrong key?")
	}
	return string(plain), nil
}
