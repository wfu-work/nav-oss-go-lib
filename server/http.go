package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

const apiPrefix = "/api/v1"

const (
	maxConsoleUploadBytes = int64(1 << 30)
	maxUploadMemory       = int64(32 << 20)
	maxMultipartOverhead  = int64(1 << 20)
)

type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg,omitempty"`
	Data any    `json:"data"`
}

// Handler returns the authenticated nav-oss management API handler.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Service) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")

	path := strings.TrimSuffix(request.URL.Path, "/")
	if path == apiPrefix+"/health" {
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer, http.MethodGet)
			return
		}
		writeSuccess(writer, map[string]any{"status": "ok", "service": "nav-oss", "version": "v1"})
		return
	}
	if path == apiPrefix+"/auth/login" {
		s.handleLogin(writer, request)
		return
	}

	token := bearerToken(request)
	if token == "" || !s.authorized(token) {
		writeUnauthorized(writer)
		return
	}

	switch {
	case path == apiPrefix+"/auth/me":
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer, http.MethodGet)
			return
		}
		writeSuccess(writer, map[string]any{
			"username":     s.config.AdminUsername,
			"roleCodeList": []string{"admin"},
			"abilityList":  []string{"*"},
		})
	case path == apiPrefix+"/auth/logout":
		if request.Method != http.MethodPost {
			writeMethodNotAllowed(writer, http.MethodPost)
			return
		}
		s.logout(token)
		writeSuccess(writer, true)
	case path == apiPrefix+"/auth/password":
		s.handlePassword(writer, request)
	case path == apiPrefix+"/dashboard":
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer, http.MethodGet)
			return
		}
		writeSuccess(writer, s.dashboard())
	case path == apiPrefix+"/system":
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer, http.MethodGet)
			return
		}
		writeSuccess(writer, s.systemInfo())
	case path == apiPrefix+"/runtimes/catalog":
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer, http.MethodGet)
			return
		}
		writeSuccess(writer, s.runtimeCatalog())
	case path == apiPrefix+"/installations":
		s.handleInstallations(writer, request)
	case strings.HasPrefix(path, apiPrefix+"/installations/"):
		s.handleInstallation(writer, request, strings.TrimPrefix(path, apiPrefix+"/installations/"))
	case path == apiPrefix+"/providers":
		s.handleProviders(writer, request)
	case strings.HasPrefix(path, apiPrefix+"/providers/"):
		s.handleProvider(writer, request, strings.TrimPrefix(path, apiPrefix+"/providers/"))
	case path == apiPrefix+"/objects":
		s.handleObjects(writer, request)
	case path == apiPrefix+"/objects/upload":
		s.handleObjectUpload(writer, request)
	case path == apiPrefix+"/objects/stat":
		s.handleObjectStat(writer, request)
	case path == apiPrefix+"/objects/content":
		s.handleObjectContent(writer, request)
	case path == apiPrefix+"/objects/presign-get":
		s.handlePresign(writer, request, false)
	case path == apiPrefix+"/objects/presign-put":
		s.handlePresign(writer, request, true)
	case path == apiPrefix+"/objects/copy":
		s.handleCopy(writer, request, false)
	case path == apiPrefix+"/objects/move":
		s.handleCopy(writer, request, true)
	case path == apiPrefix+"/multipart":
		s.handleMultipart(writer, request)
	case strings.HasPrefix(path, apiPrefix+"/multipart/"):
		s.handleMultipartItem(writer, request, strings.TrimPrefix(path, apiPrefix+"/multipart/"))
	case path == apiPrefix+"/operations":
		s.handleOperations(writer, request)
	case path == apiPrefix+"/settings":
		s.handleSettings(writer, request)
	default:
		writeNotFound(writer)
	}
}

func (s *Service) handleLogin(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		writeAppError(writer, err)
		return
	}
	token, expiresIn, err := s.login(strings.TrimSpace(input.Username), input.Password)
	if err != nil {
		writeApplicationCode(writer, http.StatusUnauthorized, err.Error())
		return
	}
	writeSuccess(writer, map[string]any{
		"token":     token,
		"tokenType": "Bearer",
		"expired":   expiresIn,
	})
}

func (s *Service) handlePassword(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut {
		writeMethodNotAllowed(writer, http.MethodPut)
		return
	}
	var input struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		writeAppError(writer, err)
		return
	}
	if err := s.changePassword(input.OldPassword, input.NewPassword); err != nil {
		writeAppError(writer, err)
		return
	}
	writeSuccess(writer, true)
}

