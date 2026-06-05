package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type ObjectHandler struct {
	backend storage.Backend
}

func NewObjectHandler(backend storage.Backend) *ObjectHandler {
	return &ObjectHandler{backend: backend}
}

func (h *ObjectHandler) Put(w http.ResponseWriter, r *http.Request, bucket, key string) {
	info, err := h.backend.PutObject(bucket, key, r.Body, r.Header.Get("Content-Type"))
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.WriteHeader(http.StatusOK)
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
	_, _ = io.Copy(w, rc)
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
	if err := h.backend.DeleteObject(bucket, key); err != nil {
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

func writeObjectHeaders(w http.ResponseWriter, info storage.ObjectInfo) {
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")
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
	default:
		WriteS3Error(w, "InternalError", http.StatusInternalServerError, resource)
	}
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
