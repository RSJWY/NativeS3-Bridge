package handlers

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/hooks"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type UsageCommitter func(credID uint, deltaBytes int64, op quota.Op) error

type ObjectHandler struct {
	backend storage.Backend
	commit  UsageCommitter
	hooks   EventEmitter
}

type EventEmitter interface {
	Emit(hooks.Event)
}

func NewObjectHandler(backend storage.Backend, commit UsageCommitter) *ObjectHandler {
	return NewObjectHandlerWithHooks(backend, commit, nil)
}

func NewObjectHandlerWithHooks(backend storage.Backend, commit UsageCommitter, emitter EventEmitter) *ObjectHandler {
	return &ObjectHandler{backend: backend, commit: commit, hooks: emitter}
}

func (h *ObjectHandler) Put(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var (
		info storage.ObjectInfo
		err  error
	)
	metadata := extractUserMetadata(r.Header)
	putOptions, err := parsePutObjectOptions(r, metadata)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	if backend, ok := h.backend.(interface {
		PutObjectWithOptions(string, string, io.Reader, storage.PutObjectOptions) (storage.ObjectInfo, error)
	}); ok {
		info, err = backend.PutObjectWithOptions(bucket, key, r.Body, putOptions)
	} else if backend, ok := h.backend.(interface {
		PutObjectWithMetadata(string, string, io.Reader, string, map[string]string) (storage.ObjectInfo, error)
	}); ok {
		info, err = backend.PutObjectWithMetadata(bucket, key, r.Body, r.Header.Get("Content-Type"), metadata)
	} else {
		info, err = h.backend.PutObject(bucket, key, r.Body, r.Header.Get("Content-Type"))
	}
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.WriteHeader(http.StatusOK)
	h.commitUsage(r, info.Size, quota.OpPut)
	h.emitObjectEvent(r, hooks.ObjectCreated, bucket, key, info)
}

