package webadmin

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	dbpkg "github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
	"gorm.io/gorm"
)

const (
	credentialStatusEnabled  = "enabled"
	credentialStatusDisabled = "disabled"
	maxRequestTrendDays      = 365
	defaultRequestTrendDays  = 30
	defaultRankingLimit      = 20
	maxCredentialNameLength  = 128
)

type credentialInvalidator interface {
	Invalidate(accessKey string)
}

type API struct {
	db          *gorm.DB
	invalidator credentialInvalidator
	buckets     *storage.BucketStore
}

type bucketResponse struct {
	Name      string    `json:"name"`
	ACL       string    `json:"acl"`
	CreatedAt time.Time `json:"created_at"`
}

type createBucketRequest struct {
	Name string `json:"name"`
}

type updateBucketACLRequest struct {
	ACL string `json:"acl"`
}

type credentialResponse struct {
	ID         uint      `json:"id"`
	AccessKey  string    `json:"access_key"`
	Name       string    `json:"name"`
	Bucket     string    `json:"bucket"`
	Status     string    `json:"status"`
	QuotaBytes int64     `json:"quota_bytes"`
	UsedBytes  int64     `json:"used_bytes"`
	CreatedAt  time.Time `json:"created_at"`
}

type createCredentialRequest struct {
	Name       string `json:"name"`
	Bucket     string `json:"bucket"`
	QuotaBytes int64  `json:"quota_bytes"`
}

type createCredentialResponse struct {
	credentialResponse
	SecretKey string `json:"secret_key"`
}

type updateCredentialRequest struct {
	Name       *string `json:"name"`
	Bucket     *string `json:"bucket"`
	Status     *string `json:"status"`
	QuotaBytes *int64  `json:"quota_bytes"`
}

type dashboardSummaryResponse struct {
	TotalCredentials int64 `json:"total_credentials"`
	TotalQuotaBytes  int64 `json:"total_quota_bytes"`
	TotalUsedBytes   int64 `json:"total_used_bytes"`
}

type usageRankingItem struct {
	AccessKey  string `json:"access_key"`
	Name       string `json:"name"`
	UsedBytes  int64  `json:"used_bytes"`
	QuotaBytes int64  `json:"quota_bytes"`
}

type requestTrendItem struct {
	Day         string `json:"day"`
	PutCount    int64  `json:"put_count"`
	GetCount    int64  `json:"get_count"`
	DeleteCount int64  `json:"delete_count"`
	BytesIn     int64  `json:"bytes_in"`
	BytesOut    int64  `json:"bytes_out"`
}

func NewAPI(gdb *gorm.DB, invalidator credentialInvalidator, buckets *storage.BucketStore) *API {
	return &API{db: gdb, invalidator: invalidator, buckets: buckets}
}

