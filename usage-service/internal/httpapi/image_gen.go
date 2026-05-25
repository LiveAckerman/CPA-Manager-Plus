package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Smart image-generation router that fronts /v1/images/generations and
// /v1/images/edits. Behavior:
//
//  1. Always try chatgpt2api first — that's where free GPT accounts live and
//     it's the only path that works for ChatGPT-web-only models like
//     gpt-image-2.
//  2. If chatgpt2api responds 5xx (or the connection fails altogether) AND the
//     operator has enabled fallback AND a CPA-issued client API key is
//     configured, retry the same request against the CPA upstream's
//     /v1/images/... endpoint. CPA is expected to route to a Plus/Pro account
//     that can call the OpenAI image API directly.
//  3. Client-side errors (4xx from chatgpt2api) are NOT retried — bad prompt,
//     unknown model, content policy, etc. would all fail the same way on CPA
//     and silent retry would just double the cost.
//
// Today's deployment has chatgpt2api only (the operator's CPA pool is free
// accounts that can't do image gen). The fallback path lies dormant: the
// flag is off by default and there's no key, so requests pass straight
// through to chatgpt2api as if this router didn't exist. When the operator
// later adds Plus/Pro accounts to CPA and signs a client API key, flipping
// IMAGE_CPA_FALLBACK_ENABLED=true + setting CPA_IMAGE_API_KEY activates the
// fallback without any client-side change.
//
// On every response we set X-Image-Resolved-Backend so callers can see which
// backend actually produced the image. On fallback we also set
// X-Image-Fallback-Trigger with the reason (e.g. status-503, upstream-unreachable).
// When the primary fails but fallback is unavailable, X-Image-Fallback-Skipped
// names the missing prerequisite (disabled, no-cpa-key, no-cpa-upstream).

const (
	// maxImageGenRequestBytes caps how much of an inbound /v1/images/* body
	// we'll buffer. We have to buffer in order to replay the request on
	// fallback. 32 MiB is generous: a 4K-resolution input PNG fits with
	// room to spare while still protecting the proxy from accidental
	// gigabyte uploads.
	maxImageGenRequestBytes = 32 * 1024 * 1024

	// imageGenRequestTimeout bounds the wall-clock time for a single
	// upstream attempt. Image generation is slow; 5 minutes accommodates
	// the worst case.
	imageGenRequestTimeout = 5 * time.Minute
)

// imageGenRouter owns the HTTP client shared by both backends. Kept as a
// struct so the handler doesn't allocate a client per request and so tests
// can inject a custom transport if they ever need to.
type imageGenRouter struct {
	httpClient *http.Client
}

func newImageGenRouter() *imageGenRouter {
	return &imageGenRouter{
		httpClient: &http.Client{Timeout: imageGenRequestTimeout},
	}
}

// handleImageGen is bound in server.go to /v1/images/generations and
// /v1/images/edits. It runs dual-auth (Management Key or CPA-issued client
// API key — see image_gen_auth.go), buffers the request body, and dispatches
// via the chatgpt2api-then-CPA decision tree.
func (s *Server) handleImageGen(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeImageGen(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, ok := readImageGenBodyWithCap(w, r)
	if !ok {
		return
	}
	s.dispatchImageGen(w, r, body)
}

func readImageGenBodyWithCap(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// Read one byte past the cap so we can detect oversize without first
	// reading the full body.
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxImageGenRequestBytes)+1))
	_ = r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, false
	}
	if len(body) > maxImageGenRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("request body exceeds %d-byte limit", maxImageGenRequestBytes))
		return nil, false
	}
	return body, true
}

func (s *Server) dispatchImageGen(w http.ResponseWriter, r *http.Request, body []byte) {
	chatResp, chatErr := s.forwardImageGenToChatGPT2API(r, body)
	chatStatus := 0
	if chatResp != nil {
		chatStatus = chatResp.StatusCode
		defer chatResp.Body.Close()
	}
	primaryFailed := chatErr != nil || shouldFallbackOnStatus(chatStatus)
	if !primaryFailed {
		// chatgpt2api succeeded (2xx) or returned a client-side error we
		// shouldn't second-guess (most 4xx). Pass through untouched.
		w.Header().Set("X-Image-Resolved-Backend", "chatgpt2api")
		copyImageGenResponse(w, chatResp)
		return
	}

	if reason := s.imageGenFallbackIneligibleReason(r.Context()); reason != "" {
		// Fallback unavailable. Surface chatgpt2api's verdict verbatim (or
		// synthesize 503 if it never even managed to respond), and explain
		// in a header why we didn't try CPA.
		w.Header().Set("X-Image-Resolved-Backend", "chatgpt2api")
		w.Header().Set("X-Image-Fallback-Skipped", reason)
		if chatErr != nil {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, chatErr)
			return
		}
		copyImageGenResponse(w, chatResp)
		return
	}

	// Retry on CPA. Always announce that this response came from the
	// fallback path so callers can attribute the cost / latency / model
	// difference correctly.
	cpaResp, cpaErr := s.forwardImageGenToCPA(r, body)
	w.Header().Set("X-Image-Resolved-Backend", "cpa")
	w.Header().Set("X-Image-Fallback-Trigger", fallbackTriggerLabel(chatStatus, chatErr))
	if cpaErr != nil {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, cpaErr)
		return
	}
	defer cpaResp.Body.Close()
	copyImageGenResponse(w, cpaResp)
}

