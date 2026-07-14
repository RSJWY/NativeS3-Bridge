package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestAdminAPI builds an AdminAPI over a migrated in-memory-ish panel DB with
// a real cipher, returning the API and its collaborators for assertions.
func newTestAdminAPI(t *testing.T) (*AdminAPI, *SecretCipher) {
	t.Helper()
	gdb := openTestDB(t)
	key := make([]byte, masterKeyLen)
	for i := range key {
		key[i] = byte(i + 7)
	}
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	hub := NewHub()
	creds := NewPanelCredentialStore(gdb, cipher)
	desired := NewDesiredStateAuthority(gdb, cipher)
	tasks := NewTaskOrchestrator(gdb, hub, 0)
	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: hub, Cipher: cipher})
	audit := NewAuditor(gdb)
	migration := NewMigrationCoordinator(gdb, cipher, desired, audit)
	return NewAdminAPI(gdb, hub, creds, desired, tasks, transport, migration, audit), cipher
}

// serve routes one request through the AdminAPI's node dispatcher.
func serve(api *AdminAPI, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rw := httptest.NewRecorder()
	switch {
	case target == "/api/admin/nodes":
		api.Nodes(rw, req)
	default:
		api.NodeByID(rw, req)
	}
	return rw
}

func TestCreateNodeAndCredentialSecretReturnedOnce(t *testing.T) {
	api, _ := newTestAdminAPI(t)

	// Create a node.
	rw := serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create node status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var node nodeResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &node); err != nil {
		t.Fatalf("decode node: %v", err)
	}

	// Create a credential: the plaintext secret must be present exactly here.
	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app","quota_bytes":0}`)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create credential status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var created credentialResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode credential: %v", err)
	}
	if created.SecretKey == "" {
		t.Fatal("create must return the plaintext secret once")
	}
	if created.AccessKey == "" {
		t.Fatal("create must return an access key")
	}
	secret := created.SecretKey

	// List credentials: the secret must NEVER appear.
	rw = serve(api, http.MethodGet, "/api/admin/nodes/1/credentials", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("list status = %d", rw.Code)
	}
	if strings.Contains(rw.Body.String(), secret) {
		t.Fatal("list credentials leaked the plaintext secret")
	}
	var listed []credentialResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].SecretKey != "" {
		t.Fatalf("list must omit secret, got %+v", listed)
	}
}

func TestRotateReturnsNewSecretOnce(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app"}`)
	var created credentialResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &created)

	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/credentials/"+created.AccessKey+"/rotate", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var rotated credentialResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	if rotated.SecretKey == "" {
		t.Fatal("rotate must return the new plaintext secret once")
	}
	if rotated.SecretKey == created.SecretKey {
		t.Fatal("rotate must produce a different secret")
	}
	if rotated.AccessKey != created.AccessKey {
		t.Fatal("rotate must preserve the access key")
	}
}

func TestAuditNeverContainsSecret(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app"}`)
	var created credentialResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &created)

	// Every audit row must be free of the plaintext secret.
	var logs []AuditLog
	if err := api.db.Find(&logs).Error; err != nil {
		t.Fatalf("load audit logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected audit rows for node/credential creation")
	}
	for _, l := range logs {
		if strings.Contains(l.TargetResource, created.SecretKey) {
			t.Fatalf("audit row leaked secret: %+v", l)
		}
	}
}

func TestPublishDesiredStateStoresMaskedSecret(t *testing.T) {
	api, cipher := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)
	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app"}`)
	var created credentialResponse
	_ = json.Unmarshal(rw.Body.Bytes(), &created)

	// Publish desired state (node offline, so no push).
	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body=%s", rw.Code, rw.Body.String())
	}

	// The stored desired_configs row must NOT contain the plaintext secret.
	var cfg DesiredConfig
	if err := api.db.Where("node_id = ?", 1).First(&cfg).Error; err != nil {
		t.Fatalf("load desired config: %v", err)
	}
	if strings.Contains(cfg.ContentJSON, created.SecretKey) {
		t.Fatal("stored desired config leaked the plaintext secret")
	}

	// But BuildPushable must re-inject the real secret for the node.
	pushable, err := api.desired.BuildPushable(1)
	if err != nil {
		t.Fatalf("build pushable: %v", err)
	}
	if len(pushable.Content.Credentials) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(pushable.Content.Credentials))
	}
	if pushable.Content.Credentials[0].SecretKey != created.SecretKey {
		t.Fatal("pushable must carry the real plaintext secret")
	}
	// And the stored hash must match the real (unmasked) content hash so the
	// node's applied hash reconciles.
	if pushable.ContentHash != pushable.Content.ContentHash() {
		t.Fatalf("stored hash %q != real content hash %q", pushable.ContentHash, pushable.Content.ContentHash())
	}
	_ = cipher
}

func TestRetireNodeRevokesAndIsIrreversible(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`)

	rw := serve(api, http.MethodDelete, "/api/admin/nodes/1", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("retire status = %d", rw.Code)
	}
	var node Node
	if err := api.db.First(&node, 1).Error; err != nil {
		t.Fatalf("load node: %v", err)
	}
	if node.Status != NodeStatusRetired {
		t.Fatalf("node status = %q, want retired", node.Status)
	}
	// A retired node rejects status updates back to active.
	rw = serve(api, http.MethodPatch, "/api/admin/nodes/1", `{"status":"active"}`)
	if rw.Code != http.StatusConflict {
		t.Fatalf("reactivate retired node status = %d, want 409", rw.Code)
	}
}
