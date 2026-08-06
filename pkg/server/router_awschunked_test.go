package server

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

// aws-chunked 端到端测试:覆盖 prd Acceptance Criteria。
//
// 复用 newServerTestDB / headerSignedRequest / stubAuthenticator 等
// 已有辅助函数(pkg/server/router_test.go:1030 起)。

// awsChunkedRequest 构造一个带 aws-chunked 编码的 PUT 请求。
//
// payload 是解码后的真实对象字节;
// declared 为 x-amz-decoded-content-length 的值(传 -1 表示不发该头);
// withSignature 控制是否带 chunk-signature=;
// trailerLines 为 trailer 段(每行形如 "name:value",不含 CRLF)。
func awsChunkedRequest(method, target, payload string, declared int, withSignature bool, trailerLines []string) *http.Request {
	raw := chunkedBody([]string{payload}, trailerLines, withSignature)
	req := httptest.NewRequest(method, target, strings.NewReader(raw))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("Content-Encoding", "aws-chunked")
	if declared >= 0 {
		req.Header.Set("x-amz-decoded-content-length", strconv.Itoa(declared))
	}
	if len(trailerLines) > 0 {
		var algos []string
		for _, line := range trailerLines {
			colon := strings.IndexByte(line, ':')
			if colon < 0 {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(line[:colon]))
			if strings.HasPrefix(name, "x-amz-checksum-") {
				algos = append(algos, name)
			}
		}
		if len(algos) > 0 {
			req.Header.Set("x-amz-trailer", strings.Join(algos, ","))
		}
	}
	return req
}

func awsChunkedRequestSHA(method, target, payload string, declared int, withSignature bool, trailerLines []string, shaValue string) *http.Request {
	req := awsChunkedRequest(method, target, payload, declared, withSignature, trailerLines)
	req.Header.Set("x-amz-content-sha256", shaValue)
	return req
}

// newAwsChunkedTestRouter 构造一个端到端 router,带真实的 FileBackend + bucketStore +
// 固定 identity 的 authenticator,使 quotaMiddleware 路径生效。
func newAwsChunkedTestRouter(t *testing.T, credential *dbpkg.Credential, dataRoot string) (http.Handler, *storage.FileBackend, *storage.BucketStore, *gorm.DB) {
	t.Helper()
	gdb := newServerTestDB(t)
	if credential != nil {
		if err := gdb.Create(credential).Error; err != nil {
			t.Fatalf("create credential: %v", err)
		}
	}
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	var identity *auth.Identity
	if credential != nil {
		identity = &auth.Identity{
			CredentialID: credential.ID,
			AccessKey:    credential.AccessKey,
			QuotaBytes:   credential.QuotaBytes,
			UsedBytes:    credential.UsedBytes,
		}
	}
	authenticator := &stubAuthenticator{id: identity}
	router := NewRouter(backend, nil, bucketStore, authenticator, func(uint, int64, quota.Op) error { return nil }, nil, config.RateLimitConfig{})
	return router, backend, bucketStore, gdb
}

// ---- R1/R2: 解码后落盘字节 == 解码后负载 ----

func TestRouterAwsChunkedPut_WithDeclaredLength_WritesDecodedBytes(t *testing.T) {
	payload := "hello world"
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, len(payload), false, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	info, err := backend.HeadObject("test-bucket", "key.txt")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("object size = %d, want %d", info.Size, len(payload))
	}
	wantMD5 := md5.Sum([]byte(payload))
	if info.ETag != hex.EncodeToString(wantMD5[:]) {
		t.Fatalf("ETag = %q, want md5 of decoded payload %q", info.ETag, hex.EncodeToString(wantMD5[:]))
	}
}

// 故障 B 回归:不带 x-amz-decoded-content-length 也要正确解码。
func TestRouterAwsChunkedPut_WithoutDeclaredLength_WritesDecodedBytes(t *testing.T) {
	payload := "no-declared-length"
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, -1, false, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	info, err := backend.HeadObject("test-bucket", "key.txt")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("object size = %d, want %d (decoded)", info.Size, len(payload))
	}
	wantMD5 := md5.Sum([]byte(payload))
	if info.ETag != hex.EncodeToString(wantMD5[:]) {
		t.Fatalf("ETag = %q, want md5 of decoded payload", info.ETag)
	}
}

// ---- 带 chunk-signature 的多分块 PUT ----

