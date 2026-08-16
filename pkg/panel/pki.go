package panel

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"
)

// DefaultClientCertTTL is the validity period of an issued node client
// certificate. Nodes renew via POST /renew over HTTPS mTLS before expiry
// (see docs/multi-node-operations.md §10.2).
const DefaultClientCertTTL = 90 * 24 * time.Hour

// caExpiryWarnAfter 是 CA 自身临期告警阈值。取 90 天的理由：与客户端证书
// TTL 同量级，且 CA 到期意味着全网重装（信任锚不可轮换，见遗留项 L1），
// 90 天是能排期的最短窗口。
const caExpiryWarnAfter = 90 * 24 * time.Hour

// CA holds the deployment CA used to sign node client certificates and the
// panel server certificate. Despite the historical "intermediate-ca" file
// name, there is NO offline root CA above it: the deployment CA is a
// self-signed, pathlen:0 root. It is simultaneously the client-cert issuer,
// the server-cert issuer, and the only trust anchor for both directions.
// Losing its key means full cluster reinstall - it cannot be rotated while
// remaining a trust anchor (known limitation L1, see
// docs/multi-node-operations.md §10.6).
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     crypto.Signer
}

// LoadIntermediateCA loads the intermediate CA certificate and private key from
// PEM files. Both are required; a missing or malformed file is a fatal,
// fail-closed error so the panel refuses to start without a usable CA.
//
// The now parameter is used for CA self-validity checks (expired / not-yet-valid
// / near-expiry warning) so callers can inject deterministic times in tests.
func LoadIntermediateCA(certPath, keyPath string, now time.Time) (*CA, error) {
	if strings.TrimSpace(certPath) == "" || strings.TrimSpace(keyPath) == "" {
		return nil, fmt.Errorf("intermediate CA cert and key paths are required")
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read intermediate CA cert %q: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read intermediate CA key %q: %w", keyPath, err)
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("parse intermediate CA cert: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("intermediate CA certificate is not a CA")
	}
	if now.UTC().Before(cert.NotBefore) {
		return nil, fmt.Errorf("intermediate CA is not yet valid (valid from %s); this is the CA itself, not a node or server certificate", cert.NotBefore.UTC().Format(time.RFC3339))
	}
	if !now.UTC().Before(cert.NotAfter) {
		return nil, fmt.Errorf("intermediate CA expired at %s; CA expiry requires full cluster reinstall (trust anchor is not rotatable); this is the CA itself, not a node or server certificate", cert.NotAfter.UTC().Format(time.RFC3339))
	}
	remaining := cert.NotAfter.UTC().Sub(now.UTC())
	if remaining < caExpiryWarnAfter {
		slog.Warn("intermediate CA is approaching expiry",
			"remaining_days", int(remaining.Hours()/24),
			"expires_at", cert.NotAfter.UTC().Format(time.RFC3339),
			"action", "CA expiry requires full cluster reinstall (trust anchor is not rotatable)")
	}
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse intermediate CA key: %w", err)
	}
	return &CA{cert: cert, certPEM: certPEM, key: key}, nil
}

// Certificate returns the intermediate CA certificate (for building chains and
// configuring mTLS client-cert verification pools).
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// CertificatePEM returns the PEM-encoded intermediate CA certificate as loaded
// from disk. The node stores this to build its mTLS trust chain and to verify
// the panel's server certificate on subsequent connections.
func (c *CA) CertificatePEM() []byte { return c.certPEM }

// SignedCert is the result of signing a node CSR.
type SignedCert struct {
	CertPEM     []byte
	Fingerprint string // hex SHA-256 of the DER certificate
	Serial      string // decimal serial number
	NotBefore   time.Time
	NotAfter    time.Time
}

