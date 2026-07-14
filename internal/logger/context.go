package logger

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// WithTrace 在 context 中绑定一个带 trace_id 的 logger,
// 后续 From(ctx) 取出的日志会自动携带该 ID, 串起一次请求全链路。
func WithTrace(ctx context.Context, traceID string) context.Context {
	l := slog.Default().With("trace_id", traceID)
	return context.WithValue(ctx, ctxKey{}, l)
}

// From 取出 context 中的 logger, 没有则返回默认 logger。
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// Redact 对敏感值做脱敏, 用于日志输出。
func Redact(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + "****" + s[len(s)-2:]
}
