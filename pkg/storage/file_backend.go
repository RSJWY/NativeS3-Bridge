package storage

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNoSuchBucket = errors.New("no such bucket")
	ErrNoSuchKey    = errors.New("no such key")
	ErrInvalidRange = errors.New("invalid range")
)

type FileBackend struct {
	root         string
	metadataMu   sync.RWMutex
	contentTypes map[string]string
}

func NewFileBackend(root string) (*FileBackend, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(rootAbs, 0o755); err != nil {
		return nil, err
	}
	return &FileBackend{root: rootAbs, contentTypes: make(map[string]string)}, nil
}

func (b *FileBackend) PutObject(bucket, key string, r io.Reader, contentType string) (ObjectInfo, error) {
	target, err := ResolveObjectPath(b.root, bucket, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ObjectInfo{}, err
	}

	tmp := target + ".tmp-" + randomHex(8)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return ObjectInfo{}, err
	}

	h := md5.New()
	size, copyErr := io.Copy(io.MultiWriter(f, h), r)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, firstErr(copyErr, syncErr, closeErr)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, err
	}

	info, err := os.Stat(target)
	if err != nil {
		return ObjectInfo{}, err
	}
	resolvedContentType := normalizeContentType(contentType, key)
	b.setContentType(target, resolvedContentType)
	return ObjectInfo{
		Key:          filepath.ToSlash(key),
		Size:         size,
		ETag:         hex.EncodeToString(h.Sum(nil)),
		LastModified: info.ModTime().UTC(),
		ContentType:  resolvedContentType,
	}, nil
}

func (b *FileBackend) GetObject(bucket, key string, rng *Range) (io.ReadCloser, ObjectInfo, error) {
	info, err := b.HeadObject(bucket, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	target, err := ResolveObjectPath(b.root, bucket, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	f, err := os.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ObjectInfo{}, ErrNoSuchKey
		}
		return nil, ObjectInfo{}, err
	}

	if rng == nil {
		return f, info, nil
	}
	normalized, err := NormalizeRange(info.Size, *rng)
	if err != nil {
		_ = f.Close()
		return nil, ObjectInfo{}, err
	}
	length := normalized.End - normalized.Start + 1
	return readCloser{Reader: io.NewSectionReader(f, normalized.Start, length), Closer: f}, info, nil
}

func (b *FileBackend) HeadObject(bucket, key string) (ObjectInfo, error) {
	target, err := ResolveObjectPath(b.root, bucket, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := b.ensureBucketExists(bucket); err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, ErrNoSuchKey
		}
		return ObjectInfo{}, err
	}
	if info.IsDir() {
		return ObjectInfo{}, ErrNoSuchKey
	}
	etag, err := fileMD5(target)
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		Key:          filepath.ToSlash(key),
		Size:         info.Size(),
		ETag:         etag,
		LastModified: info.ModTime().UTC(),
		ContentType:  b.contentTypeFor(target, key),
	}, nil
}

func (b *FileBackend) DeleteObject(bucket, key string) error {
	target, err := ResolveObjectPath(b.root, bucket, key)
	if err != nil {
		return err
	}
	if err := b.ensureBucketExists(bucket); err != nil {
		return err
	}
	err = os.Remove(target)
	if errors.Is(err, os.ErrNotExist) {
		b.deleteContentType(target)
		return nil
	}
	if err == nil {
		b.deleteContentType(target)
	}
	return err
}

func (b *FileBackend) ListObjects(bucket, prefix, delimiter, token string, maxKeys int) (ListResult, error) {
	bucketDir, err := ResolveBucketPath(b.root, bucket)
	if err != nil {
		return ListResult{}, err
	}
	stat, err := os.Stat(bucketDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ListResult{}, ErrNoSuchBucket
		}
		return ListResult{}, err
	}
	if !stat.IsDir() {
		return ListResult{}, ErrNoSuchBucket
	}
	if maxKeys < 0 {
		maxKeys = 1000
	}

	common := map[string]struct{}{}
	var objects []ObjectInfo
	err = filepath.WalkDir(bucketDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".multipart" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".s3meta") || strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") {
			return nil
		}
		rel, err := filepath.Rel(bucketDir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		if delimiter == "/" {
			remainder := strings.TrimPrefix(key, prefix)
			if idx := strings.Index(remainder, "/"); idx >= 0 {
				common[prefix+remainder[:idx+1]] = struct{}{}
				return nil
			}
		}
		obj, err := b.HeadObject(bucket, key)
		if err != nil {
			return err
		}
		objects = append(objects, obj)
		return nil
	})
	if err != nil {
		return ListResult{}, err
	}

	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	prefixes := make([]string, 0, len(common))
	for p := range common {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)

	items := make([]listItem, 0, len(objects)+len(prefixes))
	for i := range objects {
		items = append(items, listItem{key: objects[i].Key, object: &objects[i]})
	}
	for _, p := range prefixes {
		items = append(items, listItem{key: p, prefix: p})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })

	result := ListResult{}
	for _, item := range items {
		if token != "" && item.key <= token {
			continue
		}
		if len(result.Objects)+len(result.CommonPrefixes) == maxKeys {
			result.IsTruncated = true
			break
		}
		result.NextToken = item.key
		if item.object != nil {
			result.Objects = append(result.Objects, *item.object)
		} else {
			result.CommonPrefixes = append(result.CommonPrefixes, item.prefix)
		}
	}
	if !result.IsTruncated {
		result.NextToken = ""
	}
	return result, nil
}

func (b *FileBackend) ListBuckets() ([]BucketInfo, error) {
	if err := os.MkdirAll(b.root, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(b.root)
	if err != nil {
		return nil, err
	}
	buckets := make([]BucketInfo, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") || ValidateBucketName(name) != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, BucketInfo{Name: name, CreationDate: info.ModTime().UTC()})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	return buckets, nil
}

func (b *FileBackend) ensureBucketExists(bucket string) error {
	bucketDir, err := ResolveBucketPath(b.root, bucket)
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
	return nil
}

func (b *FileBackend) setContentType(path, contentType string) {
	b.metadataMu.Lock()
	defer b.metadataMu.Unlock()
	b.contentTypes[path] = contentType
}

func (b *FileBackend) contentTypeFor(path, key string) string {
	b.metadataMu.RLock()
	contentType := b.contentTypes[path]
	b.metadataMu.RUnlock()
	if contentType != "" {
		return contentType
	}
	return normalizeContentType("", key)
}

func (b *FileBackend) deleteContentType(path string) {
	b.metadataMu.Lock()
	defer b.metadataMu.Unlock()
	delete(b.contentTypes, path)
}

func NormalizeRange(size int64, rng Range) (Range, error) {
	if size < 0 || rng.Start < 0 {
		return Range{}, ErrInvalidRange
	}
	if size == 0 {
		return Range{}, ErrInvalidRange
	}
	if rng.End < 0 || rng.End >= size {
		rng.End = size - 1
	}
	if rng.Start >= size || rng.Start > rng.End {
		return Range{}, ErrInvalidRange
	}
	return rng, nil
}

type listItem struct {
	key    string
	object *ObjectInfo
	prefix string
}

type readCloser struct {
	io.Reader
	io.Closer
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func normalizeContentType(contentType, key string) string {
	if contentType != "" {
		return contentType
	}
	if byExt := mime.TypeByExtension(filepath.Ext(key)); byExt != "" {
		return byExt
	}
	return "application/octet-stream"
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
