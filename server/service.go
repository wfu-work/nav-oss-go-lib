package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	oss "github.com/wfu-work/nav-oss-go-lib"
)

const (
	maxOperationRecords  = 500
	defaultAdminUsername = "admin"
	defaultAdminPassword = "navfirst.com"
)

// Service owns console authentication, provider configuration and live stores.
type Service struct {
	mu               sync.Mutex
	config           Config
	state            persistedState
	stores           map[string]oss.Store
	sessions         map[string]time.Time
	adminHash        []byte
	startedAt        time.Time
	runtimeMu        sync.Mutex
	runtimeProcesses map[string]*runtimeProcess
	runtimeTasks     map[string]*runtimeTask
}

type storedObject struct {
	ProviderID    string    `json:"providerId"`
	ProviderName  string    `json:"providerName"`
	Key           string    `json:"key"`
	Size          int64     `json:"size"`
	ETag          string    `json:"etag"`
	ContentType   string    `json:"contentType"`
	IsPrefix      bool      `json:"isPrefix"`
	MetadataCount int       `json:"metadataCount"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type objectDetail struct {
	storedObject
	Metadata           map[string]string `json:"metadata"`
	SHA256             string            `json:"sha256,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	StorageClass       string            `json:"storageClass,omitempty"`
	Version            string            `json:"version,omitempty"`
}

type dashboard struct {
	ProviderCount        int         `json:"providerCount"`
	HealthyProviderCount int         `json:"healthyProviderCount"`
	ObjectCount          int64       `json:"objectCount"`
	UsedBytes            int64       `json:"usedBytes"`
	MultipartCount       int         `json:"multipartCount"`
	StaleMultipartCount  int         `json:"staleMultipartCount"`
	FailedOperationCount int         `json:"failedOperationCount"`
	Providers            []Provider  `json:"providers"`
	RecentOperations     []Operation `json:"recentOperations"`
	UpdatedAt            time.Time   `json:"updatedAt"`
}

// New creates the management service and restores its persisted state.
func New(config Config) (*Service, error) {
	config.StateFile = strings.TrimSpace(config.StateFile)
	if config.StateFile == "" {
		config.StateFile = filepath.Join("data", "nav-oss-state.json")
	}
	config.RuntimeDir = strings.TrimSpace(config.RuntimeDir)
	if config.RuntimeDir == "" {
		config.RuntimeDir = filepath.Join(filepath.Dir(config.StateFile), "runtimes")
	}
	config.AdminUsername = strings.TrimSpace(config.AdminUsername)
	if config.SessionTTL <= 0 {
		config.SessionTTL = 24 * time.Hour
	}

	service := &Service{
		config:           config,
		stores:           make(map[string]oss.Store),
		sessions:         make(map[string]time.Time),
		startedAt:        time.Now().UTC(),
		runtimeProcesses: make(map[string]*runtimeProcess),
		runtimeTasks:     make(map[string]*runtimeTask),
	}
	if err := service.loadState(); err != nil {
		return nil, err
	}
	if service.config.AdminUsername == "" {
		service.config.AdminUsername = strings.TrimSpace(service.state.AdminUsername)
	}
	if service.config.AdminUsername == "" {
		service.config.AdminUsername = defaultAdminUsername
	}
	service.state.AdminUsername = service.config.AdminUsername
	if err := service.configureAdmin(); err != nil {
		return nil, err
	}
	service.applySettingsDefaults()
	service.reconcileInstallations()

	service.mu.Lock()
	service.restoreStoresLocked(context.Background())
	err := service.persistLocked()
	service.mu.Unlock()
	if err != nil {
		service.Close()
		return nil, err
	}
	return service, nil
}

// Close releases all currently open provider stores.
func (s *Service) Close() error {
	s.runtimeMu.Lock()
	for _, task := range s.runtimeTasks {
		task.cancel()
	}
	s.runtimeMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	var closeError error
	for id, store := range s.stores {
		if err := store.Close(); err != nil {
			closeError = errors.Join(closeError, fmt.Errorf("close provider %s: %w", id, err))
		}
	}
	s.stores = make(map[string]oss.Store)
	return closeError
}

func (s *Service) loadState() error {
	content, err := os.ReadFile(s.config.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read nav-oss state: %w", err)
	}
	if err = json.Unmarshal(content, &s.state); err != nil {
		return fmt.Errorf("decode nav-oss state: %w", err)
	}
	return nil
}

