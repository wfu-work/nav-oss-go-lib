package webs

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	archiveBrowserRoot = "nav-oss-lib-web/browser/"
	maxArchiveSize     = 64 << 20
)

var hashedAssetName = regexp.MustCompile(`-[A-Z0-9]{8,}\.[^.]+$`)

type asset struct {
	name        string
	data        []byte
	contentType string
	etag        string
	modified    time.Time
}

// Handler serves the embedded nav-oss web application.
//
// Handler is safe for concurrent use after NewHandler returns.
type Handler struct {
	assets map[string]asset
	index  asset
}

// NewHandler loads the embedded web archive and returns a standard library
// HTTP handler. The current web build targets the site root and should be
// mounted at "/".
func NewHandler() (*Handler, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("webs: open embedded archive: %w", err)
	}

	assets := make(map[string]asset)
	var totalSize uint64

	for _, file := range reader.File {
		name, ok := browserAssetPath(file.Name)
		if !ok || file.FileInfo().IsDir() {
			continue
		}
		if file.UncompressedSize64 > maxArchiveSize-totalSize {
			return nil, fmt.Errorf("webs: browser assets exceed %d bytes", maxArchiveSize)
		}
		if _, exists := assets[name]; exists {
			return nil, fmt.Errorf("webs: duplicate browser asset %q", name)
		}

		content, err := readArchiveFile(file)
		if err != nil {
			return nil, err
		}
		totalSize += uint64(len(content))

		assets[name] = asset{
			name:        name,
			data:        content,
			contentType: matchContentType(name),
			etag:        contentETag(content),
			modified:    file.Modified.UTC(),
		}
	}

	index, ok := assets["index.html"]
	if !ok {
		return nil, fmt.Errorf("webs: %sindex.html is missing", archiveBrowserRoot)
	}

	return &Handler{assets: assets, index: index}, nil
}

// ServeHTTP serves static assets and falls back to index.html for client-side
// routes. API paths and missing file-like paths deliberately return 404.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	requestPath, ok := cleanRequestPath(request.URL.Path)
	if !ok || isReservedPath(requestPath) {
		http.NotFound(writer, request)
		return
	}

	if requestedAsset, exists := h.assets[requestPath]; exists {
		h.serveAsset(writer, request, requestedAsset)
		return
	}

	if looksLikeAsset(requestPath) || !acceptsHTML(request) {
		http.NotFound(writer, request)
		return
	}

	h.serveAsset(writer, request, h.index)
}

func browserAssetPath(name string) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	roots := [...]string{archiveBrowserRoot, "browser/"}
	for _, root := range roots {
		if !strings.HasPrefix(name, root) {
			continue
		}

		relative := strings.TrimPrefix(name, root)
		if relative == "" {
			return "", false
		}
		if strings.Contains(relative, "\\") || strings.HasPrefix(relative, "/") {
			return "", false
		}
		cleaned := path.Clean(relative)
		if cleaned == "." || cleaned != relative || strings.HasPrefix(cleaned, "../") {
			return "", false
		}
		return cleaned, true
	}
	return "", false
}

func readArchiveFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("webs: open %q: %w", file.Name, err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("webs: read %q: %w", file.Name, err)
	}
	return content, nil
}

func cleanRequestPath(requestPath string) (string, bool) {
	if requestPath == "" || requestPath == "/" {
		return "index.html", true
	}
	if strings.Contains(requestPath, "\\") {
		return "", false
	}
	for _, segment := range strings.Split(requestPath, "/") {
		if segment == ".." {
			return "", false
		}
	}

	cleaned := path.Clean("/" + strings.TrimPrefix(requestPath, "/"))
	return strings.TrimPrefix(cleaned, "/"), true
}

func isReservedPath(requestPath string) bool {
	first, _, _ := strings.Cut(requestPath, "/")
	return first == "api" || first == "oss"
}

func looksLikeAsset(requestPath string) bool {
	return path.Ext(path.Base(requestPath)) != ""
}

func acceptsHTML(request *http.Request) bool {
	accept := request.Header.Get("Accept")
	return accept == "" || strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*")
}

func (h *Handler) serveAsset(writer http.ResponseWriter, request *http.Request, requestedAsset asset) {
	writer.Header().Set("Content-Type", requestedAsset.contentType)
	writer.Header().Set("ETag", requestedAsset.etag)

	if requestedAsset.name == "index.html" {
		writer.Header().Set("Cache-Control", "no-cache")
	} else if hashedAssetName.MatchString(path.Base(requestedAsset.name)) {
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		writer.Header().Set("Cache-Control", "public, max-age=86400")
	}

	http.ServeContent(
		writer,
		request,
		requestedAsset.name,
		requestedAsset.modified,
		bytes.NewReader(requestedAsset.data),
	)
}

func matchContentType(fileName string) string {
	switch strings.ToLower(path.Ext(fileName)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".wasm":
		return "application/wasm"
	}

	if contentType := mime.TypeByExtension(path.Ext(fileName)); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

func contentETag(content []byte) string {
	sum := sha256.Sum256(content)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
