package handlers

import (
	"errors"
	"net/http"

	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type BucketHandler struct {
	backend     storage.Backend
	bucketStore *storage.BucketStore
}

func NewBucketHandler(backend storage.Backend, bucketStore *storage.BucketStore) *BucketHandler {
	return &BucketHandler{backend: backend, bucketStore: bucketStore}
}

func (h *BucketHandler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := h.backend.ListBuckets()
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	items := make([]bucketItem, 0, len(buckets))
	for _, bucket := range buckets {
		items = append(items, bucketItem{Name: bucket.Name, CreationDate: formatS3Time(bucket.CreationDate)})
	}
	WriteXML(w, http.StatusOK, listAllMyBucketsResult{
		XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/",
		Buckets: bucketsContainer{
			Buckets: items,
		},
	})
}

func (h *BucketHandler) HeadBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	_, err := h.backend.ListObjects(bucket, "", "", "", 1)
	if err != nil {
		if errors.Is(err, storage.ErrNoSuchBucket) {
			WriteS3Error(w, "NoSuchBucket", http.StatusNotFound, r.URL.Path)
			return
		}
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.WriteHeader(http.StatusOK)
}

func (h *BucketHandler) GetBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := h.ensureBucketExists(bucket, r.URL.Path); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	WriteXML(w, http.StatusOK, locationConstraint{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/"})
}

func (h *BucketHandler) GetBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := h.ensureBucketExists(bucket, r.URL.Path); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	WriteXML(w, http.StatusOK, versioningConfiguration{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/"})
}

func (h *BucketHandler) CreateBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := storage.ValidateBucketName(bucket); err != nil {
		WriteS3Error(w, "InvalidBucketName", http.StatusBadRequest, r.URL.Path)
		return
	}
	if err := h.bucketStore.Create(bucket); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.WriteHeader(http.StatusOK)
}

func (h *BucketHandler) DeleteBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	listed, err := h.backend.ListObjects(bucket, "", "", "", 1)
	if err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	if len(listed.Objects) > 0 || len(listed.CommonPrefixes) > 0 {
		WriteS3Error(w, "BucketNotEmpty", http.StatusConflict, r.URL.Path)
		return
	}
	if err := h.bucketStore.Delete(bucket); err != nil {
		writeStorageError(w, err, r.URL.Path)
		return
	}
	SetStandardHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *BucketHandler) ensureBucketExists(bucket, resource string) error {
	_, err := h.backend.ListObjects(bucket, "", "", "", 0)
	return err
}

type listAllMyBucketsResult struct {
	XMLName struct{}         `xml:"ListAllMyBucketsResult"`
	XMLNS   string           `xml:"xmlns,attr"`
	Owner   owner            `xml:"Owner"`
	Buckets bucketsContainer `xml:"Buckets"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketsContainer struct {
	Buckets []bucketItem `xml:"Bucket"`
}

type bucketItem struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type locationConstraint struct {
	XMLName struct{} `xml:"LocationConstraint"`
	XMLNS   string   `xml:"xmlns,attr"`
}

type versioningConfiguration struct {
	XMLName struct{} `xml:"VersioningConfiguration"`
	XMLNS   string   `xml:"xmlns,attr"`
}
