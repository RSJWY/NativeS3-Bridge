package awschunked

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

// chunkedBody 构造一个 aws-chunked 流;带签名时 chunk-signature 固定为 64 个 'a'。
func chunkedBody(parts []string, trailerLines []string, withSig bool) string {
	var b strings.Builder
	for _, part := range parts {
		if withSig {
			b.WriteString(toHex(int64(len(part))))
			b.WriteString(";chunk-signature=")
			b.WriteString(strings.Repeat("a", 64))
		} else {
			b.WriteString(toHex(int64(len(part))))
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

func toHex(n int64) string {
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

func TestReader_SingleChunk(t *testing.T) {
	payload := "hello world"
	raw := chunkedBody([]string{payload}, nil, false)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), nil)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
}

func TestReader_MultiChunk(t *testing.T) {
	parts := []string{"abc", "defgh", "ijklmnop"}
	payload := strings.Join(parts, "")
	raw := chunkedBody(parts, nil, false)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), nil)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
}

func TestReader_EmptyObject(t *testing.T) {
	raw := chunkedBody(nil, nil, false) // 仅终止分块
	r := NewReader(strings.NewReader(raw), 0, nil)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %q want empty", out)
	}
}

func TestReader_WithChunkSignature(t *testing.T) {
	payload := "signed-payload"
	raw := chunkedBody([]string{payload}, nil, true)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), nil)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
}

func TestReader_NoTrailer(t *testing.T) {
	payload := "no-trailer"
	raw := chunkedBody([]string{payload}, nil, false)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), nil)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
	if len(r.Trailers()) != 0 {
		t.Fatalf("trailers = %#v want empty", r.Trailers())
	}
}

func TestReader_WithTrailer(t *testing.T) {
	payload := "trailer-payload"
	raw := chunkedBody([]string{payload}, []string{"x-amz-checksum-crc32:" + base64CRC32(payload)}, false)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), []string{"x-amz-checksum-crc32"})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
	if got := r.Trailers()["x-amz-checksum-crc32"]; got == "" {
		t.Fatalf("trailer missing: %#v", r.Trailers())
	}
}

func base64CRC32(payload string) string {
	t := crc32.New(crc32.IEEETable)
	_, _ = t.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(t.Sum(nil))
}

func base64CRC32C(payload string) string {
	t := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	_, _ = t.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(t.Sum(nil))
}

func base64SHA1(payload string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func base64SHA256(payload string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestReader_ChecksumAlgorithms(t *testing.T) {
	cases := []struct {
		name    string
		algos   []string
		trailer string
	}{
		{"crc32", []string{"x-amz-checksum-crc32"}, base64CRC32("checksum-algo")},
		{"crc32c", []string{"x-amz-checksum-crc32c"}, base64CRC32C("checksum-algo")},
		{"sha1", []string{"x-amz-checksum-sha1"}, base64SHA1("checksum-algo")},
		{"sha256", []string{"x-amz-checksum-sha256"}, base64SHA256("checksum-algo")},
	}
	payload := "checksum-algo"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := chunkedBody([]string{payload}, []string{tc.name + ":" + tc.trailer}, false)
			r := NewReader(strings.NewReader(raw), int64(len(payload)), tc.algos)
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(out) != payload {
				t.Fatalf("got %q want %q", out, payload)
			}
		})
	}
}

func TestReader_ChecksumMismatch(t *testing.T) {
	payload := "mismatch"
	raw := chunkedBody([]string{payload}, []string{"x-amz-checksum-crc32:" + base64CRC32("other")}, false)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), []string{"x-amz-checksum-crc32"})
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
}

func TestReader_UnknownAlgorithmIgnored(t *testing.T) {
	payload := "unknown-algo"
	raw := chunkedBody([]string{payload}, []string{"x-amz-checksum-xxxx:" + base64CRC32(payload)}, false)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), []string{"x-amz-checksum-xxxx"})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
}

