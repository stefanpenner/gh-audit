package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/stefanpenner/gh-audit/internal/model"
)

const enrichBatchSize = 25

// GitHubSource abstracts GitHub API access for listing repos and commits.
type GitHubSource interface {
	ListOrgRepos(ctx context.Context, org string) ([]model.RepoInfo, error)
	ListCommits(ctx context.Context, org, repo, branch string, since, until time.Time) ([]model.Commit, error)
}

// Enricher abstracts commit enrichment (fetching PRs, reviews, check runs).
type Enricher interface {
	EnrichCommits(ctx context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error)
}

// Store abstracts database persistence.
type Store interface {
	GetSyncCursor(ctx context.Context, org, repo, branch string) (*model.SyncCursor, error)
	UpsertSyncCursor(ctx context.Context, cursor model.SyncCursor) error
	UpsertCommits(ctx context.Context, commits []model.Commit) error
	UpsertCommitBranches(ctx context.Context, org, repo string, shas []string, branch string) error
	UpsertPullRequests(ctx context.Context, prs []model.PullRequest) error
	UpsertReviews(ctx context.Context, reviews []model.Review) error
	UpsertCheckRuns(ctx context.Context, runs []model.CheckRun) error
	UpsertCommitPRs(ctx context.Context, org, repo, sha string, prNumbers []int) error
	UpsertAuditResults(ctx context.Context, results []model.AuditResult) error
	GetUnauditedCommits(ctx context.Context, org, repo string) ([]model.Commit, error)
}

// SyncConfig controls the sync pipeline behaviour.
type SyncConfig struct {
	Orgs                []OrgConfig
	Concurrency         int
	Since               time.Time // override, zero means use cursor
	Until               time.Time // override, zero means now
	InitialLookbackDays int
	ExemptAuthors       []string
	RequiredChecks      []RequiredCheck
}

// OrgConfig describes an org and its repo include/exclude lists.
type OrgConfig struct {
	Name         string
	Repos        []string
	ExcludeRepos []string
	Branches     []string // branch names to audit; empty = default branch only
}

// Pipeline orchestrates the sync of GitHub data into the local database.
type Pipeline struct {
	source   GitHubSource
	enricher Enricher
	store    Store
	config   *SyncConfig
	logger   *slog.Logger
}

// NewPipeline creates a new sync pipeline.
func NewPipeline(source GitHubSource, enricher Enricher, store Store, cfg *SyncConfig, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{
		source:   source,
		enricher: enricher,
		store:    store,
		config:   cfg,
		logger:   logger,
	}
}

// repoWithOrg pairs a repo with the OrgConfig it belongs to so that
// syncRepo has access to the branch list.
type repoWithOrg struct {
	repo   model.RepoInfo
	orgCfg OrgConfig
}

