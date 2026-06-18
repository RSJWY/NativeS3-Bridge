package webadmin

import (
	"bytes"
	"context"
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

func TestLoginTOTPRequired(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{
		PasswordHash:       mustPasswordHash(t),
		SessionSecret:      "test-session-secret",
		SessionTTLMinutes:  10,
		LoginMaxFailures:   3,
		LoginLockoutWindow: time.Minute,
		TOTP: config.TOTPConfig{
			Enabled: true,
			Secret:  "JBSWY3DPEHPK3PXP",
		},
	})
	auth.failureDelay = 0
	auth.totpVerifier = fixedTOTPVerifier{validCode: "123456"}

	missing := loginRequestRecorder(auth, `{"password":"test-password"}`, "192.0.2.1:1234")
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing totp status = %d, want 401", missing.Code)
	}
	wrong := loginRequestRecorder(auth, `{"password":"test-password","totp_code":"000000"}`, "192.0.2.1:1234")
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong totp status = %d, want 401", wrong.Code)
	}
	success := loginRequestRecorder(auth, `{"password":"test-password","totp_code":"123456"}`, "192.0.2.1:1234")
	if success.Code != http.StatusOK {
		t.Fatalf("valid totp status = %d, want 200; body=%s", success.Code, success.Body.String())
	}
}

func TestLoginCaptchaFailureCountsTowardLockout(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{
		PasswordHash:       mustPasswordHash(t),
		SessionSecret:      "test-session-secret",
		SessionTTLMinutes:  10,
		LoginMaxFailures:   2,
		LoginLockoutWindow: time.Minute,
		Captcha: config.CaptchaConfig{
			Enabled:   true,
			Provider:  "turnstile",
			SiteKey:   "site",
			SecretKey: "secret",
			VerifyURL: "http://127.0.0.1/verify",
			Timeout:   time.Second,
		},
	})
	auth.failureDelay = 0
	auth.captchaVerifier = fixedCaptchaVerifier{validToken: "ok"}

	missing := loginRequestRecorder(auth, `{"password":"test-password"}`, "192.0.2.1:1234")
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing captcha status = %d, want 401", missing.Code)
	}
	wrong := loginRequestRecorder(auth, `{"password":"test-password","captcha_token":"bad"}`, "192.0.2.1:1234")
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong captcha status = %d, want 401", wrong.Code)
	}
	locked := loginRequestRecorder(auth, `{"password":"test-password","captcha_token":"ok"}`, "192.0.2.1:1234")
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want 429", locked.Code)
	}
}

func TestLoginCaptchaSuccessProceedsToPassword(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{
		PasswordHash:       mustPasswordHash(t),
		SessionSecret:      "test-session-secret",
		SessionTTLMinutes:  10,
		LoginMaxFailures:   2,
		LoginLockoutWindow: time.Minute,
		Captcha: config.CaptchaConfig{
			Enabled:   true,
			Provider:  "turnstile",
			SiteKey:   "site",
			SecretKey: "secret",
			VerifyURL: "http://127.0.0.1/verify",
			Timeout:   time.Second,
		},
	})
	auth.failureDelay = 0
	auth.captchaVerifier = fixedCaptchaVerifier{validToken: "ok"}

	success := loginRequestRecorder(auth, `{"password":"test-password","captcha_token":"ok"}`, "192.0.2.1:1234")
	if success.Code != http.StatusOK {
		t.Fatalf("success status = %d, want 200; body=%s", success.Code, success.Body.String())
	}
}

func TestAuthSettingsExposeOnlyNonSensitiveFields(t *testing.T) {
	auth := NewAuth(config.WebAdminConfig{
		PasswordHash:      mustPasswordHash(t),
		SessionSecret:     "test-session-secret",
		SessionTTLMinutes: 10,
		TOTP:              config.TOTPConfig{Enabled: true, Secret: "JBSWY3DPEHPK3PXP"},
		Captcha: config.CaptchaConfig{
			Enabled:   true,
			Provider:  "turnstile",
			SiteKey:   "site",
			SecretKey: "secret",
			VerifyURL: "http://127.0.0.1/verify",
			Timeout:   time.Second,
		},
	})

	rr := httptest.NewRecorder()
	auth.AuthSettings(rr, httptest.NewRequest(http.MethodGet, "/api/admin/auth-settings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{`"totp_required":true`, `"captcha_enabled":true`, `"captcha_site_key":"site"`} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Fatalf("settings body missing %q: %s", want, body)
		}
	}
	if bytes.Contains([]byte(body), []byte("secret")) {
		t.Fatalf("settings leaked captcha secret: %s", body)
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

type fixedTOTPVerifier struct {
	validCode string
}

func (v fixedTOTPVerifier) Verify(code string, _ time.Time) bool {
	return code == v.validCode
}

type fixedCaptchaVerifier struct {
	validToken string
}

func (v fixedCaptchaVerifier) Verify(_ context.Context, token string, _ string) bool {
	return token == v.validToken
}
