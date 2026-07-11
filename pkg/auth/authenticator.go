package auth

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultClockSkew = 15 * time.Minute

type LocalSigV4Authenticator struct {
	store     *CredentialStore
	region    string
	now       func() time.Time
	clockSkew time.Duration
}

func NewLocalSigV4Authenticator(store *CredentialStore, region string) *LocalSigV4Authenticator {
	if region == "" {
		region = "us-east-1"
	}
	return &LocalSigV4Authenticator{store: store, region: region, now: time.Now, clockSkew: DefaultClockSkew}
}

func (a *LocalSigV4Authenticator) Verify(r *http.Request) (*Identity, error) {
	if HasPresignQuery(r) {
		return a.verifyPresigned(r)
	}

	parsed, err := ParseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return nil, err
	}
	if parsed.Region != a.region || parsed.Service != ServiceS3 {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}

	amzDate := r.Header.Get("x-amz-date")
	if amzDate == "" {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}
	requestTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || parsed.Date != requestTime.Format("20060102") {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}
	if skew := a.now().UTC().Sub(requestTime.UTC()); skew > a.clockSkew || skew < -a.clockSkew {
		return nil, NewError(CodeRequestTimeTooSkewed)
	}

	cred, err := a.store.Get(parsed.AccessKey)
	if err != nil {
		return nil, err
	}
	if cred.Status != "enabled" {
		return nil, NewError(CodeAccessDenied)
	}
	payloadHash, err := PayloadHash(r)
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalRequest(r, parsed.SignedHeaders, payloadHash)
	if err != nil {
		return nil, err
	}
	stringToSign := StringToSign(amzDate, parsed.Date, parsed.Region, parsed.Service, canonical)
	expected := SignString(DeriveSigningKey(cred.SecretKey, parsed.Date, parsed.Region, parsed.Service), stringToSign)
	if !ConstantTimeSignatureEqual(expected, parsed.Signature) {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}

	return &Identity{CredentialID: cred.ID, AccessKey: cred.AccessKey, Bucket: cred.Bucket, QuotaBytes: cred.QuotaBytes, UsedBytes: cred.UsedBytes}, nil
}

func HasPresignQuery(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("X-Amz-Algorithm") == Algorithm && q.Get("X-Amz-Credential") != "" && q.Get("X-Amz-Date") != "" && q.Get("X-Amz-Expires") != "" && q.Get("X-Amz-SignedHeaders") != "" && q.Get("X-Amz-Signature") != ""
}

func (a *LocalSigV4Authenticator) verifyPresigned(r *http.Request) (*Identity, error) {
	parsed, amzDate, expires, err := parsePresignQuery(r.URL.Query())
	if err != nil {
		return nil, err
	}
	if parsed.Region != a.region || parsed.Service != ServiceS3 {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}

	requestTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || parsed.Date != requestTime.Format("20060102") {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}
	if a.now().UTC().After(requestTime.UTC().Add(expires)) {
		return nil, NewError(CodeAccessDenied)
	}

	cred, err := a.store.Get(parsed.AccessKey)
	if err != nil {
		return nil, err
	}
	if cred.Status != "enabled" {
		return nil, NewError(CodeAccessDenied)
	}

	canonicalQuery := r.URL.Query()
	canonicalQuery.Del("X-Amz-Signature")
	canonical, err := CanonicalRequestWithQuery(r, parsed.SignedHeaders, UnsignedPayload, canonicalQuery)
	if err != nil {
		return nil, err
	}
	stringToSign := StringToSign(amzDate, parsed.Date, parsed.Region, parsed.Service, canonical)
	expected := SignString(DeriveSigningKey(cred.SecretKey, parsed.Date, parsed.Region, parsed.Service), stringToSign)
	if !ConstantTimeSignatureEqual(expected, parsed.Signature) {
		return nil, NewError(CodeSignatureDoesNotMatch)
	}

	return &Identity{CredentialID: cred.ID, AccessKey: cred.AccessKey, Bucket: cred.Bucket, QuotaBytes: cred.QuotaBytes, UsedBytes: cred.UsedBytes}, nil
}

func parsePresignQuery(query url.Values) (ParsedAuthorization, string, time.Duration, error) {
	if query.Get("X-Amz-Algorithm") != Algorithm {
		return ParsedAuthorization{}, "", 0, NewError(CodeSignatureDoesNotMatch)
	}
	credentialParts := strings.Split(query.Get("X-Amz-Credential"), "/")
	if len(credentialParts) != 5 || credentialParts[4] != terminalScope {
		return ParsedAuthorization{}, "", 0, NewError(CodeSignatureDoesNotMatch)
	}
	signedHeaders := strings.Split(query.Get("X-Amz-SignedHeaders"), ";")
	if len(signedHeaders) == 0 || query.Get("X-Amz-Signature") == "" {
		return ParsedAuthorization{}, "", 0, NewError(CodeSignatureDoesNotMatch)
	}
	for i := range signedHeaders {
		signedHeaders[i] = strings.ToLower(strings.TrimSpace(signedHeaders[i]))
		if signedHeaders[i] == "" {
			return ParsedAuthorization{}, "", 0, NewError(CodeSignatureDoesNotMatch)
		}
	}
	expiresSeconds, err := strconv.ParseInt(query.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expiresSeconds < 0 {
		return ParsedAuthorization{}, "", 0, NewError(CodeSignatureDoesNotMatch)
	}
	amzDate := query.Get("X-Amz-Date")
	return ParsedAuthorization{
		AccessKey:     credentialParts[0],
		Date:          credentialParts[1],
		Region:        credentialParts[2],
		Service:       credentialParts[3],
		SignedHeaders: signedHeaders,
		Signature:     query.Get("X-Amz-Signature"),
	}, amzDate, time.Duration(expiresSeconds) * time.Second, nil
}
