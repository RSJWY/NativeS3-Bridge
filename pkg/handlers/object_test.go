package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/quota"
)

func TestCommitUsageSkipsAnonymousIdentity(t *testing.T) {
	called := false
	h := &ObjectHandler{commit: func(credID uint, deltaBytes int64, op quota.Op) error {
		called = true
		return nil
	}}
	req := httptest.NewRequest("GET", "/bucket/key.txt", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.AnonymousIdentity()))

	h.commitUsage(req, 12, quota.OpGet)

	if called {
		t.Fatal("commit was called for anonymous identity")
	}
}
