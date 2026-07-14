package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// DefaultRegistrationTokenTTL is the default validity window for a single-use
// registration token (PRD: 10 minutes).
const DefaultRegistrationTokenTTL = 10 * time.Minute

// registrationTokenBytes is the raw entropy of a registration token before hex
// encoding. 32 bytes = 256 bits, well beyond brute-force reach for a 10-minute
// single-use secret.
const registrationTokenBytes = 32

// Registration token errors. These are deliberately coarse so callers do not
// leak whether a token exists, is expired, or was already used.
var (
	ErrTokenInvalid = errors.New("registration token is invalid")
	ErrTokenExpired = errors.New("registration token is expired")
	ErrTokenUsed    = errors.New("registration token already used")
)

// hashToken returns the hex SHA-256 of a token plaintext. Only this hash is
// stored; the plaintext is shown to the admin once and never persisted.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// GenerateRegistrationToken creates a single-use token for nodeID, persists its
// hash with the given TTL, and returns the plaintext exactly once. If ttl <= 0
// the default TTL is used.
func GenerateRegistrationToken(db *gorm.DB, nodeID uint, ttl time.Duration, now time.Time) (plaintext string, err error) {
	if ttl <= 0 {
		ttl = DefaultRegistrationTokenTTL
	}
	buf := make([]byte, registrationTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token entropy: %w", err)
	}
	plaintext = hex.EncodeToString(buf)
	record := RegistrationToken{
		NodeID:    nodeID,
		TokenHash: hashToken(plaintext),
		ExpiresAt: now.Add(ttl).UTC(),
		CreatedAt: now.UTC(),
	}
	if err := db.Create(&record).Error; err != nil {
		return "", fmt.Errorf("store registration token: %w", err)
	}
	return plaintext, nil
}

// ConsumeRegistrationToken validates a token plaintext for nodeID and marks it
// used atomically. It returns an error if the token does not exist, does not
// belong to nodeID, is expired, or was already used. On success the token is
// invalidated (single-use) within the same transaction.
func ConsumeRegistrationToken(db *gorm.DB, nodeID uint, plaintext string, now time.Time) error {
	tokenHash := hashToken(plaintext)
	return db.Transaction(func(tx *gorm.DB) error {
		var record RegistrationToken
		// Look up by hash only, then constant-time compare the node binding to
		// avoid leaking, via query shape, which check failed.
		if err := tx.Where("token_hash = ?", tokenHash).First(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTokenInvalid
			}
			return err
		}
		if subtle.ConstantTimeEq(int32(record.NodeID), int32(nodeID)) != 1 {
			return ErrTokenInvalid
		}
		if record.UsedAt != nil {
			return ErrTokenUsed
		}
		if !now.UTC().Before(record.ExpiresAt) {
			return ErrTokenExpired
		}
		used := now.UTC()
		// Guard the update with used_at IS NULL so a concurrent consume cannot
		// double-spend the same token.
		res := tx.Model(&RegistrationToken{}).
			Where("id = ? AND used_at IS NULL", record.ID).
			Update("used_at", used)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrTokenUsed
		}
		return nil
	})
}

// InvalidateNodeTokens marks all unused registration tokens for a node as used
// (e.g. on retire or when the admin revokes outstanding tokens).
func InvalidateNodeTokens(db *gorm.DB, nodeID uint, now time.Time) error {
	return db.Model(&RegistrationToken{}).
		Where("node_id = ? AND used_at IS NULL", nodeID).
		Update("used_at", now.UTC()).Error
}
