package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
)

// ListMultipart returns durable incomplete uploads, including sessions created
// before this Store was opened. Sessions removed during enumeration are skipped.
func (s *Store) ListMultipart(ctx context.Context, prefix string) ([]MultipartInfo, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, mapLocalError("list-multipart", prefix, err)
	}
	entries, err := os.ReadDir(s.multipartRoot)
	if err != nil {
		return nil, mapLocalError("list-multipart", prefix, err)
	}
	items := make([]MultipartInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != uploadIDBytes*2 {
			continue
		}
		if err = ctx.Err(); err != nil {
			return nil, mapLocalError("list-multipart", prefix, err)
		}
		info, readErr := s.multipartInfo(ctx, entry.Name())
		if errors.Is(readErr, ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		if strings.HasPrefix(info.Key, prefix) {
			items = append(items, info)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key == items[j].Key {
			return items[i].ID < items[j].ID
		}
		return items[i].Key < items[j].Key
	})
	return items, nil
}

func (s *Store) multipartInfo(ctx context.Context, id string) (MultipartInfo, error) {
	lock := s.multipartLockForID(id)
	lock.Lock()
	defer lock.Unlock()
	if err := ensureNoSymlinkComponents(s.root, s.multipartSessionPath(id)); err != nil {
		return MultipartInfo{}, newError("list-multipart", ProviderLocal, "", ErrInvalidPart, err)
	}
	raw, err := os.ReadFile(s.multipartSessionPath(id))
	if err != nil {
		return MultipartInfo{}, mapLocalError("list-multipart/session", "", err)
	}
	var session localMultipartSession
	if err = json.Unmarshal(raw, &session); err != nil {
		return MultipartInfo{}, newError("list-multipart/session", ProviderLocal, "", ErrInvalidPart, err)
	}
	upload := MultipartUpload{ID: id, Key: session.Key, CreatedAt: session.CreatedAt}
	if err = validateMultipartUpload(upload); err != nil {
		return MultipartInfo{}, err
	}
	if session.ID != id {
		return MultipartInfo{}, newError("list-multipart/session", ProviderLocal, session.Key, ErrInvalidPart, errors.New("upload identity mismatch"))
	}
	parts, err := s.listPartsUnlocked(ctx, upload)
	if err != nil {
		return MultipartInfo{}, err
	}
	info := MultipartInfo{MultipartUpload: upload, PartCount: len(parts), UpdatedAt: session.CreatedAt}
	for _, part := range parts {
		metadata, metadataErr := s.readPartMetadata(id, part.PartNumber)
		if metadataErr != nil {
			return MultipartInfo{}, mapLocalError("list-multipart/part", session.Key, metadataErr)
		}
		info.UploadedBytes += part.Size
		if metadata.LastModified.After(info.UpdatedAt) {
			info.UpdatedAt = metadata.LastModified
		}
	}
	return info, nil
}
