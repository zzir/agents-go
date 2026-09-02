// Package secrets seals stored credentials — provider keys, OAuth grants, SSH
// passwords, webhook secrets — with one process-level key, so possession of
// the database is not possession of every upstream credential.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// A sealed value is "enc:v2:<kid>:<base64 nonce+ciphertext>"; a stored value
// without the prefix is plaintext from before a key and passes through
// (docs/howto/workbench-deploy.md "Secret handling").
const (
	prefix   = "enc:"
	version  = "v2"
	kidBytes = 4
)

// Box seals and opens with AES-256-GCM. A nil *Box is the no-key mode:
// Seal and Open pass every value through.
type Box struct {
	aead cipher.AEAD
	kid  string
}

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
	sum := sha256.Sum256(key)
	return &Box{aead: aead, kid: hex.EncodeToString(sum[:kidBytes])}, nil
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

// Seal encrypts plain for the named place — "table.column", the additional
// data the ciphertext is bound to, so it opens nowhere else. "" stays "".
// A value that already looks sealed is sealed as the text it is.
func (b *Box) Seal(label, plain string) string {
	if b == nil || plain == "" {
		return plain
	}
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce)
	ct := b.aead.Seal(nonce, nonce, []byte(plain), []byte(label))
	return prefix + version + ":" + b.kid + ":" + base64.RawStdEncoding.EncodeToString(ct)
}

// looksSealed reports whether stored has the sealed envelope shape
// (enc:v<n>:<kid>:<payload>), not merely a leading "enc:" — so a plaintext
// value a user typed that happens to start with "enc:" is left alone instead
// of being treated as ciphertext and failing to open.
func looksSealed(stored string) bool {
	if !strings.HasPrefix(stored, prefix+"v") {
		return false
	}
	parts := strings.SplitN(stored, ":", 4)
	return len(parts) == 4 && strings.HasPrefix(parts[1], "v")
}

// Open decrypts a value sealed for label; an unsealed one passes through. A
// sealed value with no key, or under another key, is an error naming which.
func (b *Box) Open(label, stored string) (string, error) {
	if !looksSealed(stored) {
		return stored, nil
	}
	if b == nil {
		return "", errors.New("secrets: a sealed value but no key configured (AGENTS_SECRET_KEY)")
	}
	parts := strings.SplitN(stored, ":", 4)
	if len(parts) != 4 || parts[1] != version {
		return "", errors.New("secrets: malformed sealed value")
	}
	if parts[2] != b.kid {
		return "", fmt.Errorf("secrets: sealed under key %s, the configured key is %s", parts[2], b.kid)
	}
	ct, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return "", fmt.Errorf("secrets: malformed sealed value: %w", err)
	}
	n := b.aead.NonceSize()
	if len(ct) < n {
		return "", errors.New("secrets: malformed sealed value")
	}
	plain, err := b.aead.Open(nil, ct[:n], ct[n:], []byte(label))
	if err != nil {
		return "", errors.New("secrets: cannot open a sealed value: not sealed for this place")
	}
	return string(plain), nil
}
