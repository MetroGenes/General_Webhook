package logger

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	botTokenPathRe = regexp.MustCompile(`/bot[^/\s"]+`)
	urlRe          = regexp.MustCompile(`https?://[^\s"]+`)
)

// RedactSensitive masks common token-bearing URL forms before values enter logs or audit DB.
func RedactSensitive(s string) string {
	if s == "" {
		return s
	}

	s = botTokenPathRe.ReplaceAllString(s, "/bot****")
	return urlRe.ReplaceAllStringFunc(s, redactURL)
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