// Run executes the full sync pipeline across all configured orgs.
func (p *Pipeline) Run(ctx context.Context) error {
	var allRepos []repoWithOrg

	for _, orgCfg := range p.config.Orgs {
		repos, err := p.source.ListOrgRepos(ctx, orgCfg.Name)
		if err != nil {
			return fmt.Errorf("listing repos for org %s: %w", orgCfg.Name, err)
		}

		filtered := filterRepos(repos, orgCfg)
		p.logger.Info("discovered repos", "org", orgCfg.Name, "total", len(repos), "after_filter", len(filtered))
		for _, r := range filtered {
			allRepos = append(allRepos, repoWithOrg{repo: r, orgCfg: orgCfg})
		}
	}

	concurrency := p.config.Concurrency
	if concurrency <= 0 {
		concurrency = 10
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, rwo := range allRepos {
		g.Go(func() error {
			if err := p.syncRepo(gctx, rwo.repo, rwo.orgCfg); err != nil {
				p.logger.Error("sync repo failed", "org", rwo.repo.Org, "repo", rwo.repo.Name, "error", err)
				// Continue with other repos; don't fail the whole pipeline
				return nil
			}
			return nil
		})
	}

	return g.Wait()
}

func (p *Pipeline) syncRepo(ctx context.Context, repo model.RepoInfo, orgCfg OrgConfig) error {
	branches := orgCfg.Branches
	if len(branches) == 0 {
		branches = []string{repo.DefaultBranch}
	}

	for _, branch := range branches {
		if err := p.syncRepoBranch(ctx, repo, branch); err != nil {
			p.logger.Error("sync branch failed", "org", repo.Org, "repo", repo.Name, "branch", branch, "error", err)
			// Continue with other branches
		}
	}
	return nil
}

func (p *Pipeline) syncRepoBranch(ctx context.Context, repo model.RepoInfo, branch string) error {
	p.logger.Info("sync repo branch start", "org", repo.Org, "repo", repo.Name, "branch", branch)

	since, err := p.determineSince(ctx, repo.Org, repo.Name, branch)
	if err != nil {
		return fmt.Errorf("determining since: %w", err)
	}

	until := p.config.Until
	if until.IsZero() {
		until = time.Now()
	}

	// Fetch commits
	commits, err := p.source.ListCommits(ctx, repo.Org, repo.Name, branch, since, until)
	if err != nil {
		return fmt.Errorf("listing commits: %w", err)
	}

	p.logger.Info("fetched commits", "org", repo.Org, "repo", repo.Name, "branch", branch, "count", len(commits))

	if len(commits) == 0 {
		return nil
	}

	// Store commits
	if err := p.store.UpsertCommits(ctx, commits); err != nil {
		return fmt.Errorf("upserting commits: %w", err)
	}

	// Record which branch these commits belong to
	shas := make([]string, len(commits))
	for i, c := range commits {
		shas[i] = c.SHA
	}
	if err := p.store.UpsertCommitBranches(ctx, repo.Org, repo.Name, shas, branch); err != nil {
		return fmt.Errorf("upserting commit branches: %w", err)
	}

	// Get unaudited commits
	unaudited, err := p.store.GetUnauditedCommits(ctx, repo.Org, repo.Name)
	if err != nil {
		return fmt.Errorf("getting unaudited commits: %w", err)
	}

	p.logger.Info("unaudited commits", "org", repo.Org, "repo", repo.Name, "branch", branch, "count", len(unaudited))

	// Enrich in batches
	var allEnrichments []model.EnrichmentResult
	for i := 0; i < len(unaudited); i += enrichBatchSize {
		end := min(i+enrichBatchSize, len(unaudited))
		batch := unaudited[i:end]

		batchSHAs := make([]string, len(batch))
		for j, c := range batch {
			batchSHAs[j] = c.SHA
		}

		p.logger.Info("enriching batch", "org", repo.Org, "repo", repo.Name, "branch", branch, "batch", i/enrichBatchSize+1, "size", len(batchSHAs))

		enrichments, err := p.enricher.EnrichCommits(ctx, repo.Org, repo.Name, batchSHAs)
		if err != nil {
			return fmt.Errorf("enriching commits: %w", err)
		}

		// Store enrichment data
		for _, e := range enrichments {
			if err := p.store.UpsertPullRequests(ctx, e.PRs); err != nil {
				return fmt.Errorf("upserting PRs: %w", err)
			}
			if err := p.store.UpsertReviews(ctx, e.Reviews); err != nil {
				return fmt.Errorf("upserting reviews: %w", err)
			}
			if err := p.store.UpsertCheckRuns(ctx, e.CheckRuns); err != nil {
				return fmt.Errorf("upserting check runs: %w", err)
			}

			// Link commit to PRs
			prNumbers := make([]int, len(e.PRs))
			for j, pr := range e.PRs {
				prNumbers[j] = pr.Number
			}
			if err := p.store.UpsertCommitPRs(ctx, repo.Org, repo.Name, e.Commit.SHA, prNumbers); err != nil {
				return fmt.Errorf("upserting commit-PR links: %w", err)
			}
		}

		allEnrichments = append(allEnrichments, enrichments...)
	}

	// Build enrichment map for audit evaluation
	enrichmentMap := make(map[string]model.EnrichmentResult)
	for _, e := range allEnrichments {
		enrichmentMap[e.Commit.SHA] = e
	}

	// Evaluate audit rules
	var auditResults []model.AuditResult
	for _, c := range unaudited {
		enrichment := enrichmentMap[c.SHA]
		result := EvaluateCommit(c, enrichment, p.config.ExemptAuthors, p.config.RequiredChecks)
		result.AuditedAt = time.Now()
		auditResults = append(auditResults, result)
	}

	if err := p.store.UpsertAuditResults(ctx, auditResults); err != nil {
		return fmt.Errorf("upserting audit results: %w", err)
	}

	// Update sync cursor to latest commit date (per branch)
	var latestDate time.Time
	for _, c := range commits {
		if c.CommittedAt.After(latestDate) {
			latestDate = c.CommittedAt
		}
	}

	if !latestDate.IsZero() {
		cursor := model.SyncCursor{
			Org:       repo.Org,
			Repo:      repo.Name,
			Branch:    branch,
			LastDate:  latestDate,
			UpdatedAt: time.Now(),
		}
		if err := p.store.UpsertSyncCursor(ctx, cursor); err != nil {
			return fmt.Errorf("upserting sync cursor: %w", err)
		}
	}

	p.logger.Info("sync repo branch done", "org", repo.Org, "repo", repo.Name, "branch", branch, "commits", len(commits), "audited", len(auditResults))
	return nil
}

func (p *Pipeline) determineSince(ctx context.Context, org, repo, branch string) (time.Time, error) {
	// Explicit override takes priority
	if !p.config.Since.IsZero() {
		return p.config.Since, nil
	}

	// Check cursor
	cursor, err := p.store.GetSyncCursor(ctx, org, repo, branch)
	if err != nil {
		return time.Time{}, err
	}
	if cursor != nil && !cursor.LastDate.IsZero() {
		return cursor.LastDate, nil
	}

	// Fall back to initial lookback
	days := p.config.InitialLookbackDays
	if days <= 0 {
		days = 90
	}
	return time.Now().AddDate(0, 0, -days), nil
}

func filterRepos(repos []model.RepoInfo, cfg OrgConfig) []model.RepoInfo {
	excludeSet := make(map[string]bool)
	for _, r := range cfg.ExcludeRepos {
		excludeSet[r] = true
	}

	includeSet := make(map[string]bool)
	for _, r := range cfg.Repos {
		includeSet[r] = true
	}

	var result []model.RepoInfo
	for _, r := range repos {
		if r.Archived {
			continue
		}
		if excludeSet[r.Name] {
			continue
		}
		if len(includeSet) > 0 && !includeSet[r.Name] {
			continue
		}
		result = append(result, r)
	}
	return result
}
