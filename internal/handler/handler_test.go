package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestInterpolate(t *testing.T) {
	vars := map[string]string{
		"message": "hello",
		"job":     "deploy-api",
		"status":  "success",
	}

	tests := []struct {
		input string
		want  string
	}{
		{"plain text", "plain text"},
		{"${message}", "hello"},
		{"msg: ${message}", "msg: hello"},
		{"${job} - ${status}", "deploy-api - success"},
		{"${unknown}", ""},
		{"${message} ${job} ${unknown}", "hello deploy-api "},
		{"", ""},
	}

	for _, tt := range tests {
		got := interpolate(tt.input, vars)
		if got != tt.want {
			t.Errorf("interpolate(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestInterpolate_EnvFallback(t *testing.T) {
	t.Setenv("WEBHOOK_ENV_VALUE", "from-env")

	got := interpolate("${WEBHOOK_ENV_VALUE}-${missing}", nil)
	if got != "from-env-" {
		t.Fatalf("interpolate env fallback = %q, want %q", got, "from-env-")
	}
}

func TestInterpolate_ExplicitNamespaces(t *testing.T) {
	t.Setenv("BOT_TOKEN", "from-secret-env")
	vars := map[string]string{"message": "from-event", "BOT_TOKEN": "attacker-value"}
	got := interpolate("${event:message}|${secret:BOT_TOKEN}", vars)
	if got != "from-event|from-secret-env" {
		t.Fatalf("explicit namespaces = %q", got)
	}
}

func TestHTTPHandle_InterpolatesHeaderKeyAndValue(t *testing.T) {
	h := NewHTTP()
	var got string
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Get("X-Hook")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(http.NoBody),
			Header:     make(http.Header),
		}, nil
	})}

	_, err := h.Handle(context.Background(), config.ActionConfig{
		Type:   "http",
		URL:    "https://example.test/webhook",
		Method: http.MethodPost,
		Headers: map[string]string{
			"X-${header_name}": "${message}",
			"${missing}":       "ignored",
		},
	}, map[string]string{
		"header_name": "Hook",
		"message":     "hello",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got != "hello" {
		t.Fatalf("X-Hook = %q, want %q", got, "hello")
	}
}

func TestHTTPHandle_EscapesJSONBodyAndReadsEnvFallback(t *testing.T) {
	t.Setenv("TG_CHAT_ID", "12345")

	h := NewHTTP()
	var gotBody string
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll body: %v", err)
		}
		gotBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(http.NoBody),
			Header:     make(http.Header),
		}, nil
	})}

	_, err := h.Handle(context.Background(), config.ActionConfig{
		Type:   "http",
		URL:    "https://example.test/webhook",
		Method: http.MethodPost,
		Body:   `{"chat_id":"${TG_CHAT_ID}","text":"${message}"}`,
	}, map[string]string{
		"message": "line \"one\"\nnext",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	want := `{"chat_id":"12345","text":"line \"one\"\nnext"}`
	if gotBody != want {
		t.Fatalf("body = %q, want %q", gotBody, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEscapeJSONString_ControlChar(t *testing.T) {
	got := escapeJSONString("\a")
	if got != `\u0007` {
		t.Fatalf("got %q, want \\u0007", got)
	}
}

func TestHTTPHandle_SetsIdempotencyHeaders(t *testing.T) {
	h := NewHTTP()
	var method, idem, eid, attempt string
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method = req.Method
		idem = req.Header.Get("Idempotency-Key")
		eid = req.Header.Get("X-Webhook-Event-ID")
		attempt = req.Header.Get("X-Webhook-Attempt")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(http.NoBody), Header: make(http.Header)}, nil
	})}
	_, err := h.Handle(context.Background(), config.ActionConfig{
		Type: "http", URL: "https://example.test/h", Method: "post", Body: `{}`,
	}, map[string]string{"event_id": "e-1", "attempt": "2", "action_index": "3"})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || idem != "e-1:3" || eid != "e-1" || attempt != "2" {
		t.Fatalf("method=%q idem=%q eid=%q attempt=%q", method, idem, eid, attempt)
	}
}

func TestValidateURLDestination(t *testing.T) {
	if err := ValidateURLDestination("https://api.example.com/x", nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateURLDestination("http://api.example.com/x", nil); err == nil {
		t.Fatal("http without allowlist should fail")
	}
	if err := ValidateURLDestination("https://127.0.0.1/x", nil); err == nil {
		t.Fatal("loopback should fail")
	}
	if err := ValidateURLDestination("http://internal:8080/h", []string{"internal:8080"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateURLDestination("https://evil.example/h", []string{"good.example"}); err == nil {
		t.Fatal("want allowlist miss")
	}
	if err := ValidateURLDestination("https://good.example.evil/h", []string{"https://good.example"}); err == nil {
		t.Fatal("URL-prefix lookalike must not match")
	}
	if err := ValidateURLDestination("https://10.0.0.2/h", []string{"10.0.0.2"}); err == nil {
		t.Fatal("private destination requires explicit opt-in")
	}
	if err := ValidateURLDestinationWithPolicy("https://10.0.0.2/h", []string{"10.0.0.2"}, true); err != nil {
		t.Fatalf("explicit private destination: %v", err)
	}
}

func TestHTTPClient_BlocksCrossHostRedirect(t *testing.T) {
	h := NewHTTP()
	initial, _ := url.Parse("https://good.example/hook")
	client := h.clientFor(config.ActionConfig{URLAllowlist: []string{"good.example", "evil.example"}}, initial)
	req := &http.Request{URL: mustURL(t, "https://evil.example/hook")}
	via := []*http.Request{{URL: initial}}
	if err := client.CheckRedirect(req, via); err == nil {
		t.Fatal("cross-host redirect should be blocked")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestHTTPHandle_NonRetryable4xx(t *testing.T) {
	h := NewHTTP()
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(http.NoBody),
			Header:     make(http.Header),
		}, nil
	})}
	_, err := h.Handle(context.Background(), config.ActionConfig{
		Type: "http", URL: "https://example.test/h", Method: http.MethodPost,
	}, nil)
	if !IsNonRetryable(err) {
		t.Fatalf("want non-retryable, got %v", err)
	}
}

func TestHTTPHandle_AuditDataOmitsCredentialURL(t *testing.T) {
	h := NewHTTP()
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	const credential = "path-secret-123"
	res, err := h.Handle(context.Background(), config.ActionConfig{
		Type: "http", URL: "https://hooks.example.test/webhook/" + credential + "?access_token=query-secret",
	}, nil)
	if err == nil {
		t.Fatal("want request error")
	}
	if res.Target != "https://hooks.example.test" {
		t.Fatalf("audit target=%q", res.Target)
	}
	if strings.Contains(res.Target, credential) || strings.Contains(err.Error(), credential) || strings.Contains(err.Error(), "query-secret") {
		t.Fatalf("audit data leaked credential URL: target=%q err=%v", res.Target, err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if d := ParseRetryAfter("30", now); d != 30*time.Second {
		t.Fatalf("got %s", d)
	}
	if d := ParseRetryAfter("120", now); d != 120*time.Second {
		t.Fatalf("got %s", d)
	}
}
