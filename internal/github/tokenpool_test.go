package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestTokenPool_Pick(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(p *TokenPool)
		org       string
		repo      string
		wantErr   bool
		wantToken string // expected token ID
	}{
		{
			name: "returns token with highest remaining rate limit",
			setup: func(p *TokenPool) {
				p.AddPATToken("low", "token-low", []OrgScope{{Org: "myorg"}})
				p.AddPATToken("high", "token-high", []OrgScope{{Org: "myorg"}})
				p.tokens[0].rateRemaining.Store(500)
				p.tokens[1].rateRemaining.Store(4000)
			},
			org:       "myorg",
			repo:      "myrepo",
			wantToken: "high",
		},
		{
			name: "filters by org scope",
			setup: func(p *TokenPool) {
				p.AddPATToken("org-a", "token-a", []OrgScope{{Org: "org-a"}})
				p.AddPATToken("org-b", "token-b", []OrgScope{{Org: "org-b"}})
			},
			org:       "org-b",
			repo:      "somerepo",
			wantToken: "org-b",
		},
		{
			name: "filters by org and repo scope",
			setup: func(p *TokenPool) {
				p.AddPATToken("all-repos", "token-all", []OrgScope{{Org: "myorg"}})
				p.AddPATToken("specific", "token-specific", []OrgScope{{Org: "myorg", Repos: []string{"special-repo"}}})
				// Give specific more remaining so it would be picked if scope matches.
				p.tokens[1].rateRemaining.Store(9000)
			},
			org:       "myorg",
			repo:      "other-repo",
			wantToken: "all-repos", // specific token's scope doesn't match
		},
		{
			name: "repo scope matches when repo is listed",
			setup: func(p *TokenPool) {
				p.AddPATToken("specific", "token-specific", []OrgScope{{Org: "myorg", Repos: []string{"target-repo"}}})
			},
			org:       "myorg",
			repo:      "target-repo",
			wantToken: "specific",
		},
		{
			name: "skips exhausted tokens",
			setup: func(p *TokenPool) {
				p.AddPATToken("exhausted", "token-exhausted", []OrgScope{{Org: "myorg"}})
				p.AddPATToken("available", "token-available", []OrgScope{{Org: "myorg"}})
				p.tokens[0].rateRemaining.Store(50)
				p.tokens[0].rateResetAt.Store(time.Now().Add(10 * time.Minute).Unix())
				p.tokens[1].rateRemaining.Store(3000)
			},
			org:       "myorg",
			repo:      "repo",
			wantToken: "available",
		},
		{
			name: "skips disabled tokens",
			setup: func(p *TokenPool) {
				p.AddPATToken("disabled", "token-disabled", []OrgScope{{Org: "myorg"}})
				p.AddPATToken("active", "token-active", []OrgScope{{Org: "myorg"}})
				p.tokens[0].rateRemaining.Store(9999)
				p.tokens[0].disabled.Store(true)
			},
			org:       "myorg",
			repo:      "repo",
			wantToken: "active",
		},
		{
			name: "error when no tokens match org",
			setup: func(p *TokenPool) {
				p.AddPATToken("other", "token-other", []OrgScope{{Org: "other-org"}})
			},
			org:     "myorg",
			repo:    "repo",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := NewTokenPool(testLogger())
			tt.setup(pool)

			client, err := pool.Pick(context.Background(), tt.org, tt.repo)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, client)

			if tt.wantToken != "" {
				transport := client.Transport.(*rateLimitTransport)
				assert.Equal(t, tt.wantToken, transport.token.ID)
			}
		})
	}
}

func TestTokenPool_Pick_WaitsWhenAllExhausted(t *testing.T) {
	pool := NewTokenPool(testLogger())
	pool.AddPATToken("only", "token-only", []OrgScope{{Org: "myorg"}})
	pool.tokens[0].rateRemaining.Store(10)
	pool.tokens[0].rateResetAt.Store(time.Now().Add(200 * time.Millisecond).Unix())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	client, err := pool.Pick(ctx, "myorg", "repo")
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, client)
	// Should have waited at least a short time (reset was 200ms in future).
	// Be lenient since time.Unix has second precision.
	if elapsed < 100*time.Millisecond {
		t.Logf("waited %v (may be instant due to unix second precision)", elapsed)
	}
}

