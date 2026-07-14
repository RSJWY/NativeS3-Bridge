package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretCipherRoundTrip(t *testing.T) {
	key := make([]byte, masterKeyLen)
	for i := range key {
		key[i] = byte(i)
	}
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	plaintext := "super-secret-s3-key"
	ct, err := cipher.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(ct, cipherPrefix) {
		t.Fatalf("ciphertext missing version prefix: %q", ct)
	}
	if strings.Contains(ct, plaintext) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := cipher.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("round trip = %q, want %q", got, plaintext)
	}
}

func TestSecretCipherNonceIsRandom(t *testing.T) {
	key := make([]byte, masterKeyLen)
	cipher, _ := NewSecretCipher(key)
	a, _ := cipher.Encrypt("x")
	b, _ := cipher.Encrypt("x")
	if a == b {
		t.Fatal("same plaintext must not produce identical ciphertext (nonce reuse)")
	}
}

func TestSecretCipherWrongKeyFails(t *testing.T) {
	k1 := make([]byte, masterKeyLen)
	k2 := make([]byte, masterKeyLen)
	k2[0] = 0xFF
	c1, _ := NewSecretCipher(k1)
	c2, _ := NewSecretCipher(k2)
	ct, _ := c1.Encrypt("secret")
	if _, err := c2.Decrypt(ct); err == nil {
		t.Fatal("decrypt with wrong key must fail")
	}
}

// TestFailClosedWithoutMasterKey asserts that a nil / unloaded cipher refuses
// to encrypt or decrypt rather than silently handling secrets in the clear.
func TestFailClosedWithoutMasterKey(t *testing.T) {
	var cipher *SecretCipher // never loaded
	if _, err := cipher.Encrypt("x"); err != ErrMasterKeyMissing {
		t.Fatalf("Encrypt without key = %v, want ErrMasterKeyMissing", err)
	}
	if _, err := cipher.Decrypt("v1:whatever"); err != ErrMasterKeyMissing {
		t.Fatalf("Decrypt without key = %v, want ErrMasterKeyMissing", err)
	}
}

func TestNewSecretCipherRejectsWrongKeyLength(t *testing.T) {
	if _, err := NewSecretCipher(make([]byte, 16)); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestLoadMasterKeyFromFile(t *testing.T) {
	dir := t.TempDir()

	// raw 32-byte key
	rawPath := filepath.Join(dir, "raw.key")
	raw := make([]byte, masterKeyLen)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	if err := os.WriteFile(rawPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMasterKeyFromFile(rawPath)
	if err != nil {
		t.Fatalf("load raw: %v", err)
	}
	if len(got) != masterKeyLen {
		t.Fatalf("raw key len = %d", len(got))
	}

	// missing file is fail-closed
	if _, err := LoadMasterKeyFromFile(filepath.Join(dir, "nope.key")); err == nil {
		t.Fatal("missing master key file must be an error")
	}

	// empty path is fail-closed
	if _, err := LoadMasterKeyFromFile("  "); err == nil {
		t.Fatal("empty master key path must be an error")
	}

	// wrong-length content is rejected
	badPath := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(badPath, []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKeyFromFile(badPath); err == nil {
		t.Fatal("wrong-length master key must be rejected")
	}
}
