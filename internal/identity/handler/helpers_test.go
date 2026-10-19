package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	cathandler "zuscar/backend/internal/catalogue/handler"
	catservice "zuscar/backend/internal/catalogue/service"
	"zuscar/backend/internal/identity/handler"
	"zuscar/backend/internal/identity/password"
	"zuscar/backend/internal/identity/service"
	orderhandler "zuscar/backend/internal/orders/handler"
	orderservice "zuscar/backend/internal/orders/service"
	zhttp "zuscar/backend/internal/platform/http"
	"zuscar/backend/internal/platform/testdb"
	printhandler "zuscar/backend/internal/printing/handler"
	printservice "zuscar/backend/internal/printing/service"
	"zuscar/backend/internal/provision"
	saleshandler "zuscar/backend/internal/sales/handler"
	salesservice "zuscar/backend/internal/sales/service"
	sethandler "zuscar/backend/internal/settings/handler"
	setservice "zuscar/backend/internal/settings/service"
)

const (
	frontend = "http://localhost:3000"
	pw       = "Passw0rd-for-tests-1" // password of every test user
)

var (
	hashOnce sync.Once
	pwHash   string
)

func testHash(t testing.TB) string {
	hashOnce.Do(func() {
		h, err := password.Hash(pw)
		if err != nil {
			t.Fatal(err)
		}
		pwHash = h
	})
	return pwHash
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type testEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	clock  *fakeClock
	svc    *service.Service
	h      *handler.Handler
	router *gin.Engine
	cfg    service.Config
}

type envOpts struct {
	secure bool
	ipMax  int
}

