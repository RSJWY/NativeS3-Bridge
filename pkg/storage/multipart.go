package storage

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNoSuchUpload          = errors.New("no such multipart upload")
	ErrInvalidPartNumber     = errors.New("invalid part number")
	ErrInvalidPart           = errors.New("invalid part")
	ErrInvalidPartOrder      = errors.New("invalid part order")
	ErrMultipartStorageLimit = errors.New("multipart pending storage limit exceeded")
)

type CompletedPart struct {
	PartNumber int
	ETag       string
}

type PartInfo struct {
	PartNumber   int
	ETag         string
	Size         int64
	LastModified time.Time
}

type MultipartUploadInfo struct {
	UploadID  string
	Bucket    string
	Key       string
	CreatedAt time.Time
}

type MultipartStore struct {
	root            string
	tmpRoot         string
	metadataSuffix  string
	maxPendingBytes int64
	mu              sync.Mutex
}

func (s *MultipartStore) SetMaxPendingBytes(limit int64) {
	s.mu.Lock()
	s.maxPendingBytes = limit
	s.mu.Unlock()
}

type multipartManifest struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	ContentType string            `json:"content_type"`
	Metadata    map[string]string `json:"metadata"`
	Tags        map[string]string `json:"tags"`
	CreatedAt   string            `json:"created_at"`
}

func NewMultipartStore(root, tmpRoot, metadataSuffix string) (*MultipartStore, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	tmpAbs, err := filepath.Abs(tmpRoot)
	if err != nil {
		return nil, err
	}
	if metadataSuffix == "" {
		metadataSuffix = DefaultMetadataSuffix
	}
	if err := os.MkdirAll(tmpAbs, 0o755); err != nil {
		return nil, err
	}
	return &MultipartStore{root: rootAbs, tmpRoot: tmpAbs, metadataSuffix: metadataSuffix}, nil
}

