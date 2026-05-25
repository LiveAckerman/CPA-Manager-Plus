package httpapi

import (
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// chatGPT2APIProxy reverse-proxies a curated subset of routes to the
// chatgpt2api FastAPI service that runs alongside cpa-manager inside the same
// container. Three invariants are enforced here:
//
//  1. The outside world authenticates with the CPA Management Key (the same
//     bearer token the rest of the panel already uses). That check happens
//     before this proxy sees the request, via Server.authorizeIfConfigured.
//  2. Whatever Authorization header the client supplied is dropped and replaced
//     with the per-boot internal key. chatgpt2api never sees, accepts, or trusts
//     the user's CPA Management Key.
//  3. Two prefix conventions land here:
//       /openai/v1/...  -> /v1/...        (OpenAI-compatible image/chat API)
//       /v0/image/...   -> /...           (chatgpt2api admin: account pool etc.)
//     The first lets users point an OpenAI SDK at
//     base_url=http://host:18317/openai without disturbing the panel's own
//     /v1/models route. The second is reserved for the React panel's
//     "Image Pool" page; chatgpt2api's admin routes actually live under
//     /api/* internally, so we rewrite /v0/image/foo -> /api/foo.
type chatGPT2APIProxy struct {
	upstream    *url.URL
	internalKey string
	proxy       *httputil.ReverseProxy
}

// newChatGPT2APIProxy returns (nil, nil) when the proxy is intentionally
// disabled (empty upstreamURL). Returning nil-without-error keeps the rest of
// the server happy: handler short-circuits to 503, no integration required.
func newChatGPT2APIProxy(upstreamURL, internalKey string) (*chatGPT2APIProxy, error) {
	upstreamURL = strings.TrimSpace(upstreamURL)
	if upstreamURL == "" {
		return nil, nil
	}
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, errors.New("chatgpt2api upstream URL must include scheme and host")
	}

	p := &chatGPT2APIProxy{
		upstream:    u,
		internalKey: internalKey,
	}
	p.proxy = &httputil.ReverseProxy{
		Director:     p.director,
		ErrorHandler: p.errorHandler,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   2 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			// Image generation can take >30s; allow up to two minutes before
			// declaring upstream unresponsive.
			ResponseHeaderTimeout: 120 * time.Second,
		},
	}
	return p, nil
}

// stripChatGPT2APIPrefix rewrites the request path to what chatgpt2api expects.
// Returns (newPath, true) when one of our known prefixes matches, otherwise
// (path, false) so callers can decline routing.
//
//	/openai/v1/foo  ->  /v1/foo     (OpenAI-compatible routes live at /v1)
//	/v0/image/foo   ->  /api/foo    (chatgpt2api admin routes live under /api)
func stripChatGPT2APIPrefix(path string) (string, bool) {
	switch {
	case strings.HasPrefix(path, "/openai/"):
		return path[len("/openai"):], true
	case path == "/openai":
		return "/", true
	case strings.HasPrefix(path, "/v0/image/"):
		return "/api/" + path[len("/v0/image/"):], true
	case path == "/v0/image":
		return "/api", true
	}
	return path, false
}

func (p *chatGPT2APIProxy) director(req *http.Request) {
	if stripped, ok := stripChatGPT2APIPrefix(req.URL.Path); ok {
		req.URL.Path = stripped
	}
	req.URL.Scheme = p.upstream.Scheme
	req.URL.Host = p.upstream.Host
	req.Host = p.upstream.Host

	// Strip any client-supplied auth, then inject our internal token so
	// chatgpt2api accepts the request. Never forward the CPA Management Key.
	req.Header.Del("Authorization")
	if p.internalKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.internalKey)
	}
	// Drop cookies / proxy-auth that could otherwise carry user-side state we
	// don't want bridged into the in-process Python service.
	req.Header.Del("Cookie")
	req.Header.Del("Proxy-Authorization")
}

func (p *chatGPT2APIProxy) errorHandler(w http.ResponseWriter, _ *http.Request, _ error) {
	// Most common cause: chatgpt2api is still starting up, or has crashed.
	// Tell the client to retry shortly instead of buffering or hanging.
	w.Header().Set("Retry-After", "5")
	writeError(w, http.StatusServiceUnavailable, errors.New("chatgpt2api upstream not ready"))
}

// handleChatGPT2APIProxy is the HTTP entry point bound in the server mux.
// It enforces the CPA Management Key on the outside, then forwards.
func (s *Server) handleChatGPT2APIProxy(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeIfConfigured(w, r) {
		return
	}
	if s.imageProxy == nil {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, errors.New("chatgpt2api proxy disabled"))
		return
	}
	s.imageProxy.proxy.ServeHTTP(w, r)
}
