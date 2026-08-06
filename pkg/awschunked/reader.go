// Package awschunked 解码 AWS S3 "aws-chunked" 传输编码。
//
// boto3 >= 1.36 和 AWS SDK 在启用 trailer 校验和时,
// 默认对 PutObject/UploadPart 使用 aws-chunked 编码:
//
//	Content-Encoding: aws-chunked
//	x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER
//	x-amz-decoded-content-length: <解码后字节数>
//	Content-Length: <线上字节数(含分块框架)>
//
//	<hex-len>[;chunk-signature=<64 hex>]\r\n<data>\r\n
//	0[;chunk-signature=<64 hex>]\r\n
//	[<trailer-name>:<trailer-value>\r\n]...
//	\r\n
//
// 本包只解码框架,不验证 chunk-signature(见 spec 已知限制)。
// trailer 中的 x-amz-checksum-* 校验和在能识别时进行校验。
package awschunked

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
)

// 解码过程中暴露的哨兵错误。FileBackend 的 io.Copy 会原样冒泡,
// 由 handlers.writeStorageError 用 errors.Is 识别。
var (
	ErrMalformedChunk   = errors.New("malformed aws-chunked encoding")
	ErrSizeMismatch     = errors.New("decoded length does not match declared length")
	ErrChecksumMismatch = errors.New("trailer checksum mismatch")
)

const (
	// maxChunkSize 单个分块声明的最大字节数,防御恶意超长声明。
	maxChunkSize = 64 << 20 // 64 MiB
	// maxChunkHeaderLine 分块长度行(含 chunk-signature)的最大字节数。
	maxChunkHeaderLine = 16 * 1024
	// maxTrailers trailer 头最大数量。
	maxTrailers = 64
	// maxTrailerLine 单个 trailer 行的最大字节数。
	maxTrailerLine = 8 * 1024
)

type readerState int

const (
	stateChunkHeader readerState = iota
	stateChunkData
	stateChunkCRLF
	stateFinalized
)

// Reader 解码 aws-chunked 传输编码流,返回原始对象字节。
type Reader struct {
	src          *bufio.Reader
	expectedSize int64 // x-amz-decoded-content-length;-1 表示未声明
	bytesRead    int64
	algos        map[string]hash.Hash
	trailers     map[string]string

	state          readerState
	chunkRemaining int64
	err            error
}

// NewReader 返回解码 r 的 Reader。
//
// expectedSize 为 x-amz-decoded-content-length 的值;
// 传 -1 表示头缺失,此时跳过总长校验。
//
// algos 来自请求头 x-amz-trailer(逗号分隔)。
// 只识别 x-amz-checksum-crc32 / crc32c / sha1 / sha256;
// 其余算法名忽略,不报错(R4)。
func NewReader(r io.Reader, expectedSize int64, algos []string) *Reader {
	rd := &Reader{
		src:          bufio.NewReader(r),
		expectedSize: expectedSize,
		algos:        map[string]hash.Hash{},
		trailers:     map[string]string{},
		state:        stateChunkHeader,
	}
	for _, name := range algos {
		n := strings.ToLower(strings.TrimSpace(name))
		if n == "" {
			continue
		}
		if _, exists := rd.algos[n]; exists {
			continue
		}
		switch n {
		case "x-amz-checksum-crc32":
			rd.algos[n] = crc32.New(crc32.IEEETable)
		case "x-amz-checksum-crc32c":
			rd.algos[n] = crc32.New(crc32.MakeTable(crc32.Castagnoli))
		case "x-amz-checksum-sha1":
			rd.algos[n] = sha1.New()
		case "x-amz-checksum-sha256":
			rd.algos[n] = sha256.New()
		default:
			// 无法识别的算法名:忽略,不报错(R4)。
		}
	}
	return rd
}

// Trailers 返回终止分块后解析到的 trailer 头。
// 仅在 Read 返回 io.EOF 后有效。
func (r *Reader) Trailers() map[string]string {
	return r.trailers
}