func (a *API) Credentials(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listCredentials(w, r)
	case http.MethodPost:
		a.createCredential(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) CredentialByID(w http.ResponseWriter, r *http.Request) {
	idOrAccessKey := strings.TrimPrefix(r.URL.Path, "/api/admin/credentials/")
	if idOrAccessKey == "" || strings.Contains(idOrAccessKey, "/") {
		writeJSONError(w, http.StatusNotFound, "credential not found")
		return
	}

	switch r.Method {
	case http.MethodPatch:
		a.updateCredential(w, r, idOrAccessKey)
	case http.MethodDelete:
		a.deleteCredential(w, r, idOrAccessKey)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) Buckets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listBuckets(w, r)
	case http.MethodPost:
		a.createBucket(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) BucketByName(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/admin/buckets/")
	if tail == "" {
		writeJSONError(w, http.StatusNotFound, "bucket not found")
		return
	}

	if strings.HasSuffix(tail, "/acl") {
		name := strings.TrimSuffix(tail, "/acl")
		if name == "" || strings.Contains(name, "/") {
			writeJSONError(w, http.StatusNotFound, "bucket not found")
			return
		}
		if r.Method != http.MethodPut {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.updateBucketACL(w, r, name)
		return
	}

	if strings.Contains(tail, "/") {
		writeJSONError(w, http.StatusNotFound, "bucket not found")
		return
	}
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.deleteBucket(w, r, tail)
}

func (a *API) DashboardSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var resp dashboardSummaryResponse
	if err := a.db.Model(&dbpkg.Credential{}).Count(&resp.TotalCredentials).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query credentials failed")
		return
	}
	row := a.db.Model(&dbpkg.Credential{}).Select("COALESCE(SUM(quota_bytes), 0), COALESCE(SUM(used_bytes), 0)").Row()
	if err := row.Scan(&resp.TotalQuotaBytes, &resp.TotalUsedBytes); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query usage summary failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) UsageRanking(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parsePositiveInt(r.URL.Query().Get("limit"), defaultRankingLimit, 100)
	var creds []dbpkg.Credential
	if err := a.db.Order("used_bytes DESC").Order("created_at DESC").Limit(limit).Find(&creds).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query usage ranking failed")
		return
	}
	items := make([]usageRankingItem, 0, len(creds))
	for _, cred := range creds {
		items = append(items, usageRankingItem{AccessKey: cred.AccessKey, Name: cred.Name, UsedBytes: cred.UsedBytes, QuotaBytes: cred.QuotaBytes})
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *API) RequestTrend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	days := parsePositiveInt(r.URL.Query().Get("days"), defaultRequestTrendDays, maxRequestTrendDays)
	start := time.Now().UTC().AddDate(0, 0, -(days - 1))
	startDay := start.Format("2006-01-02")

	var rows []requestTrendItem
	if err := a.db.Model(&dbpkg.RequestStat{}).
		Select("day, COALESCE(SUM(put_count), 0) AS put_count, COALESCE(SUM(get_count), 0) AS get_count, COALESCE(SUM(delete_count), 0) AS delete_count, COALESCE(SUM(bytes_in), 0) AS bytes_in, COALESCE(SUM(bytes_out), 0) AS bytes_out").
		Where("day >= ?", startDay).
		Group("day").
		Order("day ASC").
		Scan(&rows).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query request trend failed")
		return
	}

	byDay := make(map[string]requestTrendItem, len(rows))
	for _, row := range rows {
		byDay[row.Day] = row
	}
	items := make([]requestTrendItem, 0, days)
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		item := byDay[day]
		item.Day = day
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *API) listCredentials(w http.ResponseWriter, _ *http.Request) {
	var creds []dbpkg.Credential
	if err := a.db.Order("created_at DESC").Find(&creds).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query credentials failed")
		return
	}
	items := make([]credentialResponse, 0, len(creds))
	for _, cred := range creds {
		items = append(items, credentialToResponse(cred))
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *API) listBuckets(w http.ResponseWriter, _ *http.Request) {
	if a.buckets == nil {
		writeJSONError(w, http.StatusInternalServerError, "bucket store is not configured")
		return
	}
	buckets, err := a.buckets.List()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query buckets failed")
		return
	}
	items := make([]bucketResponse, 0, len(buckets))
	for _, bucket := range buckets {
		items = append(items, bucketToResponse(bucket))
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *API) createBucket(w http.ResponseWriter, r *http.Request) {
	if a.buckets == nil {
		writeJSONError(w, http.StatusInternalServerError, "bucket store is not configured")
		return
	}
	var req createBucketRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	name := req.Name
	if err := a.buckets.Create(name); err != nil {
		a.writeBucketStoreError(w, err, "create bucket failed")
		return
	}
	bucket, ok := a.findBucketByName(w, name)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, bucket)
}

func (a *API) deleteBucket(w http.ResponseWriter, _ *http.Request, name string) {
	if a.buckets == nil {
		writeJSONError(w, http.StatusInternalServerError, "bucket store is not configured")
		return
	}
	if err := a.buckets.Delete(name); err != nil {
		a.writeBucketStoreError(w, err, "delete bucket failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) updateBucketACL(w http.ResponseWriter, r *http.Request, name string) {
	if a.buckets == nil {
		writeJSONError(w, http.StatusInternalServerError, "bucket store is not configured")
		return
	}
	var req updateBucketACLRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	acl := req.ACL
	if err := a.buckets.SetACL(name, acl); err != nil {
		a.writeBucketStoreError(w, err, "update bucket acl failed")
		return
	}
	bucket, ok := a.findBucketByName(w, name)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, bucket)
}

func (a *API) createCredential(w http.ResponseWriter, r *http.Request) {
	var req createCredentialRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.QuotaBytes < 0 {
		writeJSONError(w, http.StatusBadRequest, "quota_bytes must be greater than or equal to 0")
		return
	}
	bucket, err := normalizeCredentialBucket(req.Bucket)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	for attempt := 0; attempt < 5; attempt++ {
		accessKey, err := randomAccessKey()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "generate access key failed")
			return
		}
		secretKey, err := randomSecretKey()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "generate secret key failed")
			return
		}
		name, err := normalizeCredentialName(req.Name)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		cred := dbpkg.Credential{AccessKey: accessKey, SecretKey: secretKey, Name: name, Bucket: bucket, Status: credentialStatusEnabled, QuotaBytes: req.QuotaBytes}
		if err := a.db.Create(&cred).Error; err != nil {
			if attempt < 4 {
				continue
			}
			writeJSONError(w, http.StatusInternalServerError, "create credential failed")
			return
		}
		writeJSON(w, http.StatusCreated, createCredentialResponse{credentialResponse: credentialToResponse(cred), SecretKey: secretKey})
		return
	}
	writeJSONError(w, http.StatusInternalServerError, "create credential failed")
}

