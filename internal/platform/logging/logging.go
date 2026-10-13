// Package logging builds the structured JSON logger and redacts sensitive attributes
// so passwords, tokens and payment secrets never reach logs (brief 5.2, 13).
package logging

import (
	"io"
	"log/slog"
	"strings"
)

const Redacted = "[REDACTED]"

var sensitive = []string{"password", "passwd", "pin", "token", "secret", "authorization", "cookie", "csrf", "api_key", "apikey", "credential", "database_url"}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitive {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func New(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if isSensitive(a.Key) {
				return slog.String(a.Key, Redacted)
			}
			return a
		},
	})
	return slog.New(h)
}
