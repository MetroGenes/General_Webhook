package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func loadHeartbeatYAML(t *testing.T, content string) (*Config, error) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "hb.yaml")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(tmp)
}

func TestLoad_HeartbeatDefaultsAndEnvExpansion(t *testing.T) {
	os.Setenv("HB_TEST_URL", "https://hc-ping.com/uuid123/check")
	os.Setenv("HB_TEST_TOKEN", "hb-token-123456")
	os.Setenv("HB_TEST_HOST", "node-tokyo")
	defer os.Unsetenv("HB_TEST_URL")
	defer os.Unsetenv("HB_TEST_TOKEN")
	defer os.Unsetenv("HB_TEST_HOST")

	cfg, err := loadHeartbeatYAML(t, `
heartbeat:
  host: ${HB_TEST_HOST}
  targets:
    - name: healthchecks
      url: ${HB_TEST_URL}
      token: ${HB_TEST_TOKEN}
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Heartbeat.Interval.Duration != 60*time.Second {
		t.Errorf("interval = %s, want 60s", cfg.Heartbeat.Interval)
	}
	if cfg.Heartbeat.Timeout.Duration != 10*time.Second {
		t.Errorf("timeout = %s, want 10s", cfg.Heartbeat.Timeout)
	}
	if cfg.Heartbeat.Host != "node-tokyo" {
		t.Errorf("host = %q", cfg.Heartbeat.Host)
	}
	if len(cfg.Heartbeat.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(cfg.Heartbeat.Targets))
	}
	if cfg.Heartbeat.Targets[0].URL != "https://hc-ping.com/uuid123/check" {
		t.Errorf("url = %q", cfg.Heartbeat.Targets[0].URL)
	}
	if cfg.Heartbeat.Targets[0].Token != "hb-token-123456" {
		t.Errorf("token = %q", cfg.Heartbeat.Targets[0].Token)
	}
	if cfg.Heartbeat.Targets[0].Mode != "kuma" {
		t.Errorf("mode = %q, want default kuma", cfg.Heartbeat.Targets[0].Mode)
	}
}

func TestLoad_HeartbeatDisabledWhenURLEnvUnset(t *testing.T) {
	// No env set: the only target is dropped and the module stays off.
	cfg, err := loadHeartbeatYAML(t, `
heartbeat:
  targets:
    - name: healthchecks
      url: ${HB_UNSET_URL}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Heartbeat.Targets) != 0 {
		t.Errorf("targets = %d, want 0 (fail closed)", len(cfg.Heartbeat.Targets))
	}
}

func TestLoad_HeartbeatRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "interval too small",
			yaml:    "heartbeat:\n  interval: 1s\n",
			wantErr: "heartbeat.interval",
		},
		{
			name:    "timeout exceeds interval",
			yaml:    "heartbeat:\n  interval: 10s\n  timeout: 20s\n",
			wantErr: "heartbeat.timeout must not exceed",
		},
		{
			name:    "http without allow_http",
			yaml:    "heartbeat:\n  targets:\n    - name: kuma\n      url: http://192.168.1.5:3001/api/push/abc\n",
			wantErr: "must be https",
		},
		{
			name:    "userinfo in url",
			yaml:    "heartbeat:\n  targets:\n    - name: kuma\n      url: https://user:pass@example.com/push\n",
			wantErr: "userinfo",
		},
		{
			name:    "bad mode",
			yaml:    "heartbeat:\n  targets:\n    - name: kuma\n      url: https://example.com/push\n      mode: smoke\n",
			wantErr: "unknown mode",
		},
		{
			name:    "duplicate name",
			yaml:    "heartbeat:\n  targets:\n    - name: a\n      url: https://example.com/1\n    - name: a\n      url: https://example.com/2\n",
			wantErr: "duplicate target name",
		},
		{
			name:    "invalid name",
			yaml:    "heartbeat:\n  targets:\n    - name: bad name\n      url: https://example.com/push\n",
			wantErr: "invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadHeartbeatYAML(t, tc.yaml)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoad_HeartbeatAllowsHTTPWithAllowHTTP(t *testing.T) {
	cfg, err := loadHeartbeatYAML(t, `
heartbeat:
  targets:
    - name: kuma
      url: http://192.168.1.5:3001/api/push/abc
      allow_http: true
      mode: json
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Heartbeat.Targets) != 1 || cfg.Heartbeat.Targets[0].Mode != "json" {
		t.Fatalf("targets = %+v", cfg.Heartbeat.Targets)
	}
}
