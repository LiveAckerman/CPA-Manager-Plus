package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// /v1/images/* dual-auth: accept either the CPA Management Key (admin path,
// matches the rest of the panel) OR a CPA-issued client API key (so OpenAI
// SDKs and tools like sub2api configured with a regular sk-... key just work,
// the same way /v1/responses and /v1/chat/completions do via the transparent
// passthrough).
//
// Why this exists
//
// Phase 2.5 wired /v1/images/* through Server.authorizeIfConfigured, which
// only accepts the Management Key. That's fine for admin tools but breaks
// the natural deployment pattern: operators configure their downstream
// clients with a CPA client API key, point them at the panel's URL, and
// expect image gen to work without holding an admin token. With this layer
// in place, both keys work transparently.
//
// How CPA client keys are validated
//
// We don't have CPA's signing secret, so we can't verify keys offline.
// Instead we issue a one-shot GET to CPA's /v1/models endpoint with the
// client's Authorization header. CPA validates the key (200) or rejects it
// (401). A small in-memory TTL cache (default 5 min) deduplicates the
// validation so repeat requests with the same key skip the upstream round
// trip. Only positive results are cached; negatives always re-check so a
// newly-provisioned key works immediately and a revoked key starts failing
// within seconds (since chatgpt2api itself ends up doing the heavy lifting
// per request, the user-visible latency hit is one-time per ~5-minute
// window per key).
//
// Failure modes
//
// - Mgmt Key matches: accept immediately, no upstream call.
// - Auth header is a Bearer token, CPA says 200: accept; cache key.
// - Auth header is a Bearer token, CPA says 401/4xx: 401 to client.
// - Auth header missing/malformed: 401 to client.
// - CPA itself unreachable (network error, 5xx): 503 with Retry-After,
//   so clients don't burn their key budget on a transient outage.

const (
	apiKeyCacheTTL      = 5 * time.Minute
	apiKeyValidateTimeout = 8 * time.Second
)

// apiKeyValidator caches positive CPA validations of client API keys to avoid
// an extra upstream round trip on every image request. Safe for concurrent
// use; the underlying map is sync.Map.
type apiKeyValidator struct {
	httpClient *http.Client
	cache      sync.Map // key string -> time.Time expiresAt
	ttl        time.Duration
}

func newAPIKeyValidator() *apiKeyValidator {
	return &apiKeyValidator{
		httpClient: &http.Client{Timeout: apiKeyValidateTimeout},
		ttl:        apiKeyCacheTTL,
	}
}

// Validate asks CPA whether the given client API key is recognized. Returns
// (true, nil) for a valid key, (false, nil) for a confirmed-invalid one, and
// (false, err) when the validation itself couldn't complete (CPA unreachable,
// 5xx, etc.) — callers should distinguish the third case so they can respond
// 503 rather than 401.
//
// cpaBaseURL is taken per-call (rather than memoized in the validator) so
// that the panel picks up changes to setup.CPAUpstreamURL without needing
// the validator to be rebuilt.
func (v *apiKeyValidator) Validate(ctx context.Context, cpaBaseURL, key string) (bool, error) {
	if strings.TrimSpace(key) == "" {
		return false, nil
	}
	if exp, ok := v.cache.Load(key); ok {
		if expAt, ok2 := exp.(time.Time); ok2 && time.Now().Before(expAt) {
			return true, nil
		}
		// Expired entry — drop so we don't leak entries forever.
		v.cache.Delete(key)
	}

	cpaBaseURL = strings.TrimRight(strings.TrimSpace(cpaBaseURL), "/")
	if cpaBaseURL == "" {
		return false, errors.New("CPA upstream URL is not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cpaBaseURL+"/v1/models", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		v.cache.Store(key, time.Now().Add(v.ttl))
		return true, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// 401 / 403 / 404 → confirmed invalid. No cache.
		return false, nil
	default:
		// 5xx, redirects we didn't follow, anything else → treat as
		// "couldn't validate", let the caller respond 503.
		return false, fmt.Errorf("CPA returned status %d during key validation", resp.StatusCode)
	}
}

// authorizeImageGen runs the dual-auth check described at the top of this
// file. Returns true if the request should proceed; otherwise writes the
// appropriate error response and returns false.
//
// Open-mode preservation
//
// To match the historical behavior of authorizeIfConfigured (which this
// replaced), unconfigured deployments are still open: when there's no
// Setup row OR the row has no ManagementKey, the route accepts any request
// — same as before. The dual-auth flow only kicks in once the operator has
// completed the panel's setup wizard.
func (s *Server) authorizeImageGen(w http.ResponseWriter, r *http.Request) bool {
	setup, ok, err := s.resolveSetup(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return false
	}
	if !ok || setup.ManagementKey == "" {
		// Open mode — matches authorizeIfConfigured's pre-setup behavior so
		// fresh installs (and the existing image_gen_test fixtures that
		// don't save a setup row) keep working without an Authorization
		// header.
		return true
	}

	// Path 1: Management Key match — fast, no upstream call.
	if authMatches(r, setup.ManagementKey) {
		return true
	}

	// Path 2: validate as CPA client API key.
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		writeError(w, http.StatusUnauthorized, errors.New("missing or malformed Authorization header"))
		return false
	}
	key := strings.TrimSpace(auth[len(prefix):])

	if strings.TrimSpace(setup.CPAUpstreamURL) == "" {
		// No CPA upstream configured but Mgmt Key was — we have nothing to
		// validate the client key against. Reject with the same 401 shape
		// clients would see if their key were just plain wrong.
		writeError(w, http.StatusUnauthorized, errors.New("invalid api key"))
		return false
	}
	if s.apiKeyValidator == nil {
		// Shouldn't happen in normal operation (validator initialized in New)
		// but guard so a missed wiring doesn't NPE.
		writeError(w, http.StatusInternalServerError, errors.New("api key validator not initialized"))
		return false
	}

	valid, err := s.apiKeyValidator.Validate(r.Context(), setup.CPAUpstreamURL, key)
	if err != nil {
		// CPA unreachable / 5xx during validation — be honest with the
		// client: it's not their key, it's our upstream being flaky.
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable,
			fmt.Errorf("could not validate api key against CPA upstream: %w", err))
		return false
	}
	if !valid {
		writeError(w, http.StatusUnauthorized, errors.New("invalid api key"))
		return false
	}
	return true
}
