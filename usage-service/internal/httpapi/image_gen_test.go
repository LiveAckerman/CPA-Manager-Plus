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

	"github.com/seakee/cpa-manager/usage-service/internal/collector"
	"github.com/seakee/cpa-manager/usage-service/internal/config"
	"github.com/seakee/cpa-manager/usage-service/internal/store"
)

// scriptedUpstream serves a fixed status + body and records hits + the last
// Authorization header it received. Used to stand in for both chatgpt2api
// and CPA in the tests below.
type scriptedUpstream struct {
	status   int
	body     string
	hits     atomic.Int32
	lastAuth atomic.Value // string
	lastPath atomic.Value // string
}

func (u *scriptedUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.lastAuth.Store(r.Header.Get("Authorization"))
		u.lastPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = io.WriteString(w, u.body)
	})
}

func (u *scriptedUpstream) auth() string {
	v, _ := u.lastAuth.Load().(string)
	return v
}

func (u *scriptedUpstream) path() string {
	v, _ := u.lastPath.Load().(string)
	return v
}

// imageGenFixture builds a server with both backends mocked. The caller picks
// whether fallback is enabled and what each upstream returns. Returns a
// teardown that must be invoked even on failure.
type imageGenFixture struct {
	handler  http.Handler
	chatgpt  *scriptedUpstream
	cpa      *scriptedUpstream
	teardown func()
}

