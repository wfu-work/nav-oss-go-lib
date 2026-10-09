package server

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxRuntimeDownloadSize          = int64(512 << 20)
	runtimeDownloadMaxAttempts      = 3
	runtimeDownloadProgressInterval = 750 * time.Millisecond
	runtimeInstallTimeout           = 3 * time.Hour
	runtimeHealthTimeout            = 45 * time.Second
)

var runtimeDownloadRetryDelays = [...]time.Duration{time.Second, 2 * time.Second}

var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

type runtimeDefinition struct {
	Catalog     RuntimeCatalogItem
	DownloadURL func() (string, string, error)
}

type runtimeProcess struct {
	cmd     *exec.Cmd
	process *os.Process
	log     *os.File
	done    chan struct{}
}

type runtimeTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type runtimeDownloadProgress struct {
	Downloaded  int64
	Total       int64
	Speed       int64
	Percent     int
	Attempt     int
	MaxAttempts int
	Resumed     bool
	Step        string
}

type retryableRuntimeDownloadError struct {
	err error
}

func (downloadErr *retryableRuntimeDownloadError) Error() string {
	return downloadErr.err.Error()
}

func (downloadErr *retryableRuntimeDownloadError) Unwrap() error {
	return downloadErr.err
}

func runtimeDefinitions() []runtimeDefinition {
	return []runtimeDefinition{
		{
			Catalog: RuntimeCatalogItem{
				ID: "minio", Name: "MinIO", License: "AGPL-3.0",
				Description:    "支持接入和管理已有 MinIO 实例；官方社区版二进制下载已停止。",
				DefaultAPIPort: 9100, DefaultConsolePort: 9101,
				Verification: "官方 SHA256", Platforms: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"},
			},
			DownloadURL: minioDownloadURL,
		},
		{
			Catalog: RuntimeCatalogItem{
				ID: "rustfs", Name: "RustFS", License: "Apache-2.0",
				Description:    "Rust 实现的高性能 S3 兼容对象存储，首期支持 Linux。",
				DefaultAPIPort: 9200, DefaultConsolePort: 9201,
				Verification: "官方 HTTPS + 本地 SHA256 记录", Platforms: []string{"linux/amd64", "linux/arm64"},
			},
			DownloadURL: rustFSDownloadURL,
		},
	}
}

func (s *Service) runtimeCatalog() []RuntimeCatalogItem {
	definitions := runtimeDefinitions()
	items := make([]RuntimeCatalogItem, 0, len(definitions))
	for _, definition := range definitions {
		item := definition.Catalog
		if _, _, err := definition.DownloadURL(); err != nil {
			item.Supported = false
			item.UnsupportedReason = err.Error()
		} else {
			item.Supported = true
		}
		items = append(items, item)
	}
	return items
}

func runtimeDefinitionFor(software string) (runtimeDefinition, error) {
	for _, definition := range runtimeDefinitions() {
		if definition.Catalog.ID == software {
			if _, _, err := definition.DownloadURL(); err != nil {
				return runtimeDefinition{}, err
			}
			return definition, nil
		}
	}
	return runtimeDefinition{}, fmt.Errorf("不支持安装对象存储软件 %q", software)
}

func minioDownloadURL() (string, string, error) {
	// The official download service returns HTTP 410 for all community builds.
	// Existing binaries remain manageable through start/stop/restart.
	return "", "", errors.New("MinIO 官方已停止提供社区版二进制下载；请连接自行部署的 MinIO，或在 Linux 上安装 RustFS")
}

func rustFSDownloadURL() (string, string, error) {
	if runtime.GOOS != "linux" {
		return "", "", fmt.Errorf("RustFS 官方二进制安装当前仅支持 Linux")
	}
	architecture := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if architecture == "" {
		return "", "", fmt.Errorf("RustFS 一键安装暂不支持 linux/%s", runtime.GOARCH)
	}
	return fmt.Sprintf("https://dl.rustfs.com/artifacts/rustfs/release/rustfs-linux-%s-musl-latest.zip", architecture), "", nil
}

func (s *Service) reconcileInstallations() {
	for index := range s.state.Installations {
		installation := &s.state.Installations[index]
		if installation.DownloadURL == "" {
			if definition, err := runtimeDefinitionFor(installation.Software); err == nil {
				installation.DownloadURL, _, _ = definition.DownloadURL()
			}
		}
		switch installation.Status {
		case "queued", "downloading", "installing", "starting", "configuring", "stopping":
			installation.Status = "failed"
			installation.Step = "安装被中断"
			installation.ErrorSummary = "nav-oss 服务在任务完成前退出，可点击重试继续安装"
			installation.DownloadSpeedBytes = 0
			installation.PID = 0
			installation.UpdatedAt = time.Now().UTC()
		case "running":
			if !runtimeHealthy(*installation) {
				installation.Status = "stopped"
				installation.Step = "服务未运行"
				installation.PID = 0
				installation.UpdatedAt = time.Now().UTC()
			} else if !s.adoptRuntimeProcess(*installation) {
				installation.Step = "服务运行中（进程未接管）"
				installation.ErrorSummary = "检测到服务端口可用，但持久化进程身份校验未通过；为避免误操作，停止和重启功能已禁用"
				installation.UpdatedAt = time.Now().UTC()
			}
		}
	}
}

func (s *Service) installations() []RuntimeInstallation {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep the JSON contract stable: an empty collection must be [] instead of
	// null so clients can safely render and iterate the installation list.
	items := append(make([]RuntimeInstallation, 0, len(s.state.Installations)), s.state.Installations...)
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items
}

func (s *Service) installation(id string) *RuntimeInstallation {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.installationIndexLocked(id)
	if index < 0 {
		return nil
	}
	item := s.state.Installations[index]
	return &item
}

