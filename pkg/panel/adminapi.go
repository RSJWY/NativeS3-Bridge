package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/managedconfig"
)

// AdminAPI serves the panel's management REST surface: node lifecycle, tokens,
// certificate revocation, credentials (with once-only secret return + rotation),
// desired-state publish/push, task dispatch, and status/drift views. Every
// mutating handler writes an audit record via the Auditor (design §7.2). The
// handlers are mounted behind the reused webadmin auth middleware in cmd/panel,
// so they assume the caller is already authenticated.
type AdminAPI struct {
	db        *gorm.DB
	hub       *Hub
	creds     *CredentialStore
	desired   *DesiredStateAuthority
	tasks     *TaskOrchestrator
	transport *TransportServer
	migration *MigrationCoordinator
	audit     *Auditor
	// adminIdentity is the single-admin identity stamped into audit rows. The
	// first version keeps a single administrator (design/PRD: no multi-user/RBAC).
	adminIdentity string
}

// NewAdminAPI wires the admin API over its collaborators.
func NewAdminAPI(db *gorm.DB, hub *Hub, creds *CredentialStore, desired *DesiredStateAuthority, tasks *TaskOrchestrator, transport *TransportServer, migration *MigrationCoordinator, audit *Auditor) *AdminAPI {
	return &AdminAPI{
		db:            db,
		hub:           hub,
		creds:         creds,
		desired:       desired,
		tasks:         tasks,
		transport:     transport,
		migration:     migration,
		audit:         audit,
		adminIdentity: "admin",
	}
}

// Routes registers the admin REST handlers on mux. Each handler is wrapped by
// the caller's auth middleware in cmd/panel; here we only register paths.
func (a *AdminAPI) Routes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(fn http.HandlerFunc) http.Handler { return wrap(fn) }
	mux.Handle("/api/admin/nodes", h(a.Nodes))
	mux.Handle("/api/admin/nodes/", h(a.NodeByID))
}

// --- request/response shapes ---

