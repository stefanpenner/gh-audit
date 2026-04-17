package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

const (
	graphQLEndpoint    = "https://api.github.com/graphql"
	graphQLBatchSize   = 25
)

// GraphQLClient provides batched GraphQL enrichment of commits.
type GraphQLClient struct {
	pool   *TokenPool
	logger *slog.Logger
}

// NewGraphQLClient creates a new GraphQL client.
func NewGraphQLClient(pool *TokenPool, logger *slog.Logger) *GraphQLClient {
	return &GraphQLClient{
		pool:   pool,
		logger: logger,
	}
}

// EnrichCommits fetches PR, review, and check run data for a batch of commit SHAs.
// SHAs are batched into groups of 25 per GraphQL query to stay under complexity limits.
func (c *GraphQLClient) EnrichCommits(ctx context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	var results []model.EnrichmentResult

	for i := 0; i < len(shas); i += graphQLBatchSize {
		end := min(i+graphQLBatchSize, len(shas))
		batch := shas[i:end]

		batchResults, err := c.enrichBatch(ctx, org, repo, batch)
		if err != nil {
			return nil, fmt.Errorf("enriching batch starting at index %d: %w", i, err)
		}
		results = append(results, batchResults...)
	}

	return results, nil
}

func (c *GraphQLClient) enrichBatch(ctx context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	query := buildBatchQuery(org, repo, shas)

	reqBody, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("marshaling graphql request: %w", err)
	}

	httpClient, err := c.pool.Pick(ctx, org, repo)
	if err != nil {
		return nil, fmt.Errorf("picking token for graphql: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", graphQLEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("creating graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing graphql request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading graphql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql request returned status %d: %s", resp.StatusCode, string(body))
	}

	return parseGraphQLResponse(body, org, repo, shas)
}

func buildBatchQuery(org, repo string, shas []string) string {
	var buf bytes.Buffer
	buf.WriteString("query {\n")
	buf.WriteString(fmt.Sprintf("  repository(owner: %q, name: %q) {\n", org, repo))

	for i, sha := range shas {
		buf.WriteString(fmt.Sprintf("    c%d: object(oid: %q) {\n", i, sha))
		buf.WriteString("      ... on Commit {\n")
		buf.WriteString("        oid\n")
		buf.WriteString("        associatedPullRequests(first: 5, states: MERGED) {\n")
		buf.WriteString("          nodes {\n")
		buf.WriteString("            number\n")
		buf.WriteString("            title\n")
		buf.WriteString("            merged\n")
		buf.WriteString("            mergeCommit { oid }\n")
		buf.WriteString("            headRefOid\n")
		buf.WriteString("            author { login }\n")
		buf.WriteString("            mergedAt\n")
		buf.WriteString("            url\n")
		buf.WriteString("            reviews(first: 30) {\n")
		buf.WriteString("              nodes {\n")
		buf.WriteString("                databaseId\n")
		buf.WriteString("                state\n")
		buf.WriteString("                author { login }\n")
		buf.WriteString("                commit { oid }\n")
		buf.WriteString("                submittedAt\n")
		buf.WriteString("                url\n")
		buf.WriteString("              }\n")
		buf.WriteString("            }\n")
		buf.WriteString("            commits(last: 1) {\n")
		buf.WriteString("              nodes {\n")
		buf.WriteString("                commit {\n")
		buf.WriteString("                  checkSuites(first: 10) {\n")
		buf.WriteString("                    nodes {\n")
		buf.WriteString("                      checkRuns(first: 50) {\n")
		buf.WriteString("                        nodes {\n")
		buf.WriteString("                          databaseId\n")
		buf.WriteString("                          name\n")
		buf.WriteString("                          status\n")
		buf.WriteString("                          conclusion\n")
		buf.WriteString("                          completedAt\n")
		buf.WriteString("                        }\n")
		buf.WriteString("                      }\n")
		buf.WriteString("                    }\n")
		buf.WriteString("                  }\n")
		buf.WriteString("                }\n")
		buf.WriteString("              }\n")
		buf.WriteString("            }\n")
		buf.WriteString("          }\n")
		buf.WriteString("        }\n")
		buf.WriteString("      }\n")
		buf.WriteString("    }\n")
	}

	buf.WriteString("  }\n")
	buf.WriteString("}\n")
	return buf.String()
}

// graphqlResponse is the top-level GraphQL response structure.
type graphqlResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []graphqlError             `json:"errors"`
}

type graphqlError struct {
	Message string `json:"message"`
}

func parseGraphQLResponse(body []byte, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	var resp graphqlResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshaling graphql response: %w", err)
	}

	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}

	// Extract the repository object.
	repoData, ok := resp.Data["repository"]
	if !ok {
		return nil, fmt.Errorf("graphql response missing 'repository' field")
	}

	var repoObj map[string]json.RawMessage
	if err := json.Unmarshal(repoData, &repoObj); err != nil {
		return nil, fmt.Errorf("unmarshaling repository data: %w", err)
	}

	results := make([]model.EnrichmentResult, 0, len(shas))

	for i, sha := range shas {
		alias := fmt.Sprintf("c%d", i)
		result := model.EnrichmentResult{
			Commit: model.Commit{
				Org:  org,
				Repo: repo,
				SHA:  sha,
			},
		}

		commitData, ok := repoObj[alias]
		if !ok || string(commitData) == "null" {
			// Commit not found — return with empty enrichment.
			results = append(results, result)
			continue
		}

		prs, reviews, checkRuns, err := parseCommitObject(commitData, org, repo)
		if err != nil {
			return nil, fmt.Errorf("parsing commit %s: %w", sha, err)
		}

		result.PRs = prs
		result.Reviews = reviews
		result.CheckRuns = checkRuns
		results = append(results, result)
	}

	return results, nil
}

