package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const DefaultMetadataSuffix = ".s3meta"

type Sidecar struct {
	ETag        string            `json:"etag"`
	ContentType string            `json:"content_type"`
	Metadata    map[string]string `json:"metadata"`
	Tags        map[string]string `json:"tags"`
	Size        int64             `json:"size"`
	UploadedAt  string            `json:"uploaded_at"`
}

func WriteSidecar(objPath, suffix string, s Sidecar) error {
	if suffix == "" {
		suffix = DefaultMetadataSuffix
	}
	if s.UploadedAt == "" {
		s.UploadedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	if s.Tags == nil {
		s.Tags = map[string]string{}
	}

	path := objPath + suffix
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	encErr := json.NewEncoder(f).Encode(s)
	syncErr := f.Sync()
	closeErr := f.Close()
	if encErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return firstErr(encErr, syncErr, closeErr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func ReadSidecar(objPath, suffix string) (Sidecar, bool, error) {
	if suffix == "" {
		suffix = DefaultMetadataSuffix
	}
	data, err := os.ReadFile(objPath + suffix)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Sidecar{}, false, nil
		}
		return Sidecar{}, false, err
	}
	var s Sidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return Sidecar{}, true, err
	}
	if s.Metadata == nil {
		s.Metadata = map[string]string{}
	}
	if s.Tags == nil {
		s.Tags = map[string]string{}
	}
	return s, true, nil
}

func DeleteSidecar(objPath, suffix string) error {
	if suffix == "" {
		suffix = DefaultMetadataSuffix
	}
	err := os.Remove(objPath + suffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
