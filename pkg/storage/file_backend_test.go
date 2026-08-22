package storage

import (
	"crypto/md5"
	"crypto/sha256"
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

func TestFileBackendPutObjectWithOptionsValidatesDigests(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	body := "verified object"
	md5Sum := md5.Sum([]byte(body))
	sha256Sum := sha256.Sum256([]byte(body))

	info, err := backend.PutObjectWithOptions("test-bucket", "verified.txt", stringsReader(body), PutObjectOptions{
		ContentType:   "text/plain",
		ContentMD5:    md5Sum[:],
		ContentSHA256: hex.EncodeToString(sha256Sum[:]),
	})
	if err != nil {
		t.Fatalf("put object with digests: %v", err)
	}
	if info.ETag != md5Hex(body) {
		t.Fatalf("etag = %q, want %q", info.ETag, md5Hex(body))
	}
	data, err := os.ReadFile(filepath.Join(backend.root, "test-bucket", "verified.txt"))
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if string(data) != body {
		t.Fatalf("native bytes = %q, want %q", string(data), body)
	}
}

func TestFileBackendPutObjectWithOptionsBadDigestCleansTempAndPreservesExistingObject(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "keep.txt", stringsReader("original"), "text/plain"); err != nil {
		t.Fatalf("put original: %v", err)
	}
	badMD5 := bytesOf(16, 0xff)

	_, err = backend.PutObjectWithOptions("test-bucket", "keep.txt", stringsReader("replacement"), PutObjectOptions{ContentMD5: badMD5})
	if !errors.Is(err, ErrBadDigest) {
		t.Fatalf("overwrite err = %v, want ErrBadDigest", err)
	}
	data, err := os.ReadFile(filepath.Join(backend.root, "test-bucket", "keep.txt"))
	if err != nil {
		t.Fatalf("read preserved object: %v", err)
	}
	if string(data) != "original" {
		t.Fatalf("preserved bytes = %q, want original", string(data))
	}
	assertNoTempFiles(t, filepath.Join(backend.root, "test-bucket", "keep.txt"))

	_, err = backend.PutObjectWithOptions("test-bucket", "new.txt", stringsReader("new"), PutObjectOptions{ContentMD5: badMD5})
	if !errors.Is(err, ErrBadDigest) {
		t.Fatalf("new key err = %v, want ErrBadDigest", err)
	}
	if _, err := os.Stat(filepath.Join(backend.root, "test-bucket", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bad digest created final object or unexpected stat error: %v", err)
	}
	assertNoTempFiles(t, filepath.Join(backend.root, "test-bucket", "new.txt"))
}

func TestFileBackendPutObjectWithOptionsInvalidDigest(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}

	_, err = backend.PutObjectWithOptions("test-bucket", "bad-md5.txt", stringsReader("body"), PutObjectOptions{ContentMD5: []byte{1, 2, 3}})
	if !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("short md5 err = %v, want ErrInvalidDigest", err)
	}
	_, err = backend.PutObjectWithOptions("test-bucket", "bad-sha.txt", stringsReader("body"), PutObjectOptions{ContentSHA256: "not-hex"})
	if !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("bad sha err = %v, want ErrInvalidDigest", err)
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

func bytesOf(size int, value byte) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = value
	}
	return out
}

