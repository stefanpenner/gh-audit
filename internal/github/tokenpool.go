package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

// TokenKind distinguishes personal access tokens from GitHub App installation tokens.
type TokenKind string

const (
	TokenKindPAT TokenKind = "pat"
	TokenKindApp TokenKind = "app"
)

// rateLimitThreshold is the minimum rateRemaining before a token is considered exhausted.
const rateLimitThreshold = 100

// ManagedToken tracks a single GitHub API token and its rate limit state.
//
// inFlight counts requests that have been assigned to this token by Pick but
// whose responses have not yet returned. It is subtracted from rateRemaining
// at selection time to prevent herding — if it were not tracked, N concurrent
// Pick calls issued before any response lands would all see the same
// rateRemaining and all select the same token.
type ManagedToken struct {
	ID                 string
	Kind               TokenKind
	transport          http.RoundTripper
	rateRemaining      atomic.Int64
	rateLimit          atomic.Int64
	rateResetAt        atomic.Int64 // unix timestamp (primary quota reset)
	abuseCooldownUntil atomic.Int64 // unix timestamp (set by secondary rate limit); Pick skips while > now
	inFlight           atomic.Int64
	scopes             []OrgScope
	disabled           atomic.Bool
	mu                 sync.Mutex
}

// OrgScope defines the org/repo scope a token is authorized for.
type OrgScope struct {
	Org   string
	Repos []string // empty = all repos in org
}

// defaultGlobalInFlightCap bounds concurrent in-flight HTTP requests across
// the entire pool. GitHub's secondary rate limit kicks in around 80
// concurrent requests per installation token; with a typical 6-token pool
// that's ~480 concurrent. We cap at 300 for a safety margin and to leave
// some headroom for bursts. Requests above the cap block briefly until a
// slot frees — much cheaper than the 3-32 minute retry storm that follows
// a triggered secondary rate limit.
const defaultGlobalInFlightCap = 300

// TokenPool manages multiple GitHub API tokens with rate-limit-aware selection.
//
// Tokens are selected by highest remaining quota. When a token's remaining
// requests drop below rateLimitThreshold, it is skipped until its reset time.
// The transport layer handles 429 and secondary rate limit responses with
// automatic retry and backoff.
//
// The pool also enforces a **global in-flight cap** (a counting semaphore)
// that throttles total concurrent HTTP requests across all tokens. This is
// the architectural guardrail that prevents pipeline-level concurrency
// explosions (sync.concurrency × enrich_concurrency × enrichCommitFanout ×
// enrichPRFanout can geometrically blow past GitHub's ~480 concurrent ceiling)
// from triggering secondary rate limits.
type TokenPool struct {
	tokens  []*ManagedToken
	mu      sync.RWMutex
	logger  *slog.Logger
	inFlight chan struct{} // counting semaphore: size = global cap

	// Event counters — atomically updated, surfaced through Snapshot for
	// telemetry. Let operators see "is this sweep slow because of primary
	// quota exhaustion, secondary abuse retries, or just volume?".
	secondaryRateLimitEvents atomic.Int64
	primaryRateLimitEvents   atomic.Int64
	tokenReassigns           atomic.Int64
}

// NewTokenPool creates a new empty token pool with the default global
// in-flight cap (300).
func NewTokenPool(logger *slog.Logger) *TokenPool {
	return NewTokenPoolWithCap(logger, defaultGlobalInFlightCap)
}

// NewTokenPoolWithCap creates a TokenPool with an explicit global in-flight
// cap. A cap of 0 or less disables global throttling (intended for tests).
func NewTokenPoolWithCap(logger *slog.Logger, globalInFlightCap int) *TokenPool {
	p := &TokenPool{
		logger: logger,
	}
	if globalInFlightCap > 0 {
		p.inFlight = make(chan struct{}, globalInFlightCap)
	}
	return p
}

