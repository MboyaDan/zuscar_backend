package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestSensitiveAttributesRedacted(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, slog.LevelInfo)
	l.Info("login", "password", "hunter2", "session_token", "abc123", "Authorization", "Bearer x", "request_id", "r-1", "pin", "1234")
	out := buf.String()
	for _, leak := range []string{"hunter2", "abc123", "Bearer x", "1234"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaked %q: %s", leak, out)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["request_id"] != "r-1" || m["password"] != Redacted {
		t.Fatalf("unexpected log record: %v", m)
	}
}
