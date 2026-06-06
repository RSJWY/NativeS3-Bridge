package handlers

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	if backend, ok := h.backend.(interface {
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
	case errors.Is(err, storage.ErrBucketNotEmpty):
		WriteS3Error(w, "BucketNotEmpty", http.StatusConflict, resource)
	default:
		WriteS3Error(w, "InternalError", http.StatusInternalServerError, resource)
	}
}

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