func (s *Service) configureAdmin() error {
	var hash []byte
	var err error
	switch {
	case strings.TrimSpace(s.config.AdminPassword) != "":
		hash, err = bcrypt.GenerateFromPassword([]byte(s.config.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash admin password: %w", err)
		}
	case strings.TrimSpace(s.config.AdminPasswordHash) != "":
		hash = []byte(strings.TrimSpace(s.config.AdminPasswordHash))
		if _, err = bcrypt.Cost(hash); err != nil {
			return fmt.Errorf("invalid admin password hash: %w", err)
		}
	case strings.TrimSpace(s.state.AdminPasswordHash) != "":
		hash = []byte(s.state.AdminPasswordHash)
	default:
		// Only use the built-in password for a new state. Once persisted or changed,
		// the stored bcrypt hash takes precedence on subsequent starts.
		hash, err = bcrypt.GenerateFromPassword([]byte(defaultAdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash default admin password: %w", err)
		}
	}
	s.adminHash = hash
	s.state.AdminPasswordHash = string(hash)
	return nil
}

func (s *Service) applySettingsDefaults() {
	if s.state.Settings.PresignGetExpiryMinutes <= 0 {
		s.state.Settings.PresignGetExpiryMinutes = 30
	}
	if s.state.Settings.PresignPutExpiryMinutes <= 0 {
		s.state.Settings.PresignPutExpiryMinutes = 15
	}
	if s.state.Settings.ListPageSize <= 0 {
		s.state.Settings.ListPageSize = 20
	}
	if s.state.Settings.MaxListPageSize <= 0 {
		s.state.Settings.MaxListPageSize = 1000
	}
	if !s.state.Settings.RequireDeleteConfirmation {
		// A newly created zero-value state should use the safer default.
		s.state.Settings.RequireDeleteConfirmation = len(s.state.Providers) == 0
	}
}

func (s *Service) persistLocked() error {
	directory := filepath.Dir(s.config.StateFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create nav-oss state directory: %w", err)
	}
	content, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode nav-oss state: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".nav-oss-state-*")
	if err != nil {
		return fmt.Errorf("create temporary nav-oss state: %w", err)
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	if err = temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary nav-oss state: %w", err)
	}
	if _, err = temporary.Write(content); err != nil {
		return fmt.Errorf("write temporary nav-oss state: %w", err)
	}
	if err = temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary nav-oss state: %w", err)
	}
	if err = temporary.Close(); err != nil {
		return fmt.Errorf("close temporary nav-oss state: %w", err)
	}
	if err = os.Rename(temporaryName, s.config.StateFile); err != nil {
		return fmt.Errorf("replace nav-oss state: %w", err)
	}
	committed = true
	return nil
}

func (s *Service) restoreStoresLocked(ctx context.Context) {
	for index := range s.state.Providers {
		provider := &s.state.Providers[index]
		if !provider.Enabled {
			provider.Health = "unknown"
			continue
		}
		store, err := newStore(ctx, *provider)
		if err != nil {
			provider.Health = "offline"
			continue
		}
		s.stores[provider.ID] = store
		if provider.Health == "" || provider.Health == "offline" {
			provider.Health = "unknown"
		}
	}
}

func newStore(ctx context.Context, provider providerState) (oss.Store, error) {
	switch provider.Type {
	case string(oss.ProviderLocal):
		return oss.NewLocal(oss.LocalConfig{RootDir: provider.RootDir})
	case string(oss.ProviderS3):
		secure := !strings.HasPrefix(strings.ToLower(strings.TrimSpace(provider.Endpoint)), "http://")
		return oss.NewS3(ctx, oss.S3Config{
			Endpoint:         provider.Endpoint,
			Secure:           secure,
			Region:           provider.Region,
			Bucket:           provider.Bucket,
			Prefix:           provider.Prefix,
			AccessKeyID:      provider.AccessKeyID,
			SecretAccessKey:  provider.SecretAccessKey,
			SessionToken:     provider.SessionToken,
			CredentialMode:   oss.CredentialMode(provider.CredentialMode),
			BucketLookup:     oss.BucketLookup(provider.BucketLookup),
			AutoCreateBucket: provider.AutoCreateBucket,
			SkipBucketCheck:  provider.SkipBucketCheck,
		})
	default:
		return nil, fmt.Errorf("unsupported provider type %q", provider.Type)
	}
}

