package sync

import (
	"context"
	"errors"
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
	GetRepo(ctx context.Context, org, repo string) (model.RepoInfo, error)
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
	UpsertCoAuthors(ctx context.Context, commits []model.Commit) error
	UpsertCommitBranches(ctx context.Context, org, repo string, shas []string, branch string) error
	UpsertPullRequests(ctx context.Context, prs []model.PullRequest) error
	UpsertReviews(ctx context.Context, reviews []model.Review) error
	UpsertCheckRuns(ctx context.Context, runs []model.CheckRun) error
	UpsertCommitPRs(ctx context.Context, org, repo, sha string, prNumbers []int) error
	UpsertAuditResults(ctx context.Context, results []model.AuditResult) error
	UpdateCommitStats(ctx context.Context, org, repo, sha string, additions, deletions int) error
	GetUnauditedCommits(ctx context.Context, org, repo string) ([]model.Commit, error)
}

// SyncConfig controls the sync pipeline behaviour.
type SyncConfig struct {
	Orgs                []OrgConfig
	Concurrency         int
	EnrichConcurrency   int
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
	source     GitHubSource
	enricher   Enricher
	store      Store
	config     *SyncConfig
	logger     *slog.Logger
	onProgress ProgressCallback
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

// SetProgressCallback sets a function that receives progress updates during sync.
func (p *Pipeline) SetProgressCallback(cb ProgressCallback) {
	p.onProgress = cb
}

func (p *Pipeline) reportProgress(prog RepoProgress) {
	if p.onProgress != nil {
		p.onProgress(prog)
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
		if len(orgCfg.Repos) > 0 {
			// Explicit repos: fetch metadata from API for each.
			for _, repoName := range orgCfg.Repos {
				info, err := p.source.GetRepo(ctx, orgCfg.Name, repoName)
				if err != nil {
					return fmt.Errorf("fetching repo %s/%s: %w", orgCfg.Name, repoName, err)
				}
				allRepos = append(allRepos, repoWithOrg{
					repo:   info,
					orgCfg: orgCfg,
				})
			}
			p.logger.Info("using explicit repos", "org", orgCfg.Name, "count", len(orgCfg.Repos))
			continue
		}

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

	writer := NewDBWriter(concurrency * 2)
	defer writer.Close()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, rwo := range allRepos {
		g.Go(func() error {
			if err := p.syncRepo(gctx, rwo.repo, rwo.orgCfg, writer); err != nil {
				p.logger.Error("sync repo failed", "org", rwo.repo.Org, "repo", rwo.repo.Name, "error", err)
				return err
			}
			return nil
		})
	}

	return g.Wait()
}

func (p *Pipeline) syncRepo(ctx context.Context, repo model.RepoInfo, orgCfg OrgConfig, writer *DBWriter) error {
	branches := orgCfg.Branches
	if len(branches) == 0 {
		branches = []string{repo.DefaultBranch}
	}

	var errs []error
	for _, branch := range branches {
		if err := p.syncRepoBranch(ctx, repo, branch, writer); err != nil {
			p.logger.Error("sync branch failed", "org", repo.Org, "repo", repo.Name, "branch", branch, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Pipeline) syncRepoBranch(ctx context.Context, repo model.RepoInfo, branch string, writer *DBWriter) error {
	p.logger.Info("sync repo branch start", "org", repo.Org, "repo", repo.Name, "branch", branch)

	prog := RepoProgress{
		Org:       repo.Org,
		Repo:      repo.Name,
		Branch:    branch,
		Phase:     PhaseFetchingCommits,
		StartedAt: time.Now(),
	}
	p.reportProgress(prog)

	since, err := p.determineSince(ctx, repo.Org, repo.Name, branch)
	if err != nil {
		return fmt.Errorf("determining since: %w", err)
	}

	until := p.config.Until
	if until.IsZero() {
		until = time.Now()
	}

	commits, err := p.source.ListCommits(ctx, repo.Org, repo.Name, branch, since, until)
	if err != nil {
		prog.Phase = PhaseFailed
		prog.Error = err
		p.reportProgress(prog)
		return fmt.Errorf("listing commits: %w", err)
	}

	prog.Commits = len(commits)
	p.reportProgress(prog)

	p.logger.Info("fetched commits", "org", repo.Org, "repo", repo.Name, "branch", branch, "count", len(commits))

	if len(commits) == 0 {
		prog.Phase = PhaseDone
		prog.DoneAt = time.Now()
		p.reportProgress(prog)
		return nil
	}

	// Write commits through single writer
	if err := writer.Write(ctx, func() error {
		if err := p.store.UpsertCommits(ctx, commits); err != nil {
			return err
		}
		return p.store.UpsertCoAuthors(ctx, commits)
	}); err != nil {
		return fmt.Errorf("upserting commits: %w", err)
	}

	shas := make([]string, len(commits))
	for i, c := range commits {
		shas[i] = c.SHA
	}
	if err := writer.Write(ctx, func() error {
		return p.store.UpsertCommitBranches(ctx, repo.Org, repo.Name, shas, branch)
	}); err != nil {
		return fmt.Errorf("upserting commit branches: %w", err)
	}

	// Reads are safe concurrent with the writer — DuckDB MVCC
	unaudited, err := p.store.GetUnauditedCommits(ctx, repo.Org, repo.Name)
	if err != nil {
		return fmt.Errorf("getting unaudited commits: %w", err)
	}

	prog.Unaudited = len(unaudited)
	prog.Phase = PhaseEnriching
	p.reportProgress(prog)

	p.logger.Info("unaudited commits", "org", repo.Org, "repo", repo.Name, "branch", branch, "count", len(unaudited))

	// Enrich batches in parallel — these are pure API reads
	allEnrichments, err := p.enrichInParallel(ctx, repo, branch, unaudited)
	if err != nil {
		prog.Phase = PhaseFailed
		prog.Error = err
		p.reportProgress(prog)
		return fmt.Errorf("enriching commits: %w", err)
	}

	prog.Enriched = len(allEnrichments)
	prog.Phase = PhaseAuditing
	p.reportProgress(prog)

	// Collect enrichment data for a single batched write
	var allPRs []model.PullRequest
	var allReviews []model.Review
	var allCheckRuns []model.CheckRun
	var allBranchCommits []model.Commit
	type branchCommitLink struct {
		prNumber int
		shas     []string
		branch   string
	}
	type commitPRLink struct {
		sha       string
		prNumbers []int
	}
	var allLinks []commitPRLink
	var allBranchLinks []branchCommitLink

	seenPRs := make(map[string]bool)
	seenReviews := make(map[string]bool)
	seenCheckRuns := make(map[string]bool)
	seenBranchCommits := make(map[string]bool)
	for _, e := range allEnrichments {
		for _, pr := range e.PRs {
			key := fmt.Sprintf("%s/%s/%d", pr.Org, pr.Repo, pr.Number)
			if !seenPRs[key] {
				seenPRs[key] = true
				allPRs = append(allPRs, pr)
			}

			if commits, ok := e.PRBranchCommits[pr.Number]; ok {
				var branchSHAs []string
				for _, c := range commits {
					cKey := fmt.Sprintf("%s/%s/%s", c.Org, c.Repo, c.SHA)
					if !seenBranchCommits[cKey] {
						seenBranchCommits[cKey] = true
						allBranchCommits = append(allBranchCommits, c)
					}
					branchSHAs = append(branchSHAs, c.SHA)
				}
				if len(branchSHAs) > 0 {
					allBranchLinks = append(allBranchLinks, branchCommitLink{
						prNumber: pr.Number,
						shas:     branchSHAs,
						branch:   pr.HeadBranch,
					})
				}
			}
		}
		for _, r := range e.Reviews {
			key := fmt.Sprintf("%s/%s/%d/%d", r.Org, r.Repo, r.PRNumber, r.ReviewID)
			if !seenReviews[key] {
				seenReviews[key] = true
				allReviews = append(allReviews, r)
			}
		}
		for _, cr := range e.CheckRuns {
			key := fmt.Sprintf("%s/%s/%s/%d", cr.Org, cr.Repo, cr.CommitSHA, cr.CheckRunID)
			if !seenCheckRuns[key] {
				seenCheckRuns[key] = true
				allCheckRuns = append(allCheckRuns, cr)
			}
		}
		prNums := make([]int, len(e.PRs))
		for j, pr := range e.PRs {
			prNums[j] = pr.Number
		}
		if len(prNums) > 0 {
			allLinks = append(allLinks, commitPRLink{sha: e.Commit.SHA, prNumbers: prNums})
		}
	}

	// Write all enrichment data through single writer
	if err := writer.Write(ctx, func() error {
		for _, e := range allEnrichments {
			if e.Commit.Additions > 0 || e.Commit.Deletions > 0 {
				if err := p.store.UpdateCommitStats(ctx, e.Commit.Org, e.Commit.Repo, e.Commit.SHA, e.Commit.Additions, e.Commit.Deletions); err != nil {
					return err
				}
			}
		}
		if err := p.store.UpsertPullRequests(ctx, allPRs); err != nil {
			return fmt.Errorf("upserting PRs: %w", err)
		}
		if err := p.store.UpsertReviews(ctx, allReviews); err != nil {
			return fmt.Errorf("upserting reviews: %w", err)
		}
		if err := p.store.UpsertCheckRuns(ctx, allCheckRuns); err != nil {
			return fmt.Errorf("upserting check runs: %w", err)
		}
		if len(allBranchCommits) > 0 {
			if err := p.store.UpsertCommits(ctx, allBranchCommits); err != nil {
				return fmt.Errorf("upserting PR branch commits: %w", err)
			}
			if err := p.store.UpsertCoAuthors(ctx, allBranchCommits); err != nil {
				return fmt.Errorf("upserting PR branch co-authors: %w", err)
			}
		}
		for _, bl := range allBranchLinks {
			if bl.branch != "" {
				if err := p.store.UpsertCommitBranches(ctx, repo.Org, repo.Name, bl.shas, bl.branch); err != nil {
					return fmt.Errorf("upserting PR branch commit branches: %w", err)
				}
			}
			for _, sha := range bl.shas {
				if err := p.store.UpsertCommitPRs(ctx, repo.Org, repo.Name, sha, []int{bl.prNumber}); err != nil {
					return fmt.Errorf("upserting PR branch commit-PR links: %w", err)
				}
			}
		}
		for _, link := range allLinks {
			if err := p.store.UpsertCommitPRs(ctx, repo.Org, repo.Name, link.sha, link.prNumbers); err != nil {
				return fmt.Errorf("upserting commit-PR links: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Build enrichment map for audit evaluation
	enrichmentMap := make(map[string]model.EnrichmentResult)
	for _, e := range allEnrichments {
		enrichmentMap[e.Commit.SHA] = e
	}

	var auditResults []model.AuditResult
	for _, c := range unaudited {
		enrichment := enrichmentMap[c.SHA]
		// Update commit stats from GraphQL enrichment (ListCommits doesn't return these)
		if e, ok := enrichmentMap[c.SHA]; ok {
			c.Additions = e.Commit.Additions
			c.Deletions = e.Commit.Deletions
		}
		result := EvaluateCommit(c, enrichment, p.config.ExemptAuthors, p.config.RequiredChecks)
		result.AuditedAt = time.Now()
		auditResults = append(auditResults, result)
	}

	if err := writer.Write(ctx, func() error {
		return p.store.UpsertAuditResults(ctx, auditResults)
	}); err != nil {
		return fmt.Errorf("upserting audit results: %w", err)
	}

	// Update sync cursor to latest commit date
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
		if err := writer.Write(ctx, func() error {
			return p.store.UpsertSyncCursor(ctx, cursor)
		}); err != nil {
			return fmt.Errorf("upserting sync cursor: %w", err)
		}
	}

	prog.Audited = len(auditResults)
	prog.Phase = PhaseDone
	prog.DoneAt = time.Now()
	p.reportProgress(prog)

	p.logger.Info("sync repo branch done", "org", repo.Org, "repo", repo.Name, "branch", branch, "commits", len(commits), "audited", len(auditResults))
	return nil
}

// enrichInParallel runs enrichment batches concurrently using an errgroup.
// Each batch is an independent API call; results are collected by index
// to preserve ordering without a mutex.
func (p *Pipeline) enrichInParallel(ctx context.Context, repo model.RepoInfo, branch string, unaudited []model.Commit) ([]model.EnrichmentResult, error) {
	if len(unaudited) == 0 {
		return nil, nil
	}

	numBatches := (len(unaudited) + enrichBatchSize - 1) / enrichBatchSize
	batchResults := make([][]model.EnrichmentResult, numBatches)

	enrichConc := p.config.EnrichConcurrency
	if enrichConc <= 0 {
		enrichConc = 4
	}

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(enrichConc)

	for i := 0; i < len(unaudited); i += enrichBatchSize {
		batchIdx := i / enrichBatchSize
		end := min(i+enrichBatchSize, len(unaudited))
		batch := unaudited[i:end]

		eg.Go(func() error {
			batchSHAs := make([]string, len(batch))
			for j, c := range batch {
				batchSHAs[j] = c.SHA
			}

			p.logger.Info("enriching batch", "org", repo.Org, "repo", repo.Name, "branch", branch, "batch", batchIdx+1, "size", len(batchSHAs))

			enrichments, err := p.enricher.EnrichCommits(ectx, repo.Org, repo.Name, batchSHAs)
			if err != nil {
				return err
			}
			batchResults[batchIdx] = enrichments
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	var all []model.EnrichmentResult
	for _, r := range batchResults {
		all = append(all, r...)
	}
	return all, nil
}

func (p *Pipeline) determineSince(ctx context.Context, org, repo, branch string) (time.Time, error) {
	if !p.config.Since.IsZero() {
		return p.config.Since, nil
	}

	cursor, err := p.store.GetSyncCursor(ctx, org, repo, branch)
	if err != nil {
		return time.Time{}, err
	}
	if cursor != nil && !cursor.LastDate.IsZero() {
		return cursor.LastDate, nil
	}

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
