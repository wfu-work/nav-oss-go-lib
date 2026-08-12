package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopInstallationCancelsActiveTask(t *testing.T) {
	service := newRuntimeTestService(t)
	id := "0123456789abcdef0123456789abcdef"
	now := time.Now().UTC()
	service.state.Installations = []RuntimeInstallation{{
		ID:          id,
		Software:    "minio",
		Name:        "测试安装任务",
		Status:      "queued",
		Step:        "等待安装",
		APIPort:     19100,
		ConsolePort: 19101,
		Endpoint:    "http://127.0.0.1:19100",
		ConsoleURL:  "http://127.0.0.1:19101",
		Bucket:      "runtime-test",
		Region:      "us-east-1",
		DataDir:     filepath.Join(service.config.RuntimeDir, id, "data"),
		CreatedAt:   now,
		UpdatedAt:   now,
	}}

	ctx, cancel := context.WithCancel(context.Background())
	task := &runtimeTask{cancel: cancel, done: make(chan struct{})}
	service.runtimeTasks[id] = task
	go func() {
		<-ctx.Done()
		_, _ = service.updateInstallation(id, func(item *RuntimeInstallation) {
			item.Status = "stopped"
			item.Step = "安装已停止"
		})
		close(task.done)
		service.runtimeMu.Lock()
		delete(service.runtimeTasks, id)
		service.runtimeMu.Unlock()
	}()
	updated, err := service.stopInstallation(id)
	if err != nil {
		t.Fatalf("stop active installation: %v", err)
	}
	if updated.Status != "stopped" {
		t.Fatalf("expected stopped status, got %q", updated.Status)
	}
	if updated.Step != "安装已停止" {
		t.Fatalf("expected stopped step, got %q", updated.Step)
	}
	service.runtimeMu.Lock()
	_, active := service.runtimeTasks[id]
	service.runtimeMu.Unlock()
	if active {
		t.Fatal("installation task remained registered after stop")
	}
}

func TestDeleteInstallationRemovesFilesProviderAndRecord(t *testing.T) {
	service := newRuntimeTestService(t)
	id := "fedcba9876543210fedcba9876543210"
	providerID := "provider-runtime-test"
	instanceDir := filepath.Join(service.config.RuntimeDir, id)
	if err := os.MkdirAll(filepath.Join(instanceDir, "data"), 0o700); err != nil {
		t.Fatalf("create instance directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "data", "object.bin"), []byte("test"), 0o600); err != nil {
		t.Fatalf("create instance data: %v", err)
	}
	now := time.Now().UTC()
	service.state.Providers = []providerState{{
		ID: providerID,
		ProviderInput: ProviderInput{
			Name:      "测试存储源",
			Type:      "s3",
			Enabled:   false,
			IsDefault: true,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}}
	service.state.Settings.DefaultProviderID = providerID
	service.state.Installations = []RuntimeInstallation{{
		ID:          id,
		Software:    "minio",
		Name:        "测试实例",
		Status:      "stopped",
		Step:        "服务已停止",
		APIPort:     19200,
		ConsolePort: 19201,
		Endpoint:    "http://127.0.0.1:19200",
		ConsoleURL:  "http://127.0.0.1:19201",
		Bucket:      "runtime-test",
		Region:      "us-east-1",
		DataDir:     filepath.Join(instanceDir, "data"),
		ProviderID:  providerID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}}

	if err := service.deleteInstallation(id); err != nil {
		t.Fatalf("delete installation: %v", err)
	}
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Fatalf("expected instance directory removed, stat error: %v", err)
	}
	if service.installation(id) != nil {
		t.Fatal("installation record still exists after delete")
	}
	if service.provider(providerID) != nil {
		t.Fatal("linked provider still exists after delete")
	}
	if service.state.Settings.DefaultProviderID != "" {
		t.Fatalf("expected default provider cleared, got %q", service.state.Settings.DefaultProviderID)
	}
}

func TestReconcileInstallationsRestoresDownloadURL(t *testing.T) {
	service := newRuntimeTestService(t)
	service.state.Installations = []RuntimeInstallation{{
		ID:       "00112233445566778899aabbccddeeff",
		Software: "minio",
		Status:   "stopped",
	}}

	service.reconcileInstallations()
	if service.state.Installations[0].DownloadURL == "" {
		t.Fatal("expected download URL to be restored for an existing installation")
	}
}

func TestDownloadRuntimeFileReportsProgress(t *testing.T) {
	payload := bytes.Repeat([]byte("nav-oss-download-progress"), 8192)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "runtime.download")
	var reports []runtimeDownloadProgress
	checksum, err := downloadRuntimeFile(
		context.Background(),
		server.URL,
		destination,
		func(progress runtimeDownloadProgress) error {
			reports = append(reports, progress)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("download runtime: %v", err)
	}
	assertRuntimeDownloadContent(t, destination, payload)
	expectedChecksum, err := fileSHA256(destination)
	if err != nil {
		t.Fatalf("checksum downloaded runtime: %v", err)
	}
	if checksum != expectedChecksum {
		t.Fatalf("expected checksum %q, got %q", expectedChecksum, checksum)
	}
	if len(reports) < 2 {
		t.Fatalf("expected at least initial and final progress reports, got %d", len(reports))
	}
	final := reports[len(reports)-1]
	if final.Downloaded != int64(len(payload)) || final.Total != int64(len(payload)) || final.Percent != 100 {
		t.Fatalf("unexpected final progress: %+v", final)
	}
	if final.Speed <= 0 {
		t.Fatalf("expected a measured download speed, got %+v", final)
	}
}

func TestDownloadRuntimeFileResumesPartialFile(t *testing.T) {
	payload := bytes.Repeat([]byte("resume-download"), 16384)
	partialSize := len(payload) / 3
	var requestedRange string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedRange = request.Header.Get("Range")
		writer.Header().Set(
			"Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", partialSize, len(payload)-1, len(payload)),
		)
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)-partialSize))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(payload[partialSize:])
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "runtime.download")
	if err := os.WriteFile(destination, payload[:partialSize], 0o600); err != nil {
		t.Fatalf("write partial runtime: %v", err)
	}
	var resumed bool
	_, err := downloadRuntimeFile(
		context.Background(),
		server.URL,
		destination,
		func(progress runtimeDownloadProgress) error {
			resumed = resumed || progress.Resumed
			return nil
		},
	)
	if err != nil {
		t.Fatalf("resume runtime download: %v", err)
	}
	if requestedRange != fmt.Sprintf("bytes=%d-", partialSize) {
		t.Fatalf("expected resume range for %d bytes, got %q", partialSize, requestedRange)
	}
	if !resumed {
		t.Fatal("expected resumed progress to be reported")
	}
	assertRuntimeDownloadContent(t, destination, payload)
}

