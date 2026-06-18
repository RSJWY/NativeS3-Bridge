package webadmin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
)

type CaptchaVerifier interface {
	Verify(ctx context.Context, token string, remoteIP string) bool
}

type turnstileVerifier struct {
	secretKey string
	verifyURL string
	timeout   time.Duration
	client    *http.Client
}

type turnstileResponse struct {
	Success bool `json:"success"`
}

func newCaptchaVerifier(cfg config.CaptchaConfig) CaptchaVerifier {
	return &turnstileVerifier{
		secretKey: cfg.SecretKey,
		verifyURL: cfg.VerifyURL,
		timeout:   cfg.Timeout,
		client:    http.DefaultClient,
	}
}

func (v *turnstileVerifier) Verify(ctx context.Context, token string, remoteIP string) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	if v == nil || strings.TrimSpace(v.secretKey) == "" || strings.TrimSpace(v.verifyURL) == "" {
		return false
	}
	timeout := v.timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	form := url.Values{}
	form.Set("secret", v.secretKey)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, v.verifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := v.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}

	var payload turnstileResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil && !errors.Is(err, context.Canceled) {
		return false
	}
	return payload.Success
}
