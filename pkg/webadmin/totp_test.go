package webadmin

import (
	"testing"
	"time"
)

func TestStandardTOTPVerifier(t *testing.T) {
	verifier, err := newTOTPVerifier("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	at := time.Unix(1700000000, 0).UTC()
	code := totpCode(verifier.secret, at.Unix()/30)

	if !verifier.Verify(code, at) {
		t.Fatal("valid code was rejected")
	}
	if !verifier.Verify(totpCode(verifier.secret, at.Unix()/30-1), at) {
		t.Fatal("previous-step code inside window was rejected")
	}
	if verifier.Verify("000000", at) {
		t.Fatal("wrong code was accepted")
	}
	if verifier.Verify("12345x", at) {
		t.Fatal("non-numeric code was accepted")
	}
}

func TestTOTPRejectsInvalidSecret(t *testing.T) {
	if _, err := newTOTPVerifier("bad"); err == nil {
		t.Fatal("expected invalid secret error")
	}
}
