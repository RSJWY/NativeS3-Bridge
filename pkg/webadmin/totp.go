package webadmin

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const (
	totpDigits = 6
	totpPeriod = 30 * time.Second
	totpWindow = 1
)

type TOTPVerifier interface {
	Verify(code string, at time.Time) bool
}

type standardTOTPVerifier struct {
	secret []byte
}

func newTOTPVerifier(secret string) (*standardTOTPVerifier, error) {
	decoded, err := decodeTOTPSecret(secret)
	if err != nil {
		return nil, err
	}
	return &standardTOTPVerifier{secret: decoded}, nil
}

func (v *standardTOTPVerifier) Verify(code string, at time.Time) bool {
	normalized := strings.TrimSpace(code)
	if len(normalized) != totpDigits {
		return false
	}
	for _, ch := range normalized {
		if ch < '0' || ch > '9' {
			return false
		}
	}

	counter := at.UTC().Unix() / int64(totpPeriod.Seconds())
	for offset := -totpWindow; offset <= totpWindow; offset++ {
		expected := totpCode(v.secret, counter+int64(offset))
		if subtle.ConstantTimeCompare([]byte(expected), []byte(normalized)) == 1 {
			return true
		}
	}
	return false
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	if normalized == "" {
		return nil, fmt.Errorf("secret is empty")
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(normalized, "="))
	if err != nil {
		return nil, err
	}
	if len(decoded) < 10 {
		return nil, fmt.Errorf("secret is too short")
	}
	return decoded, nil
}

func totpCode(secret []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := int32(sum[offset]&0x7f)<<24 |
		int32(sum[offset+1])<<16 |
		int32(sum[offset+2])<<8 |
		int32(sum[offset+3])
	code := value % 1000000
	return fmt.Sprintf("%06d", code)
}