// acquireInFlight blocks until a concurrency slot is available, honouring
// ctx cancellation. Returns nil if acquired, or ctx.Err() on cancel. A nil
// pool or disabled (cap=0) pool returns immediately with no slot held.
func (p *TokenPool) acquireInFlight(ctx context.Context) error {
	if p == nil || p.inFlight == nil {
		return nil
	}
	select {
	case p.inFlight <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseInFlight releases a slot previously acquired by acquireInFlight.
// Safe to call on a nil/disabled pool (no-op).
func (p *TokenPool) releaseInFlight() {
	if p == nil || p.inFlight == nil {
		return
	}
	select {
	case <-p.inFlight:
	default:
	}
}

// Len returns the number of tokens in the pool.
func (p *TokenPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.tokens)
}

// PoolSnapshot summarises the pool's current rate-limit budget.
type PoolSnapshot struct {
	Total     int
	Available int
	Remaining int64
	Capacity  int64

	// Cumulative event counts since pool creation. Useful for telemetry to
	// distinguish "slow because of primary quota" from "slow because of
	// secondary abuse" from "slow because of token reassign overhead".
	SecondaryRateLimitEvents int64
	PrimaryRateLimitEvents   int64
	TokenReassigns           int64
	InFlight                 int // current global in-flight count
}

// Snapshot returns a point-in-time view of remaining rate-limit budget
// across all tokens. Intended for lightweight telemetry (called every ~30s).
func (p *TokenPool) Snapshot() PoolSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var s PoolSnapshot
	s.Total = len(p.tokens)
	now := time.Now().Unix()
	for _, t := range p.tokens {
		if t.disabled.Load() {
			continue
		}
		remaining := t.rateRemaining.Load()
		s.Remaining += remaining
		s.Capacity += t.rateLimit.Load()
		resetAt := t.rateResetAt.Load()
		abuseUntil := t.abuseCooldownUntil.Load()
		primaryOK := remaining >= rateLimitThreshold || resetAt <= now
		abuseOK := abuseUntil <= now
		if primaryOK && abuseOK {
			s.Available++
		}
	}
	s.SecondaryRateLimitEvents = p.secondaryRateLimitEvents.Load()
	s.PrimaryRateLimitEvents = p.primaryRateLimitEvents.Load()
	s.TokenReassigns = p.tokenReassigns.Load()
	if p.inFlight != nil {
		s.InFlight = len(p.inFlight)
	}
	return s
}

// bearerTransport adds a Bearer authorization header to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req2)
}

// HasToken reports whether a token with the given ID is already registered.
// Used by config-reload paths to skip tokens that already exist without
// racing against Add*Token callers.
func (p *TokenPool) HasToken(id string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, t := range p.tokens {
		if t.ID == id {
			return true
		}
	}
	return false
}

// AddPATToken registers a personal access token.
func (p *TokenPool) AddPATToken(id, token string, scopes []OrgScope) {
	mt := &ManagedToken{
		ID:   id,
		Kind: TokenKindPAT,
		transport: &bearerTransport{
			token: token,
			base:  http.DefaultTransport,
		},
		scopes: scopes,
	}
	// Assume full rate limit initially.
	mt.rateRemaining.Store(5000)
	mt.rateLimit.Store(5000)

	p.mu.Lock()
	p.tokens = append(p.tokens, mt)
	p.mu.Unlock()
}

// AddAppToken registers a GitHub App installation token.
func (p *TokenPool) AddAppToken(id string, appID, installationID int64, privateKey []byte, scopes []OrgScope) error {
	tr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKey)
	if err != nil {
		return fmt.Errorf("creating app transport for %s: %w", id, err)
	}

	mt := &ManagedToken{
		ID:        id,
		Kind:      TokenKindApp,
		transport: tr,
		scopes:    scopes,
	}
	// App tokens typically have higher rate limits.
	mt.rateRemaining.Store(5000)
	mt.rateLimit.Store(5000)

	p.mu.Lock()
	p.tokens = append(p.tokens, mt)
	p.mu.Unlock()
	return nil
}

// scopeMatches checks if a token's scopes cover the given org/repo.
// A nil or empty scopes list matches all orgs/repos (wildcard).
func scopeMatches(scopes []OrgScope, org, repo string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		if !strings.EqualFold(s.Org, org) {
			continue
		}
		if len(s.Repos) == 0 {
			return true
		}
		for _, r := range s.Repos {
			if strings.EqualFold(r, repo) {
				return true
			}
		}
	}
	return false
}

