package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Extract 用 gjson 路径从 JSON payload 抽取变量。
// 配置里的路径可写成 JSONPath 风格 "$.a.b", 前缀 "$." 会被自动去掉。
// 字段不存在时返回空字符串 (gjson 行为), 调用方需结合规则过滤处理。
func Extract(payload []byte, spec map[string]string) map[string]string {
	vars := make(map[string]string, len(spec))
	for name, path := range spec {
		res := gjson.GetBytes(payload, normalizePath(path))
		// gjson: 字段不存在 res.Exists()=false, 空字符串 res.Exists()=true
		// 统一处理: 不存在也返回 "", 但打标记便于调试
		if !res.Exists() {
			vars[name] = "" // 标记: 可在日志/规则里区分, 当前统一为空
		} else {
			vars[name] = res.String()
		}
	}
	return vars
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "$.")
	p = strings.TrimPrefix(p, "$")
	return p
}
