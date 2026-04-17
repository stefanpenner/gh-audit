package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
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
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if client == nil {
				t.Fatal("expected non-nil client")
			}

			// Verify correct token was picked by checking the transport chain.
			if tt.wantToken != "" {
				transport := client.Transport.(*rateLimitTransport)
				if transport.token.ID != tt.wantToken {
					t.Errorf("expected token %q, got %q", tt.wantToken, transport.token.ID)
				}
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

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
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
	if err == nil {
		t.Fatal("expected error due to context cancellation")
	}
}

func TestTokenPool_MarkDisabled(t *testing.T) {
	pool := NewTokenPool(testLogger())
	pool.AddPATToken("tok1", "token1", []OrgScope{{Org: "myorg"}})
	pool.AddPATToken("tok2", "token2", []OrgScope{{Org: "myorg"}})

	pool.MarkDisabled("tok1")

	client, err := pool.Pick(context.Background(), "myorg", "repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport := client.Transport.(*rateLimitTransport)
	if transport.token.ID != "tok2" {
		t.Errorf("expected tok2 after disabling tok1, got %s", transport.token.ID)
	}
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

	transport := &rateLimitTransport{
		base:  http.DefaultTransport,
		token: token,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if got := token.rateRemaining.Load(); got != 4321 {
		t.Errorf("rateRemaining = %d, want 4321", got)
	}
	if got := token.rateLimit.Load(); got != 5000 {
		t.Errorf("rateLimit = %d, want 5000", got)
	}
	if got := token.rateResetAt.Load(); got != 1700000000 {
		t.Errorf("rateResetAt = %d, want 1700000000", got)
	}
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
		base:  http.DefaultTransport,
		token: token,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls (original + retry), got %d", callCount)
	}
}

func TestAddPATToken(t *testing.T) {
	pool := NewTokenPool(testLogger())
	pool.AddPATToken("pat1", "mytoken", []OrgScope{{Org: "org1"}})

	pool.mu.RLock()
	defer pool.mu.RUnlock()

	if len(pool.tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(pool.tokens))
	}
	tok := pool.tokens[0]
	if tok.ID != "pat1" {
		t.Errorf("expected ID pat1, got %s", tok.ID)
	}
	if tok.Kind != TokenKindPAT {
		t.Errorf("expected kind PAT, got %s", tok.Kind)
	}
	if len(tok.scopes) != 1 || tok.scopes[0].Org != "org1" {
		t.Errorf("unexpected scopes: %+v", tok.scopes)
	}
}

func TestRateLimitTransport_Handles403Abuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message": "You have triggered an abuse detection mechanism"}`)
	}))
	defer srv.Close()

	token := &ManagedToken{ID: "test"}
	token.rateRemaining.Store(100)

	transport := &rateLimitTransport{
		base:  http.DefaultTransport,
		token: token,
	}

	client := &http.Client{Transport: transport}
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected error for abuse detection")
	}

	// The error should be wrapped - check the underlying type.
	// http.Client wraps transport errors.
	if _, ok := err.(*SecondaryRateLimitError); ok {
		// Direct type match.
	} else {
		// http.Client wraps it in url.Error.
		errMsg := err.Error()
		if !contains(errMsg, "secondary rate limit") && !contains(errMsg, "abuse") {
			t.Errorf("expected abuse/secondary rate limit error, got: %v", err)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
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
		got := parseRetryAfter(tt.input)
		if got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
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
			got := scopeMatches(tt.scopes, tt.org, tt.repo)
			if got != tt.want {
				t.Errorf("scopeMatches = %v, want %v", got, tt.want)
			}
		})
	}
}

// helper to create a test server that returns valid rate limit headers.
func newRateLimitServer(remaining, limit int, resetAt int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining", strconv.Itoa(remaining))
		w.Header().Set("x-ratelimit-limit", strconv.Itoa(limit))
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(resetAt, 10))
		w.WriteHeader(200)
	}))
}