func (s *Service) createInstallation(input RuntimeInstallInput) (RuntimeInstallation, error) {
	input.Software = strings.ToLower(strings.TrimSpace(input.Software))
	definition, err := runtimeDefinitionFor(input.Software)
	if err != nil {
		return RuntimeInstallation{}, err
	}
	downloadURL, _, err := definition.DownloadURL()
	if err != nil {
		return RuntimeInstallation{}, err
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		input.Name = definition.Catalog.Name + " 本机实例"
	}
	if len(input.Name) > 80 {
		return RuntimeInstallation{}, errors.New("实例名称不能超过 80 个字符")
	}
	if input.APIPort == 0 {
		input.APIPort = definition.Catalog.DefaultAPIPort
	}
	if input.ConsolePort == 0 {
		input.ConsolePort = definition.Catalog.DefaultConsolePort
	}
	if err = validateRuntimePort(input.APIPort); err != nil {
		return RuntimeInstallation{}, fmt.Errorf("API 端口: %w", err)
	}
	if err = validateRuntimePort(input.ConsolePort); err != nil {
		return RuntimeInstallation{}, fmt.Errorf("控制台端口: %w", err)
	}
	if input.APIPort == input.ConsolePort {
		return RuntimeInstallation{}, errors.New("API 端口和控制台端口不能相同")
	}
	input.Bucket = strings.TrimSpace(input.Bucket)
	if input.Bucket == "" {
		input.Bucket = "nav-oss"
	}
	if !bucketNamePattern.MatchString(input.Bucket) || strings.Contains(input.Bucket, "..") {
		return RuntimeInstallation{}, errors.New("Bucket 名称不符合 S3 命名规则")
	}
	input.Region = strings.TrimSpace(input.Region)
	if input.Region == "" {
		input.Region = "us-east-1"
	}
	if !portAvailable(input.APIPort) || !portAvailable(input.ConsolePort) {
		return RuntimeInstallation{}, errors.New("安装端口已被其他进程占用")
	}
	id, err := newID()
	if err != nil {
		return RuntimeInstallation{}, err
	}
	baseDir := filepath.Join(s.config.RuntimeDir, id)
	now := time.Now().UTC()
	installation := RuntimeInstallation{
		ID: id, Software: input.Software, Name: input.Name, Status: "queued",
		Step: "等待安装", Progress: 0, APIPort: input.APIPort, ConsolePort: input.ConsolePort,
		Endpoint:   fmt.Sprintf("http://127.0.0.1:%d", input.APIPort),
		ConsoleURL: fmt.Sprintf("http://127.0.0.1:%d", input.ConsolePort),
		Bucket:     input.Bucket, Region: input.Region, DataDir: filepath.Join(baseDir, "data"),
		DownloadURL: downloadURL, Verification: definition.Catalog.Verification,
		CreatedAt: now, UpdatedAt: now,
	}
	s.mu.Lock()
	for _, existing := range s.state.Installations {
		if existing.APIPort == input.APIPort || existing.ConsolePort == input.APIPort ||
			existing.APIPort == input.ConsolePort || existing.ConsolePort == input.ConsolePort {
			s.mu.Unlock()
			return RuntimeInstallation{}, errors.New("安装端口已被现有对象存储实例占用")
		}
	}
	s.state.Installations = append(s.state.Installations, installation)
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return RuntimeInstallation{}, err
	}
	s.launchInstallation(id)
	return installation, nil
}

func validateRuntimePort(port int) error {
	if port < 1024 || port > 65535 {
		return errors.New("必须在 1024 到 65535 之间")
	}
	return nil
}

func portAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func (s *Service) launchInstallation(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeInstallTimeout)
	task := &runtimeTask{cancel: cancel, done: make(chan struct{})}
	s.runtimeMu.Lock()
	if existing := s.runtimeTasks[id]; existing != nil {
		s.runtimeMu.Unlock()
		cancel()
		return
	}
	s.runtimeTasks[id] = task
	s.runtimeMu.Unlock()
	installation := s.installation(id)
	if installation == nil || installation.Status != "queued" {
		task.cancel()
		close(task.done)
		s.runtimeMu.Lock()
		if s.runtimeTasks[id] == task {
			delete(s.runtimeTasks, id)
		}
		s.runtimeMu.Unlock()
		return
	}
	go s.runInstallation(ctx, id, task)
}

func (s *Service) runInstallation(ctx context.Context, id string, task *runtimeTask) {
	started := time.Now()
	defer func() {
		task.cancel()
		close(task.done)
		s.runtimeMu.Lock()
		if s.runtimeTasks[id] == task {
			delete(s.runtimeTasks, id)
		}
		s.runtimeMu.Unlock()
	}()
	err := s.installRuntime(ctx, id)
	if err != nil {
		// A process may have started before the health check or provider
		// registration failed. Do not leave an untracked object-storage process
		// behind after reporting the task as failed.
		_, _ = s.stopRuntimeProcess(id, 8*time.Second)
	}
	s.mu.Lock()
	index := s.installationIndexLocked(id)
	if index >= 0 {
		installation := &s.state.Installations[index]
		operation := "INSTALL_RUNTIME"
		operationErr := err
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			installation.Status = "stopped"
			installation.Step = "安装已停止"
			installation.DownloadSpeedBytes = 0
			installation.PID = 0
			installation.ErrorSummary = ""
			installation.UpdatedAt = time.Now().UTC()
			operation = "STOP_INSTALL_RUNTIME"
			operationErr = nil
		} else if err != nil {
			installation.Status = "failed"
			installation.Step = "安装失败"
			installation.DownloadSpeedBytes = 0
			installation.ErrorSummary = err.Error()
			installation.UpdatedAt = time.Now().UTC()
		}
		s.appendOperationLocked(operation, installation.Name, installation.Software, operationErr, started)
		_ = s.persistLocked()
	}
	s.mu.Unlock()
}

