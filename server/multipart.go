package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	oss "github.com/wfu-work/nav-oss-go-lib"
)

const staleMultipartAfter = 24 * time.Hour

type multipartSession struct {
	UploadID      string    `json:"uploadId"`
	ProviderID    string    `json:"providerId"`
	ProviderName  string    `json:"providerName"`
	Key           string    `json:"key"`
	PartCount     int       `json:"partCount"`
	UploadedBytes int64     `json:"uploadedBytes"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type providerSnapshot struct {
	provider providerState
	store    oss.Store
}

func (s *Service) multipartProviders(providerID string) ([]providerSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if providerID != "" {
		provider, store, err := s.resolveProviderLocked(providerID)
		if err != nil {
			return nil, err
		}
		return []providerSnapshot{{provider: *provider, store: store}}, nil
	}
	providers := make([]providerSnapshot, 0, len(s.state.Providers))
	for _, provider := range s.state.Providers {
		if !provider.Enabled {
			continue
		}
		store := s.stores[provider.ID]
		if store == nil {
			return nil, fmt.Errorf("存储源 %s 未初始化，无法查询分片会话", provider.Name)
		}
		providers = append(providers, providerSnapshot{provider: provider, store: store})
	}
	return providers, nil
}

func (s *Service) listMultipartSessions(ctx context.Context, providerID string) ([]multipartSession, error) {
	providers, err := s.multipartProviders(providerID)
	if err != nil {
		return nil, err
	}
	items := make([]multipartSession, 0)
	for _, snapshot := range providers {
		lister, ok := snapshot.store.(oss.MultipartLister)
		if !ok {
			return nil, fmt.Errorf("存储源 %s: %w", snapshot.provider.Name, oss.ErrUnsupported)
		}
		uploads, listErr := lister.ListMultipart(ctx, "")
		if listErr != nil {
			return nil, fmt.Errorf("查询存储源 %s 的分片会话: %w", snapshot.provider.Name, listErr)
		}
		for _, upload := range uploads {
			updated := upload.UpdatedAt
			if updated.IsZero() {
				updated = upload.CreatedAt
			}
			status := "active"
			if time.Since(updated) >= staleMultipartAfter {
				status = "stale"
			}
			items = append(items, multipartSession{
				UploadID: upload.ID, ProviderID: snapshot.provider.ID, ProviderName: snapshot.provider.Name,
				Key: upload.Key, PartCount: upload.PartCount, UploadedBytes: upload.UploadedBytes,
				Status: status, CreatedAt: upload.CreatedAt, UpdatedAt: updated,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			if items[i].ProviderID == items[j].ProviderID {
				return items[i].UploadID < items[j].UploadID
			}
			return items[i].ProviderID < items[j].ProviderID
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items, nil
}

func (s *Service) multipartSessions(ctx context.Context, providerID, keyword, status string, page, size int) (pageResult[multipartSession], error) {
	if status != "" && status != "active" && status != "stale" {
		return pageResult[multipartSession]{}, errors.New("不支持的分片会话状态")
	}
	items, err := s.listMultipartSessions(ctx, providerID)
	if err != nil {
		return pageResult[multipartSession]{}, err
	}
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	filtered := make([]multipartSession, 0, len(items))
	for _, item := range items {
		if (status == "" || item.Status == status) && (keyword == "" || strings.Contains(strings.ToLower(item.Key), keyword)) {
			filtered = append(filtered, item)
		}
	}
	return paginate(filtered, page, size), nil
}

func (s *Service) resolveMultipart(ctx context.Context, providerID, uploadID string) (providerSnapshot, oss.MultipartStore, oss.MultipartUpload, error) {
	if providerID == "" || uploadID == "" {
		return providerSnapshot{}, nil, oss.MultipartUpload{}, errors.New("Provider ID 和 Upload ID 不能为空")
	}
	providers, err := s.multipartProviders(providerID)
	if err != nil {
		return providerSnapshot{}, nil, oss.MultipartUpload{}, err
	}
	snapshot := providers[0]
	store, ok := snapshot.store.(oss.MultipartStore)
	lister, canList := snapshot.store.(oss.MultipartLister)
	if !ok || !canList {
		return providerSnapshot{}, nil, oss.MultipartUpload{}, oss.ErrUnsupported
	}
	uploads, err := lister.ListMultipart(ctx, "")
	if err != nil {
		return providerSnapshot{}, nil, oss.MultipartUpload{}, err
	}
	for _, upload := range uploads {
		if upload.ID == uploadID {
			return snapshot, store, upload.MultipartUpload, nil
		}
	}
	return providerSnapshot{}, nil, oss.MultipartUpload{}, oss.ErrNotFound
}

func (s *Service) multipartParts(ctx context.Context, providerID, uploadID string) ([]oss.CompletedPart, error) {
	_, store, upload, err := s.resolveMultipart(ctx, providerID, uploadID)
	if err != nil {
		return nil, err
	}
	return store.ListParts(ctx, upload)
}

func (s *Service) abortMultipart(ctx context.Context, providerID, uploadID string) error {
	snapshot, store, upload, err := s.resolveMultipart(ctx, providerID, uploadID)
	if err != nil {
		return err
	}
	started := time.Now()
	err = store.AbortMultipart(ctx, upload)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendOperationLocked("ABORT_MULTIPART", upload.Key, snapshot.provider.Name, err, started)
	if persistErr := s.persistLocked(); persistErr != nil {
		return errors.Join(err, persistErr)
	}
	return err
}
