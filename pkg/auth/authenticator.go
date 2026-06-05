package auth

import (
	"net/http"
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

	return &Identity{CredentialID: cred.ID, AccessKey: cred.AccessKey, QuotaBytes: cred.QuotaBytes, UsedBytes: cred.UsedBytes}, nil
}