func newImageGenFixture(t *testing.T, opts imageGenFixtureOpts) *imageGenFixture {
	t.Helper()

	chatUp := &scriptedUpstream{status: opts.chatStatus, body: opts.chatBody}
	cpaUp := &scriptedUpstream{status: opts.cpaStatus, body: opts.cpaBody}
	chatSrv := httptest.NewServer(chatUp.handler())
	cpaSrv := httptest.NewServer(cpaUp.handler())

	cfg := config.Config{
		DBPath:                  filepath.Join(t.TempDir(), "usage.sqlite"),
		CORSOrigins:             []string{"*"},
		ChatGPT2APIUpstreamURL:  chatSrv.URL,
		ChatGPT2APIInternalKey:  "internal-secret",
		ImageCPAFallbackEnabled: opts.fallbackEnabled,
		CPAImageAPIKey:          opts.cpaImageKey,
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if opts.setupCPAURL != "" || opts.mgmtKey != "" {
		setup := store.Setup{
			CPAUpstreamURL: opts.setupCPAURL,
			ManagementKey:  opts.mgmtKey,
			Queue:          "usage",
			PopSide:        "right",
		}
		if setup.CPAUpstreamURL == "" {
			// SaveSetup requires both; satisfy validation with a sentinel.
			setup.CPAUpstreamURL = cpaSrv.URL
		}
		if setup.ManagementKey == "" {
			setup.ManagementKey = "dummy"
		}
		if err := db.SaveSetup(context.Background(), setup); err != nil {
			t.Fatalf("save setup: %v", err)
		}
	}

	manager := collector.NewManager(cfg, db)
	handler := New(cfg, db, manager).Handler()

	return &imageGenFixture{
		handler: handler,
		chatgpt: chatUp,
		cpa:     cpaUp,
		teardown: func() {
			chatSrv.Close()
			cpaSrv.Close()
		},
	}
}

type imageGenFixtureOpts struct {
	chatStatus      int
	chatBody        string
	cpaStatus       int
	cpaBody         string
	fallbackEnabled bool
	cpaImageKey     string
	mgmtKey         string // outer auth; empty means anonymous mode
	setupCPAURL     string // setup.CPAUpstreamURL; empty defaults to cpa httptest URL
}

func postImageGen(t *testing.T, h http.Handler, mgmtKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if mgmtKey != "" {
		req.Header.Set("Authorization", "Bearer "+mgmtKey)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// --- Success path ------------------------------------------------------------

func TestImageGenPassesThroughChatGPT2APISuccess(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{
		chatStatus: 200,
		chatBody:   `{"data":[{"b64_json":"AAA"}]}`,
	})
	defer fx.teardown()

	w := postImageGen(t, fx.handler, "", `{"prompt":"a cat","model":"gpt-image-2"}`)

	if w.Code != 200 {
		t.Fatalf("status: want 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Image-Resolved-Backend"); got != "chatgpt2api" {
		t.Errorf("X-Image-Resolved-Backend: want chatgpt2api, got %q", got)
	}
	if fx.cpa.hits.Load() != 0 {
		t.Errorf("CPA should not have been called (hits=%d)", fx.cpa.hits.Load())
	}
	if !strings.Contains(w.Body.String(), `"b64_json":"AAA"`) {
		t.Errorf("body not echoed from chatgpt2api: %s", w.Body.String())
	}
	if got := fx.chatgpt.auth(); got != "Bearer internal-secret" {
		t.Errorf("chatgpt2api saw wrong Authorization: %q", got)
	}
}

// --- 429 from chatgpt2api triggers fallback (insufficient_quota path) -------

func TestImageGen429TriggersFallback(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{
		chatStatus:      429,
		chatBody:        `{"error":{"code":"insufficient_quota"}}`,
		cpaStatus:       200,
		cpaBody:         `{"data":[{"b64_json":"FALLBACK"}]}`,
		fallbackEnabled: true,
		cpaImageKey:     "cpa-key",
		mgmtKey:         "outer-mgmt",
	})
	defer fx.teardown()

	w := postImageGen(t, fx.handler, "outer-mgmt", `{"prompt":"x","model":"gpt-image-2"}`)
	if w.Code != 200 {
		t.Fatalf("want 200 (from CPA), got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Image-Resolved-Backend"); got != "cpa" {
		t.Errorf("want resolved=cpa, got %q", got)
	}
	if got := w.Header().Get("X-Image-Fallback-Trigger"); got != "status-429" {
		t.Errorf("want trigger=status-429, got %q", got)
	}
	if !strings.Contains(w.Body.String(), "FALLBACK") {
		t.Errorf("expected CPA body, got %s", w.Body.String())
	}
}

// --- 4xx (non-429) is NOT retried -------------------------------------------

func TestImageGen4xxFromChatGPT2APIIsNotRetried(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{
		chatStatus:      400,
		chatBody:        `{"error":"bad prompt"}`,
		cpaStatus:       200,
		cpaBody:         `{"data":[]}`,
		fallbackEnabled: true,
		cpaImageKey:     "cpa-key",
	})
	defer fx.teardown()

	w := postImageGen(t, fx.handler, "", `{"prompt":"","model":"gpt-image-2"}`)

	if w.Code != 400 {
		t.Fatalf("status: want 400 (pass-through), got %d", w.Code)
	}
	if fx.cpa.hits.Load() != 0 {
		t.Errorf("CPA should NOT have been called on 4xx (hits=%d)", fx.cpa.hits.Load())
	}
	if got := w.Header().Get("X-Image-Resolved-Backend"); got != "chatgpt2api" {
		t.Errorf("X-Image-Resolved-Backend: want chatgpt2api, got %q", got)
	}
}

// --- 5xx WITH fallback fully wired: falls over to CPA ------------------------

func TestImageGen5xxFallsBackToCPA(t *testing.T) {
	// Setting mgmtKey causes the fixture to persist a Setup row whose
	// CPAUpstreamURL points at the CPA mock — fallback eligibility needs
	// that DB row to find the upstream URL.
	fx := newImageGenFixture(t, imageGenFixtureOpts{
		chatStatus:      500,
		chatBody:        `{"error":"no available account"}`,
		cpaStatus:       200,
		cpaBody:         `{"data":[{"url":"https://example.invalid/img.png"}]}`,
		fallbackEnabled: true,
		cpaImageKey:     "cpa-key-XYZ",
		mgmtKey:         "outer-mgmt",
	})
	defer fx.teardown()

	w := postImageGen(t, fx.handler, "outer-mgmt", `{"prompt":"a dog","model":"gpt-image-2"}`)

	if w.Code != 200 {
		t.Fatalf("status: want 200 (from CPA), got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Image-Resolved-Backend"); got != "cpa" {
		t.Errorf("X-Image-Resolved-Backend: want cpa, got %q", got)
	}
	if got := w.Header().Get("X-Image-Fallback-Trigger"); got != "status-500" {
		t.Errorf("X-Image-Fallback-Trigger: want status-500, got %q", got)
	}
	if fx.cpa.hits.Load() != 1 {
		t.Errorf("CPA hits: want 1, got %d", fx.cpa.hits.Load())
	}
	if got := fx.cpa.auth(); got != "Bearer cpa-key-XYZ" {
		t.Errorf("CPA saw wrong Authorization: %q", got)
	}
	if got := fx.cpa.path(); got != "/v1/images/generations" {
		t.Errorf("CPA upstream path: want /v1/images/generations, got %q", got)
	}
	if !strings.Contains(w.Body.String(), "img.png") {
		t.Errorf("body not echoed from CPA: %s", w.Body.String())
	}
}

// --- 5xx WITHOUT fallback enabled: chatgpt2api error returned as-is ----------

func TestImageGen5xxFallbackDisabled(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{
		chatStatus:      503,
		chatBody:        `{"error":"no available account"}`,
		fallbackEnabled: false,
		cpaImageKey:     "cpa-key", // present but flag off
	})
	defer fx.teardown()

	w := postImageGen(t, fx.handler, "", `{"prompt":"a dog","model":"gpt-image-2"}`)

	if w.Code != 503 {
		t.Fatalf("status: want 503 (chatgpt2api pass-through), got %d", w.Code)
	}
	if got := w.Header().Get("X-Image-Resolved-Backend"); got != "chatgpt2api" {
		t.Errorf("X-Image-Resolved-Backend: want chatgpt2api, got %q", got)
	}
	if got := w.Header().Get("X-Image-Fallback-Skipped"); got != "disabled" {
		t.Errorf("X-Image-Fallback-Skipped: want disabled, got %q", got)
	}
	if fx.cpa.hits.Load() != 0 {
		t.Errorf("CPA should NOT have been called (hits=%d)", fx.cpa.hits.Load())
	}
}

// --- 5xx WITH fallback enabled but NO key: flag-on but key-missing -----------

func TestImageGen5xxFallbackEnabledNoKey(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{
		chatStatus:      503,
		chatBody:        `{"error":"x"}`,
		fallbackEnabled: true,
		cpaImageKey:     "", // key not configured
	})
	defer fx.teardown()

	w := postImageGen(t, fx.handler, "", `{"prompt":"a dog","model":"gpt-image-2"}`)

	if w.Code != 503 {
		t.Fatalf("status: want 503, got %d", w.Code)
	}
	if got := w.Header().Get("X-Image-Fallback-Skipped"); got != "no-cpa-key" {
		t.Errorf("X-Image-Fallback-Skipped: want no-cpa-key, got %q", got)
	}
}

// --- Auth on the outer layer -------------------------------------------------

func TestImageGenRequiresMgmtKeyWhenConfigured(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{
		chatStatus: 200,
		chatBody:   `{"data":[]}`,
		mgmtKey:    "outer-mgmt",
	})
	defer fx.teardown()

	w := postImageGen(t, fx.handler, "", `{"prompt":"x"}`) // no auth
	if w.Code != 401 {
		t.Fatalf("missing auth: want 401, got %d", w.Code)
	}

	w = postImageGen(t, fx.handler, "wrong", `{"prompt":"x"}`) // wrong auth
	if w.Code != 401 {
		t.Fatalf("wrong auth: want 401, got %d", w.Code)
	}

	w = postImageGen(t, fx.handler, "outer-mgmt", `{"prompt":"x"}`) // good auth
	if w.Code != 200 {
		t.Fatalf("good auth: want 200, got %d (body=%s)", w.Code, w.Body.String())
	}
}

// --- GET (or any non-POST) is rejected ---------------------------------------

func TestImageGenRejectsNonPost(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{chatStatus: 200, chatBody: "ok"})
	defer fx.teardown()

	req := httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil)
	w := httptest.NewRecorder()
	fx.handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

// --- Oversize body returns 413 -----------------------------------------------

func TestImageGenRejectsOversizeBody(t *testing.T) {
	fx := newImageGenFixture(t, imageGenFixtureOpts{chatStatus: 200, chatBody: "ok"})
	defer fx.teardown()

	huge := strings.Repeat("A", maxImageGenRequestBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	fx.handler.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", w.Code)
	}
}
