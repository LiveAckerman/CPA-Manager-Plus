package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager/usage-service/internal/collector"
	"github.com/seakee/cpa-manager/usage-service/internal/config"
	"github.com/seakee/cpa-manager/usage-service/internal/store"
)

// fakeCPA stands in for the real CPA upstream during validator tests. It
// serves /v1/models with a configurable status and counts hits so tests can
// assert that the validator cached / didn't cache as expected.
type fakeCPA struct {
	validKeys map[string]bool // key -> valid?
	hits      atomic.Int32
	status5xx atomic.Bool // when true, always return 503 regardless of key
}

func (c *fakeCPA) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		if c.status5xx.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"upstream down"}`)
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(auth) <= len(prefix) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := strings.TrimSpace(auth[len(prefix):])
		if c.validKeys[key] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
}

// authFixture builds a Server with a mock chatgpt2api upstream + a mock CPA
// upstream, saves a Setup row tying things together, and returns both mocks
// for assertions.
type authFixture struct {
	handler http.Handler
	chatgpt *recordingUpstream
	cpa     *fakeCPA
	cpaSrv  *httptest.Server
	cleanup func()
}

func newAuthFixture(t *testing.T, mgmtKey string, validClientKeys ...string) *authFixture {
	t.Helper()

	chat := &recordingUpstream{status: 200, resp: `{"data":[{"b64_json":"AAA"}]}`}
	chatSrv := httptest.NewServer(chat.server().Config.Handler)

	validSet := make(map[string]bool, len(validClientKeys))
	for _, k := range validClientKeys {
		validSet[k] = true
	}
	cpa := &fakeCPA{validKeys: validSet}
	cpaSrv := httptest.NewServer(cpa.handler())

	cfg := config.Config{
		DBPath:                 filepath.Join(t.TempDir(), "usage.sqlite"),
		CORSOrigins:            []string{"*"},
		ChatGPT2APIUpstreamURL: chatSrv.URL,
		ChatGPT2APIInternalKey: "internal-secret",
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.SaveSetup(context.Background(), store.Setup{
		CPAUpstreamURL: cpaSrv.URL,
		ManagementKey:  mgmtKey,
		Queue:          "usage",
		PopSide:        "right",
	}); err != nil {
		t.Fatalf("save setup: %v", err)
	}

	manager := collector.NewManager(cfg, db)
	handler := New(cfg, db, manager).Handler()

	return &authFixture{
		handler: handler,
		chatgpt: chat,
		cpa:     cpa,
		cpaSrv:  cpaSrv,
		cleanup: func() {
			chatSrv.Close()
			cpaSrv.Close()
		},
	}
}

func postImage(t *testing.T, h http.Handler, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// --- Mgmt Key path (regression) ---------------------------------------------

func TestImageGenAuth_MgmtKeyStillWorks(t *testing.T) {
	fx := newAuthFixture(t, "outer-mgmt-key")
	defer fx.cleanup()

	w := postImage(t, fx.handler, "outer-mgmt-key")
	if w.Code != 200 {
		t.Fatalf("want 200 (Mgmt Key accepted), got %d (body=%s)", w.Code, w.Body.String())
	}
	if fx.cpa.hits.Load() != 0 {
		t.Errorf("CPA should NOT be hit when Mgmt Key matches (hits=%d)", fx.cpa.hits.Load())
	}
}

// --- CPA client API key path ------------------------------------------------

func TestImageGenAuth_CPAValidatedClientKeyWorks(t *testing.T) {
	fx := newAuthFixture(t, "outer-mgmt-key", "sk-good-client-key")
	defer fx.cleanup()

	w := postImage(t, fx.handler, "sk-good-client-key")
	if w.Code != 200 {
		t.Fatalf("want 200 (CPA validated client key), got %d (body=%s)", w.Code, w.Body.String())
	}
	if hits := fx.cpa.hits.Load(); hits != 1 {
		t.Errorf("CPA hits: want 1 (one validation), got %d", hits)
	}
}

func TestImageGenAuth_CPAValidationCachedSecondCall(t *testing.T) {
	fx := newAuthFixture(t, "outer-mgmt-key", "sk-cache-me")
	defer fx.cleanup()

	for i := 0; i < 3; i++ {
		w := postImage(t, fx.handler, "sk-cache-me")
		if w.Code != 200 {
			t.Fatalf("iter %d: want 200, got %d", i, w.Code)
		}
	}
	if hits := fx.cpa.hits.Load(); hits != 1 {
		t.Errorf("CPA hits across 3 requests: want 1 (cached after first), got %d", hits)
	}
}

func TestImageGenAuth_CPARejectedKeyReturns401(t *testing.T) {
	fx := newAuthFixture(t, "outer-mgmt-key", "sk-good")
	defer fx.cleanup()

	w := postImage(t, fx.handler, "sk-wrong")
	if w.Code != 401 {
		t.Fatalf("want 401, got %d (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid api key") {
		t.Errorf("body should mention invalid api key, got: %s", w.Body.String())
	}
}

func TestImageGenAuth_CPAUnreachableReturns503(t *testing.T) {
	fx := newAuthFixture(t, "outer-mgmt-key", "sk-good")
	defer fx.cleanup()
	fx.cpa.status5xx.Store(true) // CPA temporarily down

	w := postImage(t, fx.handler, "sk-good")
	if w.Code != 503 {
		t.Fatalf("want 503 (CPA down), got %d (body=%s)", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on 503")
	}
}

// --- Malformed Authorization -------------------------------------------------

func TestImageGenAuth_MissingHeaderReturns401(t *testing.T) {
	fx := newAuthFixture(t, "outer-mgmt-key")
	defer fx.cleanup()

	w := postImage(t, fx.handler, "")
	if w.Code != 401 {
		t.Fatalf("want 401 (missing header), got %d", w.Code)
	}
}

func TestImageGenAuth_MalformedHeaderReturns401(t *testing.T) {
	fx := newAuthFixture(t, "outer-mgmt-key")
	defer fx.cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "NotBearer abc") // wrong scheme
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	fx.handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("want 401 (malformed header), got %d", w.Code)
	}
}

// --- apiKeyValidator unit-level (cache eviction on expiry) -----------------

func TestAPIKeyValidator_CacheExpires(t *testing.T) {
	cpa := &fakeCPA{validKeys: map[string]bool{"sk": true}}
	srv := httptest.NewServer(cpa.handler())
	defer srv.Close()

	v := newAPIKeyValidator()
	v.ttl = 50 * time.Millisecond // override for fast test
	v.httpClient = &http.Client{Timeout: 2 * time.Second}

	for i := 0; i < 2; i++ {
		ok, err := v.Validate(context.Background(), srv.URL, "sk")
		if err != nil || !ok {
			t.Fatalf("iter %d: want (true,nil), got (%v,%v)", i, ok, err)
		}
	}
	if h := cpa.hits.Load(); h != 1 {
		t.Errorf("after 2 quick calls: want 1 upstream hit (cached), got %d", h)
	}

	time.Sleep(70 * time.Millisecond) // let cache expire
	ok, err := v.Validate(context.Background(), srv.URL, "sk")
	if err != nil || !ok {
		t.Fatalf("post-expiry: want (true,nil), got (%v,%v)", ok, err)
	}
	if h := cpa.hits.Load(); h != 2 {
		t.Errorf("after expiry + 1 call: want 2 upstream hits, got %d", h)
	}
}

func TestAPIKeyValidator_NoCacheOnRejection(t *testing.T) {
	cpa := &fakeCPA{validKeys: map[string]bool{}} // nothing valid
	srv := httptest.NewServer(cpa.handler())
	defer srv.Close()

	v := newAPIKeyValidator()
	v.httpClient = &http.Client{Timeout: 2 * time.Second}

	for i := 0; i < 3; i++ {
		ok, err := v.Validate(context.Background(), srv.URL, "sk-bad")
		if err != nil || ok {
			t.Fatalf("iter %d: want (false,nil), got (%v,%v)", i, ok, err)
		}
	}
	if h := cpa.hits.Load(); h != 3 {
		t.Errorf("after 3 rejected calls: want 3 upstream hits (negatives not cached), got %d", h)
	}
}