func parseCommitObject(data json.RawMessage, org, repo string) ([]model.PullRequest, []model.Review, []model.CheckRun, error) {
	var obj struct {
		AssociatedPullRequests struct {
			Nodes []json.RawMessage `json:"nodes"`
		} `json:"associatedPullRequests"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, nil, nil, fmt.Errorf("unmarshaling commit object: %w", err)
	}

	var prs []model.PullRequest
	var reviews []model.Review
	var checkRuns []model.CheckRun

	for _, prRaw := range obj.AssociatedPullRequests.Nodes {
		pr, prReviews, prCheckRuns, err := parsePRNode(prRaw, org, repo)
		if err != nil {
			return nil, nil, nil, err
		}
		prs = append(prs, pr)
		reviews = append(reviews, prReviews...)
		checkRuns = append(checkRuns, prCheckRuns...)
	}

	if prs == nil {
		prs = []model.PullRequest{}
	}
	if reviews == nil {
		reviews = []model.Review{}
	}
	if checkRuns == nil {
		checkRuns = []model.CheckRun{}
	}

	return prs, reviews, checkRuns, nil
}

type gqlPRNode struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Merged      bool   `json:"merged"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	HeadRefOid string `json:"headRefOid"`
	Author     *struct {
		Login string `json:"login"`
	} `json:"author"`
	MergedAt string `json:"mergedAt"`
	URL      string `json:"url"`
	Reviews  struct {
		Nodes []gqlReviewNode `json:"nodes"`
	} `json:"reviews"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				CheckSuites struct {
					Nodes []struct {
						CheckRuns struct {
							Nodes []gqlCheckRunNode `json:"nodes"`
						} `json:"checkRuns"`
					} `json:"nodes"`
				} `json:"checkSuites"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type gqlReviewNode struct {
	DatabaseID int64  `json:"databaseId"`
	State      string `json:"state"`
	Author     *struct {
		Login string `json:"login"`
	} `json:"author"`
	Commit *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	SubmittedAt string `json:"submittedAt"`
	URL         string `json:"url"`
}

type gqlCheckRunNode struct {
	DatabaseID  int64  `json:"databaseId"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completedAt"`
}

func parsePRNode(data json.RawMessage, org, repo string) (model.PullRequest, []model.Review, []model.CheckRun, error) {
	var node gqlPRNode
	if err := json.Unmarshal(data, &node); err != nil {
		return model.PullRequest{}, nil, nil, fmt.Errorf("unmarshaling PR node: %w", err)
	}

	pr := model.PullRequest{
		Org:     org,
		Repo:    repo,
		Number:  node.Number,
		Title:   node.Title,
		Merged:  node.Merged,
		HeadSHA: node.HeadRefOid,
		Href:    node.URL,
	}
	if node.MergeCommit != nil {
		pr.MergeCommitSHA = node.MergeCommit.OID
	}
	if node.Author != nil {
		pr.AuthorLogin = node.Author.Login
	}
	if node.MergedAt != "" {
		if t, err := time.Parse(time.RFC3339, node.MergedAt); err == nil {
			pr.MergedAt = t
		}
	}

	// Parse reviews.
	var reviews []model.Review
	for _, rn := range node.Reviews.Nodes {
		review := model.Review{
			Org:      org,
			Repo:     repo,
			PRNumber: node.Number,
			ReviewID: rn.DatabaseID,
			State:    rn.State,
			Href:     rn.URL,
		}
		if rn.Author != nil {
			review.ReviewerLogin = rn.Author.Login
		}
		if rn.Commit != nil {
			review.CommitID = rn.Commit.OID
		}
		if rn.SubmittedAt != "" {
			if t, err := time.Parse(time.RFC3339, rn.SubmittedAt); err == nil {
				review.SubmittedAt = t
			}
		}
		reviews = append(reviews, review)
	}

	// Parse check runs from the last commit in the PR.
	var checkRuns []model.CheckRun
	for _, commitNode := range node.Commits.Nodes {
		for _, suite := range commitNode.Commit.CheckSuites.Nodes {
			for _, cr := range suite.CheckRuns.Nodes {
				checkRun := model.CheckRun{
					Org:        org,
					Repo:       repo,
					CheckRunID: cr.DatabaseID,
					CheckName:  cr.Name,
					Status:     cr.Status,
					Conclusion: cr.Conclusion,
				}
				if pr.MergeCommitSHA != "" {
					checkRun.CommitSHA = pr.MergeCommitSHA
				}
				if cr.CompletedAt != "" {
					if t, err := time.Parse(time.RFC3339, cr.CompletedAt); err == nil {
						checkRun.CompletedAt = t
					}
				}
				checkRuns = append(checkRuns, checkRun)
			}
		}
	}

	return pr, reviews, checkRuns, nil
}
