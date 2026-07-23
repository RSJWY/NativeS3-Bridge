package panel

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestNodeScopedDraftCRUDAndDirtyStatus(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-one"}`)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-two"}`)

	rw := serve(api, http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"bucket-one"}`)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create bucket = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodPost, "/api/admin/nodes/2/credentials", `{"name":"wrong-node","bucket":"bucket-one"}`)
	if rw.Code != http.StatusBadRequest || !strings.Contains(rw.Body.String(), "bucket does not exist") {
		t.Fatalf("cross-node bucket bind = %d %s", rw.Code, rw.Body.String())
	}

	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app","bucket":"bucket-one","quota_bytes":1024}`)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create credential = %d %s", rw.Code, rw.Body.String())
	}
	var created credentialResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.SecretKey == "" || created.QuotaBytes != 1024 {
		t.Fatalf("create response = %+v", created)
	}

	rw = serve(api, http.MethodDelete, "/api/admin/nodes/1/buckets/bucket-one", "")
	if rw.Code != http.StatusConflict {
		t.Fatalf("delete bound bucket = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodPatch, "/api/admin/nodes/2/credentials/"+created.AccessKey, `{"status":"disabled"}`)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("cross-node credential update = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodPatch, "/api/admin/nodes/1/credentials/"+created.AccessKey, `{"name":"renamed","bucket":"","status":"disabled","quota_bytes":2048}`)
	if rw.Code != http.StatusOK {
		t.Fatalf("update credential = %d %s", rw.Code, rw.Body.String())
	}
	if strings.Contains(rw.Body.String(), created.SecretKey) || strings.Contains(rw.Body.String(), "secret_key") {
		t.Fatal("credential update leaked secret")
	}
	rw = serve(api, http.MethodDelete, "/api/admin/nodes/1/buckets/bucket-one", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("delete unbound bucket = %d %s", rw.Code, rw.Body.String())
	}

	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/webhooks", `{"url":"https://hooks.example.test/events","events":["ObjectDeleted","ObjectCreated","ObjectDeleted"],"enabled":false}`)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create webhook = %d %s", rw.Code, rw.Body.String())
	}
	var webhook nodeWebhookResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &webhook); err != nil {
		t.Fatal(err)
	}
	if webhook.Enabled || strings.Join(webhook.Events, ",") != "ObjectCreated,ObjectDeleted" {
		t.Fatalf("webhook response = %+v", webhook)
	}
	rw = serve(api, http.MethodPatch, "/api/admin/nodes/2/webhooks/"+strconv.FormatUint(uint64(webhook.ID), 10), `{"enabled":true}`)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("cross-node webhook update = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodPatch, "/api/admin/nodes/1/webhooks/"+strconv.FormatUint(uint64(webhook.ID), 10), `{"enabled":true,"events":["ObjectCreated"]}`)
	if rw.Code != http.StatusOK {
		t.Fatalf("update webhook = %d %s", rw.Code, rw.Body.String())
	}

	rw = serve(api, http.MethodGet, "/api/admin/nodes/1/rate-limit", "")
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"configured":false`) || !strings.Contains(rw.Body.String(), `"anonymous_rps":10`) {
		t.Fatalf("default rate limit = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodPut, "/api/admin/nodes/1/rate-limit", `{"anonymous_rps":0,"anonymous_burst":1,"trust_forwarded":false}`)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("invalid rate limit = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodPut, "/api/admin/nodes/1/rate-limit", `{"anonymous_rps":2.5,"anonymous_burst":4,"trust_forwarded":true}`)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"configured":true`) {
		t.Fatalf("upsert rate limit = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodDelete, "/api/admin/nodes/1/rate-limit", "")
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"configured":false`) {
		t.Fatalf("reset rate limit = %d %s", rw.Code, rw.Body.String())
	}

	rw = serve(api, http.MethodGet, "/api/admin/nodes/1", "")
	var node nodeResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	if !node.DraftDirty || node.DesiredVersion != 0 {
		t.Fatalf("pre-publish node status = %+v", node)
	}
	rw = serve(api, http.MethodPost, "/api/admin/nodes/1/desired-state", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodGet, "/api/admin/nodes/1", "")
	if err := json.Unmarshal(rw.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	if node.DraftDirty || node.PublishRequired || node.DesiredVersion != 1 {
		t.Fatalf("post-publish node status = %+v", node)
	}
	rw = serve(api, http.MethodPatch, "/api/admin/nodes/1/credentials/"+created.AccessKey, `{"quota_bytes":4096}`)
	if rw.Code != http.StatusOK {
		t.Fatalf("second credential update = %d %s", rw.Code, rw.Body.String())
	}
	rw = serve(api, http.MethodGet, "/api/admin/nodes/1", "")
	if err := json.Unmarshal(rw.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	if !node.DraftDirty || node.DesiredVersion != 1 {
		t.Fatalf("unpublished node status = %+v", node)
	}

	var audits []AuditLog
	if err := api.db.Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	for _, audit := range audits {
		encoded, _ := json.Marshal(audit)
		if strings.Contains(string(encoded), created.SecretKey) {
			t.Fatal("audit leaked credential secret")
		}
	}
}

func TestNodeDraftAPIValidationAndUnknownFields(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node"}`)
	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"Bad_Name"}`},
		{http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"valid-bucket","acl":"public-write"}`},
		{http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"valid-bucket","extra":true}`},
		{http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app","bucket":"missing-bucket"}`},
		{http.MethodPost, "/api/admin/nodes/1/webhooks", `{"url":"ftp://example.test/hook","events":["ObjectCreated"]}`},
		{http.MethodPost, "/api/admin/nodes/1/webhooks", `{"url":"https://example.test/hook","events":["Unknown"]}`},
		{http.MethodPut, "/api/admin/nodes/1/rate-limit", `{"anonymous_rps":1,"anonymous_burst":0,"trust_forwarded":false}`},
	}
	for _, tc := range tests {
		rw := serve(api, tc.method, tc.path, tc.body)
		if rw.Code != http.StatusBadRequest {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, rw.Code, rw.Body.String())
		}
		if strings.Contains(strings.ToLower(rw.Body.String()), "select ") || strings.Contains(strings.ToLower(rw.Body.String()), "sqlite") {
			t.Fatalf("validation error leaked internals: %s", rw.Body.String())
		}
	}
}

