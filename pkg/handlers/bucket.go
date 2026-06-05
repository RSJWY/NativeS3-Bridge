package handlers

import (
	"errors"
	"net/http"

	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type BucketHandler struct {
	backend storage.Backend
}

func NewBucketHandler(backend storage.Backend) *BucketHandler {
	return &BucketHandler{backend: backend}
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