func (h *ObjectHandler) Get(w http.ResponseWriter, r *http.Request, bucket, key string) {
	rng, hasRange, err := parseRangeHeader(r.Header.Get("Range"))
	if err != nil {
		WriteS3Error(w, "InvalidRange", http.StatusRequestedRangeNotSatisfiable, r.URL.Path)
		return
	}
	rc, info, err := h.backend.GetObject(bucket, key, rng)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	defer rc.Close()

	SetStandardHeaders(w)
	writeObjectHeaders(w, info)
	status := http.StatusOK
	length := info.Size
	if hasRange && rng != nil {
		normalized, err := storage.NormalizeRange(info.Size, *rng)
		if err != nil {
			WriteS3Error(w, "InvalidRange", http.StatusRequestedRangeNotSatisfiable, r.URL.Path)
			return
		}
		length = normalized.End - normalized.Start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", normalized.Start, normalized.End, info.Size))
		status = http.StatusPartialContent
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	written, copyErr := io.Copy(w, rc)
	if copyErr != nil {
		slog.Warn("stream object", "bucket", bucket, "key", key, "error", copyErr)
		return
	}
	h.commitUsage(r, written, quota.OpGet)
}

func (h *ObjectHandler) Head(w http.ResponseWriter, r *http.Request, bucket, key string) {
	info, err := h.backend.HeadObject(bucket, key)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	writeObjectHeaders(w, info)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func (h *ObjectHandler) Delete(w http.ResponseWriter, r *http.Request, bucket, key string) {
	deletedSize := int64(0)
	deleted := false
	info, headErr := h.backend.HeadObject(bucket, key)
	if headErr != nil && !errors.Is(headErr, storage.ErrNoSuchKey) {
		writeStorageError(w, headErr, r.URL.Path)
		return
	}
	if headErr == nil {
		deletedSize = info.Size
		deleted = true
	}
	if err := h.backend.DeleteObject(bucket, key); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.WriteHeader(http.StatusNoContent)
	h.commitUsage(r, -deletedSize, quota.OpDelete)
	if deleted {
		h.emitObjectEvent(r, hooks.ObjectDeleted, bucket, key, storage.ObjectInfo{Key: key, Size: deletedSize, ETag: info.ETag, Metadata: info.Metadata})
	}
}

func (h *ObjectHandler) DeleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var req deleteObjectsRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
		return
	}
	if len(req.Objects) == 0 {
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
		return
	}
	resp := deleteObjectsResult{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, obj := range req.Objects {
		if obj.Key == "" {
			WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
			return
		}
		info, headErr := h.backend.HeadObject(bucket, obj.Key)
		if headErr != nil {
			if !errors.Is(headErr, storage.ErrNoSuchKey) {
				writeStorageError(w, headErr, r.URL.Path)
				return
			}
		} else {
			if err := h.backend.DeleteObject(bucket, obj.Key); err != nil {
				writeStorageError(w, err, r.URL.Path)
				return
			}
			h.commitUsage(r, -info.Size, quota.OpDelete)
			h.emitObjectEvent(r, hooks.ObjectDeleted, bucket, obj.Key, storage.ObjectInfo{Key: obj.Key, Size: info.Size, ETag: info.ETag, Metadata: info.Metadata})
		}
		if !req.Quiet {
			resp.Deleted = append(resp.Deleted, deletedObject{Key: obj.Key})
		}
	}
	WriteXML(w, http.StatusOK, resp)
}

func (h *ObjectHandler) Copy(w http.ResponseWriter, r *http.Request, dstBucket, dstKey string) {
	srcBucket, srcKey, err := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if err != nil {
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
		return
	}
	srcInfo, err := h.backend.HeadObject(srcBucket, srcKey)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	if _, err := h.backend.ListObjects(dstBucket, "", "", "", 0); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	if id, ok := auth.IdentityFromContext(r.Context()); !ok || id == nil {
		WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
		return
	} else if err := quota.Check(id, srcInfo.Size); err != nil {
		if errors.Is(err, quota.ErrQuotaExceeded) {
			WriteS3Error(w, "QuotaExceeded", http.StatusForbidden, r.URL.Path)
			return
		}
		WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
		return
	}
	backend, ok := h.backend.(interface {
		CopyObject(string, string, string, string) (storage.ObjectInfo, error)
	})
	if !ok {
		WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
		return
	}
	info, err := backend.CopyObject(srcBucket, srcKey, dstBucket, dstKey)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	WriteXML(w, http.StatusOK, copyObjectResult{
		XMLNS:        "http://s3.amazonaws.com/doc/2006-03-01/",
		LastModified: formatS3Time(info.LastModified),
		ETag:         quoteETag(info.ETag),
	})
	h.commitUsage(r, info.Size, quota.OpPut)
	h.emitObjectEvent(r, hooks.ObjectCreated, dstBucket, dstKey, info)
}

func (h *ObjectHandler) PutTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var req taggingRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
		return
	}
	tags := make(map[string]string, len(req.TagSet.Tags))
	for _, tag := range req.TagSet.Tags {
		if tag.Key == "" {
			WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
			return
		}
		tags[tag.Key] = tag.Value
	}
	backend, ok := h.backend.(interface {
		PutObjectTags(string, string, map[string]string) error
	})
	if !ok {
		WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
		return
	}
	if err := backend.PutObjectTags(bucket, key, tags); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.WriteHeader(http.StatusOK)
}

func (h *ObjectHandler) GetTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	backend, ok := h.backend.(interface {
		GetObjectTags(string, string) (map[string]string, error)
	})
	if !ok {
		WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
		return
	}
	tags, err := backend.GetObjectTags(bucket, key)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	resp := taggingRequest{TagSet: tagSet{Tags: make([]tagEntry, 0, len(tags))}}
	for key, value := range tags {
		resp.TagSet.Tags = append(resp.TagSet.Tags, tagEntry{Key: key, Value: value})
	}
	WriteXML(w, http.StatusOK, resp)
}