func TestRouterAwsChunkedPut_WithChunkSignature_Decodes(t *testing.T) {
	payload := "signed-multi-chunk-payload"
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, len(payload), true, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	info, err := backend.HeadObject("test-bucket", "key.txt")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", info.Size, len(payload))
	}
	// 分块签名不得进入对象字节:解码后字节必须等于原始 payload。
	rc, _, err := backend.GetObject("test-bucket", "key.txt", nil)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != payload {
		t.Fatalf("body = %q, want %q (chunk-signature leaked?)", body, payload)
	}
}

// ---- trailer 不进入对象字节 ----

func TestRouterAwsChunkedPut_TrailerNotInObjectBytes(t *testing.T) {
	payload := "trailer-test"
	// trailer 中带正确 CRC32,确保不报 BadDigest 同时 trailer 不写入对象字节。
	sum := crc32.New(crc32.IEEETable)
	_, _ = sum.Write([]byte(payload))
	trailer := "x-amz-checksum-crc32:" + base64.StdEncoding.EncodeToString(sum.Sum(nil))
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, len(payload), false, []string{trailer})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	rc, _, err := backend.GetObject("test-bucket", "key.txt", nil)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != payload {
		t.Fatalf("body = %q, want %q (trailer leaked into object?)", body, payload)
	}
}

// ---- trailer CRC32 不匹配 → 400 BadDigest 且无残留 ----

func TestRouterAwsChunkedPut_TrailerChecksumMismatch_BadDigest(t *testing.T) {
	payload := "mismatch-test"
	// 用其它 payload 算出的 CRC32 作为错误的 trailer 值。
	other := crc32.New(crc32.IEEETable)
	_, _ = other.Write([]byte("other"))
	trailer := "x-amz-checksum-crc32:" + base64.StdEncoding.EncodeToString(other.Sum(nil))
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, len(payload), false, []string{trailer})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "BadDigest") {
		t.Fatalf("body = %s, want BadDigest", rr.Body.String())
	}
	// 不得留下对象、sidecar 或 .tmp-* 残留。
	if _, err := backend.HeadObject("test-bucket", "key.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("HeadObject after BadDigest = %v, want ErrNoSuchKey", err)
	}
	assertNoTmpResidue(t, dataRoot, "test-bucket")
}

// ---- 非法分块框架 → 400 且响应码不是 QuotaExceeded(本 issue 的回归守卫)----

func TestRouterAwsChunkedPut_MalformedChunk_NotQuotaExceeded(t *testing.T) {
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 1000000}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	// 非法长度行(不是十六进制)。
	raw := "not-hex\r\n0\r\n\r\n"
	req := httptest.NewRequest(http.MethodPut, "/test-bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "5")
	req.Header.Set("Content-Encoding", "aws-chunked")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	// 回归守卫:响应码必须是 IncompleteBody,绝不能是 QuotaExceeded。
	if strings.Contains(rr.Body.String(), "QuotaExceeded") {
		t.Fatalf("body must NOT contain QuotaExceeded; got: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "IncompleteBody") {
		t.Fatalf("body = %s, want IncompleteBody (regression guard for issue #2)", rr.Body.String())
	}
	// 不得留下对象或 .tmp-* 残留。
	if _, err := backend.HeadObject("test-bucket", "key.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("HeadObject after malformed chunk = %v, want ErrNoSuchKey", err)
	}
	assertNoTmpResidue(t, dataRoot, "test-bucket")
}

// ---- 缺 CRLF → 400 且不是 QuotaExceeded ----

func TestRouterAwsChunkedPut_MissingCRLF_NotQuotaExceeded(t *testing.T) {
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 1000000}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	// 缺少 data 后的 CRLF。
	raw := "5\r\nhello0\r\n\r\n"
	req := httptest.NewRequest(http.MethodPut, "/test-bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "5")
	req.Header.Set("Content-Encoding", "aws-chunked")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "QuotaExceeded") {
		t.Fatalf("body must NOT contain QuotaExceeded; got: %s", rr.Body.String())
	}
	if _, err := backend.HeadObject("test-bucket", "key.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("HeadObject after missing CRLF = %v, want ErrNoSuchKey", err)
	}
}

// ---- 解码后长度与 declared 不一致 → 400 且不是 QuotaExceeded ----

func TestRouterAwsChunkedPut_SizeMismatch_NotQuotaExceeded(t *testing.T) {
	payload := "abc"
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 1000000}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	// declared=10 但实际只有 3 字节。
	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, 10, false, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "QuotaExceeded") {
		t.Fatalf("body must NOT contain QuotaExceeded; got: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "IncompleteBody") {
		t.Fatalf("body = %s, want IncompleteBody", rr.Body.String())
	}
	if _, err := backend.HeadObject("test-bucket", "key.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("HeadObject after size mismatch = %v, want ErrNoSuchKey", err)
	}
}

