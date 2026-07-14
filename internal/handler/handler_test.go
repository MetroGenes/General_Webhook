package handler

import (
	"context"
	"io"
	"net/http"
	"testing"

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
	var idem, eid, attempt string
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		idem = req.Header.Get("Idempotency-Key")
		eid = req.Header.Get("X-Webhook-Event-ID")
		attempt = req.Header.Get("X-Webhook-Attempt")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(http.NoBody), Header: make(http.Header)}, nil
	})}
	_, err := h.Handle(context.Background(), config.ActionConfig{
		Type: "http", URL: "https://example.test/h", Method: http.MethodPost, Body: `{}`,
	}, map[string]string{"event_id": "e-1", "attempt": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if idem != "e-1" || eid != "e-1" || attempt != "2" {
		t.Fatalf("idem=%q eid=%q attempt=%q", idem, eid, attempt)
	}
}
