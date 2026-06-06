package handlers

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/hooks"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

func TestCommitUsageSkipsAnonymousIdentity(t *testing.T) {
	called := false
	h := &ObjectHandler{commit: func(credID uint, deltaBytes int64, op quota.Op) error {
		called = true
		return nil
	}}
	req := httptest.NewRequest("GET", "/bucket/key.txt", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.AnonymousIdentity()))

	h.commitUsage(req, 12, quota.OpGet)

	if called {
		t.Fatal("commit was called for anonymous identity")
	}
}

func TestDeleteObjectsDeletesExistingAndReportsMissing(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "a.txt", strings.NewReader("hello"), "text/plain"); err != nil {
		t.Fatalf("put object: %v", err)
	}
	var commits []int64
	emitter := &recordingEmitter{}
	h := NewObjectHandlerWithHooks(backend, func(_ uint, deltaBytes int64, op quota.Op) error {
		if op != quota.OpDelete {
			t.Fatalf("op = %v, want delete", op)
		}
		commits = append(commits, deltaBytes)
		return nil
	}, emitter)
	req := httptest.NewRequest(http.MethodPost, "/test-bucket?delete", strings.NewReader(`<Delete><Object><Key>a.txt</Key></Object><Object><Key>missing.txt</Key></Object></Delete>`))
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 7, AccessKey: "ak"}))
	rr := httptest.NewRecorder()

	h.DeleteObjects(rr, req, "test-bucket")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := backend.HeadObject("test-bucket", "a.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("head deleted object err = %v, want ErrNoSuchKey", err)
	}
	if len(commits) != 1 || commits[0] != -5 {
		t.Fatalf("commits = %+v, want [-5]", commits)
	}
	if len(emitter.events) != 1 || emitter.events[0].Type != hooks.ObjectDeleted || emitter.events[0].Key != "a.txt" {
		t.Fatalf("events = %+v", emitter.events)
	}
	var resp deleteObjectsResult
	if err := xml.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Deleted) != 2 || resp.Deleted[0].Key != "a.txt" || resp.Deleted[1].Key != "missing.txt" {
		t.Fatalf("deleted response = %+v", resp.Deleted)
	}
}

func TestCopyObjectChecksQuotaBeforeCopy(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "source.txt", strings.NewReader("too large"), "text/plain"); err != nil {
		t.Fatalf("put source: %v", err)
	}
	committed := false
	h := NewObjectHandler(backend, func(_ uint, _ int64, _ quota.Op) error {
		committed = true
		return nil
	})
	req := httptest.NewRequest(http.MethodPut, "/test-bucket/copy.txt", bytes.NewReader(nil))
	req.Header.Set("x-amz-copy-source", "test-bucket/source.txt")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 7, AccessKey: "ak", QuotaBytes: 4, UsedBytes: 0}))
	rr := httptest.NewRecorder()

	h.Copy(rr, req, "test-bucket", "copy.txt")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rr.Code, rr.Body.String())
	}
	if committed {
		t.Fatal("usage commit should not run on quota failure")
	}
	if _, err := backend.HeadObject("test-bucket", "copy.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("destination err = %v, want ErrNoSuchKey", err)
	}
}

func TestCopyObjectCopiesDataCommitsUsageAndEmitsEvent(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObjectWithMetadata("test-bucket", "source.txt", strings.NewReader("copy me"), "text/plain", map[string]string{"author": "alice"}); err != nil {
		t.Fatalf("put source: %v", err)
	}
	if err := backend.PutObjectTags("test-bucket", "source.txt", map[string]string{"env": "test"}); err != nil {
		t.Fatalf("tag source: %v", err)
	}
	var commits []int64
	emitter := &recordingEmitter{}
	h := NewObjectHandlerWithHooks(backend, func(_ uint, deltaBytes int64, op quota.Op) error {
		if op != quota.OpPut {
			t.Fatalf("op = %v, want put", op)
		}
		commits = append(commits, deltaBytes)
		return nil
	}, emitter)
	req := httptest.NewRequest(http.MethodPut, "/test-bucket/copy.txt", bytes.NewReader(nil))
	req.Header.Set("x-amz-copy-source", "test-bucket/source.txt")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 7, AccessKey: "ak", QuotaBytes: 100}))
	rr := httptest.NewRecorder()

	h.Copy(rr, req, "test-bucket", "copy.txt")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp copyObjectResult
	if err := xml.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.ETag != `"`+md5Hex("copy me")+`"` || resp.LastModified == "" {
		t.Fatalf("copy response = %+v", resp)
	}
	if len(commits) != 1 || commits[0] != int64(len("copy me")) {
		t.Fatalf("commits = %+v, want copied size", commits)
	}
	if len(emitter.events) != 1 || emitter.events[0].Type != hooks.ObjectCreated || emitter.events[0].Key != "copy.txt" || emitter.events[0].Size != int64(len("copy me")) {
		t.Fatalf("events = %+v", emitter.events)
	}
	rc, info, err := backend.GetObject("test-bucket", "copy.txt", nil)
	if err != nil {
		t.Fatalf("get copy: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(data) != "copy me" || info.ContentType != "text/plain" || info.Metadata["author"] != "alice" {
		t.Fatalf("copy data/info = %q %+v", string(data), info)
	}
	tags, err := backend.GetObjectTags("test-bucket", "copy.txt")
	if err != nil {
		t.Fatalf("get copy tags: %v", err)
	}
	if tags["env"] != "test" {
		t.Fatalf("copy tags = %+v", tags)
	}
}