func (s *Service) installRuntime(ctx context.Context, id string) error {
	installation := s.installation(id)
	if installation == nil {
		return errors.New("安装任务不存在")
	}
	baseDir := filepath.Dir(installation.DataDir)
	for _, directory := range []string{baseDir, installation.DataDir, filepath.Join(baseDir, "bin"), filepath.Join(baseDir, "logs")} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("创建运行时目录: %w", err)
		}
	}
	if _, err := s.updateActiveInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "downloading"
		item.Step = "正在连接官方下载地址"
		item.Progress = 5
		item.DownloadSpeedBytes = 0
		item.DownloadAttempt = 0
		item.DownloadMaxAttempts = runtimeDownloadMaxAttempts
		item.ErrorSummary = ""
	}); err != nil {
		return err
	}
	binaryPath, checksum, version, err := installRuntimeBinary(ctx, *installation, baseDir, func(progress runtimeDownloadProgress) error {
		_, updateErr := s.updateActiveInstallation(id, func(item *RuntimeInstallation) {
			item.Status = "downloading"
			item.Step = progress.Step
			item.DownloadedBytes = progress.Downloaded
			item.DownloadSpeedBytes = progress.Speed
			if progress.Total > 0 {
				item.DownloadTotalBytes = progress.Total
			}
			if item.DownloadTotalBytes > 0 {
				item.DownloadPercent = runtimeDownloadPercent(progress.Downloaded, item.DownloadTotalBytes)
			} else {
				item.DownloadPercent = progress.Percent
			}
			item.Progress = 5 + item.DownloadPercent/2
			item.DownloadAttempt = progress.Attempt
			item.DownloadMaxAttempts = progress.MaxAttempts
			item.DownloadResumed = progress.Resumed
		})
		return updateErr
	})
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	installation, err = s.updateActiveInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "installing"
		item.Step = "准备运行目录"
		item.Progress = 60
		item.DownloadSpeedBytes = 0
		item.BinaryPath = binaryPath
		item.Checksum = checksum
		item.Version = version
	})
	if err != nil {
		return err
	}
	environment, accessKey, secretKey, err := prepareRuntimeEnvironment(*installation, baseDir)
	if err != nil {
		return err
	}
	if !runtimeHealthy(*installation) {
		if err = ctx.Err(); err != nil {
			return err
		}
		if _, err = s.updateActiveInstallation(id, func(item *RuntimeInstallation) {
			item.Status = "starting"
			item.Step = "启动对象存储服务"
			item.Progress = 72
		}); err != nil {
			return err
		}
		if err = s.startRuntimeProcess(*installation, binaryPath, environment); err != nil {
			return err
		}
		if err = waitRuntimeHealth(ctx, *installation, runtimeHealthTimeout); err != nil {
			return err
		}
	}
	if installation.ProviderID != "" {
		if err = ctx.Err(); err != nil {
			return err
		}
		if _, err = s.setProviderEnabled(ctx, installation.ProviderID, true); err != nil {
			return fmt.Errorf("重新启用存储源: %w", err)
		}
		_, err = s.updateActiveInstallation(id, func(item *RuntimeInstallation) {
			item.Status = "running"
			item.Step = "服务运行中"
			item.Progress = 100
			item.ErrorSummary = ""
		})
		return err
	}
	if _, err = s.updateActiveInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "configuring"
		item.Step = "创建 Bucket 并注册存储源"
		item.Progress = 86
	}); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	provider, err := s.saveProvider(ctx, "", ProviderInput{
		Name: installation.Name, Type: "s3", Endpoint: installation.Endpoint,
		Region: installation.Region, Bucket: installation.Bucket, CredentialMode: "static",
		BucketLookup: "path", AccessKeyID: accessKey, SecretAccessKey: secretKey,
		AutoCreateBucket: true, Enabled: true,
	})
	if err != nil {
		return fmt.Errorf("注册存储源: %w", err)
	}
	if _, err = s.updateInstallation(id, func(item *RuntimeInstallation) {
		item.ProviderID = provider.ID
	}); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	_, err = s.updateActiveInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "running"
		item.Step = "安装完成"
		item.Progress = 100
		item.ErrorSummary = ""
	})
	return err
}

func installRuntimeBinary(
	ctx context.Context,
	installation RuntimeInstallation,
	baseDir string,
	reportProgress func(runtimeDownloadProgress) error,
) (string, string, string, error) {
	definition, err := runtimeDefinitionFor(installation.Software)
	if err != nil {
		return "", "", "", err
	}
	downloadURL, checksumURL, err := definition.DownloadURL()
	if err != nil {
		return "", "", "", err
	}
	binaryPath := filepath.Join(baseDir, "bin", installation.Software)
	if info, statErr := os.Stat(binaryPath); statErr == nil && info.Mode().IsRegular() {
		if reportProgress != nil {
			if err = reportProgress(runtimeDownloadProgress{
				Downloaded: info.Size(), Total: info.Size(), Percent: 100,
				Attempt: 1, MaxAttempts: runtimeDownloadMaxAttempts, Step: "安装文件已就绪",
			}); err != nil {
				return "", "", "", err
			}
		}
		checksum, hashErr := fileSHA256(binaryPath)
		if hashErr != nil {
			return "", "", "", hashErr
		}
		return binaryPath, checksum, runtimeBinaryVersion(ctx, binaryPath), nil
	}
	if installation.Software == "minio" {
		temporary := binaryPath + ".download"
		actual, downloadErr := downloadRuntimeFile(ctx, downloadURL, temporary, reportProgress)
		if downloadErr != nil {
			return "", "", "", downloadErr
		}
		expected, checksumErr := fetchRuntimeChecksum(ctx, checksumURL)
		if checksumErr != nil {
			return "", "", "", checksumErr
		}
		if !strings.EqualFold(actual, expected) {
			_ = os.Remove(temporary)
			_ = os.Remove(runtimeDownloadMetadataPath(temporary))
			return "", "", "", errors.New("MinIO 下载文件 SHA256 与官方校验值不一致，已移除损坏文件")
		}
		if err = os.Chmod(temporary, 0o700); err != nil {
			return "", "", "", err
		}
		if err = os.Rename(temporary, binaryPath); err != nil {
			return "", "", "", err
		}
		_ = os.Remove(runtimeDownloadMetadataPath(temporary))
		return binaryPath, actual, runtimeBinaryVersion(ctx, binaryPath), nil
	}
	archivePath := filepath.Join(baseDir, "bin", "rustfs.download.zip")
	actual, err := downloadRuntimeFile(ctx, downloadURL, archivePath, reportProgress)
	if err != nil {
		return "", "", "", err
	}
	if err = extractRuntimeBinary(ctx, archivePath, binaryPath, "rustfs"); err != nil {
		_ = os.Remove(archivePath)
		_ = os.Remove(runtimeDownloadMetadataPath(archivePath))
		return "", "", "", err
	}
	_ = os.Remove(archivePath)
	_ = os.Remove(runtimeDownloadMetadataPath(archivePath))
	return binaryPath, actual, runtimeBinaryVersion(ctx, binaryPath), nil
}

