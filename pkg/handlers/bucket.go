package handlers

import (
	"encoding/xml"
	"errors"
	"net/http"

	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type BucketHandler struct {
	backend                storage.Backend
	bucketStore            *storage.BucketStore
	boundCredentialChecker func(bucket string) (bool, error)
	managed                bool
}

func NewManagedBucketHandler(backend storage.Backend, bucketStore *storage.BucketStore) *BucketHandler {
	return &BucketHandler{backend: backend, bucketStore: bucketStore, managed: true}
}

func NewBucketHandler(backend storage.Backend, bucketStore *storage.BucketStore) *BucketHandler {
	return &BucketHandler{backend: backend, bucketStore: bucketStore}
}

func NewBucketHandlerWithCredentialChecker(backend storage.Backend, bucketStore *storage.BucketStore, checker func(bucket string) (bool, error)) *BucketHandler {
	return &BucketHandler{backend: backend, bucketStore: bucketStore, boundCredentialChecker: checker}
}

func (h *BucketHandler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	if h.managed {
		managed, err := h.bucketStore.List()
		if err != nil {
			writeStorageError(w, err, r.URL.Path)
			return
		}
		items := make([]bucketItem, 0, len(managed))
		for _, bucket := range managed {
			items = append(items, bucketItem{Name: bucket.Name, CreationDate: formatS3Time(bucket.CreatedAt)})
		}
		WriteXML(w, http.StatusOK, listAllMyBucketsResult{
			XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/", Buckets: bucketsContainer{Buckets: items},
		})
		return
	}
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
	if h.managed {
		if err := h.ensureBucketExists(bucket, r.URL.Path); err != nil {
			writeStorageError(w, err, r.URL.Path)
			return
		}
		SetStandardHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}
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
	if h.managed {
		WriteS3Error(w, "AccessDenied", http.StatusForbidden, r.URL.Path)
		return
	}
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
	if h.managed {
		WriteS3Error(w, "AccessDenied", http.StatusForbidden, r.URL.Path)
		return
	}
	if h.boundCredentialChecker != nil {
		bound, err := h.boundCredentialChecker(bucket)
		if err != nil {
			WriteS3Error(w, "InternalError", http.StatusInternalServerError, r.URL.Path)
			return
		}
		if bound {
			WriteS3Error(w, "BucketNotEmpty", http.StatusConflict, r.URL.Path)
			return
		}
	}
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
	if h.managed {
		_, exists, err := h.bucketStore.GetACL(bucket)
		if err != nil {
			return err
		}
		if !exists {
			return storage.ErrNoSuchBucket
		}
		return nil
	}
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
	XMLName  xml.Name `xml:"LocationConstraint"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:",chardata"`
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	XMLNS   string   `xml:"xmlns,attr"`
	Status  string   `xml:"Status,omitempty"`
}