// shouldFallbackOnStatus decides whether a given chatgpt2api response status
// should trigger a CPA retry. Two cases qualify:
//
//   - 5xx: chatgpt2api itself is broken (process down, internal error). CPA
//     is genuinely different infrastructure and worth trying.
//   - 429: rate-limit / quota exhaustion. In chatgpt2api this commonly
//     surfaces as the body `{"error":{"code":"insufficient_quota",...}}`,
//     which in practice means "no accounts available right now" (every
//     account in the pool is either rate-limited or absent). That's exactly
//     the "no chatgpt2api channel" condition where CPA's Plus/Pro path is
//     the intended fallback.
//
// All other 4xx — bad prompt, unknown model, content policy, etc. — pass
// through untouched: CPA would reject them for the same reason and silently
// retrying would just double the latency and cost.
func shouldFallbackOnStatus(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests
}

// fallbackTriggerLabel produces a short tag for the X-Image-Fallback-Trigger
// header explaining what made us reach for CPA.
func fallbackTriggerLabel(chatStatus int, chatErr error) string {
	if chatErr != nil {
		return "upstream-unreachable"
	}
	return fmt.Sprintf("status-%d", chatStatus)
}

// imageGenFallbackIneligibleReason returns the empty string when CPA fallback
// is good to fire, or a short reason tag otherwise. Order matters: the cheapest
// check (in-process bool) runs first; the DB read for setup runs last.
func (s *Server) imageGenFallbackIneligibleReason(ctx context.Context) string {
	if !s.cfg.ImageCPAFallbackEnabled {
		return "disabled"
	}
	if strings.TrimSpace(s.cfg.CPAImageAPIKey) == "" {
		return "no-cpa-key"
	}
	setup, ok, err := s.resolveSetup(ctx)
	if err != nil {
		return "setup-load-error"
	}
	if !ok || strings.TrimSpace(setup.CPAUpstreamURL) == "" {
		return "no-cpa-upstream"
	}
	return ""
}

func (s *Server) forwardImageGenToChatGPT2API(r *http.Request, body []byte) (*http.Response, error) {
	if s.imageProxy == nil {
		return nil, errors.New("chatgpt2api proxy not configured")
	}
	upstream := s.imageProxy.upstream.String() + r.URL.Path
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyForwardedHeaders(req.Header, r.Header)
	if s.imageProxy.internalKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.imageProxy.internalKey)
	}
	return s.imageGen.httpClient.Do(req)
}

func (s *Server) forwardImageGenToCPA(r *http.Request, body []byte) (*http.Response, error) {
	setup, _, err := s.resolveSetup(r.Context())
	if err != nil {
		return nil, err
	}
	upstream := strings.TrimRight(setup.CPAUpstreamURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyForwardedHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+s.cfg.CPAImageAPIKey)
	return s.imageGen.httpClient.Do(req)
}

// hopByHopHeaders are the headers RFC 7230 §6.1 forbids from crossing a
// proxy boundary. Plus "Host", which net/http sets correctly from the URL.
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"proxy-connection":    {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"host":                {},
}

// copyForwardedHeaders copies request headers from src into dst, omitting
// the Authorization header (we always set our own) and any hop-by-hop
// headers that should terminate at this proxy.
func copyForwardedHeaders(dst, src http.Header) {
	for k, v := range src {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		if _, hop := hopByHopHeaders[strings.ToLower(k)]; hop {
			continue
		}
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}

// copyImageGenResponse mirrors the upstream response headers + status + body
// into the client response, dropping hop-by-hop headers. It assumes the
// caller already set any X-Image-* informational headers before invoking.
func copyImageGenResponse(w http.ResponseWriter, src *http.Response) {
	for k, v := range src.Header {
		if _, hop := hopByHopHeaders[strings.ToLower(k)]; hop {
			continue
		}
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(src.StatusCode)
	_, _ = io.Copy(w, src.Body)
}
