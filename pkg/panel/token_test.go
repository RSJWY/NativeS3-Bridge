package panel

import (
	"errors"
	"testing"
	"time"
)

func TestRegistrationTokenLifecycle(t *testing.T) {
	gdb := openTestDB(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()

	plaintext, err := GenerateRegistrationToken(gdb, node.ID, 0, now)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if len(plaintext) != registrationTokenBytes*2 {
		t.Fatalf("token length = %d, want %d hex chars", len(plaintext), registrationTokenBytes*2)
	}

	// Plaintext must not be persisted; only the hash.
	var stored RegistrationToken
	if err := gdb.Where("node_id = ?", node.ID).First(&stored).Error; err != nil {
		t.Fatalf("load token: %v", err)
	}
	if stored.TokenHash == plaintext {
		t.Fatal("stored token hash must not equal plaintext")
	}
	if stored.TokenHash != hashToken(plaintext) {
		t.Fatal("stored hash must be sha256 of plaintext")
	}

	// First consume succeeds.
	if err := ConsumeRegistrationToken(gdb, node.ID, plaintext, now.Add(time.Minute)); err != nil {
		t.Fatalf("consume token: %v", err)
	}
	// Second consume fails (single use).
	if err := ConsumeRegistrationToken(gdb, node.ID, plaintext, now.Add(2*time.Minute)); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("second consume err = %v, want ErrTokenUsed", err)
	}
}

func TestConsumeRejectsWrongNode(t *testing.T) {
	gdb := openTestDB(t)
	n1 := Node{DisplayName: "n1", Status: NodeStatusActive}
	n2 := Node{DisplayName: "n2", Status: NodeStatusActive}
	if err := gdb.Create(&n1).Error; err != nil {
		t.Fatalf("create n1: %v", err)
	}
	if err := gdb.Create(&n2).Error; err != nil {
		t.Fatalf("create n2: %v", err)
	}
	now := time.Now().UTC()
	plaintext, err := GenerateRegistrationToken(gdb, n1.ID, 0, now)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	// Token belongs to n1; using it for n2 must fail and must not consume it.
	if err := ConsumeRegistrationToken(gdb, n2.ID, plaintext, now.Add(time.Minute)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("wrong-node consume err = %v, want ErrTokenInvalid", err)
	}
	// Token still usable by the correct node.
	if err := ConsumeRegistrationToken(gdb, n1.ID, plaintext, now.Add(time.Minute)); err != nil {
		t.Fatalf("correct-node consume after wrong attempt: %v", err)
	}
}

func TestConsumeRejectsExpired(t *testing.T) {
	gdb := openTestDB(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	plaintext, err := GenerateRegistrationToken(gdb, node.ID, time.Minute, now)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	// 2 minutes later the 1-minute token is expired.
	if err := ConsumeRegistrationToken(gdb, node.ID, plaintext, now.Add(2*time.Minute)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired consume err = %v, want ErrTokenExpired", err)
	}
}

func TestConsumeUnknownToken(t *testing.T) {
	gdb := openTestDB(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := ConsumeRegistrationToken(gdb, node.ID, "deadbeef", time.Now()); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("unknown token err = %v, want ErrTokenInvalid", err)
	}
}

func TestInvalidateNodeTokens(t *testing.T) {
	gdb := openTestDB(t)
	node := Node{DisplayName: "n1", Status: NodeStatusActive}
	if err := gdb.Create(&node).Error; err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	plaintext, err := GenerateRegistrationToken(gdb, node.ID, 0, now)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := InvalidateNodeTokens(gdb, node.ID, now); err != nil {
		t.Fatalf("invalidate tokens: %v", err)
	}
	if err := ConsumeRegistrationToken(gdb, node.ID, plaintext, now.Add(time.Minute)); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("consume after invalidate err = %v, want ErrTokenUsed", err)
	}
}
