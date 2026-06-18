package webadmin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTurnstileVerifierAcceptsSuccess(t *testing.T) {
	verifier := &turnstileVerifier{
		secretKey: "secret",
		verifyURL: "http://captcha.local/verify",
		timeout:   time.Second,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			form := string(body)
			for _, want := range []string{"secret=secret", "response=token", "remoteip=192.0.2.1"} {
				if !strings.Contains(form, want) {
					t.Fatalf("form %q missing %q", form, want)
				}
			}
			return stringResponse(http.StatusOK, `{"success":true}`), nil
		})},
	}
	if !verifier.Verify(context.Background(), "token", "192.0.2.1") {
		t.Fatal("valid captcha token was rejected")
	}
}

func TestTurnstileVerifierRejectsProviderFailure(t *testing.T) {
	verifier := &turnstileVerifier{
		secretKey: "secret",
		verifyURL: "http://captcha.local/verify",
		timeout:   time.Second,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return stringResponse(http.StatusOK, `{"success":false}`), nil
		})},
	}
	if verifier.Verify(context.Background(), "token", "") {
		t.Fatal("provider rejection was accepted")
	}
}

func TestTurnstileVerifierFailsClosedOnTimeout(t *testing.T) {
	verifier := &turnstileVerifier{
		secretKey: "secret",
		verifyURL: "http://captcha.local/verify",
		timeout:   time.Nanosecond,
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
	}
	if verifier.Verify(context.Background(), "token", "") {
		t.Fatal("timeout was accepted")
	}
}

func TestTurnstileVerifierRejectsMissingToken(t *testing.T) {
	verifier := &turnstileVerifier{secretKey: "secret", verifyURL: "http://captcha.local/verify", timeout: time.Second}
	if verifier.Verify(context.Background(), strings.Repeat(" ", 3), "") {
		t.Fatal("blank token was accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func stringResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
