package auth

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

func TestLocalSigV4AuthenticatorVerify(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "TESTACCESS", SecretKey: "TESTSECRET", Status: "enabled", QuotaBytes: 100, UsedBytes: 5}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	authenticator := NewLocalSigV4Authenticator(NewCredentialStore(gdb, time.Second), "us-east-1")
	authenticator.now = func() time.Time { return time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC) }

	req := signedTestRequest(t, "TESTACCESS", "TESTSECRET", authenticator.now())
	id, err := authenticator.Verify(req)
	if err != nil {
		t.Fatalf("verify signed request: %v", err)
	}
	if id.CredentialID != cred.ID || id.AccessKey != cred.AccessKey || id.QuotaBytes != 100 || id.UsedBytes != 5 {
		t.Fatalf("identity = %+v, want credential values", id)
	}

	badSecret := signedTestRequest(t, "TESTACCESS", "WRONG", authenticator.now())
	if _, err := authenticator.Verify(badSecret); ErrorCode(err) != CodeSignatureDoesNotMatch {
		t.Fatalf("wrong secret error = %v, want SignatureDoesNotMatch", err)
	}

	missingAK := signedTestRequest(t, "NOPE", "TESTSECRET", authenticator.now())
	if _, err := authenticator.Verify(missingAK); ErrorCode(err) != CodeInvalidAccessKeyID {
		t.Fatalf("missing access key error = %v, want InvalidAccessKeyId", err)
	}

	if err := gdb.Model(&db.Credential{}).Where("id = ?", cred.ID).Update("status", "disabled").Error; err != nil {
		t.Fatalf("disable credential: %v", err)
	}
	authenticator.store.Invalidate("TESTACCESS")
	disabled := signedTestRequest(t, "TESTACCESS", "TESTSECRET", authenticator.now())
	if _, err := authenticator.Verify(disabled); ErrorCode(err) != CodeAccessDenied {
		t.Fatalf("disabled access key error = %v, want AccessDenied", err)
	}

	old := signedTestRequest(t, "TESTACCESS", "TESTSECRET", authenticator.now().Add(-16*time.Minute))
	if _, err := authenticator.Verify(old); ErrorCode(err) != CodeRequestTimeTooSkewed {
		t.Fatalf("skewed request error = %v, want RequestTimeTooSkewed", err)
	}
}

func TestLocalSigV4AuthenticatorVerifyPresignedURL(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "PRESIGNAK", SecretKey: "PRESIGNSK", Status: "enabled", QuotaBytes: 100, UsedBytes: 5}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	authenticator := NewLocalSigV4Authenticator(NewCredentialStore(gdb, time.Second), "us-east-1")
	issuedAt := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	authenticator.now = func() time.Time { return issuedAt.Add(30 * time.Second) }

	req := presignedTestRequest(t, http.MethodGet, "http://localhost:9000/test-bucket/p.txt", cred.AccessKey, cred.SecretKey, issuedAt, 60*time.Second)
	id, err := authenticator.Verify(req)
	if err != nil {
		t.Fatalf("verify presigned request: %v", err)
	}
	if id.CredentialID != cred.ID || id.AccessKey != cred.AccessKey || id.UsedBytes != 5 {
		t.Fatalf("identity = %+v, want credential values", id)
	}

	authenticator.now = func() time.Time { return issuedAt.Add(61 * time.Second) }
	if _, err := authenticator.Verify(req); ErrorCode(err) != CodeAccessDenied {
		t.Fatalf("expired presign error = %v, want AccessDenied", err)
	}

	tampered := presignedTestRequest(t, http.MethodGet, "http://localhost:9000/test-bucket/p.txt", cred.AccessKey, cred.SecretKey, issuedAt, 60*time.Second)
	tampered.URL.Path = "/test-bucket/other.txt"
	authenticator.now = func() time.Time { return issuedAt.Add(30 * time.Second) }
	if _, err := authenticator.Verify(tampered); ErrorCode(err) != CodeSignatureDoesNotMatch {
		t.Fatalf("tampered presign error = %v, want SignatureDoesNotMatch", err)
	}
}