func (s *Service) handleProviders(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		page, size := pageQuery(request)
		query := request.URL.Query()
		writeSuccess(writer, s.providers(page, size, query.Get("keyword"), query.Get("type"), query.Get("health"), query.Get("enabled")))
	case http.MethodPost:
		var input ProviderInput
		if err := decodeJSON(writer, request, &input); err != nil {
			writeAppError(writer, err)
			return
		}
		provider, err := s.saveProvider(request.Context(), "", input)
		if err != nil {
			writeAppError(writer, err)
			return
		}
		writeSuccess(writer, provider)
	default:
		writeMethodNotAllowed(writer, http.MethodGet, http.MethodPost)
	}
}

func (s *Service) handleProvider(writer http.ResponseWriter, request *http.Request, suffix string) {
	parts := strings.Split(suffix, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeNotFound(writer)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch request.Method {
		case http.MethodGet:
			writeSuccess(writer, s.provider(id))
		case http.MethodPut:
			var input ProviderInput
			if err := decodeJSON(writer, request, &input); err != nil {
				writeAppError(writer, err)
				return
			}
			provider, err := s.saveProvider(request.Context(), id, input)
			if err != nil {
				writeAppError(writer, err)
				return
			}
			writeSuccess(writer, provider)
		default:
			writeMethodNotAllowed(writer, http.MethodGet, http.MethodPut)
		}
		return
	}
	if request.Method != http.MethodPost || len(parts) != 2 {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}
	var (
		provider Provider
		err      error
	)
	switch parts[1] {
	case "health":
		provider, err = s.checkProvider(request.Context(), id)
	case "enable":
		provider, err = s.setProviderEnabled(request.Context(), id, true)
	case "disable":
		provider, err = s.setProviderEnabled(request.Context(), id, false)
	default:
		writeNotFound(writer)
		return
	}
	if err != nil {
		writeAppError(writer, err)
		return
	}
	writeSuccess(writer, provider)
}

