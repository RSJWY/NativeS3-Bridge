package quota

import (
	"errors"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrQuotaExceeded = errors.New("quota exceeded")

var ErrInvalidOp = errors.New("invalid quota operation")

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

		stat := statFor(credID, deltaBytes, op)
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
	})
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