// Pick selects the best available token for the given org/repo.
// It blocks if all tokens are exhausted, waiting until the earliest reset time.
func (p *TokenPool) Pick(ctx context.Context, org, repo string) (*http.Client, error) {
	for {
		client, waitUntil, err := p.tryPick(org, repo)
		if err != nil {
			return nil, err
		}
		if client != nil {
			return client, nil
		}

		// All tokens exhausted — wait until the earliest reset.
		delay := time.Until(waitUntil)
		if delay <= 0 {
			// Reset time already passed; retry immediately.
			continue
		}

		p.logger.Warn("all tokens exhausted, waiting for rate limit reset",
			"org", org,
			"repo", repo,
			"wait", delay,
			"reset_at", waitUntil,
		)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("context cancelled while waiting for rate limit reset: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// tryPick attempts to select a token. Returns (client, zero, nil) on success,
// (nil, resetTime, nil) if all exhausted, or (nil, zero, err) on hard failure.
//
// Selection scores each eligible token by (rateRemaining - inFlight) and picks
// the highest. Including inFlight prevents herding: if N Pick calls fire
// concurrently before any response returns, each call increments the chosen
// token's inFlight so subsequent peers see a reduced score for it and spread
// across the pool.
func (p *TokenPool) tryPick(org, repo string) (*http.Client, time.Time, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var best *ManagedToken
	var bestScore int64 = -1
	var earliestReset time.Time
	found := false

	now := time.Now().Unix()

	for _, t := range p.tokens {
		if t.disabled.Load() {
			continue
		}
		if !scopeMatches(t.scopes, org, repo) {
			continue
		}
		found = true

		remaining := t.rateRemaining.Load()
		resetAt := t.rateResetAt.Load()
		abuseUntil := t.abuseCooldownUntil.Load()

		// Skip exhausted tokens whose reset is still in the future.
		if remaining < rateLimitThreshold && resetAt > now {
			rt := time.Unix(resetAt, 0)
			if earliestReset.IsZero() || rt.Before(earliestReset) {
				earliestReset = rt
			}
			continue
		}
		// Skip tokens that recently tripped GitHub's abuse detector. The
		// cooldown is short (seconds) and resets automatically.
		if abuseUntil > now {
			at := time.Unix(abuseUntil, 0)
			if earliestReset.IsZero() || at.Before(earliestReset) {
				earliestReset = at
			}
			continue
		}

		score := remaining - t.inFlight.Load()
		if score > bestScore {
			best = t
			bestScore = score
		}
	}

	if !found {
		return nil, time.Time{}, fmt.Errorf("no tokens available for org=%s repo=%s", org, repo)
	}

	if best == nil {
		// All tokens exhausted.
		return nil, earliestReset, nil
	}

	// Reserve this request so concurrent Pick callers see reduced headroom on
	// this token. The transport decrements on RoundTrip completion.
	best.inFlight.Add(1)

	client := &http.Client{
		Transport: &rateLimitTransport{
			base:   best.transport,
			token:  best,
			pool:   p,
			logger: p.logger,
		},
	}
	return client, time.Time{}, nil
}

// MarkDisabled permanently removes a token from rotation (e.g., on 401).
func (p *TokenPool) MarkDisabled(id string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, t := range p.tokens {
		if t.ID == id {
			t.disabled.Store(true)
			p.logger.Warn("token disabled", "id", id)
			return
		}
	}
}

// rateLimitTransport wraps a base transport to update rate limit fields from response headers
// and handle 429/403 abuse detection responses.
type rateLimitTransport struct {
	base   http.RoundTripper
	token  *ManagedToken
	pool   *TokenPool
	logger *slog.Logger
}

// maxRateLimitReassigns bounds how many times RoundTrip will transparently
// re-pick a fresh token when a token reports primary or secondary rate limit.
// Each reassign calls pool.Pick which skips poisoned tokens. The ceiling is
// intentionally high because at an hour-boundary multiple tokens may report
// "just reset" from stale headers but still 403 on the first request; each
// 403 updates the token's resetAt forward and eventually all pool members
// get blacklisted, at which point Pick itself blocks on the earliest reset
// time. Without sufficient headroom here we bail prematurely mid-boundary.
const maxRateLimitReassigns = 30

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.doRoundTrip(req)
	if err == nil {
		return resp, nil
	}

	// Route both primary quota exhaustion and secondary (abuse) rate limits
	// through the same re-pick loop. In each case Pick skips the faulted
	// token (rateRemaining=0 for primary, abuseCooldownUntil>now for
	// secondary), or blocks until the earliest reset if all tokens are
	// faulted. This keeps callers unaware of transient pool health issues.
	var (
		primary   *PrimaryRateLimitError
		secondary *SecondaryRateLimitError
	)
	isPrimary := errors.As(err, &primary)
	isSecondary := errors.As(err, &secondary)
	if !isPrimary && !isSecondary {
		return nil, err
	}

	// Unit tests construct a transport without a pool; in production the
	// pool is always set. Without a pool we can't re-pick, so surface the
	// error as-is.
	if t.pool == nil {
		return nil, err
	}

	org, repo := parseOrgRepoFromPath(req.URL.Path)
	failedToken := t.token.ID
	for attempt := 1; attempt <= maxRateLimitReassigns; attempt++ {
		newClient, pickErr := t.pool.Pick(req.Context(), org, repo)
		if pickErr != nil {
			return nil, fmt.Errorf("rate limit on token %s; re-pick failed: %w", failedToken, pickErr)
		}
		newTransport, ok := newClient.Transport.(*rateLimitTransport)
		if !ok {
			return nil, fmt.Errorf("re-pick returned unexpected transport type %T", newClient.Transport)
		}
		t.pool.tokenReassigns.Add(1)

		t.logger.Warn("rate limit; retrying with different token",
			"failed_token", failedToken,
			"new_token", newTransport.token.ID,
			"attempt", attempt,
			"org", org,
			"repo", repo,
			"kind", rateLimitKind(isPrimary, isSecondary),
		)

		resp, err = newTransport.doRoundTrip(req)
		if err == nil {
			return resp, nil
		}
		isPrimary = errors.As(err, &primary)
		isSecondary = errors.As(err, &secondary)
		if !isPrimary && !isSecondary {
			return nil, err
		}
		failedToken = newTransport.token.ID
	}
	return nil, fmt.Errorf("rate limit exhausted across %d token reassigns: %w", maxRateLimitReassigns, err)
}

func rateLimitKind(primary, secondary bool) string {
	switch {
	case primary:
		return "primary"
	case secondary:
		return "secondary"
	default:
		return "unknown"
	}
}

// isInstallationTokenRefreshFailure matches the ghinstallation library's error
// message when it fails to mint an installation token via the `/app/installations/{id}/access_tokens`
// endpoint. That call uses the app's JWT and has its own rate limit, separate
// from the per-installation REST quota we track on each token. A failure here
// is recoverable by switching to another pool token whose underlying app
// isn't rate-limited.
func isInstallationTokenRefreshFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "could not refresh installation") ||
		strings.Contains(msg, "/app/installations/") && strings.Contains(msg, "access_tokens")
}

