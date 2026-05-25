package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingUpstream captures whatever the proxy forwarded so each test can
// assert on path/method/auth/body without rolling its own channel.
type recordingUpstream struct {
	hits   atomic.Int32
	method atomic.Value // string
	path   atomic.Value // string
	query  atomic.Value // string
	auth   atomic.Value // string
	body   atomic.Value // string
	status int
	resp   string
}

func (u *recordingUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.method.Store(r.Method)
		u.path.Store(r.URL.Path)
		u.query.Store(r.URL.RawQuery)
		u.auth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		u.body.Store(string(body))
		status := u.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, u.resp)
	}))
}

func (u *recordingUpstream) gotPath() string  { v, _ := u.path.Load().(string); return v }
func (u *recordingUpstream) gotAuth() string  { v, _ := u.auth.Load().(string); return v }
func (u *recordingUpstream) gotBody() string  { v, _ := u.body.Load().(string); return v }
func (u *recordingUpstream) gotQuery() string { v, _ := u.query.Load().(string); return v }
func (u *recordingUpstream) gotMethod() string {
	v, _ := u.method.Load().(string)
	return v
}

// --- Table-driven: every passthrough-eligible /v1/* path lands on CPA ------

func TestCPAPassthroughForwardsV1RoutesToCPA(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "responses POST (JSON)", method: http.MethodPost, path: "/v1/responses", body: `{"model":"gpt-4o","input":"hi"}`},
		{name: "chat completions POST (JSON)", method: http.MethodPost, path: "/v1/chat/completions", body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`},
		{name: "embeddings POST (JSON)", method: http.MethodPost, path: "/v1/embeddings", body: `{"model":"text-embedding-3-small","input":"hi"}`},
		{name: "audio speech POST (nested path)", method: http.MethodPost, path: "/v1/audio/speech", body: `{"model":"tts-1","input":"hi","voice":"alloy"}`},
		{name: "files GET (no body)", method: http.MethodGet, path: "/v1/files", body: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &recordingUpstream{status: 200, resp: `{"ok":true}`}
			srv := up.server()
			t.Cleanup(srv.Close)

			handler := newTestHandler(t, srv.URL, true)

			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Authorization", "Bearer client-cpa-key")
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			if up.hits.Load() != 1 {
				t.Fatalf("upstream hits = %d (want 1)", up.hits.Load())
			}
			if got, want := up.gotPath(), tc.path; got != want {
				t.Errorf("upstream path = %q, want %q", got, want)
			}
			if got, want := up.gotMethod(), tc.method; got != want {
				t.Errorf("upstream method = %q, want %q", got, want)
			}
			if got, want := up.gotAuth(), "Bearer client-cpa-key"; got != want {
				t.Errorf("upstream Authorization = %q, want %q (client key must pass through unchanged)", got, want)
			}
			if got, want := up.gotBody(), tc.body; got != want {
				t.Errorf("upstream body = %q, want %q", got, want)
			}
		})
	}
}

// --- Query string is forwarded -----------------------------------------------

func TestCPAPassthroughPreservesQueryString(t *testing.T) {
	up := &recordingUpstream{status: 200, resp: `{}`}
	srv := up.server()
	t.Cleanup(srv.Close)

	handler := newTestHandler(t, srv.URL, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/files?purpose=fine-tune&limit=10", nil)
	req.Header.Set("Authorization", "Bearer x")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got, want := up.gotQuery(), "purpose=fine-tune&limit=10"; got != want {
		t.Errorf("upstream query = %q, want %q", got, want)
	}
}

// --- /v1/models is owned by handleModelListProxy, NOT our passthrough -------
//
// handleModelListProxy is method-restricted to GET; our passthrough is not.
// So POST /v1/models on a bare main (no Phase 2 routes) goes through
// handleRoot, matches isModelListProxyPath FIRST, and is rejected with 405
// by handleModelListProxy. If our passthrough were accidentally catching
// it instead, we'd see 200 (the mock upstream would serve any method).

func TestCPAPassthroughDoesNotShadowV1Models(t *testing.T) {
	up := &recordingUpstream{status: 200, resp: `{"data":[]}`}
	srv := up.server()
	t.Cleanup(srv.Close)

	handler := newTestHandler(t, srv.URL, true)

	// GET /v1/models — goes to handleModelListProxy. We don't assert which
	// path matched; we only assert the request reached SOME proxy that hit
	// the upstream with the right shape, then verify POST below.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	getReq.Header.Set("Authorization", "Bearer x")
	getRR := httptest.NewRecorder()
	handler.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: status = %d body=%s", getRR.Code, getRR.Body.String())
	}
	if up.gotPath() != "/v1/models" {
		t.Fatalf("GET /v1/models: upstream path = %q", up.gotPath())
	}

	// Reset and try POST. handleModelListProxy hard-rejects non-GET, so we
	// expect 405 — proving the passthrough did NOT catch it.
	up.hits.Store(0)
	postReq := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader("{}"))
	postReq.Header.Set("Authorization", "Bearer x")
	postRR := httptest.NewRecorder()
	handler.ServeHTTP(postRR, postReq)
	if postRR.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models: want 405 (from handleModelListProxy), got %d. If 200, the passthrough is incorrectly shadowing /v1/models.", postRR.Code)
	}
	if up.hits.Load() != 0 {
		t.Errorf("POST /v1/models: upstream should not have been called (was hit %d times)", up.hits.Load())
	}
}

// --- Without a Setup row, passthrough fails fast with 428 -------------------

func TestCPAPassthroughRequires428WhenUnconfigured(t *testing.T) {
	handler := newTestHandler(t, "http://unused.example", false) // saveSetup=false
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d (want 428 PreconditionRequired) body=%s", rr.Code, rr.Body.String())
	}
}

// --- isCPAOpenAIPassthroughPath classification ------------------------------

func TestIsCPAOpenAIPassthroughPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/responses", true},
		{"/v1/chat/completions", true},
		{"/v1/embeddings", true},
		{"/v1/audio/speech", true},
		{"/v1/files", true},
		{"/v1/files/abc", true},
		{"/v1/", true},
		{"/v0/management/foo", false},
		{"/openai/v1/foo", false},
		{"/v0/image/foo", false},
		{"/v1bogus", false}, // must require trailing slash
		{"/", false},
		{"/setup", false},
	}
	for _, tc := range cases {
		if got := isCPAOpenAIPassthroughPath(tc.path); got != tc.want {
			t.Errorf("isCPAOpenAIPassthroughPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