func (h *ObjectHandler) DeleteTagging(w http.ResponseWriter, r *http.Request, bucket, key string) {
	backend, ok := h.backend.(interface {
		DeleteObjectTags(string, string) error
	})
	if !ok {
		WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
		return
	}
	if err := backend.DeleteObjectTags(bucket, key); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *ObjectHandler) ListObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	maxKeys := 1000
	if raw := query.Get("max-keys"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
			return
		}
		maxKeys = parsed
	}

	result, err := h.backend.ListObjects(bucket, query.Get("prefix"), query.Get("delimiter"), query.Get("continuation-token"), maxKeys)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	contents := make([]listObjectContent, 0, len(result.Objects))
	for _, obj := range result.Objects {
		contents = append(contents, listObjectContent{
			Key:          obj.Key,
			LastModified: formatS3Time(obj.LastModified),
			ETag:         quoteETag(obj.ETag),
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	prefixes := make([]commonPrefix, 0, len(result.CommonPrefixes))
	for _, prefix := range result.CommonPrefixes {
		prefixes = append(prefixes, commonPrefix{Prefix: prefix})
	}
	WriteXML(w, http.StatusOK, listBucketResult{
		XMLNS:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:                  bucket,
		Prefix:                query.Get("prefix"),
		Delimiter:             query.Get("delimiter"),
		MaxKeys:               maxKeys,
		KeyCount:              len(contents) + len(prefixes),
		IsTruncated:           result.IsTruncated,
		NextContinuationToken: result.NextToken,
		Contents:              contents,
		CommonPrefixes:        prefixes,
	})
}

type listBucketResult struct {
	XMLName               struct{}            `xml:"ListBucketResult"`
	XMLNS                 string              `xml:"xmlns,attr"`
	Name                  string              `xml:"Name"`
	Prefix                string              `xml:"Prefix"`
	Delimiter             string              `xml:"Delimiter,omitempty"`
	MaxKeys               int                 `xml:"MaxKeys"`
	KeyCount              int                 `xml:"KeyCount"`
	IsTruncated           bool                `xml:"IsTruncated"`
	NextContinuationToken string              `xml:"NextContinuationToken,omitempty"`
	Contents              []listObjectContent `xml:"Contents"`
	CommonPrefixes        []commonPrefix      `xml:"CommonPrefixes"`
}

type listObjectContent struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type deleteObjectsRequest struct {
	XMLName xml.Name              `xml:"Delete"`
	Objects []deleteObjectRequest `xml:"Object"`
	Quiet   bool                  `xml:"Quiet"`
}

type deleteObjectRequest struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
}

type deleteObjectsResult struct {
	XMLName struct{}        `xml:"DeleteResult"`
	XMLNS   string          `xml:"xmlns,attr"`
	Deleted []deletedObject `xml:"Deleted,omitempty"`
}

type deletedObject struct {
	Key string `xml:"Key"`
}

type copyObjectResult struct {
	XMLName      struct{} `xml:"CopyObjectResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type taggingRequest struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  tagSet   `xml:"TagSet"`
}

type tagSet struct {
	Tags []tagEntry `xml:"Tag"`
}

type tagEntry struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func writeObjectHeaders(w http.ResponseWriter, info storage.ObjectInfo) {
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")
	for key, value := range info.Metadata {
		if key == "" {
			continue
		}
		w.Header().Set("x-amz-meta-"+key, value)
	}
}

func extractUserMetadata(header http.Header) map[string]string {
	metadata := map[string]string{}
	for key, values := range header {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "x-amz-meta-") || len(values) == 0 {
			continue
		}
		metadata[strings.TrimPrefix(lower, "x-amz-meta-")] = values[0]
	}
	return metadata
}

func parsePutObjectOptions(r *http.Request, metadata map[string]string) (storage.PutObjectOptions, error) {
	opts := storage.PutObjectOptions{
		ContentType: r.Header.Get("Content-Type"),
		Metadata:    metadata,
	}
	if raw := r.Header.Get("Content-MD5"); raw != "" {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(decoded) != md5DigestSize {
			return storage.PutObjectOptions{}, storage.ErrInvalidDigest
		}
		opts.ContentMD5 = decoded
	}
	if raw := r.Header.Get("x-amz-content-sha256"); raw != "" && isConcretePayloadSHA256(raw) {
		opts.ContentSHA256 = strings.ToLower(raw)
	} else if raw != "" && !isIgnoredPayloadSHA256(raw) {
		return storage.PutObjectOptions{}, storage.ErrInvalidDigest
	}
	return opts, nil
}

func isConcretePayloadSHA256(value string) bool {
	if len(value) != sha256HexSize {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isIgnoredPayloadSHA256(value string) bool {
	switch value {
	case "UNSIGNED-PAYLOAD", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER":
		return true
	default:
		return false
	}
}

func parseRangeHeader(header string) (*storage.Range, bool, error) {
	if header == "" {
		return nil, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return nil, true, storage.ErrInvalidRange
	}
	parts := strings.Split(strings.TrimPrefix(header, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" {
		return nil, true, storage.ErrInvalidRange
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, true, err
	}
	end := int64(-1)
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, true, err
		}
	}
	return &storage.Range{Start: start, End: end}, true, nil
}

func parseCopySource(raw string) (string, string, error) {
	if raw == "" {
		return "", "", storage.ErrInvalidPath
	}
	trimmed := strings.TrimPrefix(raw, "/")
	if idx := strings.Index(trimmed, "?"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", storage.ErrInvalidPath
	}
	bucket, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", err
	}
	key, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", "", err
	}
	if bucket == "" || key == "" {
		return "", "", storage.ErrInvalidPath
	}
	return bucket, key, nil
}

func writeStorageError(w http.ResponseWriter, err error, resource string) {
	switch {
	case errors.Is(err, storage.ErrInvalidBucketName):
		WriteS3Error(w, "InvalidBucketName", http.StatusBadRequest, resource)
	case errors.Is(err, storage.ErrInvalidPath):
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, resource)
	case errors.Is(err, storage.ErrNoSuchBucket):
		WriteS3Error(w, "NoSuchBucket", http.StatusNotFound, resource)
	case errors.Is(err, storage.ErrNoSuchKey):
		WriteS3Error(w, "NoSuchKey", http.StatusNotFound, resource)
	case errors.Is(err, storage.ErrInvalidRange):
		WriteS3Error(w, "InvalidRange", http.StatusRequestedRangeNotSatisfiable, resource)
	case errors.Is(err, storage.ErrBadDigest):
		WriteS3Error(w, "BadDigest", http.StatusBadRequest, resource)
	case errors.Is(err, storage.ErrInvalidDigest):
		WriteS3Error(w, "InvalidDigest", http.StatusBadRequest, resource)
	case errors.Is(err, storage.ErrBucketNotEmpty):
		WriteS3Error(w, "BucketNotEmpty", http.StatusConflict, resource)
	default:
		WriteS3Error(w, "InternalError", http.StatusInternalServerError, resource)
	}
}

const (
	md5DigestSize = 16
	sha256HexSize = 64
)

func (h *ObjectHandler) commitUsage(r *http.Request, deltaBytes int64, op quota.Op) {
	if h.commit == nil {
		return
	}
	id, ok := auth.IdentityFromContext(r.Context())
	if !ok || id == nil {
		return
	}
	if auth.IsAnonymous(id) {
		return
	}
	if err := h.commit(id.CredentialID, deltaBytes, op); err != nil {
		slog.Warn("commit usage", "credential_id", id.CredentialID, "op", op, "delta_bytes", deltaBytes, "error", err)
	}
}

func (h *ObjectHandler) emitObjectEvent(r *http.Request, eventType hooks.EventType, bucket, key string, info storage.ObjectInfo) {
	if h.hooks == nil {
		return
	}
	id, ok := auth.IdentityFromContext(r.Context())
	if !ok || id == nil {
		return
	}
	h.hooks.Emit(hooks.Event{
		Type:         eventType,
		Bucket:       bucket,
		Key:          key,
		Size:         info.Size,
		ETag:         info.ETag,
		Metadata:     info.Metadata,
		CredentialID: id.CredentialID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
}

func quoteETag(etag string) string {
	if strings.HasPrefix(etag, "\"") && strings.HasSuffix(etag, "\"") {
		return etag
	}
	return "\"" + etag + "\""
}

func formatS3Time(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