// parseOrgRepoFromPath extracts org and repo segments from a GitHub REST API
// request path. Returns empty strings when the path doesn't name a repo
// (e.g. /rate_limit, /user, ...).
func parseOrgRepoFromPath(path string) (org, repo string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		return parts[1], parts[2]
	}
	if len(parts) >= 2 && parts[0] == "orgs" {
		return parts[1], ""
	}
	return "", ""
}

func (t *rateLimitTransport) doRoundTrip(req *http.Request) (*http.Response, error) {
	defer t.token.inFlight.Add(-1)

	// Acquire a global in-flight slot. This throttles total concurrent HTTP
	// requests across the whole pool, preventing pipeline-level concurrency
	// explosions from blowing past GitHub's ~480-concurrent secondary rate
	// limit. Retries inside this RoundTrip (secondary-rate-limit backoff,
	// 5xx retries) reuse the same slot; no extra budget is taken.
	if err := t.pool.acquireInFlight(req.Context()); err != nil {
		return nil, err
	}
	defer t.pool.releaseInFlight()

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		// Installation-token refresh failures surface here before any
		// request is made. The ghinstallation library calls
		// `/app/installations/{id}/access_tokens`, and a 403 there usually
		// means a transient per-app JWT-level limit on GitHub's side. We
		// treat it like a primary rate limit: mark this token's quota
		// exhausted (with a short cooldown window) so Pick skips it, and
		// return SecondaryRateLimitError so the outer RoundTrip wrapper
		// re-picks a different token transparently. Without this the
		// ghinstallation error propagates up and kills the sweep.
		if isInstallationTokenRefreshFailure(err) {
			if t.pool != nil {
				t.pool.secondaryRateLimitEvents.Add(1)
			}
			// 10-minute cooldown is enough for GitHub's per-app JWT quota
			// to drain while other tokens absorb traffic.
			until := time.Now().Add(10 * time.Minute).Unix()
			for {
				cur := t.token.abuseCooldownUntil.Load()
				if cur >= until {
					break
				}
				if t.token.abuseCooldownUntil.CompareAndSwap(cur, until) {
					break
				}
			}
			if t.logger != nil {
				t.logger.Warn("installation token refresh failed — cooling down token",
					"token", t.token.ID,
					"cooldown", 10*time.Minute,
					"underlying", err,
				)
			}
			return nil, &SecondaryRateLimitError{
				RetryAfter: 10 * time.Minute,
				Message:    err.Error(),
			}
		}
		return nil, err
	}

	t.updateRateLimitHeaders(resp)

	switch resp.StatusCode {
	case 403:
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading 403 response body: %w", readErr)
		}
		bodyStr := strings.ToLower(string(body))

		// Secondary rate limit / abuse detection → exponential-backoff retry.
		// GitHub's abuse detector sometimes masquerades as a permission error
		// ("Resource not accessible by integration") under concentrated load,
		// so we route those through the same retry path when the token's
		// remaining primary budget is healthy (i.e. not a real 403 from
		// quota exhaustion). Real permission gaps surface after retries
		// are exhausted, taking ~minutes per request; acceptable because
		// a truly broken repo will hit the same wall repeatedly and can
		// be diagnosed quickly.
		if strings.Contains(bodyStr, "abuse") || strings.Contains(bodyStr, "secondary rate limit") {
			return t.markSecondaryRateLimit(resp.Header.Get("Retry-After"), string(body))
		}
		if strings.Contains(bodyStr, "resource not accessible by integration") &&
			t.token.rateRemaining.Load() > rateLimitThreshold {
			t.logger.Warn("403 resource-not-accessible under healthy budget; retrying as transient abuse",
				"token_id", t.token.ID,
				"url", req.URL.String(),
				"remaining", t.token.rateRemaining.Load(),
			)
			return t.markSecondaryRateLimit(resp.Header.Get("Retry-After"), string(body))
		}

		// Primary rate limit — GitHub's per-token 5000/hr quota exhausted.
		// The body says `API rate limit exceeded for installation ID <id>`.
		// The x-ratelimit-reset header we just processed tells us when the
		// budget refills; return a classified error so the caller (Pick
		// users) can observe the failure instead of a silent retry that
		// blocks for minutes.
		if strings.Contains(bodyStr, "api rate limit exceeded") {
			if t.pool != nil {
				t.pool.primaryRateLimitEvents.Add(1)
			}
			resetAt := time.Unix(t.token.rateResetAt.Load(), 0)
			return nil, &PrimaryRateLimitError{
				TokenID: t.token.ID,
				ResetAt: resetAt,
				Message: string(body),
			}
		}

		// Basic one-shot retry for other 403s. Covers transient permission
		// hiccups (e.g. token rotation) without slowing down hard failures.
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
		retry, retryErr := t.base.RoundTrip(req)
		if retryErr != nil {
			return nil, retryErr
		}
		t.updateRateLimitHeaders(retry)
		return retry, nil

	case 429:
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()

		// Sleep and retry once.
		timer := time.NewTimer(retryAfter)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}

		return t.base.RoundTrip(req)

	case 500, 502, 503, 504:
		resp.Body.Close()
		for attempt := 1; attempt <= 3; attempt++ {
			delay := time.Duration(attempt) * 2 * time.Second
			timer := time.NewTimer(delay)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
			retry, retryErr := t.base.RoundTrip(req)
			if retryErr != nil {
				if attempt == 3 {
					return nil, retryErr
				}
				continue
			}
			if retry.StatusCode < 500 {
				return retry, nil
			}
			retry.Body.Close()
		}
		return nil, fmt.Errorf("GET %s: server error after 3 retries", req.URL)
	}

	return resp, nil
}