func (s *Service) login(username, password string) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if username != s.config.AdminUsername || bcrypt.CompareHashAndPassword(s.adminHash, []byte(password)) != nil {
		return "", 0, errors.New("账号或密码错误")
	}
	token, err := secureToken(32)
	if err != nil {
		return "", 0, err
	}
	s.cleanupSessionsLocked(time.Now())
	s.sessions[token] = time.Now().Add(s.config.SessionTTL)
	return token, int64(s.config.SessionTTL.Seconds()), nil
}

func (s *Service) authorized(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiresAt, ok := s.sessions[token]
	if !ok || time.Now().After(expiresAt) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *Service) logout(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *Service) changePassword(oldPassword, newPassword string) error {
	if len(newPassword) < 10 {
		return errors.New("新密码至少需要 10 个字符")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bcrypt.CompareHashAndPassword(s.adminHash, []byte(oldPassword)) != nil {
		return errors.New("当前密码不正确")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.adminHash = hash
	s.state.AdminPasswordHash = string(hash)
	s.sessions = make(map[string]time.Time)
	return s.persistLocked()
}

func (s *Service) cleanupSessionsLocked(now time.Time) {
	for token, expiresAt := range s.sessions {
		if now.After(expiresAt) {
			delete(s.sessions, token)
		}
	}
}

func secureToken(size int) (string, error) {
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		return "", fmt.Errorf("generate secure token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(content), nil
}

func newID() (string, error) {
	content := make([]byte, 16)
	if _, err := rand.Read(content); err != nil {
		return "", err
	}
	return hex.EncodeToString(content), nil
}

func (s *Service) providers(page, size int, keyword, providerType, health, enabled string) pageResult[Provider] {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	items := make([]Provider, 0, len(s.state.Providers))
	for _, provider := range s.state.Providers {
		if keyword != "" && !strings.Contains(strings.ToLower(provider.Name+" "+provider.Endpoint+" "+provider.Bucket+" "+provider.RootDir), keyword) {
			continue
		}
		if providerType != "" && provider.Type != providerType {
			continue
		}
		if health != "" && provider.Health != health {
			continue
		}
		if enabled != "" && fmt.Sprintf("%t", provider.Enabled) != enabled {
			continue
		}
		items = append(items, publicProvider(provider))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	return paginate(items, page, size)
}

func (s *Service) provider(id string) *Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.providerIndexLocked(id)
	if index < 0 {
		return nil
	}
	provider := publicProvider(s.state.Providers[index])
	return &provider
}

func (s *Service) saveProvider(ctx context.Context, id string, input ProviderInput) (Provider, error) {
	if err := validateProviderInput(input, id == ""); err != nil {
		return Provider{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	index := s.providerIndexLocked(id)
	if id != "" && index < 0 {
		return Provider{}, errors.New("Provider 不存在")
	}
	var provider providerState
	if index >= 0 {
		provider = s.state.Providers[index]
		if input.AccessKeyID == "" {
			input.AccessKeyID = provider.AccessKeyID
		}
		if input.SecretAccessKey == "" {
			input.SecretAccessKey = provider.SecretAccessKey
		}
		if input.SessionToken == "" {
			input.SessionToken = provider.SessionToken
		}
	} else {
		var err error
		id, err = newID()
		if err != nil {
			return Provider{}, err
		}
		provider = providerState{ID: id, CreatedAt: now, Health: "unknown"}
	}
	if input.CredentialMode == "" {
		input.CredentialMode = "env"
	}
	if input.BucketLookup == "" {
		input.BucketLookup = "auto"
	}
	provider.ProviderInput = input
	provider.UpdatedAt = now

	var replacement oss.Store
	if provider.Enabled {
		var err error
		replacement, err = newStore(ctx, provider)
		if err != nil {
			return Provider{}, err
		}
		provider.Health = "unknown"
	} else {
		provider.Health = "unknown"
	}
	if existing := s.stores[id]; existing != nil {
		_ = existing.Close()
		delete(s.stores, id)
	}
	if replacement != nil {
		s.stores[id] = replacement
	}

	if provider.IsDefault {
		for itemIndex := range s.state.Providers {
			s.state.Providers[itemIndex].IsDefault = false
		}
		s.state.Settings.DefaultProviderID = id
	}
	if index >= 0 {
		s.state.Providers[index] = provider
	} else {
		if len(s.state.Providers) == 0 {
			provider.IsDefault = true
			s.state.Settings.DefaultProviderID = id
		}
		s.state.Providers = append(s.state.Providers, provider)
	}
	s.appendOperationLocked("SAVE_PROVIDER", provider.Name, provider.Name, nil, now)
	if err := s.persistLocked(); err != nil {
		return Provider{}, err
	}
	return publicProvider(provider), nil
}

func validateProviderInput(input ProviderInput, isNew bool) error {
	if strings.TrimSpace(input.Name) == "" {
		return errors.New("Provider 名称不能为空")
	}
	switch input.Type {
	case string(oss.ProviderLocal):
		if strings.TrimSpace(input.RootDir) == "" {
			return errors.New("Local Provider 必须配置根目录")
		}
	case string(oss.ProviderS3):
		if strings.TrimSpace(input.Endpoint) == "" || strings.TrimSpace(input.Bucket) == "" {
			return errors.New("S3 Provider 必须配置 Endpoint 和 Bucket")
		}
		if input.CredentialMode == "static" && isNew && (input.AccessKeyID == "" || input.SecretAccessKey == "") {
			return errors.New("静态凭证模式必须配置 Access Key 和 Secret Key")
		}
	default:
		return errors.New("Provider 类型仅支持 local 或 s3")
	}
	return nil
}

func (s *Service) checkProvider(ctx context.Context, id string) (Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.providerIndexLocked(id)
	if index < 0 {
		return Provider{}, errors.New("Provider 不存在")
	}
	provider := &s.state.Providers[index]
	store := s.stores[id]
	if !provider.Enabled || store == nil {
		return Provider{}, errors.New("Provider 未启用")
	}
	started := time.Now()
	healthContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := store.Health(healthContext)
	checkedAt := time.Now().UTC()
	provider.LastCheckedAt = &checkedAt
	if err != nil {
		provider.Health = "offline"
	} else {
		provider.Health = "healthy"
	}
	s.appendOperationLocked("HEALTH_CHECK", provider.Name, provider.Name, err, started)
	if persistErr := s.persistLocked(); persistErr != nil && err == nil {
		err = persistErr
	}
	return publicProvider(*provider), err
}

func (s *Service) setProviderEnabled(ctx context.Context, id string, enabled bool) (Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.providerIndexLocked(id)
	if index < 0 {
		return Provider{}, errors.New("Provider 不存在")
	}
	provider := &s.state.Providers[index]
	if enabled && !provider.Enabled {
		store, err := newStore(ctx, *provider)
		if err != nil {
			return Provider{}, err
		}
		s.stores[id] = store
	}
	if !enabled {
		if store := s.stores[id]; store != nil {
			_ = store.Close()
			delete(s.stores, id)
		}
		provider.Health = "unknown"
	}
	provider.Enabled = enabled
	provider.UpdatedAt = time.Now().UTC()
	s.appendOperationLocked(map[bool]string{true: "ENABLE_PROVIDER", false: "DISABLE_PROVIDER"}[enabled], provider.Name, provider.Name, nil, time.Now())
	if err := s.persistLocked(); err != nil {
		return Provider{}, err
	}
	return publicProvider(*provider), nil
}

func publicProvider(provider providerState) Provider {
	health := provider.Health
	if health == "" {
		health = "unknown"
	}
	return Provider{
		ID: provider.ID, Name: provider.Name, Type: provider.Type, Endpoint: provider.Endpoint,
		Region: provider.Region, Bucket: provider.Bucket, Prefix: provider.Prefix, RootDir: provider.RootDir,
		CredentialMode: provider.CredentialMode, BucketLookup: provider.BucketLookup, Enabled: provider.Enabled,
		Health: health, IsDefault: provider.IsDefault, ObjectCount: provider.ObjectCount,
		UsedBytes: provider.UsedBytes, LastCheckedAt: provider.LastCheckedAt, UpdatedAt: provider.UpdatedAt,
	}
}

func (s *Service) providerIndexLocked(id string) int {
	for index := range s.state.Providers {
		if s.state.Providers[index].ID == id {
			return index
		}
	}
	return -1
}

func (s *Service) resolveProviderLocked(id string) (*providerState, oss.Store, error) {
	if id == "" {
		id = s.state.Settings.DefaultProviderID
	}
	if id == "" {
		for _, provider := range s.state.Providers {
			if provider.Enabled {
				id = provider.ID
				break
			}
		}
	}
	index := s.providerIndexLocked(id)
	if index < 0 {
		return nil, nil, errors.New("请先配置可用的 Provider")
	}
	provider := &s.state.Providers[index]
	store := s.stores[id]
	if !provider.Enabled || store == nil {
		return nil, nil, errors.New("Provider 未启用或初始化失败")
	}
	return provider, store, nil
}

func (s *Service) listObjects(ctx context.Context, providerID, prefix, keyword string, page, size int) (pageResult[storedObject], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, store, err := s.resolveProviderLocked(providerID)
	if err != nil {
		return pageResult[storedObject]{}, err
	}
	started := time.Now()
	limit := s.state.Settings.MaxListPageSize
	if limit <= 0 {
		limit = 1000
	}
	result, err := store.List(ctx, oss.ListOptions{Prefix: prefix, Limit: limit, Recursive: true})
	s.appendOperationLocked("LIST_OBJECTS", prefix, provider.Name, err, started)
	if err != nil {
		_ = s.persistLocked()
		return pageResult[storedObject]{}, err
	}
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	items := make([]storedObject, 0, len(result.Items))
	var usedBytes int64
	var objectCount int64
	for _, item := range result.Items {
		if keyword != "" && !strings.Contains(strings.ToLower(item.Key), keyword) {
			continue
		}
		items = append(items, mapStoredObject(*provider, item))
		if !item.IsPrefix {
			objectCount++
			usedBytes += item.Size
		}
	}
	provider.ObjectCount = objectCount
	provider.UsedBytes = usedBytes
	provider.UpdatedAt = time.Now().UTC()
	pageValue := paginate(items, page, size)
	pageValue.NextCursor = result.NextCursor
	pageValue.HasMore = result.HasMore || pageValue.HasMore
	_ = s.persistLocked()
	return pageValue, nil
}

func (s *Service) statObject(ctx context.Context, providerID, key string) (objectDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, store, err := s.resolveProviderLocked(providerID)
	if err != nil {
		return objectDetail{}, err
	}
	started := time.Now()
	info, err := store.Stat(ctx, key)
	s.appendOperationLocked("STAT_OBJECT", key, provider.Name, err, started)
	_ = s.persistLocked()
	if err != nil {
		return objectDetail{}, err
	}
	return mapObjectDetail(*provider, info), nil
}

func (s *Service) openObject(ctx context.Context, providerID, key string) (io.ReadCloser, oss.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, store, err := s.resolveProviderLocked(providerID)
	if err != nil {
		return nil, oss.ObjectInfo{}, err
	}
	started := time.Now()
	info, err := store.Stat(ctx, key)
	if err == nil {
		var reader io.ReadCloser
		reader, err = store.Open(ctx, key, oss.OpenOptions{})
		if err == nil {
			s.appendOperationLocked("READ_OBJECT", key, provider.Name, nil, started)
			_ = s.persistLocked()
			return reader, info, nil
		}
	}
	s.appendOperationLocked("READ_OBJECT", key, provider.Name, err, started)
	_ = s.persistLocked()
	return nil, oss.ObjectInfo{}, err
}

func (s *Service) presignObject(ctx context.Context, put bool, request presignRequest) (presignResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, store, err := s.resolveProviderLocked(request.ProviderID)
	if err != nil {
		return presignResponse{}, err
	}
	presigner, ok := store.(oss.Presigner)
	if !ok {
		return presignResponse{}, oss.ErrUnsupported
	}
	minutes := request.ExpiryMinutes
	if minutes <= 0 {
		if put {
			minutes = s.state.Settings.PresignPutExpiryMinutes
		} else {
			minutes = s.state.Settings.PresignGetExpiryMinutes
		}
	}
	expiry := time.Duration(minutes) * time.Minute
	started := time.Now()
	var signedURL string
	if put {
		value, signErr := presigner.PresignPut(ctx, request.Key, oss.PresignPutOptions{Expiry: expiry})
		err = signErr
		if value != nil {
			signedURL = value.String()
		}
	} else {
		value, signErr := presigner.PresignGet(ctx, request.Key, oss.PresignGetOptions{
			Expiry: expiry, Version: request.Version,
			ResponseContentType:        request.ResponseContentType,
			ResponseContentDisposition: request.ResponseContentDisposition,
		})
		err = signErr
		if value != nil {
			signedURL = value.String()
		}
	}
	action := "PRESIGN_GET"
	if put {
		action = "PRESIGN_PUT"
	}
	s.appendOperationLocked(action, request.Key, provider.Name, err, started)
	_ = s.persistLocked()
	if err != nil {
		return presignResponse{}, err
	}
	return presignResponse{URL: signedURL, ExpiresAt: time.Now().Add(expiry).UTC()}, nil
}

type presignRequest struct {
	ProviderID                 string `json:"providerId"`
	Key                        string `json:"key"`
	ExpiryMinutes              int    `json:"expiryMinutes"`
	Version                    string `json:"version"`
	ResponseContentType        string `json:"responseContentType"`
	ResponseContentDisposition string `json:"responseContentDisposition"`
}

type presignResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type copyRequest struct {
	ProviderID      string            `json:"providerId"`
	SourceKey       string            `json:"sourceKey"`
	TargetKey       string            `json:"targetKey"`
	ReplaceMetadata bool              `json:"replaceMetadata"`
	Metadata        map[string]string `json:"metadata"`
	ContentType     string            `json:"contentType"`
}

func (s *Service) copyObject(ctx context.Context, move bool, request copyRequest) (objectDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, store, err := s.resolveProviderLocked(request.ProviderID)
	if err != nil {
		return objectDetail{}, err
	}
	started := time.Now()
	options := oss.CopyOptions{ReplaceMetadata: request.ReplaceMetadata, Metadata: request.Metadata, ContentType: request.ContentType}
	var info oss.ObjectInfo
	if move {
		info, err = oss.Move(ctx, store, request.SourceKey, request.TargetKey, options)
	} else if copier, ok := store.(oss.Copier); ok {
		info, err = copier.Copy(ctx, request.SourceKey, request.TargetKey, options)
	} else {
		err = oss.ErrUnsupported
	}
	action := "COPY_OBJECT"
	if move {
		action = "MOVE_OBJECT"
	}
	s.appendOperationLocked(action, request.SourceKey+" -> "+request.TargetKey, provider.Name, err, started)
	_ = s.persistLocked()
	if err != nil {
		return objectDetail{}, err
	}
	return mapObjectDetail(*provider, info), nil
}

func (s *Service) deleteObject(ctx context.Context, providerID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, store, err := s.resolveProviderLocked(providerID)
	if err != nil {
		return err
	}
	started := time.Now()
	err = store.Delete(ctx, key)
	s.appendOperationLocked("DELETE_OBJECT", key, provider.Name, err, started)
	_ = s.persistLocked()
	return err
}

func mapStoredObject(provider providerState, info oss.ObjectInfo) storedObject {
	return storedObject{
		ProviderID: provider.ID, ProviderName: provider.Name, Key: info.Key, Size: info.Size,
		ETag: info.ETag, ContentType: info.ContentType, IsPrefix: info.IsPrefix,
		MetadataCount: len(info.Metadata), UpdatedAt: info.LastModified,
	}
}

func mapObjectDetail(provider providerState, info oss.ObjectInfo) objectDetail {
	return objectDetail{
		storedObject: mapStoredObject(provider, info), Metadata: info.Metadata, SHA256: info.SHA256,
		ContentEncoding: info.ContentEncoding, ContentDisposition: info.ContentDisposition,
		CacheControl: info.CacheControl, StorageClass: info.StorageClass, Version: info.Version,
	}
}

func (s *Service) dashboard() dashboard {
	s.mu.Lock()
	defer s.mu.Unlock()
	providers := make([]Provider, 0, len(s.state.Providers))
	var healthy int
	var objectCount int64
	var usedBytes int64
	for _, provider := range s.state.Providers {
		public := publicProvider(provider)
		providers = append(providers, public)
		if provider.Health == "healthy" {
			healthy++
		}
		if provider.Enabled {
			objectCount += provider.ObjectCount
			usedBytes += provider.UsedBytes
		}
	}
	recent := append([]Operation(nil), s.state.Operations...)
	sort.Slice(recent, func(i, j int) bool { return recent[i].OccurredAt.After(recent[j].OccurredAt) })
	failed := 0
	for _, operation := range recent {
		if operation.Result == "failed" {
			failed++
		}
	}
	if len(recent) > 10 {
		recent = recent[:10]
	}
	return dashboard{
		ProviderCount: len(providers), HealthyProviderCount: healthy, ObjectCount: objectCount,
		UsedBytes: usedBytes, FailedOperationCount: failed, Providers: providers,
		RecentOperations: recent, UpdatedAt: time.Now().UTC(),
	}
}

func (s *Service) systemInfo() SystemInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	enabled := 0
	healthy := 0
	for _, provider := range s.state.Providers {
		if provider.Enabled {
			enabled++
		}
		if provider.Health == "healthy" {
			healthy++
		}
	}
	apiAddress := strings.TrimSpace(s.config.APIAddress)
	if apiAddress == "" {
		apiAddress = "embedded"
	}
	consoleAddress := strings.TrimSpace(s.config.ConsoleAddress)
	if consoleAddress == "" {
		consoleAddress = "embedded"
	}
	runtimeDir := s.config.RuntimeDir
	if absoluteRuntimeDir, err := filepath.Abs(runtimeDir); err == nil {
		runtimeDir = absoluteRuntimeDir
	}
	return SystemInfo{
		Service:              "nav-oss",
		Version:              oss.Version,
		GoVersion:            runtime.Version(),
		APIAddress:           apiAddress,
		ConsoleAddress:       consoleAddress,
		RuntimeDir:           runtimeDir,
		StartedAt:            s.startedAt,
		UptimeSeconds:        int64(time.Since(s.startedAt).Seconds()),
		SessionTTLMinutes:    int64(s.config.SessionTTL / time.Minute),
		ProviderCount:        len(s.state.Providers),
		EnabledProviderCount: enabled,
		HealthyProviderCount: healthy,
		CurrentSessionCount:  len(s.sessions),
	}
}

func (s *Service) operations(page, size int, keyword, result, action string) pageResult[Operation] {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	items := make([]Operation, 0, len(s.state.Operations))
	for _, operation := range s.state.Operations {
		if keyword != "" && !strings.Contains(strings.ToLower(operation.Resource+" "+operation.ProviderName+" "+operation.Action), keyword) {
			continue
		}
		if result != "" && operation.Result != result {
			continue
		}
		if action != "" && operation.Action != action {
			continue
		}
		items = append(items, operation)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].OccurredAt.After(items[j].OccurredAt) })
	return paginate(items, page, size)
}

func (s *Service) settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Settings
}

func (s *Service) saveSettings(settings Settings) (Settings, error) {
	if settings.PresignGetExpiryMinutes < 1 || settings.PresignGetExpiryMinutes > 1440 ||
		settings.PresignPutExpiryMinutes < 1 || settings.PresignPutExpiryMinutes > 1440 {
		return Settings{}, errors.New("临时签名有效期必须在 1 到 1440 分钟之间")
	}
	if settings.ListPageSize < 10 || settings.ListPageSize > 200 || settings.MaxListPageSize < 100 || settings.MaxListPageSize > 1000 {
		return Settings{}, errors.New("对象列表分页配置超出允许范围")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if settings.DefaultProviderID != "" && s.providerIndexLocked(settings.DefaultProviderID) < 0 {
		return Settings{}, errors.New("默认 Provider 不存在")
	}
	for index := range s.state.Providers {
		s.state.Providers[index].IsDefault = s.state.Providers[index].ID == settings.DefaultProviderID
	}
	s.state.Settings = settings
	if err := s.persistLocked(); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func (s *Service) appendOperationLocked(action, resource, providerName string, operationErr error, started time.Time) {
	operationID, err := newID()
	if err != nil {
		operationID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	result := "success"
	errorSummary := ""
	if operationErr != nil {
		result = "failed"
		errorSummary = operationErr.Error()
	}
	s.state.Operations = append(s.state.Operations, Operation{
		ID: operationID, Action: action, Resource: resource, ProviderName: providerName,
		Operator: s.config.AdminUsername, DurationMS: time.Since(started).Milliseconds(),
		Result: result, ErrorSummary: errorSummary, OccurredAt: time.Now().UTC(),
	})
	if len(s.state.Operations) > maxOperationRecords {
		s.state.Operations = append([]Operation(nil), s.state.Operations[len(s.state.Operations)-maxOperationRecords:]...)
	}
}

func paginate[T any](items []T, page, size int) pageResult[T] {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 1000 {
		size = 1000
	}
	start := (page - 1) * size
	if start > len(items) {
		start = len(items)
	}
	end := start + size
	if end > len(items) {
		end = len(items)
	}
	data := make([]T, end-start)
	copy(data, items[start:end])
	return pageResult[T]{Data: data, Total: len(items), Page: page, Size: size, HasMore: end < len(items)}
}
