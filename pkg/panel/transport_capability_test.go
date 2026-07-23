package panel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"gorm.io/gorm"
)

type testSQLiteError struct {
	code int
	msg  string
}

func (e *testSQLiteError) Error() string { return e.msg }
func (e *testSQLiteError) Code() int     { return e.code }

func TestFailedAckPreservesPreviouslyAppliedVersionAndHash(t *testing.T) {
	gdb := openTestDB(t)
	if err := gdb.Create(&NodeState{NodeID: 1, AppliedVersion: 5, ContentHash: "hash-five", SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: NewHub()})
	env, err := controlproto.NewEnvelope(controlproto.TypeAck, "", controlproto.AckPayload{
		Version: 6, State: controlproto.SyncStateFailed, Error: "desired state apply failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.handleAck(&AgentConn{NodeID: 1}, env); err != nil {
		t.Fatal(err)
	}
	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.AppliedVersion != 5 || state.ContentHash != "hash-five" || state.SyncState != SyncStateFailed {
		t.Fatalf("failed ack state = %+v", state)
	}
}

func TestSyncedAckWithUnexpectedHashIsRecordedAsDrift(t *testing.T) {
	gdb := openTestDB(t)
	if err := gdb.Create(&DesiredConfig{NodeID: 1, Version: 3, ContentJSON: `{}`, ContentHash: "expected"}).Error; err != nil {
		t.Fatal(err)
	}
	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: NewHub()})
	env, err := controlproto.NewEnvelope(controlproto.TypeAck, "", controlproto.AckPayload{
		Version: 3, State: controlproto.SyncStateSynced, ContentHash: "unexpected",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.handleAck(&AgentConn{NodeID: 1}, env); err != nil {
		t.Fatal(err)
	}
	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.SyncState != SyncStateDrift || state.AppliedVersion != 3 || state.ContentHash != "unexpected" || state.LastError == "" {
		t.Fatalf("mismatched ack state = %+v", state)
	}
}

func TestNodeStateUpsertRetriesTransientSQLiteBusy(t *testing.T) {
	gdb := openTestDB(t)
	if err := gdb.Create(&NodeState{NodeID: 1, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: NewHub()})
	done := make(chan error, 1)
	go func() {
		done <- transport.upsertNodeState(1, map[string]any{
			"sync_state": SyncStateWaiting, "last_error": "", "updated_at": nowUTC(),
		})
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	committed = true
	if err := <-done; err != nil {
		t.Fatalf("upsert after transient SQLite lock: %v", err)
	}

	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.SyncState != SyncStateWaiting {
		t.Fatalf("state after retry = %+v", state)
	}
}

func TestNodeStateUpsertRetriesOnlySQLiteBusyCodes(t *testing.T) {
	gdb := openTestDB(t)
	var attempts atomic.Int32
	const callbackName = "test:transient_sqlite_busy"
	if err := gdb.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if attempts.Add(1) <= 2 {
			tx.AddError(&testSQLiteError{code: 5, msg: "forced SQLITE_BUSY"})
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer gdb.Callback().Create().Remove(callbackName)

	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: NewHub()})
	if err := transport.upsertNodeState(1, map[string]any{"sync_state": SyncStateWaiting}); err != nil {
		t.Fatalf("upsert after forced busy errors: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}

	for _, code := range []int{5, 6, 517} {
		if !isSQLiteBusyError(&testSQLiteError{code: code, msg: "coded"}) {
			t.Fatalf("SQLite code %d was not retryable", code)
		}
	}
	if isSQLiteBusyError(&testSQLiteError{code: 19, msg: "constraint"}) {
		t.Fatal("constraint error was treated as retryable")
	}
	if isSQLiteBusyError(errors.New("database is locked by application policy")) {
		t.Fatal("string lookalike was treated as SQLite busy")
	}
}

func TestNodeStateUpsertBusyRetryIsBounded(t *testing.T) {
	gdb := openTestDB(t)
	var attempts atomic.Int32
	const callbackName = "test:persistent_sqlite_busy"
	if err := gdb.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		attempts.Add(1)
		tx.AddError(&testSQLiteError{code: 5, msg: "persistent SQLITE_BUSY"})
	}); err != nil {
		t.Fatal(err)
	}
	defer gdb.Callback().Create().Remove(callbackName)

	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: NewHub()})
	err := transport.upsertNodeState(1, map[string]any{"sync_state": SyncStateWaiting})
	if err == nil || !isSQLiteBusyError(err) {
		t.Fatalf("persistent busy error = %v", err)
	}
	if got, want := attempts.Load(), int32(nodeStateMaxBusyRetries+1); got != want {
		t.Fatalf("attempts = %d, want %d", got, want)
	}
}