// SignNodeCSR validates a PEM-encoded CSR and issues a client certificate bound
// to nodeID for the given TTL. The node's private key never leaves the node;
// only the CSR (public key + proof of possession) is presented here.
func (c *CA) SignNodeCSR(csrPEM []byte, nodeID uint, ttl time.Duration, now time.Time) (*SignedCert, error) {
	if c == nil || c.cert == nil || c.key == nil {
		return nil, fmt.Errorf("intermediate CA is not loaded")
	}
	if ttl <= 0 {
		ttl = DefaultClientCertTTL
	}
	csr, err := parseCSRPEM(csrPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature invalid: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	notBefore := now.UTC().Add(-1 * time.Minute) // small backdate for clock skew
	notAfter := now.UTC().Add(ttl)

	// The certificate subject CN is the internal node identity. This is the
	// identity the panel binds the mTLS connection to; it is not reused.
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               nodeSubject(nodeID),
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	fp := sha256.Sum256(der)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &SignedCert{
		CertPEM:     certPEM,
		Fingerprint: hex.EncodeToString(fp[:]),
		Serial:      serial.String(),
		NotBefore:   notBefore,
		NotAfter:    notAfter,
	}, nil
}

// nodeSubject builds the certificate subject for a node identity. Kept in one
// place so verification (subject -> node id) stays consistent with issuance.
func nodeSubject(nodeID uint) pkix.Name {
	return pkix.Name{CommonName: fmt.Sprintf("node-%d", nodeID)}
}

// FingerprintDER returns the hex SHA-256 fingerprint of a DER certificate. Used
// at mTLS handshake time to look the presented certificate up in the cert table.
func FingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// IsCertValid reports whether the certificate with the given fingerprint is
// currently accepted: it must exist, not be revoked, and belong to a node that
// is not retired. This is the application-layer revocation check performed after
// the mTLS handshake and before any control-plane logic runs.
func IsCertValid(db *gorm.DB, fingerprint string, now time.Time) (nodeID uint, ok bool, err error) {
	var cert NodeCert
	if err := db.Where("fingerprint = ?", fingerprint).First(&cert).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if cert.Revoked {
		return 0, false, nil
	}
	if now.UTC().Before(cert.NotBefore) || !now.UTC().Before(cert.NotAfter) {
		return 0, false, nil
	}
	var node Node
	if err := db.Where("id = ?", cert.NodeID).First(&node).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if node.Status != NodeStatusActive {
		return 0, false, nil
	}
	return cert.NodeID, true, nil
}

// RevokeNodeCerts marks all certificates for a node as revoked (e.g. on retire
// or reinstall). Returns the number of certificates revoked.
func RevokeNodeCerts(db *gorm.DB, nodeID uint, now time.Time) (int64, error) {
	revokedAt := now.UTC()
	res := db.Model(&NodeCert{}).
		Where("node_id = ? AND revoked = ?", nodeID, false).
		Updates(map[string]any{"revoked": true, "revoked_at": revokedAt})
	return res.RowsAffected, res.Error
}

// ActivateCert marks the given certificate as activated (first successful
// control-plane connection with this cert) and revokes all other unrevoked
// certificates for the same node. This implements decision D1: the old
// certificate is only revoked once the new one has proven it can connect.
//
// The operation is idempotent: if the cert is already activated,
// RowsAffected == 0 and no revocation occurs. The cert itself is never
// revoked (fingerprint <> ? excludes it).
func ActivateCert(db *gorm.DB, fingerprint string, nodeID uint, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		activatedAt := now.UTC()
		res := tx.Model(&NodeCert{}).
			Where("fingerprint = ? AND activated_at IS NULL", fingerprint).
			Update("activated_at", activatedAt)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		// Revoke all other unrevoked certs for this node.
		return tx.Model(&NodeCert{}).
			Where("node_id = ? AND fingerprint <> ? AND revoked = ?", nodeID, fingerprint, false).
			Updates(map[string]any{"revoked": true, "revoked_at": activatedAt}).Error
	})
}

func parseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseCSRPEM(data []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("no CERTIFICATE REQUEST PEM block found")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

func parsePrivateKeyPEM(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		signer, ok := key.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is not a signer")
		}
		return signer, nil
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported private key PEM type %q", block.Type)
	}
}
