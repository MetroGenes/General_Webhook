package parser

import "testing"

func TestExtractAndNormalize(t *testing.T) {
	vars := Extract([]byte(`{"repository":{"name":"demo"},"empty":""}`), map[string]string{
		"repo": "$.repository.name", "empty": "$empty", "missing": "$.missing",
	})
	if vars["repo"] != "demo" || vars["empty"] != "" || vars["missing"] != "" {
		t.Fatalf("vars=%v", vars)
	}
	if normalizePath("  $.a.b ") != "a.b" {
		t.Fatal("path normalization failed")
	}
}
