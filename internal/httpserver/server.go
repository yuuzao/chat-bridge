// Package httpserver 提供任务创建、回调、状态查询和文件下载四个接口。
package httpserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strconv"

	"chat_bridge/internal/config"
	"chat_bridge/internal/forward"
	"chat_bridge/internal/taskstore"
)

// Server 处理 Chat Bridge 的 HTTP 请求。
type Server struct {
	cfg     config.Config
	store   *taskstore.Store
	forward *forward.Client
	log     *slog.Logger
}

// New 校验依赖并返回服务。配置里的监听地址由进程自行绑定，这里不使用。
func New(cfg config.Config, store *taskstore.Store, fwd *forward.Client, logger *slog.Logger) (*Server, error) {
	if store == nil || fwd == nil || logger == nil {
		return nil, errors.New("store, forward client, and logger are required")
	}
	if cfg.AuthToken == "" || cfg.MaxBodyBytes <= 0 {
		return nil, errors.New("auth token and max body bytes are required")
	}
	return &Server{cfg: cfg, store: store, forward: fwd, log: logger}, nil
}

// Handler 返回只包含四个业务路由的 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("POST /api/webhook", s.webhook)
	mux.HandleFunc("GET /api/tasks/{id}/file", s.getFile)
	mux.HandleFunc("GET /api/tasks/{id}", s.getTask)
	return mux
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	in, err := s.readInbound(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.authorize(r, in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := validateInbound(inboundCreate, in); err != nil {
		s.writeError(w, r, err)
		return
	}
	view, err := s.store.Create()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := writeJSON(w, http.StatusAccepted, createResponse{RequestID: view.ID, Status: view.Status}); err != nil {
		if delErr := s.store.Delete(view.ID); delErr != nil {
			s.log.Error("remove task after create write failure", "request_id", view.ID, "error", delErr.Error())
		}
		s.log.Error("write create response", "request_id", view.ID, "error", err.Error())
		return
	}
	uploadFile := in.file != nil
	uploadBytes := 0
	uploadName := ""
	if uploadFile {
		uploadBytes = len(in.file.data)
		uploadName = in.file.name
	}
	s.log.Info("task created",
		"request_id", view.ID,
		"content_bytes", len(in.content),
		"upload_file", uploadFile,
		"upload_bytes", uploadBytes,
		"upload_name", uploadName,
	)
	go s.forwardTask(view.ID, in)
}

func (s *Server) forwardTask(id string, in inbound) {
	msg := forward.Message{RequestID: id, Content: in.content}
	if in.file != nil {
		msg.File = &forward.File{Name: in.file.name, MIME: in.file.mime, Bytes: in.file.data}
	}
	err := s.forward.Send(context.Background(), msg)
	if err == nil {
		s.log.Info("bot accepted task", "request_id", id)
		return
	}
	s.log.Error("bot forward failed", "request_id", id, "error", err.Error())
	if failErr := s.store.Fail(id, err.Error()); failErr != nil {
		s.log.Warn("forward failure did not change task", "request_id", id, "error", failErr.Error())
	}
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	in, err := s.readInbound(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.authorize(r, in); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := validateInbound(inboundWebhook, in); err != nil {
		s.writeError(w, r, err)
		return
	}
	result := taskstore.Result{Content: in.content}
	if in.file != nil {
		result.HasFile = true
		result.FileName = in.file.name
		result.FileMIME = in.file.mime
		result.FileBytes = in.file.data
	}
	view, err := s.store.Complete(in.requestID, result)
	if errors.Is(err, taskstore.ErrNotFound) {
		s.log.Warn("webhook for missing task", "request_id", in.requestID)
		s.writeMissing(w, in.requestID)
		return
	}
	if errors.Is(err, taskstore.ErrNotPending) {
		s.log.Warn("duplicate webhook", "request_id", in.requestID, "status", string(view.Status))
		s.respond(w, http.StatusOK, webhookResponse{RequestID: view.ID, Status: view.Status})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	hasFile := in.file != nil
	s.log.Info("webhook applied",
		"request_id", in.requestID,
		"content_bytes", len(in.content),
		"has_file", hasFile,
	)
	s.respond(w, http.StatusOK, webhookResponse{RequestID: view.ID, Status: view.Status})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if err := s.authorizeHeader(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if !taskstore.ValidID(id) {
		s.writeMissing(w, id)
		return
	}
	wait, err := parseWait(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	view, err := s.waitView(r.Context(), id, wait)
	if err != nil {
		s.writeWaitError(w, r, id, err)
		return
	}
	s.finishStatus(w, view)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	if err := s.authorizeHeader(r); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if !taskstore.ValidID(id) {
		s.writeMissing(w, id)
		return
	}
	wait, err := parseWait(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	view, err := s.waitView(r.Context(), id, wait)
	if err != nil {
		s.writeWaitError(w, r, id, err)
		return
	}
	if view.Status != taskstore.StatusDone || !view.HasFile {
		s.writeFileUnavailable(w, view)
		return
	}
	file, err := s.store.File(id)
	if errors.Is(err, taskstore.ErrNotFound) {
		s.writeMissing(w, id)
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := writeFile(w, file); err != nil {
		s.log.Error("write file response", "request_id", id, "error", err.Error())
		return
	}
	if err := s.store.Delete(id); err != nil {
		if !errors.Is(err, taskstore.ErrNotFound) {
			s.log.Error("remove task after file", "request_id", id, "error", err.Error())
		}
		return
	}
	s.log.Info("task removed", "request_id", id, "reason", "file_read", "file_bytes", len(file.Bytes), "file_name", file.Name)
}

func (s *Server) finishStatus(w http.ResponseWriter, view taskstore.View) {
	if err := writeJSON(w, http.StatusOK, statusFromView(view)); err != nil {
		s.log.Error("write status response", "request_id", view.ID, "error", err.Error())
		return
	}
	if !shouldRemoveAfterStatus(view) {
		if view.Status == taskstore.StatusPending {
			s.log.Debug("task status served", "request_id", view.ID, "status", string(view.Status), "removed", false)
			return
		}
		s.log.Info("task status served", "request_id", view.ID, "status", string(view.Status), "removed", false)
		return
	}
	if err := s.store.Delete(view.ID); err != nil {
		if !errors.Is(err, taskstore.ErrNotFound) {
			s.log.Error("remove task after status", "request_id", view.ID, "error", err.Error())
		}
		return
	}
	s.log.Info("task removed", "request_id", view.ID, "reason", "status_read", "status", string(view.Status))
}

func (s *Server) writeWaitError(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, taskstore.ErrNotFound) {
		s.writeMissing(w, id)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.log.Debug("wait canceled", "request_id", id, "error", err.Error())
		return
	}
	s.writeError(w, r, err)
}

func (s *Server) writeFileUnavailable(w http.ResponseWriter, view taskstore.View) {
	switch view.Status {
	case taskstore.StatusPending:
		s.log.Debug("file not ready", "request_id", view.ID, "status", string(view.Status))
		s.respond(w, http.StatusConflict, statusWithError(view, "file not ready"))
	case taskstore.StatusFailed:
		if view.Failure == "" {
			s.log.Error("failed task missing error", "request_id", view.ID)
			s.respond(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
			return
		}
		s.log.Info("file unavailable", "request_id", view.ID, "status", string(view.Status))
		s.respond(w, http.StatusConflict, statusFromView(view))
	case taskstore.StatusDone:
		if view.HasFile {
			s.log.Error("file marked present but unavailable", "request_id", view.ID)
			s.respond(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
			return
		}
		s.log.Info("file unavailable", "request_id", view.ID, "status", string(view.Status))
		s.respond(w, http.StatusNotFound, statusWithError(view, "task has no file"))
	default:
		s.log.Error("unexpected task status", "request_id", view.ID, "status", string(view.Status))
		s.respond(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
	}
}

func shouldRemoveAfterStatus(view taskstore.View) bool {
	if view.Status == taskstore.StatusFailed {
		return true
	}
	return view.Status == taskstore.StatusDone && !view.HasFile
}

func writeFile(w http.ResponseWriter, file taskstore.File) error {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": file.Name})
	if disposition == "" {
		return errors.New("format content disposition")
	}
	w.Header().Set("Content-Type", file.MIME)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.Itoa(len(file.Bytes)))
	w.WriteHeader(http.StatusOK)
	n, err := w.Write(file.Bytes)
	if err != nil {
		return err
	}
	if n != len(file.Bytes) {
		return errors.New("short file write")
	}
	return nil
}

func (s *Server) authorizeHeader(r *http.Request) error {
	return s.authorize(r, inbound{})
}

func (s *Server) authorize(r *http.Request, in inbound) error {
	header, headerPresent, err := headerToken(r)
	if err != nil {
		return err
	}
	if headerPresent && in.authPresent && header != in.authToken {
		return statusErr(http.StatusUnauthorized, "auth_token mismatch")
	}
	var token string
	switch {
	case headerPresent:
		token = header
	case in.authPresent:
		token = in.authToken
	default:
		return statusErr(http.StatusUnauthorized, "unauthorized")
	}
	if token == "" || !tokenMatches(token, s.cfg.AuthToken) {
		return statusErr(http.StatusUnauthorized, "unauthorized")
	}
	return nil
}

func headerToken(r *http.Request) (string, bool, error) {
	values := r.Header.Values("X-Auth-Token")
	switch len(values) {
	case 0:
		return "", false, nil
	case 1:
		return values[0], true, nil
	default:
		return "", false, statusErr(http.StatusBadRequest, "duplicate X-Auth-Token")
	}
}

func tokenMatches(got, want string) bool {
	sumGot := sha256.Sum256([]byte(got))
	sumWant := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(sumGot[:], sumWant[:]) == 1
}
