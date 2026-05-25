package httpapi

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// handleCPAOpenAIPassthrough is a transparent reverse proxy for OpenAI-API
// shaped routes (/v1/responses, /v1/chat/completions, /v1/embeddings,
// /v1/audio/*, /v1/files/*, /v1/threads/*, /v1/vector_stores/*, etc.).
// Each request is forwarded to the configured CPA upstream with the client's
// own Authorization header preserved verbatim.
//
// Why this exists
//
// The original CPA-Manager only proxied the management surface
// (/v0/management/*) plus a single model-list shortcut (/v1/models). When
// operators add the panel URL as an upstream to OpenAI-compatible aggregators
// like sub2api, those aggregators probe additional /v1/* paths — typically
// /v1/responses — to detect what the upstream supports. The panel used to
// return 404 for every such path, so aggregators concluded the upstream
// didn't support the Responses API even though CPA itself does. This handler
// closes that gap: anything under /v1/ that isn't claimed by a more specific
// route just bounces straight through to CPA.
//
// Auth model
//
// This is the NON-admin path. The client's Authorization header is forwarded
// to CPA unchanged — it must be a CPA-issued client API key, which CPA
// validates itself. We deliberately don't substitute the Management Key
// here: that would force every API consumer to hold an admin token, which
// they shouldn't.
//
// Routes that pre-empt this passthrough (because they're matched earlier)
//
//   - /v1/models                 ->  handleModelListProxy (already proxies to CPA)
//   - /v1/images/generations     ->  smart router (Phase 2.5, when present)
//   - /v1/images/edits           ->  smart router (Phase 2.5, when present)
//   - /openai/v1/...             ->  chatgpt2api passthrough (Phase 2, when present)
//
// Exact-match mux entries always win over the "/" catch-all that dispatches
// into handleRoot, so adding Phase 2/2.5 routes later does not require
// changes here.
//
// Streaming
//
// Chat completions and Responses API commonly stream via SSE. httputil's
// ReverseProxy flushes streamed bytes automatically; no extra wiring is
// needed beyond using it.

// isCPAOpenAIPassthroughPath reports whether handleRoot should hand the
// request to handleCPAOpenAIPassthrough. Called AFTER isModelListProxyPath
// inside handleRoot, so /v1/models is already routed elsewhere by the time
// we reach this check.
func isCPAOpenAIPassthroughPath(path string) bool {
	return strings.HasPrefix(path, "/v1/")
}

func (s *Server) handleCPAOpenAIPassthrough(w http.ResponseWriter, r *http.Request) {
	setup, ok, err := s.resolveSetup(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok || strings.TrimSpace(setup.CPAUpstreamURL) == "" {
		writeError(w, http.StatusPreconditionRequired, errors.New("usage service is not configured"))
		return
	}
	target, err := url.Parse(setup.CPAUpstreamURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		// Authorization is intentionally NOT modified: we forward the
		// client's CPA-issued API key as-is. CPA validates it itself.
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeError(w, http.StatusBadGateway, err)
	}
	proxy.ServeHTTP(w, r)
}
