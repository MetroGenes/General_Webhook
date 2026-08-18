package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 webhook 网关的完整配置。
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Admin     AdminConfig     `yaml:"admin"`
	Log       LogConfig       `yaml:"log"`
	Store     StoreConfig     `yaml:"store"`
	Queue     QueueConfig     `yaml:"queue"`
	Heartbeat HeartbeatConfig `yaml:"heartbeat"`
	Sources   []Source        `yaml:"sources"`
}

// HeartbeatConfig 配置进程外死链（deadman）心跳推送。
// 标准部署不发布宿主端口、也没有公网反向代理，外部探针无法入站访问
// /healthz 或 /readyz；心跳改为由进程自己按周期向外部监控端点主动
// POST（Healthchecks.io、Uptime Kuma push、Cloudflare Pages 等）。
// 不配置任何有效 target（URL 展开后为空）时整个模块不启动，fail closed。
type HeartbeatConfig struct {
	Interval Duration          `yaml:"interval"` // 心跳周期, 默认 60s, 最小 10s
	Timeout  Duration          `yaml:"timeout"`  // 每个 target 的 HTTP 超时, 默认 10s
	Host     string            `yaml:"host"`     // JSON 负载里的主机标识, 默认 general-webhook
	Targets  []HeartbeatTarget `yaml:"targets"`
}

// HeartbeatTarget 是一个外部监控端点。
// URL 通常携带监控端的一次性 push token（如 Healthchecks 的 /uuid/slug），
// 必须经环境变量 ${VAR} 提供，不要明文写进配置文件。
type HeartbeatTarget struct {
	Name      string `yaml:"name"`       // 日志标识, 必须是 [a-zA-Z0-9_-]{1,64}
	URL       string `yaml:"url"`        // 死链 push URL, 默认要求 https
	Token     string `yaml:"token"`      // 可选; 非空时作为 X-Heartbeat-Token 头发送
	Mode      string `yaml:"mode"`       // push(默认) | json
	AllowHTTP bool   `yaml:"allow_http"` // 显式允许 http:// URL (内部 Uptime Kuma 等)
}

// AdminConfig protects operational and replay endpoints. When Token is empty,
// those endpoints are not registered at all (fail closed).
type AdminConfig struct {
	Header string `yaml:"header"`
	Token  string `yaml:"token"`
}