func downloadRuntimeFile(
	ctx context.Context,
	sourceURL string,
	destination string,
	reportProgress func(runtimeDownloadProgress) error,
) (string, error) {
	if err := prepareRuntimeDownloadDestination(sourceURL, destination); err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 1; attempt <= runtimeDownloadMaxAttempts; attempt++ {
		checksum, err := downloadRuntimeFileAttempt(ctx, sourceURL, destination, attempt, reportProgress)
		if err == nil {
			return checksum, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var retryable *retryableRuntimeDownloadError
		if !errors.As(err, &retryable) || attempt == runtimeDownloadMaxAttempts {
			if errors.As(err, &retryable) && attempt == runtimeDownloadMaxAttempts {
				return "", fmt.Errorf("下载运行时连续尝试 %d 次后仍失败: %w", attempt, err)
			}
			return "", err
		}
		lastErr = err
		if reportProgress != nil {
			info, statErr := os.Stat(destination)
			var downloaded int64
			if statErr == nil {
				downloaded = info.Size()
			}
			if reportErr := reportProgress(runtimeDownloadProgress{
				Downloaded:  downloaded,
				Attempt:     attempt + 1,
				MaxAttempts: runtimeDownloadMaxAttempts,
				Resumed:     downloaded > 0,
				Step:        fmt.Sprintf("网络中断，%s后自动续传（第 %d/%d 次）", runtimeDownloadRetryDelays[attempt-1], attempt+1, runtimeDownloadMaxAttempts),
			}); reportErr != nil {
				return "", reportErr
			}
		}
		timer := time.NewTimer(runtimeDownloadRetryDelays[attempt-1])
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", lastErr
}

func runtimeDownloadMetadataPath(destination string) string {
	return destination + ".source"
}

func prepareRuntimeDownloadDestination(sourceURL, destination string) error {
	metadataPath := runtimeDownloadMetadataPath(destination)
	if info, err := os.Stat(destination); err == nil && info.Size() > 0 {
		storedURL, readErr := os.ReadFile(metadataPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			return fmt.Errorf("读取运行时下载来源: %w", readErr)
		}
		// Downloads created by older nav-oss versions have no source sidecar.
		// The destination is scoped to a catalog-controlled software path, so it
		// is safe to adopt that legacy partial file. Once metadata exists, a URL
		// change resets it instead of combining bytes from different releases.
		if readErr == nil && strings.TrimSpace(string(storedURL)) != sourceURL {
			if removeErr := os.Remove(destination); removeErr != nil {
				return fmt.Errorf("重置来源不明的未完成下载: %w", removeErr)
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(metadataPath, []byte(sourceURL+"\n"), 0o600); err != nil {
		return fmt.Errorf("保存运行时下载来源: %w", err)
	}
	return nil
}

func downloadRuntimeFileAttempt(
	ctx context.Context,
	sourceURL string,
	destination string,
	attempt int,
	reportProgress func(runtimeDownloadProgress) error,
) (string, error) {
	existing, err := runtimeDownloadFileSize(destination)
	if err != nil {
		return "", err
	}
	if existing > maxRuntimeDownloadSize {
		return "", errors.New("本地未完成文件超过 512 MiB 限制，请删除实例后重新安装")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "nav-oss-runtime-installer/"+runtime.Version())
	request.Header.Set("Accept-Encoding", "identity")
	if existing > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", existing))
	}
	client := &http.Client{Timeout: runtimeInstallTimeout}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &retryableRuntimeDownloadError{err: fmt.Errorf("下载运行时: %w", err)}
	}
	defer response.Body.Close()
	if runtimeDownloadStatusRetryable(response.StatusCode) {
		return "", &retryableRuntimeDownloadError{err: fmt.Errorf("下载运行时返回 HTTP %d", response.StatusCode)}
	}
	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable && existing > 0 {
		total, ok := parseRuntimeUnsatisfiedRange(response.Header.Get("Content-Range"))
		if ok && total == existing {
			if reportProgress != nil {
				if err = reportProgress(runtimeDownloadProgress{
					Downloaded: existing, Total: total, Percent: 100,
					Attempt: attempt, MaxAttempts: runtimeDownloadMaxAttempts,
					Resumed: true, Step: "下载完成，正在校验文件",
				}); err != nil {
					return "", err
				}
			}
			return fileSHA256(destination)
		}
		return "", errors.New("服务器返回的断点位置与本地文件不一致，请删除实例后重新安装")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return "", fmt.Errorf("下载运行时返回 HTTP %d", response.StatusCode)
	}

	appendFile := false
	total := response.ContentLength
	if response.StatusCode == http.StatusPartialContent {
		start, contentTotal, ok := parseRuntimeContentRange(response.Header.Get("Content-Range"))
		if !ok || start != existing {
			return "", errors.New("服务器返回的断点续传范围无效，请删除实例后重新安装")
		}
		appendFile = existing > 0
		total = contentTotal
	} else if existing > 0 {
		// Some mirrors ignore Range and return the complete object. Truncate the
		// partial file so the response is never appended twice.
		existing = 0
	}
	if total > maxRuntimeDownloadSize || (total <= 0 && response.ContentLength > maxRuntimeDownloadSize) {
		return "", errors.New("运行时下载文件超过 512 MiB 限制")
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendFile {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(destination, flags, 0o600)
	if err != nil {
		return "", err
	}
	lastReportAt := time.Now()
	lastReportedBytes := existing
	downloaded := existing
	report := func(step string, force bool) error {
		if reportProgress == nil {
			return nil
		}
		now := time.Now()
		if !force && now.Sub(lastReportAt) < runtimeDownloadProgressInterval {
			return nil
		}
		elapsed := now.Sub(lastReportAt)
		var speed int64
		if elapsed > 0 {
			speed = int64(float64(downloaded-lastReportedBytes) / elapsed.Seconds())
		}
		if err := reportProgress(runtimeDownloadProgress{
			Downloaded: downloaded, Total: total, Speed: speed,
			Percent: runtimeDownloadPercent(downloaded, total),
			Attempt: attempt, MaxAttempts: runtimeDownloadMaxAttempts,
			Resumed: appendFile, Step: step,
		}); err != nil {
			return err
		}
		lastReportAt = now
		lastReportedBytes = downloaded
		return nil
	}
	step := "正在下载安装文件"
	if appendFile {
		step = "正在从上次进度继续下载"
	}
	if err = report(step, true); err != nil {
		_ = file.Close()
		return "", err
	}
	buffer := make([]byte, 64*1024)
	var copyErr error
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			copyErr = ctxErr
			break
		}
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			if downloaded+int64(read) > maxRuntimeDownloadSize {
				copyErr = errors.New("运行时下载文件超过 512 MiB 限制")
				break
			}
			written, writeErr := file.Write(buffer[:read])
			downloaded += int64(written)
			if writeErr != nil {
				copyErr = writeErr
				break
			}
			if written != read {
				copyErr = io.ErrShortWrite
				break
			}
			if reportErr := report(step, false); reportErr != nil {
				copyErr = reportErr
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				copyErr = readErr
			}
			break
		}
	}
	closeErr := file.Close()
	if copyErr != nil {
		if errors.Is(copyErr, context.Canceled) || (errors.Is(copyErr, context.DeadlineExceeded) && ctx.Err() != nil) {
			return "", copyErr
		}
		if errors.Is(copyErr, io.ErrShortWrite) || !isRuntimeDownloadNetworkError(copyErr) {
			return "", fmt.Errorf("保存运行时: %w", copyErr)
		}
		return "", &retryableRuntimeDownloadError{err: fmt.Errorf("保存运行时: %w", copyErr)}
	}
	if closeErr != nil {
		return "", closeErr
	}
	if total > 0 && downloaded != total {
		return "", &retryableRuntimeDownloadError{err: fmt.Errorf("下载提前结束：已接收 %d 字节，应为 %d 字节", downloaded, total)}
	}
	if total <= 0 {
		total = downloaded
	}
	if err = report("下载完成，正在校验文件", true); err != nil {
		return "", err
	}
	return fileSHA256(destination)
}

func runtimeDownloadFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("运行时临时下载路径不是普通文件")
	}
	return info.Size(), nil
}

func runtimeDownloadPercent(downloaded, total int64) int {
	if total <= 0 || downloaded <= 0 {
		return 0
	}
	percent := int(downloaded * 100 / total)
	if percent > 100 {
		return 100
	}
	return percent
}

