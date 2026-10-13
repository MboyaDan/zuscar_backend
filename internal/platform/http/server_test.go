package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestRouter(db Pinger, logs io.Writer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	l := slog.New(slog.NewJSONHandler(logs, nil))
	r, v1 := NewRouter(Deps{Logger: l, DB: db, MaxRequestBytes: 64})
	v1.GET("/boom", func(*gin.Context) { panic("kaboom secret-value") })
	v1.POST("/echo", func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		if err != nil {
			WriteError(c, nethttp.StatusRequestEntityTooLarge, "request_too_large", "too large")
			return
		}
		c.String(200, string(b))
	})
	return r
}

func do(r *gin.Engine, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) ErrorDetail {
	t.Helper()
	var b ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not an error body: %s", w.Body.String())
	}
	return b.Error
}

func TestHealthzAlwaysOK(t *testing.T) {
	w := do(newTestRouter(fakeDB{errors.New("down")}, io.Discard), "GET", "/healthz", "", nil)
	if w.Code != 200 {
		t.Fatalf("healthz = %d", w.Code)
	}
}

func TestReadyzReflectsDatabase(t *testing.T) {
	if w := do(newTestRouter(fakeDB{}, io.Discard), "GET", "/readyz", "", nil); w.Code != 200 {
		t.Fatalf("ready = %d", w.Code)
	}
	w := do(newTestRouter(fakeDB{errors.New("down")}, io.Discard), "GET", "/readyz", "", nil)
	if w.Code != 503 || decodeErr(t, w).Code != "not_ready" {
		t.Fatalf("not ready = %d %s", w.Code, w.Body.String())
	}
	if w := do(newTestRouter(nil, io.Discard), "GET", "/readyz", "", nil); w.Code != 503 {
		t.Fatalf("nil db = %d", w.Code)
	}
}

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	r := newTestRouter(fakeDB{}, io.Discard)
	w := do(r, "GET", "/healthz", "", nil)
	if len(w.Header().Get(RequestIDHeader)) < 16 {
		t.Fatal("expected generated request id")
	}
	w = do(r, "GET", "/healthz", "", map[string]string{RequestIDHeader: "trace-12345678"})
	if w.Header().Get(RequestIDHeader) != "trace-12345678" {
		t.Fatal("valid inbound id should be kept")
	}
	w = do(r, "GET", "/healthz", "", map[string]string{RequestIDHeader: "bad id\nx"})
	if strings.Contains(w.Header().Get(RequestIDHeader), " ") {
		t.Fatal("malformed id must be replaced")
	}
}

func TestNotFoundUsesStandardBody(t *testing.T) {
	w := do(newTestRouter(fakeDB{}, io.Discard), "GET", "/api/v1/nothing", "", map[string]string{RequestIDHeader: "trace-12345678"})
	e := decodeErr(t, w)
	if w.Code != 404 || e.Code != "not_found" || e.RequestID != "trace-12345678" {
		t.Fatalf("got %d %+v", w.Code, e)
	}
}

func TestMethodNotAllowedUsesStandardBody(t *testing.T) {
	w := do(newTestRouter(fakeDB{}, io.Discard), "POST", "/healthz", "", nil)
	if w.Code != 405 || decodeErr(t, w).Code != "method_not_allowed" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestPanicRecoveredWithoutLeakingToClient(t *testing.T) {
	w := do(newTestRouter(fakeDB{}, io.Discard), "GET", "/api/v1/boom", "", nil)
	if w.Code != 500 || decodeErr(t, w).Code != "internal_error" || strings.Contains(w.Body.String(), "secret-value") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestBodyLimitEnforced(t *testing.T) {
	r := newTestRouter(fakeDB{}, io.Discard)
	if w := do(r, "POST", "/api/v1/echo", strings.Repeat("x", 65), nil); w.Code != 413 {
		t.Fatalf("oversize = %d", w.Code)
	}
	if w := do(r, "POST", "/api/v1/echo", "ok", nil); w.Code != 200 {
		t.Fatalf("small = %d", w.Code)
	}
}

func TestAccessLogHasRequestIDAndNoQuery(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRouter(fakeDB{}, &buf)
	do(r, "GET", "/healthz?token=supersecret", "", map[string]string{RequestIDHeader: "trace-12345678", "Authorization": "Bearer zzz"})
	out := buf.String()
	if !strings.Contains(out, "trace-12345678") || strings.Contains(out, "supersecret") || strings.Contains(out, "Bearer") {
		t.Fatalf("log line wrong: %s", out)
	}
}

func TestAPIHealthReachableUnderV1(t *testing.T) {
	w := do(newTestRouter(fakeDB{errors.New("down")}, io.Discard), "GET", "/api/v1/health", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}