func newEnv(t *testing.T, o ...envOpts) *testEnv {
	t.Helper()
	opt := envOpts{ipMax: 1000}
	if len(o) > 0 {
		opt = o[0]
		if opt.ipMax == 0 {
			opt.ipMax = 1000
		}
	}
	pool := testdb.New(t)
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	if err := provision.ApplyBaseData(ctx, tx, provision.BaseOptions{BusinessName: "Test", TaxRateBP: 1600, Timezone: "Africa/Nairobi", ReceiptStart: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	cfg := service.Config{IdleTTL: 2 * time.Hour, AbsoluteTTL: 12 * time.Hour, MaxAttempts: 5, LockoutWindow: 15 * time.Minute,
		IPMaxAttempts: opt.ipMax, ReauthWindow: 5 * time.Minute, TouchInterval: time.Minute}
	svc, err := service.New(pool, cfg, clock)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gin.SetMode(gin.TestMode)
	router, v1 := zhttp.NewRouter(zhttp.Deps{Logger: log, DB: pool, MaxRequestBytes: 1 << 20})
	h, err := handler.New(svc, frontend, opt.secure, log)
	if err != nil {
		t.Fatal(err)
	}
	h.Register(v1)
	h.RegisterAdmin(v1)
	sethandler.New(setservice.New(pool, clock, svc, svc.RecentReauth), log).Register(v1, h.Authenticate(false), h.CSRF())
	cathandler.New(catservice.New(pool, svc), log).Register(v1, h.Authenticate(false), h.CSRF())
	orderSvc := orderservice.New(pool, svc)
	orderhandler.New(orderSvc, log).Register(v1, h.Authenticate(false), h.CSRF())
	setSvc := setservice.New(pool, clock, svc, svc.RecentReauth)
	salesSvc := salesservice.New(pool, svc, orderSvc, setSvc, setSvc)
	saleshandler.New(salesSvc, log).Register(v1, h.Authenticate(false), h.CSRF())
	printhandler.New(printservice.New(pool, svc, salesSvc, orderSvc, setSvc), log).Register(v1, h.Authenticate(false), h.CSRF())
	// Probe routes stand in for ordinary protected endpoints that later stages add.
	v1.GET("/_probe", h.Authenticate(false), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	v1.GET("/_sensitive", h.Authenticate(false), h.RequireRecentReauth(), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	return &testEnv{t: t, pool: pool, clock: clock, svc: svc, h: h, router: router, cfg: cfg}
}

func (e *testEnv) addUser(username string, mustChange bool, roles ...string) int64 {
	e.t.Helper()
	ctx := context.Background()
	var id int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (username, display_name, password_hash, must_change_password) VALUES ($1, $1, $2, $3) RETURNING id`, username, testHash(e.t), mustChange).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := e.pool.Exec(ctx, `INSERT INTO user_roles (user_id, role_id) SELECT $1, id FROM roles WHERE code = $2`, id, r); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func (e *testEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// jar is a minimal cookie store for one browser.
type jar map[string]string

func (j jar) absorb(res *http.Response) {
	for _, c := range res.Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(j, c.Name)
		} else {
			j[c.Name] = c.Value
		}
	}
}

type req struct {
	method, path string
	body         any
	jar          jar
	origin       string // default: the web app
	noOrigin     bool
	noCSRF       bool   // do not add the X-CSRF-Token header automatically
	csrf         string // explicit header value
	remote       string // default 192.0.2.10:5000
	header       map[string]string
}

func (e *testEnv) do(r req) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if r.body != nil {
		b, _ := json.Marshal(r.body)
		rd = bytes.NewReader(b)
	}
	hr := httptest.NewRequest(r.method, r.path, rd)
	hr.RemoteAddr = "192.0.2.10:5000"
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	if r.body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	if !r.noOrigin {
		o := r.origin
		if o == "" {
			o = frontend
		}
		hr.Header.Set("Origin", o)
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	for n, v := range r.jar {
		hr.AddCookie(&http.Cookie{Name: n, Value: v})
	}
	if r.csrf != "" {
		hr.Header.Set(handler.CSRFHeader, r.csrf)
	} else if !r.noCSRF && r.jar != nil && r.jar[handler.CSRFCookie] != "" && r.method != http.MethodGet {
		hr.Header.Set(handler.CSRFHeader, r.jar[handler.CSRFCookie])
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, hr)
	if r.jar != nil {
		r.jar.absorb(w.Result())
	}
	return w
}

func (e *testEnv) login(username, password string, j jar) *httptest.ResponseRecorder {
	return e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]string{"username": username, "password": password}, jar: j})
}

func (e *testEnv) mustLogin(username string) jar {
	e.t.Helper()
	j := jar{}
	if w := e.login(username, pw, j); w.Code != 200 {
		e.t.Fatalf("login %s: %d %s", username, w.Code, w.Body.String())
	}
	return j
}

type errBody struct {
	Error struct {
		Code      string                            `json:"code"`
		Message   string                            `json:"message"`
		RequestID string                            `json:"request_id"`
		Fields    []struct{ Field, Message string } `json:"fields"`
	} `json:"error"`
}

func code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b errBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not an error body (%d): %s", w.Code, w.Body.String())
	}
	return b.Error.Code
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int, errCode string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if errCode != "" {
		if got := code(t, w); got != errCode {
			t.Fatalf("error code = %q, want %q: %s", got, errCode, w.Body.String())
		}
	}
}

type meResp struct {
	User struct {
		ID       int64
		Username string
	} `json:"user"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	MustChange  bool     `json:"must_change_password"`
	AuthMethod  string   `json:"auth_method"`
	Session     struct {
		Idle   time.Time `json:"idle_expires_at"`
		Abs    time.Time `json:"absolute_expires_at"`
		Reauth bool      `json:"recently_reauthenticated"`
	} `json:"session"`
}

func (e *testEnv) me(j jar) (*httptest.ResponseRecorder, meResp) {
	e.t.Helper()
	w := e.do(req{method: "GET", path: "/api/v1/auth/me", jar: j})
	var m meResp
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w, m
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type httpResp struct{ W *httptest.ResponseRecorder }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