// ---- 配额确实不足 → 403 QuotaExceeded 仍成立 ----

func TestRouterAwsChunkedPut_QuotaActuallyExceeded_Still403(t *testing.T) {
	payload := "this-payload-exceeds-quota"
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 5}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, len(payload), false, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "QuotaExceeded") {
		t.Fatalf("body = %s, want QuotaExceeded", rr.Body.String())
	}
	if _, err := backend.HeadObject("test-bucket", "key.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("HeadObject after real quota exceeded = %v, want ErrNoSuchKey", err)
	}
}

// ---- used_bytes 增量 == 解码后长度 ----

func TestRouterAwsChunkedPut_RecordsDecodedUsedBytes(t *testing.T) {
	payload := "used-bytes-decoded"
	dataRoot := t.TempDir()
	gdb := newServerTestDB(t)
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	if err := gdb.Create(cred).Error; err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatal(err)
	}
	authenticator := &stubAuthenticator{id: &auth.Identity{CredentialID: cred.ID, AccessKey: cred.AccessKey, QuotaBytes: 100}}
	router := NewRouterWithQuotaManager(backend, nil, bucketStore, authenticator, quota.NewManager(gdb), nil, nil, config.RateLimitConfig{})

	req := awsChunkedRequest(http.MethodPut, "/test-bucket/key.txt", payload, len(payload), false, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var got dbpkg.Credential
	if err := gdb.First(&got, cred.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != int64(len(payload)) {
		t.Fatalf("used_bytes = %d, want %d (decoded length)", got.UsedBytes, len(payload))
	}
	var stat dbpkg.RequestStat
	if err := gdb.Where("credential_id = ?", cred.ID).First(&stat).Error; err != nil {
		t.Fatalf("read stat: %v", err)
	}
	if stat.PutCount != 1 || stat.BytesIn != int64(len(payload)) {
		t.Fatalf("stat = %+v, want put_count=1 bytes_in=%d", stat, len(payload))
	}
}

// ---- 非法 x-amz-decoded-content-length → 400 InvalidArgument ----

func TestRouterAwsChunkedPut_InvalidDeclaredLength_400(t *testing.T) {
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	raw := chunkedBody([]string{"x"}, nil, false)
	req := httptest.NewRequest(http.MethodPut, "/test-bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "not-a-number")
	req.Header.Set("Content-Encoding", "aws-chunked")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "InvalidArgument") {
		t.Fatalf("body = %s, want InvalidArgument", rr.Body.String())
	}
	if _, err := backend.HeadObject("test-bucket", "key.txt"); !errors.Is(err, storage.ErrNoSuchKey) {
		t.Fatalf("HeadObject after invalid declared length = %v, want ErrNoSuchKey", err)
	}
}

// ---- 非 aws-chunked PUT 的字节与 ETag 不变 ----

func TestRouterNonChunkedPut_BytesAndETagUnchanged(t *testing.T) {
	payload := "plain-payload-bytes"
	dataRoot := t.TempDir()
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 100}
	router, backend, _, _ := newAwsChunkedTestRouter(t, cred, dataRoot)

	req := headerSignedRequest(http.MethodPut, "/test-bucket/plain.txt")
	req = requestBody(req, payload)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	info, err := backend.HeadObject("test-bucket", "plain.txt")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", info.Size, len(payload))
	}
	wantMD5 := md5.Sum([]byte(payload))
	if info.ETag != hex.EncodeToString(wantMD5[:]) {
		t.Fatalf("ETag = %q, want %q", info.ETag, hex.EncodeToString(wantMD5[:]))
	}
}

// ---- aws-chunked UploadPart + Complete 全流程字节正确 ----

