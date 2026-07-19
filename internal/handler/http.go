package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// HTTP 是通用 HTTP 请求 action (合并原 notify + n8n)。
// 支持自定义 URL/Method/Headers/Body, 所有字段都可 ${var} 插值。
type HTTP struct {
	client *http.Client
	now    func() time.Time
}

func NewHTTP() *HTTP {
	return &HTTP{
		client: &http.Client{Timeout: 15 * time.Second},
		now:    time.Now,
	}
}

func (h *HTTP) Type() string { return "http" }

func (h *HTTP) Handle(ctx context.Context, ac config.ActionConfig, vars map[string]string) (Result, error) {
	secretFields := []string{ac.URL, ac.Body}
	for k, v := range ac.Headers {
		secretFields = append(secretFields, k, v)
	}
	if missing := missingExplicitSecret(secretFields...); missing != "" {
		return Result{}, fmt.Errorf("%w: required action secret %s is not set", ErrNonRetryable, missing)
	}
	rawURL := interpolate(ac.URL, vars)
	auditTarget := auditURLTarget(rawURL)
	if err := ValidateURLDestinationWithPolicy(rawURL, ac.URLAllowlist, ac.AllowPrivate); err != nil {
		return Result{Target: auditTarget}, fmt.Errorf("%w: %v", ErrNonRetryable, err)
	}

	method := ac.Method
	if method == "" {
		method = http.MethodPost
	} else {
		method = strings.ToUpper(method)
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
			return Result{Target: auditTarget}, fmt.Errorf("%w: http body is not valid JSON after interpolation", ErrNonRetryable)
		}
	} else {
		body = interpolate(ac.Body, vars)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader([]byte(body)))
	if err != nil {
		return Result{Target: auditTarget}, err
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
		key := vars["event_id"]
		if vars["action_index"] != "" {
			key += ":" + vars["action_index"]
		}
		req.Header.Set("Idempotency-Key", key)
	}
	if req.Header.Get("X-Webhook-Event-ID") == "" && vars["event_id"] != "" {
		req.Header.Set("X-Webhook-Event-ID", vars["event_id"])
	}
	if req.Header.Get("X-Webhook-Attempt") == "" && vars["attempt"] != "" {
		req.Header.Set("X-Webhook-Attempt", vars["attempt"])
	}

	client := h.clientFor(ac, req.URL)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if urlErr, ok := err.(*url.Error); ok && urlErr.Err != nil {
			err = urlErr.Err
		}
		return Result{Target: auditTarget}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Result{Target: auditTarget, Detail: fmt.Sprintf("status=%d", resp.StatusCode)}, nil
	}

	he := &HTTPError{
		Status:     resp.StatusCode,
		Body:       string(respBody),
		RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), h.now()),
	}
	return Result{Target: auditTarget}, he
}

// auditURLTarget deliberately excludes path, query, and fragment because many
// webhook providers place bearer credentials in URL path segments.
func auditURLTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "invalid-url"
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host
}

func (h *HTTP) clientFor(ac config.ActionConfig, initial *url.URL) *http.Client {
	client := *h.client
	allowPrivate := ac.AllowPrivate && len(ac.URLAllowlist) > 0
	if client.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = secureDialContext(allowPrivate)
		client.Transport = transport
	} else if base, ok := client.Transport.(*http.Transport); ok {
		transport := base.Clone()
		transport.Proxy = nil
		transport.DialContext = secureDialContext(allowPrivate)
		client.Transport = transport
	}
	previousCheck := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("redirect limit exceeded")
		}
		if err := ValidateURLDestinationWithPolicy(req.URL.String(), ac.URLAllowlist, ac.AllowPrivate); err != nil {
			return fmt.Errorf("redirect destination: %w", err)
		}
		previous := initial
		if len(via) > 0 {
			previous = via[len(via)-1].URL
		}
		if !sameAuthority(previous, req.URL) {
			return fmt.Errorf("cross-host redirect blocked")
		}
		if previousCheck != nil {
			return previousCheck(req, via)
		}
		return nil
	}
	return &client
}

func sameAuthority(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(normalizeHostname(a.Hostname()), normalizeHostname(b.Hostname())) && effectivePort(a) == effectivePort(b)
}

func secureDialContext(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address: %w", err)
		}
		if isBlockedHostname(host) {
			return nil, fmt.Errorf("destination host blocked: %s", host)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve destination: %w", err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("destination resolved to no addresses")
		}
		for _, resolved := range ips {
			if err := validateDestinationIP(resolved.IP, allowPrivate); err != nil {
				return nil, err
			}
		}
		var lastErr error
		for _, resolved := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("dial destination: %w", lastErr)
	}
}
