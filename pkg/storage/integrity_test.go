package storage

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	info, err := backend.PutObjectWithOptions("test-bucket", "ok.txt", stringsReader(body), PutOptions{ExpectedMD5: goodHex})
	if err != nil {
		t.Fatalf("put with matching md5: %v", err)
	}
	if info.ETag != goodHex {
		t.Fatalf("etag = %q, want %q", info.ETag, goodHex)
	}

	// Base64 input path (what the handler decodes) should also work.
	if _, err := backend.PutObjectWithOptions("test-bucket", "ok2.txt", stringsReader(body), PutOptions{ExpectedMD5: hexFromBase64(t, base64.StdEncoding.EncodeToString(sum[:]))}); err != nil {
		t.Fatalf("put with base64-derived md5: %v", err)
	}
}

func TestPutObjectWithOptionsRejectsBadDigestAndLeavesNoObject(t *testing.T) {
	root := t.TempDir()
	backend, err := NewFileBackend(root)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	_, err = backend.PutObjectWithOptions("test-bucket", "dir/bad.txt", stringsReader("actual content"), PutOptions{ExpectedMD5: "00000000000000000000000000000000"})
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
	if err := assertNoTempFiles(filepath.Join(root, "test-bucket")); err != nil {
		t.Fatal(err)
	}
}

func assertNoTempFiles(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.Contains(d.Name(), ".tmp-") {
			return fmt.Errorf("unexpected leftover temp file: %s", path)
		}
		return nil
	})
}

func hexFromBase64(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return hex.EncodeToString(raw)
}
