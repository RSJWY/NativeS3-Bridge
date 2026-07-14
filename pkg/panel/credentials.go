package panel

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"gorm.io/gorm"
)

// CredentialStore manages panel-authoritative S3 credentials. Secret keys are
// stored only as ciphertext (AEAD, master key external). The plaintext secret
// is returned to the admin EXACTLY ONCE at creation or rotation and is never
// returned by any list/detail/log/audit path (design §2.3): callers that need
// to display it must do so from the single return value here.
type CredentialStore struct {
	db     *gorm.DB
	cipher *SecretCipher
}

// NewPanelCredentialStore builds the store over the panel DB and cipher.
func NewPanelCredentialStore(db *gorm.DB, cipher *SecretCipher) *CredentialStore {
	return &CredentialStore{db: db, cipher: cipher}
}

// CreatedCredential is the one-time result of Create/Rotate. SecretKey is the
// plaintext, present only in this return value.
type CreatedCredential struct {
	ID        uint
	NodeID    uint
	AccessKey string
	SecretKey string // plaintext, returned once
	Name      string
	Bucket    string
	Status    string
}

// Create provisions a new credential for a node: it generates the access/secret
// keypair, encrypts the secret with the master key, persists ciphertext, and
// returns the plaintext secret exactly once. It never stores the plaintext.
func (s *CredentialStore) Create(nodeID uint, name, bucket string, quotaBytes int64) (CreatedCredential, error) {
	if s.cipher == nil {
		return CreatedCredential{}, ErrMasterKeyMissing
	}
	if quotaBytes < 0 {
		return CreatedCredential{}, fmt.Errorf("quota_bytes must be >= 0")
	}
	name = strings.TrimSpace(name)
	bucket = strings.TrimSpace(bucket)

	for attempt := 0; attempt < 5; attempt++ {
		accessKey, err := randomAccessKey()
		if err != nil {
			return CreatedCredential{}, err
		}
		secretKey, err := randomSecretKey()
		if err != nil {
			return CreatedCredential{}, err
		}
		ciphertext, err := s.cipher.Encrypt(secretKey)
		if err != nil {
			return CreatedCredential{}, fmt.Errorf("encrypt secret: %w", err)
		}
		cred := NodeCredential{
			NodeID:          nodeID,
			AccessKey:       accessKey,
			SecretKeyCipher: ciphertext,
			Name:            name,
			Bucket:          bucket,
			Status:          "enabled",
			QuotaBytes:      quotaBytes,
		}
		if err := s.db.Create(&cred).Error; err != nil {
			if attempt < 4 {
				continue // access key collision; retry with a fresh key
			}
			return CreatedCredential{}, fmt.Errorf("create credential: %w", err)
		}
		return CreatedCredential{
			ID: cred.ID, NodeID: nodeID, AccessKey: accessKey, SecretKey: secretKey,
			Name: name, Bucket: bucket, Status: "enabled",
		}, nil
	}
	return CreatedCredential{}, fmt.Errorf("create credential: exhausted access key attempts")
}

// Rotate generates a new secret key for an existing credential, re-encrypts and
// persists it, and returns the new plaintext once. The access key is unchanged;
// the old secret is immediately invalid once the node applies the new desired
// state (design §2.3 rotation). The caller must publish + push desired state so
// the node picks up the new secret.
func (s *CredentialStore) Rotate(nodeID uint, accessKey string) (CreatedCredential, error) {
	if s.cipher == nil {
		return CreatedCredential{}, ErrMasterKeyMissing
	}
	var cred NodeCredential
	if err := s.db.Where("node_id = ? AND access_key = ?", nodeID, accessKey).First(&cred).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CreatedCredential{}, ErrCredentialNotFound
		}
		return CreatedCredential{}, err
	}
	secretKey, err := randomSecretKey()
	if err != nil {
		return CreatedCredential{}, err
	}
	ciphertext, err := s.cipher.Encrypt(secretKey)
	if err != nil {
		return CreatedCredential{}, fmt.Errorf("encrypt secret: %w", err)
	}
	if err := s.db.Model(&NodeCredential{}).Where("id = ?", cred.ID).
		Update("secret_key_cipher", ciphertext).Error; err != nil {
		return CreatedCredential{}, fmt.Errorf("update secret: %w", err)
	}
	return CreatedCredential{
		ID: cred.ID, NodeID: nodeID, AccessKey: accessKey, SecretKey: secretKey,
		Name: cred.Name, Bucket: cred.Bucket, Status: cred.Status,
	}, nil
}

// ErrCredentialNotFound is returned when a credential lookup misses.
var ErrCredentialNotFound = errors.New("credential not found")

// randomAccessKey mirrors the webadmin access-key alphabet/length so panel and
// node credentials look identical to S3 clients.
func randomAccessKey() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 20)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

func randomSecretKey() (string, error) {
	raw := make([]byte, 30)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw), nil
}