type nodeResponse struct {
	ID              uint       `json:"id"`
	DisplayName     string     `json:"display_name"`
	Status          string     `json:"status"`
	Online          bool       `json:"online"`
	AppliedVersion  int64      `json:"applied_version"`
	DesiredVersion  int64      `json:"desired_version"`
	SyncState       string     `json:"sync_state"`
	LastError       string     `json:"last_error,omitempty"`
	DraftDirty      bool       `json:"draft_dirty"`
	PublishRequired bool       `json:"publish_required"`
	LastHeartbeat   *time.Time `json:"last_heartbeat,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type createNodeRequest struct {
	DisplayName string `json:"display_name"`
}

// Nodes handles GET (list) and POST (create) on /api/admin/nodes.
func (a *AdminAPI) Nodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listNodes(w, r)
	case http.MethodPost:
		a.createNode(w, r)
	default:
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// NodeByID dispatches the per-node sub-resources: lifecycle, tokens, certs,
// credentials, desired-state publish/push, tasks, and status.
func (a *AdminAPI) NodeByID(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/admin/nodes/")
	if tail == "" {
		writeTransportError(w, http.StatusNotFound, "not found")
		return
	}
	segments := strings.Split(tail, "/")
	nodeID, err := strconv.ParseUint(segments[0], 10, 64)
	if err != nil {
		writeTransportError(w, http.StatusNotFound, "invalid node id")
		return
	}
	id := uint(nodeID)

	// /api/admin/nodes/{id}
	if len(segments) == 1 {
		switch r.Method {
		case http.MethodGet:
			a.getNode(w, r, id)
		case http.MethodPatch:
			a.updateNode(w, r, id)
		case http.MethodDelete:
			a.retireNode(w, r, id)
		default:
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// /api/admin/nodes/{id}/{sub...}
	switch segments[1] {
	case "tokens":
		a.issueToken(w, r, id)
	case "credentials":
		a.credentialsRoute(w, r, id, segments[2:])
	case "buckets":
		a.bucketsRoute(w, r, id, segments[2:])
	case "webhooks":
		a.webhooksRoute(w, r, id, segments[2:])
	case "rate-limit":
		a.rateLimitRoute(w, r, id, segments[2:])
	case "desired-state":
		a.desiredStateRoute(w, r, id, segments[2:])
	case "tasks":
		a.tasksRoute(w, r, id, segments[2:])
	case "certs":
		a.certsRoute(w, r, id, segments[2:])
	case "import":
		a.importRoute(w, r, id, segments[2:])
	default:
		writeTransportError(w, http.StatusNotFound, "not found")
	}
}

// --- node lifecycle ---

func (a *AdminAPI) listNodes(w http.ResponseWriter, _ *http.Request) {
	var nodes []Node
	if err := a.db.Order("id ASC").Find(&nodes).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query nodes failed")
		return
	}
	items := make([]nodeResponse, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, a.nodeToResponse(n))
	}
	writeTransportJSON(w, http.StatusOK, items)
}

func (a *AdminAPI) createNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if err := decodeAdminJSON(r, &req); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		writeTransportError(w, http.StatusBadRequest, "display_name is required")
		return
	}
	node := Node{DisplayName: name, Status: NodeStatusActive}
	if err := a.db.Create(&node).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "create node failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "node_create", TargetNode: node.ID, Result: "created", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusCreated, a.nodeToResponse(node))
}

func (a *AdminAPI) getNode(w http.ResponseWriter, _ *http.Request, id uint) {
	node, ok := a.loadNode(w, id)
	if !ok {
		return
	}
	writeTransportJSON(w, http.StatusOK, a.nodeToResponse(node))
}

type updateNodeRequest struct {
	DisplayName *string `json:"display_name"`
	Status      *string `json:"status"`
}

func (a *AdminAPI) updateNode(w http.ResponseWriter, r *http.Request, id uint) {
	node, ok := a.loadNode(w, id)
	if !ok {
		return
	}
	if node.Status == NodeStatusRetired {
		writeTransportError(w, http.StatusConflict, "node is retired")
		return
	}
	var req updateNodeRequest
	if err := decodeAdminJSON(r, &req); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	updates := map[string]any{}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" {
			writeTransportError(w, http.StatusBadRequest, "display_name must not be empty")
			return
		}
		updates["display_name"] = name
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		// Admins may toggle between active and disabled here. Retire is a separate,
		// irreversible DELETE operation so it cannot happen by accident.
		if status != NodeStatusActive && status != NodeStatusDisabled {
			writeTransportError(w, http.StatusBadRequest, "status must be active or disabled")
			return
		}
		updates["status"] = status
	}
	if len(updates) > 0 {
		updates["updated_at"] = nowUTC()
		if err := a.db.Model(&Node{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			writeTransportError(w, http.StatusInternalServerError, "update node failed")
			return
		}
		if status, ok := updates["status"].(string); ok {
			a.audit.Write(AuditEntry{Action: "node_status", TargetNode: id, Result: status, Source: a.adminIdentity})
			// Disabling a node drops its live control-plane connection so no further
			// desired state or tasks reach it until re-enabled.
			if status == NodeStatusDisabled {
				if conn, online := a.hub.Get(id); online {
					conn.close("node disabled")
				}
			}
		}
	}
	node, _ = a.loadNode(w, id)
	writeTransportJSON(w, http.StatusOK, a.nodeToResponse(node))
}

// retireNode implements UI "delete" as permanent retirement (design §3.4): the
// node row is retained for audit relationships, all certs + tokens are revoked,
// and the live connection is dropped. Retire is irreversible.
func (a *AdminAPI) retireNode(w http.ResponseWriter, _ *http.Request, id uint) {
	node, ok := a.loadNode(w, id)
	if !ok {
		return
	}
	if node.Status == NodeStatusRetired {
		writeTransportJSON(w, http.StatusOK, a.nodeToResponse(node))
		return
	}
	now := nowUTC()
	err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Node{}).Where("id = ?", id).Updates(map[string]any{
			"status": NodeStatusRetired, "retired_at": &now, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		if _, err := RevokeNodeCerts(tx, id, now); err != nil {
			return err
		}
		return InvalidateNodeTokens(tx, id, now)
	})
	if err != nil {
		writeTransportError(w, http.StatusInternalServerError, "retire node failed")
		return
	}
	if conn, online := a.hub.Get(id); online {
		conn.close("node retired")
	}
	a.audit.Write(AuditEntry{Action: "node_retire", TargetNode: id, Result: "retired", Source: a.adminIdentity})
	node, _ = a.loadNode(w, id)
	writeTransportJSON(w, http.StatusOK, a.nodeToResponse(node))
}

// --- registration tokens ---

type issueTokenResponse struct {
	Token     string    `json:"token"` // returned once; only the hash is stored
	ExpiresAt time.Time `json:"expires_at"`
}

func (a *AdminAPI) issueToken(w http.ResponseWriter, r *http.Request, id uint) {
	if r.Method != http.MethodPost {
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	node, ok := a.loadNode(w, id)
	if !ok {
		return
	}
	if node.Status == NodeStatusRetired {
		writeTransportError(w, http.StatusConflict, "node is retired")
		return
	}
	now := nowUTC()
	token, err := GenerateRegistrationToken(a.db, id, DefaultRegistrationTokenTTL, now)
	if err != nil {
		writeTransportError(w, http.StatusInternalServerError, "issue token failed")
		return
	}
	// Audit records that a token was issued, never the token value itself.
	a.audit.Write(AuditEntry{Action: "token_issue", TargetNode: id, Result: "issued", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusCreated, issueTokenResponse{
		Token:     token,
		ExpiresAt: now.Add(DefaultRegistrationTokenTTL),
	})
}

// --- certificates ---

func (a *AdminAPI) certsRoute(w http.ResponseWriter, r *http.Request, id uint, rest []string) {
	// /api/admin/nodes/{id}/certs           GET  -> list
	// /api/admin/nodes/{id}/certs/revoke     POST -> revoke all node certs
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var certs []NodeCert
		if err := a.db.Where("node_id = ?", id).Order("id ASC").Find(&certs).Error; err != nil {
			writeTransportError(w, http.StatusInternalServerError, "query certs failed")
			return
		}
		writeTransportJSON(w, http.StatusOK, certs)
		return
	}
	if rest[0] == "revoke" && r.Method == http.MethodPost {
		n, err := RevokeNodeCerts(a.db, id, nowUTC())
		if err != nil {
			writeTransportError(w, http.StatusInternalServerError, "revoke certs failed")
			return
		}
		// Drop the live connection so the revoked cert cannot keep the session.
		if conn, online := a.hub.Get(id); online {
			conn.close("certificate revoked")
		}
		a.audit.Write(AuditEntry{Action: "cert_revoke", TargetNode: id, Result: strconv.FormatInt(n, 10), Source: a.adminIdentity})
		writeTransportJSON(w, http.StatusOK, map[string]any{"revoked": n})
		return
	}
	writeTransportError(w, http.StatusNotFound, "not found")
}

// --- credentials (once-only secret + rotation) ---

type credentialResponse struct {
	ID         uint   `json:"id"`
	NodeID     uint   `json:"node_id"`
	AccessKey  string `json:"access_key"`
	Name       string `json:"name"`
	Bucket     string `json:"bucket"`
	Status     string `json:"status"`
	QuotaBytes int64  `json:"quota_bytes"`
	// SecretKey is populated ONLY in create/rotate responses (returned once).
	SecretKey string `json:"secret_key,omitempty"`
}

type createCredentialRequest struct {
	Name       string `json:"name"`
	Bucket     string `json:"bucket"`
	QuotaBytes int64  `json:"quota_bytes"`
}

func (a *AdminAPI) credentialsRoute(w http.ResponseWriter, r *http.Request, id uint, rest []string) {
	// /api/admin/nodes/{id}/credentials                 GET (list) / POST (create)
	// /api/admin/nodes/{id}/credentials/{ak}/rotate      POST
	if _, ok := a.loadNode(w, id); !ok {
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			a.listCredentials(w, id)
		case http.MethodPost:
			a.createCredential(w, r, id)
		default:
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "rotate" && r.Method == http.MethodPost {
		a.rotateCredential(w, r, id, rest[0])
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodPatch:
			a.updateCredential(w, r, id, rest[0])
		case http.MethodDelete:
			a.deleteCredential(w, id, rest[0])
		default:
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	writeTransportError(w, http.StatusNotFound, "not found")
}

func credentialToResponse(credential NodeCredential) credentialResponse {
	return credentialResponse{
		ID: credential.ID, NodeID: credential.NodeID, AccessKey: credential.AccessKey,
		Name: credential.Name, Bucket: credential.Bucket, Status: credential.Status, QuotaBytes: credential.QuotaBytes,
	}
}

func (a *AdminAPI) listCredentials(w http.ResponseWriter, id uint) {
	var creds []NodeCredential
	if err := a.db.Where("node_id = ?", id).Order("access_key ASC").Find(&creds).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query credentials failed")
		return
	}
	items := make([]credentialResponse, 0, len(creds))
	for _, c := range creds {
		// SecretKey is deliberately omitted: list never returns plaintext (§2.3).
		items = append(items, credentialToResponse(c))
	}
	writeTransportJSON(w, http.StatusOK, items)
}

func (a *AdminAPI) createCredential(w http.ResponseWriter, r *http.Request, id uint) {
	var req createCredentialRequest
	if err := decodeAdminJSON(r, &req); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	created, err := a.creds.Create(id, req.Name, req.Bucket, req.QuotaBytes)
	if err != nil {
		if errors.Is(err, ErrMasterKeyMissing) {
			writeTransportError(w, http.StatusInternalServerError, "master key unavailable")
			return
		}
		if errors.Is(err, ErrNodeBucketNotFound) {
			writeTransportError(w, http.StatusBadRequest, "bucket does not exist")
			return
		}
		if errors.Is(err, managedconfig.ErrInvalidCredential) {
			writeTransportError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeTransportError(w, http.StatusInternalServerError, "create credential failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "credential_create", TargetNode: id, TargetResource: created.AccessKey, Result: "created", Source: a.adminIdentity})
	// The plaintext secret is returned exactly once here.
	writeTransportJSON(w, http.StatusCreated, credentialResponse{
		ID: created.ID, NodeID: id, AccessKey: created.AccessKey, Name: created.Name,
		Bucket: created.Bucket, Status: created.Status, QuotaBytes: created.QuotaBytes, SecretKey: created.SecretKey,
	})
}

type updateCredentialRequest struct {
	Name       *string `json:"name"`
	Bucket     *string `json:"bucket"`
	Status     *string `json:"status"`
	QuotaBytes *int64  `json:"quota_bytes"`
}

func (a *AdminAPI) updateCredential(w http.ResponseWriter, r *http.Request, nodeID uint, accessKey string) {
	var request updateCredentialRequest
	if err := decodeAdminJSON(r, &request); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	credential, err := a.creds.Update(nodeID, accessKey, CredentialUpdate{
		Name: request.Name, Bucket: request.Bucket, Status: request.Status, QuotaBytes: request.QuotaBytes,
	})
	switch {
	case errors.Is(err, ErrCredentialNotFound):
		writeTransportError(w, http.StatusNotFound, "credential not found")
		return
	case errors.Is(err, ErrNodeBucketNotFound):
		writeTransportError(w, http.StatusBadRequest, "bucket does not exist")
		return
	case errors.Is(err, managedconfig.ErrInvalidCredential):
		writeTransportError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeTransportError(w, http.StatusInternalServerError, "update credential failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "credential_update", TargetNode: nodeID, TargetResource: accessKey, Result: "updated", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, credentialToResponse(credential))
}

func (a *AdminAPI) deleteCredential(w http.ResponseWriter, nodeID uint, accessKey string) {
	if err := a.creds.Delete(nodeID, accessKey); err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			writeTransportError(w, http.StatusNotFound, "credential not found")
			return
		}
		writeTransportError(w, http.StatusInternalServerError, "delete credential failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "credential_delete", TargetNode: nodeID, TargetResource: accessKey, Result: "deleted", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *AdminAPI) rotateCredential(w http.ResponseWriter, _ *http.Request, id uint, accessKey string) {
	rotated, err := a.creds.Rotate(id, accessKey)
	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			writeTransportError(w, http.StatusNotFound, "credential not found")
			return
		}
		if errors.Is(err, ErrMasterKeyMissing) {
			writeTransportError(w, http.StatusInternalServerError, "master key unavailable")
			return
		}
		writeTransportError(w, http.StatusInternalServerError, "rotate credential failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "credential_rotate", TargetNode: id, TargetResource: accessKey, Result: "rotated", Source: a.adminIdentity})
	// New plaintext secret returned once. The admin must publish + push desired
	// state for the node to pick it up (surfaced in the response hint).
	writeTransportJSON(w, http.StatusOK, credentialResponse{
		ID: rotated.ID, NodeID: id, AccessKey: rotated.AccessKey, Name: rotated.Name,
		Bucket: rotated.Bucket, Status: rotated.Status, QuotaBytes: rotated.QuotaBytes, SecretKey: rotated.SecretKey,
	})
}

// --- desired state publish + push ---

type publishResponse struct {
	Version     int64  `json:"version"`
	ContentHash string `json:"content_hash"`
	Pushed      bool   `json:"pushed"`
	PushError   string `json:"push_error,omitempty"`
}

func (a *AdminAPI) desiredStateRoute(w http.ResponseWriter, r *http.Request, id uint, rest []string) {
	// /api/admin/nodes/{id}/desired-state         POST -> publish new version
	// /api/admin/nodes/{id}/desired-state/push     POST -> push to online node
	if len(rest) == 0 && r.Method == http.MethodPost {
		a.publishDesiredState(w, r, id)
		return
	}
	if len(rest) == 1 && rest[0] == "push" && r.Method == http.MethodPost {
		a.pushDesiredState(w, r, id)
		return
	}
	writeTransportError(w, http.StatusNotFound, "not found")
}

func (a *AdminAPI) publishDesiredState(w http.ResponseWriter, r *http.Request, id uint) {
	if _, ok := a.loadNode(w, id); !ok {
		return
	}
	version, hash, err := a.desired.Publish(id, a.adminIdentity)
	if err != nil {
		if errors.Is(err, ErrMasterKeyMissing) {
			writeTransportError(w, http.StatusInternalServerError, "master key unavailable")
			return
		}
		writeTransportError(w, http.StatusInternalServerError, "publish desired state failed")
		return
	}
	a.audit.Write(AuditEntry{Action: "desired_publish", TargetNode: id, Result: "v" + strconv.FormatInt(version, 10), Source: a.adminIdentity})

	// Best-effort immediate push if the node is online; otherwise it reconciles
	// on next reconnect (design §5.3: desired state is never a queued "task").
	resp := publishResponse{Version: version, ContentHash: hash}
	if a.hub.IsOnline(id) {
		if err := a.transport.PushDesiredState(r.Context(), id); err != nil {
			resp.PushError = desiredPushAdminMessage(err)
		} else {
			resp.Pushed = true
		}
	}
	writeTransportJSON(w, http.StatusOK, resp)
}

func (a *AdminAPI) pushDesiredState(w http.ResponseWriter, r *http.Request, id uint) {
	if !a.hub.IsOnline(id) {
		writeTransportError(w, http.StatusConflict, "node is offline")
		return
	}
	if err := a.transport.PushDesiredState(r.Context(), id); err != nil {
		if errors.Is(err, ErrAuthoritativeConfigCapabilityRequired) || errors.Is(err, ErrDesiredSnapshotRepublishRequired) {
			writeTransportError(w, http.StatusConflict, desiredPushAdminMessage(err))
			return
		}
		writeTransportError(w, http.StatusInternalServerError, desiredPushAdminMessage(err))
		return
	}
	a.audit.Write(AuditEntry{Action: "desired_push", TargetNode: id, Result: "pushed", Source: a.adminIdentity})
	writeTransportJSON(w, http.StatusOK, map[string]any{"pushed": true})
}

func desiredPushAdminMessage(err error) string {
	switch {
	case errors.Is(err, ErrAuthoritativeConfigCapabilityRequired):
		return "node agent must be upgraded before authoritative config can be pushed"
	case errors.Is(err, ErrDesiredSnapshotRepublishRequired):
		return "published snapshot is legacy; publish the current draft again"
	case errors.Is(err, ErrDesiredSnapshotHashMismatch):
		return "published snapshot failed integrity verification; publish again"
	default:
		return "push desired state failed"
	}
}

// --- tasks ---

type dispatchTaskRequest struct {
	Type   string                  `json:"type"`
	Params controlproto.TaskParams `json:"params"`
}

func (a *AdminAPI) tasksRoute(w http.ResponseWriter, r *http.Request, id uint, rest []string) {
	// /api/admin/nodes/{id}/tasks            POST -> dispatch
	// /api/admin/nodes/{id}/tasks/{taskID}   GET  -> result
	if len(rest) == 0 && r.Method == http.MethodPost {
		a.dispatchTask(w, r, id)
		return
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		a.getTask(w, rest[0])
		return
	}
	writeTransportError(w, http.StatusNotFound, "not found")
}

func (a *AdminAPI) dispatchTask(w http.ResponseWriter, r *http.Request, id uint) {
	var req dispatchTaskRequest
	if err := decodeAdminJSON(r, &req); err != nil {
		writeTransportError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	taskID, err := a.tasks.Dispatch(r.Context(), id, controlproto.TaskType(req.Type), req.Params, a.adminIdentity)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnsupportedTaskType):
			writeTransportError(w, http.StatusBadRequest, "unsupported task type")
		case errors.Is(err, ErrNodeOffline):
			writeTransportError(w, http.StatusConflict, "node is offline")
		case errors.Is(err, ErrTooManyInFlight):
			writeTransportError(w, http.StatusTooManyRequests, "node has too many tasks in flight")
		default:
			writeTransportError(w, http.StatusInternalServerError, "dispatch task failed")
		}
		return
	}
	writeTransportJSON(w, http.StatusAccepted, map[string]any{"task_id": taskID})
}

func (a *AdminAPI) getTask(w http.ResponseWriter, taskID string) {
	task, err := a.tasks.GetTask(taskID)
	if err != nil {
		writeTransportError(w, http.StatusNotFound, "task not found")
		return
	}
	writeTransportJSON(w, http.StatusOK, task)
}

// --- in-place migration (import) ---

type importConfirmResponse struct {
	Version     int64  `json:"version"`
	ContentHash string `json:"content_hash"`
}

// importRoute handles the in-place migration endpoints:
//
//	POST /api/admin/nodes/{id}/import          -> request import (read-only report)
//	GET  /api/admin/nodes/{id}/import          -> pending import summary
//	POST /api/admin/nodes/{id}/import/confirm  -> adopt + publish v1 baseline
//	POST /api/admin/nodes/{id}/import/abort    -> discard pending import
//
// The red line: nothing is written to the node at any point, and nothing is
// written to the panel's authoritative tables until confirm.
func (a *AdminAPI) importRoute(w http.ResponseWriter, r *http.Request, id uint, rest []string) {
	if a.migration == nil {
		writeTransportError(w, http.StatusNotImplemented, "migration is not configured")
		return
	}
	if _, ok := a.loadNode(w, id); !ok {
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			a.requestImport(w, r, id)
		case http.MethodGet:
			a.pendingImport(w, id)
		default:
			writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if len(rest) == 1 && r.Method == http.MethodPost {
		switch rest[0] {
		case "confirm":
			a.confirmImport(w, id)
		case "abort":
			a.abortImport(w, id)
		default:
			writeTransportError(w, http.StatusNotFound, "not found")
		}
		return
	}
	writeTransportError(w, http.StatusNotFound, "not found")
}

func (a *AdminAPI) requestImport(w http.ResponseWriter, r *http.Request, id uint) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	summary, err := a.migration.RequestImport(ctx, a.hub, id)
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeOffline):
			writeTransportError(w, http.StatusConflict, "node is offline")
		case errors.Is(err, ErrImportTimeout):
			writeTransportError(w, http.StatusGatewayTimeout, "node did not report in time")
		case errors.Is(err, ErrAlreadyManaged):
			writeTransportError(w, http.StatusConflict, "node already has managed config")
		case errors.Is(err, ErrImportPending), errors.Is(err, ErrImportInProgress):
			writeTransportError(w, http.StatusConflict, err.Error())
		default:
			writeTransportError(w, http.StatusInternalServerError, "request import failed")
		}
		return
	}
	writeTransportJSON(w, http.StatusOK, summary)
}

func (a *AdminAPI) pendingImport(w http.ResponseWriter, id uint) {
	summary, ok := a.migration.PendingSummary(id)
	if !ok {
		writeTransportError(w, http.StatusNotFound, "no pending import")
		return
	}
	writeTransportJSON(w, http.StatusOK, summary)
}

func (a *AdminAPI) confirmImport(w http.ResponseWriter, id uint) {
	version, hash, err := a.migration.Confirm(id, a.adminIdentity)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoPendingImport):
			writeTransportError(w, http.StatusNotFound, "no pending import")
		case errors.Is(err, ErrAlreadyManaged):
			writeTransportError(w, http.StatusConflict, "node already has managed config")
		case errors.Is(err, ErrImportInProgress):
			writeTransportError(w, http.StatusConflict, "import confirmation is already in progress")
		default:
			writeTransportError(w, http.StatusInternalServerError, "confirm import failed")
		}
		return
	}
	writeTransportJSON(w, http.StatusOK, importConfirmResponse{Version: version, ContentHash: hash})
}

func (a *AdminAPI) abortImport(w http.ResponseWriter, id uint) {
	if err := a.migration.Abort(id, a.adminIdentity); err != nil {
		if errors.Is(err, ErrImportInProgress) {
			writeTransportError(w, http.StatusConflict, "import operation is already in progress")
			return
		}
		writeTransportError(w, http.StatusNotFound, "no pending import")
		return
	}
	writeTransportJSON(w, http.StatusOK, map[string]any{"aborted": true})
}

// --- helpers ---

func (a *AdminAPI) loadNode(w http.ResponseWriter, id uint) (Node, bool) {
	var node Node
	if err := a.db.Where("id = ?", id).First(&node).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeTransportError(w, http.StatusNotFound, "node not found")
			return Node{}, false
		}
		writeTransportError(w, http.StatusInternalServerError, "query node failed")
		return Node{}, false
	}
	return node, true
}

func (a *AdminAPI) nodeToResponse(n Node) nodeResponse {
	resp := nodeResponse{
		ID: n.ID, DisplayName: n.DisplayName, Status: n.Status, CreatedAt: n.CreatedAt,
	}
	resp.Online = a.hub.IsOnline(n.ID)
	var st NodeState
	if err := a.db.Where("node_id = ?", n.ID).First(&st).Error; err == nil {
		resp.AppliedVersion = st.AppliedVersion
		resp.SyncState = st.SyncState
		resp.LastError = st.LastError
		resp.LastHeartbeat = st.LastHeartbeat
	}
	var desired DesiredConfig
	desiredErr := a.db.Where("node_id = ?", n.ID).First(&desired).Error
	if desiredErr == nil {
		resp.DesiredVersion = desired.Version
		// Failed and drift states carry useful evidence that must remain visible
		// until a later successful reconciliation. For every other state, a
		// published version/hash that is not the last observed apply is waiting,
		// even when the node was offline and no push attempt occurred.
		if desired.Version > 0 && resp.SyncState != SyncStateFailed && resp.SyncState != SyncStateDrift &&
			(st.AppliedVersion != desired.Version || st.ContentHash != desired.ContentHash) {
			resp.SyncState = SyncStateWaiting
		}
		if desired.Version <= 0 && resp.SyncState == SyncStateSynced {
			resp.SyncState = SyncStateWaiting
		}
	} else if errors.Is(desiredErr, gorm.ErrRecordNotFound) && resp.SyncState == SyncStateSynced {
		// Synced means equality with a published target. With no desired row there
		// is no target to match, even if an older persisted NodeState says synced.
		resp.SyncState = SyncStateWaiting
	}
	dirty, publishRequired, err := a.desired.DraftStatus(n.ID)
	if err == nil {
		resp.DraftDirty = dirty
		resp.PublishRequired = publishRequired
	} else {
		resp.PublishRequired = resp.DesiredVersion > 0
	}
	if resp.PublishRequired && resp.SyncState != SyncStateFailed && resp.SyncState != SyncStateDrift {
		resp.SyncState = SyncStateFailed
		if resp.LastError == "" {
			resp.LastError = "published snapshot is unavailable and must be republished"
		}
	}
	return resp
}

// decodeAdminJSON decodes a JSON body with a 1 MiB cap and rejects unknown
// fields, matching the webadmin decode contract.
func decodeAdminJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra struct{}
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("unexpected extra json")
	}
	return nil
}
