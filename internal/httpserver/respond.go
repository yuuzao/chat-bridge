package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"chat_bridge/internal/taskstore"
)

type statusError struct {
	code  int
	msg   string
	cause error
}

func (e *statusError) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *statusError) Unwrap() error {
	return e.cause
}

func statusErr(code int, msg string) error {
	return &statusError{code: code, msg: msg}
}

func statusErrCause(code int, msg string, cause error) error {
	return &statusError{code: code, msg: msg, cause: cause}
}

type errorResponse struct {
	Error string `json:"error"`
}

type idErrorResponse struct {
	RequestID string `json:"request_id"`
	Error     string `json:"error"`
}

type createResponse struct {
	RequestID string           `json:"request_id"`
	Status    taskstore.Status `json:"status"`
}

type webhookResponse struct {
	RequestID string           `json:"request_id"`
	Status    taskstore.Status `json:"status"`
}

type statusResponse struct {
	RequestID string           `json:"request_id"`
	Status    taskstore.Status `json:"status"`
	Content   string           `json:"content"`
	Error     string           `json:"error"`
	HasFile   bool             `json:"has_file"`
	FileName  string           `json:"file_name"`
	FileMIME  string           `json:"file_mime"`
}

func statusFromView(view taskstore.View) statusResponse {
	return statusResponse{
		RequestID: view.ID,
		Status:    view.Status,
		Content:   view.Content,
		Error:     view.Failure,
		HasFile:   view.HasFile,
		FileName:  view.FileName,
		FileMIME:  view.FileMIME,
	}
}

func statusWithError(view taskstore.View, message string) statusResponse {
	resp := statusFromView(view)
	resp.Error = message
	return resp
}

func writeJSON(w http.ResponseWriter, code int, payload any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

func (s *Server) respond(w http.ResponseWriter, code int, payload any) {
	if err := writeJSON(w, code, payload); err != nil {
		s.log.Error("write response", "status", code, "error", err.Error())
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var httpErr *statusError
	if errors.As(err, &httpErr) {
		attrs := []any{
			"path", r.URL.Path,
			"status", httpErr.code,
			"error", httpErr.msg,
			"remote_addr", r.RemoteAddr,
		}
		if httpErr.cause != nil {
			attrs = append(attrs, "cause", httpErr.cause.Error())
		}
		if httpErr.code >= http.StatusInternalServerError {
			s.log.Error("request failed", attrs...)
		} else {
			s.log.Warn("request rejected", attrs...)
		}
		s.respond(w, httpErr.code, errorResponse{Error: httpErr.msg})
		return
	}
	s.log.Error("request failed", "path", r.URL.Path, "error", err.Error(), "remote_addr", r.RemoteAddr)
	s.respond(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
}

func (s *Server) writeMissing(w http.ResponseWriter, id string) {
	s.log.Warn("task not found", "request_id", id)
	s.respond(w, http.StatusNotFound, idErrorResponse{RequestID: id, Error: "task not found"})
}
