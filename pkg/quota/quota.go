package quota

import (
	"context"
	"errors"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrQuotaExceeded = errors.New("quota exceeded")

var ErrInvalidOp = errors.New("invalid quota operation")

var ErrInvalidReservation = errors.New("invalid quota reservation")

type Reservation struct {
	CredentialID uint
	Bytes        int64
}

type Manager struct {
	db *gorm.DB
}

type declaredSizeContextKey struct{}

func NewManager(gdb *gorm.DB) *Manager {
	return &Manager{db: gdb}
}

func WithDeclaredSize(ctx context.Context, bytes int64) context.Context {
	return context.WithValue(ctx, declaredSizeContextKey{}, bytes)
}

func DeclaredSizeFromContext(ctx context.Context) (int64, bool) {
	bytes, ok := ctx.Value(declaredSizeContextKey{}).(int64)
	return bytes, ok
}

type Op string

const (
	OpPut    Op = "put"
	OpGet    Op = "get"
	OpDelete Op = "delete"
)

func Check(id *auth.Identity, incoming int64) error {
	if id == nil {
		return auth.NewError(auth.CodeAccessDenied)
	}
	if incoming < 0 {
		return ErrQuotaExceeded
	}
	if id.QuotaBytes > 0 && incoming > id.QuotaBytes-id.UsedBytes {
		return ErrQuotaExceeded
	}
	return nil
}

func (m *Manager) Reserve(credID uint, bytes int64) (*Reservation, error) {
	if m == nil || m.db == nil || credID == 0 || bytes < 0 {
		return nil, ErrInvalidReservation
	}
	result := m.db.Model(&db.Credential{}).
		Where("id = ? AND (quota_bytes = 0 OR used_bytes <= quota_bytes - ?)", credID, bytes).
		Update("used_bytes", gorm.Expr("used_bytes + ?", bytes))
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := m.db.Model(&db.Credential{}).Where("id = ?", credID).Count(&count).Error; err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, gorm.ErrRecordNotFound
		}
		return nil, ErrQuotaExceeded
	}
	return &Reservation{CredentialID: credID, Bytes: bytes}, nil
}

func (m *Manager) Release(reservation *Reservation) error {
	if m == nil || m.db == nil || reservation == nil || reservation.CredentialID == 0 || reservation.Bytes < 0 {
		return ErrInvalidReservation
	}
	return m.adjustUsage(reservation.CredentialID, -reservation.Bytes)
}

func (m *Manager) Settle(reservation *Reservation, actualBytes, replacedBytes int64, op Op) error {
	if m == nil || m.db == nil || reservation == nil || reservation.CredentialID == 0 || reservation.Bytes < 0 || actualBytes < 0 || replacedBytes < 0 || actualBytes-replacedBytes > reservation.Bytes || op != OpPut {
		return ErrInvalidReservation
	}
	return m.db.Transaction(func(tx *gorm.DB) error {
		if delta := actualBytes - replacedBytes - reservation.Bytes; delta != 0 {
			if err := adjustUsage(tx, reservation.CredentialID, delta); err != nil {
				return err
			}
		}
		stat := statFor(reservation.CredentialID, actualBytes, op)
		return upsertStat(tx, stat)
	})
}

func (m *Manager) Record(credID uint, bytes int64, op Op) error {
	return Commit(m.db, credID, bytes, op)
}

func (m *Manager) adjustUsage(credID uint, delta int64) error {
	return adjustUsage(m.db, credID, delta)
}

func adjustUsage(gdb *gorm.DB, credID uint, delta int64) error {
	result := gdb.Model(&db.Credential{}).
		Where("id = ?", credID).
		Update("used_bytes", gorm.Expr("CASE WHEN used_bytes + ? < 0 THEN 0 ELSE used_bytes + ? END", delta, delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func Commit(gdb *gorm.DB, credID uint, deltaBytes int64, op Op) error {
	return gdb.Transaction(func(tx *gorm.DB) error {
		usageDelta, updateUsage, err := usageDeltaFor(deltaBytes, op)
		if err != nil {
			return err
		}
		if updateUsage {
			if err := tx.Model(&db.Credential{}).
				Where("id = ?", credID).
				Update("used_bytes", gorm.Expr("CASE WHEN used_bytes + ? < 0 THEN 0 ELSE used_bytes + ? END", usageDelta, usageDelta)).Error; err != nil {
				return err
			}
		}

		return upsertStat(tx, statFor(credID, deltaBytes, op))
	})
}

func upsertStat(tx *gorm.DB, stat db.RequestStat) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "credential_id"}, {Name: "day"}},
		DoUpdates: clause.Assignments(map[string]any{
			"put_count":    gorm.Expr("put_count + ?", stat.PutCount),
			"get_count":    gorm.Expr("get_count + ?", stat.GetCount),
			"delete_count": gorm.Expr("delete_count + ?", stat.DeleteCount),
			"bytes_in":     gorm.Expr("bytes_in + ?", stat.BytesIn),
			"bytes_out":    gorm.Expr("bytes_out + ?", stat.BytesOut),
		}),
	}).Create(&stat).Error
}

func usageDeltaFor(deltaBytes int64, op Op) (int64, bool, error) {
	switch op {
	case OpPut:
		return deltaBytes, true, nil
	case OpDelete:
		if deltaBytes < 0 {
			return deltaBytes, true, nil
		}
		return -deltaBytes, true, nil
	case OpGet:
		return 0, false, nil
	default:
		return 0, false, ErrInvalidOp
	}
}

func statFor(credID uint, deltaBytes int64, op Op) db.RequestStat {
	stat := db.RequestStat{CredentialID: credID, Day: time.Now().UTC().Format("2006-01-02")}
	size := deltaBytes
	if size < 0 {
		size = -size
	}
	switch op {
	case OpPut:
		stat.PutCount = 1
		stat.BytesIn = size
	case OpGet:
		stat.GetCount = 1
		stat.BytesOut = size
	case OpDelete:
		stat.DeleteCount = 1
	}
	return stat
}
