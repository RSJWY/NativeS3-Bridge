package auth

import (
	"net/http"
	"strings"
)

// MultiSchemeAuthenticator 按 v4/v2 签名形态分派到对应认证器。
// v2 == nil 表示 v2 已被配置禁用, 此时 v2 形态请求返回 CodeInvalidRequest
// (明确错误码, 而非 SignatureDoesNotMatch, 便于运维区分"被禁用"与"签名算错")。
type MultiSchemeAuthenticator struct {
	v4 Authenticator
	v2 Authenticator // nil 表示 v2 已禁用
}

// NewMultiSchemeAuthenticator 构造多方案认证器。v2 可为 nil。
func NewMultiSchemeAuthenticator(v4, v2 Authenticator) *MultiSchemeAuthenticator {
	return &MultiSchemeAuthenticator{v4: v4, v2: v2}
}

// Verify 按 design.md §2 的五条顺序判定签名形态:
//  1. Authorization 以 "AWS4-HMAC-SHA256 " 开头 -> v4
//  2. HasPresignQuery(r) 为真 -> v4 预签名
//  3. Authorization 以 "AWS " (带空格) 开头且不是 AWS4- -> v2
//  4. 查询串同时含 AWSAccessKeyId + Expires + Signature -> v2 预签名
//  5. 其余 -> 交回 v4 (让 v4 走原有的无凭证/拒绝路径, 保持现状)
//
// 关键: "AWS4-HMAC-SHA256" 本身也以 AWS 开头, 必须用带空格的 HasPrefix(h, "AWS ")
// 且已被第 1 条拦截在前, 这里做双保险。
func (a *MultiSchemeAuthenticator) Verify(r *http.Request) (*Identity, error) {
	authHeader := r.Header.Get("Authorization")

	// 1. v4 头部签名
	if strings.HasPrefix(authHeader, Algorithm+" ") {
		return a.v4.Verify(r)
	}
	// 2. v4 预签名 (X-Amz-Algorithm=AWS4-HMAC-SHA256 + 完整 query 集)
	if HasPresignQuery(r) {
		return a.v4.Verify(r)
	}
	// 3. v2 头部签名 ("AWS " 带空格; AWS4- 不会匹配因为其后紧跟的是空格而非 4)
	if strings.HasPrefix(authHeader, AlgorithmV2+" ") {
		if a.v2 == nil {
			return nil, NewError(CodeInvalidRequest)
		}
		return a.v2.Verify(r)
	}
	// 4. v2 预签名 (AWSAccessKeyId + Expires + Signature)
	if hasV2PresignQuery(r) {
		if a.v2 == nil {
			return nil, NewError(CodeInvalidRequest)
		}
		return a.v2.Verify(r)
	}
	// 5. 其余: 交回 v4, 保持原有无凭证/拒绝行为不变
	return a.v4.Verify(r)
}

// hasV2PresignQuery 判定 v2 预签名 URL 的查询串形态。
// 必须与 v4 的 HasPresignQuery 互斥: v4 用 X-Amz-Algorithm 等,
// v2 用 AWSAccessKeyId/Expires/Signature, 两者参数名不重叠。
func hasV2PresignQuery(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("AWSAccessKeyId") != "" && q.Get("Expires") != "" && q.Get("Signature") != ""
}
