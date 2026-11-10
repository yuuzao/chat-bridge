package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"chat_bridge/internal/config"
	"chat_bridge/internal/forward"
	"chat_bridge/internal/taskstore"
)

func TestJSONTaskAndTextWebhook(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	resp := h.do(t, h.jsonReq(t, http.MethodPost, "/api/tasks", `{"content":"hello <b>"}`, h.token))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readRaw(t, resp))
	}
	created := decodeJSON[createResponse](t, resp)
	if created.Status != taskstore.StatusPending || !taskstore.ValidID(created.RequestID) {
		t.Fatalf("created = %+v", created)
	}
	forwarded := h.awaitBot(t)
	if forwarded.token != "Bearer "+h.bearer || !strings.Contains(forwarded.contentType, "application/json") {
		t.Fatalf("forward = %+v", forwarded)
	}
	var sent struct {
		RequestID string `json:"request_id"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(forwarded.body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.RequestID != created.RequestID || sent.Content != "hello <b>" {
		t.Fatalf("sent = %+v", sent)
	}

	waitResult := make(chan httpResult, 1)
	go func() {
		resp, err := h.get("/api/tasks/" + created.RequestID + "?wait=5")
		waitResult <- httpResult{resp: resp, err: err}
	}()
	time.Sleep(150 * time.Millisecond)
	hook := h.do(t, h.jsonReq(t, http.MethodPost, "/api/webhook", `{"request_id":"`+created.RequestID+`","content":"reply-secret-text"}`, h.token))
	if hook.StatusCode != http.StatusOK {
		t.Fatalf("webhook = %d %s", hook.StatusCode, readRaw(t, hook))
	}
	waited := <-waitResult
	if waited.err != nil {
		t.Fatal(waited.err)
	}
	t.Cleanup(func() {
		_ = waited.resp.Body.Close()
	})
	got := decodeJSON[statusResponse](t, waited.resp)
	if got.Status != taskstore.StatusDone || got.Content != "reply-secret-text" || got.HasFile {
		t.Fatalf("status = %+v", got)
	}
	missing := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("second status = %d", missing.StatusCode)
	}
	if strings.Contains(h.logs.String(), "reply-secret-text") || strings.Contains(h.logs.String(), h.token) {
		t.Fatalf("log leaked data: %s", h.logs.String())
	}
	if !strings.Contains(h.logs.String(), created.RequestID) {
		t.Fatalf("log missing request id: %s", h.logs.String())
	}
}

func TestFileRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	contentType, body := multipartBody(t, map[string]string{"content": "draw"}, "笔记.txt", "image/png", []byte{0, 1, 255})
	resp := h.do(t, h.rawReq(t, http.MethodPost, "/api/tasks", contentType, body, h.token))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, readRaw(t, resp))
	}
	created := decodeJSON[createResponse](t, resp)
	forwarded := h.awaitBot(t)
	if !strings.HasPrefix(forwarded.contentType, "multipart/form-data") {
		t.Fatalf("content type = %s", forwarded.contentType)
	}
	fields, file := parseMultipart(t, forwarded.contentType, forwarded.body)
	if fields["request_id"] != created.RequestID || fields["content"] != "draw" || !bytes.Equal(file, []byte{0, 1, 255}) {
		t.Fatalf("fields = %#v file = %v", fields, file)
	}
	statusResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	if statusResp.StatusCode != http.StatusOK {
		t.Fatal(statusResp.StatusCode)
	}
	pending := decodeJSON[statusResponse](t, statusResp)
	if pending.Status != taskstore.StatusPending {
		t.Fatalf("pending = %+v", pending)
	}

	fileResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID+"/file", "", h.token))
	if fileResp.StatusCode != http.StatusConflict {
		t.Fatalf("early file = %d", fileResp.StatusCode)
	}

	hookType, hookBody := multipartBody(t, map[string]string{
		"request_id": created.RequestID,
		"content":    "reply-secret-text",
	}, "笔记.txt", "image/png", []byte{0, 1, 255})
	hook := h.do(t, h.rawReq(t, http.MethodPost, "/api/webhook", hookType, hookBody, h.token))
	if hook.StatusCode != http.StatusOK {
		t.Fatalf("webhook = %d %s", hook.StatusCode, readRaw(t, hook))
	}
	metaResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	meta := decodeJSON[statusResponse](t, metaResp)
	if metaResp.StatusCode != http.StatusOK || !meta.HasFile || meta.FileName != "笔记.txt" || meta.Content != "reply-secret-text" {
		t.Fatalf("meta status %d %+v", metaResp.StatusCode, meta)
	}
	again := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	againMeta := decodeJSON[statusResponse](t, again)
	if again.StatusCode != http.StatusOK || againMeta.FileName != "笔记.txt" {
		t.Fatalf("status removed file metadata early: %d %+v", again.StatusCode, againMeta)
	}
	download := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID+"/file", "", h.token))
	if download.StatusCode != http.StatusOK {
		t.Fatalf("download = %d %s", download.StatusCode, readRaw(t, download))
	}
	if !bytes.Equal(readRaw(t, download), []byte{0, 1, 255}) {
		t.Fatal("file bytes mismatch")
	}
	_, params, err := mime.ParseMediaType(download.Header.Get("Content-Disposition"))
	if err != nil || params["filename"] != "笔记.txt" {
		t.Fatalf("disposition = %q err = %v", download.Header.Get("Content-Disposition"), err)
	}
	if download.Header.Get("Content-Type") != "image/png" {
		t.Fatal(download.Header.Get("Content-Type"))
	}
	gone := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID+"/file", "", h.token))
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("second download = %d", gone.StatusCode)
	}
	if strings.Contains(h.logs.String(), "reply-secret-text") || strings.Contains(h.logs.String(), h.token) {
		t.Fatalf("log leaked data: %s", h.logs.String())
	}
}

func TestFileWait(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	created := h.create(t, `{"content":"draw"}`)
	h.awaitBot(t)
	waitResult := make(chan httpResult, 1)
	go func() {
		resp, err := h.get("/api/tasks/" + created.RequestID + "/file?wait=5")
		waitResult <- httpResult{resp: resp, err: err}
	}()
	time.Sleep(150 * time.Millisecond)
	hookType, hookBody := multipartBody(t, map[string]string{
		"request_id": created.RequestID,
		"content":    "ok",
	}, "a.txt", "text/plain", []byte("xyz"))
	hook := h.do(t, h.rawReq(t, http.MethodPost, "/api/webhook", hookType, hookBody, h.token))
	if hook.StatusCode != http.StatusOK {
		t.Fatal(readRaw(t, hook))
	}
	waited := <-waitResult
	if waited.err != nil {
		t.Fatal(waited.err)
	}
	t.Cleanup(func() {
		_ = waited.resp.Body.Close()
	})
	if waited.resp.StatusCode != http.StatusOK || string(readRaw(t, waited.resp)) != "xyz" {
		t.Fatalf("wait file = %d", waited.resp.StatusCode)
	}
}

func TestTextResultFileEndpointKeepsTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	created := h.create(t, `{"content":"hi"}`)
	h.awaitBot(t)
	hook := h.do(t, h.jsonReq(t, http.MethodPost, "/api/webhook", `{"request_id":"`+created.RequestID+`","content":"only text"}`, h.token))
	if hook.StatusCode != http.StatusOK {
		t.Fatal(readRaw(t, hook))
	}
	fileResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID+"/file", "", h.token))
	fileBody := decodeJSON[statusResponse](t, fileResp)
	if fileResp.StatusCode != http.StatusNotFound || fileBody.Error != "task has no file" || fileBody.Content != "only text" {
		t.Fatalf("file = %d %+v", fileResp.StatusCode, fileBody)
	}
	statusResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	statusBody := decodeJSON[statusResponse](t, statusResp)
	if statusResp.StatusCode != http.StatusOK || statusBody.Content != "only text" {
		t.Fatalf("status = %d %+v", statusResp.StatusCode, statusBody)
	}
	gone := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("second status = %d", gone.StatusCode)
	}
}

func TestForwardFailureIsReadableOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{botStatus: http.StatusInternalServerError})
	created := h.create(t, `{"content":"hi"}`)
	h.awaitBot(t)
	statusResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID+"?wait=2", "", h.token))
	body := decodeJSON[statusResponse](t, statusResp)
	if statusResp.StatusCode != http.StatusOK || body.Status != taskstore.StatusFailed || !strings.Contains(body.Error, "HTTP 500") {
		t.Fatalf("status = %d %+v", statusResp.StatusCode, body)
	}
	gone := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("second status = %d", gone.StatusCode)
	}
}

func TestDuplicateWebhookKeepsFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	created := h.create(t, `{"content":"hi"}`)
	h.awaitBot(t)
	payload := `{"request_id":"` + created.RequestID + `","content":"once"}`
	first := h.do(t, h.jsonReq(t, http.MethodPost, "/api/webhook", payload, h.token))
	second := h.do(t, h.jsonReq(t, http.MethodPost, "/api/webhook", payload, h.token))
	if first.StatusCode != http.StatusOK || second.StatusCode != http.StatusOK {
		t.Fatalf("webhook %d %d", first.StatusCode, second.StatusCode)
	}
	secondBody := decodeJSON[webhookResponse](t, second)
	if secondBody.Status != taskstore.StatusDone {
		t.Fatalf("second = %+v", secondBody)
	}
	statusResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	body := decodeJSON[statusResponse](t, statusResp)
	if body.Content != "once" {
		t.Fatalf("content changed: %+v", body)
	}
}

func TestWaitTimeoutKeepsPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	created := h.create(t, `{"content":"hi"}`)
	h.awaitBot(t)
	start := time.Now()
	statusResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID+"?wait=1", "", h.token))
	body := decodeJSON[statusResponse](t, statusResp)
	if time.Since(start) < 900*time.Millisecond || body.Status != taskstore.StatusPending {
		t.Fatalf("elapsed = %s body = %+v", time.Since(start), body)
	}
	again := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID, "", h.token))
	againBody := decodeJSON[statusResponse](t, again)
	if again.StatusCode != http.StatusOK || againBody.Status != taskstore.StatusPending {
		t.Fatalf("again = %d %+v", again.StatusCode, againBody)
	}
}

func TestRejectedRequestsDoNotForward(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		path       string
		body       string
		media      string
		token      string
		wantStatus int
		wantError  string
	}{
		{name: "no token", path: "/api/tasks", body: `{"content":"hi"}`, media: "application/json", wantStatus: http.StatusUnauthorized, wantError: "unauthorized"},
		{name: "bad token", path: "/api/tasks", body: `{"content":"hi"}`, media: "application/json", token: "nope", wantStatus: http.StatusUnauthorized, wantError: "unauthorized"},
		{name: "client request id", path: "/api/tasks", body: `{"request_id":"11111111-1111-4111-8111-111111111111","content":"hi"}`, media: "application/json", token: "test-token", wantStatus: http.StatusBadRequest, wantError: "request_id is generated by the server"},
		{name: "missing content", path: "/api/tasks", body: `{"extra":false}`, media: "application/json", token: "test-token", wantStatus: http.StatusBadRequest, wantError: "unknown field"},
		{name: "duplicate content", path: "/api/tasks", body: `{"content":"a","content":"b"}`, media: "application/json", token: "test-token", wantStatus: http.StatusBadRequest, wantError: "duplicate field"},
		{name: "text body", path: "/api/tasks", body: "hi", media: "text/plain", token: "test-token", wantStatus: http.StatusUnsupportedMediaType, wantError: "unsupported content type"},
		{name: "bad wait", path: "/api/tasks/11111111-1111-4111-8111-111111111111?wait=0", media: "", token: "test-token", wantStatus: http.StatusBadRequest, wantError: "invalid wait"},
		{name: "unknown task", path: "/api/webhook", body: `{"request_id":"11111111-1111-4111-8111-111111111111","content":"x"}`, media: "application/json", token: "test-token", wantStatus: http.StatusNotFound, wantError: "task not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, harnessOptions{})
			method := http.MethodPost
			if strings.Contains(test.path, "/api/tasks/") {
				method = http.MethodGet
			}
			req := h.rawReq(t, method, test.path, test.media, []byte(test.body), test.token)
			resp := h.do(t, req)
			body := decodeJSON[errorResponse](t, resp)
			if resp.StatusCode != test.wantStatus || body.Error != test.wantError {
				t.Fatalf("status = %d body = %+v", resp.StatusCode, body)
			}
			h.assertNoBot(t)
		})
	}
}

func TestTokenMismatchAndBodyToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	mismatch := h.do(t, h.jsonReq(t, http.MethodPost, "/api/tasks", `{"auth_token":"other","content":"hi"}`, h.token))
	body := decodeJSON[errorResponse](t, mismatch)
	if mismatch.StatusCode != http.StatusUnauthorized || body.Error != "auth_token mismatch" {
		t.Fatalf("mismatch = %d %+v", mismatch.StatusCode, body)
	}
	h.assertNoBot(t)
	accepted := h.do(t, h.jsonReq(t, http.MethodPost, "/api/tasks", `{"auth_token":"test-token","content":"hi"}`, ""))
	if accepted.StatusCode != http.StatusAccepted {
		t.Fatalf("body token = %d %s", accepted.StatusCode, readRaw(t, accepted))
	}
	h.awaitBot(t)
}

func TestCharsetAndOversizedBody(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	req := h.rawReq(t, http.MethodPost, "/api/tasks", "application/json; charset=utf-8", []byte(`{"content":"hi"}`), h.token)
	resp := h.do(t, req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("charset = %d %s", resp.StatusCode, readRaw(t, resp))
	}
	h.awaitBot(t)

	small := newHarness(t, harnessOptions{maxBody: 32})
	payload := []byte(`{"content":"` + strings.Repeat("a", 80) + `"}`)
	over := small.do(t, small.rawReq(t, http.MethodPost, "/api/tasks", "application/json", payload, small.token))
	overBody := decodeJSON[errorResponse](t, over)
	if over.StatusCode != http.StatusRequestEntityTooLarge || overBody.Error != "request body too large" {
		t.Fatalf("over = %d %+v", over.StatusCode, overBody)
	}
}

func TestDuplicateFileAndMissingFileType(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("content", "hi"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		part, err := writer.CreatePart(fileHeader(t, name, "text/plain"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	resp := h.do(t, h.rawReq(t, http.MethodPost, "/api/tasks", writer.FormDataContentType(), buf.Bytes(), h.token))
	body := decodeJSON[errorResponse](t, resp)
	if resp.StatusCode != http.StatusBadRequest || body.Error != "duplicate field" {
		t.Fatalf("duplicate = %d %+v", resp.StatusCode, body)
	}

	var missing bytes.Buffer
	missingWriter := multipart.NewWriter(&missing)
	if err := missingWriter.WriteField("content", "hi"); err != nil {
		t.Fatal(err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="a.txt"`)
	part, err := missingWriter.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := missingWriter.Close(); err != nil {
		t.Fatal(err)
	}
	missingResp := h.do(t, h.rawReq(t, http.MethodPost, "/api/tasks", missingWriter.FormDataContentType(), missing.Bytes(), h.token))
	missingBody := decodeJSON[errorResponse](t, missingResp)
	if missingResp.StatusCode != http.StatusBadRequest || missingBody.Error != "missing file content type" {
		t.Fatalf("missing type = %d %+v", missingResp.StatusCode, missingBody)
	}
}

func TestForwardTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{
		forwardTimeout: 200 * time.Millisecond,
		bot: func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			w.WriteHeader(http.StatusNoContent)
		},
	})
	created := h.create(t, `{"content":"hi"}`)
	statusResp := h.do(t, h.jsonReq(t, http.MethodGet, "/api/tasks/"+created.RequestID+"?wait=2", "", h.token))
	body := decodeJSON[statusResponse](t, statusResp)
	if body.Status != taskstore.StatusFailed || !strings.Contains(body.Error, "bot request failed") {
		t.Fatalf("status = %+v", body)
	}
	if strings.Contains(body.Error, h.token) {
		t.Fatalf("failure leaked token: %s", body.Error)
	}
}

type harnessOptions struct {
	maxBody        int64
	forwardTimeout time.Duration
	botStatus      int
	bot            http.HandlerFunc
}

type httpResult struct {
	resp *http.Response
	err  error
}

type capture struct {
	token       string
	contentType string
	body        []byte
}

type harness struct {
	token    string
	bearer   string
	logs     *lockedBuffer
	captured chan capture
	api      *httptest.Server
	client   *http.Client
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	if opts.maxBody == 0 {
		opts.maxBody = 1 << 20
	}
	if opts.forwardTimeout == 0 {
		opts.forwardTimeout = 2 * time.Second
	}
	if opts.botStatus == 0 {
		opts.botStatus = http.StatusNoContent
	}
	h := &harness{
		token:    "test-token",
		bearer:   "bot-bearer",
		logs:     &lockedBuffer{},
		captured: make(chan capture, 8),
		client:   &http.Client{},
	}
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read bot body: %v", err)
			return
		}
		if r.Header.Get("X-Auth-Token") != "" {
			t.Errorf("bridge token was sent")
		}
		h.captured <- capture{token: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), body: body}
		if opts.bot != nil {
			opts.bot(w, r)
			return
		}
		w.WriteHeader(opts.botStatus)
	}))
	t.Cleanup(bot.Close)
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.Config{
		ListenAddr:      "127.0.0.1:0",
		AuthToken:       h.token,
		BotURL:          bot.URL,
		BotBearerToken:  h.bearer,
		MaxBodyBytes:    opts.maxBody,
		TaskTTL:         time.Minute,
		ForwardTimeout:  opts.forwardTimeout,
		CleanupInterval: time.Minute,
	}
	fwd, err := forward.New(cfg.BotURL, cfg.BotBearerToken, cfg.ForwardTimeout, cfg.MaxBodyBytes, logger)
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(cfg, taskstore.New(), fwd, logger)
	if err != nil {
		t.Fatal(err)
	}
	h.api = httptest.NewServer(api.Handler())
	t.Cleanup(h.api.Close)
	return h
}