func TestPutObjectWithContentMD5Success(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	body := "md5 verified"
	md5Sum := md5.Sum([]byte(body))
	var commits []int64
	emitter := &recordingEmitter{}
	h := NewObjectHandlerWithHooks(backend, func(_ uint, deltaBytes int64, op quota.Op) error {
		if op != quota.OpPut {
			t.Fatalf("op = %v, want put", op)
		}
		commits = append(commits, deltaBytes)
		return nil
	}, emitter)
	req := newSignedPutRequest("/test-bucket/md5.txt", body)
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(md5Sum[:]))
	rr := httptest.NewRecorder()

	h.Put(rr, req, "test-bucket", "md5.txt")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("ETag") != `"`+md5Hex(body)+`"` {
		t.Fatalf("etag = %q, want quoted md5", rr.Header().Get("ETag"))
	}
	if len(commits) != 1 || commits[0] != int64(len(body)) {
		t.Fatalf("commits = %+v, want uploaded size", commits)
	}
	if len(emitter.events) != 1 || emitter.events[0].Type != hooks.ObjectCreated || emitter.events[0].Key != "md5.txt" {
		t.Fatalf("events = %+v", emitter.events)
	}
}

func TestPutObjectWithBadContentMD5DoesNotCommitEmitOrCreateObject(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	committed := false
	emitter := &recordingEmitter{}
	h := NewObjectHandlerWithHooks(backend, func(_ uint, _ int64, _ quota.Op) error {
		committed = true
		return nil
	}, emitter)
	req := newSignedPutRequest("/test-bucket/bad-md5.txt", "corrupted")
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 16)))
	rr := httptest.NewRecorder()

	h.Put(rr, req, "test-bucket", "bad-md5.txt")

	if rr.Code != http.StatusBadRequest || errorCodeFromResponse(t, rr) != "BadDigest" {
		t.Fatalf("status/code = %d/%s, body = %s", rr.Code, errorCodeFromResponse(t, rr), rr.Body.String())
	}
	if committed {
		t.Fatal("usage commit should not run on bad digest")
	}
	if len(emitter.events) != 0 {
		t.Fatalf("events = %+v, want none", emitter.events)
	}
	if _, err := backend.HeadObject("test-bucket", "bad-md5.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("head bad object err = %v, want ErrNoSuchKey", err)
	}
}

func TestPutObjectWithBadContentMD5PreservesOverwrite(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := backend.PutObject("test-bucket", "keep.txt", strings.NewReader("original"), "text/plain"); err != nil {
		t.Fatalf("put original: %v", err)
	}
	h := NewObjectHandler(backend, nil)
	req := newSignedPutRequest("/test-bucket/keep.txt", "replacement")
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 16)))
	rr := httptest.NewRecorder()

	h.Put(rr, req, "test-bucket", "keep.txt")

	if rr.Code != http.StatusBadRequest || errorCodeFromResponse(t, rr) != "BadDigest" {
		t.Fatalf("status/code = %d/%s, body = %s", rr.Code, errorCodeFromResponse(t, rr), rr.Body.String())
	}
	rc, info, err := backend.GetObject("test-bucket", "keep.txt", nil)
	if err != nil {
		t.Fatalf("get preserved object: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read preserved object: %v", err)
	}
	if string(data) != "original" || info.ETag != md5Hex("original") {
		t.Fatalf("preserved object = %q %+v", string(data), info)
	}
}

