package auth

import (
	"net/http"
	"strings"
	"testing"
)

func TestCanonicalRequestMatchesAWSS3Example(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?lifecycle", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "examplebucket.s3.amazonaws.com"
	req.Header.Set("x-amz-content-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	req.Header.Set("x-amz-date", "20130524T000000Z")

	canonical, err := CanonicalRequest(req, []string{"host", "x-amz-content-sha256", "x-amz-date"}, req.Header.Get("x-amz-content-sha256"))
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	want := strings.Join([]string{
		"GET",
		"/",
		"lifecycle=",
		"host:examplebucket.s3.amazonaws.com",
		"x-amz-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"x-amz-date:20130524T000000Z",
		"",
		"host;x-amz-content-sha256;x-amz-date",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}, "\n")
	if canonical != want {
		t.Fatalf("canonical request mismatch\ngot:\n%s\nwant:\n%s", canonical, want)
	}

	stringToSign := StringToSign("20130524T000000Z", "20130524", "us-east-1", "s3", canonical)
	wantStringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		"20130524T000000Z",
		"20130524/us-east-1/s3/aws4_request",
		"9766c798316ff2757b517bc739a67f6213b4ab36dd5da2f94eaebf79c77395ca",
	}, "\n")
	if stringToSign != wantStringToSign {
		t.Fatalf("string to sign mismatch\ngot:\n%s\nwant:\n%s", stringToSign, wantStringToSign)
	}
}

func TestParseAuthorization(t *testing.T) {
	parsed, err := ParseAuthorization("AWS4-HMAC-SHA256 Credential=AKID/20260605/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abcdef")
	if err != nil {
		t.Fatalf("parse authorization: %v", err)
	}
	if parsed.AccessKey != "AKID" || parsed.Date != "20260605" || parsed.Region != "us-east-1" || parsed.Service != "s3" || parsed.Signature != "abcdef" {
		t.Fatalf("unexpected parsed authorization: %+v", parsed)
	}
}