func TestNodeStateUpsertStopsBusyRetryOnContextCancellation(t *testing.T) {
	gdb := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	const callbackName = "test:cancel_sqlite_busy"
	if err := gdb.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		attempts.Add(1)
		cancel()
		tx.AddError(&testSQLiteError{code: 5, msg: "busy before cancellation"})
	}); err != nil {
		t.Fatal(err)
	}
	defer gdb.Callback().Create().Remove(callbackName)

	transport := NewTransportServer(TransportDeps{DB: gdb.WithContext(ctx), Hub: NewHub()})
	err := transport.upsertNodeState(1, map[string]any{"sync_state": SyncStateWaiting})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retry error = %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts after cancellation = %d, want 1", got)
	}
}

func TestPushDesiredStateRequiresAuthoritativeCapability(t *testing.T) {
	gdb := openTestDB(t)
	cipher, err := NewSecretCipher(make([]byte, masterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&Node{ID: 1, DisplayName: "node", Status: NodeStatusActive}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewDesiredStateAuthority(gdb, cipher).Publish(1, "admin"); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	conn := &AgentConn{NodeID: 1}
	hub.Register(1, conn)
	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: hub, Cipher: cipher})
	err = transport.PushDesiredState(context.Background(), 1)
	if !errors.Is(err, ErrAuthoritativeConfigCapabilityRequired) {
		t.Fatalf("PushDesiredState error = %v", err)
	}
	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.SyncState != SyncStateFailed || !strings.Contains(state.LastError, "upgrade") {
		t.Fatalf("node state = %+v", state)
	}
}

func TestPushDesiredStateRejectsLegacySnapshotBeforeSend(t *testing.T) {
	gdb := openTestDB(t)
	cipher, err := NewSecretCipher(make([]byte, masterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	legacy := controlproto.DesiredState{}
	raw, _ := json.Marshal(legacy)
	if err := gdb.Create(&DesiredConfig{NodeID: 1, Version: 1, ContentJSON: string(raw), ContentHash: legacy.ContentHash()}).Error; err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	hub.Register(1, &AgentConn{NodeID: 1, Capabilities: []string{controlproto.CapabilityAuthoritativeConfigV1}})
	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: hub, Cipher: cipher})
	if err := transport.PushDesiredState(context.Background(), 1); !errors.Is(err, ErrDesiredSnapshotRepublishRequired) {
		t.Fatalf("PushDesiredState error = %v", err)
	}
	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.SyncState != SyncStateFailed || !strings.Contains(state.LastError, "republished") {
		t.Fatalf("node state = %+v", state)
	}
}

func TestReplacedConnectionCannotMarkNewConnectionOffline(t *testing.T) {
	gdb := openTestDB(t)
	if err := gdb.Create(&NodeState{NodeID: 1, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	oldConn := &AgentConn{NodeID: 1}
	newConn := &AgentConn{NodeID: 1}
	hub.Register(1, oldConn)
	hub.Register(1, newConn)
	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: hub})

	transport.disconnect(oldConn)
	if current, ok := hub.Get(1); !ok || current != newConn {
		t.Fatalf("new connection was unregistered: current=%p ok=%v", current, ok)
	}
	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if !state.Online {
		t.Fatal("old connection marked the replacement offline")
	}

	transport.disconnect(newConn)
	if hub.IsOnline(1) {
		t.Fatal("current connection remained registered after disconnect")
	}
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Online {
		t.Fatal("last connection disconnect did not mark node offline")
	}
}

func TestDisconnectRestoresOnlineWhenReplacementRegistersDuringOfflineWrite(t *testing.T) {
	gdb := openTestDB(t)
	if err := gdb.Create(&NodeState{NodeID: 1, Online: true, SyncState: SyncStateSynced}).Error; err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	oldConn := &AgentConn{NodeID: 1}
	newConn := &AgentConn{NodeID: 1}
	hub.Register(1, oldConn)
	var registered atomic.Bool
	const callbackName = "test:register_replacement_during_disconnect"
	if err := gdb.Callback().Create().Before("gorm:create").Register(callbackName, func(_ *gorm.DB) {
		if registered.CompareAndSwap(false, true) {
			hub.Register(1, newConn)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer gdb.Callback().Create().Remove(callbackName)

	transport := NewTransportServer(TransportDeps{DB: gdb, Hub: hub})
	transport.disconnect(oldConn)
	var state NodeState
	if err := gdb.Where("node_id = ?", 1).First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if current, ok := hub.Get(1); !ok || current != newConn || !state.Online {
		t.Fatalf("replacement state current=%p ok=%v node=%+v", current, ok, state)
	}
}
