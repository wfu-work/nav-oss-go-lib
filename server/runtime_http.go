package server

import (
	"net/http"
	"strings"
)

func (s *Service) handleInstallations(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		writeSuccess(writer, s.installations())
	case http.MethodPost:
		var input RuntimeInstallInput
		if err := decodeJSON(writer, request, &input); err != nil {
			writeAppError(writer, err)
			return
		}
		installation, err := s.createInstallation(input)
		if err != nil {
			writeAppError(writer, err)
			return
		}
		writeSuccess(writer, installation)
	default:
		writeMethodNotAllowed(writer, http.MethodGet, http.MethodPost)
	}
}

func (s *Service) handleInstallation(writer http.ResponseWriter, request *http.Request, suffix string) {
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeNotFound(writer)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch request.Method {
		case http.MethodGet:
			installation := s.installation(id)
			if installation == nil {
				writeNotFound(writer)
				return
			}
			writeSuccess(writer, installation)
		case http.MethodDelete:
			if err := s.deleteInstallation(id); err != nil {
				writeAppError(writer, err)
				return
			}
			writeSuccess(writer, true)
		default:
			writeMethodNotAllowed(writer, http.MethodGet, http.MethodDelete)
		}
		return
	}
	if len(parts) != 2 || request.Method != http.MethodPost {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}
	var (
		installation RuntimeInstallation
		err          error
	)
	switch parts[1] {
	case "retry":
		installation, err = s.retryInstallation(id)
	case "start":
		installation, err = s.startInstallation(id)
	case "stop":
		installation, err = s.stopInstallation(id)
	case "restart":
		installation, err = s.restartInstallation(id)
	default:
		writeNotFound(writer)
		return
	}
	if err != nil {
		writeAppError(writer, err)
		return
	}
	writeSuccess(writer, installation)
}
