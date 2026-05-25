package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/seakee/cpa-manager/usage-service/internal/collector"
	"github.com/seakee/cpa-manager/usage-service/internal/config"
	"github.com/seakee/cpa-manager/usage-service/internal/store"
)

// fakeUpstream captures the request the proxy forwarded, so tests can assert on
// the rewritten path and the injected Authorization header.
type fakeUpstream struct {
	mu     sync.Mutex
	last   *http.Request
	status int
	body   string
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Make a shallow clone with body buffered so the test can read it.
		clone := r.Clone(context.Background())
		clone.Body = io.NopCloser(strings.NewReader(""))
		f.mu.Lock()
		f.last = clone
		status := f.status
		body := f.body
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func (f *fakeUpstream) lastRequest(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		t.Fatalf("upstream never received a request")
	}
	return f.last
}

// newImageProxyHandler builds a Server.Handler() pre-wired to a httptest.Server
// upstream and (optionally) a saved Setup row whose ManagementKey is the
// "outside-world" Bearer token. Setup is persisted via the SAME store handle
// the server uses, otherwise cross-connection visibility on SQLite makes the
// row invisible to the first request.
func newImageProxyHandler(t *testing.T, mgmtKey string) (http.Handler, *fakeUpstream, func()) {
	t.Helper()
	upstream := &fakeUpstream{status: http.StatusOK, body: `{"ok":true}`}
	ts := httptest.NewServer(upstream.handler())

	cfg := config.Config{
		DBPath:                 filepath.Join(t.TempDir(), "usage.sqlite"),
		CORSOrigins:            []string{"*"},
		ChatGPT2APIUpstreamURL: ts.URL,
		ChatGPT2APIInternalKey: "internal-secret",
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if mgmtKey != "" {
		// SaveSetup requires CPAUpstreamURL+ManagementKey non-empty; the URL
		// is irrelevant for these proxy-auth tests but must validate.
		if err := db.SaveSetup(context.Background(), store.Setup{
			CPAUpstreamURL: "http://cpa.invalid",
			ManagementKey:  mgmtKey,
			Queue:          "usage",
			PopSide:        "right",
		}); err != nil {
			t.Fatalf("save setup: %v", err)
		}
	}

	manager := collector.NewManager(cfg, db)
	handler := New(cfg, db, manager).Handler()
	cleanup := func() { ts.Close() }
	return handler, upstream, cleanup
}

func TestImageProxyRejectsMissingAuth(t *testing.T) {
	handler, upstream, cleanup := newImageProxyHandler(t, "outer-mgmt-key")
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d (body=%q)", w.Code, w.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.last != nil {
		t.Fatalf("upstream should not have been called on unauthenticated request")
	}
}

func TestImageProxyRejectsWrongAuth(t *testing.T) {
	handler, upstream, cleanup := newImageProxyHandler(t, "outer-mgmt-key")
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d (body=%q)", w.Code, w.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.last != nil {
		t.Fatalf("upstream should not have been called on bad-credential request")
	}
}

func TestImageProxyForwardsWithInternalKeyAndStripsPrefix(t *testing.T) {
	handler, upstream, cleanup := newImageProxyHandler(t, "outer-mgmt-key")
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/images/generations", strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer outer-mgmt-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	got := upstream.lastRequest(t)
	if got.URL.Path != "/v1/images/generations" {
		t.Errorf("upstream path: want /v1/images/generations, got %q", got.URL.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer internal-secret" {
		t.Errorf("upstream Authorization: want %q, got %q", "Bearer internal-secret", h)
	}
}

func TestImageProxyStripsV0ImagePrefix(t *testing.T) {
	handler, upstream, cleanup := newImageProxyHandler(t, "")
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v0/image/accounts", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", w.Code, w.Body.String())
	}
	got := upstream.lastRequest(t)
	if got.URL.Path != "/api/accounts" {
		t.Errorf("upstream path: want /api/accounts, got %q", got.URL.Path)
	}
	// When no outer mgmt key is configured, the proxy still injects the internal one.
	if h := got.Header.Get("Authorization"); h != "Bearer internal-secret" {
		t.Errorf("upstream Authorization: want %q, got %q", "Bearer internal-secret", h)
	}
}

func TestImageProxyReturns503WhenUpstreamDown(t *testing.T) {
	cfg := config.Config{
		CORSOrigins:            []string{"*"},
		ChatGPT2APIUpstreamURL: "http://127.0.0.1:1", // RFC-reserved unreachable port
		ChatGPT2APIInternalKey: "internal-secret",
	}
	handler := newTestHandlerWithConfig(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d (body=%q)", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on 503")
	}
}

func TestStripChatGPT2APIPrefix(t *testing.T) {
	cases := []struct {
		in       string
		wantOut  string
		wantHit  bool
	}{
		{"/openai/v1/models", "/v1/models", true},
		{"/openai/v1/images/generations", "/v1/images/generations", true},
		{"/openai", "/", true},
		{"/openai/", "/", true},
		{"/v0/image/accounts", "/api/accounts", true},
		{"/v0/image/accounts/refresh", "/api/accounts/refresh", true},
		{"/v0/image", "/api", true},
		{"/v0/image/", "/api/", true},
		{"/openaibogus", "/openaibogus", false}, // must not match without trailing /
		{"/v0/management/foo", "/v0/management/foo", false},
		{"/", "/", false},
	}
	for _, tc := range cases {
		got, hit := stripChatGPT2APIPrefix(tc.in)
		if got != tc.wantOut || hit != tc.wantHit {
			t.Errorf("strip(%q) = (%q,%v), want (%q,%v)", tc.in, got, hit, tc.wantOut, tc.wantHit)
		}
	}
}