// Read 实现 io.Reader,返回解码后的对象字节。
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.state == stateFinalized {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	for {
		switch r.state {
		case stateChunkHeader:
			line, err := readLine(r.src, maxChunkHeaderLine)
			if err != nil {
				return 0, r.fail(err)
			}
			size, err := parseChunkSize(line)
			if err != nil {
				return 0, r.fail(err)
			}
			if size == 0 {
				if err := r.readTrailers(); err != nil {
					return 0, r.fail(err)
				}
				if err := r.finalize(); err != nil {
					return 0, r.fail(err)
				}
				r.state = stateFinalized
				return 0, io.EOF
			}
			r.chunkRemaining = size
			r.state = stateChunkData

		case stateChunkData:
			if r.chunkRemaining == 0 {
				r.state = stateChunkCRLF
				continue
			}
			toRead := int64(len(p))
			if toRead > r.chunkRemaining {
				toRead = r.chunkRemaining
			}
			n, err := io.ReadFull(r.src, p[:toRead])
			for _, h := range r.algos {
				_, _ = h.Write(p[:n])
			}
			r.chunkRemaining -= int64(n)
			r.bytesRead += int64(n)
			if r.expectedSize >= 0 && r.bytesRead > r.expectedSize {
				return n, r.fail(ErrSizeMismatch)
			}
			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return n, r.fail(ErrMalformedChunk)
				}
				return n, r.fail(err)
			}
			if r.chunkRemaining == 0 {
				r.state = stateChunkCRLF
			}
			return n, nil

		case stateChunkCRLF:
			if err := readCRLF(r.src); err != nil {
				return 0, r.fail(err)
			}
			r.state = stateChunkHeader

		case stateFinalized:
			return 0, io.EOF
		}
	}
}

func (r *Reader) fail(err error) error {
	r.err = err
	return err
}

func (r *Reader) readTrailers() error {
	for i := 0; i < maxTrailers; i++ {
		line, err := readLine(r.src, maxTrailerLine)
		if err != nil {
			return err
		}
		if line == "" {
			return nil
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return ErrMalformedChunk
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		if name == "" {
			return ErrMalformedChunk
		}
		r.trailers[name] = value
	}
	return ErrMalformedChunk
}

func (r *Reader) finalize() error {
	if r.expectedSize >= 0 && r.bytesRead != r.expectedSize {
		return ErrSizeMismatch
	}
	for name, h := range r.algos {
		expected, present := r.trailers[name]
		if !present {
			continue
		}
		sum := h.Sum(nil)
		encoded := base64.StdEncoding.EncodeToString(sum)
		if encoded != expected {
			return ErrChecksumMismatch
		}
	}
	return nil
}

// readLine 从 bufio.Reader 读取一行(以 \r\n 结尾),
// 返回不含 CRLF 的内容。遇到 EOF 或行长超限返回 ErrMalformedChunk。
func readLine(r *bufio.Reader, maxLen int) (string, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return "", ErrMalformedChunk
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", ErrMalformedChunk
	}
	body := line[:len(line)-2]
	if len(body) > maxLen {
		return "", ErrMalformedChunk
	}
	return string(body), nil
}

// readCRLF 从 bufio.Reader 精确读取并丢弃 \r\n,
// 不匹配则返回 ErrMalformedChunk。
func readCRLF(r *bufio.Reader) error {
	b1, err := r.ReadByte()
	if err != nil || b1 != '\r' {
		return ErrMalformedChunk
	}
	b2, err := r.ReadByte()
	if err != nil || b2 != '\n' {
		return ErrMalformedChunk
	}
	return nil
}

// parseChunkSize 解析分块长度行,格式为 <hex>[;<ext>]。
// 分块扩展(如 chunk-signature=...)解析后丢弃,不验证(R1/design.md §3.1)。
func parseChunkSize(line string) (int64, error) {
	semi := strings.IndexByte(line, ';')
	hexPart := line
	if semi >= 0 {
		hexPart = line[:semi]
	}
	hexPart = strings.TrimSpace(hexPart)
	n, err := strconv.ParseInt(hexPart, 16, 64)
	if err != nil || n < 0 {
		return 0, ErrMalformedChunk
	}
	if n > maxChunkSize {
		return 0, ErrMalformedChunk
	}
	return n, nil
}

// readCloser 把 Reader 和原 body 的 Closer 组合成 io.ReadCloser,
// 供中间件替换 r.Body 使用。
type readCloser struct {
	*Reader
	closer io.Closer
}

// NewReadCloser 返回 io.ReadCloser,解码 aws-chunked 并在 Close 时关闭底层 body。
func NewReadCloser(r io.ReadCloser, expectedSize int64, algos []string) io.ReadCloser {
	return &readCloser{
		Reader: NewReader(r, expectedSize, algos),
		closer: r,
	}
}

func (rc *readCloser) Close() error {
	return rc.closer.Close()
}