func (a *API) updateCredential(w http.ResponseWriter, r *http.Request, idOrAccessKey string) {
	cred, ok := a.findCredential(w, idOrAccessKey)
	if !ok {
		return
	}
	var req updateCredentialRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	updates := map[string]any{}
	if req.Name != nil {
		name, err := normalizeCredentialName(*req.Name)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		updates["name"] = name
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != credentialStatusEnabled && status != credentialStatusDisabled {
			writeJSONError(w, http.StatusBadRequest, "status must be enabled or disabled")
			return
		}
		updates["status"] = status
	}
	if req.Bucket != nil {
		bucket, err := normalizeCredentialBucket(*req.Bucket)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		updates["bucket"] = bucket
	}
	if req.QuotaBytes != nil {
		if *req.QuotaBytes < 0 {
			writeJSONError(w, http.StatusBadRequest, "quota_bytes must be greater than or equal to 0")
			return
		}
		updates["quota_bytes"] = *req.QuotaBytes
	}
	if len(updates) > 0 {
		if err := a.db.Model(&cred).Updates(updates).Error; err != nil {
			writeJSONError(w, http.StatusInternalServerError, "update credential failed")
			return
		}
		if err := a.db.First(&cred, cred.ID).Error; err != nil {
			writeJSONError(w, http.StatusInternalServerError, "reload credential failed")
			return
		}
		a.invalidate(cred.AccessKey)
	}
	writeJSON(w, http.StatusOK, credentialToResponse(cred))
}

func (a *API) deleteCredential(w http.ResponseWriter, _ *http.Request, idOrAccessKey string) {
	cred, ok := a.findCredential(w, idOrAccessKey)
	if !ok {
		return
	}
	if err := a.db.Delete(&cred).Error; err != nil {
		writeJSONError(w, http.StatusInternalServerError, "delete credential failed")
		return
	}
	a.invalidate(cred.AccessKey)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) findCredential(w http.ResponseWriter, idOrAccessKey string) (dbpkg.Credential, bool) {
	var cred dbpkg.Credential
	query := a.db
	if id, err := strconv.ParseUint(idOrAccessKey, 10, 64); err == nil {
		query = query.Where("id = ?", uint(id))
	} else {
		query = query.Where("access_key = ?", idOrAccessKey)
	}
	if err := query.First(&cred).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSONError(w, http.StatusNotFound, "credential not found")
			return dbpkg.Credential{}, false
		}
		writeJSONError(w, http.StatusInternalServerError, "query credential failed")
		return dbpkg.Credential{}, false
	}
	return cred, true
}

func (a *API) findBucketByName(w http.ResponseWriter, name string) (bucketResponse, bool) {
	buckets, err := a.buckets.List()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query buckets failed")
		return bucketResponse{}, false
	}
	for _, bucket := range buckets {
		if bucket.Name == name {
			return bucketToResponse(bucket), true
		}
	}
	writeJSONError(w, http.StatusNotFound, "bucket not found")
	return bucketResponse{}, false
}

func (a *API) writeBucketStoreError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, storage.ErrInvalidBucketName):
		writeJSONError(w, http.StatusBadRequest, "invalid bucket name")
	case errors.Is(err, storage.ErrInvalidACL):
		writeJSONError(w, http.StatusBadRequest, "acl must be private or public-read")
	case errors.Is(err, storage.ErrBucketNotEmpty):
		writeJSONError(w, http.StatusConflict, "bucket not empty")
	case errors.Is(err, storage.ErrNoSuchBucket):
		writeJSONError(w, http.StatusNotFound, "bucket not found")
	default:
		writeJSONError(w, http.StatusInternalServerError, fallback)
	}
}

func (a *API) invalidate(accessKey string) {
	if a.invalidator != nil {
		a.invalidator.Invalidate(accessKey)
	}
}

func credentialToResponse(cred dbpkg.Credential) credentialResponse {
	return credentialResponse{ID: cred.ID, AccessKey: cred.AccessKey, Name: cred.Name, Bucket: cred.Bucket, Status: cred.Status, QuotaBytes: cred.QuotaBytes, UsedBytes: cred.UsedBytes, CreatedAt: cred.CreatedAt}
}

func bucketToResponse(bucket dbpkg.Bucket) bucketResponse {
	return bucketResponse{Name: bucket.Name, ACL: bucket.ACL, CreatedAt: bucket.CreatedAt}
}

func randomAccessKey() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 20)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

func randomSecretKey() (string, error) {
	raw := make([]byte, 30)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw), nil
}

func parsePositiveInt(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func normalizeCredentialName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if utf8.RuneCountInString(trimmed) > maxCredentialNameLength {
		return "", errors.New("name must be 128 characters or fewer")
	}
	return trimmed, nil
}

func normalizeCredentialBucket(bucket string) (string, error) {
	trimmed := strings.TrimSpace(bucket)
	if trimmed == "" {
		return "", nil
	}
	if err := storage.ValidateBucketName(trimmed); err != nil {
		return "", errors.New("bucket must be a valid bucket name or empty for all buckets")
	}
	return trimmed, nil
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := jsonDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra struct{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("unexpected extra json")
	}
	return nil
}
