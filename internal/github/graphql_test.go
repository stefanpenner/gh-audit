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
	return graphqlFixtureWithPageInfo(shas, withPRs, false, false, false)
}

func graphqlFixtureWithPageInfo(shas []string, withPRs bool, reviewsHasNext, checkRunsHasNext, checkSuitesHasNext bool) map[string]any {
	repo := map[string]any{}

	for i, sha := range shas {
		alias := fmt.Sprintf("c%d", i)
		if !withPRs {
			repo[alias] = map[string]any{
				"oid":       sha,
				"additions": 0,
				"deletions": 0,
				"associatedPullRequests": map[string]any{
					"nodes":    []any{},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			}
			continue
		}

		repo[alias] = map[string]any{
			"oid":       sha,
			"additions": 10 + i,
			"deletions": 5 + i,
			"associatedPullRequests": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false},
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
							"pageInfo": map[string]any{"hasNextPage": reviewsHasNext, "endCursor": "cursor123"},
						},
						"commits": map[string]any{
							"nodes": []any{
								map[string]any{
									"commit": map[string]any{
										"checkSuites": map[string]any{
											"pageInfo": map[string]any{"hasNextPage": checkSuitesHasNext},
											"nodes": []any{
												map[string]any{
													"pageInfo": map[string]any{"hasNextPage": false},
													"checkRuns": map[string]any{
														"pageInfo": map[string]any{"hasNextPage": checkRunsHasNext},
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
	if r.Commit.Additions != 10 {
		t.Errorf("Additions = %d, want 10", r.Commit.Additions)
	}
	if r.Commit.Deletions != 5 {
		t.Errorf("Deletions = %d, want 5", r.Commit.Deletions)
	}
}

func TestEnrichCommits_BatchOf5SendsSingleQuery(t *testing.T) {
	shas := make([]string, 5)
	for i := range shas {
		shas[i] = fmt.Sprintf("%040x", i)
	}

	queryCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryCount++
		body, _ := io.ReadAll(r.Body)
		for i := range 5 {
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
	if len(results) != 5 {
		t.Errorf("expected 5 results, got %d", len(results))
	}
}

func TestEnrichCommits_10CommitsSends2Queries(t *testing.T) {
	shas := make([]string, 10)
	for i := range shas {
		shas[i] = fmt.Sprintf("%040x", i)
	}

	queryCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryCount++
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(body, &req)

		query := req["query"]
		aliasCount := strings.Count(query, ": object(oid:")

		batchStart := (queryCount - 1) * graphQLBatchSize
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
	if len(results) != 10 {
		t.Errorf("expected 10 results, got %d", len(results))
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
						"oid":       sha,
						"additions": 42,
						"deletions": 13,
						"associatedPullRequests": map[string]any{
							"pageInfo": map[string]any{"hasNextPage": false},
							"nodes": []any{
								map[string]any{
									"number":      10,
									"title":       "First PR",
									"merged":      true,
									"mergeCommit":  map[string]any{"oid": sha},
									"headRefOid":  "head1",
									"author":      map[string]any{"login": "dev1"},
									"mergedAt":    "2024-01-15T10:00:00Z",
									"url":         "https://github.com/testorg/repo/pull/10",
									"reviews":     map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
									"commits":     map[string]any{"nodes": []any{}},
								},
								map[string]any{
									"number":      11,
									"title":       "Second PR",
									"merged":      true,
									"mergeCommit":  map[string]any{"oid": sha},
									"headRefOid":  "head2",
									"author":      map[string]any{"login": "dev2"},
									"mergedAt":    "2024-01-16T10:00:00Z",
									"url":         "https://github.com/testorg/repo/pull/11",
									"reviews":     map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
									"commits":     map[string]any{"nodes": []any{}},
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
						"oid":       sha,
						"additions": 100,
						"deletions": 25,
						"associatedPullRequests": map[string]any{
							"pageInfo": map[string]any{"hasNextPage": false},
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
										"pageInfo": map[string]any{"hasNextPage": false},
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
														"pageInfo": map[string]any{"hasNextPage": false},
														"nodes": []any{
															map[string]any{
																"pageInfo": map[string]any{"hasNextPage": false},
																"checkRuns": map[string]any{
																	"pageInfo": map[string]any{"hasNextPage": false},
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
	// Preserve path for REST API calls; rewrite to /graphql for GraphQL.
	if req.URL.Host == "api.github.com" && req.URL.Path == "/graphql" {
		req2.URL.Path = "/graphql"
	}
	// For REST API calls (e.g. /repos/...), preserve the original path.
	return t.base.RoundTrip(req2)
}

func TestEnrichCommits_Pagination(t *testing.T) {
	sha := "abc1234567890abc1234567890abc1234567890ab"

	tests := []struct {
		name              string
		reviewsHasNext    bool
		checkRunsHasNext  bool
		checkSuitesHasNext bool
		restReviews       []map[string]any // REST API reviews to return (nil = no REST handler)
		wantReviewCount   int
		wantCheckRunCount int
		wantRESTCalls     int
	}{
		{
			name:              "no pagination needed",
			reviewsHasNext:    false,
			checkRunsHasNext:  false,
			checkSuitesHasNext: false,
			wantReviewCount:   1,
			wantCheckRunCount: 1,
			wantRESTCalls:     0,
		},
		{
			name:             "reviews hasNextPage triggers REST fallback",
			reviewsHasNext:   true,
			checkRunsHasNext: false,
			restReviews: []map[string]any{
				{"id": 200, "state": "APPROVED", "user": map[string]any{"login": "reviewer1"}, "commit_id": sha, "submitted_at": "2024-01-14T10:00:00Z", "html_url": "https://github.com/testorg/repo/pull/100#pullrequestreview-200"},
				{"id": 201, "state": "CHANGES_REQUESTED", "user": map[string]any{"login": "reviewer2"}, "commit_id": sha, "submitted_at": "2024-01-14T11:00:00Z", "html_url": "https://github.com/testorg/repo/pull/100#pullrequestreview-201"},
				{"id": 202, "state": "APPROVED", "user": map[string]any{"login": "reviewer3"}, "commit_id": sha, "submitted_at": "2024-01-14T12:00:00Z", "html_url": "https://github.com/testorg/repo/pull/100#pullrequestreview-202"},
			},
			wantReviewCount:   3,
			wantCheckRunCount: 1,
			wantRESTCalls:     1,
		},
		{
			name:              "checkRuns hasNextPage logs warning only",
			reviewsHasNext:    false,
			checkRunsHasNext:  true,
			checkSuitesHasNext: false,
			wantReviewCount:   1,
			wantCheckRunCount: 1,
			wantRESTCalls:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restCalls := 0

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/graphql" {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(graphqlFixtureWithPageInfo(
						[]string{sha}, true,
						tt.reviewsHasNext, tt.checkRunsHasNext, tt.checkSuitesHasNext,
					))
					return
				}

				// REST API handler for reviews.
				if strings.Contains(r.URL.Path, "/reviews") {
					restCalls++
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(tt.restReviews)
					return
				}

				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			pool := mockGraphQLPool(t, srv.URL)
			client := NewGraphQLClient(pool, testLogger())

			results, err := client.EnrichCommits(context.Background(), "testorg", "repo", []string{sha})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(results))
			}

			r := results[0]
			if len(r.Reviews) != tt.wantReviewCount {
				t.Errorf("review count = %d, want %d", len(r.Reviews), tt.wantReviewCount)
			}
			if len(r.CheckRuns) != tt.wantCheckRunCount {
				t.Errorf("check run count = %d, want %d", len(r.CheckRuns), tt.wantCheckRunCount)
			}
			if restCalls != tt.wantRESTCalls {
				t.Errorf("REST calls = %d, want %d", restCalls, tt.wantRESTCalls)
			}
		})
	}
}

func TestEnrichCommits_ReviewsPaginationMultiplePages(t *testing.T) {
	sha := "abc1234567890abc1234567890abc1234567890ab"

	// Simulate 150 reviews: GraphQL returns first 100 (with hasNextPage=true),
	// REST returns all 150 across 2 pages.
	allRESTReviews := make([]map[string]any, 150)
	for i := range 150 {
		allRESTReviews[i] = map[string]any{
			"id":           int64(1000 + i),
			"state":        "APPROVED",
			"user":         map[string]any{"login": fmt.Sprintf("reviewer%d", i)},
			"commit_id":    sha,
			"submitted_at": "2024-01-14T10:00:00Z",
			"html_url":     fmt.Sprintf("https://github.com/testorg/repo/pull/100#pullrequestreview-%d", 1000+i),
		}
	}

	restCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(graphqlFixtureWithPageInfo(
				[]string{sha}, true, true, false, false,
			))
			return
		}

		// REST API handler for reviews with pagination.
		if strings.Contains(r.URL.Path, "/reviews") {
			restCalls++
			page := r.URL.Query().Get("page")
			w.Header().Set("Content-Type", "application/json")

			if page == "" || page == "1" {
				// Return first 100 with a Link header for page 2.
				nextURL := fmt.Sprintf("<%s%s?per_page=100&page=2>; rel=\"next\"", "http://"+r.Host, r.URL.Path)
				w.Header().Set("Link", nextURL)
				json.NewEncoder(w).Encode(allRESTReviews[:100])
			} else {
				// Return remaining 50 with no next link.
				json.NewEncoder(w).Encode(allRESTReviews[100:])
			}
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	pool := mockGraphQLPool(t, srv.URL)
	client := NewGraphQLClient(pool, testLogger())

	results, err := client.EnrichCommits(context.Background(), "testorg", "repo", []string{sha})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r := results[0]
	if len(r.Reviews) != 150 {
		t.Errorf("review count = %d, want 150", len(r.Reviews))
	}
	if restCalls != 2 {
		t.Errorf("REST calls = %d, want 2", restCalls)
	}

	// Verify first and last review data.
	if r.Reviews[0].ReviewID != 1000 {
		t.Errorf("first review ID = %d, want 1000", r.Reviews[0].ReviewID)
	}
	if r.Reviews[149].ReviewID != 1149 {
		t.Errorf("last review ID = %d, want 1149", r.Reviews[149].ReviewID)
	}
	if r.Reviews[0].ReviewerLogin != "reviewer0" {
		t.Errorf("first reviewer = %q, want reviewer0", r.Reviews[0].ReviewerLogin)
	}
}
