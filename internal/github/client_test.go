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
					repos := make([]map[string]interface{}, 2)
					repos[0] = map[string]interface{}{
						"name":           "repo1",
						"full_name":      "testorg/repo1",
						"default_branch": "main",
						"archived":       false,
					}
					repos[1] = map[string]interface{}{
						"name":           "repo2",
						"full_name":      "testorg/repo2",
						"default_branch": "master",
						"archived":       false,
					}
					w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/testorg/repos?page=2>; rel="next"`, "http://"+r.Host))
					json.NewEncoder(w).Encode(repos)
				} else {
					repos := []map[string]interface{}{
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
				repos := []map[string]interface{}{
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
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(repos) != tt.wantCount {
				t.Errorf("got %d repos, want %d", len(repos), tt.wantCount)
			}
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
				commits := []map[string]interface{}{
					{
						"sha":      "abc123",
						"html_url": "https://github.com/testorg/repo/commit/abc123",
						"commit": map[string]interface{}{
							"message": "fix: something",
							"author": map[string]interface{}{
								"email": "dev@example.com",
								"date":  "2024-01-15T10:00:00Z",
							},
						},
						"author": map[string]interface{}{
							"login": "developer",
						},
						"parents": []map[string]interface{}{
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
					commits := []map[string]interface{}{
						{
							"sha": "sha1",
							"commit": map[string]interface{}{
								"message": "commit 1",
								"author": map[string]interface{}{
									"email": "a@b.com",
									"date":  "2024-01-15T10:00:00Z",
								},
							},
							"author":  map[string]interface{}{"login": "dev1"},
							"parents": []map[string]interface{}{},
						},
					}
					w.Header().Set("Link", fmt.Sprintf(`<%s/repos/testorg/repo/commits?page=2>; rel="next"`, "http://"+r.Host))
					json.NewEncoder(w).Encode(commits)
				} else {
					commits := []map[string]interface{}{
						{
							"sha": "sha2",
							"commit": map[string]interface{}{
								"message": "commit 2",
								"author": map[string]interface{}{
									"email": "b@c.com",
									"date":  "2024-01-16T10:00:00Z",
								},
							},
							"author":  map[string]interface{}{"login": "dev2"},
							"parents": []map[string]interface{}{},
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
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(commits) != tt.wantCount {
				t.Errorf("got %d commits, want %d", len(commits), tt.wantCount)
			}
		})
	}
}

func TestGetCommitDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commit := map[string]interface{}{
			"sha":      "abc123",
			"html_url": "https://github.com/testorg/repo/commit/abc123",
			"commit": map[string]interface{}{
				"message": "feat: add feature",
				"author": map[string]interface{}{
					"email": "dev@example.com",
					"date":  "2024-01-15T10:00:00Z",
				},
			},
			"author": map[string]interface{}{
				"login": "developer",
			},
			"parents": []map[string]interface{}{
				{"sha": "parent1"},
			},
			"stats": map[string]interface{}{
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if commit.SHA != "abc123" {
		t.Errorf("SHA = %q, want abc123", commit.SHA)
	}
	if commit.Additions != 42 {
		t.Errorf("Additions = %d, want 42", commit.Additions)
	}
	if commit.Deletions != 13 {
		t.Errorf("Deletions = %d, want 13", commit.Deletions)
	}
	if commit.AuthorLogin != "developer" {
		t.Errorf("AuthorLogin = %q, want developer", commit.AuthorLogin)
	}
	if commit.Message != "feat: add feature" {
		t.Errorf("Message = %q, want 'feat: add feature'", commit.Message)
	}
	if commit.ParentCount != 1 {
		t.Errorf("ParentCount = %d, want 1", commit.ParentCount)
	}
}

// Ensure go-github is used (compile check).
var _ = (*gogithub.Client)(nil)