func TestPutObjectWithSHA256Digest(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	h := NewObjectHandler(backend, nil)
	body := "sha verified"
	shaSum := sha256.Sum256([]byte(body))
	req := newSignedPutRequest("/test-bucket/sha.txt", body)
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(shaSum[:]))
	rr := httptest.NewRecorder()

	h.Put(rr, req, "test-bucket", "sha.txt")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	badReq := newSignedPutRequest("/test-bucket/bad-sha.txt", body)
	badReq.Header.Set("x-amz-content-sha256", strings.Repeat("0", 64))
	badRR := httptest.NewRecorder()

	h.Put(badRR, badReq, "test-bucket", "bad-sha.txt")

	if badRR.Code != http.StatusBadRequest || errorCodeFromResponse(t, badRR) != "BadDigest" {
		t.Fatalf("status/code = %d/%s, body = %s", badRR.Code, errorCodeFromResponse(t, badRR), badRR.Body.String())
	}
	if _, err := backend.HeadObject("test-bucket", "bad-sha.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("head bad sha object err = %v, want ErrNoSuchKey", err)
	}
}

func TestPutObjectIgnoresUnsignedPayloadSHA256Sentinel(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	h := NewObjectHandler(backend, nil)
	req := newSignedPutRequest("/test-bucket/unsigned.txt", "unsigned payload body")
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	rr := httptest.NewRecorder()

	h.Put(rr, req, "test-bucket", "unsigned.txt")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestPutObjectRejectsMalformedDigestHeaders(t *testing.T) {
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	h := NewObjectHandler(backend, nil)
	req := newSignedPutRequest("/test-bucket/bad-header.txt", "body")
	req.Header.Set("Content-MD5", "not base64")
	rr := httptest.NewRecorder()

	h.Put(rr, req, "test-bucket", "bad-header.txt")

	if rr.Code != http.StatusBadRequest || errorCodeFromResponse(t, rr) != "InvalidDigest" {
		t.Fatalf("status/code = %d/%s, body = %s", rr.Code, errorCodeFromResponse(t, rr), rr.Body.String())
	}
	shortMD5Req := newSignedPutRequest("/test-bucket/short-md5-header.txt", "body")
	shortMD5Req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString([]byte{1, 2, 3}))
	shortMD5RR := httptest.NewRecorder()

	h.Put(shortMD5RR, shortMD5Req, "test-bucket", "short-md5-header.txt")

	if shortMD5RR.Code != http.StatusBadRequest || errorCodeFromResponse(t, shortMD5RR) != "InvalidDigest" {
		t.Fatalf("status/code = %d/%s, body = %s", shortMD5RR.Code, errorCodeFromResponse(t, shortMD5RR), shortMD5RR.Body.String())
	}
	shaReq := newSignedPutRequest("/test-bucket/bad-sha-header.txt", "body")
	shaReq.Header.Set("x-amz-content-sha256", strings.Repeat("z", 64))
	shaRR := httptest.NewRecorder()

	h.Put(shaRR, shaReq, "test-bucket", "bad-sha-header.txt")

	if shaRR.Code != http.StatusBadRequest || errorCodeFromResponse(t, shaRR) != "InvalidDigest" {
		t.Fatalf("status/code = %d/%s, body = %s", shaRR.Code, errorCodeFromResponse(t, shaRR), shaRR.Body.String())
	}
}

func TestParseCopySource(t *testing.T) {
	bucket, key, err := parseCopySource("/test-bucket/dir%20name/source%3Ffile.txt?versionId=ignored")
	if err != nil {
		t.Fatalf("parse copy source: %v", err)
	}
	if bucket != "test-bucket" || key != "dir name/source?file.txt" {
		t.Fatalf("bucket=%q key=%q", bucket, key)
	}
}

type recordingEmitter struct {
	events []hooks.Event
}

func (r *recordingEmitter) Emit(event hooks.Event) {
	r.events = append(r.events, event)
}

func newSignedPutRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	return req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 7, AccessKey: "ak", QuotaBytes: 1000}))
}

func errorCodeFromResponse(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var s3Err S3Error
	if err := xml.Unmarshal(rr.Body.Bytes(), &s3Err); err != nil {
		t.Fatalf("unmarshal s3 error: %v", err)
	}
	return s3Err.Code
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
