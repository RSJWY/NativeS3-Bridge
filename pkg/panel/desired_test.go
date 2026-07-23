package panel

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

func TestPublishedSnapshotIsExactAndIndependentFromDraft(t *testing.T) {
	gdb := openTestDB(t)
	key := make([]byte, masterKeyLen)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	oldCipher, err := cipher.Encrypt("old-secret")
	if err != nil {
		t.Fatal(err)
	}
	credential := NodeCredential{NodeID: 1, AccessKey: "AKEXACT", SecretKeyCipher: oldCipher, Status: "enabled"}
	if err := gdb.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&NodeBucket{NodeID: 1, Name: "bucket-one", ACL: "private"}).Error; err != nil {
		t.Fatal(err)
	}
	authority := NewDesiredStateAuthority(gdb, cipher)
	version, hash, err := authority.Publish(1, "admin")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d", version)
	}

	var stored DesiredConfig
	if err := gdb.Where("node_id = ?", 1).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.ContentJSON, "old-secret") {
		t.Fatal("published snapshot contains plaintext secret")
	}
	var persisted persistedDesiredSnapshot
	if err := json.Unmarshal([]byte(stored.ContentJSON), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.SchemaVersion != desiredSnapshotSchemaVersion || persisted.Credentials[0].SecretKeyCipher != oldCipher {
		t.Fatalf("unexpected persisted snapshot: %+v", persisted)
	}

	newCipher, err := cipher.Encrypt("new-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Model(&NodeCredential{}).Where("id = ?", credential.ID).Updates(map[string]any{
		"secret_key_cipher": newCipher, "status": "disabled",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Model(&NodeBucket{}).Where("node_id = ? AND name = ?", 1, "bucket-one").Update("acl", "public-read").Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Where("id = ?", credential.ID).Delete(&NodeCredential{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Where("node_id = ? AND name = ?", 1, "bucket-one").Delete(&NodeBucket{}).Error; err != nil {
		t.Fatal(err)
	}

	pushable, err := authority.BuildPushable(1)
	if err != nil {
		t.Fatalf("build pushable: %v", err)
	}
	if pushable.Version != 1 || pushable.ContentHash != hash {
		t.Fatalf("pushable metadata changed: %+v", pushable)
	}
	if pushable.Content.Credentials[0].SecretKey != "old-secret" || pushable.Content.Credentials[0].Status != "enabled" {
		t.Fatalf("pushable leaked draft credential changes: %+v", pushable.Content.Credentials[0])
	}
	if pushable.Content.Buckets[0].ACL != "private" {
		t.Fatalf("pushable leaked draft bucket change: %+v", pushable.Content.Buckets[0])
	}
	dirty, required, err := authority.DraftStatus(1)
	if err != nil || !dirty || required {
		t.Fatalf("draft status = dirty=%v required=%v err=%v", dirty, required, err)
	}
}

func TestLegacyDesiredSnapshotFailsClosed(t *testing.T) {
	gdb := openTestDB(t)
	cipher, err := NewSecretCipher(make([]byte, masterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	legacy := controlproto.DesiredState{
		Credentials: []controlproto.DesiredCredential{{AccessKey: "AKLEGACY", Status: "enabled"}},
	}
	raw, _ := json.Marshal(legacy)
	if err := gdb.Create(&DesiredConfig{NodeID: 1, Version: 4, ContentJSON: string(raw), ContentHash: legacy.ContentHash()}).Error; err != nil {
		t.Fatal(err)
	}
	authority := NewDesiredStateAuthority(gdb, cipher)
	if _, err := authority.BuildPushable(1); !errors.Is(err, ErrDesiredSnapshotRepublishRequired) {
		t.Fatalf("BuildPushable error = %v", err)
	}
	dirty, required, err := authority.DraftStatus(1)
	if err != nil || dirty || !required {
		t.Fatalf("draft status = dirty=%v required=%v err=%v", dirty, required, err)
	}
}

func TestPublishedSnapshotHashMismatchFailsClosed(t *testing.T) {
	gdb := openTestDB(t)
	cipher, err := NewSecretCipher(make([]byte, masterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	authority := NewDesiredStateAuthority(gdb, cipher)
	if _, _, err := authority.Publish(1, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Model(&DesiredConfig{}).Where("node_id = ?", 1).Update("content_hash", strings.Repeat("0", 64)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := authority.BuildPushable(1); !errors.Is(err, ErrDesiredSnapshotHashMismatch) {
		t.Fatalf("BuildPushable error = %v", err)
	}
}

func TestEmptyDraftRequiresInitialPublish(t *testing.T) {
	gdb := openTestDB(t)
	cipher, err := NewSecretCipher(make([]byte, masterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	authority := NewDesiredStateAuthority(gdb, cipher)
	dirty, required, err := authority.DraftStatus(1)
	if err != nil || !dirty || required {
		t.Fatalf("pre-publish status = dirty=%v required=%v err=%v", dirty, required, err)
	}
	version, _, err := authority.Publish(1, "admin")
	if err != nil || version != 1 {
		t.Fatalf("publish empty draft = version=%d err=%v", version, err)
	}
	dirty, required, err = authority.DraftStatus(1)
	if err != nil || dirty || required {
		t.Fatalf("post-publish status = dirty=%v required=%v err=%v", dirty, required, err)
	}
}
