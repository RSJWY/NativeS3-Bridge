package webadmin

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookieName = "natives3_admin_session"

type Auth struct {
	passwordHash    []byte
	sessionKey      []byte
	ttl             time.Duration
	failureDelay    time.Duration
	cookieSecure    bool
	trustForwarded  bool
	totpEnabled     bool
	totpVerifier    TOTPVerifier
	captchaEnabled  bool
	captchaVerifier CaptchaVerifier
	captchaProvider string
	captchaSiteKey  string
	limiter         *loginLimiter
	now             func() time.Time
}

type loginRequest struct {
	Password     string `json:"password"`
	TOTPCode     string `json:"totp_code"`
	CaptchaToken string `json:"captcha_token"`
}

type sessionPayload struct {
	ExpiresAt int64 `json:"exp"`
}

func BootstrapPasswordHash(cfg *config.WebAdminConfig) error {
	if cfg.PasswordHash != "" || cfg.AdminBootstrapPassword == "" {
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.AdminBootstrapPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	cfg.PasswordHash = string(hash)
	slog.Warn("webadmin password_hash generated from admin_bootstrap_password; copy this hash into config and clear admin_bootstrap_password", "password_hash", cfg.PasswordHash)
	return nil
}

func NewAuth(cfg config.WebAdminConfig, secureCookie ...bool) *Auth {
	ttl := time.Duration(cfg.SessionTTLMinutes) * time.Minute
	if ttl <= 0 {
		ttl = 720 * time.Minute
	}
	secure := len(secureCookie) > 0 && secureCookie[0]
	if cfg.PasswordHash == "" {
		slog.Warn("webadmin password_hash is empty; admin login is disabled until a bcrypt hash is configured")
	}
	var totpVerifier TOTPVerifier
	if cfg.TOTP.Enabled {
		verifier, err := newTOTPVerifier(cfg.TOTP.Secret)
		if err != nil {
			slog.Error("configure webadmin totp", "error", err)
		} else {
			totpVerifier = verifier
		}
	}
	a := &Auth{
		passwordHash:    []byte(cfg.PasswordHash),
		sessionKey:      []byte(cfg.SessionSecret),
		ttl:             ttl,
		failureDelay:    500 * time.Millisecond,
		cookieSecure:    secure,
		totpEnabled:     cfg.TOTP.Enabled,
		totpVerifier:    totpVerifier,
		captchaEnabled:  cfg.Captcha.Enabled,
		captchaProvider: cfg.Captcha.Provider,
		captchaSiteKey:  cfg.Captcha.SiteKey,
		now:             time.Now,
	}
	if cfg.Captcha.Enabled {
		a.captchaVerifier = newCaptchaVerifier(cfg.Captcha)
	}
	a.limiter = newLoginLimiter(cfg.LoginMaxFailures, cfg.LoginLockoutWindow, a.now)
	return a
}

func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ip := clientIP(r, a.trustForwarded)
	if locked, retryAfter := a.limiter.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
		writeJSONError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}

	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if a.captchaEnabled && (a.captchaVerifier == nil || !a.captchaVerifier.Verify(r.Context(), req.CaptchaToken, ip)) {
		a.rejectLogin(w, ip)
		return
	}
	if a.passwordHash == nil || bcrypt.CompareHashAndPassword(a.passwordHash, []byte(req.Password)) != nil {
		a.rejectLogin(w, ip)
		return
	}
	if a.totpEnabled && (a.totpVerifier == nil || !a.totpVerifier.Verify(req.TOTPCode, a.now().UTC())) {
		a.rejectLogin(w, ip)
		return
	}
	a.limiter.recordSuccess(ip)

	expires := a.now().UTC().Add(a.ttl)
	value, err := a.signSession(sessionPayload{ExpiresAt: expires.Unix()})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create session failed")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires.Format(time.RFC3339)})
}

func (a *Auth) AuthSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totp_required":    a.totpEnabled,
		"captcha_enabled":  a.captchaEnabled,
		"captcha_provider": a.captchaProvider,
		"captcha_site_key": a.captchaSiteKey,
	})
}

func (a *Auth) rejectLogin(w http.ResponseWriter, ip string) {
	a.limiter.recordFailure(ip)
	time.Sleep(a.failureDelay)
	writeJSONError(w, http.StatusUnauthorized, "invalid login")
}

func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if _, err := a.verifySession(cookie.Value); err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) signSession(payload sessionPayload) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(body)
	sig := a.hmac([]byte(encodedPayload))
	encodedSig := base64.RawURLEncoding.EncodeToString(sig)
	return encodedPayload + "." + encodedSig, nil
}

func (a *Auth) verifySession(value string) (sessionPayload, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return sessionPayload{}, errors.New("invalid session format")
	}
	expectedSig := a.hmac([]byte(parts[0]))
	actualSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(expectedSig, actualSig) != 1 {
		return sessionPayload{}, errors.New("invalid session signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return sessionPayload{}, err
	}
	var payload sessionPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return sessionPayload{}, err
	}
	if payload.ExpiresAt <= a.now().UTC().Unix() {
		return sessionPayload{}, errors.New("session expired")
	}
	return payload, nil
}

func (a *Auth) hmac(body []byte) []byte {
	mac := hmac.New(sha256.New, a.sessionKey)
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}