func parseRuntimeContentRange(value string) (int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, false
	}
	rangePart, totalPart, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !ok || totalPart == "*" {
		return 0, 0, false
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, false
	}
	start, startErr := strconv.ParseInt(startPart, 10, 64)
	end, endErr := strconv.ParseInt(endPart, 10, 64)
	total, totalErr := strconv.ParseInt(totalPart, 10, 64)
	if startErr != nil || endErr != nil || totalErr != nil || start < 0 || end < start || total <= end {
		return 0, 0, false
	}
	return start, total, true
}

func parseRuntimeUnsatisfiedRange(value string) (int64, bool) {
	if !strings.HasPrefix(value, "bytes */") {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimPrefix(value, "bytes */"), 10, 64)
	return total, err == nil && total >= 0
}

func runtimeDownloadStatusRetryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRuntimeDownloadNetworkError(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"connection reset", "broken pipe", "server closed", "stream error", "timeout", "unexpected eof"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func fetchRuntimeChecksum(ctx context.Context, sourceURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return "", fmt.Errorf("读取官方校验值: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("官方校验值返回 HTTP %d", response.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(content))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", errors.New("官方 SHA256 校验文件格式无效")
	}
	if _, err = hex.DecodeString(fields[0]); err != nil {
		return "", errors.New("官方 SHA256 校验值无效")
	}
	return strings.ToLower(fields[0]), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func extractRuntimeBinary(ctx context.Context, archivePath, destination, executableName string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开运行时压缩包: %w", err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if filepath.Base(entry.Name) != executableName || entry.FileInfo().IsDir() {
			continue
		}
		if int64(entry.UncompressedSize64) > maxRuntimeDownloadSize {
			return errors.New("压缩包中的运行时文件过大")
		}
		source, openErr := entry.Open()
		if openErr != nil {
			return openErr
		}
		temporary := destination + ".extract"
		target, createErr := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
		if createErr != nil {
			_ = source.Close()
			return createErr
		}
		_, copyErr := io.Copy(target, io.LimitReader(&contextReader{ctx: ctx, reader: source}, maxRuntimeDownloadSize+1))
		closeTargetErr := target.Close()
		closeSourceErr := source.Close()
		if copyErr != nil || closeTargetErr != nil || closeSourceErr != nil {
			_ = os.Remove(temporary)
			return errors.Join(copyErr, closeTargetErr, closeSourceErr)
		}
		if err = os.Rename(temporary, destination); err != nil {
			_ = os.Remove(temporary)
			return err
		}
		return nil
	}
	return fmt.Errorf("压缩包中未找到 %s 可执行程序", executableName)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func runtimeBinaryVersion(ctx context.Context, binaryPath string) string {
	versionContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionContext, binaryPath, "--version").CombinedOutput()
	if err != nil {
		return "latest"
	}
	line := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	if len(line) > 120 {
		line = line[:120]
	}
	return line
}

func prepareRuntimeEnvironment(installation RuntimeInstallation, baseDir string) ([]string, string, string, error) {
	environmentPath := filepath.Join(baseDir, "runtime.env")
	accessKey := ""
	secretKey := ""
	if content, err := os.ReadFile(environmentPath); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if key == "NAV_OSS_RUNTIME_ACCESS_KEY" {
				accessKey = value
			}
			if key == "NAV_OSS_RUNTIME_SECRET_KEY" {
				secretKey = value
			}
		}
	}
	if accessKey == "" || secretKey == "" {
		accessSuffix, err := secureToken(8)
		if err != nil {
			return nil, "", "", err
		}
		secretKey, err = secureToken(32)
		if err != nil {
			return nil, "", "", err
		}
		accessKey = "navoss-" + strings.ToLower(accessSuffix)
		content := fmt.Sprintf("NAV_OSS_RUNTIME_ACCESS_KEY=%s\nNAV_OSS_RUNTIME_SECRET_KEY=%s\n", accessKey, secretKey)
		if err = os.WriteFile(environmentPath, []byte(content), 0o600); err != nil {
			return nil, "", "", fmt.Errorf("保存运行时凭证: %w", err)
		}
	}
	logDir := filepath.Join(baseDir, "logs")
	if installation.Software == "minio" {
		return []string{"MINIO_ROOT_USER=" + accessKey, "MINIO_ROOT_PASSWORD=" + secretKey}, accessKey, secretKey, nil
	}
	return []string{
		"RUSTFS_ACCESS_KEY=" + accessKey,
		"RUSTFS_SECRET_KEY=" + secretKey,
		"RUSTFS_VOLUMES=" + installation.DataDir,
		fmt.Sprintf("RUSTFS_ADDRESS=127.0.0.1:%d", installation.APIPort),
		fmt.Sprintf("RUSTFS_CONSOLE_ADDRESS=127.0.0.1:%d", installation.ConsolePort),
		"RUSTFS_CONSOLE_ENABLE=true",
		"RUSTFS_OBS_LOGGER_LEVEL=error",
		"RUSTFS_OBS_LOG_DIRECTORY=" + logDir,
	}, accessKey, secretKey, nil
}