func TestRouterAwsChunked_UploadPartAndComplete_BytesCorrect(t *testing.T) {
	gdb := newServerTestDB(t)
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 1000}
	if err := gdb.Create(cred).Error; err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	multipartStore, err := storage.NewMultipartStore(dataRoot, filepath.Join(dataRoot, ".multipart"), storage.DefaultMetadataSuffix)
	if err != nil {
		t.Fatal(err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatal(err)
	}
	authenticator := &stubAuthenticator{id: &auth.Identity{CredentialID: cred.ID, AccessKey: cred.AccessKey, QuotaBytes: 1000}}
	// 用 NewRouterWithQuotaManager 使 used_bytes 真正落盘,以便断言 AC#6。
	router := NewRouterWithQuotaManager(backend, multipartStore, bucketStore, authenticator, quota.NewManager(gdb), nil, nil, config.RateLimitConfig{})

	// 1. Create multipart
	createReq := headerSignedRequest(http.MethodPost, "/test-bucket/big.bin?uploads")
	createRR := httptest.NewRecorder()
	router.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", createRR.Code, createRR.Body.String())
	}
	var initiate struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(createRR.Body.Bytes(), &initiate); err != nil {
		t.Fatalf("unmarshal initiate: %v", err)
	}
	if initiate.UploadID == "" {
		t.Fatalf("upload id empty: %s", createRR.Body.String())
	}

	// 2. UploadPart(part 1) 用 aws-chunked 编码
	partPayload := "part-one-data" // 13 bytes
	part1Req := awsChunkedRequest(http.MethodPut, "/test-bucket/big.bin?uploadId="+initiate.UploadID+"&partNumber=1", partPayload, len(partPayload), false, nil)
	part1RR := httptest.NewRecorder()
	router.ServeHTTP(part1RR, part1Req)
	if part1RR.Code != http.StatusOK {
		t.Fatalf("part1 status = %d, want 200; body=%s", part1RR.Code, part1RR.Body.String())
	}
	if part1RR.Header().Get("ETag") == "" {
		t.Fatalf("part1 ETag header missing: %+v", part1RR.Header())
	}

	// 3. Complete
	completeBody := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + part1RR.Header().Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	completeReq := headerSignedRequest(http.MethodPost, "/test-bucket/big.bin?uploadId="+initiate.UploadID)
	completeReq.Body = io.NopCloser(strings.NewReader(completeBody))
	completeReq.ContentLength = int64(len(completeBody))
	completeRR := httptest.NewRecorder()
	router.ServeHTTP(completeRR, completeReq)
	if completeRR.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want 200; body=%s", completeRR.Code, completeRR.Body.String())
	}

	// 4. 校验合并后对象字节
	info, err := backend.HeadObject("test-bucket", "big.bin")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if info.Size != int64(len(partPayload)) {
		t.Fatalf("merged size = %d, want %d", info.Size, len(partPayload))
	}
	rc, _, err := backend.GetObject("test-bucket", "big.bin", nil)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != partPayload {
		t.Fatalf("body = %q, want %q", body, partPayload)
	}

	// 5. used_bytes == 合并后对象大小(AC#6)
	var got dbpkg.Credential
	if err := gdb.First(&got, cred.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != int64(len(partPayload)) {
		t.Fatalf("used_bytes = %d, want %d (merged object size)", got.UsedBytes, len(partPayload))
	}
}

// ---- 配额确实不足仍返回 QuotaExceeded(既有行为保持,强化断言)----

func TestRouterAwsChunkedPut_ExistingQuotaTestsStillPass(t *testing.T) {
	// 这条测试本身就是个 meta 断言:既有 quota 路径不能被 aws-chunked 解码器破坏。
	// 真正的既有测试在 TestRouterQuotaManagerConcurrentPutsCannotExceedQuota 等里,
	// 这里只做一条最简单的 sanity:小配额 + 普通 PUT 仍返回 QuotaExceeded。
	gdb := newServerTestDB(t)
	cred := &dbpkg.Credential{AccessKey: "test", SecretKey: "secret", Status: "enabled", QuotaBytes: 3}
	if err := gdb.Create(cred).Error; err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackend(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	bucketStore := storage.NewBucketStore(gdb, dataRoot, storage.DefaultBucketACLCacheTTL)
	if err := bucketStore.Create("test-bucket"); err != nil {
		t.Fatal(err)
	}
	authenticator := &stubAuthenticator{id: &auth.Identity{CredentialID: cred.ID, AccessKey: cred.AccessKey, QuotaBytes: 3}}
	router := NewRouterWithQuotaManager(backend, nil, bucketStore, authenticator, quota.NewManager(gdb), nil, nil, config.RateLimitConfig{})

	req := headerSignedRequest(http.MethodPut, "/test-bucket/key.txt")
	req = requestBody(req, "toolong")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "QuotaExceeded") {
		t.Fatalf("body = %s, want QuotaExceeded", rr.Body.String())
	}
}

// ---- 辅助 ----

func assertNoTmpResidue(t *testing.T, dataRoot, bucket string) {
	t.Helper()
	if err := filepath.WalkDir(filepath.Join(dataRoot, bucket), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), ".tmp-") {
			return fmt.Errorf("unexpected leftover temp file: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 避免 unused import 警告(在某些构建配置下)。
var _ = sha256.New
var _ = os.ErrNotExist
