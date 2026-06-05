package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveObjectPathRejectsParentSegments(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"../escape.txt", "dir/../../escape.txt", "dir/../escape.txt"} {
		t.Run(key, func(t *testing.T) {
			_, err := ResolveObjectPath(root, "valid-bucket", key)
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("expected ErrInvalidPath, got %v", err)
			}
		})
	}
}

func TestResolveObjectPathRejectsInvalidBucketNames(t *testing.T) {
	root := t.TempDir()
	for _, bucket := range []string{"UPPER", "ab", "bad_bucket", "bad.bucket", "-bad", "bad-"} {
		t.Run(bucket, func(t *testing.T) {
			_, err := ResolveObjectPath(root, bucket, "file.txt")
			if !errors.Is(err, ErrInvalidBucketName) {
				t.Fatalf("expected ErrInvalidBucketName, got %v", err)
			}
		})
	}
}

func TestResolveObjectPathStaysUnderBucket(t *testing.T) {
	root := t.TempDir()
	got, err := ResolveObjectPath(root, "valid-bucket", "dir/file.txt")
	if err != nil {
		t.Fatalf("resolve object path: %v", err)
	}
	if !strings.Contains(got, "valid-bucket") || !strings.HasSuffix(got, "dir/file.txt") {
		t.Fatalf("unexpected resolved path %q", got)
	}
}
