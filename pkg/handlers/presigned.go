package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
)

func GeneratePresignedURL(cred db.Credential, method, endpoint, bucket, key, region string, expires time.Duration) (string, error) {
	if region == "" {
		region = "us-east-1"
	}
	if expires <= 0 {
		expires = time.Minute
	}
	now := time.Now().UTC()
	base, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + bucket + "/" + strings.TrimLeft(key, "/")
	base.RawQuery = ""
	base.Fragment = ""
	q := base.Query()
	date := now.Format("20060102")
	q.Set("X-Amz-Algorithm", auth.Algorithm)
	q.Set("X-Amz-Credential", cred.AccessKey+"/"+date+"/"+region+"/"+auth.ServiceS3+"/aws4_request")
	q.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	base.RawQuery = q.Encode()

	req := &http.Request{Method: method, URL: base, Host: base.Host}
	canonicalQuery := base.Query()
	canonicalQuery.Del("X-Amz-Signature")
	canonical, err := auth.CanonicalRequestWithQuery(req, []string{"host"}, auth.UnsignedPayload, canonicalQuery)
	if err != nil {
		return "", err
	}
	stringToSign := auth.StringToSign(q.Get("X-Amz-Date"), date, region, auth.ServiceS3, canonical)
	signature := auth.SignString(auth.DeriveSigningKey(cred.SecretKey, date, region, auth.ServiceS3), stringToSign)
	q.Set("X-Amz-Signature", signature)
	base.RawQuery = q.Encode()
	return base.String(), nil
}