func (s *Service) startRuntimeProcess(installation RuntimeInstallation, binaryPath string, environment []string) error {
	s.runtimeMu.Lock()
	if existing := s.runtimeProcesses[installation.ID]; existing != nil {
		s.runtimeMu.Unlock()
		return nil
	}
	baseDir := filepath.Dir(installation.DataDir)
	logFile, err := os.OpenFile(filepath.Join(baseDir, "logs", installation.Software+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.runtimeMu.Unlock()
		return fmt.Errorf("打开运行时日志: %w", err)
	}
	arguments := []string{}
	if installation.Software == "minio" {
		arguments = []string{"server", installation.DataDir, "--address", fmt.Sprintf("127.0.0.1:%d", installation.APIPort), "--console-address", fmt.Sprintf("127.0.0.1:%d", installation.ConsolePort)}
	}
	command := exec.Command(binaryPath, arguments...)
	command.Dir = baseDir
	command.Env = append(os.Environ(), environment...)
	command.Stdout = logFile
	command.Stderr = logFile
	if err = command.Start(); err != nil {
		_ = logFile.Close()
		s.runtimeMu.Unlock()
		return fmt.Errorf("启动 %s: %w", installation.Software, err)
	}
	process := &runtimeProcess{cmd: command, process: command.Process, log: logFile, done: make(chan struct{})}
	s.runtimeProcesses[installation.ID] = process
	s.runtimeMu.Unlock()
	_, _ = s.updateInstallation(installation.ID, func(item *RuntimeInstallation) { item.PID = command.Process.Pid })
	go s.waitRuntimeProcess(installation.ID, process)
	return nil
}

func (s *Service) waitRuntimeProcess(id string, process *runtimeProcess) {
	err := process.cmd.Wait()
	_ = process.log.Close()
	close(process.done)
	s.runtimeMu.Lock()
	if s.runtimeProcesses[id] == process {
		delete(s.runtimeProcesses, id)
	}
	s.runtimeMu.Unlock()
	_, _ = s.updateInstallation(id, func(item *RuntimeInstallation) {
		if item.PID != process.process.Pid {
			return
		}
		item.PID = 0
		if item.Status == "stopping" {
			item.Status = "stopped"
			item.Step = "服务已停止"
			return
		}
		if item.Status != "failed" {
			item.Status = "failed"
			item.Step = "进程异常退出"
			if err != nil {
				item.ErrorSummary = err.Error()
			} else {
				item.ErrorSummary = "对象存储进程已退出"
			}
		}
	})
}

func (s *Service) adoptRuntimeProcess(installation RuntimeInstallation) bool {
	if installation.PID <= 0 || installation.BinaryPath == "" {
		return false
	}
	process, err := os.FindProcess(installation.PID)
	if err != nil || !runtimeProcessMatches(installation, process) {
		return false
	}
	managed := &runtimeProcess{process: process, done: make(chan struct{})}
	s.runtimeMu.Lock()
	if s.runtimeProcesses[installation.ID] != nil {
		s.runtimeMu.Unlock()
		return true
	}
	s.runtimeProcesses[installation.ID] = managed
	s.runtimeMu.Unlock()
	go s.monitorAdoptedRuntimeProcess(installation, managed)
	return true
}

func (s *Service) monitorAdoptedRuntimeProcess(installation RuntimeInstallation, managed *runtimeProcess) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if runtimeProcessMatches(installation, managed.process) {
			continue
		}
		close(managed.done)
		s.runtimeMu.Lock()
		if s.runtimeProcesses[installation.ID] == managed {
			delete(s.runtimeProcesses, installation.ID)
		}
		s.runtimeMu.Unlock()
		_, _ = s.updateInstallation(installation.ID, func(item *RuntimeInstallation) {
			if item.PID != managed.process.Pid {
				return
			}
			item.PID = 0
			if item.Status == "stopping" {
				item.Status = "stopped"
				item.Step = "服务已停止"
				return
			}
			if item.Status != "stopped" {
				item.Status = "failed"
				item.Step = "进程异常退出"
				item.ErrorSummary = "对象存储进程已退出"
			}
		})
		return
	}
}

func runtimeProcessMatches(installation RuntimeInstallation, process *os.Process) bool {
	if process == nil || process.Pid != installation.PID {
		return false
	}
	var executablePath string
	switch runtime.GOOS {
	case "linux":
		path, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(process.Pid), "exe"))
		if err != nil {
			return false
		}
		executablePath = strings.TrimSuffix(path, " (deleted)")
	case "darwin":
		output, err := exec.Command("ps", "-p", strconv.Itoa(process.Pid), "-o", "comm=").Output()
		if err != nil {
			return false
		}
		executablePath = strings.TrimSpace(string(output))
	default:
		return false
	}
	return sameExecutablePath(executablePath, installation.BinaryPath)
}

func sameExecutablePath(left, right string) bool {
	leftPath, leftErr := filepath.Abs(left)
	rightPath, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if evaluated, err := filepath.EvalSymlinks(leftPath); err == nil {
		leftPath = evaluated
	}
	if evaluated, err := filepath.EvalSymlinks(rightPath); err == nil {
		rightPath = evaluated
	}
	return filepath.Clean(leftPath) == filepath.Clean(rightPath)
}

func runtimeHealthy(installation RuntimeInstallation) bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	paths := []string{"/minio/health/live"}
	if installation.Software == "rustfs" {
		paths = append(paths, "/health")
	}
	for _, path := range paths {
		response, err := client.Get(installation.Endpoint + path)
		if err != nil {
			continue
		}
		_ = response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 400 {
			return true
		}
	}
	return false
}

func waitRuntimeHealth(ctx context.Context, installation RuntimeInstallation, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if runtimeHealthy(installation) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(750 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s 健康检查在 %s 内未通过，请查看运行日志", installation.Software, timeout)
}

func (s *Service) retryInstallation(id string) (RuntimeInstallation, error) {
	current := s.installation(id)
	if current == nil {
		return RuntimeInstallation{}, errors.New("安装实例不存在")
	}
	if current.Status != "failed" && !(current.Status == "stopped" && current.ProviderID == "") {
		return RuntimeInstallation{}, errors.New("只有失败或未完成的安装任务可以重试")
	}
	installation, err := s.updateInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "queued"
		item.Step = "等待重试"
		item.Progress = 0
		item.DownloadSpeedBytes = 0
		item.DownloadAttempt = 0
		item.ErrorSummary = ""
	})
	if err != nil {
		return RuntimeInstallation{}, err
	}
	s.launchInstallation(id)
	return *installation, nil
}

func (s *Service) startInstallation(id string) (RuntimeInstallation, error) {
	installation := s.installation(id)
	if installation == nil {
		return RuntimeInstallation{}, errors.New("安装实例不存在")
	}
	if installation.Status != "stopped" {
		return RuntimeInstallation{}, errors.New("只有已停止的实例可以启动")
	}
	if installation.BinaryPath == "" {
		return RuntimeInstallation{}, errors.New("可执行程序尚未安装，请先重试安装任务")
	}
	if runtimeHealthy(*installation) {
		if installation.ProviderID != "" {
			if _, err := s.setProviderEnabled(context.Background(), installation.ProviderID, true); err != nil {
				return RuntimeInstallation{}, fmt.Errorf("重新启用关联存储源: %w", err)
			}
		}
		updated, err := s.updateInstallation(id, func(item *RuntimeInstallation) {
			item.Status = "running"
			item.Step = "服务运行中"
			item.Progress = 100
			item.ErrorSummary = ""
		})
		if err != nil {
			return RuntimeInstallation{}, err
		}
		return *updated, err
	}
	baseDir := filepath.Dir(installation.DataDir)
	environment, _, _, err := prepareRuntimeEnvironment(*installation, baseDir)
	if err != nil {
		return RuntimeInstallation{}, err
	}
	updated, err := s.updateInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "starting"
		item.Step = "正在启动"
		item.ErrorSummary = ""
	})
	if err != nil {
		return RuntimeInstallation{}, err
	}
	s.launchResumeInstallation(id, installation.BinaryPath, environment)
	return *updated, nil
}