func (s *MultipartStore) Create(bucket, key, contentType string, meta map[string]string, tags map[string]string) (string, error) {
	if _, err := ResolveObjectPath(s.root, bucket, key); err != nil {
		return "", err
	}
	uploadID := uuid.NewString()
	dir := s.uploadDir(uploadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	manifest := multipartManifest{
		Bucket:      bucket,
		Key:         filepath.ToSlash(key),
		ContentType: normalizeContentType(contentType, key),
		Metadata:    cloneStringMap(meta),
		Tags:        cloneStringMap(tags),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.writeManifest(uploadID, manifest); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return uploadID, nil
}

func (s *MultipartStore) UploadPart(uploadID string, partNumber int, r io.Reader) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if partNumber < 1 || partNumber > 10000 {
		return "", ErrInvalidPartNumber
	}
	if _, err := s.readManifest(uploadID); err != nil {
		return "", err
	}
	path := s.partPath(uploadID, partNumber)
	oldSize := int64(0)
	if info, err := os.Stat(path); err == nil {
		oldSize = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	available := int64(-1)
	if s.maxPendingBytes > 0 {
		pending, err := s.pendingBytesLocked()
		if err != nil {
			return "", err
		}
		available = s.maxPendingBytes - pending + oldSize
		if available < 0 {
			return "", ErrMultipartStorageLimit
		}
	}
	tmp := path + ".tmp-" + randomHex(8)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	h := md5.New()
	reader := r
	if available >= 0 {
		reader = io.LimitReader(r, available+1)
	}
	written, copyErr := io.Copy(io.MultiWriter(f, h), reader)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return "", firstErr(copyErr, syncErr, closeErr)
	}
	if available >= 0 && written > available {
		_ = os.Remove(tmp)
		return "", ErrMultipartStorageLimit
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *MultipartStore) pendingBytesLocked() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.tmpRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "part-") || strings.Contains(entry.Name(), ".tmp-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (s *MultipartStore) ValidateTarget(uploadID, bucket, key string) error {
	manifest, err := s.readManifest(uploadID)
	if err != nil {
		return err
	}
	if manifest.Bucket != bucket || manifest.Key != filepath.ToSlash(key) {
		return ErrNoSuchUpload
	}
	return nil
}

func (s *MultipartStore) CompleteSize(uploadID string, parts []CompletedPart) (int64, error) {
	_, err := s.validateParts(uploadID, parts)
	if err != nil {
		return 0, err
	}
	total := int64(0)
	for _, part := range parts {
		info, err := os.Stat(s.partPath(uploadID, part.PartNumber))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return 0, ErrInvalidPart
			}
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func (s *MultipartStore) ExistingObjectSize(uploadID string) (int64, error) {
	manifest, err := s.readManifest(uploadID)
	if err != nil {
		return 0, err
	}
	target, err := ResolveObjectPath(s.root, manifest.Bucket, manifest.Key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *MultipartStore) Complete(uploadID string, parts []CompletedPart) (ObjectInfo, error) {
	manifest, err := s.validateParts(uploadID, parts)
	if err != nil {
		return ObjectInfo{}, err
	}
	target, err := ResolveObjectPath(s.root, manifest.Bucket, manifest.Key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ObjectInfo{}, err
	}
	tmp := target + ".merge-" + uploadID
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return ObjectInfo{}, err
	}

	var partMD5s []byte
	total := int64(0)
	for _, part := range parts {
		partPath := s.partPath(uploadID, part.PartNumber)
		partETag, md5Bytes, size, err := partDigest(partPath)
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return ObjectInfo{}, err
		}
		if normalizeETag(part.ETag) != partETag {
			_ = out.Close()
			_ = os.Remove(tmp)
			return ObjectInfo{}, ErrInvalidPart
		}
		in, err := os.Open(partPath)
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return ObjectInfo{}, err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := in.Close()
		if copyErr != nil || closeErr != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return ObjectInfo{}, firstErr(copyErr, closeErr)
		}
		partMD5s = append(partMD5s, md5Bytes...)
		total += size
	}
	syncErr := out.Sync()
	closeErr := out.Close()
	if syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, firstErr(syncErr, closeErr)
	}

	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, err
	}
	stat, err := os.Stat(target)
	if err != nil {
		return ObjectInfo{}, err
	}
	etagSum := md5.Sum(partMD5s)
	etag := hex.EncodeToString(etagSum[:]) + "-" + strconv.Itoa(len(parts))
	metadata := cloneStringMap(manifest.Metadata)
	if err := WriteSidecar(target, s.metadataSuffix, Sidecar{
		ETag:        etag,
		ContentType: normalizeContentType(manifest.ContentType, manifest.Key),
		Metadata:    metadata,
		Tags:        cloneStringMap(manifest.Tags),
		Size:        total,
		UploadedAt:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return ObjectInfo{}, err
	}
	if err := os.RemoveAll(s.uploadDir(uploadID)); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		Key:          filepath.ToSlash(manifest.Key),
		Size:         total,
		ETag:         etag,
		LastModified: stat.ModTime().UTC(),
		ContentType:  normalizeContentType(manifest.ContentType, manifest.Key),
		Metadata:     metadata,
	}, nil
}

func (s *MultipartStore) Abort(uploadID string) error {
	if err := validateUploadID(uploadID); err != nil {
		return err
	}
	dir := s.uploadDir(uploadID)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoSuchUpload
		}
		return err
	}
	return os.RemoveAll(dir)
}

func (s *MultipartStore) ListParts(uploadID string) ([]PartInfo, error) {
	if _, err := s.readManifest(uploadID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.uploadDir(uploadID))
	if err != nil {
		return nil, err
	}
	parts := make([]PartInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "part-") || strings.Contains(entry.Name(), ".tmp-") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "part-"))
		if err != nil {
			continue
		}
		path := filepath.Join(s.uploadDir(uploadID), entry.Name())
		etag, _, size, err := partDigest(path)
		if err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		parts = append(parts, PartInfo{PartNumber: n, ETag: etag, Size: size, LastModified: info.ModTime().UTC()})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