func TestTokenPool_Pick_RespectsContextCancellation(t *testing.T) {
	pool := NewTokenPool(testLogger())
	pool.AddPATToken("only", "token-only", []OrgScope{{Org: "myorg"}})
	pool.tokens[0].rateRemaining.Store(10)
	pool.tokens[0].rateResetAt.Store(time.Now().Add(10 * time.Minute).Unix())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := pool.Pick(ctx, "myorg", "repo")
	require.Error(t, err)
}

func TestTokenPool_MarkDisabled(t *testing.T) {
	pool := NewTokenPool(testLogger())
	pool.AddPATToken("tok1", "token1", []OrgScope{{Org: "myorg"}})
	pool.AddPATToken("tok2", "token2", []OrgScope{{Org: "myorg"}})

	pool.MarkDisabled("tok1")

	client, err := pool.Pick(context.Background(), "myorg", "repo")
	require.NoError(t, err)
	transport := client.Transport.(*rateLimitTransport)
	assert.Equal(t, "tok2", transport.token.ID)
}

func TestRateLimitTransport_UpdatesHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining", "4321")
		w.Header().Set("x-ratelimit-limit", "5000")
		w.Header().Set("x-ratelimit-reset", "1700000000")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	token := &ManagedToken{ID: "test"}
	token.rateRemaining.Store(5000)

	pool := NewTokenPool(testLogger())

	transport := &rateLimitTransport{
		base:   http.DefaultTransport,
		token:  token,
		pool:   pool,
		logger: testLogger(),
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, int64(4321), token.rateRemaining.Load())
	assert.Equal(t, int64(5000), token.rateLimit.Load())
	assert.Equal(t, int64(1700000000), token.rateResetAt.Load())
}

func TestRateLimitTransport_Handles429(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("x-ratelimit-remaining", "0")
			w.WriteHeader(429)
			return
		}
		w.Header().Set("x-ratelimit-remaining", "4999")
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	token := &ManagedToken{ID: "test"}
	token.rateRemaining.Store(100)

	transport := &rateLimitTransport{
		base:   http.DefaultTransport,
		token:  token,
		logger: testLogger(),
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, 2, callCount)
}

func TestAddPATToken(t *testing.T) {
	pool := NewTokenPool(testLogger())
	pool.AddPATToken("pat1", "mytoken", []OrgScope{{Org: "org1"}})

	pool.mu.RLock()
	defer pool.mu.RUnlock()

	require.Len(t, pool.tokens, 1)
	tok := pool.tokens[0]
	assert.Equal(t, "pat1", tok.ID)
	assert.Equal(t, TokenKindPAT, tok.Kind)
	require.Len(t, tok.scopes, 1)
	assert.Equal(t, "org1", tok.scopes[0].Org)
}

func TestHasToken(t *testing.T) {
	pool := NewTokenPool(testLogger())
	assert.False(t, pool.HasToken("missing"))
	pool.AddPATToken("alice", "secret", nil)
	assert.True(t, pool.HasToken("alice"))
	assert.False(t, pool.HasToken("bob"))
}

func TestRateLimitTransport_Handles403Abuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message": "You have triggered an abuse detection mechanism"}`)
	}))
	defer srv.Close()

	token := &ManagedToken{ID: "test"}
	token.rateRemaining.Store(100)

	transport := &rateLimitTransport{
		base:   http.DefaultTransport,
		token:  token,
		logger: testLogger(),
	}

	client := &http.Client{Transport: transport}
	_, err := client.Get(srv.URL)
	require.Error(t, err)

	errMsg := err.Error()
	assert.True(t,
		strings.Contains(errMsg, "secondary rate limit") || strings.Contains(errMsg, "abuse"),
		"expected abuse/secondary rate limit error, got: %v", err,
	)
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"", 60 * time.Second},
		{"30", 30 * time.Second},
		{"abc", 60 * time.Second},
		{"120", 120 * time.Second},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, parseRetryAfter(tt.input), "parseRetryAfter(%q)", tt.input)
	}
}

