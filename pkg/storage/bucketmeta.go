package storage

import (
	"errors"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ACLPrivate    = "private"
	ACLPublicRead = "public-read"

	DefaultBucketACLCacheTTL = 60 * time.Second
)

var (
	ErrInvalidACL     = errors.New("invalid bucket acl")
	ErrBucketNotEmpty = errors.New("bucket not empty")
)

type BucketStore struct {
	db       *gorm.DB
	dataRoot string
	ttl      time.Duration
	mu       sync.RWMutex
	cache    map[string]cachedBucketACL
}

type cachedBucketACL struct {
	acl       string
	exists    bool
	expiresAt time.Time
}

func NewBucketStore(gdb *gorm.DB, dataRoot string, ttl time.Duration) *BucketStore {
	if ttl <= 0 {
		ttl = DefaultBucketACLCacheTTL
	}
	return &BucketStore{db: gdb, dataRoot: dataRoot, ttl: ttl, cache: make(map[string]cachedBucketACL)}
}

func (s *BucketStore) GetACL(name string) (string, bool, error) {
	now := time.Now()
	s.mu.RLock()
	entry, ok := s.cache[name]
	s.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.acl, entry.exists, nil
	}

	var bucket db.Bucket
	if err := s.db.Where("name = ?", name).First(&bucket).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			s.setCachedACL(name, "", false, now)
			return "", false, nil
		}
		return "", false, err
	}

	s.setCachedACL(name, bucket.ACL, true, now)
	return bucket.ACL, true, nil
}

func (s *BucketStore) Create(name string) error {
	if err := ValidateBucketName(name); err != nil {
		return err
	}
	bucketDir, err := ResolveBucketPath(s.dataRoot, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(bucketDir, 0o755); err != nil {
		return err
	}

	bucket := db.Bucket{Name: name}
	if err := s.db.Clauses(clause.OnConflict{DoNothing: true}).Where(db.Bucket{Name: name}).Attrs(db.Bucket{ACL: ACLPrivate}).FirstOrCreate(&bucket).Error; err != nil {
		return err
	}
	s.Invalidate(name)
	return nil
}

func (s *BucketStore) Delete(name string) error {
	if err := ValidateBucketName(name); err != nil {
		return err
	}
	bucketDir, err := ResolveBucketPath(s.dataRoot, name)
	if err != nil {
		return err
	}
	info, err := os.Stat(bucketDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoSuchBucket
		}
		return err
	}
	if !info.IsDir() {
		return ErrNoSuchBucket
	}

	if err := os.Remove(bucketDir); err != nil {
		if isDirNotEmpty(err) {
			slog.Warn("delete bucket directory not empty", "bucket", name, "error", err)
			return ErrBucketNotEmpty
		}
		return err
	}
	if err := s.db.Where("name = ?", name).Delete(&db.Bucket{}).Error; err != nil {
		return err
	}
	s.Invalidate(name)
	return nil
}

func (s *BucketStore) SetACL(name, acl string) error {
	if err := ValidateBucketName(name); err != nil {
		return err
	}
	if !validBucketACL(acl) {
		return ErrInvalidACL
	}
	res := s.db.Model(&db.Bucket{}).Where("name = ?", name).Update("acl", acl)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNoSuchBucket
	}
	s.Invalidate(name)
	return nil
}

func (s *BucketStore) List() ([]db.Bucket, error) {
	var buckets []db.Bucket
	if err := s.db.Order("name ASC").Find(&buckets).Error; err != nil {
		return nil, err
	}
	return buckets, nil
}

func (s *BucketStore) Invalidate(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, name)
}

func (s *BucketStore) setCachedACL(name, acl string, exists bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[name] = cachedBucketACL{acl: acl, exists: exists, expiresAt: now.Add(s.ttl)}
}

func validBucketACL(acl string) bool {
	return acl == ACLPrivate || acl == ACLPublicRead
}

func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) || errors.Is(err, os.ErrExist)
}
