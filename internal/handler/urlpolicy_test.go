package handler

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestURLPathPolicy_RejectsAmbiguousPaths(t *testing.T) {
	for _, route := range []string{
		"%2e%2e/admin", ".%2e/admin", "%2E/admin", "..%2fadmin", "%2Fadmin",
		"%2e%2e%2fadmin", "..%5cadmin", `..\admin`, "%252e%252e/admin",
		"%252fadmin", "%255cadmin", "%25252e%25252e/admin", "%25%32%65%25%32%65/admin",
		"../admin", "one/../../admin", "%zz",
	} {
		t.Run(route, func(t *testing.T) {
			err := ValidateURLDestination("https://hooks.example.test/hooks/"+route, []string{"https://hooks.example.test/hooks"})
			if err == nil {
				t.Fatal("unsafe path passed URL allowlist")
			}
		})
	}
}

func TestURLPathPolicy_CanonicalAndLiteralPercentPaths(t *testing.T) {
	for _, route := range []string{
		"plain", "file%2ename", "a%20b", "%e4%b8%ad%e6%96%87", "100%25", "100%25off",
		"literal%25zz", "literal%252G", "one/../two", "./two", "one//two",
	} {
		t.Run(route, func(t *testing.T) {
			err := ValidateURLDestination("https://hooks.example.test/hooks/"+route, []string{"https://hooks.example.test/hoo%6bs"})
			if err != nil {
				t.Fatalf("valid canonical path rejected: %v", err)
			}
		})
	}
	if err := ValidateURLDestination("https://hooks.example.test/hooks-evil/path", []string{"https://hooks.example.test/hooks"}); err == nil {
		t.Fatal("path segment boundary not enforced")
	}
	if err := ValidateURLDestination("https://hooks.example.test/hooks/plain", []string{"https://hooks.example.test/hooks/%2e"}); err == nil {
		t.Fatal("ambiguous allowlist prefix accepted")
	}
}

func TestHTTPHandle_TraversalDoesNotReachTransport(t *testing.T) {
	h := NewHTTP()
	calls := 0
	h.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(http.NoBody), Header: make(http.Header)}, nil
	})
	_, err := h.Handle(context.Background(), config.ActionConfig{
		Type: "http", URL: "https://hooks.example.test/hooks/${event:route}",
		URLAllowlist: []string{"https://hooks.example.test/hooks"},
	}, map[string]string{"route": "%2e%2e/admin"})
	if !IsNonRetryable(err) || calls != 0 {
		t.Fatalf("traversal dispatched: calls=%d err=%v", calls, err)
	}
}

func TestHTTPHandle_RedirectCannotEscapePathPolicy(t *testing.T) {
	for _, location := range []string{
		"/hooks/%2e%2e/admin", "/hooks/..%2fadmin", "/hooks/%252e%252e/admin", "/admin",
	} {
		t.Run(location, func(t *testing.T) {
			h := NewHTTP()
			calls := 0
			h.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{
						StatusCode: http.StatusTemporaryRedirect, Body: io.NopCloser(http.NoBody),
						Header: http.Header{"Location": []string{location}},
					}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(http.NoBody), Header: make(http.Header)}, nil
			})
			_, err := h.Handle(context.Background(), config.ActionConfig{
				Type: "http", URL: "https://hooks.example.test/hooks/allowed",
				URLAllowlist: []string{"https://hooks.example.test/hooks"},
			}, nil)
			if err == nil || calls != 1 {
				t.Fatalf("redirect escaped allowed path: calls=%d err=%v", calls, err)
			}
		})
	}
}
