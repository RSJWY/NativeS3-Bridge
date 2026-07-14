package panel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// masterKeyLen is the required length (bytes) of the raw AEAD master key.
const masterKeyLen = 32

// ErrMasterKeyMissing is returned when the panel is asked to encrypt or decrypt
// but no master key has been loaded. This is the fail-closed signal: the panel
// must refuse to start (or refuse the operation) rather than fall back to
// storing secrets in the clear.
var ErrMasterKeyMissing = errors.New("secret-key master key is not loaded")

// SecretCipher provides AEAD encryption/decryption of S3 secret keys using a
// master key that is held outside the database (loaded from an external file or
// KMS via panel config). The ciphertext form is versioned so the algorithm can
// evolve without ambiguity.
type SecretCipher struct {
	aead cipher.AEAD
}

// cipherPrefix tags the stored ciphertext with an algorithm version so future
// schemes remain distinguishable. Format: "v1:" + base64(nonce||ciphertext).
const cipherPrefix = "v1:"

// LoadMasterKeyFromFile reads a master key from path. The file must contain
// exactly masterKeyLen bytes, either raw or base64-encoded (with optional
// surrounding whitespace). A missing file, wrong-length key, or unreadable file
// is a fatal, fail-closed error: the caller must refuse to start.
func LoadMasterKeyFromFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("master key path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key %q: %w", path, err)
	}
	key, err := parseMasterKey(raw)
	if err != nil {
		return nil, fmt.Errorf("master key %q: %w", path, err)
	}
	return key, nil
}

// parseMasterKey accepts either raw masterKeyLen-byte content or a base64
// (std or url, padded or not) encoding of masterKeyLen bytes.
func parseMasterKey(raw []byte) ([]byte, error) {
	if len(raw) == masterKeyLen {
		out := make([]byte, masterKeyLen)
		copy(out, raw)
		return out, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(trimmed); err == nil && len(decoded) == masterKeyLen {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("must be %d raw bytes or a base64 encoding of %d bytes", masterKeyLen, masterKeyLen)
}

// NewSecretCipher builds a SecretCipher from a masterKeyLen-byte key.
func NewSecretCipher(masterKey []byte) (*SecretCipher, error) {
	if len(masterKey) != masterKeyLen {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", masterKeyLen, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	return &SecretCipher{aead: aead}, nil
}

// Encrypt seals plaintext and returns the versioned, base64-wrapped ciphertext
// string suitable for storage in NodeCredential.SecretKeyCipher.
func (c *SecretCipher) Encrypt(plaintext string) (string, error) {
	if c == nil || c.aead == nil {
		return "", ErrMasterKeyMissing
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return cipherPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a ciphertext produced by Encrypt and returns the plaintext.
func (c *SecretCipher) Decrypt(ciphertext string) (string, error) {
	if c == nil || c.aead == nil {
		return "", ErrMasterKeyMissing
	}
	if !strings.HasPrefix(ciphertext, cipherPrefix) {
		return "", fmt.Errorf("unrecognized ciphertext format")
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, cipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(sealed) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, body := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
