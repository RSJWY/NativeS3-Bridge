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
	"math/big"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"
)

// DefaultClientCertTTL is the validity period of an issued node client
// certificate. Nodes renew over mTLS before expiry (see design §3.3).
const DefaultClientCertTTL = 90 * 24 * time.Hour

// CA holds the online intermediate CA used to sign node client certificates.
// The offline root CA is not loaded here: it only signs/rotates the
// intermediate and is kept off the panel's daily path (design §3.1).
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     crypto.Signer
}

// LoadIntermediateCA loads the intermediate CA certificate and private key from
// PEM files. Both are required; a missing or malformed file is a fatal,
// fail-closed error so the panel refuses to start without a usable CA.
func LoadIntermediateCA(certPath, keyPath string) (*CA, error) {
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
	if node.Status == NodeStatusRetired {
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
