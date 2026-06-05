package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	Algorithm       = "AWS4-HMAC-SHA256"
	ServiceS3       = "s3"
	terminalScope   = "aws4_request"
	UnsignedPayload = "UNSIGNED-PAYLOAD"
)

type ParsedAuthorization struct {
	AccessKey     string
	Date          string
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

func ParseAuthorization(header string) (ParsedAuthorization, error) {
	if header == "" {
		return ParsedAuthorization{}, NewError(CodeAccessDenied)
	}
	if !strings.HasPrefix(header, Algorithm+" ") {
		return ParsedAuthorization{}, NewError(CodeSignatureDoesNotMatch)
	}

	parts := strings.Split(strings.TrimPrefix(header, Algorithm+" "), ",")
	values := make(map[string]string, len(parts))
	for _, part := range parts {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || value == "" {
			return ParsedAuthorization{}, NewError(CodeSignatureDoesNotMatch)
		}
		values[name] = value
	}

	credentialParts := strings.Split(values["Credential"], "/")
	if len(credentialParts) != 5 || credentialParts[4] != terminalScope {
		return ParsedAuthorization{}, NewError(CodeSignatureDoesNotMatch)
	}
	signedHeaders := strings.Split(values["SignedHeaders"], ";")
	if len(signedHeaders) == 0 || values["Signature"] == "" {
		return ParsedAuthorization{}, NewError(CodeSignatureDoesNotMatch)
	}
	for i := range signedHeaders {
		signedHeaders[i] = strings.ToLower(strings.TrimSpace(signedHeaders[i]))
		if signedHeaders[i] == "" {
			return ParsedAuthorization{}, NewError(CodeSignatureDoesNotMatch)
		}
	}

	return ParsedAuthorization{
		AccessKey:     credentialParts[0],
		Date:          credentialParts[1],
		Region:        credentialParts[2],
		Service:       credentialParts[3],
		SignedHeaders: signedHeaders,
		Signature:     values["Signature"],
	}, nil
}

func CanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) (string, error) {
	canonicalHeaders, signedHeaderList, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r),
		canonicalQueryString(r.URL.Query()),
		canonicalHeaders,
		signedHeaderList,
		payloadHash,
	}, "\n"), nil
}

func CanonicalRequestWithQuery(r *http.Request, signedHeaders []string, payloadHash string, query url.Values) (string, error) {
	canonicalHeaders, signedHeaderList, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r),
		canonicalQueryString(query),
		canonicalHeaders,
		signedHeaderList,
		payloadHash,
	}, "\n"), nil
}

func StringToSign(amzDate, date, region, service, canonicalRequest string) string {
	return strings.Join([]string{
		Algorithm,
		amzDate,
		strings.Join([]string{date, region, service, terminalScope}, "/"),
		hexSHA256(canonicalRequest),
	}, "\n")
}

func DeriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, terminalScope)
}

func SignString(signingKey []byte, stringToSign string) string {
	return hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
}

func PayloadHash(r *http.Request) (string, error) {
	hash := r.Header.Get("x-amz-content-sha256")
	if hash != "" {
		return hash, nil
	}
	if r.ContentLength == 0 || r.Body == nil {
		return hexSHA256(""), nil
	}
	return "", NewError(CodeSignatureDoesNotMatch)
}

func canonicalURI(r *http.Request) string {
	path := r.URL.EscapedPath()
	if path == "" {
		path = r.URL.Path
	}
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func canonicalQueryString(values url.Values) string {
	type pair struct {
		key   string
		value string
	}
	pairs := make([]pair, 0)
	for key, vals := range values {
		encodedKey := awsURIEncode(key, true)
		if len(vals) == 0 {
			pairs = append(pairs, pair{key: encodedKey, value: ""})
			continue
		}
		for _, value := range vals {
			pairs = append(pairs, pair{key: encodedKey, value: awsURIEncode(value, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.key+"="+p.value)
	}
	return strings.Join(parts, "&")
}

func canonicalHeaders(r *http.Request, signedHeaders []string) (string, string, error) {
	headers := append([]string(nil), signedHeaders...)
	sort.Strings(headers)
	lines := make([]string, 0, len(headers))
	for _, name := range headers {
		value := ""
		if name == "host" {
			value = r.Host
		} else if name == "content-length" {
			if r.ContentLength < 0 {
				return "", "", NewError(CodeSignatureDoesNotMatch)
			}
			value = strconv.FormatInt(r.ContentLength, 10)
		} else {
			values := r.Header.Values(name)
			if len(values) == 0 {
				return "", "", NewError(CodeSignatureDoesNotMatch)
			}
			value = strings.Join(values, ",")
		}
		lines = append(lines, name+":"+normalizeHeaderValue(value))
	}
	return strings.Join(lines, "\n") + "\n", strings.Join(headers, ";"), nil
}

func normalizeHeaderValue(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func awsURIEncode(value string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		if c == '/' && !encodeSlash {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%")
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

func hexSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}

func ConstantTimeSignatureEqual(expectedHex, actualHex string) bool {
	expected, err := hex.DecodeString(expectedHex)
	if err != nil {
		return false
	}
	actual, err := hex.DecodeString(actualHex)
	if err != nil {
		return false
	}
	return hmac.Equal(expected, actual)
}