func (s *Service) launchResumeInstallation(id, binaryPath string, environment []string) {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeHealthTimeout)
	task := &runtimeTask{cancel: cancel, done: make(chan struct{})}
	s.runtimeMu.Lock()
	if existing := s.runtimeTasks[id]; existing != nil {
		s.runtimeMu.Unlock()
		cancel()
		return
	}
	s.runtimeTasks[id] = task
	s.runtimeMu.Unlock()
	installation := s.installation(id)
	if installation == nil || installation.Status != "starting" {
		task.cancel()
		close(task.done)
		s.runtimeMu.Lock()
		if s.runtimeTasks[id] == task {
			delete(s.runtimeTasks, id)
		}
		s.runtimeMu.Unlock()
		return
	}
	go s.resumeInstallation(ctx, id, binaryPath, environment, task)
}

func (s *Service) resumeInstallation(ctx context.Context, id, binaryPath string, environment []string, task *runtimeTask) {
	defer func() {
		task.cancel()
		close(task.done)
		s.runtimeMu.Lock()
		if s.runtimeTasks[id] == task {
			delete(s.runtimeTasks, id)
		}
		s.runtimeMu.Unlock()
	}()
	installation := s.installation(id)
	if installation == nil {
		return
	}
	err := s.startRuntimeProcess(*installation, binaryPath, environment)
	if err == nil {
		err = waitRuntimeHealth(ctx, *installation, runtimeHealthTimeout)
		if err == nil && installation.ProviderID != "" {
			_, err = s.setProviderEnabled(ctx, installation.ProviderID, true)
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		_, _ = s.stopRuntimeProcess(id, 8*time.Second)
	}
	_, _ = s.updateInstallation(id, func(item *RuntimeInstallation) {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			item.Status = "stopped"
			item.Step = "服务已停止"
			item.PID = 0
			item.ErrorSummary = ""
			return
		}
		if err != nil {
			item.Status = "failed"
			item.Step = "启动失败"
			item.ErrorSummary = err.Error()
			return
		}
		item.Status = "running"
		item.Step = "服务运行中"
		item.Progress = 100
		item.ErrorSummary = ""
	})
}

func (s *Service) stopInstallation(id string) (RuntimeInstallation, error) {
	installation := s.installation(id)
	if installation == nil {
		return RuntimeInstallation{}, errors.New("安装实例不存在")
	}
	if isRuntimeInstallStatus(installation.Status) {
		return s.stopInstallationTask(*installation)
	}
	if installation.Status == "stopping" {
		return RuntimeInstallation{}, errors.New("实例正在停止，请稍后再试")
	}
	if installation.Status != "running" {
		return RuntimeInstallation{}, errors.New("只有安装中或运行中的实例可以停止")
	}
	if _, err := s.updateInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "stopping"
		item.Step = "正在停止"
	}); err != nil {
		return RuntimeInstallation{}, err
	}
	managed, stopErr := s.stopRuntimeProcess(id, 8*time.Second)
	if !managed {
		_, _ = s.updateInstallation(id, func(item *RuntimeInstallation) {
			item.Status = "running"
			item.Step = "服务运行中"
		})
		return RuntimeInstallation{}, errors.New("当前进程不是由本次 nav-oss 服务会话启动，为避免误杀进程已拒绝停止")
	}
	if stopErr != nil {
		_, _ = s.updateInstallation(id, func(item *RuntimeInstallation) {
			item.Status = "failed"
			item.Step = "停止失败"
			item.ErrorSummary = stopErr.Error()
		})
		return RuntimeInstallation{}, stopErr
	}
	updated, err := s.updateInstallation(id, func(item *RuntimeInstallation) {
		item.Status = "stopped"
		item.Step = "服务已停止"
		item.PID = 0
	})
	if err != nil {
		return RuntimeInstallation{}, err
	}
	if installation.ProviderID != "" {
		if _, err = s.setProviderEnabled(context.Background(), installation.ProviderID, false); err != nil {
			return RuntimeInstallation{}, fmt.Errorf("停用关联存储源: %w", err)
		}
	}
	return *updated, err
}

func isRuntimeInstallStatus(status string) bool {
	switch status {
	case "queued", "downloading", "installing", "starting", "configuring":
		return true
	default:
		return false
	}
}

func (s *Service) stopInstallationTask(installation RuntimeInstallation) (RuntimeInstallation, error) {
	if _, err := s.updateInstallation(installation.ID, func(item *RuntimeInstallation) {
		item.Status = "stopping"
		item.Step = "正在停止安装"
		item.ErrorSummary = ""
	}); err != nil {
		return RuntimeInstallation{}, err
	}

	s.runtimeMu.Lock()
	task := s.runtimeTasks[installation.ID]
	if task != nil {
		task.cancel()
	}
	s.runtimeMu.Unlock()
	if task != nil {
		select {
		case <-task.done:
		case <-time.After(15 * time.Second):
			return RuntimeInstallation{}, errors.New("停止安装任务超时，请稍后重试")
		}
	} else {
		if _, err := s.updateInstallation(installation.ID, func(item *RuntimeInstallation) {
			item.Status = "stopped"
			item.Step = "安装已停止"
			item.PID = 0
		}); err != nil {
			return RuntimeInstallation{}, err
		}
	}
	managed, stopErr := s.stopRuntimeProcess(installation.ID, 8*time.Second)
	if stopErr != nil {
		return RuntimeInstallation{}, stopErr
	}
	if !managed {
		current := s.installation(installation.ID)
		if current != nil && runtimeHealthy(*current) {
			return RuntimeInstallation{}, errors.New("检测到实例进程仍在运行，请稍后重试停止操作")
		}
	}

	updated := s.installation(installation.ID)
	if updated == nil {
		return RuntimeInstallation{}, errors.New("安装实例不存在")
	}
	if updated.ProviderID != "" {
		if _, err := s.setProviderEnabled(context.Background(), updated.ProviderID, false); err != nil {
			return RuntimeInstallation{}, fmt.Errorf("停用关联存储源: %w", err)
		}
	}
	return *updated, nil
}

