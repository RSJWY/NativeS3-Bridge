package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// LocalSigV2Authenticator 实现 S3 Signature Version 2 验签。
// v2 用 HMAC-SHA1, 不签请求体, 无 region/service scope, 安全性弱于 v4,
// 因此默认关闭 (见 MultiSchemeAuthenticator 与 config.AuthConfig.AllowSigV2)。
type LocalSigV2Authenticator struct {
	store     *CredentialStore
	now       func() time.Time
	clockSkew time.Duration
}

// NewLocalSigV2Authenticator 构造默认 v2 认证器。
// now=time.Now, clockSkew=DefaultClockSkew (与 v4 一致)。
func NewLocalSigV2Authenticator(store *CredentialStore) *LocalSigV2Authenticator {
	return &LocalSigV2Authenticator{store: store, now: time.Now, clockSkew: DefaultClockSkew}
}

// Verify 校验 v2 头部签名或 v2 预签名 URL。
// 流程与 v4 对齐: 解析 -> 时间校验 -> store.Get -> 状态检查 ->
// 计算期望签名 -> 恒定时间比较 -> 返回同构 Identity。
func (a *LocalSigV2Authenticator) Verify(r *http.Request) (*Identity, error) {
	accessKey, providedSignature, expires, err := a.extractV2Material(r)
	if err != nil {
		return nil, err
	}

	// 时间处理 (R4):
	// - 预签名: Expires 是绝对 Unix 时间戳, 过期 -> AccessDenied
	// - 头部签名: x-amz-date 优先 (此时 StringToSign 的 Date 行置空),
	//   否则用 Date 头; 两者支持 RFC1123 与 ISO8601; 时钟偏移 ±15 分钟
	if expires != "" {
		expiry, err := parseV2Expires(expires)
		if err != nil {
			return nil, NewError(CodeSignatureDoesNotMatch)
		}
		if a.now().UTC().After(expiry) {
			return nil, NewError(CodeAccessDenied)
		}
	} else {
		requestTime, err := parseV2RequestTime(r)
		if err != nil {
			return nil, NewError(CodeSignatureDoesNotMatch)
		}
		if skew := a.now().UTC().Sub(requestTime.UTC()); skew > a.clockSkew || skew < -a.clockSkew {
			return nil, NewError(CodeRequestTimeTooSkewed)
		}
	}

	cred, err := a.store.Get(accessKey)
	if err != nil {
		return nil, err
	}
	if cred.Status != "enabled" {
		return nil, NewError(CodeAccessDenied)
	}

	stringToSign := StringToSignV2(r, expires)
	expected := SignStringV2(cred.SecretKey, stringToSign)
	if !constantTimeBase64Equal(expected, providedSignature) {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}

	return &Identity{
		CredentialID: cred.ID,
		AccessKey:    cred.AccessKey,
		Bucket:       cred.Bucket,
		QuotaBytes:   cred.QuotaBytes,
		UsedBytes:    cred.UsedBytes,
	}, nil
}

// extractV2Material 从请求中提取 v2 签名材料。
// 返回: accessKey, signature, expires(预签名时非空, 头部签名时为空), error
func (a *LocalSigV2Authenticator) extractV2Material(r *http.Request) (string, string, string, error) {
	// 头部签名优先: "AWS <AccessKey>:<Signature>"
	if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, AlgorithmV2+" ") {
		parsed, err := ParseV2Authorization(authHeader)
		if err != nil {
			return "", "", "", err
		}
		return parsed.AccessKey, parsed.Signature, "", nil
	}

	// 查询串预签名: AWSAccessKeyId + Expires + Signature 三参齐备
	q := r.URL.Query()
	accessKey := q.Get("AWSAccessKeyId")
	expires := q.Get("Expires")
	signature := q.Get("Signature")
	if accessKey != "" && expires != "" && signature != "" {
		return accessKey, signature, expires, nil
	}

	// 不是 v2 形态。返回 SignatureDoesNotMatch 让分派器/上层兜底。
	return "", "", "", NewError(CodeSignatureDoesNotMatch)
}

// parseV2Expires 解析预签名 URL 的 Expires (Unix 秒, UTC)。
func parseV2Expires(raw string) (time.Time, error) {
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(seconds, 0).UTC(), nil
}

// parseV2RequestTime 解析头部签名的时间戳: x-amz-date 优先, 否则 Date。
// RFC1123 (http.TimeFormat/time.RFC1123) 与 ISO8601 (2006-01-02T15:04:05Z) 都支持。
func parseV2RequestTime(r *http.Request) (time.Time, error) {
	raw := r.Header.Get("x-amz-date")
	if raw == "" {
		raw = r.Header.Get("Date")
	}
	if raw == "" {
		return time.Time{}, NewError(CodeSignatureDoesNotMatch)
	}
	// ISO8601 (S3 v2 规范要求 x-amz-date 用 ISO8601)
	if t, err := time.Parse("2006-01-02T15:04:05Z", raw); err == nil {
		return t, nil
	}
	// RFC1123 / RFC1123GMT (Date 头常用)
	if t, err := time.Parse(time.RFC1123, raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC1123Z, raw); err == nil {
		return t, nil
	}
	return time.Time{}, NewError(CodeSignatureDoesNotMatch)
}
