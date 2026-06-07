package webadmin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
)

func TestLoginLockoutReturns429AndSkipsBcryptDelay(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{
		PasswordHash:       mustPasswordHash(t),
		SessionSecret:      "test-session-secret",
		SessionTTLMinutes:  10,
		LoginMaxFailures:   1,
		LoginLockoutWindow: time.Minute,
	})
	auth.failureDelay = 0
	auth.now = func() time.Time { return time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC) }
	auth.limiter.now = auth.now

	first := loginRequestRecorder(auth, `{"password":"wrong"}`, "192.0.2.1:1234")
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("first status = %d, want 401", first.Code)
	}

	auth.failureDelay = 2 * time.Second
	start := time.Now()
	second := loginRequestRecorder(auth, `{"password":"test-password"}`, "192.0.2.1:1234")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("locked login took %v, bcrypt/failure path likely ran", elapsed)
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", second.Header().Get("Retry-After"))
	}
}

func TestLoginLockoutCheckedBeforeBodyDecode(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{
		PasswordHash:       mustPasswordHash(t),
		SessionSecret:      "test-session-secret",
		SessionTTLMinutes:  10,
		LoginMaxFailures:   1,
		LoginLockoutWindow: time.Minute,
	})
	auth.failureDelay = 0

	failed := loginRequestRecorder(auth, `{"password":"wrong"}`, "192.0.2.1:1234")
	if failed.Code != http.StatusUnauthorized {
		t.Fatalf("failed status = %d, want 401", failed.Code)
	}
	locked := loginRequestRecorder(auth, `{not-json`, "192.0.2.1:1234")
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("locked malformed status = %d, want 429", locked.Code)
	}
}

func TestLoginSuccessClearsFailureCount(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{
		PasswordHash:       mustPasswordHash(t),
		SessionSecret:      "test-session-secret",
		SessionTTLMinutes:  10,
		LoginMaxFailures:   2,
		LoginLockoutWindow: time.Minute,
	})
	auth.failureDelay = 0

	failed := loginRequestRecorder(auth, `{"password":"wrong"}`, "192.0.2.1:1234")
	if failed.Code != http.StatusUnauthorized {
		t.Fatalf("failed status = %d, want 401", failed.Code)
	}
	success := loginRequestRecorder(auth, `{"password":"test-password"}`, "192.0.2.1:1234")
	if success.Code != http.StatusOK {
		t.Fatalf("success status = %d, want 200; body=%s", success.Code, success.Body.String())
	}
	failedAgain := loginRequestRecorder(auth, `{"password":"wrong"}`, "192.0.2.1:1234")
	if failedAgain.Code != http.StatusUnauthorized {
		t.Fatalf("failed again status = %d, want 401", failedAgain.Code)
	}
}

func TestLoginCookieSecureFollowsAdminTLS(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{PasswordHash: mustPasswordHash(t), SessionSecret: "test-session-secret", SessionTTLMinutes: 10}, true)

	rr := loginRequestRecorder(auth, `{"password":"test-password"}`, "192.0.2.1:1234")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].Secure {
		t.Fatalf("cookies = %+v, want Secure cookie", cookies)
	}
}

func loginRequestRecorder(auth *Auth, body string, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	rr := httptest.NewRecorder()
	auth.Login(rr, req)
	return rr
}