func (s *Service) deleteInstallation(id string) error {
	installation := s.installation(id)
	if installation == nil {
		return errors.New("安装实例不存在")
	}
	if isRuntimeInstallStatus(installation.Status) || installation.Status == "running" {
		if _, err := s.stopInstallation(id); err != nil {
			return err
		}
	} else if installation.Status == "stopping" {
		s.runtimeMu.Lock()
		task := s.runtimeTasks[id]
		s.runtimeMu.Unlock()
		if task != nil {
			select {
			case <-task.done:
			case <-time.After(15 * time.Second):
				return errors.New("等待实例停止超时，请稍后重试")
			}
		}
	}

	installation = s.installation(id)
	if installation == nil {
		return nil
	}
	if installation.ProviderID != "" && s.provider(installation.ProviderID) == nil {
		return errors.New("关联存储源不存在，为避免误删数据，请先恢复或解除实例关联")
	}
	if installation.Status != "stopped" && installation.Status != "failed" {
		return errors.New("实例尚未停止，暂时不能删除")
	}
	if managed, err := s.stopRuntimeProcess(id, 8*time.Second); err != nil {
		return err
	} else if !managed && runtimeHealthy(*installation) {
		return errors.New("检测到实例端口仍在运行，为避免误删数据已拒绝删除")
	}
	if installation.ProviderID != "" {
		if provider := s.provider(installation.ProviderID); provider != nil && provider.Enabled {
			if _, err := s.setProviderEnabled(context.Background(), installation.ProviderID, false); err != nil {
				return fmt.Errorf("停用关联存储源: %w", err)
			}
		}
	}

	baseDir, err := s.runtimeInstallationDir(id)
	if err != nil {
		return err
	}
	stagedDir := baseDir + ".deleting"
	directoryStaged := false
	if _, statErr := os.Stat(baseDir); statErr == nil {
		if err = os.Rename(baseDir, stagedDir); err != nil {
			return fmt.Errorf("隔离实例目录: %w", err)
		}
		directoryStaged = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("检查实例目录: %w", statErr)
	}
	if err = s.removeInstallationRecord(id); err != nil {
		if directoryStaged {
			_ = os.Rename(stagedDir, baseDir)
		}
		return err
	}
	if directoryStaged {
		if err = os.RemoveAll(stagedDir); err != nil {
			return fmt.Errorf("删除实例目录: %w", err)
		}
	}
	return nil
}

func (s *Service) runtimeInstallationDir(id string) (string, error) {
	if matched, _ := regexp.MatchString(`^[a-f0-9]{32}$`, id); !matched {
		return "", errors.New("实例编号格式无效")
	}
	root, err := filepath.Abs(s.config.RuntimeDir)
	if err != nil {
		return "", fmt.Errorf("解析运行时根目录: %w", err)
	}
	target, err := filepath.Abs(filepath.Join(root, id))
	if err != nil {
		return "", fmt.Errorf("解析实例目录: %w", err)
	}
	if filepath.Dir(target) != filepath.Clean(root) {
		return "", errors.New("实例目录超出运行时根目录")
	}
	return target, nil
}

func (s *Service) removeInstallationRecord(id string) error {
	s.mu.Lock()
	index := s.installationIndexLocked(id)
	if index < 0 {
		s.mu.Unlock()
		return nil
	}
	installation := s.state.Installations[index]
	previousInstallations := append([]RuntimeInstallation(nil), s.state.Installations...)
	previousProviders := append([]providerState(nil), s.state.Providers...)
	previousSettings := s.state.Settings
	previousOperations := append([]Operation(nil), s.state.Operations...)
	providerIndex := s.providerIndexLocked(installation.ProviderID)
	var providerStore io.Closer
	providerID := ""
	if providerIndex >= 0 {
		providerID = s.state.Providers[providerIndex].ID
		providerStore = s.stores[providerID]
		s.state.Providers = append(s.state.Providers[:providerIndex], s.state.Providers[providerIndex+1:]...)
		if s.state.Settings.DefaultProviderID == providerID {
			s.state.Settings.DefaultProviderID = ""
			for itemIndex := range s.state.Providers {
				s.state.Providers[itemIndex].IsDefault = false
			}
			if len(s.state.Providers) > 0 {
				defaultIndex := 0
				for itemIndex := range s.state.Providers {
					if s.state.Providers[itemIndex].Enabled {
						defaultIndex = itemIndex
						break
					}
				}
				s.state.Providers[defaultIndex].IsDefault = true
				s.state.Settings.DefaultProviderID = s.state.Providers[defaultIndex].ID
			}
		}
	}
	s.state.Installations = append(s.state.Installations[:index], s.state.Installations[index+1:]...)
	s.appendOperationLocked("DELETE_RUNTIME", installation.Name, installation.Software, nil, time.Now())
	err := s.persistLocked()
	if err != nil {
		s.state.Installations = previousInstallations
		s.state.Providers = previousProviders
		s.state.Settings = previousSettings
		s.state.Operations = previousOperations
		s.mu.Unlock()
		return err
	}
	if providerID != "" {
		delete(s.stores, providerID)
	}
	s.mu.Unlock()
	if providerStore != nil {
		_ = providerStore.Close()
	}
	return err
}

func (s *Service) restartInstallation(id string) (RuntimeInstallation, error) {
	installation := s.installation(id)
	if installation == nil {
		return RuntimeInstallation{}, errors.New("安装实例不存在")
	}
	if installation.Status != "running" {
		return RuntimeInstallation{}, errors.New("只有运行中的实例可以重启")
	}
	if _, err := s.stopInstallation(id); err != nil {
		return RuntimeInstallation{}, err
	}
	return s.startInstallation(id)
}

func (s *Service) stopRuntimeProcess(id string, gracePeriod time.Duration) (bool, error) {
	s.runtimeMu.Lock()
	process := s.runtimeProcesses[id]
	s.runtimeMu.Unlock()
	if process == nil || process.process == nil {
		return false, nil
	}
	installation := s.installation(id)
	if installation == nil || !runtimeProcessMatches(*installation, process.process) {
		return false, nil
	}
	if err := process.process.Signal(os.Interrupt); err != nil {
		select {
		case <-process.done:
			return true, nil
		default:
			return true, fmt.Errorf("停止对象存储进程: %w", err)
		}
	}
	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-process.done:
		return true, nil
	case <-timer.C:
	}
	if err := process.process.Signal(syscall.SIGKILL); err != nil {
		select {
		case <-process.done:
			return true, nil
		default:
			return true, fmt.Errorf("强制停止对象存储进程: %w", err)
		}
	}
	select {
	case <-process.done:
		return true, nil
	case <-time.After(3 * time.Second):
		return true, errors.New("对象存储进程在强制停止后仍未退出")
	}
}

func (s *Service) updateInstallation(id string, update func(*RuntimeInstallation)) (*RuntimeInstallation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.installationIndexLocked(id)
	if index < 0 {
		return nil, errors.New("安装实例不存在")
	}
	update(&s.state.Installations[index])
	s.state.Installations[index].UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	item := s.state.Installations[index]
	return &item, nil
}

func (s *Service) updateActiveInstallation(id string, update func(*RuntimeInstallation)) (*RuntimeInstallation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.installationIndexLocked(id)
	if index < 0 {
		return nil, errors.New("安装实例不存在")
	}
	if !isRuntimeInstallStatus(s.state.Installations[index].Status) {
		return nil, context.Canceled
	}
	update(&s.state.Installations[index])
	s.state.Installations[index].UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	item := s.state.Installations[index]
	return &item, nil
}

func (s *Service) installationIndexLocked(id string) int {
	for index := range s.state.Installations {
		if s.state.Installations[index].ID == id {
			return index
		}
	}
	return -1
}