func TestNodeBucketSubroutesRejectUnsupportedMethods(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node"}`)
	serve(api, http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"bucket-one"}`)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPatch, "/api/admin/nodes/1/buckets/bucket-one"},
		{http.MethodPost, "/api/admin/nodes/1/buckets/bucket-one/acl"},
	} {
		rw := serve(api, tc.method, tc.path, `{}`)
		if rw.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, rw.Code, rw.Body.String())
		}
	}
}

func TestNodeScopedCredentialAndImportRoutesRequireExistingNode(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/admin/nodes/999/credentials"},
		{http.MethodGet, "/api/admin/nodes/999/import"},
		{http.MethodPost, "/api/admin/nodes/999/import/confirm"},
		{http.MethodPost, "/api/admin/nodes/999/import/abort"},
	} {
		rw := serve(api, tc.method, tc.path, "")
		if rw.Code != http.StatusNotFound || !strings.Contains(rw.Body.String(), "node not found") {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, rw.Code, rw.Body.String())
		}
	}
}

func TestConcurrentWebhookCreateKeepsDraftPublishable(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node"}`)

	start := make(chan struct{})
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rw := serve(api, http.MethodPost, "/api/admin/nodes/1/webhooks", `{"url":"https://hooks.example.test/events","events":["ObjectCreated"],"enabled":true}`)
			statuses <- rw.Code
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)

	got := make([]int, 0, 2)
	for status := range statuses {
		got = append(got, status)
	}
	sort.Ints(got)
	want := []int{http.StatusCreated, http.StatusConflict}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("concurrent webhook statuses = %v, want %v", got, want)
	}
	var count int64
	if err := api.db.Model(&NodeWebhook{}).Where("node_id = ?", 1).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate webhook rows = %d", count)
	}
	if _, _, err := api.desired.Publish(1, "admin"); err != nil {
		t.Fatalf("publish after concurrent create: %v", err)
	}
}

func TestConcurrentBucketDeleteAndCredentialCreateNeverLeavesDanglingBinding(t *testing.T) {
	for iteration := 0; iteration < 10; iteration++ {
		api, _ := newTestAdminAPI(t)
		serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node"}`)
		serve(api, http.MethodPost, "/api/admin/nodes/1/buckets", `{"name":"bucket-one"}`)

		start := make(chan struct{})
		statuses := make(chan int, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			statuses <- serve(api, http.MethodPost, "/api/admin/nodes/1/credentials", `{"name":"app","bucket":"bucket-one"}`).Code
		}()
		go func() {
			defer wg.Done()
			<-start
			statuses <- serve(api, http.MethodDelete, "/api/admin/nodes/1/buckets/bucket-one", "").Code
		}()
		close(start)
		wg.Wait()
		close(statuses)
		for status := range statuses {
			if status >= http.StatusInternalServerError {
				t.Fatalf("iteration %d returned status %d", iteration, status)
			}
		}

		var credentials []NodeCredential
		if err := api.db.Where("node_id = ?", 1).Find(&credentials).Error; err != nil {
			t.Fatal(err)
		}
		if len(credentials) > 1 {
			t.Fatalf("iteration %d credentials = %d", iteration, len(credentials))
		}
		if len(credentials) == 1 {
			var bucketCount int64
			if err := api.db.Model(&NodeBucket{}).Where("node_id = ? AND name = ?", 1, credentials[0].Bucket).Count(&bucketCount).Error; err != nil {
				t.Fatal(err)
			}
			if bucketCount != 1 {
				t.Fatalf("iteration %d left dangling credential %+v", iteration, credentials[0])
			}
		}
	}
}
