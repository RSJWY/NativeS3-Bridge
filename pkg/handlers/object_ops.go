package handlers

import (
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/hooks"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

// IsCopyRequest reports whether a PUT object request is a server-side copy
// (carries the x-amz-copy-source header) rather than a body upload.
func IsCopyRequest(r *http.Request) bool {
	return r.Header.Get("x-amz-copy-source") != ""
}

// Copy implements PUT Object - Copy (server-side copy). The source is given by
// the x-amz-copy-source header; metadata is copied from the source unless
// x-amz-metadata-directive is REPLACE.
func (h *ObjectHandler) Copy(w http.ResponseWriter, r *http.Request, bucket, key string) {
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

	// Copying onto the exact same object is only valid when metadata is being
	// replaced; otherwise it is a no-op error per S3 semantics.
	replaceMeta := strings.EqualFold(r.Header.Get("x-amz-metadata-directive"), "REPLACE")
	if srcBucket == bucket && srcKey == key && !replaceMeta {
		WriteS3Error(w, "InvalidRequest", http.StatusBadRequest, r.URL.Path)
		return
	}

	id, ok := auth.IdentityFromContext(r.Context())
	if !ok || id == nil || auth.IsAnonymous(id) {
		WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
		return
	}
	var reservation *quota.Reservation
	settled := false
	replacedBytes := int64(0)
	if existing, headErr := h.backend.HeadObject(bucket, key); headErr == nil {
		replacedBytes = existing.Size
	} else if !errors.Is(headErr, storage.ErrNoSuchKey) && !errors.Is(headErr, storage.ErrNoSuchBucket) {
		writeStorageError(w, headErr, r.URL.Path)
		return
	}
	reserveBytes := srcInfo.Size - replacedBytes
	if reserveBytes < 0 {
		reserveBytes = 0
	}
	if h.quota != nil {
		reservation, err = h.quota.Reserve(id.CredentialID, reserveBytes)
	} else {
		err = quota.Check(id, srcInfo.Size)
	}
	if err != nil {
		if errors.Is(err, quota.ErrQuotaExceeded) {
			WriteS3Error(w, "QuotaExceeded", http.StatusForbidden, r.URL.Path)
		} else {
			WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
		}
		return
	}
	if reservation != nil {
		defer func() {
			if !settled {
				if releaseErr := h.quota.Release(reservation); releaseErr != nil {
					slog.Warn("release copy quota reservation", "credential_id", reservation.CredentialID, "bytes", reservation.Bytes, "error", releaseErr)
				}
			}
		}()
	}

	contentType := srcInfo.ContentType
	metadata := srcInfo.Metadata
	if replaceMeta {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			contentType = ct
		}
		metadata = extractUserMetadata(r.Header)
	}

	rc, _, err := h.backend.GetObject(srcBucket, srcKey, nil)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	defer rc.Close()

	var info storage.ObjectInfo
	if backend, ok := h.backend.(interface {
		PutObjectWithOptions(string, string, io.Reader, storage.PutObjectOptions) (storage.ObjectInfo, error)
	}); ok {
		info, err = backend.PutObjectWithOptions(bucket, key, rc, storage.PutObjectOptions{ContentType: contentType, Metadata: metadata})
	} else {
		info, err = h.backend.PutObject(bucket, key, rc, contentType)
	}
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	if reservation != nil {
		if err := h.quota.Settle(reservation, info.Size, replacedBytes, quota.OpPut); err != nil {
			settled = true
			WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
			return
		}
		settled = true
	}
	if tagBackend, ok := h.backend.(interface {
		GetObjectTags(string, string) (map[string]string, error)
		PutObjectTags(string, string, map[string]string) error
	}); ok {
		tags, err := tagBackend.GetObjectTags(srcBucket, srcKey)
		if err != nil {
			writeStorageError(w, err, r.URL.Path)
			return
		}
		if err := tagBackend.PutObjectTags(bucket, key, tags); err != nil {
			writeStorageError(w, err, r.URL.Path)
			return
		}
	}

	if reservation == nil {
		h.commitUsage(r, info.Size, quota.OpPut)
	}
	h.emitObjectEvent(r, hooks.ObjectCreated, bucket, key, info)
	WriteXML(w, http.StatusOK, copyObjectResult{
		ETag:         quoteETag(info.ETag),
		LastModified: formatS3Time(info.LastModified),
	})
}

// DeleteObjects implements POST /{bucket}?delete (multi-object delete).
func (h *ObjectHandler) DeleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var req deleteRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
		return
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		WriteS3Error(w, "MalformedXML", http.StatusBadRequest, r.URL.Path)
		return
	}
	if len(req.Objects) == 0 {
		WriteS3Error(w, "MalformedXML", http.StatusBadRequest, r.URL.Path)
		return
	}

	result := deleteResult{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, obj := range req.Objects {
		if obj.Key == "" {
			result.Errors = append(result.Errors, deleteError{Key: obj.Key, Code: "InvalidArgument", Message: "missing key"})
			continue
		}
		// Capture size before deletion so quota usage can be decremented.
		var deletedSize int64
		if info, headErr := h.backend.HeadObject(bucket, obj.Key); headErr == nil {
			deletedSize = info.Size
		} else if !errors.Is(headErr, storage.ErrNoSuchKey) {
			result.Errors = append(result.Errors, deleteError{Key: obj.Key, Code: deleteErrorCode(headErr), Message: "delete failed"})
			continue
		}
		if err := h.backend.DeleteObject(bucket, obj.Key); err != nil {
			result.Errors = append(result.Errors, deleteError{Key: obj.Key, Code: deleteErrorCode(err), Message: "delete failed"})
			continue
		}
		if deletedSize > 0 {
			h.commitUsage(r, -deletedSize, quota.OpDelete)
			h.emitObjectEvent(r, hooks.ObjectDeleted, bucket, obj.Key, storage.ObjectInfo{Key: obj.Key, Size: deletedSize})
		}
		if !req.Quiet {
			result.Deleted = append(result.Deleted, deletedObject{Key: obj.Key})
		}
	}
	WriteXML(w, http.StatusOK, result)
}

func parseCopySource(header string) (bucket, key string, err error) {
	if header == "" {
		return "", "", storage.ErrInvalidPath
	}
	// Strip optional leading slash and any ?versionId suffix.
	trimmed := strings.TrimPrefix(header, "/")
	if idx := strings.Index(trimmed, "?"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	decoded, decErr := url.QueryUnescape(trimmed)
	if decErr == nil {
		trimmed = decoded
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", storage.ErrInvalidPath
	}
	return parts[0], parts[1], nil
}

func deleteErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrNoSuchBucket):
		return "NoSuchBucket"
	case errors.Is(err, storage.ErrInvalidPath):
		return "InvalidArgument"
	default:
		return "InternalError"
	}
}

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

type deleteRequest struct {
	XMLName xml.Name         `xml:"Delete"`
	Quiet   bool             `xml:"Quiet"`
	Objects []deleteReqEntry `xml:"Object"`
}

type deleteReqEntry struct {
	Key string `xml:"Key"`
}

type deleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	XMLNS   string          `xml:"xmlns,attr"`
	Deleted []deletedObject `xml:"Deleted"`
	Errors  []deleteError   `xml:"Error"`
}

type deletedObject struct {
	Key string `xml:"Key"`
}

type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}