func assertNoTempFiles(t *testing.T, target string) {
	t.Helper()
	matches, err := filepath.Glob(target + ".tmp-*")
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files remain: %v", matches)
	}
}
func TestDirectoryMarkerOperations(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "put marker creates directory and sidecar",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				info, err := backend.PutObject("test-bucket", "dir/", stringsReader(""), "application/x-directory")
				if err != nil {
					t.Fatalf("put marker: %v", err)
				}
				if info.Size != 0 || info.ETag != emptyETag || info.Key != "dir/" {
					t.Fatalf("marker info = %+v", info)
				}
				dirPath := filepath.Join(backend.root, "test-bucket", "dir")
				if st, err := os.Stat(dirPath); err != nil || !st.IsDir() {
					t.Fatalf("marker directory missing or not dir: %v", err)
				}
				sidecar, exists, err := ReadSidecar(dirPath, backend.metadataSuffix)
				if err != nil || !exists || !sidecar.Directory {
					t.Fatalf("marker sidecar = %+v exists=%t err=%v", sidecar, exists, err)
				}
				head, err := backend.HeadObject("test-bucket", "dir/")
				if err != nil {
					t.Fatalf("head marker: %v", err)
				}
				if head.Size != 0 || head.ETag != emptyETag {
					t.Fatalf("head marker = %+v", head)
				}
				rc, gotInfo, err := backend.GetObject("test-bucket", "dir/", nil)
				if err != nil {
					t.Fatalf("get marker: %v", err)
				}
				defer rc.Close()
				data, err := io.ReadAll(rc)
				if err != nil || len(data) != 0 {
					t.Fatalf("marker body = %q err=%v", data, err)
				}
				if gotInfo.Size != 0 {
					t.Fatalf("get marker info size = %d", gotInfo.Size)
				}
				if _, _, err := backend.GetObject("test-bucket", "dir/", &Range{Start: 0, End: 0}); !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("range on marker err = %v, want ErrInvalidRange", err)
				}
			},
		},
		{
			name: "child write after marker succeeds",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir2/", stringsReader(""), ""); err != nil {
					t.Fatalf("put marker: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir2/file.txt", stringsReader("child"), "text/plain"); err != nil {
					t.Fatalf("put child: %v", err)
				}
				head, err := backend.HeadObject("test-bucket", "dir2/file.txt")
				if err != nil {
					t.Fatalf("head child: %v", err)
				}
				if head.Size != int64(len("child")) {
					t.Fatalf("child size = %d", head.Size)
				}
			},
		},
		{
			name: "non-empty marker body rejected",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				_, err = backend.PutObjectWithOptions("test-bucket", "dir3/", stringsReader("x"), PutObjectOptions{ContentType: "text/plain"})
				if !errors.Is(err, ErrInvalidObjectBody) {
					t.Fatalf("err = %v, want ErrInvalidObjectBody", err)
				}
			},
		},
		{
			name: "delete marker keeps children",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir4/", stringsReader(""), ""); err != nil {
					t.Fatalf("put marker: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir4/file.txt", stringsReader("keep"), ""); err != nil {
					t.Fatalf("put child: %v", err)
				}
				if err := backend.DeleteObject("test-bucket", "dir4/"); err != nil {
					t.Fatalf("delete marker: %v", err)
				}
				if _, err := backend.HeadObject("test-bucket", "dir4/"); !errors.Is(err, ErrNoSuchKey) {
					t.Fatalf("head deleted marker err = %v, want ErrNoSuchKey", err)
				}
				if _, err := backend.HeadObject("test-bucket", "dir4/file.txt"); err != nil {
					t.Fatalf("head child after marker delete: %v", err)
				}
				listed, err := backend.ListObjects("test-bucket", "", "", "", 100)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if gotKeys(listed.Objects) != "dir4/file.txt" {
					t.Fatalf("objects = %q, want dir4/file.txt", gotKeys(listed.Objects))
				}
			},
		},
		{
			name: "regular object blocks marker and child",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir5", stringsReader(""), "text/plain"); err != nil {
					t.Fatalf("put regular: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir5/", stringsReader(""), ""); !errors.Is(err, ErrObjectConflict) {
					t.Fatalf("put marker over regular err = %v, want ErrObjectConflict", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir5/file.txt", stringsReader("child"), ""); !errors.Is(err, ErrObjectConflict) {
					t.Fatalf("put child under regular err = %v, want ErrObjectConflict", err)
				}
			},
		},
		{
			name: "marker blocks regular object but allows children",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir6/", stringsReader(""), ""); err != nil {
					t.Fatalf("put marker: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir6", stringsReader("regular"), ""); !errors.Is(err, ErrObjectConflict) {
					t.Fatalf("put regular over marker err = %v, want ErrObjectConflict", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir6/file.txt", stringsReader("child"), ""); err != nil {
					t.Fatalf("put child under marker: %v", err)
				}
			},
		},
		{
			name: "legacy file-shaped marker remains readable and blocks children",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if err := os.MkdirAll(filepath.Join(backend.root, "test-bucket"), 0o755); err != nil {
					t.Fatalf("mkdir bucket: %v", err)
				}
				legacyPath := filepath.Join(backend.root, "test-bucket", "dir7")
				if err := os.WriteFile(legacyPath, []byte{}, 0o644); err != nil {
					t.Fatalf("write legacy marker file: %v", err)
				}
				if err := os.WriteFile(legacyPath+".s3meta", []byte("{}"), 0o644); err != nil {
					t.Fatalf("write legacy sidecar: %v", err)
				}
				head, err := backend.HeadObject("test-bucket", "dir7")
				if err != nil {
					t.Fatalf("head legacy marker: %v", err)
				}
				if head.Size != 0 {
					t.Fatalf("legacy marker size = %d", head.Size)
				}
				listed, err := backend.ListObjects("test-bucket", "", "", "", 100)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if gotKeys(listed.Objects) != "dir7" {
					t.Fatalf("objects = %q, want dir7", gotKeys(listed.Objects))
				}
				if _, err := backend.PutObject("test-bucket", "dir7/file.txt", stringsReader("child"), ""); !errors.Is(err, ErrObjectConflict) {
					t.Fatalf("put child under legacy marker err = %v, want ErrObjectConflict", err)
				}
				if err := backend.DeleteObject("test-bucket", "dir7"); err != nil {
					t.Fatalf("delete legacy marker: %v", err)
				}
				if _, err := backend.HeadObject("test-bucket", "dir7"); !errors.Is(err, ErrNoSuchKey) {
					t.Fatalf("head after delete err = %v", err)
				}
			},
		},
		{
			name: "list delimiter includes marker object and child prefixes",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir8/", stringsReader(""), ""); err != nil {
					t.Fatalf("put marker: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir8/file.txt", stringsReader("a"), ""); err != nil {
					t.Fatalf("put child: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir8/sub/deep.txt", stringsReader("b"), ""); err != nil {
					t.Fatalf("put deep child: %v", err)
				}
				listed, err := backend.ListObjects("test-bucket", "dir8/", "/", "", 100)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if gotKeys(listed.Objects) != "dir8/,dir8/file.txt" {
					t.Fatalf("objects = %q, want dir8/,dir8/file.txt", gotKeys(listed.Objects))
				}
				if gotPrefixes(listed.CommonPrefixes) != "dir8/sub/" {
					t.Fatalf("prefixes = %q, want dir8/sub/", gotPrefixes(listed.CommonPrefixes))
				}
			},
		},
		{
			name: "pagination token is stable across markers",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir9/", stringsReader(""), ""); err != nil {
					t.Fatalf("put marker: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir9/a.txt", stringsReader("a"), ""); err != nil {
					t.Fatalf("put a: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir9/b.txt", stringsReader("b"), ""); err != nil {
					t.Fatalf("put b: %v", err)
				}
				first, err := backend.ListObjects("test-bucket", "", "", "", 1)
				if err != nil {
					t.Fatalf("list first: %v", err)
				}
				if !first.IsTruncated || gotKeys(first.Objects) != "dir9/" || first.NextToken != "dir9/" {
					t.Fatalf("first page = %+v", first)
				}
				second, err := backend.ListObjects("test-bucket", "", "", first.NextToken, 10)
				if err != nil {
					t.Fatalf("list second: %v", err)
				}
				if second.IsTruncated || gotKeys(second.Objects) != "dir9/a.txt,dir9/b.txt" {
					t.Fatalf("second page = %+v", second)
				}
			},
		},
		{
			name: "marker tags round-trip",
			run: func(t *testing.T) {
				backend, err := NewFileBackend(t.TempDir())
				if err != nil {
					t.Fatalf("new backend: %v", err)
				}
				if _, err := backend.PutObject("test-bucket", "dir10/", stringsReader(""), ""); err != nil {
					t.Fatalf("put marker: %v", err)
				}
				if err := backend.PutObjectTags("test-bucket", "dir10/", map[string]string{"env": "prod"}); err != nil {
					t.Fatalf("put tags: %v", err)
				}
				tags, err := backend.GetObjectTags("test-bucket", "dir10/")
				if err != nil {
					t.Fatalf("get tags: %v", err)
				}
				if tags["env"] != "prod" {
					t.Fatalf("tags = %+v", tags)
				}
				head, err := backend.HeadObject("test-bucket", "dir10/")
				if err != nil {
					t.Fatalf("head tagged marker: %v", err)
				}
				if head.Metadata["env"] != "" {
					t.Fatalf("tags leaked into metadata: %+v", head.Metadata)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

func TestDirectoryMarkerFailureLeavesNoDirectory(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	target := filepath.Join(backend.root, "test-bucket", "failed")
	if _, err := backend.PutObject("test-bucket", "failed/", stringsReader("body"), ""); !errors.Is(err, ErrInvalidObjectBody) {
		t.Fatalf("non-empty marker err = %v, want ErrInvalidObjectBody", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed marker left target %q, stat err = %v", target, err)
	}

	badMD5 := make([]byte, md5.Size)
	if _, err := backend.PutObjectWithOptions("test-bucket", "digest-failed/", stringsReader(""), PutObjectOptions{ContentMD5: badMD5}); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("bad digest marker err = %v, want ErrBadDigest", err)
	}
	if _, err := os.Stat(filepath.Join(backend.root, "test-bucket", "digest-failed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("digest-failed marker left directory, stat err = %v", err)
	}
}

func TestLegacyFileMarkerTrailingSlashCompatibility(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	path := filepath.Join(backend.root, "test-bucket", "legacy")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(path, ".s3meta", Sidecar{ETag: emptyETag, Size: 0}); err != nil {
		t.Fatal(err)
	}

	if _, err := backend.HeadObject("test-bucket", "legacy/"); err != nil {
		t.Fatalf("head legacy marker with slash: %v", err)
	}
	rc, _, err := backend.GetObject("test-bucket", "legacy/", nil)
	if err != nil {
		t.Fatalf("get legacy marker with slash: %v", err)
	}
	_ = rc.Close()
	if err := backend.DeleteObject("test-bucket", "legacy/"); err != nil {
		t.Fatalf("delete legacy marker with slash: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy marker file remains, stat err = %v", err)
	}
}

func TestFileBackendCopyDirectoryMarker(t *testing.T) {
	backend, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "source/", stringsReader(""), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PutObject("test-bucket", "source/child.txt", stringsReader("child"), ""); err != nil {
		t.Fatal(err)
	}
	info, err := backend.CopyObject("test-bucket", "source/", "test-bucket", "copied/")
	if err != nil {
		t.Fatalf("copy marker: %v", err)
	}
	if info.Key != "copied/" || info.Size != 0 {
		t.Fatalf("copied marker info = %+v", info)
	}
	if _, err := backend.HeadObject("test-bucket", "copied/"); err != nil {
		t.Fatalf("head copied marker: %v", err)
	}
}
