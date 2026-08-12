package local

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type localObjectMetadata struct {
	Key                string            `json:"key"`
	Size               int64             `json:"size"`
	ETag               string            `json:"etag"`
	SHA256             string            `json:"sha256"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	StorageClass       string            `json:"storageClass,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	LastModified       time.Time         `json:"lastModified"`
}

func (m localObjectMetadata) objectInfo() ObjectInfo {
	return ObjectInfo{
		Key:                m.Key,
		Size:               m.Size,
		ETag:               m.ETag,
		SHA256:             m.SHA256,
		ContentType:        m.ContentType,
		ContentEncoding:    m.ContentEncoding,
		ContentDisposition: m.ContentDisposition,
		CacheControl:       m.CacheControl,
		StorageClass:       m.StorageClass,
		Metadata:           cloneMetadata(m.Metadata),
		LastModified:       m.LastModified,
	}
}

func (s *Store) metadataPath(key string) string {
	digest := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(digest[:])
	return filepath.Join(s.metadataRoot, name[:2], name+".json")
}

func (s *Store) writeMetadata(metadata localObjectMetadata) error {
	path := s.metadataPath(metadata.Key)
	return writeJSONAtomic(path, metadata, s.dirMode, 0o600)
}

func (s *Store) readMetadata(key string) (localObjectMetadata, error) {
	var metadata localObjectMetadata
	raw, err := os.ReadFile(s.metadataPath(key))
	if err != nil {
		return metadata, err
	}
	if err = json.Unmarshal(raw, &metadata); err != nil {
		return metadata, err
	}
	if metadata.Key != key {
		return localObjectMetadata{}, errors.New("metadata key mismatch")
	}
	return metadata, nil
}

func writeJSONAtomic(path string, value any, dirMode, fileMode fs.FileMode) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".nav-oss-json-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(fileMode); err != nil {
		return err
	}
	if _, err = temporary.Write(raw); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = replaceFile(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return syncDirectory(filepath.Dir(path))
}