func TestDownloadRuntimeFileRestartsWhenServerIgnoresRange(t *testing.T) {
	payload := bytes.Repeat([]byte("complete-download"), 8192)
	partial := payload[:len(payload)/4]
	var requestedRange string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedRange = request.Header.Get("Range")
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "runtime.download")
	if err := os.WriteFile(destination, partial, 0o600); err != nil {
		t.Fatalf("write partial runtime: %v", err)
	}
	if _, err := downloadRuntimeFile(context.Background(), server.URL, destination, nil); err != nil {
		t.Fatalf("restart runtime download: %v", err)
	}
	if requestedRange != fmt.Sprintf("bytes=%d-", len(partial)) {
		t.Fatalf("expected initial range request, got %q", requestedRange)
	}
	assertRuntimeDownloadContent(t, destination, payload)
}

func TestDownloadRuntimeFileRetriesTemporaryHTTPFailure(t *testing.T) {
	originalDelays := runtimeDownloadRetryDelays
	runtimeDownloadRetryDelays = [...]time.Duration{0, 0}
	t.Cleanup(func() { runtimeDownloadRetryDelays = originalDelays })

	payload := []byte("download succeeded after retry")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		if attempt < 3 {
			http.Error(writer, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "runtime.download")
	var finalAttempt int
	if _, err := downloadRuntimeFile(
		context.Background(),
		server.URL,
		destination,
		func(progress runtimeDownloadProgress) error {
			if progress.Attempt > finalAttempt {
				finalAttempt = progress.Attempt
			}
			return nil
		},
	); err != nil {
		t.Fatalf("download runtime after retries: %v", err)
	}
	if requests.Load() != 3 || finalAttempt != 3 {
		t.Fatalf("expected success on attempt 3, requests=%d progress attempt=%d", requests.Load(), finalAttempt)
	}
	assertRuntimeDownloadContent(t, destination, payload)
}

func TestDownloadRuntimeFileCancellationPreservesPartialFile(t *testing.T) {
	payload := bytes.Repeat([]byte("preserve-partial"), 4096)
	partialSize := len(payload) / 2
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set(
			"Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", partialSize, len(payload)-1, len(payload)),
		)
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)-partialSize))
		writer.WriteHeader(http.StatusPartialContent)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "runtime.download")
	if err := os.WriteFile(destination, payload[:partialSize], 0o600); err != nil {
		t.Fatalf("write partial runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := downloadRuntimeFile(ctx, server.URL, destination, func(runtimeDownloadProgress) error {
		cancel()
		return nil
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled download, got %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("canceled download should not retry, got %d requests", requests.Load())
	}
	info, statErr := os.Stat(destination)
	if statErr != nil {
		t.Fatalf("stat partial runtime: %v", statErr)
	}
	if info.Size() != int64(partialSize) {
		t.Fatalf("expected %d preserved bytes, got %d", partialSize, info.Size())
	}
}

func TestDownloadRuntimeFileKeepsLegacyPartialFile(t *testing.T) {
	payload := bytes.Repeat([]byte("legacy-partial"), 4096)
	partialSize := len(payload) / 4
	var requestedRange string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedRange = request.Header.Get("Range")
		writer.Header().Set(
			"Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", partialSize, len(payload)-1, len(payload)),
		)
		writer.Header().Set("Content-Length", fmt.Sprint(len(payload)-partialSize))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(payload[partialSize:])
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "runtime.download")
	if err := os.WriteFile(destination, payload[:partialSize], 0o600); err != nil {
		t.Fatalf("write legacy partial runtime: %v", err)
	}
	if _, err := os.Stat(runtimeDownloadMetadataPath(destination)); !os.IsNotExist(err) {
		t.Fatalf("legacy partial unexpectedly has source metadata: %v", err)
	}
	if _, err := downloadRuntimeFile(context.Background(), server.URL, destination, nil); err != nil {
		t.Fatalf("resume legacy partial runtime: %v", err)
	}
	if requestedRange != fmt.Sprintf("bytes=%d-", partialSize) {
		t.Fatalf("expected legacy partial to be resumed, got range %q", requestedRange)
	}
	assertRuntimeDownloadContent(t, destination, payload)
}

func assertRuntimeDownloadContent(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded runtime: %v", err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("downloaded content mismatch: got %d bytes, expected %d", len(actual), len(expected))
	}
}

func newRuntimeTestService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	return &Service{
		config: Config{
			StateFile:  filepath.Join(root, "state", "nav-oss-state.json"),
			RuntimeDir: filepath.Join(root, "runtimes"),
		},
		runtimeProcesses: make(map[string]*runtimeProcess),
		runtimeTasks:     make(map[string]*runtimeTask),
	}
}