// updateRateLimitHeaders reads GitHub rate limit headers and updates the token state.
func (t *rateLimitTransport) updateRateLimitHeaders(resp *http.Response) {
	if v := resp.Header.Get("x-ratelimit-remaining"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			t.token.rateRemaining.Store(n)
		}
	}
	if v := resp.Header.Get("x-ratelimit-limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			t.token.rateLimit.Store(n)
		}
	}
	if v := resp.Header.Get("x-ratelimit-reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			t.token.rateResetAt.Store(n)
		}
	}
}

// abuseCooldownBase is the initial cooldown when a token first hits GitHub's
// secondary rate limit / abuse detector. If the same token re-trips abuse
// within abuseRepeatWindow of the prior cooldown expiring, the cooldown
// escalates to abuseCooldownEscalated so we stop re-picking the poisoned
// token immediately and let other pool members (or the hourly primary
// reset) absorb traffic.
const (
	abuseCooldownBase       = 90 * time.Second
	abuseCooldownEscalated  = 15 * time.Minute
	abuseRepeatWindow       = 3 * time.Minute
)

// markSecondaryRateLimit records that GitHub's secondary rate limit / abuse
// detector has tripped on this token. Pick skips the token until the
// cooldown expires. Returns a SecondaryRateLimitError so the outer RoundTrip
// wrapper can re-pick a different token and retry the request transparently.
//
// We intentionally do NOT retry the same token in-place: the whole point of
// a multi-token pool is to route around poisoned tokens. Per-token
// exponential backoff within the transport starves the caller without
// helping, because other pool tokens are still healthy.
func (t *rateLimitTransport) markSecondaryRateLimit(retryAfterHeader, body string) (*http.Response, error) {
	if t.pool != nil {
		t.pool.secondaryRateLimitEvents.Add(1)
	}
	retryAfter := parseRetryAfter(retryAfterHeader)

	now := time.Now()
	prev := t.token.abuseCooldownUntil.Load()
	// Escalate if the previous cooldown expired (prev <= now) within the last
	// abuseRepeatWindow — the token is still poisoned, not just unlucky.
	cooldown := abuseCooldownBase
	if prev > 0 && prev <= now.Unix() && now.Unix()-prev < int64(abuseRepeatWindow.Seconds()) {
		cooldown = abuseCooldownEscalated
	}
	if retryAfter > cooldown {
		cooldown = retryAfter
	}

	until := now.Add(cooldown).Unix()
	// Never hold a cooldown past the token's hourly primary reset — at that
	// point the whole budget refills and abuse state typically also clears.
	if rr := t.token.rateResetAt.Load(); rr > 0 && rr < until {
		until = rr
	}
	for {
		cur := t.token.abuseCooldownUntil.Load()
		if cur >= until {
			break
		}
		if t.token.abuseCooldownUntil.CompareAndSwap(cur, until) {
			break
		}
	}

	if t.logger != nil {
		t.logger.Warn("secondary rate limit — cooling down token",
			"token", t.token.ID,
			"cooldown", cooldown,
			"retry_after", retryAfter,
			"until", time.Unix(until, 0),
		)
	}
	return nil, &SecondaryRateLimitError{
		RetryAfter: retryAfter,
		Message:    body,
	}
}

// SecondaryRateLimitError is returned when GitHub's abuse detection triggers.
type SecondaryRateLimitError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *SecondaryRateLimitError) Error() string {
	return fmt.Sprintf("secondary rate limit hit, retry after %s: %s", e.RetryAfter, e.Message)
}

// PrimaryRateLimitError is returned when a GitHub installation token hits
// its hourly REST quota (5000/hr for PAT, 15000/hr for App). The ResetAt
// field records when the quota will refill. The sweep should wait until at
// least that time before retrying against this token.
type PrimaryRateLimitError struct {
	TokenID string
	ResetAt time.Time
	Message string
}

func (e *PrimaryRateLimitError) Error() string {
	return fmt.Sprintf("primary rate limit exhausted on token %s (resets %s)",
		e.TokenID, e.ResetAt.Format(time.RFC3339))
}

// parseRetryAfter parses the Retry-After header value as seconds.
// Returns 60s as default if parsing fails.
func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 60 * time.Second
	}
	secs, err := strconv.Atoi(val)
	if err != nil {
		return 60 * time.Second
	}
	return time.Duration(secs) * time.Second
}
