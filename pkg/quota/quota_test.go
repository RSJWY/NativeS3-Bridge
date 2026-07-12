package quota

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
)

func TestManagerReserveSettleAndRelease(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "AK", SecretKey: "SK", Status: "enabled", QuotaBytes: 10}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	manager := NewManager(gdb)

	reservation, err := manager.Reserve(cred.ID, 8)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := manager.Reserve(cred.ID, 3); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over quota reserve error = %v, want ErrQuotaExceeded", err)
	}
	if err := manager.Settle(reservation, 5, 0, OpPut); err != nil {
		t.Fatalf("settle: %v", err)
	}
	released, err := manager.Reserve(cred.ID, 4)
	if err != nil {
		t.Fatalf("reserve after settlement: %v", err)
	}
	if err := manager.Release(released); err != nil {
		t.Fatalf("release: %v", err)
	}

	var got db.Credential
	if err := gdb.First(&got, cred.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != 5 {
		t.Fatalf("used_bytes = %d, want 5", got.UsedBytes)
	}
	var stat db.RequestStat
	if err := gdb.Where("credential_id = ?", cred.ID).First(&stat).Error; err != nil {
		t.Fatalf("read stat: %v", err)
	}
	if stat.PutCount != 1 || stat.BytesIn != 5 {
		t.Fatalf("unexpected stat: %+v", stat)
	}
}

func TestManagerConcurrentReservationsCannotExceedQuota(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "AK", SecretKey: "SK", Status: "enabled", QuotaBytes: 50}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	manager := NewManager(gdb)

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.Reserve(cred.ID, 10); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			} else if !errors.Is(err, ErrQuotaExceeded) {
				t.Errorf("reserve: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes != 5 {
		t.Fatalf("successful reservations = %d, want 5", successes)
	}
	var got db.Credential
	if err := gdb.First(&got, cred.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != 50 {
		t.Fatalf("used_bytes = %d, want 50", got.UsedBytes)
	}
}

func TestCheck(t *testing.T) {
	if err := Check(&auth.Identity{QuotaBytes: 10, UsedBytes: 5}, 6); err != ErrQuotaExceeded {
		t.Fatalf("check over quota error = %v, want ErrQuotaExceeded", err)
	}
	if err := Check(&auth.Identity{QuotaBytes: 10, UsedBytes: 5}, 5); err != nil {
		t.Fatalf("check exact quota: %v", err)
	}
	if err := Check(&auth.Identity{QuotaBytes: 0, UsedBytes: 100}, 1000); err != nil {
		t.Fatalf("check unlimited quota: %v", err)
	}
	if err := Check(&auth.Identity{QuotaBytes: math.MaxInt64, UsedBytes: math.MaxInt64 - 1}, 10); err != ErrQuotaExceeded {
		t.Fatalf("check overflow-safe over quota error = %v, want ErrQuotaExceeded", err)
	}
}

func TestCommitUpdatesUsageAndStats(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "AK", SecretKey: "SK", Status: "enabled"}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}

	if err := Commit(gdb, cred.ID, 10, OpPut); err != nil {
		t.Fatalf("commit put: %v", err)
	}
	if err := Commit(gdb, cred.ID, 7, OpGet); err != nil {
		t.Fatalf("commit get: %v", err)
	}
	if err := Commit(gdb, cred.ID, -3, OpDelete); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
	if err := Commit(gdb, cred.ID, 2, OpDelete); err != nil {
		t.Fatalf("commit delete with positive size: %v", err)
	}
	if err := Commit(gdb, cred.ID, -20, OpDelete); err != nil {
		t.Fatalf("commit delete below zero: %v", err)
	}

	var got db.Credential
	if err := gdb.First(&got, cred.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d, want lower bounded 0", got.UsedBytes)
	}

	var stat db.RequestStat
	if err := gdb.Where("credential_id = ?", cred.ID).First(&stat).Error; err != nil {
		t.Fatalf("read stat: %v", err)
	}
	if stat.PutCount != 1 || stat.GetCount != 1 || stat.DeleteCount != 3 || stat.BytesIn != 10 || stat.BytesOut != 7 {
		t.Fatalf("unexpected stat: %+v", stat)
	}
}

func TestCommitRejectsInvalidOperation(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "AK", SecretKey: "SK", Status: "enabled"}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if err := Commit(gdb, cred.ID, 1, Op("unknown")); err != ErrInvalidOp {
		t.Fatalf("invalid op error = %v, want ErrInvalidOp", err)
	}
}

func TestCommitConcurrentPutUsage(t *testing.T) {
	gdb := testDB(t)
	cred := db.Credential{AccessKey: "AK", SecretKey: "SK", Status: "enabled"}
	if err := gdb.Create(&cred).Error; err != nil {
		t.Fatalf("create credential: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Commit(gdb, cred.ID, 3, OpPut); err != nil {
				t.Errorf("commit put: %v", err)
			}
		}()
	}
	wg.Wait()

	var got db.Credential
	if err := gdb.First(&got, cred.ID).Error; err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if got.UsedBytes != 60 {
		t.Fatalf("used_bytes = %d, want 60", got.UsedBytes)
	}

	var stat db.RequestStat
	if err := gdb.Where("credential_id = ?", cred.ID).First(&stat).Error; err != nil {
		t.Fatalf("read stat: %v", err)
	}
	if stat.PutCount != 20 || stat.BytesIn != 60 {
		t.Fatalf("unexpected stat: %+v", stat)
	}
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(gdb); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return gdb
}
