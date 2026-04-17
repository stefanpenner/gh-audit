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
type ManagedToken struct {
	ID            string
	Kind          TokenKind
	transport     http.RoundTripper
	rateRemaining atomic.Int64
	rateLimit     atomic.Int64
	rateResetAt   atomic.Int64 // unix timestamp
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
	defer p.mu.Unlock()
	p.tokens = append(p.tokens, mt)
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
	defer p.mu.Unlock()
	p.tokens = append(p.tokens, mt)
	return nil
}

// scopeMatches checks if a token's scopes cover the given org/repo.
func scopeMatches(scopes []OrgScope, org, repo string) bool {
	for _, s := range scopes {
		if !strings.EqualFold(s.Org, org) {
			continue
		}
		// Empty repos list means all repos in org.
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
func (p *TokenPool) tryPick(org, repo string) (*http.Client, time.Time, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var best *ManagedToken
	var bestRemaining int64 = -1
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

		if remaining > bestRemaining {
			best = t
			bestRemaining = remaining
		}
	}

	if !found {
		return nil, time.Time{}, fmt.Errorf("no tokens available for org=%s repo=%s", org, repo)
	}

	if best == nil {
		// All tokens exhausted.
		return nil, earliestReset, nil
	}

	client := &http.Client{
		Transport: &rateLimitTransport{
			base:  best.transport,
			token: best,
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
	base  http.RoundTripper
	token *ManagedToken
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// Update rate limit fields from headers.
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

	switch resp.StatusCode {
	case 403:
		// Check for secondary rate limit / abuse detection.
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return resp, nil
		}
		bodyStr := strings.ToLower(string(body))
		if strings.Contains(bodyStr, "abuse") || strings.Contains(bodyStr, "secondary rate limit") {
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			return nil, &SecondaryRateLimitError{
				RetryAfter: retryAfter,
				Message:    string(body),
			}
		}
		// Re-wrap body for non-abuse 403s.
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return resp, nil

	case 429:
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

		// Sleep and retry once.
		timer := time.NewTimer(retryAfter)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return resp, nil
		case <-timer.C:
		}
		resp.Body.Close()

		return t.base.RoundTrip(req)
	}

	return resp, nil
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
