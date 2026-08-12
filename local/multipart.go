package local

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const uploadIDBytes = 32

type localMultipartSession struct {
	ID        string     `json:"id"`
	Key       string     `json:"key"`
	CreatedAt time.Time  `json:"createdAt"`
	Options   PutOptions `json:"options"`
}

type localPartMetadata struct {
	PartNumber   int       `json:"partNumber"`
	ETag         string    `json:"etag"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

// CreateMultipart starts a durable local multipart upload. The upload can be
// resumed after a process restart by retaining the returned upload ID.
func (s *Store) CreateMultipart(ctx context.Context, key string, options PutOptions) (MultipartUpload, error) {
	if err := ctx.Err(); err != nil {
		return MultipartUpload{}, mapLocalError("create-multipart", key, err)
	}
	if _, err := s.pathForKey(key); err != nil {
		return MultipartUpload{}, err
	}

	for attempt := 0; attempt < 8; attempt++ {
		id, err := randomUploadID()
		if err != nil {
			return MultipartUpload{}, mapLocalError("create-multipart/id", key, err)
		}
		upload := MultipartUpload{ID: id, Key: key, CreatedAt: time.Now().UTC()}
		directory := s.multipartSessionDir(id)
		if err = os.Mkdir(directory, s.dirMode); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return MultipartUpload{}, mapLocalError("create-multipart/mkdir", key, err)
		}
		session := localMultipartSession{
			ID:        upload.ID,
			Key:       upload.Key,
			CreatedAt: upload.CreatedAt,
			Options:   clonePutOptions(options),
		}
		if err = writeJSONAtomic(s.multipartSessionPath(id), session, s.dirMode, 0o600); err != nil {
			_ = os.RemoveAll(directory)
			return MultipartUpload{}, mapLocalError("create-multipart/session", key, err)
		}
		return upload, nil
	}
	return MultipartUpload{}, newError("create-multipart", ProviderLocal, key, ErrConflict, errors.New("could not allocate upload ID"))
}

// UploadPart writes or replaces one part. Part numbers are in the S3-compatible
// range 1..10000 and the declared size must match the stream exactly.
func (s *Store) UploadPart(ctx context.Context, upload MultipartUpload, partNumber int, reader io.Reader, size int64) (CompletedPart, error) {
	if reader == nil {
		return CompletedPart{}, newError("upload-part", ProviderLocal, upload.Key, ErrInvalidPart, errors.New("reader is required"))
	}
	if partNumber < 1 || partNumber > maxMultipartParts || size < 0 {
		return CompletedPart{}, newError("upload-part", ProviderLocal, upload.Key, ErrInvalidPart, errors.New("part number must be 1..10000 and size must be non-negative"))
	}
	if err := validateMultipartUpload(upload); err != nil {
		return CompletedPart{}, err
	}

	lock := s.multipartLockForID(upload.ID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.loadMultipartSession(upload); err != nil {
		return CompletedPart{}, err
	}
	if err := ctx.Err(); err != nil {
		return CompletedPart{}, mapLocalError("upload-part", upload.Key, err)
	}

	partsDirectory := s.multipartPartsDir(upload.ID)
	if err := os.MkdirAll(partsDirectory, s.dirMode); err != nil {
		return CompletedPart{}, mapLocalError("upload-part/mkdir", upload.Key, err)
	}
	temporary, err := os.CreateTemp(partsDirectory, ".part-write-*")
	if err != nil {
		return CompletedPart{}, mapLocalError("upload-part/temp", upload.Key, err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(s.fileMode); err != nil {
		return CompletedPart{}, mapLocalError("upload-part/chmod", upload.Key, err)
	}
	hash := md5.New()
	written, err := copyWithExpectedSize(ctx, io.MultiWriter(temporary, hash), reader, size)
	if err != nil {
		return CompletedPart{}, mapLocalError("upload-part/write", upload.Key, err)
	}
	if err = ctx.Err(); err != nil {
		return CompletedPart{}, mapLocalError("upload-part", upload.Key, err)
	}
	if err = temporary.Sync(); err != nil {
		return CompletedPart{}, mapLocalError("upload-part/sync", upload.Key, err)
	}
	if err = temporary.Close(); err != nil {
		return CompletedPart{}, mapLocalError("upload-part/close", upload.Key, err)
	}

	partPath := s.multipartPartPath(upload.ID, partNumber)
	if err = replaceFile(temporaryPath, partPath); err != nil {
		return CompletedPart{}, mapLocalError("upload-part/commit", upload.Key, err)
	}
	committed = true
	stat, err := os.Stat(partPath)
	if err != nil {
		return CompletedPart{}, mapLocalError("upload-part/stat", upload.Key, err)
	}
	part := CompletedPart{
		PartNumber: partNumber,
		ETag:       hex.EncodeToString(hash.Sum(nil)),
		Size:       written,
	}
	metadata := localPartMetadata{
		PartNumber:   part.PartNumber,
		ETag:         part.ETag,
		Size:         part.Size,
		LastModified: stat.ModTime().UTC(),
	}
	if err = writeJSONAtomic(s.multipartPartMetadataPath(upload.ID, partNumber), metadata, s.dirMode, 0o600); err != nil {
		_ = os.Remove(partPath)
		return CompletedPart{}, mapLocalError("upload-part/metadata", upload.Key, err)
	}
	_ = syncDirectory(partsDirectory)
	return part, nil
}

// ListParts returns uploaded parts ordered by part number.
func (s *Store) ListParts(ctx context.Context, upload MultipartUpload) ([]CompletedPart, error) {
	if err := validateMultipartUpload(upload); err != nil {
		return nil, err
	}
	lock := s.multipartLockForID(upload.ID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.loadMultipartSession(upload); err != nil {
		return nil, err
	}
	return s.listPartsUnlocked(ctx, upload)
}

// CompleteMultipart validates and concatenates the selected parts, commits the
// final object through Put, and removes the durable upload session.
func (s *Store) CompleteMultipart(ctx context.Context, upload MultipartUpload, parts []CompletedPart) (ObjectInfo, error) {
	if err := validateMultipartUpload(upload); err != nil {
		return ObjectInfo{}, err
	}
	if err := validateCompletedParts(parts); err != nil {
		return ObjectInfo{}, newError("complete-multipart", ProviderLocal, upload.Key, ErrInvalidPart, err)
	}

	lock := s.multipartLockForID(upload.ID)
	lock.Lock()
	defer lock.Unlock()
	session, err := s.loadMultipartSession(upload)
	if err != nil {
		return ObjectInfo{}, err
	}
	actualParts, err := s.listPartsUnlocked(ctx, upload)
	if err != nil {
		return ObjectInfo{}, err
	}
	actualByNumber := make(map[int]CompletedPart, len(actualParts))
	for _, part := range actualParts {
		actualByNumber[part.PartNumber] = part
	}
	var totalSize int64
	for _, requested := range parts {
		actual, ok := actualByNumber[requested.PartNumber]
		if !ok || normalizeETag(requested.ETag) != normalizeETag(actual.ETag) || requested.Size != actual.Size {
			return ObjectInfo{}, newError("complete-multipart", ProviderLocal, upload.Key, ErrInvalidPart, fmt.Errorf("part %d does not match uploaded data", requested.PartNumber))
		}
		if actual.Size > 0 && totalSize > int64(^uint64(0)>>1)-actual.Size {
			return ObjectInfo{}, newError("complete-multipart", ProviderLocal, upload.Key, ErrInvalidPart, errors.New("combined size overflow"))
		}
		totalSize += actual.Size
	}

	temporary, err := os.CreateTemp(s.multipartSessionDir(upload.ID), ".complete-*")
	if err != nil {
		return ObjectInfo{}, mapLocalError("complete-multipart/temp", upload.Key, err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err = temporary.Chmod(s.fileMode); err != nil {
		return ObjectInfo{}, mapLocalError("complete-multipart/chmod", upload.Key, err)
	}
	for _, requested := range parts {
		if err = ctx.Err(); err != nil {
			return ObjectInfo{}, mapLocalError("complete-multipart", upload.Key, err)
		}
		partFile, openErr := os.Open(s.multipartPartPath(upload.ID, requested.PartNumber))
		if openErr != nil {
			return ObjectInfo{}, mapLocalError("complete-multipart/open-part", upload.Key, openErr)
		}
		_, copyErr := copyWithExpectedSize(ctx, temporary, partFile, requested.Size)
		closeErr := partFile.Close()
		if copyErr != nil {
			return ObjectInfo{}, mapLocalError("complete-multipart/read-part", upload.Key, copyErr)
		}
		if closeErr != nil {
			return ObjectInfo{}, mapLocalError("complete-multipart/close-part", upload.Key, closeErr)
		}
	}
	if err = temporary.Sync(); err != nil {
		return ObjectInfo{}, mapLocalError("complete-multipart/sync", upload.Key, err)
	}
	if err = temporary.Close(); err != nil {
		return ObjectInfo{}, mapLocalError("complete-multipart/close", upload.Key, err)
	}
	merged, err := os.Open(temporaryPath)
	if err != nil {
		return ObjectInfo{}, mapLocalError("complete-multipart/reopen", upload.Key, err)
	}
	info, putErr := s.Put(ctx, upload.Key, merged, totalSize, clonePutOptions(session.Options))
	closeErr := merged.Close()
	if putErr != nil {
		return ObjectInfo{}, putErr
	}
	if closeErr != nil {
		return ObjectInfo{}, mapLocalError("complete-multipart/close-merged", upload.Key, closeErr)
	}
	if err = os.RemoveAll(s.multipartSessionDir(upload.ID)); err != nil {
		return info, newError("complete-multipart/cleanup", ProviderLocal, upload.Key, ErrPartialFailure, err)
	}
	return info, nil
}

// AbortMultipart removes a local upload session. It is idempotent.
func (s *Store) AbortMultipart(ctx context.Context, upload MultipartUpload) error {
	if err := validateMultipartUpload(upload); err != nil {
		return err
	}
	lock := s.multipartLockForID(upload.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return mapLocalError("abort-multipart", upload.Key, err)
	}
	if _, err := s.loadMultipartSession(upload); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if err := os.RemoveAll(s.multipartSessionDir(upload.ID)); err != nil {
		return mapLocalError("abort-multipart", upload.Key, err)
	}
	return nil
}

func (s *Store) listPartsUnlocked(ctx context.Context, upload MultipartUpload) ([]CompletedPart, error) {
	directory := s.multipartPartsDir(upload.ID)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return []CompletedPart{}, nil
	}
	if err != nil {
		return nil, mapLocalError("list-parts", upload.Key, err)
	}
	parts := make([]CompletedPart, 0, len(entries)/2)
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return nil, mapLocalError("list-parts", upload.Key, err)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		numberText := strings.TrimSuffix(entry.Name(), ".json")
		partNumber, parseErr := strconv.Atoi(numberText)
		if parseErr != nil || partNumber < 1 || partNumber > maxMultipartParts {
			continue
		}
		metadata, readErr := s.readPartMetadata(upload.ID, partNumber)
		if readErr != nil {
			return nil, mapLocalError("list-parts/metadata", upload.Key, readErr)
		}
		stat, statErr := os.Stat(s.multipartPartPath(upload.ID, partNumber))
		if statErr != nil {
			return nil, mapLocalError("list-parts/stat", upload.Key, statErr)
		}
		if metadata.PartNumber != partNumber || metadata.Size != stat.Size() || !metadata.LastModified.Equal(stat.ModTime().UTC()) {
			return nil, newError("list-parts", ProviderLocal, upload.Key, ErrInvalidPart, fmt.Errorf("part %d metadata mismatch", partNumber))
		}
		parts = append(parts, CompletedPart{PartNumber: partNumber, ETag: metadata.ETag, Size: metadata.Size})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts, nil
}

func (s *Store) loadMultipartSession(upload MultipartUpload) (localMultipartSession, error) {
	var session localMultipartSession
	raw, err := os.ReadFile(s.multipartSessionPath(upload.ID))
	if err != nil {
		return session, mapLocalError("multipart-session", upload.Key, err)
	}
	if err = json.Unmarshal(raw, &session); err != nil {
		return session, newError("multipart-session", ProviderLocal, upload.Key, ErrInvalidPart, err)
	}
	if session.ID != upload.ID || session.Key != upload.Key {
		return localMultipartSession{}, newError("multipart-session", ProviderLocal, upload.Key, ErrInvalidPart, errors.New("upload identity mismatch"))
	}
	return session, nil
}

func (s *Store) readPartMetadata(uploadID string, partNumber int) (localPartMetadata, error) {
	var metadata localPartMetadata
	raw, err := os.ReadFile(s.multipartPartMetadataPath(uploadID, partNumber))
	if err != nil {
		return metadata, err
	}
	if err = json.Unmarshal(raw, &metadata); err != nil {
		return metadata, err
	}
	return metadata, nil
}

func (s *Store) multipartSessionDir(uploadID string) string {
	return filepath.Join(s.multipartRoot, uploadID)
}

func (s *Store) multipartSessionPath(uploadID string) string {
	return filepath.Join(s.multipartSessionDir(uploadID), "session.json")
}

func (s *Store) multipartPartsDir(uploadID string) string {
	return filepath.Join(s.multipartSessionDir(uploadID), "parts")
}

func (s *Store) multipartPartPath(uploadID string, partNumber int) string {
	return filepath.Join(s.multipartPartsDir(uploadID), fmt.Sprintf("%05d.part", partNumber))
}

func (s *Store) multipartPartMetadataPath(uploadID string, partNumber int) string {
	return filepath.Join(s.multipartPartsDir(uploadID), fmt.Sprintf("%05d.json", partNumber))
}

func randomUploadID() (string, error) {
	buffer := make([]byte, uploadIDBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func validateMultipartUpload(upload MultipartUpload) error {
	if err := validateObjectKey(upload.Key); err != nil {
		return err
	}
	if len(upload.ID) != uploadIDBytes*2 {
		return newError("validate-upload", ProviderLocal, upload.Key, ErrInvalidPart, errors.New("invalid upload ID"))
	}
	decoded, err := hex.DecodeString(upload.ID)
	if err != nil || len(decoded) != uploadIDBytes || strings.ToLower(upload.ID) != upload.ID {
		return newError("validate-upload", ProviderLocal, upload.Key, ErrInvalidPart, errors.New("invalid upload ID"))
	}
	return nil
}

func clonePutOptions(options PutOptions) PutOptions {
	options.Metadata = cloneMetadata(options.Metadata)
	return options
}