func (h *harness) create(t *testing.T, payload string) createResponse {
	t.Helper()
	resp := h.do(t, h.jsonReq(t, http.MethodPost, "/api/tasks", payload, h.token))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, readRaw(t, resp))
	}
	return decodeJSON[createResponse](t, resp)
}

func (h *harness) awaitBot(t *testing.T) capture {
	t.Helper()
	select {
	case got := <-h.captured:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("bot was not called")
	}
	return capture{}
}

func (h *harness) assertNoBot(t *testing.T) {
	t.Helper()
	select {
	case got := <-h.captured:
		t.Fatalf("bot was called: %+v", got)
	default:
	}
}

func (h *harness) get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, h.api.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Auth-Token", h.token)
	return h.client.Do(req)
}

func (h *harness) jsonReq(t *testing.T, method, path, payload, token string) *http.Request {
	t.Helper()
	media := ""
	if payload != "" || method == http.MethodPost {
		media = "application/json"
	}
	return h.rawReq(t, method, path, media, []byte(payload), token)
}

func (h *harness) rawReq(t *testing.T, method, path, contentType string, body []byte, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, h.api.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	return req
}

func (h *harness) do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = resp.Body.Close()
	})
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var payload T
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload
}

func readRaw(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func multipartBody(t *testing.T, fields map[string]string, fileName, fileType string, file []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if fileName != "" {
		part, err := writer.CreatePart(fileHeader(t, fileName, fileType))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return writer.FormDataContentType(), buf.Bytes()
}

func fileHeader(t *testing.T, fileName, fileType string) textproto.MIMEHeader {
	t.Helper()
	disposition := mime.FormatMediaType("form-data", map[string]string{
		"name":     "file",
		"filename": fileName,
	})
	if disposition == "" {
		t.Fatal("empty disposition")
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", disposition)
	if fileType != "" {
		header.Set("Content-Type", fileType)
	}
	return header
}

func parseMultipart(t *testing.T, contentType string, body []byte) (map[string]string, []byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string]string{}
	var file []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return fields, file
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == "file" {
			file = data
			continue
		}
		fields[part.FormName()] = string(data)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
