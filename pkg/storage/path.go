package storage

import (
	"errors"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	ErrInvalidBucketName = errors.New("invalid bucket name")
	ErrInvalidPath       = errors.New("invalid object path")

	bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

func ValidateBucketName(bucket string) error {
	if !bucketNamePattern.MatchString(bucket) {
		return ErrInvalidBucketName
	}
	return nil
}

func ResolveBucketPath(root, bucket string) (string, error) {
	if err := ValidateBucketName(bucket); err != nil {
		return "", err
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(rootAbs, bucket), nil
}

func ResolveObjectPath(root, bucket, key string) (string, error) {
	bucketDir, err := ResolveBucketPath(root, bucket)
	if err != nil {
		return "", err
	}
	if key == "" || hasParentSegment(key) {
		return "", ErrInvalidPath
	}

	cleanKey := strings.TrimPrefix(path.Clean("/"+key), "/")
	if cleanKey == "." || cleanKey == "" || hasParentSegment(cleanKey) {
		return "", ErrInvalidPath
	}

	target := filepath.Join(bucketDir, filepath.FromSlash(cleanKey))
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	bucketAbs, err := filepath.Abs(bucketDir)
	if err != nil {
		return "", err
	}

	sep := string(filepath.Separator)
	if targetAbs == bucketAbs || !strings.HasPrefix(targetAbs, bucketAbs+sep) {
		return "", ErrInvalidPath
	}
	return targetAbs, nil
}

func hasParentSegment(key string) bool {
	for _, part := range strings.Split(filepath.ToSlash(key), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