type ServerConfig struct {
	Addr             string   `yaml:"addr"`
	TrustedProxies   []string `yaml:"trusted_proxies"`
	MaxInFlight      int      `yaml:"max_in_flight"`
	MaxInFlightBytes int      `yaml:"max_in_flight_bytes"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

type StoreConfig struct {
	Path          string   `yaml:"path"`
	Retention     Duration `yaml:"retention"`
	RetainPayload *bool    `yaml:"retain_payload"`
}

type QueueConfig struct {
	Workers       int      `yaml:"workers"`
	Buffer        int      `yaml:"buffer"`
	MaxRetries    int      `yaml:"max_retries"`
	RetryBase     Duration `yaml:"retry_base"`
	MaxRetryDelay Duration `yaml:"max_retry_delay"`
	MaxRetryAge   Duration `yaml:"max_retry_age"`
}

// Source 是一个事件源, 对应 /webhook/{name}。
type Source struct {
	Name      string            `yaml:"name"`
	Auth      AuthConfig        `yaml:"auth"`
	Extract   map[string]string `yaml:"extract"` // 变量名 -> JSONPath
	Rules     []Rule            `yaml:"rules"`
	Actions   []ActionConfig    `yaml:"actions"`
	LogPolicy *LogPolicyConfig  `yaml:"log_policy,omitempty"`
	Redaction *RedactionConfig  `yaml:"redaction,omitempty"`
}

// LogPolicyConfig is an optional egress allowlist evaluated after extract.
// mode is hard-coded to allowlist semantics (denylist is not offered).
type LogPolicyConfig struct {
	Mode     string   `yaml:"mode"`                // must be "allowlist"
	Field    string   `yaml:"field,omitempty"`     // extracted var to check; default "service"
	Services []string `yaml:"services"`            // allowed values (non-empty when set)
	OnReject string   `yaml:"on_reject,omitempty"` // must be "drop_and_count" when set
}

// RedactionConfig applies regex replacements to extracted vars before delivery.
type RedactionConfig struct {
	Patterns []string         `yaml:"patterns" json:"patterns"`
	Compiled []*regexp.Regexp `yaml:"-" json:"-"`
}

type AuthConfig struct {
	Type          string   `yaml:"type"` // hmac | token | none
	Header        string   `yaml:"header"`
	Secret        string   `yaml:"secret"`
	SignedHeaders []string `yaml:"signed_headers"` // e.g. [timestamp, delivery_id, body] for bound HMAC
}

type Rule struct {
	Var   string `yaml:"var"`
	Regex string `yaml:"regex"`
}

// ActionConfig 是动作层配置, 字段按 Type 取并集。
type ActionConfig struct {
	Type string `yaml:"type"` // http | exec

	// http
	URL          string            `yaml:"url"`
	Method       string            `yaml:"method"`
	Headers      map[string]string `yaml:"headers"`
	Body         string            `yaml:"body"`
	URLAllowlist []string          `yaml:"url_allowlist"` // host, host:port, CIDR, or URL prefix
	AllowPrivate bool              `yaml:"allow_private"` // explicit opt-in for resolved private IPs

	// exec
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Timeout Duration `yaml:"timeout"`
}

const (
	minSecretLen            = 16
	maxWorkers              = 64
	maxBuffer               = 10000
	maxRetriesBound         = 20
	maxActionTimeout        = 15 * time.Minute
	defaultMaxInFlight      = 16
	defaultMaxInFlightBytes = 32 << 20 // 32 MiB
	defaultMaxRetries       = 2        // retries after the initial attempt (3 total)
	defaultRetryBase        = time.Second
	defaultMaxRetryDelay    = 15 * time.Minute
	defaultMaxRetryAge      = 24 * time.Hour
)

// Duration 包装 time.Duration, 支持 YAML 字符串 ("300s")。
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Value == "" {
		return nil
	}
	v, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = v
	return nil
}

var (
	envBraceRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	envPlainRe = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	envPrefRe  = regexp.MustCompile(`\$\{ENV:([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// Load 读取配置文件并校验。
// 配置层 env 展开：已设置的 ${NAME} / $NAME / ${ENV:NAME} 会替换；未设置的 ${var} 保留给运行时。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	// Pre-seed values where zero has an explicit meaning. YAML max_retries: 0
	// therefore remains zero, while an omitted field receives the default.
	c := Config{Queue: QueueConfig{MaxRetries: defaultMaxRetries}}
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	for i := range c.Sources {
		c.Sources[i].Auth.Secret = expandConfigEnv(c.Sources[i].Auth.Secret, true)
	}
	c.Admin.Token = expandConfigEnv(c.Admin.Token, true)
	c.Heartbeat.Host = expandConfigEnv(c.Heartbeat.Host, true)
	for i := range c.Heartbeat.Targets {
		c.Heartbeat.Targets[i].URL = expandConfigEnv(c.Heartbeat.Targets[i].URL, true)
		c.Heartbeat.Targets[i].Token = expandConfigEnv(c.Heartbeat.Targets[i].Token, true)
	}
	// Drop targets whose URL env is unset: the whole module stays off until
	// the operator supplies at least one push endpoint (fail closed).
	filtered := c.Heartbeat.Targets[:0]
	for _, t := range c.Heartbeat.Targets {
		if strings.TrimSpace(t.URL) != "" {
			filtered = append(filtered, t)
		}
	}
	c.Heartbeat.Targets = filtered

	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// expandConfigEnv replaces env placeholders. If requireSet, missing env becomes empty (secrets).
// Otherwise missing ${name} is preserved for runtime event interpolation.
func expandConfigEnv(s string, requireSet bool) string {
	s = envPrefRe.ReplaceAllStringFunc(s, func(m string) string {
		key := envPrefRe.FindStringSubmatch(m)[1]
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if requireSet {
			return ""
		}
		return m
	})
	s = envBraceRe.ReplaceAllStringFunc(s, func(m string) string {
		key := envBraceRe.FindStringSubmatch(m)[1]
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if requireSet {
			return ""
		}
		return m // keep for runtime ${var}
	})
	s = envPlainRe.ReplaceAllStringFunc(s, func(m string) string {
		key := envPlainRe.FindStringSubmatch(m)[1]
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if requireSet {
			return ""
		}
		return m
	})
	return s
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.MaxInFlight <= 0 {
		c.Server.MaxInFlight = defaultMaxInFlight
	}
	if c.Server.MaxInFlightBytes <= 0 {
		c.Server.MaxInFlightBytes = defaultMaxInFlightBytes
	}
	if c.Admin.Header == "" {
		c.Admin.Header = "Authorization"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Store.Path == "" {
		c.Store.Path = "data/webhook.db"
	}
	if c.Store.Retention.Duration <= 0 {
		c.Store.Retention = Duration{30 * 24 * time.Hour}
	}
	if c.Store.RetainPayload == nil {
		v := false
		c.Store.RetainPayload = &v
	}
	if c.Queue.Workers <= 0 {
		c.Queue.Workers = 4
	}
	if c.Queue.Buffer <= 0 {
		c.Queue.Buffer = 256
	}
	if c.Queue.RetryBase.Duration <= 0 {
		c.Queue.RetryBase = Duration{defaultRetryBase}
	}
	if c.Queue.MaxRetryDelay.Duration <= 0 {
		c.Queue.MaxRetryDelay = Duration{defaultMaxRetryDelay}
	}
	if c.Queue.MaxRetryAge.Duration <= 0 {
		c.Queue.MaxRetryAge = Duration{defaultMaxRetryAge}
	}
	if c.Heartbeat.Interval.Duration <= 0 {
		c.Heartbeat.Interval = Duration{60 * time.Second}
	}
	if c.Heartbeat.Timeout.Duration <= 0 {
		c.Heartbeat.Timeout = Duration{10 * time.Second}
	}
	if c.Heartbeat.Host == "" {
		c.Heartbeat.Host = "general-webhook"
	}
	for i := range c.Heartbeat.Targets {
		if strings.TrimSpace(c.Heartbeat.Targets[i].Mode) == "" {
			c.Heartbeat.Targets[i].Mode = "push"
		}
	}
}

func (c *Config) validate() error {
	if c.Admin.Token != "" && len(c.Admin.Token) < minSecretLen {
		return fmt.Errorf("admin.token must be at least %d bytes", minSecretLen)
	}
	if strings.TrimSpace(c.Admin.Header) == "" {
		return fmt.Errorf("admin.header must not be empty")
	}
	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if net.ParseIP(cidr) == nil {
				return fmt.Errorf("server.trusted_proxies: invalid entry %q", cidr)
			}
		}
	}
	if c.Server.MaxInFlight < 1 || c.Server.MaxInFlight > 256 {
		return fmt.Errorf("server.max_in_flight must be 1..256")
	}
	if c.Server.MaxInFlightBytes < 1<<20 || c.Server.MaxInFlightBytes > 256<<20 {
		return fmt.Errorf("server.max_in_flight_bytes must be 1MiB..256MiB")
	}
	if c.Queue.Workers < 1 || c.Queue.Workers > maxWorkers {
		return fmt.Errorf("queue.workers must be 1..%d", maxWorkers)
	}
	if c.Queue.Buffer < 1 || c.Queue.Buffer > maxBuffer {
		return fmt.Errorf("queue.buffer must be 1..%d", maxBuffer)
	}
	if c.Queue.MaxRetries < 0 || c.Queue.MaxRetries > maxRetriesBound {
		return fmt.Errorf("queue.max_retries must be 0..%d", maxRetriesBound)
	}
	if c.Queue.RetryBase.Duration < 100*time.Millisecond || c.Queue.RetryBase.Duration > time.Hour {
		return fmt.Errorf("queue.retry_base must be 100ms..1h")
	}
	if c.Queue.MaxRetryDelay.Duration < c.Queue.RetryBase.Duration || c.Queue.MaxRetryDelay.Duration > 7*24*time.Hour {
		return fmt.Errorf("queue.max_retry_delay must be >= retry_base and <= 168h")
	}
	if c.Queue.MaxRetryAge.Duration < c.Queue.RetryBase.Duration || c.Queue.MaxRetryAge.Duration > 30*24*time.Hour {
		return fmt.Errorf("queue.max_retry_age must be >= retry_base and <= 720h")
	}
	if err := c.validateHeartbeat(); err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, s := range c.Sources {
		if s.Name == "" {
			return fmt.Errorf("source with empty name")
		}
		if !validSourceName(s.Name) {
			return fmt.Errorf("source name %q invalid (use [a-zA-Z0-9_-]{1,64})", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		seen[s.Name] = true

		if s.Auth.Type == "" {
			return fmt.Errorf("source %q: auth.type is required (hmac/token/none)", s.Name)
		}
		if s.Auth.Type != "hmac" && s.Auth.Type != "token" && s.Auth.Type != "none" {
			return fmt.Errorf("source %q: unknown auth.type %q", s.Name, s.Auth.Type)
		}

		if s.Auth.Type == "hmac" || s.Auth.Type == "token" {
			if s.Auth.Secret == "" {
				return fmt.Errorf("source %q: auth.type=%s requires non-empty secret", s.Name, s.Auth.Type)
			}
			if len(s.Auth.Secret) < minSecretLen {
				return fmt.Errorf("source %q: auth.secret must be at least %d bytes", s.Name, minSecretLen)
			}
		}
		signedSeen := map[string]bool{}
		for _, h := range s.Auth.SignedHeaders {
			h = strings.ToLower(strings.TrimSpace(h))
			switch strings.ToLower(strings.TrimSpace(h)) {
			case "timestamp", "delivery_id", "body":
			default:
				return fmt.Errorf("source %q: auth.signed_headers unknown entry %q", s.Name, h)
			}
			if signedSeen[h] {
				return fmt.Errorf("source %q: auth.signed_headers duplicate entry %q", s.Name, h)
			}
			signedSeen[h] = true
		}
		if len(s.Auth.SignedHeaders) > 0 {
			if s.Auth.Type != "hmac" {
				return fmt.Errorf("source %q: auth.signed_headers requires auth.type=hmac", s.Name)
			}
			want := []string{"timestamp", "delivery_id", "body"}
			if len(s.Auth.SignedHeaders) != len(want) {
				return fmt.Errorf("source %q: auth.signed_headers must be [timestamp, delivery_id, body]", s.Name)
			}
			for i, h := range want {
				if strings.ToLower(strings.TrimSpace(s.Auth.SignedHeaders[i])) != h {
					return fmt.Errorf("source %q: auth.signed_headers must be [timestamp, delivery_id, body]", s.Name)
				}
			}
		}

		for i, r := range s.Rules {
			if r.Var == "" {
				return fmt.Errorf("source %q: rule[%d] has empty var", s.Name, i)
			}
			if r.Regex == "" {
				return fmt.Errorf("source %q: rule[%d] has empty regex", s.Name, i)
			}
			if _, err := regexp.Compile(r.Regex); err != nil {
				return fmt.Errorf("source %q: rule[%d] invalid regex %q: %w", s.Name, i, r.Regex, err)
			}
		}

		if s.LogPolicy != nil {
			lp := s.LogPolicy
			if lp.Mode != "allowlist" {
				return fmt.Errorf("source %q: log_policy.mode must be allowlist", s.Name)
			}
			if len(lp.Services) == 0 {
				return fmt.Errorf("source %q: log_policy.services must be non-empty", s.Name)
			}
			for i, svc := range lp.Services {
				if strings.TrimSpace(svc) == "" {
					return fmt.Errorf("source %q: log_policy.services[%d] is empty", s.Name, i)
				}
			}
			if lp.OnReject != "" && lp.OnReject != "drop_and_count" {
				return fmt.Errorf("source %q: log_policy.on_reject must be drop_and_count", s.Name)
			}
			if lp.OnReject == "" {
				lp.OnReject = "drop_and_count"
			}
			if lp.Field == "" {
				lp.Field = "service"
			}
		}
		if s.Redaction != nil {
			if err := compileRedaction(s.Redaction); err != nil {
				return fmt.Errorf("source %q: %w", s.Name, err)
			}
		}

		if len(s.Actions) == 0 {
			return fmt.Errorf("source %q: no actions defined", s.Name)
		}

		for i, a := range s.Actions {
			if a.Type == "" {
				return fmt.Errorf("source %q: action[%d] with empty type", s.Name, i)
			}
			switch a.Type {
			case "http":
				if a.URL == "" {
					return fmt.Errorf("source %q: action[%d] type=http requires url", s.Name, i)
				}
				method := strings.ToUpper(a.Method)
				if method != "" && method != "GET" && method != "POST" && method != "PUT" && method != "PATCH" && method != "DELETE" {
					return fmt.Errorf("source %q: action[%d] invalid method %q", s.Name, i, a.Method)
				}
			case "exec":
				if a.Command == "" {
					return fmt.Errorf("source %q: action[%d] type=exec requires command", s.Name, i)
				}
				if a.Timeout.Duration < 0 {
					return fmt.Errorf("source %q: action[%d] timeout must be >= 0", s.Name, i)
				}
				if a.Timeout.Duration > maxActionTimeout {
					return fmt.Errorf("source %q: action[%d] timeout must be <= %s", s.Name, i, maxActionTimeout)
				}
			default:
				return fmt.Errorf("source %q: action[%d] unknown type %q (http/exec)", s.Name, i, a.Type)
			}
			for _, value := range actionStrings(a) {
				for _, match := range explicitSecretRefRe.FindAllStringSubmatch(value, -1) {
					if secret, ok := os.LookupEnv(match[1]); !ok || secret == "" {
						return fmt.Errorf("source %q: action[%d] requires environment secret %s", s.Name, i, match[1])
					}
				}
			}
		}
	}
	return nil
}

func (c *Config) validateHeartbeat() error {
	hb := &c.Heartbeat
	if hb.Interval.Duration < 10*time.Second || hb.Interval.Duration > time.Hour {
		return fmt.Errorf("heartbeat.interval must be 10s..1h")
	}
	if hb.Timeout.Duration < time.Second || hb.Timeout.Duration > 30*time.Second {
		return fmt.Errorf("heartbeat.timeout must be 1s..30s")
	}
	if hb.Timeout.Duration > hb.Interval.Duration {
		return fmt.Errorf("heartbeat.timeout must not exceed heartbeat.interval")
	}
	if strings.TrimSpace(hb.Host) == "" {
		return fmt.Errorf("heartbeat.host must not be empty")
	}
	for _, r := range hb.Host {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("heartbeat.host contains control characters")
		}
	}
	seen := map[string]bool{}
	for i, t := range hb.Targets {
		if t.Name == "" {
			return fmt.Errorf("heartbeat.targets[%d]: name is required", i)
		}
		if !validSourceName(t.Name) {
			return fmt.Errorf("heartbeat.targets[%d]: name %q invalid (use [a-zA-Z0-9_-]{1,64})", i, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("heartbeat.targets: duplicate target name %q", t.Name)
		}
		seen[t.Name] = true

		u, err := url.Parse(t.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("heartbeat.targets[%d]: invalid url %q", i, t.URL)
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "https" && !(scheme == "http" && t.AllowHTTP) {
			return fmt.Errorf("heartbeat.targets[%d]: url must be https (or http with allow_http: true)", i)
		}
		if u.User != nil {
			return fmt.Errorf("heartbeat.targets[%d]: url must not contain userinfo", i)
		}
		switch t.Mode {
		case "push", "json":
		default:
			return fmt.Errorf("heartbeat.targets[%d]: unknown mode %q (push/json)", i, t.Mode)
		}
	}
	return nil
}

func compileRedaction(r *RedactionConfig) error {
	if r == nil {
		return nil
	}
	compiled := make([]*regexp.Regexp, 0, len(r.Patterns))
	for i, p := range r.Patterns {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("redaction.patterns[%d] is empty", i)
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("redaction.patterns[%d]: %w", i, err)
		}
		compiled = append(compiled, re)
	}
	r.Compiled = compiled
	return nil
}

func validSourceName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// SourceByName 返回指定事件源, 不存在返回 nil。
func (c *Config) SourceByName(name string) *Source {
	for i := range c.Sources {
		if c.Sources[i].Name == name {
			return &c.Sources[i]
		}
	}
	return nil
}
