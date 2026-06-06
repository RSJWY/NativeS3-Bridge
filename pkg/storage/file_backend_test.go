package storage

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileBackendPutHeadGetRangeDelete(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	body := "hello native s3"
	info, err := backend.PutObject("test-bucket", "dir/a.bin", stringsReader(body), "application/x-test")
	if err != nil {
		t.Fatalf("put object: %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", info.Size, len(body))
	}
	if info.ETag != md5Hex(body) {
		t.Fatalf("etag = %q, want %q", info.ETag, md5Hex(body))
	}

	onDisk := filepath.Join(backend.root, "test-bucket", "dir", "a.bin")
	data, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("read native file: %v", err)
	}
	if string(data) != body {
		t.Fatalf("native bytes = %q, want %q", string(data), body)
	}

	head, err := backend.HeadObject("test-bucket", "dir/a.bin")
	if err != nil {
		t.Fatalf("head object: %v", err)
	}
	if head.ContentType != "application/x-test" {
		t.Fatalf("content type = %q, want upload header", head.ContentType)
	}
	if head.LastModified.IsZero() || head.LastModified.Location() != time.UTC {
		t.Fatalf("last modified should be non-zero UTC, got %v", head.LastModified)
	}

	rc, gotInfo, err := backend.GetObject("test-bucket", "dir/a.bin", &Range{Start: 6, End: 11})
	if err != nil {
		t.Fatalf("range get: %v", err)
	}
	defer rc.Close()
	partial, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if string(partial) != "native" {
		t.Fatalf("range bytes = %q, want native", string(partial))
	}
	if gotInfo.Size != int64(len(body)) {
		t.Fatalf("range info size = %d, want full object size", gotInfo.Size)
	}

	if err := backend.DeleteObject("test-bucket", "dir/a.bin"); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	if _, err := os.Stat(onDisk); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted native file still exists or unexpected stat error: %v", err)
	}
	if _, err := backend.HeadObject("test-bucket", "dir/a.bin"); !errors.Is(err, ErrNoSuchKey) {
		t.Fatalf("head after delete error = %v, want ErrNoSuchKey", err)
	}
	if _, err := backend.HeadObject("missing-bucket", "dir/a.bin"); !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("head missing bucket error = %v, want ErrNoSuchBucket", err)
	}
	if err := backend.DeleteObject("missing-bucket", "dir/a.bin"); !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("delete missing bucket error = %v, want ErrNoSuchBucket", err)
	}
}

func TestFileBackendListObjectsDelimiterAndPagination(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	for _, key := range []string{"dir/a.txt", "dir/b.txt", "dir/sub/c.txt", "other.txt"} {
		if _, err := backend.PutObject("test-bucket", key, stringsReader(key), ""); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := os.WriteFile(filepath.Join(backend.root, "test-bucket", "dir", "hidden.s3meta"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write sidecar fixture: %v", err)
	}

	listed, err := backend.ListObjects("test-bucket", "dir/", "/", "", 10)
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	if gotKeys(listed.Objects) != "dir/a.txt,dir/b.txt" {
		t.Fatalf("objects = %q, want dir/a.txt,dir/b.txt", gotKeys(listed.Objects))
	}
	if gotPrefixes(listed.CommonPrefixes) != "dir/sub/" {
		t.Fatalf("common prefixes = %q, want dir/sub/", gotPrefixes(listed.CommonPrefixes))
	}

	first, err := backend.ListObjects("test-bucket", "dir/", "", "", 1)
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if !first.IsTruncated || first.NextToken != "dir/a.txt" || gotKeys(first.Objects) != "dir/a.txt" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	second, err := backend.ListObjects("test-bucket", "dir/", "", first.NextToken, 10)
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if second.IsTruncated || gotKeys(second.Objects) != "dir/b.txt,dir/sub/c.txt" {
		t.Fatalf("unexpected second page: %+v", second)
	}

	zero, err := backend.ListObjects("test-bucket", "dir/", "", "", 0)
	if err != nil {
		t.Fatalf("list zero max keys: %v", err)
	}
	if len(zero.Objects) != 0 || len(zero.CommonPrefixes) != 0 || !zero.IsTruncated {
		t.Fatalf("max-keys=0 should return no entries and report truncation, got %+v", zero)
	}
}

func TestFileBackendListBucketsFiltersHiddenAndInvalidDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"test-bucket", ".multipart", "Bad_Bucket"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "natives3.db"), []byte("db"), 0o644); err != nil {
		t.Fatalf("write db fixture: %v", err)
	}

	backend, err := NewFileBackend(root)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	buckets, err := backend.ListBuckets()
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Name != "test-bucket" {
		t.Fatalf("buckets = %+v, want only test-bucket", buckets)
	}
}

func TestFileBackendCopyObjectPreservesBytesMetadataAndTags(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObjectWithMetadata("test-bucket", "dir/source.txt", stringsReader("copy me"), "text/plain", map[string]string{"author": "alice"}); err != nil {
		t.Fatalf("put source: %v", err)
	}
	if err := backend.PutObjectTags("test-bucket", "dir/source.txt", map[string]string{"env": "test"}); err != nil {
		t.Fatalf("tag source: %v", err)
	}

	info, err := backend.CopyObject("test-bucket", "dir/source.txt", "test-bucket", "dir/copy.txt")
	if err != nil {
		t.Fatalf("copy object: %v", err)
	}
	if info.Size != int64(len("copy me")) || info.ETag != md5Hex("copy me") {
		t.Fatalf("copy info = %+v", info)
	}
	data, err := os.ReadFile(filepath.Join(backend.root, "test-bucket", "dir", "copy.txt"))
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(data) != "copy me" {
		t.Fatalf("copy bytes = %q", string(data))
	}
	head, err := backend.HeadObject("test-bucket", "dir/copy.txt")
	if err != nil {
		t.Fatalf("head copy: %v", err)
	}
	if head.ContentType != "text/plain" || head.Metadata["author"] != "alice" {
		t.Fatalf("copy metadata = content-type %q metadata %+v", head.ContentType, head.Metadata)
	}
	tags, err := backend.GetObjectTags("test-bucket", "dir/copy.txt")
	if err != nil {
		t.Fatalf("get copy tags: %v", err)
	}
	if tags["env"] != "test" {
		t.Fatalf("copy tags = %+v", tags)
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func stringsReader(s string) io.Reader {
	return strings.NewReader(s)
}

func gotKeys(objects []ObjectInfo) string {
	keys := make([]string, 0, len(objects))
	for _, obj := range objects {
		keys = append(keys, obj.Key)
	}
	return strings.Join(keys, ",")
}

func gotPrefixes(prefixes []string) string {
	return strings.Join(prefixes, ",")
}
