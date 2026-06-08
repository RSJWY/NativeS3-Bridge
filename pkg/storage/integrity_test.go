package storage

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPutObjectWithOptionsVerifiesContentMD5(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	body := "integrity-checked payload"
	sum := md5.Sum([]byte(body))
	goodHex := hex.EncodeToString(sum[:])

	info, err := backend.PutObjectWithOptions("test-bucket", "ok.txt", stringsReader(body), PutObjectOptions{ContentMD5: sum[:]})
	if err != nil {
		t.Fatalf("put with matching md5: %v", err)
	}
	if info.ETag != goodHex {
		t.Fatalf("etag = %q, want %q", info.ETag, goodHex)
	}

	// Base64 input path (what the handler decodes) should also work.
	if _, err := backend.PutObjectWithOptions("test-bucket", "ok2.txt", stringsReader(body), PutObjectOptions{ContentMD5: md5FromBase64(t, base64.StdEncoding.EncodeToString(sum[:]))}); err != nil {
		t.Fatalf("put with base64-derived md5: %v", err)
	}
}

func TestPutObjectWithOptionsRejectsBadDigestAndLeavesNoObject(t *testing.T) {
	root := t.TempDir()
	backend, err := NewFileBackend(root)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	_, err = backend.PutObjectWithOptions("test-bucket", "dir/bad.txt", stringsReader("actual content"), PutObjectOptions{ContentMD5: make([]byte, md5.Size)})
	if !errors.Is(err, ErrBadDigest) {
		t.Fatalf("err = %v, want ErrBadDigest", err)
	}

	// No object, sidecar, or leftover tmp file should remain on disk.
	if _, statErr := backend.HeadObject("test-bucket", "dir/bad.txt"); !errors.Is(statErr, ErrNoSuchKey) {
		t.Fatalf("HeadObject after bad digest = %v, want ErrNoSuchKey", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "test-bucket", "dir", "bad.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target object exists or unexpected stat error: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "test-bucket", "dir", "bad.txt"+DefaultMetadataSuffix)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("sidecar exists or unexpected stat error: %v", statErr)
	}
	assertNoTempFiles(t, filepath.Join(root, "test-bucket", "dir", "bad.txt"))
}

func md5FromBase64(t *testing.T, b64 string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return raw
}
