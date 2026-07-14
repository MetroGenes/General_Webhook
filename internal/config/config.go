package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 webhook 网关的完整配置。
type Config struct {
	Server  ServerConfig `yaml:"server"`
	Log     LogConfig    `yaml:"log"`
	Store   StoreConfig  `yaml:"store"`
	Queue   QueueConfig  `yaml:"queue"`
	Sources []Source     `yaml:"sources"`
}

type ServerConfig struct {
	Addr           string   `yaml:"addr"`
	TrustedProxies []string `yaml:"trusted_proxies"`
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
	Workers    int `yaml:"workers"`
	Buffer     int `yaml:"buffer"`
	MaxRetries int `yaml:"max_retries"`
}

// Source 是一个事件源, 对应 /webhook/{name}。
type Source struct {
	Name    string            `yaml:"name"`
	Auth    AuthConfig        `yaml:"auth"`
	Extract map[string]string `yaml:"extract"` // 变量名 -> JSONPath
	Rules   []Rule            `yaml:"rules"`
	Actions []ActionConfig    `yaml:"actions"`
}

type AuthConfig struct {
	Type   string `yaml:"type"` // hmac | token | none
	Header string `yaml:"header"`
	Secret string `yaml:"secret"`
}

type Rule struct {
	Var   string `yaml:"var"`
	Regex string `yaml:"regex"`
}

// ActionConfig 是动作层配置, 字段按 Type 取并集。
type ActionConfig struct {
	Type string `yaml:"type"` // http | exec

	// http
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`

	// exec
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Timeout Duration `yaml:"timeout"`
}

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

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	for i := range c.Sources {
		c.Sources[i].Auth.Secret = expandConfigEnv(c.Sources[i].Auth.Secret, true)
		for j := range c.Sources[i].Actions {
			c.Sources[i].Actions[j].URL = expandConfigEnv(c.Sources[i].Actions[j].URL, false)
			c.Sources[i].Actions[j].Command = expandConfigEnv(c.Sources[i].Actions[j].Command, false)
		}
	}

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
		v := true
		c.Store.RetainPayload = &v
	}
	if c.Queue.Workers <= 0 {
		c.Queue.Workers = 4
	}
	if c.Queue.Buffer <= 0 {
		c.Queue.Buffer = 256
	}
	if c.Queue.MaxRetries <= 0 {
		c.Queue.MaxRetries = 3
	}
}

func (c *Config) validate() error {
	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if net.ParseIP(cidr) == nil {
				return fmt.Errorf("server.trusted_proxies: invalid entry %q", cidr)
			}
		}
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

		if (s.Auth.Type == "hmac" || s.Auth.Type == "token") && s.Auth.Secret == "" {
			return fmt.Errorf("source %q: auth.type=%s requires non-empty secret", s.Name, s.Auth.Type)
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
			default:
				return fmt.Errorf("source %q: action[%d] unknown type %q (http/exec)", s.Name, i, a.Type)
			}
		}
	}
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
