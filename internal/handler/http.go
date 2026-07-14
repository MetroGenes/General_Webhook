package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// HTTP 是通用 HTTP 请求 action (合并原 notify + n8n)。
// 支持自定义 URL/Method/Headers/Body, 所有字段都可 ${var} 插值。
type HTTP struct {
	client *http.Client
}

func NewHTTP() *HTTP {
	return &HTTP{client: &http.Client{Timeout: 15 * time.Second}}
}

func (h *HTTP) Type() string { return "http" }

func (h *HTTP) Handle(ctx context.Context, ac config.ActionConfig, vars map[string]string) (Result, error) {
	url := interpolate(ac.URL, vars)
	method := ac.Method
	if method == "" {
		method = http.MethodPost
	}

	ctHint := ""
	for k, v := range ac.Headers {
		if strings.EqualFold(interpolate(k, vars), "Content-Type") {
			ctHint = interpolate(v, vars)
			break
		}
	}
	isJSON := ctHint == "" || strings.Contains(strings.ToLower(ctHint), "json")

	var body string
	if isJSON {
		body = interpolateJSONString(ac.Body, vars)
		if body != "" && !json.Valid([]byte(body)) {
			return Result{Target: url}, fmt.Errorf("http body is not valid JSON after interpolation")
		}
	} else {
		body = interpolate(ac.Body, vars)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return Result{Target: url}, err
	}

	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	for k, v := range ac.Headers {
		key := interpolate(k, vars)
		if key == "" {
			continue
		}
		req.Header.Set(key, interpolate(v, vars))
	}

	if req.Header.Get("Idempotency-Key") == "" && vars["event_id"] != "" {
		req.Header.Set("Idempotency-Key", vars["event_id"])
	}
	if req.Header.Get("X-Webhook-Event-ID") == "" && vars["event_id"] != "" {
		req.Header.Set("X-Webhook-Event-ID", vars["event_id"])
	}
	if req.Header.Get("X-Webhook-Attempt") == "" && vars["attempt"] != "" {
		req.Header.Set("X-Webhook-Attempt", vars["attempt"])
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return Result{Target: url}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return Result{Target: url}, fmt.Errorf("http status %d: %s", resp.StatusCode, respBody)
	}

	return Result{Target: url, Detail: fmt.Sprintf("status=%d", resp.StatusCode)}, nil
}
