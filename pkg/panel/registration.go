package panel

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	errRegistrationDenied = errors.New("registration denied")
	errRegistrationCSR    = errors.New("invalid registration CSR")
)

type registrationOutcome struct {
	response    registerResponse
	fingerprint string
	replayed    bool
}

// issueOrReplayRegistration atomically binds a one-time token to the node's
// public key and persists the certificate response. A retry with the same token,
// node, and key returns the exact prior response; a different key is rejected.
func (s *TransportServer) issueOrReplayRegistration(nodeID uint, token string, csrPEM []byte, now time.Time) (registrationOutcome, error) {
	var outcome registrationOutcome
	publicKeyFingerprint, err := registrationPublicKeyFingerprint(csrPEM)
	if err != nil {
		return outcome, errRegistrationCSR
	}
	if s.deps.CA == nil {
		return outcome, errors.New("registration CA unavailable")
	}

	err = s.deps.DB.Transaction(func(tx *gorm.DB) error {
		var record RegistrationToken
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", hashToken(token)).First(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errRegistrationDenied
			}
			return err
		}
		if subtle.ConstantTimeEq(int32(record.NodeID), int32(nodeID)) != 1 {
			return errRegistrationDenied
		}

		var node Node
		if err := tx.Where("id = ?", nodeID).First(&node).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errRegistrationDenied
			}
			return err
		}
		if node.Status != NodeStatusActive {
			return errRegistrationDenied
		}

		if record.UsedAt != nil {
			if subtle.ConstantTimeCompare([]byte(record.PublicKeyFingerprint), []byte(publicKeyFingerprint)) != 1 ||
				record.IssuedCertPEM == "" || record.IssuedCAPEM == "" || record.IssuedNotAfter == nil {
				return errRegistrationDenied
			}
			outcome = registrationOutcome{
				response: registerResponse{
					CertPEM:   record.IssuedCertPEM,
					CACertPEM: record.IssuedCAPEM,
					NotAfter:  record.IssuedNotAfter.UTC().Format(time.RFC3339),
				},
				replayed: true,
			}
			return nil
		}
		if !now.UTC().Before(record.ExpiresAt) {
			return errRegistrationDenied
		}

		signed, err := s.deps.CA.SignNodeCSR(csrPEM, nodeID, s.deps.ClientCTTL, now)
		if err != nil {
			return errRegistrationCSR
		}
		caPEM := string(s.deps.CA.CertificatePEM())
		usedAt := now.UTC()
		notAfter := signed.NotAfter.UTC()
		res := tx.Model(&RegistrationToken{}).
			Where("id = ? AND used_at IS NULL", record.ID).
			Updates(map[string]any{
				"used_at":                usedAt,
				"public_key_fingerprint": publicKeyFingerprint,
				"issued_cert_pem":        string(signed.CertPEM),
				"issued_ca_pem":          caPEM,
				"issued_not_after":       notAfter,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errRegistrationDenied
		}
		cert := NodeCert{
			NodeID:      nodeID,
			Fingerprint: signed.Fingerprint,
			Serial:      signed.Serial,
			NotBefore:   signed.NotBefore,
			NotAfter:    signed.NotAfter,
		}
		if err := tx.Create(&cert).Error; err != nil {
			return err
		}
		outcome = registrationOutcome{
			response: registerResponse{
				CertPEM:   string(signed.CertPEM),
				CACertPEM: caPEM,
				NotAfter:  signed.NotAfter.Format(time.RFC3339),
			},
			fingerprint: signed.Fingerprint,
		}
		return nil
	})
	return outcome, err
}

func registrationPublicKeyFingerprint(csrPEM []byte) (string, error) {
	csr, err := parseCSRPEM(csrPEM)
	if err != nil {
		return "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return "", err
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(publicKeyDER)
	return hex.EncodeToString(sum[:]), nil
}