func (s *MultipartStore) ListMultipartUploads(bucket, prefix string) ([]MultipartUploadInfo, error) {
	entries, err := os.ReadDir(s.tmpRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	uploads := make([]MultipartUploadInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, err := s.readManifest(entry.Name())
		if err != nil {
			continue
		}
		if manifest.Bucket != bucket || (prefix != "" && !strings.HasPrefix(manifest.Key, prefix)) {
			continue
		}
		createdAt, _ := time.Parse(time.RFC3339, manifest.CreatedAt)
		uploads = append(uploads, MultipartUploadInfo{UploadID: entry.Name(), Bucket: manifest.Bucket, Key: manifest.Key, CreatedAt: createdAt.UTC()})
	}
	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].Key == uploads[j].Key {
			return uploads[i].UploadID < uploads[j].UploadID
		}
		return uploads[i].Key < uploads[j].Key
	})
	return uploads, nil
}

func (s *MultipartStore) CleanupExpired(ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	entries, err := os.ReadDir(s.tmpRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		uploadID := entry.Name()
		createdAt := time.Time{}
		manifest, err := s.readManifest(uploadID)
		if err == nil {
			createdAt, _ = time.Parse(time.RFC3339, manifest.CreatedAt)
		}
		if createdAt.IsZero() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			createdAt = info.ModTime().UTC()
		}
		if now.Sub(createdAt.UTC()) > ttl {
			if err := os.RemoveAll(s.uploadDir(uploadID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *MultipartStore) StartGC(ctx <-chan struct{}, interval, ttl time.Duration) {
	if interval <= 0 || ttl <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx:
				return
			case <-ticker.C:
				if err := s.CleanupExpired(ttl); err != nil {
					slog.Warn("multipart gc", "error", err)
				}
			}
		}
	}()
}

func (s *MultipartStore) validateParts(uploadID string, parts []CompletedPart) (multipartManifest, error) {
	manifest, err := s.readManifest(uploadID)
	if err != nil {
		return multipartManifest{}, err
	}
	if len(parts) == 0 {
		return multipartManifest{}, ErrInvalidPart
	}
	last := 0
	for _, part := range parts {
		if part.PartNumber < 1 || part.PartNumber > 10000 {
			return multipartManifest{}, ErrInvalidPartNumber
		}
		if part.PartNumber != last+1 {
			return multipartManifest{}, ErrInvalidPartOrder
		}
		if part.ETag == "" {
			return multipartManifest{}, ErrInvalidPart
		}
		if _, err := os.Stat(s.partPath(uploadID, part.PartNumber)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return multipartManifest{}, ErrInvalidPart
			}
			return multipartManifest{}, err
		}
		last = part.PartNumber
	}
	return manifest, nil
}

func (s *MultipartStore) readManifest(uploadID string) (multipartManifest, error) {
	if err := validateUploadID(uploadID); err != nil {
		return multipartManifest{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.uploadDir(uploadID), "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return multipartManifest{}, ErrNoSuchUpload
		}
		return multipartManifest{}, err
	}
	var manifest multipartManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return multipartManifest{}, err
	}
	if manifest.Metadata == nil {
		manifest.Metadata = map[string]string{}
	}
	if manifest.Tags == nil {
		manifest.Tags = map[string]string{}
	}
	return manifest, nil
}

func (s *MultipartStore) writeManifest(uploadID string, manifest multipartManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.uploadDir(uploadID), "manifest.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *MultipartStore) uploadDir(uploadID string) string {
	return filepath.Join(s.tmpRoot, filepath.Base(uploadID))
}

func (s *MultipartStore) partPath(uploadID string, partNumber int) string {
	return filepath.Join(s.uploadDir(uploadID), fmt.Sprintf("part-%05d", partNumber))
}

func validateUploadID(uploadID string) error {
	trimmed := strings.TrimSpace(uploadID)
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed.String() != strings.ToLower(trimmed) {
		return ErrNoSuchUpload
	}
	return nil
}

func partDigest(path string) (string, []byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, 0, ErrInvalidPart
		}
		return "", nil, 0, err
	}
	defer f.Close()
	h := md5.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", nil, 0, err
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum), sum, size, nil
}

func normalizeETag(etag string) string {
	etag = strings.TrimSpace(etag)
	etag = strings.Trim(etag, "\"")
	return strings.ToLower(etag)
}
