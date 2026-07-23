package managedconfig

import (
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

func TestCanonicalWebhookEvents(t *testing.T) {
	encoded, events, err := CanonicalWebhookEvents([]string{"ObjectDeleted", "ObjectCreated", "ObjectDeleted"})
	if err != nil {
		t.Fatal(err)
	}
	if encoded != "ObjectCreated,ObjectDeleted" || strings.Join(events, ",") != encoded {
		t.Fatalf("encoded=%q events=%v", encoded, events)
	}
	if _, _, err := CanonicalWebhookEvents([]string{"Unknown"}); err == nil {
		t.Fatal("unsupported event accepted")
	}
}

func TestValidateDesiredStateReferences(t *testing.T) {
	state := controlproto.DesiredState{
		Buckets: []controlproto.DesiredBucket{{Name: "bucket-one", ACL: "private"}},
		Credentials: []controlproto.DesiredCredential{{
			AccessKey: "AK", SecretKey: "secret", Bucket: "missing-bucket", Status: "enabled",
		}},
	}
	if err := ValidateDesiredState(state); err == nil || !strings.Contains(err.Error(), "undeclared bucket") {
		t.Fatalf("validation error = %v", err)
	}
	state.Credentials[0].Bucket = "bucket-one"
	state.Credentials[0].Status = ""
	if err := ValidateDesiredState(state); err != nil {
		t.Fatalf("wire-compatible empty status rejected: %v", err)
	}
}
