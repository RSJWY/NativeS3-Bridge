// awschunked 中间件:在 Auth 之后、quotaMiddleware 之前插入,
// 把 aws-chunked 请求体解码成原始对象字节,使下游配额预检、
// quotaLimitReadCloser、handler、FileBackend 全部只看到解码后的字节。
package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/RSJWY/NativeS3-Bridge/pkg/awschunked"
	"github.com/RSJWY/NativeS3-Bridge/pkg/handlers"
)

// streamingPayloadHashValues 是 x-amz-content-sha256 表示 aws-chunked 的字面量集合。
//
// 这三个值本身就是 payload hash 的占位符(不消费 body),
// 因此解码放在 Auth 后不影响 SigV4 验签(见 design.md §2)。
var streamingPayloadHashValues = map[string]struct{}{
	"STREAMING-AWS4-HMAC-SHA256-PAYLOAD":         {},
	"STREAMING-UNSIGNED-PAYLOAD-TRAILER":         {},
	"STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER": {},
}

// AwsChunked 中间件:命中 aws-chunked 请求时替换 r.Body 为解码器,
// 覆写 r.ContentLength 为解码后长度,并从 Content-Encoding 移除 aws-chunked 值。
//
// 未命中时原样透传,零开销。
func AwsChunked(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAwsChunkedRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		declared, err := declaredDecodedLength(r)
		if err != nil {
			handlers.WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
			return
		}
		algos := trailerAlgorithms(r)
		r.Body = awschunked.NewReadCloser(r.Body, declared, algos)
		if declared >= 0 {
			r.ContentLength = declared
		}
		r.Header.Set("Content-Encoding", stripAwsChunkedEncoding(r.Header.Get("Content-Encoding")))
		next.ServeHTTP(w, r)
	})
}

// isAwsChunkedRequest 判定是否需要对请求体解码。
//
// 判定条件必须严格(R1):
//   - x-amz-content-sha256 属于 STREAMING-* 三值之一;
//   - 或 Content-Encoding 含 aws-chunked(大小写不敏感、逗号分隔取值)。
//
// 判宽了会把普通 PUT 全部损坏,因此必须有未命中矩阵测试兜底。
func isAwsChunkedRequest(r *http.Request) bool {
	if _, ok := streamingPayloadHashValues[r.Header.Get("x-amz-content-sha256")]; ok {
		return true
	}
	for _, v := range parseContentEncoding(r.Header.Get("Content-Encoding")) {
		if strings.EqualFold(v, "aws-chunked") {
			return true
		}
	}
	return false
}

// trailerAlgorithms 解析 x-amz-trailer 头,返回需要校验的算法集合。
//
// 只保留 x-amz-checksum-* 形式的名称;Reader 内部会再过滤一次无法识别的算法。
func trailerAlgorithms(r *http.Request) []string {
	raw := r.Header.Get("x-amz-trailer")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.ToLower(strings.TrimSpace(p))
		if name == "" {
			continue
		}
		if !strings.HasPrefix(name, "x-amz-checksum-") {
			continue
		}
		out = append(out, name)
	}
	return out
}

func declaredDecodedLength(r *http.Request) (int64, error) {
	raw := r.Header.Get("x-amz-decoded-content-length")
	if raw == "" {
		return -1, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errMalformedDeclaredLength{err: err}
	}
	return n, nil
}

// parseContentEncoding 解析 Content-Encoding 头,返回按逗号分隔的编码列表(已 trim 空格)。
func parseContentEncoding(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// stripAwsChunkedEncoding 从 Content-Encoding 值列表里移除 aws-chunked 这一个值,
// 保留其余(如 gzip),避免它被当作对象元数据存进 sidecar(design.md §7 风险)。
func stripAwsChunkedEncoding(header string) string {
	parts := parseContentEncoding(header)
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if !strings.EqualFold(p, "aws-chunked") {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		// 移除后为空 → 整个头删掉(返回空字符串,由 Header.Del 清理)。
		return ""
	}
	return strings.Join(kept, ", ")
}

// errMalformedDeclaredLength 在 x-amz-decoded-content-length 解析失败时,
// 让中间件返回 400 InvalidArgument。
type errMalformedDeclaredLength struct{ err error }

func (e errMalformedDeclaredLength) Error() string { return "malformed x-amz-decoded-content-length" }