func TestLocalSigV4AuthenticatorVerifyAWSCliPresignedURL(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "PRESIGNAK", SecretKey: "PRESIGNSK", Status: "enabled"}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	authenticator := NewLocalSigV4Authenticator(NewCredentialStore(gdb, time.Second), "us-east-1")
	authenticator.now = func() time.Time { return time.Date(2026, 6, 5, 22, 27, 30, 0, time.UTC) }

	req, err := http.NewRequest(http.MethodGet, "http://localhost:9000/test-bucket/p.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=PRESIGNAK%2F20260605%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260605T222729Z&X-Amz-Expires=60&X-Amz-SignedHeaders=host&X-Amz-Signature=3cc1d34be1898814cec7eee116d0ad96a4e2e03badd1b2eb318d111732071087", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "localhost:9000"
	if _, err := authenticator.Verify(req); err != nil {
		t.Fatalf("verify aws-cli presigned request: %v", err)
	}
}

func TestCredentialStoreRefreshesUsedBytesOnCacheHit(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "CACHEAK", SecretKey: "CACHESK", Status: "enabled", UsedBytes: 1}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	store := NewCredentialStore(gdb, time.Minute)
	first, err := store.Get("CACHEAK")
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if first.UsedBytes != 1 {
		t.Fatalf("first used bytes = %d, want 1", first.UsedBytes)
	}
	if err := gdb.Model(&db.Credential{}).Where("id = ?", cred.ID).Update("used_bytes", 25).Error; err != nil {
		t.Fatalf("update used bytes: %v", err)
	}
	second, err := store.Get("CACHEAK")
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if second.UsedBytes != 25 {
		t.Fatalf("cache hit used bytes = %d, want refreshed 25", second.UsedBytes)
	}
}

func signedTestRequest(t *testing.T, accessKey, secretKey string, at time.Time) *http.Request {
	t.Helper()
	body := []byte("hello")
	req, err := http.NewRequest(http.MethodPut, "http://localhost:9000/test-bucket/a.txt", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "localhost:9000"
	req.ContentLength = int64(len(body))
	req.Header.Set("x-amz-date", at.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", UnsignedPayload)
	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonical, err := CanonicalRequest(req, signedHeaders, UnsignedPayload)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	date := at.UTC().Format("20060102")
	stringToSign := StringToSign(req.Header.Get("x-amz-date"), date, "us-east-1", "s3", canonical)
	signature := SignString(DeriveSigningKey(secretKey, date, "us-east-1", "s3"), stringToSign)
	req.Header.Set("Authorization", Algorithm+" Credential="+accessKey+"/"+date+"/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+signature)
	return req
}

func presignedTestRequest(t *testing.T, method, rawURL, accessKey, secretKey string, at time.Time, expires time.Duration) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatalf("new presigned request: %v", err)
	}
	req.Host = req.URL.Host
	date := at.UTC().Format("20060102")
	query := req.URL.Query()
	query.Set("X-Amz-Algorithm", Algorithm)
	query.Set("X-Amz-Credential", accessKey+"/"+date+"/us-east-1/s3/aws4_request")
	query.Set("X-Amz-Date", at.UTC().Format("20060102T150405Z"))
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	query.Set("X-Amz-SignedHeaders", "host")
	req.URL.RawQuery = query.Encode()
	canonical, err := CanonicalRequestWithQuery(req, []string{"host"}, UnsignedPayload, req.URL.Query())
	if err != nil {
		t.Fatalf("canonical presign request: %v", err)
	}
	stringToSign := StringToSign(query.Get("X-Amz-Date"), date, "us-east-1", "s3", canonical)
	signature := SignString(DeriveSigningKey(secretKey, date, "us-east-1", "s3"), stringToSign)
	query.Set("X-Amz-Signature", signature)
	req.URL.RawQuery = query.Encode()
	return req
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return gdb
}
