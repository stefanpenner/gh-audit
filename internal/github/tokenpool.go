package github

import (
	"context"
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
	ID            string
	Kind          TokenKind
	transport     http.RoundTripper
	rateRemaining atomic.Int64
	rateLimit     atomic.Int64
	rateResetAt   atomic.Int64 // unix timestamp
	inFlight      atomic.Int64
	scopes        []OrgScope
	disabled      atomic.Bool
	mu            sync.Mutex
}

// OrgScope defines the org/repo scope a token is authorized for.
type OrgScope struct {
	Org   string
	Repos []string // empty = all repos in org
}

// TokenPool manages multiple GitHub API tokens with rate-limit-aware selection.
//
// Tokens are selected by highest remaining quota. When a token's remaining
// requests drop below rateLimitThreshold, it is skipped until its reset time.
// The transport layer handles 429 and secondary rate limit responses with
// automatic retry and backoff.
type TokenPool struct {
	tokens []*ManagedToken
	mu     sync.RWMutex
	logger *slog.Logger
}

// NewTokenPool creates a new empty token pool.
func NewTokenPool(logger *slog.Logger) *TokenPool {
	return &TokenPool{
		logger: logger,
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
		if remaining >= rateLimitThreshold || resetAt <= now {
			s.Available++
		}
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

		// Skip exhausted tokens whose reset is still in the future.
		if remaining < rateLimitThreshold && resetAt > now {
			rt := time.Unix(resetAt, 0)
			if earliestReset.IsZero() || rt.Before(earliestReset) {
				earliestReset = rt
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

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	defer t.token.inFlight.Add(-1)

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	t.updateRateLimitHeaders(resp)

	switch resp.StatusCode {
	case 403:
		// Check for secondary rate limit / abuse detection.
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading 403 response body: %w", readErr)
		}
		bodyStr := strings.ToLower(string(body))
		if strings.Contains(bodyStr, "abuse") || strings.Contains(bodyStr, "secondary rate limit") {
			return t.retrySecondaryRateLimit(req, resp.Header.Get("Retry-After"), string(body))
		}
		// Re-wrap body for non-abuse 403s.
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return resp, nil

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

// retrySecondaryRateLimit retries a request up to 3 times with exponential backoff
// when GitHub's secondary (abuse) rate limit is triggered.
func (t *rateLimitTransport) retrySecondaryRateLimit(req *http.Request, retryAfterHeader, body string) (*http.Response, error) {
	baseDelay := parseRetryAfter(retryAfterHeader)
	if baseDelay > 10*time.Second {
		baseDelay = 10 * time.Second
	}

	for attempt := 1; attempt <= 3; attempt++ {
		delay := baseDelay * time.Duration(1<<(attempt-1)) // 1x, 2x, 4x
		if t.logger != nil {
			t.logger.Warn("secondary rate limit, retrying",
				"attempt", attempt,
				"delay", delay,
				"token", t.token.ID,
			)
		}
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}

		resp, err := t.base.RoundTrip(req)
		if err != nil {
			if attempt == 3 {
				return nil, err
			}
			continue
		}
		t.updateRateLimitHeaders(resp)
		if resp.StatusCode != 403 {
			return resp, nil
		}
		// Still 403 — check if it's still abuse-related
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading 403 response body on retry: %w", readErr)
		}
		bodyStr := strings.ToLower(string(respBody))
		if !strings.Contains(bodyStr, "abuse") && !strings.Contains(bodyStr, "secondary rate limit") {
			resp.Body = io.NopCloser(strings.NewReader(string(respBody)))
			return resp, nil
		}
		body = string(respBody)
	}
	return nil, &SecondaryRateLimitError{
		RetryAfter: baseDelay,
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
