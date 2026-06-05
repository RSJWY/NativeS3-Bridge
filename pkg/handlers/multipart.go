package handlers

import (
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type MultipartHandler struct {
	store  *storage.MultipartStore
	commit UsageCommitter
}

func NewMultipartHandler(store *storage.MultipartStore, commit UsageCommitter) *MultipartHandler {
	return &MultipartHandler{store: store, commit: commit}
}

func (h *MultipartHandler) Create(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID, err := h.store.Create(bucket, key, r.Header.Get("Content-Type"), extractUserMetadata(r.Header), parseTaggingHeader(r.Header.Get("x-amz-tagging")))
	if err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	WriteXML(w, http.StatusOK, initiateMultipartUploadResult{
		XMLNS:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
	})
}

func (h *MultipartHandler) UploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil {
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	if err := h.store.ValidateTarget(uploadID, bucket, key); err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	etag, err := h.store.UploadPart(uploadID, partNumber, r.Body)
	if err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.Header().Set("ETag", quoteETag(etag))
	w.WriteHeader(http.StatusOK)
}

func (h *MultipartHandler) Complete(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var req completeMultipartUploadRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, r.URL.Path)
		return
	}
	parts := make([]storage.CompletedPart, 0, len(req.Parts))
	for _, part := range req.Parts {
		parts = append(parts, storage.CompletedPart{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	uploadID := r.URL.Query().Get("uploadId")
	if err := h.store.ValidateTarget(uploadID, bucket, key); err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	total, err := h.store.CompleteSize(uploadID, parts)
	if err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	if id, ok := auth.IdentityFromContext(r.Context()); !ok || id == nil {
		WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
		return
	} else if err := quota.Check(id, total); err != nil {
		if errors.Is(err, quota.ErrQuotaExceeded) {
			if abortErr := h.store.Abort(uploadID); abortErr != nil {
				slog.Warn("abort quota-exceeded multipart", "upload_id", uploadID, "error", abortErr)
			}
			WriteS3Error(w, "QuotaExceeded", http.StatusForbidden, r.URL.Path)
			return
		}
		WriteS3Error(w, auth.CodeAccessDenied, http.StatusForbidden, r.URL.Path)
		return
	}
	info, err := h.store.Complete(uploadID, parts)
	if err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	WriteXML(w, http.StatusOK, completeMultipartUploadResult{
		XMLNS:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: r.URL.Path,
		Bucket:   bucket,
		Key:      key,
		ETag:     quoteETag(info.ETag),
	})
	h.commitUsage(r, info.Size, quota.OpPut)
}

func (h *MultipartHandler) Abort(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if err := h.store.ValidateTarget(uploadID, bucket, key); err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	if err := h.store.Abort(uploadID); err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *MultipartHandler) ListParts(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	if err := h.store.ValidateTarget(uploadID, bucket, key); err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	parts, err := h.store.ListParts(uploadID)
	if err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	respParts := make([]listPartItem, 0, len(parts))
	for _, part := range parts {
		respParts = append(respParts, listPartItem{
			PartNumber:   part.PartNumber,
			LastModified: formatS3Time(part.LastModified),
			ETag:         quoteETag(part.ETag),
			Size:         part.Size,
		})
	}
	WriteXML(w, http.StatusOK, listPartsResult{
		XMLNS:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
		Parts:    respParts,
	})
}

func (h *MultipartHandler) ListUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	uploads, err := h.store.ListMultipartUploads(bucket, r.URL.Query().Get("prefix"))
	if err != nil {
		writeMultipartStorageError(w, err, r.URL.Path)
		return
	}
	items := make([]multipartUploadItem, 0, len(uploads))
	for _, upload := range uploads {
		items = append(items, multipartUploadItem{Key: upload.Key, UploadID: upload.UploadID, Initiated: formatS3Time(upload.CreatedAt)})
	}
	WriteXML(w, http.StatusOK, listMultipartUploadsResult{
		XMLNS:   "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:  bucket,
		Uploads: items,
	})
}

func (h *MultipartHandler) commitUsage(r *http.Request, deltaBytes int64, op quota.Op) {
	if h.commit == nil {
		return
	}
	id, ok := auth.IdentityFromContext(r.Context())
	if !ok || id == nil {
		return
	}
	if err := h.commit(id.CredentialID, deltaBytes, op); err != nil {
		slog.Warn("commit usage", "credential_id", id.CredentialID, "op", op, "delta_bytes", deltaBytes, "error", err)
	}
}

func writeMultipartStorageError(w http.ResponseWriter, err error, resource string) {
	switch {
	case errors.Is(err, storage.ErrNoSuchUpload):
		WriteS3Error(w, "NoSuchUpload", http.StatusNotFound, resource)
	case errors.Is(err, storage.ErrInvalidPartNumber):
		WriteS3Error(w, "InvalidArgument", http.StatusBadRequest, resource)
	case errors.Is(err, storage.ErrInvalidPartOrder):
		WriteS3Error(w, "InvalidPartOrder", http.StatusBadRequest, resource)
	case errors.Is(err, storage.ErrInvalidPart):
		WriteS3Error(w, "InvalidPart", http.StatusBadRequest, resource)
	default:
		writeStorageError(w, err, resource)
	}
}

func parseTaggingHeader(raw string) map[string]string {
	tags := map[string]string{}
	if raw == "" {
		return tags
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return tags
	}
	for key, value := range values {
		if key == "" || len(value) == 0 {
			continue
		}
		tags[key] = value[0]
	}
	return tags
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUploadRequest struct {
	XMLName xml.Name               `xml:"CompleteMultipartUpload"`
	Parts   []completedPartRequest `xml:"Part"`
}

type completedPartRequest struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type listPartsResult struct {
	XMLName  xml.Name       `xml:"ListPartsResult"`
	XMLNS    string         `xml:"xmlns,attr"`
	Bucket   string         `xml:"Bucket"`
	Key      string         `xml:"Key"`
	UploadID string         `xml:"UploadId"`
	Parts    []listPartItem `xml:"Part"`
}

type listPartItem struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type listMultipartUploadsResult struct {
	XMLName xml.Name              `xml:"ListMultipartUploadsResult"`
	XMLNS   string                `xml:"xmlns,attr"`
	Bucket  string                `xml:"Bucket"`
	Uploads []multipartUploadItem `xml:"Upload"`
}

type multipartUploadItem struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"`
}

func queryHas(r *http.Request, key string) bool {
	_, ok := r.URL.Query()[key]
	return ok
}

func hasUploadID(r *http.Request) bool {
	return strings.TrimSpace(r.URL.Query().Get("uploadId")) != ""
}
