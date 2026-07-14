package handler

import (
	"context"
	"encoding/json"
	"os"
	"regexp"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// Result 携带动作执行结果, 用于审计。
type Result struct {
	Target string // 目标 (channel / url / command)
	Detail string // 简要详情 (响应码 / 输出)
}

// Handler 执行一种动作类型。
type Handler interface {
	Type() string
	Handle(ctx context.Context, ac config.ActionConfig, vars map[string]string) (Result, error)
}

// Registry 按动作类型分发到对应 handler。
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry(hs ...Handler) *Registry {
	m := make(map[string]Handler, len(hs))
	for _, h := range hs {
		m[h.Type()] = h
	}
	return &Registry{handlers: m}
}

func (r *Registry) Get(typ string) (Handler, bool) {
	h, ok := r.handlers[typ]
	return h, ok
}

var varRe = regexp.MustCompile(`\$\{(\w+)\}`)

// interpolate 用 vars 替换字符串中的 ${var} 占位符。
func interpolate(s string, vars map[string]string) string {
	return interpolateWith(s, vars, func(v string) string { return v })
}

// interpolateJSONString 用 JSON 字符串转义后的 vars 替换占位符。
func interpolateJSONString(s string, vars map[string]string) string {
	return interpolateWith(s, vars, escapeJSONString)
}

func interpolateWith(s string, vars map[string]string, escape func(string) string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		key := varRe.FindStringSubmatch(m)[1]
		if v, ok := vars[key]; ok {
			return escape(v)
		}
		if v, ok := os.LookupEnv(key); ok {
			return escape(v)
		}
		return ""
	})
}

func escapeJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b[1 : len(b)-1])
}