func TestReader_MalformedLengthLine(t *testing.T) {
	raw := "not-hex\r\n0\r\n\r\n"
	r := NewReader(strings.NewReader(raw), -1, nil)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrMalformedChunk) {
		t.Fatalf("err = %v, want ErrMalformedChunk", err)
	}
}

func TestReader_MissingCRLF(t *testing.T) {
	raw := "5\r\nhello0\r\n\r\n"
	r := NewReader(strings.NewReader(raw), -1, nil)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrMalformedChunk) {
		t.Fatalf("err = %v, want ErrMalformedChunk", err)
	}
}

func TestReader_ChunkLengthMismatch(t *testing.T) {
	raw := "10\r\nhello\r\n0\r\n\r\n"
	r := NewReader(strings.NewReader(raw), -1, nil)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrMalformedChunk) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrMalformedChunk or ErrUnexpectedEOF", err)
	}
}

func TestReader_DeclaredTooLarge(t *testing.T) {
	payload := "abc"
	raw := chunkedBody([]string{payload}, nil, false)
	r := NewReader(strings.NewReader(raw), 100, nil)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v, want ErrSizeMismatch", err)
	}
}

func TestReader_DeclaredTooSmall(t *testing.T) {
	payload := "abc"
	raw := chunkedBody([]string{payload}, nil, false)
	r := NewReader(strings.NewReader(raw), 1, nil)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v, want ErrSizeMismatch", err)
	}
}

func TestReader_DeclaredUnset(t *testing.T) {
	payload := "no-declared"
	raw := chunkedBody([]string{payload}, nil, false)
	r := NewReader(strings.NewReader(raw), -1, nil)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
}

func TestReader_SmallBufferMultiRead(t *testing.T) {
	payload := strings.Repeat("abcdefgh", 32) // 256 bytes
	parts := []string{
		payload[:64],
		payload[64:128],
		payload[128:192],
		payload[192:],
	}
	raw := chunkedBody(parts, nil, false)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), nil)

	buf := make([]byte, 7)
	var out bytes.Buffer
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if out.String() != payload {
		t.Fatalf("got %q want %q", out.String(), payload)
	}
}

func TestReader_ReadAllConsistency(t *testing.T) {
	payload := strings.Repeat("abcdefgh", 32)
	parts := []string{payload[:100], payload[100:200], payload[200:]}
	raw := chunkedBody(parts, nil, false)
	r1 := NewReader(strings.NewReader(raw), int64(len(payload)), nil)
	all, err := io.ReadAll(r1)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(all) != payload {
		t.Fatalf("got %q want %q", all, payload)
	}
}

func TestReader_OverlongChunkSizeRejected(t *testing.T) {
	huge := toHex(maxChunkSize + 1)
	raw := huge + "\r\n0\r\n\r\n"
	r := NewReader(strings.NewReader(raw), -1, nil)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrMalformedChunk) {
		t.Fatalf("err = %v, want ErrMalformedChunk", err)
	}
}

func TestNewReadCloser_ClosesUnderlyingBody(t *testing.T) {
	payload := "closer"
	raw := chunkedBody([]string{payload}, nil, false)
	src := &closingReader{data: strings.NewReader(raw)}
	rc := NewReadCloser(src, int64(len(payload)), nil)
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !src.closed {
		t.Fatal("underlying body not closed")
	}
}

type closingReader struct {
	data   *strings.Reader
	closed bool
}

func (c *closingReader) Read(p []byte) (int, error) { return c.data.Read(p) }
func (c *closingReader) Close() error {
	c.closed = true
	return nil
}

func TestReader_MultiChunkWithSignatureAndTrailer(t *testing.T) {
	parts := []string{"part1-", "part2-", "part3"}
	payload := strings.Join(parts, "")
	trailer := "x-amz-checksum-crc32:" + base64CRC32(payload)
	raw := chunkedBody(parts, []string{trailer}, true)
	r := NewReader(strings.NewReader(raw), int64(len(payload)), []string{"x-amz-checksum-crc32"})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("got %q want %q", out, payload)
	}
}
