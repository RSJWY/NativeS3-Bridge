package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// chunkedBody 构造 aws-chunked 请求体;带签名时 chunk-signature 固定为 64 个 'a'。
func chunkedBody(parts []string, trailerLines []string, withSig bool) string {
	var b strings.Builder
	for _, part := range parts {
		if withSig {
			b.WriteString(toHexChunk(int64(len(part))))
			b.WriteString(";chunk-signature=")
			b.WriteString(strings.Repeat("a", 64))
		} else {
			b.WriteString(toHexChunk(int64(len(part))))
		}
		b.WriteString("\r\n")
		b.WriteString(part)
		b.WriteString("\r\n")
	}
	if withSig {
		b.WriteString("0;chunk-signature=")
		b.WriteString(strings.Repeat("a", 64))
	} else {
		b.WriteString("0")
	}
	b.WriteString("\r\n")
	for _, line := range trailerLines {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

func toHexChunk(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		rem := int(n & 0xF)
		if rem < 10 {
			digits = append([]byte{byte('0' + rem)}, digits...)
		} else {
			digits = append([]byte{byte('a' + rem - 10)}, digits...)
		}
		n >>= 4
	}
	return string(digits)
}

func TestIsAwsChunkedRequest_MatchMatrix(t *testing.T) {
	cases := []struct {
		name       string
		contentSHA string
		encoding   string
		want       bool
	}{
		{"streaming signed payload", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "", true},
		{"streaming unsigned trailer", "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "", true},
		{"streaming signed trailer", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", "", true},
		{"encoding only", "", "aws-chunked", true},
		{"encoding with gzip", "", "aws-chunked, gzip", true},
		{"encoding case insensitive", "", "AWS-CHUNKED", true},
		{"encoding with spaces", "", "aws-chunked,  gzip", true},
		{"neither", "", "", false},
		{"unsigned payload", "UNSIGNED-PAYLOAD", "", false},
		{"concrete sha256", "abcdef0123456789", "", false},
		{"gzip without aws-chunked", "", "gzip", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(""))
			if tc.contentSHA != "" {
				req.Header.Set("x-amz-content-sha256", tc.contentSHA)
			}
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
			}
			if got := isAwsChunkedRequest(req); got != tc.want {
				t.Fatalf("isAwsChunkedRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsAwsChunkedRequest_NormalPutNotDecoded(t *testing.T) {
	// 回归守卫:普通 PUT 绝不能被识别为 aws-chunked,
	// 否则中间件会去解码,把对象字节全部损坏(design.md §7 风险)。
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader("plain"))
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	if isAwsChunkedRequest(req) {
		t.Fatal("normal PUT with UNSIGNED-PAYLOAD must not trigger aws-chunked decoding")
	}
	req2 := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader("plain"))
	if isAwsChunkedRequest(req2) {
		t.Fatal("PUT without any aws-chunked header must not trigger decoding")
	}
}

func TestAwsChunkedMiddleware_ReplacesBodyAndContentLength(t *testing.T) {
	payload := "hello world"
	raw := chunkedBody([]string{payload}, nil, false)
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != int64(len(payload)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(payload))
		}
		out, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(out) != payload {
			t.Fatalf("body = %q, want %q", out, payload)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", strconv.Itoa(len(payload)))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAwsChunkedMiddleware_StripsEncodingFromSidecar(t *testing.T) {
	// aws-chunked 必须从 Content-Encoding 移除,避免它被存进 sidecar 当对象元数据。
	// 同时保留其它编码(如 gzip)。
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want %q", got, "gzip")
		}
		w.WriteHeader(http.StatusOK)
	}))
	payload := "x"
	raw := chunkedBody([]string{payload}, nil, false)
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "1")
	req.Header.Set("Content-Encoding", "aws-chunked, gzip")
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAwsChunkedMiddleware_StripsEncodingWhenOnlyAwsChunked(t *testing.T) {
	// 移除后为空 → 删头。
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want empty (deleted)", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	payload := "x"
	raw := chunkedBody([]string{payload}, nil, false)
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "1")
	req.Header.Set("Content-Encoding", "aws-chunked")
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAwsChunkedMiddleware_PassthroughWhenNotChunked(t *testing.T) {
	// 未命中:原样透传,Body/ContentLength/Header 都不能变。
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(out) != "plain" {
			t.Fatalf("body = %q, want %q", out, "plain")
		}
		if r.ContentLength != int64(len("plain")) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len("plain"))
		}
		if got := r.Header.Get("Content-Encoding"); got != "identity" {
			t.Fatalf("Content-Encoding = %q, want %q", got, "identity")
		}
		w.WriteHeader(http.StatusOK)
	}))
	body := "plain"
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(body))
	req.Header.Set("Content-Encoding", "identity")
	req.ContentLength = int64(len(body))
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAwsChunkedMiddleware_InvalidDeclaredLength(t *testing.T) {
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached on invalid declared length")
	}))
	raw := chunkedBody([]string{"x"}, nil, false)
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "not-a-number")
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "InvalidArgument") {
		t.Fatalf("body = %s, want InvalidArgument", rr.Body.String())
	}
}

