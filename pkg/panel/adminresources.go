package panel

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RSJWY/NativeS3-Bridge/pkg/config"
	"github.com/RSJWY/NativeS3-Bridge/pkg/managedconfig"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

type nodeBucketResponse struct {
	Name      string    `json:"name"`
	ACL       string    `json:"acl"`
	CreatedAt time.Time `json:"created_at"`
}

type createNodeBucketRequest struct {
	Name string `json:"name"`
	ACL  string `json:"acl"`
}

type updateNodeBucketACLRequest struct {
	ACL string `json:"acl"`
}

func (a *AdminAPI) bucketsRoute(w http.ResponseWriter, r *http.Request, nodeID uint, rest []string) {
	if _, ok := a.loadNode(w, nodeID); !ok {
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			a.listNodeBuckets(w, nodeID)
		case http.MethodPost:
			a.createNodeBucket(w, r, nodeID)
		default:
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if len(rest) == 1 {
		if r.Method != http.MethodDelete {
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.deleteNodeBucket(w, nodeID, rest[0])
		return
	}
	if len(rest) == 2 && rest[1] == "acl" {
		if r.Method != http.MethodPut {
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.updateNodeBucketACL(w, r, nodeID, rest[0])
		return
	}
	writeTransportError(w, http.StatusNotFound, "not found")
}

func (a *AdminAPI) listNodeBuckets(w http.ResponseWriter, nodeID uint) {
	var buckets []NodeBucket
	if err := a.db.Where("node_id = ?", nodeID).Order("name ASC").Find(&buckets).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query buckets failed")
		return
	}
	items := make([]nodeBucketResponse, 0, len(buckets))
	for _, bucket := range buckets {
		items = append(items, nodeBucketResponse{Name: bucket.Name, ACL: bucket.ACL, CreatedAt: bucket.CreatedAt})
	}
	writeTransportJSON(w, http.StatusOK, items)
}

func (a *AdminAPI) createNodeBucket(w http.ResponseWriter, r *http.Request, nodeID uint) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	var request createNodeBucketRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.ACL = strings.TrimSpace(request.ACL)
	if request.ACL == "" {
		request.ACL = storage.ACLPrivate
	}
	if err := storage.ValidateBucketName(request.Name); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid bucket name")
		return
	}
	if request.ACL != storage.ACLPrivate && request.ACL != storage.ACLPublicRead {
		writeTransportError(w, http.StatusBadRequest, "acl must be private or public-read")
		return
	}
	bucket := NodeBucket{NodeID: nodeID, Name: request.Name, ACL: request.ACL}
	result := a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&bucket)
	if result.Error != nil {
		writeTransportError(w, http.StatusInternalServerError, "create bucket failed")
		return
	}
	if result.RowsAffected == 0 {
		writeTransportError(w, http.StatusConflict, "bucket already exists")
		return
	}
	a.audit.Write(AuditEntry{Action: "bucket_create", TargetNode: nodeID, TargetResource: request.Name, Result: "created", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusCreated, nodeBucketResponse{Name: bucket.Name, ACL: bucket.ACL, CreatedAt: bucket.CreatedAt})
}

func (a *AdminAPI) updateNodeBucketACL(w http.ResponseWriter, r *http.Request, nodeID uint, name string) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	if err := storage.ValidateBucketName(name); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid bucket name")
		return
	}
	var request updateNodeBucketACLRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	request.ACL = strings.TrimSpace(request.ACL)
	if request.ACL != storage.ACLPrivate && request.ACL != storage.ACLPublicRead {
		writeTransportError(w, http.StatusBadRequest, "acl must be private or public-read")
		return
	}
	result := a.db.Model(&NodeBucket{}).Where("node_id = ? AND name = ?", nodeID, name).Update("acl", request.ACL)
	if result.Error != nil {
		writeTransportError(w, http.StatusInternalServerError, "update bucket acl failed")
		return
	}
	if result.RowsAffected == 0 {
		writeTransportError(w, http.StatusNotFound, "bucket not found")
		return
	}
	var bucket NodeBucket
	if err := a.db.Where("node_id = ? AND name = ?", nodeID, name).First(&bucket).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query bucket failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "bucket_acl", TargetNode: nodeID, TargetResource: name, Result: request.ACL, Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, nodeBucketResponse{Name: bucket.Name, ACL: bucket.ACL, CreatedAt: bucket.CreatedAt})
}

