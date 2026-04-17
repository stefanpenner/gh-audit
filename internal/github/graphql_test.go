package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// graphqlFixture builds a realistic GraphQL response for the given SHAs.
func graphqlFixture(shas []string, withPRs bool) map[string]any {
	repo := map[string]any{}

	for i, sha := range shas {
		alias := fmt.Sprintf("c%d", i)
		if !withPRs {
			repo[alias] = map[string]any{
				"oid": sha,
				"associatedPullRequests": map[string]any{
					"nodes": []any{},
				},
			}
			continue
		}

		repo[alias] = map[string]any{
			"oid": sha,
			"associatedPullRequests": map[string]any{
				"nodes": []any{
					map[string]any{
						"number":     100 + i,
						"title":      fmt.Sprintf("PR for %s", sha[:7]),
						"merged":     true,
						"mergeCommit": map[string]any{"oid": sha},
						"headRefOid": fmt.Sprintf("head%s", sha[:7]),
						"author":     map[string]any{"login": "author1"},
						"mergedAt":   "2024-01-15T10:00:00Z",
						"url":        fmt.Sprintf("https://github.com/testorg/repo/pull/%d", 100+i),
						"reviews": map[string]any{
							"nodes": []any{
								map[string]any{
									"databaseId":  int64(200 + i),
									"state":       "APPROVED",
									"author":      map[string]any{"login": "reviewer1"},
									"commit":      map[string]any{"oid": sha},
									"submittedAt": "2024-01-14T10:00:00Z",
									"url":         fmt.Sprintf("https://github.com/testorg/repo/pull/%d#pullrequestreview-%d", 100+i, 200+i),
								},
							},
						},
						"commits": map[string]any{
							"nodes": []any{
								map[string]any{
									"commit": map[string]any{
										"checkSuites": map[string]any{
											"nodes": []any{
												map[string]any{
													"checkRuns": map[string]any{
														"nodes": []any{
															map[string]any{
																"databaseId":  int64(300 + i),
																"name":        "ci/tests",
																"status":      "COMPLETED",
																"conclusion":  "SUCCESS",
																"completedAt": "2024-01-15T09:30:00Z",
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}
	}

	return map[string]any{
		"data": map[string]any{
			"repository": repo,
		},
	}
}

func TestEnrichCommits_SingleCommit(t *testing.T) {
	shas := []string{"abc1234567890abc1234567890abc1234567890ab"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(graphqlFixture(shas, true))
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	results, err := client.EnrichCommits(context.Background(), "testorg", "repo", shas)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Commit.SHA != shas[0] {
		t.Errorf("SHA = %q, want %q", r.Commit.SHA, shas[0])
	}
	if len(r.PRs) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(r.PRs))
	}
	if r.PRs[0].Number != 100 {
		t.Errorf("PR number = %d, want 100", r.PRs[0].Number)
	}
	if r.PRs[0].Title != "PR for abc1234" {
		t.Errorf("PR title = %q", r.PRs[0].Title)
	}
	if len(r.Reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(r.Reviews))
	}
	if r.Reviews[0].State != "APPROVED" {
		t.Errorf("review state = %q, want APPROVED", r.Reviews[0].State)
	}
	if r.Reviews[0].ReviewerLogin != "reviewer1" {
		t.Errorf("reviewer = %q, want reviewer1", r.Reviews[0].ReviewerLogin)
	}
	if len(r.CheckRuns) != 1 {
		t.Fatalf("expected 1 check run, got %d", len(r.CheckRuns))
	}
	if r.CheckRuns[0].CheckName != "ci/tests" {
		t.Errorf("check name = %q, want ci/tests", r.CheckRuns[0].CheckName)
	}
}

func TestEnrichCommits_BatchOf25SendsSingleQuery(t *testing.T) {
	shas := make([]string, 25)
	for i := range shas {
		shas[i] = fmt.Sprintf("%040x", i)
	}

	queryCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryCount++
		body, _ := io.ReadAll(r.Body)
		// Verify all 25 aliases are present.
		for i := range 25 {
			alias := fmt.Sprintf("c%d", i)
			if !strings.Contains(string(body), alias) {
				t.Errorf("query missing alias %s", alias)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(graphqlFixture(shas, true))
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	results, err := client.EnrichCommits(context.Background(), "testorg", "repo", shas)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if queryCount != 1 {
		t.Errorf("expected 1 GraphQL query, got %d", queryCount)
	}
	if len(results) != 25 {
		t.Errorf("expected 25 results, got %d", len(results))
	}
}

func TestEnrichCommits_50CommitsSends2Queries(t *testing.T) {
	shas := make([]string, 50)
	for i := range shas {
		shas[i] = fmt.Sprintf("%040x", i)
	}

	queryCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryCount++
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(body, &req)

		// Count aliases in the query to determine batch.
		query := req["query"]
		aliasCount := strings.Count(query, ": object(oid:")

		// Build response for this batch.
		batchStart := (queryCount - 1) * 25
		batchShas := shas[batchStart : batchStart+aliasCount]

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(graphqlFixture(batchShas, true))
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	results, err := client.EnrichCommits(context.Background(), "testorg", "repo", shas)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if queryCount != 2 {
		t.Errorf("expected 2 GraphQL queries, got %d", queryCount)
	}
	if len(results) != 50 {
		t.Errorf("expected 50 results, got %d", len(results))
	}
}

func TestEnrichCommits_NoPRs(t *testing.T) {
	shas := []string{"abc1234567890abc1234567890abc1234567890ab"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(graphqlFixture(shas, false))
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	results, err := client.EnrichCommits(context.Background(), "testorg", "repo", shas)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if len(results[0].PRs) != 0 {
		t.Errorf("expected 0 PRs, got %d", len(results[0].PRs))
	}
}

func TestEnrichCommits_MultiplePRs(t *testing.T) {
	sha := "abc1234567890abc1234567890abc1234567890ab"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"c0": map[string]any{
						"oid": sha,
						"associatedPullRequests": map[string]any{
							"nodes": []any{
								map[string]any{
									"number":     10,
									"title":      "First PR",
									"merged":     true,
									"mergeCommit": map[string]any{"oid": sha},
									"headRefOid": "head1",
									"author":     map[string]any{"login": "dev1"},
									"mergedAt":   "2024-01-15T10:00:00Z",
									"url":        "https://github.com/testorg/repo/pull/10",
									"reviews":    map[string]any{"nodes": []any{}},
									"commits":    map[string]any{"nodes": []any{}},
								},
								map[string]any{
									"number":     11,
									"title":      "Second PR",
									"merged":     true,
									"mergeCommit": map[string]any{"oid": sha},
									"headRefOid": "head2",
									"author":     map[string]any{"login": "dev2"},
									"mergedAt":   "2024-01-16T10:00:00Z",
									"url":        "https://github.com/testorg/repo/pull/11",
									"reviews":    map[string]any{"nodes": []any{}},
									"commits":    map[string]any{"nodes": []any{}},
								},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	results, err := client.EnrichCommits(context.Background(), "testorg", "repo", []string{sha})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results[0].PRs) != 2 {
		t.Errorf("expected 2 PRs, got %d", len(results[0].PRs))
	}
}

func TestEnrichCommits_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"errors": []map[string]any{
				{"message": "something went wrong"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	_, err := client.EnrichCommits(context.Background(), "testorg", "repo", []string{"abc123"})
	if err == nil {
		t.Fatal("expected error for GraphQL error response")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error should contain GraphQL message, got: %v", err)
	}
}

func TestEnrichCommits_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Close connection without response.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(500)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	_, err := client.EnrichCommits(context.Background(), "testorg", "repo", []string{"abc123"})
	if err == nil {
		t.Fatal("expected error for network failure")
	}
}

func TestEnrichCommits_ResponseParsing(t *testing.T) {
	sha := "abc1234567890abc1234567890abc1234567890ab"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"c0": map[string]any{
						"oid": sha,
						"associatedPullRequests": map[string]any{
							"nodes": []any{
								map[string]any{
									"number":      42,
									"title":       "Add feature X",
									"merged":      true,
									"mergeCommit":  map[string]any{"oid": "merge123"},
									"headRefOid":  "headabc",
									"author":      map[string]any{"login": "octocat"},
									"mergedAt":    "2024-03-20T15:30:00Z",
									"url":         "https://github.com/testorg/repo/pull/42",
									"reviews": map[string]any{
										"nodes": []any{
											map[string]any{
												"databaseId":  int64(999),
												"state":       "CHANGES_REQUESTED",
												"author":      map[string]any{"login": "critic"},
												"commit":      map[string]any{"oid": "rev123"},
												"submittedAt": "2024-03-19T10:00:00Z",
												"url":         "https://github.com/testorg/repo/pull/42#pullrequestreview-999",
											},
											map[string]any{
												"databaseId":  int64(1000),
												"state":       "APPROVED",
												"author":      map[string]any{"login": "approver"},
												"commit":      map[string]any{"oid": "rev456"},
												"submittedAt": "2024-03-20T12:00:00Z",
												"url":         "https://github.com/testorg/repo/pull/42#pullrequestreview-1000",
											},
										},
									},
									"commits": map[string]any{
										"nodes": []any{
											map[string]any{
												"commit": map[string]any{
													"checkSuites": map[string]any{
														"nodes": []any{
															map[string]any{
																"checkRuns": map[string]any{
																	"nodes": []any{
																		map[string]any{
																			"databaseId":  int64(501),
																			"name":        "lint",
																			"status":      "COMPLETED",
																			"conclusion":  "SUCCESS",
																			"completedAt": "2024-03-20T14:00:00Z",
																		},
																		map[string]any{
																			"databaseId":  int64(502),
																			"name":        "test",
																			"status":      "COMPLETED",
																			"conclusion":  "FAILURE",
																			"completedAt": "2024-03-20T14:30:00Z",
																		},
																	},
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	results, err := client.EnrichCommits(context.Background(), "testorg", "repo", []string{sha})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := results[0]

	// Verify PR fields.
	pr := r.PRs[0]
	if pr.Number != 42 {
		t.Errorf("PR.Number = %d, want 42", pr.Number)
	}
	if pr.Title != "Add feature X" {
		t.Errorf("PR.Title = %q", pr.Title)
	}
	if !pr.Merged {
		t.Error("PR.Merged should be true")
	}
	if pr.MergeCommitSHA != "merge123" {
		t.Errorf("PR.MergeCommitSHA = %q, want merge123", pr.MergeCommitSHA)
	}
	if pr.HeadSHA != "headabc" {
		t.Errorf("PR.HeadSHA = %q, want headabc", pr.HeadSHA)
	}
	if pr.AuthorLogin != "octocat" {
		t.Errorf("PR.AuthorLogin = %q, want octocat", pr.AuthorLogin)
	}
	if pr.Org != "testorg" || pr.Repo != "repo" {
		t.Errorf("PR org/repo = %s/%s", pr.Org, pr.Repo)
	}

	// Verify reviews.
	if len(r.Reviews) != 2 {
		t.Fatalf("expected 2 reviews, got %d", len(r.Reviews))
	}
	if r.Reviews[0].State != "CHANGES_REQUESTED" {
		t.Errorf("review[0].State = %q", r.Reviews[0].State)
	}
	if r.Reviews[1].State != "APPROVED" {
		t.Errorf("review[1].State = %q", r.Reviews[1].State)
	}
	if r.Reviews[1].ReviewerLogin != "approver" {
		t.Errorf("review[1].ReviewerLogin = %q", r.Reviews[1].ReviewerLogin)
	}

	// Verify check runs.
	if len(r.CheckRuns) != 2 {
		t.Fatalf("expected 2 check runs, got %d", len(r.CheckRuns))
	}
	if r.CheckRuns[0].CheckName != "lint" {
		t.Errorf("checkRun[0].CheckName = %q", r.CheckRuns[0].CheckName)
	}
	if r.CheckRuns[1].Conclusion != "FAILURE" {
		t.Errorf("checkRun[1].Conclusion = %q", r.CheckRuns[1].Conclusion)
	}
}

// mockGraphQLPool creates a pool whose transport rewrites the GraphQL endpoint
// to point at a test server.
func mockGraphQLPool(t *testing.T, serverURL string) *TokenPool {
	t.Helper()
	pool := NewTokenPool(testLogger())
	mt := &ManagedToken{
		ID:   "graphql-test-token",
		Kind: TokenKindPAT,
		transport: &graphqlOverrideTransport{
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

// graphqlOverrideTransport rewrites requests to a test server URL.
type graphqlOverrideTransport struct {
	base    http.RoundTripper
	baseURL string
}

func (t *graphqlOverrideTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = "http"
	req2.URL.Host = t.baseURL[len("http://"):]
	req2.URL.Path = "/graphql"
	return t.base.RoundTrip(req2)
}