func (s *Service) handleObjects(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		page, size := pageQuery(request)
		query := request.URL.Query()
		result, err := s.listObjects(
			request.Context(),
			query.Get("providerId"),
			query.Get("prefix"),
			query.Get("keyword"),
			query.Get("sortBy"),
			query.Get("sortOrder"),
			page,
			size,
		)
		if err != nil {
			writeAppError(writer, err)
			return
		}
		writeSuccess(writer, result)
	case http.MethodDelete:
		var input struct {
			ProviderID string `json:"providerId"`
			Key        string `json:"key"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeAppError(writer, err)
			return
		}
		if err := s.deleteObject(request.Context(), input.ProviderID, input.Key); err != nil {
			writeAppError(writer, err)
			return
		}
		writeSuccess(writer, true)
	default:
		writeMethodNotAllowed(writer, http.MethodGet, http.MethodDelete)
	}
}

func (s *Service) handleObjectUpload(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}

	request.Body = http.MaxBytesReader(
		writer,
		request.Body,
		maxConsoleUploadBytes+maxMultipartOverhead,
	)
	if err := request.ParseMultipartForm(maxUploadMemory); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeJSON(writer, http.StatusRequestEntityTooLarge, apiResponse{
				Code: http.StatusRequestEntityTooLarge,
				Msg:  "文件大小不能超过 1 GiB",
				Data: nil,
			})
			return
		}
		writeAppError(writer, fmt.Errorf("解析上传文件失败: %w", err))
		return
	}
	if request.MultipartForm != nil {
		defer request.MultipartForm.RemoveAll()
	}

	file, header, err := request.FormFile("file")
	if err != nil {
		writeAppError(writer, errors.New("请选择要上传的文件"))
		return
	}
	defer file.Close()

	key := strings.TrimSpace(request.FormValue("key"))
	if key == "" {
		key = filepath.Base(header.Filename)
	}
	if key == "." || key == "" {
		writeAppError(writer, errors.New("对象 Key 不能为空"))
		return
	}

	contentType := strings.TrimSpace(request.FormValue("contentType"))
	if contentType == "" {
		contentType = strings.TrimSpace(header.Header.Get("Content-Type"))
	}
	if contentType == "" || contentType == "application/octet-stream" {
		if inferred := mime.TypeByExtension(filepath.Ext(key)); inferred != "" {
			contentType = inferred
		}
	}

	object, err := s.putObject(
		request.Context(),
		strings.TrimSpace(request.FormValue("providerId")),
		key,
		file,
		header.Size,
		contentType,
	)
	if err != nil {
		writeAppError(writer, err)
		return
	}
	writeSuccess(writer, object)
}

func (s *Service) handleObjectStat(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	query := request.URL.Query()
	result, err := s.statObject(request.Context(), query.Get("providerId"), query.Get("key"))
	if err != nil {
		writeAppError(writer, err)
		return
	}
	writeSuccess(writer, result)
}

func (s *Service) handleObjectContent(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	query := request.URL.Query()
	reader, info, err := s.openObject(request.Context(), query.Get("providerId"), query.Get("key"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, apiResponse{Code: http.StatusBadRequest, Msg: err.Error(), Data: nil})
		return
	}
	defer reader.Close()

	contentType := strings.TrimSpace(info.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	writer.Header().Set("Content-Disposition", "inline")
	if _, err = io.Copy(writer, reader); err != nil && request.Context().Err() == nil {
		return
	}
}

func (s *Service) handlePresign(writer http.ResponseWriter, request *http.Request, put bool) {
	if request.Method != http.MethodPost {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}
	var input presignRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeAppError(writer, err)
		return
	}
	result, err := s.presignObject(request.Context(), put, input)
	if err != nil {
		writeAppError(writer, err)
		return
	}
	writeSuccess(writer, result)
}

func (s *Service) handleCopy(writer http.ResponseWriter, request *http.Request, move bool) {
	if request.Method != http.MethodPost {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}
	var input copyRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeAppError(writer, err)
		return
	}
	result, err := s.copyObject(request.Context(), move, input)
	if err != nil {
		writeAppError(writer, err)
		return
	}
	writeSuccess(writer, result)
}

func (s *Service) handleMultipart(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	page, size := pageQuery(request)
	writeSuccess(writer, pageResult[any]{Data: []any{}, Total: 0, Page: page, Size: size})
}

func (s *Service) handleMultipartItem(writer http.ResponseWriter, request *http.Request, suffix string) {
	parts := strings.Split(suffix, "/")
	if len(parts) == 2 && parts[1] == "parts" && request.Method == http.MethodGet {
		writeSuccess(writer, []any{})
		return
	}
	if len(parts) == 1 && request.Method == http.MethodDelete {
		writeAppError(writer, errors.New("底层 Provider 未提供 Multipart 会话枚举，无法按 ID 终止未知会话"))
		return
	}
	writeNotFound(writer)
}

func (s *Service) handleOperations(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	page, size := pageQuery(request)
	query := request.URL.Query()
	writeSuccess(writer, s.operations(page, size, query.Get("keyword"), query.Get("result"), query.Get("action")))
}

func (s *Service) handleSettings(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		writeSuccess(writer, s.settings())
	case http.MethodPut:
		var input Settings
		if err := decodeJSON(writer, request, &input); err != nil {
			writeAppError(writer, err)
			return
		}
		settings, err := s.saveSettings(input)
		if err != nil {
			writeAppError(writer, err)
			return
		}
		writeSuccess(writer, settings)
	default:
		writeMethodNotAllowed(writer, http.MethodGet, http.MethodPut)
	}
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("请求内容不是有效 JSON: %w", err)
	}
	return nil
}

func pageQuery(request *http.Request) (int, int) {
	page, _ := strconv.Atoi(request.URL.Query().Get("page"))
	size, _ := strconv.Atoi(request.URL.Query().Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	return page, size
}

func bearerToken(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	prefix, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(prefix, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func writeSuccess(writer http.ResponseWriter, data any) {
	writeJSON(writer, http.StatusOK, apiResponse{Code: http.StatusOK, Data: data})
}

func writeApplicationCode(writer http.ResponseWriter, code int, message string) {
	writeJSON(writer, http.StatusOK, apiResponse{Code: code, Msg: message, Data: nil})
}

func writeAppError(writer http.ResponseWriter, err error) {
	writeApplicationCode(writer, http.StatusBadRequest, err.Error())
}

func writeUnauthorized(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusUnauthorized, apiResponse{Code: http.StatusUnauthorized, Msg: "登录已过期，请重新登录", Data: nil})
}

func writeNotFound(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusNotFound, apiResponse{Code: http.StatusNotFound, Msg: "接口不存在", Data: nil})
}

func writeMethodNotAllowed(writer http.ResponseWriter, methods ...string) {
	writer.Header().Set("Allow", strings.Join(methods, ", "))
	writeJSON(writer, http.StatusMethodNotAllowed, apiResponse{Code: http.StatusMethodNotAllowed, Msg: "请求方法不允许", Data: nil})
}

func writeJSON(writer http.ResponseWriter, status int, response apiResponse) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(response)
}
