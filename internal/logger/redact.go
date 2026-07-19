package logger

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	botTokenPathRe = regexp.MustCompile(`/bot[^/\s"]+`)
	urlRe          = regexp.MustCompile(`https?://[^\s"]+`)
	bearerRe       = regexp.MustCompile(`(?i)\b(authorization:\s*bearer\s+)(\S+)`)
	assignRe       = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|secret|password|token)\b\s*[=:]\s*([^\s"'\\,]+)`)
	jsonSecretRe   = regexp.MustCompile(`(?i)"(api[_-]?key|access[_-]?token|refresh[_-]?token|secret|password|token)"\s*:\s*"([^"]*)"`)
)

// RedactSensitive masks common token-bearing URL forms and key/value secrets
// before values enter logs or audit DB.
func RedactSensitive(s string) string {
	if s == "" {
		return s
	}

	s = botTokenPathRe.ReplaceAllString(s, "/bot****")
	s = urlRe.ReplaceAllStringFunc(s, redactURL)
	s = bearerRe.ReplaceAllString(s, "${1}****")
	s = assignRe.ReplaceAllString(s, "${1}=****")
	s = jsonSecretRe.ReplaceAllString(s, `"${1}":"****"`)
	return s
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for key := range q {
		if isSensitiveKey(key) {
			q.Set(key, "****")
		}
	}
	u.RawQuery = q.Encode()
	u.Path = botTokenPathRe.ReplaceAllString(u.Path, "/bot****")
	return u.String()
}

func isSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "key") ||
		strings.Contains(key, "sign") ||
		strings.Contains(key, "password")
}
