package logger

import (
	"strings"
	"testing"
)

func TestRedactSensitive_TokenURLs(t *testing.T) {
	input := `Post "https://api.telegram.org/bot123456:ABC-DEF/sendMessage?ok=1": failed https://oapi.dingtalk.com/robot/send?access_token=dt-token&foo=bar`

	got := RedactSensitive(input)
	if strings.Contains(got, "123456:ABC-DEF") || strings.Contains(got, "dt-token") {
		t.Fatalf("RedactSensitive leaked token: %s", got)
	}
	if !strings.Contains(got, "/bot****/sendMessage") {
		t.Fatalf("telegram bot token was not redacted: %s", got)
	}
	if !strings.Contains(got, "access_token=%2A%2A%2A%2A") {
		t.Fatalf("query token was not redacted: %s", got)
	}
}
