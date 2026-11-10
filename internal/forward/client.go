// Package forward 把任务查询 POST 给 Bot。
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// File 是随查询一起转发出去的单个文件。
type File struct {
	Name  string
	MIME  string
	Bytes []byte
}

// Message 是一次转发的内容。Content 允许为空字符串。File 为空表示没有文件。
type Message struct {
	RequestID string
	Content   string
	File      *File
}

// Client 按固定协议向 Bot 发 POST。
type Client struct {
	url      string
	token    string
	maxDrain int64
	http     *http.Client
	log      *slog.Logger
}

// New 创建转发客户端。timeout 是整次请求的超时。重定向不会被跟随。
// bearerToken 是 Bot 要求的密钥，请求头写成 Authorization: Bearer <bearerToken>。
func New(botURL, bearerToken string, timeout time.Duration, maxDrain int64, logger *slog.Logger) (*Client, error) {
	if _, err := validateBotURL(botURL); err != nil {
		return nil, err
	}
	if bearerToken == "" {
		return nil, errors.New("bot bearer token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("forward timeout must be positive")
	}
	if maxDrain <= 0 {
		return nil, errors.New("max body bytes must be positive")
	}
	if logger == nil {
		return nil, errors.New("logger is required")
	}
	return &Client{
		url:      botURL,
		token:    bearerToken,
		maxDrain: maxDrain,
		log:      logger,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Send 发送查询。2xx 视为成功，不解析响应体。其他状态码和网络错误原样返回。
func (c *Client) Send(ctx context.Context, msg Message) error {
	if c == nil {
		return errors.New("forward client is required")
	}
	if ctx == nil {
		return errors.New("context is required")
	}
	if msg.RequestID == "" {
		return errors.New("request id is required")
	}
	if msg.File != nil {
		if msg.File.Name == "" || msg.File.MIME == "" || msg.File.Bytes == nil {
			return errors.New("file name, mime, and bytes are required")
		}
	}
	req, err := c.newRequest(ctx, msg)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bot request failed: %w", scrubURLError(err))
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			c.log.Warn("close bot response", "request_id", msg.RequestID, "error", closeErr.Error())
		}
	}()
	if _, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, c.maxDrain+1)); copyErr != nil {
		c.log.Warn("discard bot response", "request_id", msg.RequestID, "status", resp.StatusCode, "error", copyErr.Error())
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		return fmt.Errorf("bot responded with HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, msg Message) (*http.Request, error) {
	body, contentType, err := encodeMessage(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build bot request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

func encodeMessage(msg Message) ([]byte, string, error) {
	if msg.File == nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		payload := struct {
			RequestID string `json:"request_id"`
			Content   string `json:"content"`
		}{
			RequestID: msg.RequestID,
			Content:   msg.Content,
		}
		if err := enc.Encode(&payload); err != nil {
			return nil, "", fmt.Errorf("encode bot json: %w", err)
		}
		return buf.Bytes(), "application/json", nil
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("request_id", msg.RequestID); err != nil {
		return nil, "", fmt.Errorf("write bot request_id: %w", err)
	}
	if err := writer.WriteField("content", msg.Content); err != nil {
		return nil, "", fmt.Errorf("write bot content: %w", err)
	}
	disposition := mime.FormatMediaType("form-data", map[string]string{
		"name":     "file",
		"filename": msg.File.Name,
	})
	if disposition == "" {
		return nil, "", errors.New("format bot file disposition")
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", disposition)
	header.Set("Content-Type", msg.File.MIME)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("create bot file part: %w", err)
	}
	if _, err := part.Write(msg.File.Bytes); err != nil {
		return nil, "", fmt.Errorf("write bot file: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close bot multipart: %w", err)
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

func validateBotURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("bot url is required")
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return "", errors.New("bot url must not contain whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bot url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("bot url scheme must be http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("bot url host is required")
	}
	if parsed.User != nil {
		return "", errors.New("bot url must not contain user info")
	}
	return raw, nil
}
