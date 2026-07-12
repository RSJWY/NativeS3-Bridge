package webadmin

import (
	"errors"
	"net/http"

	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

type reconcileRequest struct {
	Apply bool `json:"apply"`
}

type reconcileCredential struct {
	ID        uint   `json:"id"`
	AccessKey string `json:"access_key"`
	Name      string `json:"name"`
	UsedBytes int64  `json:"used_bytes"`
	DiffBytes int64  `json:"diff_bytes"`
	Updated   bool   `json:"updated"`
}

type reconcileResponse struct {
	Bucket               string                `json:"bucket"`
	Apply                bool                  `json:"apply"`
	ObjectCount          int64                 `json:"object_count"`
	ScannedBytes         int64                 `json:"scanned_bytes"`
	OrphanSidecarCount   int                   `json:"orphan_sidecar_count"`
	OrphanSidecarSamples []string              `json:"orphan_sidecar_samples"`
	BoundCredentials     []reconcileCredential `json:"bound_credentials"`
	OrphansDeleted       int                   `json:"orphans_deleted"`
	CredentialsUpdated   int                   `json:"credentials_updated"`
}

func (a *API) reconcileBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if a.dataRoot == "" {
		writeJSONError(w, http.StatusInternalServerError, "storage reconcile is not configured")
		return
	}
	var request reconcileRequest
	if err := decodeJSON(r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := storage.ResolveBucketPath(a.dataRoot, bucket); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid bucket name")
		return
	}
	var count int64
	if err := a.db.Model(&dbpkg.Bucket{}).Where("name = ?", bucket).Count(&count).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query bucket failed")
		return
	}
	if count == 0 {
		writeJSONError(w, http.StatusNotFound, "bucket not found")
		return
	}
	report, err := storage.ReconcileBucket(a.dataRoot, bucket, a.metadataSuffix)
	if err != nil {
		if errors.Is(err, storage.ErrInvalidBucketName) {
			writeJSONError(w, http.StatusBadRequest, "invalid bucket name")
		} else if errors.Is(err, storage.ErrNoSuchBucket) {
			writeJSONError(w, http.StatusNotFound, "bucket not found")
		} else {
			writeJSONError(w, http.StatusInternalServerError, "scan bucket failed")
		}
		return
	}
	response := reconcileResponse{Bucket: bucket, Apply: request.Apply, ObjectCount: report.ObjectCount, ScannedBytes: report.ScannedBytes, OrphanSidecarCount: report.OrphanSidecarCount(), OrphanSidecarSamples: report.OrphanSidecars, BoundCredentials: []reconcileCredential{}}
	var credentials []dbpkg.Credential
	if err := a.db.Where("bucket = ? AND bucket <> ''", bucket).Order("id ASC").Find(&credentials).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query credentials failed")
		return
	}
	if request.Apply {
		deleted, deleteErr := report.DeleteOrphanSidecars()
		if deleteErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "delete orphan sidecars failed")
			return
		}
		response.OrphansDeleted = deleted
		if err := a.db.Transaction(func(tx *gorm.DB) error {
			for index := range credentials {
				if err := tx.Model(&credentials[index]).Update("used_bytes", report.ScannedBytes).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "update credentials failed")
			return
		}
		response.CredentialsUpdated = len(credentials)
		for index := range credentials {
			credentials[index].UsedBytes = report.ScannedBytes
			if a.invalidator != nil {
				a.invalidator.Invalidate(credentials[index].AccessKey)
			}
		}
	}
	for _, credential := range credentials {
		diff := credential.UsedBytes - report.ScannedBytes
		response.BoundCredentials = append(response.BoundCredentials, reconcileCredential{ID: credential.ID, AccessKey: credential.AccessKey, Name: credential.Name, UsedBytes: credential.UsedBytes, DiffBytes: diff, Updated: request.Apply})
	}
	writeJSON(w, http.StatusOK, response)
}