func (a *AdminAPI) deleteNodeBucket(w http.ResponseWriter, nodeID uint, name string) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	if err := storage.ValidateBucketName(name); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid bucket name")
		return
	}
	err := a.db.Transaction(func(tx *gorm.DB) error {
		var bucket NodeBucket
		if err := tx.Where("node_id = ? AND name = ?", nodeID, name).First(&bucket).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNodeBucketNotFound
			}
			return err
		}
		var bound int64
		if err := tx.Model(&NodeCredential{}).Where("node_id = ? AND bucket = ?", nodeID, name).Count(&bound).Error; err != nil {
			return err
		}
		if bound > 0 {
			return ErrNodeBucketBound
		}
		return tx.Where("id = ? AND node_id = ?", bucket.ID, nodeID).Delete(&NodeBucket{}).Error
	})
	switch {
	case errors.Is(err, ErrNodeBucketNotFound):
		writeTransportError(w, http.StatusNotFound, "bucket not found")
		return
	case errors.Is(err, ErrNodeBucketBound):
		writeTransportError(w, http.StatusConflict, "bucket has bound credentials")
		return
	case err != nil:
		writeTransportError(w, http.StatusInternalServerError, "delete bucket failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "bucket_delete", TargetNode: nodeID, TargetResource: name, Result: "deleted", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

type nodeWebhookResponse struct {
	ID        uint      `json:"id"`
	NodeID    uint      `json:"node_id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type createNodeWebhookRequest struct {
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled"`
}

type updateNodeWebhookRequest struct {
	URL     *string   `json:"url"`
	Events  *[]string `json:"events"`
	Enabled *bool     `json:"enabled"`
}

func (a *AdminAPI) webhooksRoute(w http.ResponseWriter, r *http.Request, nodeID uint, rest []string) {
	if _, ok := a.loadNode(w, nodeID); !ok {
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			a.listNodeWebhooks(w, nodeID)
		case http.MethodPost:
			a.createNodeWebhook(w, r, nodeID)
		default:
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if len(rest) != 1 {
		writeTransportError(w, http.StatusNotFound, "not found")
		return
	}
	webhookID, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil || webhookID == 0 {
		writeTransportError(w, http.StatusNotFound, "webhook not found")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		a.updateNodeWebhook(w, r, nodeID, uint(webhookID))
	case http.MethodDelete:
		a.deleteNodeWebhook(w, nodeID, uint(webhookID))
	default:
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func webhookToResponse(webhook NodeWebhook) (nodeWebhookResponse, error) {
	events, err := managedconfig.ParseWebhookEvents(webhook.Events)
	if err != nil {
		return nodeWebhookResponse{}, err
	}
	return nodeWebhookResponse{
		ID: webhook.ID, NodeID: webhook.NodeID, URL: webhook.URL, Events: events,
		Enabled: webhook.Enabled, CreatedAt: webhook.CreatedAt,
	}, nil
}

func (a *AdminAPI) listNodeWebhooks(w http.ResponseWriter, nodeID uint) {
	var webhooks []NodeWebhook
	if err := a.db.Where("node_id = ?", nodeID).Order("id ASC").Find(&webhooks).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query webhooks failed")
		return
	}
	items := make([]nodeWebhookResponse, 0, len(webhooks))
	for _, webhook := range webhooks {
		item, err := webhookToResponse(webhook)
		if err != nil {
			writeTransportError(w, http.StatusInternalServerError, "stored webhook is invalid")
			return
		}
		items = append(items, item)
	}
	writeTransportJSON(w, http.StatusOK, items)
}

func (a *AdminAPI) createNodeWebhook(w http.ResponseWriter, r *http.Request, nodeID uint) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	var request createNodeWebhookRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	request.URL = strings.TrimSpace(request.URL)
	events, _, err := managedconfig.CanonicalWebhookEvents(request.Events)
	if err != nil || managedconfig.ValidateWebhook(request.URL, events) != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid webhook url or events")
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	var duplicate int64
	if err := a.db.Model(&NodeWebhook{}).Where("node_id = ? AND url = ? AND events = ?", nodeID, request.URL, events).Count(&duplicate).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query webhooks failed")
		return
	}
	if duplicate > 0 {
		writeTransportError(w, http.StatusConflict, "webhook already exists")
		return
	}
	webhook := NodeWebhook{NodeID: nodeID, URL: request.URL, Events: events, Enabled: enabled}
	err = a.db.Transaction(func(tx *gorm.DB) error {
		return insertNodeWebhook(tx, &webhook)
	})
	if err != nil {
		writeTransportError(w, http.StatusInternalServerError, "create webhook failed")
		return
	}
	item, _ := webhookToResponse(webhook)
	a.audit.Write(AuditEntry{Action: "webhook_create", TargetNode: nodeID, TargetResource: strconv.FormatUint(uint64(webhook.ID), 10), Result: "created", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusCreated, item)
}

// insertNodeWebhook preserves an explicit disabled value despite the model's
// default:true tag. GORM otherwise omits false on Create and lets the database
// default silently turn an imported or newly-created disabled hook on.
func insertNodeWebhook(tx *gorm.DB, webhook *NodeWebhook) error {
	enabled := webhook.Enabled
	webhook.Enabled = true
	if err := tx.Create(webhook).Error; err != nil {
		webhook.Enabled = enabled
		return err
	}
	if !enabled {
		if err := tx.Model(&NodeWebhook{}).Where("id = ? AND node_id = ?", webhook.ID, webhook.NodeID).Update("enabled", false).Error; err != nil {
			return err
		}
	}
	webhook.Enabled = enabled
	return nil
}

func (a *AdminAPI) updateNodeWebhook(w http.ResponseWriter, r *http.Request, nodeID, webhookID uint) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	var webhook NodeWebhook
	if err := a.db.Where("node_id = ? AND id = ?", nodeID, webhookID).First(&webhook).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeTransportError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeTransportError(w, http.StatusInternalServerError, "query webhook failed")
		return
	}
	var request updateNodeWebhookRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	urlValue := webhook.URL
	eventsValue := webhook.Events
	enabled := webhook.Enabled
	if request.URL != nil {
		urlValue = strings.TrimSpace(*request.URL)
	}
	if request.Events != nil {
		var err error
		eventsValue, _, err = managedconfig.CanonicalWebhookEvents(*request.Events)
		if err != nil {
			writeTransportError(w, http.StatusBadRequest, "invalid webhook events")
			return
		}
	}
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	if err := managedconfig.ValidateWebhook(urlValue, eventsValue); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid webhook url or events")
		return
	}
	var duplicate int64
	if err := a.db.Model(&NodeWebhook{}).Where("node_id = ? AND url = ? AND events = ? AND id <> ?", nodeID, urlValue, eventsValue, webhookID).Count(&duplicate).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query webhooks failed")
		return
	}
	if duplicate > 0 {
		writeTransportError(w, http.StatusConflict, "webhook already exists")
		return
	}
	if err := a.db.Model(&NodeWebhook{}).Where("node_id = ? AND id = ?", nodeID, webhookID).Updates(map[string]any{
		"url": urlValue, "events": eventsValue, "enabled": enabled,
	}).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "update webhook failed")
		return
	}
	webhook.URL = urlValue
	webhook.Events = eventsValue
	webhook.Enabled = enabled
	item, _ := webhookToResponse(webhook)
	a.audit.Write(AuditEntry{Action: "webhook_update", TargetNode: nodeID, TargetResource: strconv.FormatUint(uint64(webhookID), 10), Result: "updated", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, item)
}