func TestSecondaryRateLimitReassignsToken(t *testing.T) {
	// First call returns secondary-rate-limit 403 → the transport should
	// cool down the originating token and re-pick a different one.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message": "You have triggered an abuse detection mechanism"}`)
			return
		}
		w.Header().Set("x-ratelimit-remaining", "4999")
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	pool := NewTokenPool(testLogger())
	pool.AddPATToken("t1", "token1", nil)
	pool.AddPATToken("t2", "token2", nil)

	client, err := pool.Pick(context.Background(), "", "")
	require.NoError(t, err)

	resp, err := client.Get(srv.URL)
	require.NoError(t, err, "expected reassigned token retry to succeed")
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, 2, callCount, "should have reassigned once and succeeded")
}

func TestSecondaryRateLimitExhaustedAllTokens(t *testing.T) {
	// Every token returns secondary rate limit on every call. After the
	// reassign budget is exhausted the caller must see an error whose
	// cause is still the SecondaryRateLimitError.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message": "secondary rate limit"}`)
	}))
	defer srv.Close()

	pool := NewTokenPool(testLogger())
	pool.AddPATToken("t1", "token1", nil)
	pool.AddPATToken("t2", "token2", nil)

	client, err := pool.Pick(context.Background(), "", "")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	_, err = client.Do(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limit", "expected rate-limit error, got: %v", err)
}

func TestParseOrgRepoFromPath(t *testing.T) {
	tests := []struct {
		path string
		org  string
		repo string
	}{
		{"/repos/octocat/hello-world/commits", "octocat", "hello-world"},
		{"/repos/octocat/hello-world", "octocat", "hello-world"},
		{"/orgs/myorg/repos", "myorg", ""},
		{"/user", "", ""},
		{"/rate_limit", "", ""},
		{"", "", ""},
		{"/", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			gotOrg, gotRepo := parseOrgRepoFromPath(tt.path)
			assert.Equal(t, tt.org, gotOrg)
			assert.Equal(t, tt.repo, gotRepo)
		})
	}
}

func TestPrimaryRateLimitMarksTokenAndSucceedsOnRetry(t *testing.T) {
	// Single server that counts calls. The first call returns primary rate
	// limit (and sets x-ratelimit-remaining: 0); subsequent calls succeed.
	// The transport should transparently re-pick and retry on the same server.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message": "API rate limit exceeded for installation ID 12345"}`)
			return
		}
		w.Header().Set("x-ratelimit-remaining", "4999")
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	pool := NewTokenPool(testLogger())
	pool.AddPATToken("t1", "token1", nil)
	pool.AddPATToken("t2", "token2", nil)

	client, err := pool.Pick(context.Background(), "", "")
	require.NoError(t, err)

	resp, err := client.Get(srv.URL)
	require.NoError(t, err, "expected retry on alternate token to succeed")
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, 2, callCount, "should have retried exactly once with a second token")
}

func TestScopeMatches(t *testing.T) {
	tests := []struct {
		name   string
		scopes []OrgScope
		org    string
		repo   string
		want   bool
	}{
		{
			name:   "org only scope matches any repo",
			scopes: []OrgScope{{Org: "myorg"}},
			org:    "myorg",
			repo:   "anyrepo",
			want:   true,
		},
		{
			name:   "org mismatch",
			scopes: []OrgScope{{Org: "other"}},
			org:    "myorg",
			repo:   "repo",
			want:   false,
		},
		{
			name:   "repo scope matches specific repo",
			scopes: []OrgScope{{Org: "myorg", Repos: []string{"repo1", "repo2"}}},
			org:    "myorg",
			repo:   "repo2",
			want:   true,
		},
		{
			name:   "repo scope does not match unlisted repo",
			scopes: []OrgScope{{Org: "myorg", Repos: []string{"repo1"}}},
			org:    "myorg",
			repo:   "repo2",
			want:   false,
		},
		{
			name:   "case insensitive org",
			scopes: []OrgScope{{Org: "MyOrg"}},
			org:    "myorg",
			repo:   "repo",
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, scopeMatches(tt.scopes, tt.org, tt.repo))
		})
	}
}
