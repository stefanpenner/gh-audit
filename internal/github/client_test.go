package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v72/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTokenPool creates a TokenPool with a single PAT token whose transport
// points at the given test server URL.
func mockTokenPool(t *testing.T, serverURL string) *TokenPool {
	t.Helper()
	pool := NewTokenPool(testLogger())
	mt := &ManagedToken{
		ID:   "test-token",
		Kind: TokenKindPAT,
		transport: &overrideURLTransport{
			base:    http.DefaultTransport,
			baseURL: serverURL,
		},
		scopes: []OrgScope{{Org: "testorg"}},
	}
	mt.rateRemaining.Store(5000)
	mt.rateLimit.Store(5000)
	pool.tokens = append(pool.tokens, mt)
	return pool
}

// overrideURLTransport rewrites request URLs to point at a test server.
type overrideURLTransport struct {
	base    http.RoundTripper
	baseURL string
}

func (t *overrideURLTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = "http"
	req2.URL.Host = t.baseURL[len("http://"):]
	return t.base.RoundTrip(req2)
}

func TestListOrgRepos(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantCount int
		wantErr   bool
	}{
		{
			name: "returns all repos across multiple pages",
			handler: func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				if page == "" || page == "1" {
					repos := make([]map[string]any, 2)
					repos[0] = map[string]any{
						"name":           "repo1",
						"full_name":      "testorg/repo1",
						"default_branch": "main",
						"archived":       false,
					}
					repos[1] = map[string]any{
						"name":           "repo2",
						"full_name":      "testorg/repo2",
						"default_branch": "master",
						"archived":       false,
					}
					w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/testorg/repos?page=2>; rel="next"`, "http://"+r.Host))
					json.NewEncoder(w).Encode(repos)
				} else {
					repos := []map[string]any{
						{
							"name":           "repo3",
							"full_name":      "testorg/repo3",
							"default_branch": "main",
							"archived":       true,
						},
					}
					json.NewEncoder(w).Encode(repos)
				}
			},
			wantCount: 3,
		},
		{
			name: "includes archived repos",
			handler: func(w http.ResponseWriter, r *http.Request) {
				repos := []map[string]any{
					{
						"name":           "archived-repo",
						"full_name":      "testorg/archived-repo",
						"default_branch": "main",
						"archived":       true,
					},
				}
				json.NewEncoder(w).Encode(repos)
			},
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			pool := mockTokenPool(t, srv.URL)
			client := NewClient(pool, testLogger())

			repos, err := client.ListOrgRepos(context.Background(), "testorg")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, repos, tt.wantCount)
		})
	}
}

func TestListCommits(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantCount int
		wantErr   bool
	}{
		{
			name: "returns commits in date range",
			handler: func(w http.ResponseWriter, r *http.Request) {
				commits := []map[string]any{
					{
						"sha":      "abc123",
						"html_url": "https://github.com/testorg/repo/commit/abc123",
						"commit": map[string]any{
							"message": "fix: something",
							"author": map[string]any{
								"email": "dev@example.com",
								"date":  "2024-01-15T10:00:00Z",
							},
						},
						"author": map[string]any{
							"login": "developer",
						},
						"parents": []map[string]any{
							{"sha": "parent1"},
						},
					},
				}
				json.NewEncoder(w).Encode(commits)
			},
			wantCount: 1,
		},
		{
			name: "handles pagination",
			handler: func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				if page == "" || page == "1" {
					commits := []map[string]any{
						{
							"sha": "sha1",
							"commit": map[string]any{
								"message": "commit 1",
								"author": map[string]any{
									"email": "a@b.com",
									"date":  "2024-01-15T10:00:00Z",
								},
							},
							"author":  map[string]any{"login": "dev1"},
							"parents": []map[string]any{},
						},
					}
					w.Header().Set("Link", fmt.Sprintf(`<%s/repos/testorg/repo/commits?page=2>; rel="next"`, "http://"+r.Host))
					json.NewEncoder(w).Encode(commits)
				} else {
					commits := []map[string]any{
						{
							"sha": "sha2",
							"commit": map[string]any{
								"message": "commit 2",
								"author": map[string]any{
									"email": "b@c.com",
									"date":  "2024-01-16T10:00:00Z",
								},
							},
							"author":  map[string]any{"login": "dev2"},
							"parents": []map[string]any{},
						},
					}
					json.NewEncoder(w).Encode(commits)
				}
			},
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			pool := mockTokenPool(t, srv.URL)
			client := NewClient(pool, testLogger())

			since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			until := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

			commits, err := client.ListCommits(context.Background(), "testorg", "repo", "main", since, until)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, commits, tt.wantCount)
		})
	}
}

func TestGetCommitDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commit := map[string]any{
			"sha":      "abc123",
			"html_url": "https://github.com/testorg/repo/commit/abc123",
			"commit": map[string]any{
				"message": "feat: add feature",
				"author": map[string]any{
					"email": "dev@example.com",
					"date":  "2024-01-15T10:00:00Z",
				},
			},
			"author": map[string]any{
				"login": "developer",
			},
			"parents": []map[string]any{
				{"sha": "parent1"},
			},
			"stats": map[string]any{
				"additions": 42,
				"deletions": 13,
			},
		}
		json.NewEncoder(w).Encode(commit)
	}))
	defer srv.Close()

	pool := mockTokenPool(t, srv.URL)
	client := NewClient(pool, testLogger())

	commit, err := client.GetCommitDetail(context.Background(), "testorg", "repo", "abc123")
	require.NoError(t, err)

	assert.Equal(t, "abc123", commit.SHA)
	assert.Equal(t, 42, commit.Additions)
	assert.Equal(t, 13, commit.Deletions)
	assert.Equal(t, "developer", commit.AuthorLogin)
	assert.Equal(t, "feat: add feature", commit.Message)
	assert.Equal(t, 1, commit.ParentCount)
}

func TestListCommitsUsesCommitterDate(t *testing.T) {
	authorDate := "2024-01-15T10:00:00Z"
	committerDate := "2024-01-20T14:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commits := []map[string]any{
			{
				"sha":      "abc123",
				"html_url": "https://github.com/testorg/repo/commit/abc123",
				"commit": map[string]any{
					"message": "fix: something",
					"author": map[string]any{
						"email": "dev@example.com",
						"date":  authorDate,
					},
					"committer": map[string]any{
						"email": "merger@example.com",
						"date":  committerDate,
					},
				},
				"author":  map[string]any{"login": "developer"},
				"parents": []map[string]any{},
			},
		}
		json.NewEncoder(w).Encode(commits)
	}))
	defer srv.Close()

	pool := mockTokenPool(t, srv.URL)
	client := NewClient(pool, testLogger())

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	commits, err := client.ListCommits(context.Background(), "testorg", "repo", "main", since, until)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	wantTime, _ := time.Parse(time.RFC3339, committerDate)
	assert.True(t, commits[0].CommittedAt.Equal(wantTime), "CommittedAt = %v, want committer date %v (not author date %s)", commits[0].CommittedAt, wantTime, authorDate)
}

func TestGetCommitDetailUsesCommitterDate(t *testing.T) {
	authorDate := "2024-01-15T10:00:00Z"
	committerDate := "2024-01-20T14:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commit := map[string]any{
			"sha":      "abc123",
			"html_url": "https://github.com/testorg/repo/commit/abc123",
			"commit": map[string]any{
				"message": "feat: add feature",
				"author": map[string]any{
					"email": "dev@example.com",
					"date":  authorDate,
				},
				"committer": map[string]any{
					"email": "merger@example.com",
					"date":  committerDate,
				},
			},
			"author":    map[string]any{"login": "developer"},
			"committer": map[string]any{"login": "merger"},
			"parents":   []map[string]any{{"sha": "parent1"}},
			"stats":     map[string]any{"additions": 10, "deletions": 5},
		}
		json.NewEncoder(w).Encode(commit)
	}))
	defer srv.Close()

	pool := mockTokenPool(t, srv.URL)
	client := NewClient(pool, testLogger())

	commit, err := client.GetCommitDetail(context.Background(), "testorg", "repo", "abc123")
	require.NoError(t, err)
	wantTime, _ := time.Parse(time.RFC3339, committerDate)
	assert.True(t, commit.CommittedAt.Equal(wantTime), "CommittedAt = %v, want committer date %v (not author date %s)", commit.CommittedAt, wantTime, authorDate)
}

func TestGetRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo := map[string]any{
			"name":           "myrepo",
			"full_name":      "testorg/myrepo",
			"default_branch": "develop",
			"archived":       false,
		}
		json.NewEncoder(w).Encode(repo)
	}))
	defer srv.Close()

	pool := mockTokenPool(t, srv.URL)
	client := NewClient(pool, testLogger())

	info, err := client.GetRepo(context.Background(), "testorg", "myrepo")
	require.NoError(t, err)
	assert.Equal(t, "myrepo", info.Name)
	assert.Equal(t, "develop", info.DefaultBranch)
	assert.False(t, info.Archived)
}

func TestParseCoAuthors(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    int
		names   []string
		emails  []string
	}{
		{
			name:    "no co-authors",
			message: "feat: add feature\n\nSome description",
			want:    0,
		},
		{
			name:    "single co-author",
			message: "feat: add feature\n\nCo-authored-by: Jane Doe <jane@example.com>",
			want:    1,
			names:   []string{"Jane Doe"},
			emails:  []string{"jane@example.com"},
		},
		{
			name:    "multiple co-authors",
			message: "feat: add feature\n\nCo-authored-by: Jane Doe <jane@example.com>\nCo-authored-by: Bob Smith <bob@example.com>",
			want:    2,
			names:   []string{"Jane Doe", "Bob Smith"},
			emails:  []string{"jane@example.com", "bob@example.com"},
		},
		{
			name:    "case insensitive",
			message: "fix: bug\n\nco-authored-by: Alice <alice@test.com>",
			want:    1,
			names:   []string{"Alice"},
			emails:  []string{"alice@test.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCoAuthors(tt.message)
			require.Len(t, got, tt.want)
			for i, ca := range got {
				if i < len(tt.names) {
					assert.Equal(t, tt.names[i], ca.Name, "co-author[%d].Name", i)
				}
				if i < len(tt.emails) {
					assert.Equal(t, tt.emails[i], ca.Email, "co-author[%d].Email", i)
				}
			}
		})
	}
}

func TestListCommitPullRequests(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantCount int
		wantErr   bool
	}{
		{
			name: "merged_at null is skipped (not merged)",
			handler: func(w http.ResponseWriter, r *http.Request) {
				prs := []map[string]any{
					{
						"number":    42,
						"title":     "Open PR",
						"state":     "open",
						"merged_at": nil,
						"head":      map[string]any{"sha": "head123"},
						"html_url":  "https://github.com/testorg/repo/pull/42",
					},
				}
				json.NewEncoder(w).Encode(prs)
			},
			wantCount: 0,
		},
		{
			name: "merged_at set means PR is merged even if merged field is null",
			handler: func(w http.ResponseWriter, r *http.Request) {
				prs := []map[string]any{
					{
						"number":           99,
						"title":            "Merged PR",
						"state":            "closed",
						"merged_at":        "2026-04-10T12:00:00Z",
						"merge_commit_sha": "merge123",
						"head":             map[string]any{"sha": "head456"},
						"user":             map[string]any{"login": "author1"},
						"html_url":         "https://github.com/testorg/repo/pull/99",
					},
				}
				json.NewEncoder(w).Encode(prs)
			},
			wantCount: 1,
		},
		{
			name: "multiple PRs filters to only merged",
			handler: func(w http.ResponseWriter, r *http.Request) {
				prs := []map[string]any{
					{
						"number":           10,
						"title":            "Merged",
						"state":            "closed",
						"merged_at":        "2026-04-10T12:00:00Z",
						"merge_commit_sha": "m1",
						"head":             map[string]any{"sha": "h1"},
						"user":             map[string]any{"login": "dev"},
						"html_url":         "https://github.com/testorg/repo/pull/10",
					},
					{
						"number":    20,
						"title":     "Not merged",
						"state":     "closed",
						"merged_at": nil,
						"head":      map[string]any{"sha": "h2"},
						"html_url":  "https://github.com/testorg/repo/pull/20",
					},
					{
						"number":           30,
						"title":            "Also merged",
						"state":            "closed",
						"merged_at":        "2026-04-11T12:00:00Z",
						"merge_commit_sha": "m3",
						"head":             map[string]any{"sha": "h3"},
						"user":             map[string]any{"login": "dev2"},
						"html_url":         "https://github.com/testorg/repo/pull/30",
					},
				}
				json.NewEncoder(w).Encode(prs)
			},
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			pool := mockTokenPool(t, srv.URL)
			client := NewClient(pool, testLogger())

			prs, err := client.ListCommitPullRequests(context.Background(), "testorg", "repo", "abc123")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, prs, tt.wantCount)
			for _, pr := range prs {
				assert.True(t, pr.Merged)
				assert.False(t, pr.MergedAt.IsZero())
			}
		})
	}
}

func TestRateLimitTransport_Retries5xx(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 2 {
			w.WriteHeader(502)
			return
		}
		w.Header().Set("x-ratelimit-remaining", "4999")
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	token := &ManagedToken{ID: "test"}
	token.rateRemaining.Store(5000)

	transport := &rateLimitTransport{
		base:  &overrideURLTransport{base: http.DefaultTransport, baseURL: srv.URL},
		token: token,
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Get("http://api.github.com/test")
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, 3, callCount)
}

func TestRateLimitTransport_5xxExhaustsRetries(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(504)
	}))
	defer srv.Close()

	token := &ManagedToken{ID: "test"}
	token.rateRemaining.Store(5000)

	transport := &rateLimitTransport{
		base:  &overrideURLTransport{base: http.DefaultTransport, baseURL: srv.URL},
		token: token,
	}

	client := &http.Client{Transport: transport}
	_, err := client.Get("http://api.github.com/test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server error after 3 retries")
	assert.Equal(t, 4, callCount) // 1 initial + 3 retries
}

// Ensure go-github is used (compile check).
func TestGetPullRequest(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr := map[string]any{
			"number":           42,
			"title":            "Fix the thing",
			"merged":           true,
			"html_url":         "https://github.com/testorg/testrepo/pull/42",
			"merge_commit_sha": "mergeabc123",
			"head":             map[string]any{"sha": "headsha123"},
			"user":             map[string]any{"login": "author1"},
			"merged_by":        map[string]any{"login": "merger1"},
			"merged_at":        "2024-06-01T12:00:00Z",
		}
		json.NewEncoder(w).Encode(pr)
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	pool := mockTokenPool(t, server.URL)
	client := NewClient(pool, testLogger())

	pr, err := client.GetPullRequest(context.Background(), "testorg", "testrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, 42, pr.Number)
	assert.Equal(t, "Fix the thing", pr.Title)
	assert.True(t, pr.Merged)
	assert.Equal(t, "headsha123", pr.HeadSHA)
	assert.Equal(t, "author1", pr.AuthorLogin)
	assert.Equal(t, "merger1", pr.MergedByLogin)
	assert.Equal(t, "mergeabc123", pr.MergeCommitSHA)
}

var _ = (*gogithub.Client)(nil)
