package auth

import (
	"errors"
	"sync"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

const DefaultCredentialCacheTTL = 60 * time.Second

type CredentialStore struct {
	db    *gorm.DB
	ttl   time.Duration
	mu    sync.RWMutex
	cache map[string]cachedCredential
}

type cachedCredential struct {
	credential db.Credential
	expiresAt  time.Time
}

func NewCredentialStore(gdb *gorm.DB, ttl time.Duration) *CredentialStore {
	if ttl <= 0 {
		ttl = DefaultCredentialCacheTTL
	}
	return &CredentialStore{db: gdb, ttl: ttl, cache: make(map[string]cachedCredential)}
}

func (s *CredentialStore) Get(accessKey string) (*db.Credential, error) {
	now := time.Now()
	s.mu.RLock()
	entry, ok := s.cache[accessKey]
	s.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		cred := entry.credential
		if cred.Status != "enabled" {
			return &cred, nil
		}
		var fresh struct{ UsedBytes int64 }
		if err := s.db.Model(&db.Credential{}).Select("used_bytes").Where("access_key = ?", accessKey).First(&fresh).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				s.Invalidate(accessKey)
				return nil, NewError(CodeInvalidAccessKeyID)
			}
			return nil, err
		}
		cred.UsedBytes = fresh.UsedBytes
		return &cred, nil
	}

	var cred db.Credential
	if err := s.db.Where("access_key = ?", accessKey).First(&cred).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, NewError(CodeInvalidAccessKeyID)
		}
		return nil, err
	}

	s.mu.Lock()
	s.cache[accessKey] = cachedCredential{credential: cred, expiresAt: now.Add(s.ttl)}
	s.mu.Unlock()

	return &cred, nil
}

func (s *CredentialStore) Invalidate(accessKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, accessKey)
}