func TestAwsChunkedMiddleware_NegativeDeclaredLength(t *testing.T) {
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached on negative declared length")
	}))
	raw := chunkedBody([]string{"x"}, nil, false)
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", "-1")
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAwsChunkedMiddleware_MissingDeclaredLengthPassesMinusOne(t *testing.T) {
	// 缺失 x-amz-decoded-content-length → 不报错,传 -1(不校验总长)。
	payload := "no-declared"
	raw := chunkedBody([]string{payload}, nil, false)
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(out) != payload {
			t.Fatalf("body = %q, want %q", out, payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestTrailerAlgorithms_ParsesXAmzTrailer(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", nil)
	req.Header.Set("x-amz-trailer", "x-amz-checksum-crc32, x-amz-checksum-sha256, x-amz-meta-ignored")
	got := trailerAlgorithms(req)
	want := []string{"x-amz-checksum-crc32", "x-amz-checksum-sha256"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestParseContentEncoding(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"aws-chunked", []string{"aws-chunked"}},
		{"aws-chunked, gzip", []string{"aws-chunked", "gzip"}},
		{"aws-chunked,  gzip,  deflate", []string{"aws-chunked", "gzip", "deflate"}},
	}
	for _, tc := range cases {
		got := parseContentEncoding(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("parseContentEncoding(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i, v := range tc.want {
			if got[i] != v {
				t.Fatalf("parseContentEncoding(%q)[%d] = %q, want %q", tc.in, i, got[i], v)
			}
		}
	}
}

func TestStripAwsChunkedEncoding(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"aws-chunked", ""},
		{"aws-chunked, gzip", "gzip"},
		{"gzip, aws-chunked", "gzip"},
		{"gzip, aws-chunked, deflate", "gzip, deflate"},
		{"AWS-CHUNKED", ""},
		{"gzip", "gzip"},
		{"", ""},
	}
	for _, tc := range cases {
		got := stripAwsChunkedEncoding(tc.in)
		if got != tc.want {
			t.Fatalf("stripAwsChunkedEncoding(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 确保 aws-chunked 流在中间件替换后小 buffer 多次读与一次性 ReadAll 结果一致。
func TestAwsChunkedMiddleware_SmallBufferConsistency(t *testing.T) {
	payload := strings.Repeat("abcdefgh", 32)
	parts := []string{payload[:100], payload[100:200], payload[200:]}
	raw := chunkedBody(parts, nil, false)
	middleware := AwsChunked(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 7)
		var out strings.Builder
		for {
			n, err := r.Body.Read(buf)
			out.Write(buf[:n])
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
		}
		if out.String() != payload {
			t.Fatalf("body = %q, want %q", out.String(), payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPut, "/bucket/key.txt", strings.NewReader(raw))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", strconv.Itoa(len(payload)))
	rr := httptest.NewRecorder()
	middleware.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}