func (a *AdminAPI) deleteNodeWebhook(w http.ResponseWriter, nodeID, webhookID uint) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	result := a.db.Where("node_id = ? AND id = ?", nodeID, webhookID).Delete(&NodeWebhook{})
	if result.Error != nil {
		writeTransportError(w, http.StatusInternalServerError, "delete webhook failed")
		return
	}
	if result.RowsAffected == 0 {
		writeTransportError(w, http.StatusNotFound, "webhook not found")
		return
	}
	a.audit.Write(AuditEntry{Action: "webhook_delete", TargetNode: nodeID, TargetResource: strconv.FormatUint(uint64(webhookID), 10), Result: "deleted", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

type rateLimitValues struct {
	AnonymousRPS   float64 `json:"anonymous_rps"`
	AnonymousBurst int     `json:"anonymous_burst"`
	TrustForwarded bool    `json:"trust_forwarded"`
}

type nodeRateLimitResponse struct {
	Configured bool             `json:"configured"`
	Values     *rateLimitValues `json:"values,omitempty"`
	Effective  rateLimitValues  `json:"effective"`
}

type upsertNodeRateLimitRequest struct {
	AnonymousRPS   float64 `json:"anonymous_rps"`
	AnonymousBurst int     `json:"anonymous_burst"`
	TrustForwarded bool    `json:"trust_forwarded"`
}

func (a *AdminAPI) rateLimitRoute(w http.ResponseWriter, r *http.Request, nodeID uint, rest []string) {
	if len(rest) != 0 {
		writeTransportError(w, http.StatusNotFound, "not found")
		return
	}
	if _, ok := a.loadNode(w, nodeID); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.getNodeRateLimit(w, nodeID)
	case http.MethodPut:
		a.upsertNodeRateLimit(w, r, nodeID)
	case http.MethodDelete:
		a.resetNodeRateLimit(w, nodeID)
	default:
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func defaultRateLimitValues() rateLimitValues {
	return rateLimitValues{AnonymousRPS: config.DefaultAnonymousRPS, AnonymousBurst: config.DefaultAnonymousBurst}
}

func (a *AdminAPI) getNodeRateLimit(w http.ResponseWriter, nodeID uint) {
	var limit NodeRateLimit
	err := a.db.Where("node_id = ?", nodeID).First(&limit).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeTransportJSON(w, http.StatusOK, nodeRateLimitResponse{Configured: false, Effective: defaultRateLimitValues()})
		return
	}
	if err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query rate limit failed")
		return
	}
	values := rateLimitValues{AnonymousRPS: limit.AnonymousRPS, AnonymousBurst: limit.AnonymousBurst, TrustForwarded: limit.TrustForwarded}
	writeTransportJSON(w, http.StatusOK, nodeRateLimitResponse{Configured: true, Values: &values, Effective: values})
}

func (a *AdminAPI) upsertNodeRateLimit(w http.ResponseWriter, r *http.Request, nodeID uint) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	var request upsertNodeRateLimitRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if err := managedconfig.ValidateRateLimit(request.AnonymousRPS, request.AnonymousBurst); err != nil {
		writeTransportError(w, http.StatusBadRequest, "anonymous_rps and anonymous_burst must be positive")
		return
	}
	limit := NodeRateLimit{NodeID: nodeID, AnonymousRPS: request.AnonymousRPS, AnonymousBurst: request.AnonymousBurst, TrustForwarded: request.TrustForwarded}
	if err := a.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"anonymous_rps": request.AnonymousRPS, "anonymous_burst": request.AnonymousBurst,
			"trust_forwarded": request.TrustForwarded, "updated_at": nowUTC(),
		}),
	}).Create(&limit).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "save rate limit failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "rate_limit_upsert", TargetNode: nodeID, Result: "updated", Source: a.adminIdentity})
	values := rateLimitValues{AnonymousRPS: request.AnonymousRPS, AnonymousBurst: request.AnonymousBurst, TrustForwarded: request.TrustForwarded}
	writeTransportJSON(w, http.StatusOK, nodeRateLimitResponse{Configured: true, Values: &values, Effective: values})
}

func (a *AdminAPI) resetNodeRateLimit(w http.ResponseWriter, nodeID uint) {
	unlock := lockNodeDraft(nodeID)
	defer unlock()

	if err := a.db.Where("node_id = ?", nodeID).Delete(&NodeRateLimit{}).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "reset rate limit failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "rate_limit_reset", TargetNode: nodeID, Result: "default", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, nodeRateLimitResponse{Configured: false, Effective: defaultRateLimitValues()})
}
