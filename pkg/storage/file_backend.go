package storage

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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
	ErrNoSuchBucket      = errors.New("no such bucket")
	ErrNoSuchKey         = errors.New("no such key")
	ErrInvalidRange      = errors.New("invalid range")
	ErrBadDigest         = errors.New("bad digest")
	ErrInvalidDigest     = errors.New("invalid digest")
	ErrObjectConflict    = errors.New("object conflicts with existing directory or regular object")
	ErrInvalidObjectBody = errors.New("invalid object body")

	emptyETag = "d41d8cd98f00b204e9800998ecf8427e"
)

type FileBackend struct {
	root           string
	metadataSuffix string
	metadataMu     sync.RWMutex
	contentTypes   map[string]string
}

func NewFileBackend(root string) (*FileBackend, error) {
	return NewFileBackendWithMetadataSuffix(root, DefaultMetadataSuffix)
}

func NewFileBackendWithMetadataSuffix(root, metadataSuffix string) (*FileBackend, error) {
	if metadataSuffix == "" {
		metadataSuffix = DefaultMetadataSuffix
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(rootAbs, 0o755); err != nil {
		return nil, err
	}
	return &FileBackend{root: rootAbs, metadataSuffix: metadataSuffix, contentTypes: make(map[string]string)}, nil
}

func (b *FileBackend) PutObject(bucket, key string, r io.Reader, contentType string) (ObjectInfo, error) {
	return b.PutObjectWithOptions(bucket, key, r, PutObjectOptions{ContentType: contentType})
}

func (b *FileBackend) PutObjectWithMetadata(bucket, key string, r io.Reader, contentType string, metadata map[string]string) (ObjectInfo, error) {
	return b.PutObjectWithOptions(bucket, key, r, PutObjectOptions{ContentType: contentType, Metadata: metadata})
}

func (b *FileBackend) PutObjectWithOptions(bucket, key string, r io.Reader, opts PutObjectOptions) (ObjectInfo, error) {
	expectedSHA256, err := validatePutObjectOptions(opts)
	if err != nil {
		return ObjectInfo{}, err
	}
	target, isMarker, err := b.preparePutTarget(bucket, key)
	if err != nil {
		return ObjectInfo{}, err
	}

	resolvedContentType := normalizeContentType(opts.ContentType, key)
	meta := cloneStringMap(opts.Metadata)
	now := time.Now().UTC()

	if isMarker {
		h := md5.New()
		var sha256Hasher hash.Hash
		writers := []io.Writer{h}
		if expectedSHA256 != nil {
			sha256Hasher = sha256.New()
			writers = append(writers, sha256Hasher)
		}
		size, copyErr := io.Copy(io.MultiWriter(writers...), io.LimitReader(r, 1))
		if copyErr != nil {
			return ObjectInfo{}, copyErr
		}
		if size > 0 {
			return ObjectInfo{}, ErrInvalidObjectBody
		}
		computedMD5 := h.Sum(nil)
		if len(opts.ContentMD5) > 0 && !bytes.Equal(computedMD5, opts.ContentMD5) {
			return ObjectInfo{}, ErrBadDigest
		}
		if expectedSHA256 != nil && !bytes.Equal(sha256Hasher.Sum(nil), expectedSHA256) {
			return ObjectInfo{}, ErrBadDigest
		}
		_, statErr := os.Stat(target)
		createdDir := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !createdDir {
			return ObjectInfo{}, statErr
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return ObjectInfo{}, err
		}

		if err := WriteSidecar(target, b.metadataSuffix, Sidecar{
			ETag:        emptyETag,
			ContentType: resolvedContentType,
			Metadata:    meta,
			Tags:        map[string]string{},
			Size:        0,
			Directory:   true,
			UploadedAt:  now.Format(time.RFC3339),
		}); err != nil {
			if createdDir {
				_ = os.Remove(target)
			}
			return ObjectInfo{}, err
		}
		b.setContentType(target, resolvedContentType)
		return ObjectInfo{
			Key:          filepath.ToSlash(key),
			Size:         0,
			ETag:         emptyETag,
			LastModified: now,
			ContentType:  resolvedContentType,
			Metadata:     meta,
		}, nil
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
	writers := []io.Writer{f, h}
	var sha256Hasher hash.Hash
	if expectedSHA256 != nil {
		sha256Hasher = sha256.New()
		writers = append(writers, sha256Hasher)
	}
	size, copyErr := io.Copy(io.MultiWriter(writers...), r)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, firstErr(copyErr, syncErr, closeErr)
	}
	computedMD5 := h.Sum(nil)
	if len(opts.ContentMD5) > 0 && !bytes.Equal(computedMD5, opts.ContentMD5) {
		_ = os.Remove(tmp)
		return ObjectInfo{}, ErrBadDigest
	}
	if expectedSHA256 != nil && !bytes.Equal(sha256Hasher.Sum(nil), expectedSHA256) {
		_ = os.Remove(tmp)
		return ObjectInfo{}, ErrBadDigest
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, err
	}

	info, err := os.Stat(target)
	if err != nil {
		return ObjectInfo{}, err
	}
	etag := hex.EncodeToString(computedMD5)
	if err := WriteSidecar(target, b.metadataSuffix, Sidecar{
		ETag:        etag,
		ContentType: resolvedContentType,
		Metadata:    meta,
		Tags:        map[string]string{},
		Size:        size,
		UploadedAt:  now.Format(time.RFC3339),
	}); err != nil {
		return ObjectInfo{}, err
	}
	b.setContentType(target, resolvedContentType)
	return ObjectInfo{
		Key:          filepath.ToSlash(key),
		Size:         size,
		ETag:         etag,
		LastModified: info.ModTime().UTC(),
		ContentType:  resolvedContentType,
		Metadata:     meta,
	}, nil
}

func validatePutObjectOptions(opts PutObjectOptions) ([]byte, error) {
	if len(opts.ContentMD5) > 0 && len(opts.ContentMD5) != md5.Size {
		return nil, ErrInvalidDigest
	}
	if opts.ContentSHA256 == "" {
		return nil, nil
	}
	expectedSHA256, err := hex.DecodeString(opts.ContentSHA256)
	if err != nil || len(expectedSHA256) != sha256.Size {
		return nil, ErrInvalidDigest
	}
	return expectedSHA256, nil
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
	if isMarkerKey(key) {
		if !isDirectoryMarker(target, b.metadataSuffix) {
			if !isLegacyFileMarker(target, b.metadataSuffix) {
				return nil, ObjectInfo{}, ErrNoSuchKey
			}
		}
		if rng != nil {
			if _, err := NormalizeRange(info.Size, *rng); err != nil {
				return nil, ObjectInfo{}, err
			}
		}
		return io.NopCloser(strings.NewReader("")), info, nil
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

	if isMarkerKey(key) {
		info, err := os.Stat(target)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ObjectInfo{}, ErrNoSuchKey
			}
			return ObjectInfo{}, err
		}
		if !info.IsDir() {
			if !isLegacyFileMarker(target, b.metadataSuffix) {
				return ObjectInfo{}, ErrNoSuchKey
			}
			sidecar, exists, err := ReadSidecar(target, b.metadataSuffix)
			if err != nil {
				return ObjectInfo{}, err
			}
			contentType := b.contentTypeFor(target, key)
			metadata := map[string]string{}
			etag := ""
			if exists {
				etag = sidecar.ETag
				contentType = normalizeSidecarContentType(sidecar, contentType)
				metadata = cloneStringMap(sidecar.Metadata)
			}
			if etag == "" {
				etag, err = fileMD5(target)
				if err != nil {
					return ObjectInfo{}, err
				}
			}
			return ObjectInfo{Key: filepath.ToSlash(key), Size: 0, ETag: etag, LastModified: info.ModTime().UTC(), ContentType: contentType, Metadata: metadata}, nil
		}
		sidecar, exists, err := ReadSidecar(target, b.metadataSuffix)
		if err != nil {
			return ObjectInfo{}, err
		}
		if !exists || !sidecar.Directory {
			return ObjectInfo{}, ErrNoSuchKey
		}
		contentType := b.contentTypeFor(target, key)
		if sidecar.ContentType != "" {
			contentType = sidecar.ContentType
		}
		return ObjectInfo{
			Key:          filepath.ToSlash(key),
			Size:         0,
			ETag:         emptyETag,
			LastModified: info.ModTime().UTC(),
			ContentType:  contentType,
			Metadata:     cloneStringMap(sidecar.Metadata),
		}, nil
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
	sidecar, exists, err := ReadSidecar(target, b.metadataSuffix)
	if err != nil {
		return ObjectInfo{}, err
	}
	contentType := b.contentTypeFor(target, key)
	metadata := map[string]string{}
	etag := ""
	if exists {
		if sidecar.ETag != "" {
			etag = sidecar.ETag
		}
		if sidecar.ContentType != "" {
			contentType = sidecar.ContentType
		}
		metadata = cloneStringMap(sidecar.Metadata)
	}
	if etag == "" {
		etag, err = fileMD5(target)
		if err != nil {
			return ObjectInfo{}, err
		}
	}
	return ObjectInfo{
		Key:          filepath.ToSlash(key),
		Size:         info.Size(),
		ETag:         etag,
		LastModified: info.ModTime().UTC(),
		ContentType:  contentType,
		Metadata:     metadata,
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

	if isMarkerKey(key) {
		sidecarPath := markerSidecarPath(target, b.metadataSuffix)
		legacyFile := isLegacyFileMarker(target, b.metadataSuffix)
		if _, err := os.Stat(sidecarPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrNoSuchKey
			}
			return err
		}
		if err := os.Remove(sidecarPath); err != nil {
			return err
		}
		b.deleteContentType(target)
		if info, statErr := os.Stat(target); statErr == nil {
			if info.IsDir() {
				entries, err := os.ReadDir(target)
				if err == nil && len(entries) == 0 {
					_ = os.Remove(target)
				}
			} else if legacyFile {
				_ = os.Remove(target)
			}
		}
		return nil
	}

	if info, err := os.Stat(target); err == nil && info.IsDir() {
		return ErrNoSuchKey
	}

	err = os.Remove(target)
	if errors.Is(err, os.ErrNotExist) {
		b.deleteContentType(target)
		_ = DeleteSidecar(target, b.metadataSuffix)
		return nil
	}
	if err == nil {
		b.deleteContentType(target)
		if sidecarErr := DeleteSidecar(target, b.metadataSuffix); sidecarErr != nil {
			return sidecarErr
		}
	}
	return err
}

func (b *FileBackend) CopyObject(srcBucket, srcKey, dstBucket, dstKey string) (ObjectInfo, error) {
	srcInfo, err := b.HeadObject(srcBucket, srcKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	srcPath, err := ResolveObjectPath(b.root, srcBucket, srcKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	srcIsMarker := isMarkerKey(srcKey) && isDirectoryMarker(srcPath, b.metadataSuffix)

	dstPath, isMarker, err := b.preparePutTarget(dstBucket, dstKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := b.ensureBucketExists(dstBucket); err != nil {
		return ObjectInfo{}, err
	}

	sidecar, exists, err := ReadSidecar(srcPath, b.metadataSuffix)
	if err != nil {
		return ObjectInfo{}, err
	}
	if !exists {
		sidecar = Sidecar{
			ContentType: srcInfo.ContentType,
			Metadata:    cloneStringMap(srcInfo.Metadata),
			Tags:        map[string]string{},
		}
	}
	if sidecar.ContentType == "" {
		sidecar.ContentType = srcInfo.ContentType
	}
	if sidecar.Metadata == nil {
		sidecar.Metadata = map[string]string{}
	}
	if sidecar.Tags == nil {
		sidecar.Tags = map[string]string{}
	}

	contentType := normalizeContentType(sidecar.ContentType, dstKey)
	metadata := cloneStringMap(sidecar.Metadata)
	now := time.Now().UTC()

	if isMarker {
		if srcInfo.Size != 0 {
			return ObjectInfo{}, ErrInvalidObjectBody
		}
		if err := os.MkdirAll(dstPath, 0o755); err != nil {
			return ObjectInfo{}, err
		}
		if err := WriteSidecar(dstPath, b.metadataSuffix, Sidecar{
			ETag:        emptyETag,
			ContentType: contentType,
			Metadata:    metadata,
			Tags:        cloneStringMap(sidecar.Tags),
			Size:        0,
			Directory:   true,
			UploadedAt:  now.Format(time.RFC3339),
		}); err != nil {
			return ObjectInfo{}, err
		}
		b.setContentType(dstPath, contentType)
		return ObjectInfo{
			Key:          filepath.ToSlash(dstKey),
			Size:         0,
			ETag:         emptyETag,
			LastModified: now,
			ContentType:  contentType,
			Metadata:     metadata,
		}, nil
	}

	var in io.ReadCloser
	if srcIsMarker {
		in = io.NopCloser(strings.NewReader(""))
	} else {
		in, err = os.Open(srcPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ObjectInfo{}, ErrNoSuchKey
			}
			return ObjectInfo{}, err
		}
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return ObjectInfo{}, err
	}

	tmp := dstPath + ".tmp-" + randomHex(8)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return ObjectInfo{}, err
	}
	h := md5.New()
	size, copyErr := io.Copy(io.MultiWriter(out, h), in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, firstErr(copyErr, syncErr, closeErr)
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return ObjectInfo{}, err
	}

	stat, err := os.Stat(dstPath)
	if err != nil {
		return ObjectInfo{}, err
	}
	etag := hex.EncodeToString(h.Sum(nil))
	if err := WriteSidecar(dstPath, b.metadataSuffix, Sidecar{
		ETag:        etag,
		ContentType: contentType,
		Metadata:    metadata,
		Tags:        cloneStringMap(sidecar.Tags),
		Size:        size,
		UploadedAt:  now.Format(time.RFC3339),
	}); err != nil {
		return ObjectInfo{}, err
	}
	b.setContentType(dstPath, contentType)
	return ObjectInfo{
		Key:          filepath.ToSlash(dstKey),
		Size:         size,
		ETag:         etag,
		LastModified: stat.ModTime().UTC(),
		ContentType:  contentType,
		Metadata:     metadata,
	}, nil
}

func (b *FileBackend) PutObjectTags(bucket, key string, tags map[string]string) error {
	info, target, sidecar, err := b.sidecarForExistingObject(bucket, key)
	if err != nil {
		return err
	}
	sidecar.Tags = cloneStringMap(tags)
	if sidecar.UploadedAt == "" {
		sidecar.UploadedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if sidecar.ETag == "" {
		sidecar.ETag = info.ETag
	}
	if sidecar.ContentType == "" {
		sidecar.ContentType = info.ContentType
	}
	if sidecar.Size == 0 && !sidecar.Directory {
		sidecar.Size = info.Size
	}
	return WriteSidecar(target, b.metadataSuffix, sidecar)
}

func (b *FileBackend) GetObjectTags(bucket, key string) (map[string]string, error) {
	_, _, sidecar, err := b.sidecarForExistingObject(bucket, key)
	if err != nil {
		return nil, err
	}
	return cloneStringMap(sidecar.Tags), nil
}

func (b *FileBackend) DeleteObjectTags(bucket, key string) error {
	return b.PutObjectTags(bucket, key, map[string]string{})
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
			if p == bucketDir {
				return nil
			}
			if name == ".multipart" {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(bucketDir, p)
			if err != nil {
				return err
			}
			markerKey := filepath.ToSlash(rel) + "/"
			if strings.HasPrefix(markerKey, prefix) {
				if delimiter == "/" {
					remainder := strings.TrimPrefix(markerKey, prefix)
					if idx := strings.Index(remainder, "/"); idx >= 0 {
						common[prefix+remainder[:idx+1]] = struct{}{}
					} else if isDirectoryMarker(p, b.metadataSuffix) {
						obj, err := b.HeadObject(bucket, markerKey)
						if err != nil {
							return err
						}
						objects = append(objects, obj)
					}
				} else if isDirectoryMarker(p, b.metadataSuffix) {
					obj, err := b.HeadObject(bucket, markerKey)
					if err != nil {
						return err
					}
					objects = append(objects, obj)
				}
			}
			return nil
		}
		if strings.HasSuffix(name, b.metadataSuffix) || strings.HasSuffix(name, ".s3meta") || strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") {
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

func (b *FileBackend) WriteCompletedObject(bucket, key, tmpPath, etag, contentType string, metadata, tags map[string]string, size int64) (ObjectInfo, error) {
	target, err := ResolveObjectPath(b.root, bucket, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ObjectInfo{}, err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return ObjectInfo{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return ObjectInfo{}, err
	}
	resolvedContentType := normalizeContentType(contentType, key)
	meta := cloneStringMap(metadata)
	if err := WriteSidecar(target, b.metadataSuffix, Sidecar{
		ETag:        etag,
		ContentType: resolvedContentType,
		Metadata:    meta,
		Tags:        cloneStringMap(tags),
		Size:        size,
		UploadedAt:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return ObjectInfo{}, err
	}
	b.setContentType(target, resolvedContentType)
	return ObjectInfo{
		Key:          filepath.ToSlash(key),
		Size:         size,
		ETag:         etag,
		LastModified: info.ModTime().UTC(),
		ContentType:  resolvedContentType,
		Metadata:     meta,
	}, nil
}

func (b *FileBackend) sidecarForExistingObject(bucket, key string) (ObjectInfo, string, Sidecar, error) {
	info, err := b.HeadObject(bucket, key)
	if err != nil {
		return ObjectInfo{}, "", Sidecar{}, err
	}
	target, err := ResolveObjectPath(b.root, bucket, key)
	if err != nil {
		return ObjectInfo{}, "", Sidecar{}, err
	}
	sidecar, exists, err := ReadSidecar(target, b.metadataSuffix)
	if err != nil {
		return ObjectInfo{}, "", Sidecar{}, err
	}
	if !exists {
		sidecar = Sidecar{
			ETag:        info.ETag,
			ContentType: info.ContentType,
			Metadata:    cloneStringMap(info.Metadata),
			Tags:        map[string]string{},
			Size:        info.Size,
			UploadedAt:  time.Now().UTC().Format(time.RFC3339),
		}
	}
	if sidecar.Metadata == nil {
		sidecar.Metadata = map[string]string{}
	}
	if sidecar.Tags == nil {
		sidecar.Tags = map[string]string{}
	}
	return info, target, sidecar, nil
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

func (b *FileBackend) preparePutTarget(bucket, key string) (string, bool, error) {
	isMarker := isMarkerKey(key)
	resolveKey := key
	if isMarker {
		resolveKey = strings.TrimSuffix(key, "/")
		if resolveKey == "" {
			return "", false, ErrInvalidPath
		}
	}
	target, err := ResolveObjectPath(b.root, bucket, resolveKey)
	if err != nil {
		return "", false, err
	}
	bucketDir, err := ResolveBucketPath(b.root, bucket)
	if err != nil {
		return "", false, err
	}

	sep := string(filepath.Separator)
	for p := filepath.Dir(target); p != bucketDir && strings.HasPrefix(p, bucketDir+sep); p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err == nil && info.Mode().IsRegular() {
			return "", false, ErrObjectConflict
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}

	info, err := os.Stat(target)
	if err == nil {
		if isMarker && info.Mode().IsRegular() {
			return "", false, ErrObjectConflict
		}
		if !isMarker && info.IsDir() {
			return "", false, ErrObjectConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	return target, isMarker, nil
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

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func isMarkerKey(key string) bool { return strings.HasSuffix(key, "/") }

func markerSidecarPath(dirPath, suffix string) string {
	if suffix == "" {
		suffix = DefaultMetadataSuffix
	}
	return dirPath + suffix
}

func isDirectoryMarker(dirPath, suffix string) bool {
	sidecar, exists, err := ReadSidecar(dirPath, suffix)
	return err == nil && exists && sidecar.Directory
}

func isLegacyFileMarker(path, suffix string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		return false
	}
	sidecar, exists, err := ReadSidecar(path, suffix)
	return err == nil && exists && !sidecar.Directory
}

func normalizeSidecarContentType(sidecar Sidecar, fallback string) string {
	if sidecar.ContentType != "" {
		return sidecar.ContentType
	}
	return fallback
}
