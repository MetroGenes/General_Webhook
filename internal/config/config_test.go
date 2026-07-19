package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_EnvExpansion(t *testing.T) {
	// 设置测试环境变量
	os.Setenv("TEST_SECRET", "test-secret-12345")
	os.Setenv("TEST_BOT_URL", "https://example.com/bot")
	defer os.Unsetenv("TEST_SECRET")
	defer os.Unsetenv("TEST_BOT_URL")

	// 创建临时配置文件
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "test.yaml")
	content := `
server:
  addr: ":8080"
log:
  level: "info"
store:
  path: "test.db"
queue:
  workers: 2
  buffer: 10
  max_retries: 3
sources:
  - name: test-source
    auth:
      type: token
      header: X-Token
      secret: ${TEST_SECRET}
    extract:
      message: $.message
    actions:
      - type: http
        url: ${TEST_BOT_URL}
        body: |
          {"text": "${message}"}
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 加载配置
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Auth secrets expand at load time; action templates stay unresolved so
	// durable snapshots never contain the credential value.
	if cfg.Sources[0].Auth.Secret != "test-secret-12345" {
		t.Errorf("auth.secret = %q, want %q", cfg.Sources[0].Auth.Secret, "test-secret-12345")
	}
	if cfg.Sources[0].Actions[0].URL != "${TEST_BOT_URL}" {
		t.Errorf("action.url = %q, want preserved template", cfg.Sources[0].Actions[0].URL)
	}

	// 验证运行时插值字段未被展开（YAML 多行字符串末尾会保留换行符）
	expectedBody := "{\"text\": \"${message}\"}\n"
	if cfg.Sources[0].Actions[0].Body != expectedBody {
		t.Errorf("action.body = %q, want %q", cfg.Sources[0].Actions[0].Body, expectedBody)
	}
}

func TestSnapshot_DoesNotPersistExplicitSecret(t *testing.T) {
	t.Setenv("BOT_SECRET_URL", "https://example.com/hook/super-secret")
	src := &Source{Name: "test", Actions: []ActionConfig{{Type: "http", URL: "${secret:BOT_SECRET_URL}"}}}
	raw, err := SnapshotSource(src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "super-secret") {
		t.Fatalf("snapshot contains credential: %s", raw)
	}
	if !strings.Contains(raw, "${secret:BOT_SECRET_URL}") {
		t.Fatalf("snapshot lost secret reference: %s", raw)
	}
}

func TestScrubSnapshotSecrets(t *testing.T) {
	const secretURL = "https://example.com/hook/credential-value"
	t.Setenv("BOT_SECRET_URL", secretURL)
	cfg := &Config{Sources: []Source{{Name: "test", Actions: []ActionConfig{{
		Type: "http", URL: "${secret:BOT_SECRET_URL}", Headers: map[string]string{"X-Key": "${secret:BOT_SECRET_URL}"},
	}}}}}
	legacy, err := SnapshotSource(&Source{Name: "test", Actions: []ActionConfig{{Type: "http", URL: secretURL}}})
	if err != nil {
		t.Fatal(err)
	}
	clean, err := ScrubSnapshotSecrets(legacy, ActionSecretReplacements(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(clean, "credential-value") || !strings.Contains(clean, "${secret:BOT_SECRET_URL}") {
		t.Fatalf("snapshot not scrubbed: %s", clean)
	}
	parsed, err := ParseSourceSnapshot(clean)
	if err != nil || parsed.Actions[0].URL != "${secret:BOT_SECRET_URL}" {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	if ConfigHash(cfg) == "" {
		t.Fatal("config hash should be populated")
	}
}

func TestLoad_RejectsUnsafeSignedHeaders(t *testing.T) {
	t.Setenv("HMAC_SECRET", "0123456789abcdef")
	path := filepath.Join(t.TempDir(), "unsafe.yaml")
	raw := `
sources:
  - name: test
    auth:
      type: hmac
      secret: ${HMAC_SECRET}
      signed_headers: [timestamp]
    actions:
      - type: exec
        command: /bin/true
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unsafe signed_headers should be rejected")
	}
}

func TestLoad_RuntimeVarsPreserved(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "test.yaml")
	content := `
server:
  addr: ":8080"
sources:
  - name: test
    auth:
      type: none
    extract:
      job: $.job
    actions:
      - type: exec
        command: /bin/echo
        args: ["${job}"]
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// 验证 args 中的 ${job} 未被展开（运行时变量）
	if len(cfg.Sources[0].Actions[0].Args) != 1 || cfg.Sources[0].Actions[0].Args[0] != "${job}" {
		t.Errorf("args = %v, want [${job}]", cfg.Sources[0].Actions[0].Args)
	}
}

func TestLoad_RejectsUnknownFields(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.yaml")
	content := `
server:
  adrr: ":9999"
sources:
  - name: t
    auth:
      type: none
    actions:
      - type: exec
        command: /bin/true
`
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(tmp); err == nil {
		t.Fatal("want error for unknown field server.adrr")
	}
}

func TestLoad_PreservesUnsetEventVarInURL(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "url.yaml")
	content := `
sources:
  - name: t
    auth:
      type: none
    actions:
      - type: http
        url: https://${tenant}.example/hook
`
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources[0].Actions[0].URL != "https://${tenant}.example/hook" {
		t.Fatalf("url = %q", cfg.Sources[0].Actions[0].URL)
	}
}
