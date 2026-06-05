package db

import "time"

type Credential struct {
	ID         uint   `gorm:"primaryKey"`
	AccessKey  string `gorm:"uniqueIndex;size:128;not null"`
	SecretKey  string `gorm:"size:256;not null"`
	Name       string `gorm:"size:128"`
	Status     string `gorm:"size:16;not null;default:enabled"`
	QuotaBytes int64  `gorm:"not null;default:0"`
	UsedBytes  int64  `gorm:"not null;default:0"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type RequestStat struct {
	ID           uint   `gorm:"primaryKey"`
	CredentialID uint   `gorm:"index;uniqueIndex:idx_cred_day;not null"`
	Day          string `gorm:"size:10;index;uniqueIndex:idx_cred_day;not null"`
	PutCount     int64  `gorm:"not null;default:0"`
	GetCount     int64  `gorm:"not null;default:0"`
	DeleteCount  int64  `gorm:"not null;default:0"`
	BytesIn      int64  `gorm:"not null;default:0"`
	BytesOut     int64  `gorm:"not null;default:0"`
}

type HookConfig struct {
	ID        uint   `gorm:"primaryKey"`
	URL       string `gorm:"size:512;not null"`
	Events    string `gorm:"size:256;not null"`
	Enabled   bool   `gorm:"not null;default:true"`
	CreatedAt time.Time
}
