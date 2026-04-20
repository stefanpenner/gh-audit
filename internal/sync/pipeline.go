package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
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

// TokenStatsSnapshot describes the current budget state of the token pool.
// A zero-value snapshot means the pool is unknown (telemetry will skip the
// token section).
type TokenStatsSnapshot struct {
	Total     int   // total tokens registered
	Available int   // tokens not currently rate-limited
	Remaining int64 // sum of remaining requests across all tokens
	Capacity  int64 // sum of each token's advertised limit

	// Cumulative rate-limit events since pool creation.
	SecondaryRateLimitEvents int64
	PrimaryRateLimitEvents   int64
	TokenReassigns           int64
	InFlight                 int // current global in-flight requests
}

// TokenStatsFn returns the pool's current stats. Pipeline uses it for
// periodic telemetry. Optional; nil disables token-pool reporting.
type TokenStatsFn func() TokenStatsSnapshot

// APIStatsSnapshot describes cumulative per-endpoint REST call counts.
// Used to emit "where are the requests going" telemetry so a slow sweep
// can be attributed to a specific endpoint family (e.g. reviews vs
// check runs vs PR commits).
type APIStatsSnapshot struct {
	CommitDetail       int64
	CommitPRs          int64
	PRDetail           int64
	Reviews            int64
	CheckRuns          int64
	PRCommits          int64
	RevertVerification int64
	CacheHits          int64
	DBHits             int64
}

// Total is the sum of API-call fields (excludes cache/DB hits).
func (s APIStatsSnapshot) Total() int64 {
	return s.CommitDetail + s.CommitPRs + s.PRDetail +
		s.Reviews + s.CheckRuns + s.PRCommits + s.RevertVerification
}

// APIStatsFn returns the enricher's current API-call stats. Optional; nil
// disables per-endpoint telemetry.
type APIStatsFn func() APIStatsSnapshot

// Pipeline orchestrates the sync of GitHub data into the local database.
type Pipeline struct {
	source         GitHubSource
	enricher       Enricher
	store          Store
	config         *SyncConfig
	logger         *slog.Logger
	onProgress     ProgressCallback
	tokenStats     TokenStatsFn
	apiStats       APIStatsFn
	telemetryOut   io.Writer        // optional JSONL sink for structured telemetry
	statsFetcher   StatsFetcher // optional lazy additions/deletions resolver for audit empty-commit fallback
	commitsSynced  atomic.Int64
	commitsAudited atomic.Int64
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

// SetTokenStatsFn wires a token-pool snapshot source so the pipeline can log
// rate-limit headroom periodically. Optional.
func (p *Pipeline) SetTokenStatsFn(fn TokenStatsFn) {
	p.tokenStats = fn
}

// SetAPIStatsFn wires an enricher-stats snapshot source so the pipeline can
// log per-endpoint API-call breakdowns in telemetry. Optional.
func (p *Pipeline) SetAPIStatsFn(fn APIStatsFn) {
	p.apiStats = fn
}

// SetTelemetryOutput wires a sink for structured JSONL telemetry. Each
// telemetry tick (including the final one) emits one JSON object per line
// with all fields flattened. Optional; nil disables the JSONL sink.
func (p *Pipeline) SetTelemetryOutput(w io.Writer) {
	p.telemetryOut = w
}

// SetStatsFetcher wires a lazy additions/deletions resolver used by the
// audit's empty-commit fallback. Without it, EvaluateCommit treats any
// already-zero stats as "empty" (legacy behaviour). Optional.
func (p *Pipeline) SetStatsFetcher(fn StatsFetcher) {
	p.statsFetcher = fn
}

// runTelemetry emits throughput and token-pool headroom periodically until
// the pipeline finishes. Always emits a final line on shutdown so even short
// syncs leave a record. Safe no-op if the context cancels first.
func (p *Pipeline) runTelemetry(ctx context.Context, done <-chan struct{}) {
	const interval = 10 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()

	start := time.Now()
	var lastSynced, lastAudited int64
	var lastUsed, cumUsed int64
	var hasLastUsed bool
	lastTick := start
	var lastAPI APIStatsSnapshot
	var hasLastAPI bool

	emit := func(now time.Time, final bool) {
		synced := p.commitsSynced.Load()
		audited := p.commitsAudited.Load()
		windowSec := now.Sub(lastTick).Seconds()
		if windowSec <= 0 {
			windowSec = 1
		}
		elapsedSec := now.Sub(start).Seconds()
		if elapsedSec <= 0 {
			elapsedSec = 1
		}

		attrs := []any{
			"elapsed", time.Duration(elapsedSec * float64(time.Second)).Round(time.Second),
			"commits_synced", synced,
			"commits_audited", audited,
			"sync_rate_recent", fmt.Sprintf("%.1f/s", float64(synced-lastSynced)/windowSec),
			"audit_rate_recent", fmt.Sprintf("%.1f/s", float64(audited-lastAudited)/windowSec),
			"sync_rate_total", fmt.Sprintf("%.1f/s", float64(synced)/elapsedSec),
			"audit_rate_total", fmt.Sprintf("%.1f/s", float64(audited)/elapsedSec),
		}
		if p.tokenStats != nil {
			s := p.tokenStats()
			if s.Total > 0 {
				used := s.Capacity - s.Remaining
				pct := 0.0
				if s.Capacity > 0 {
					pct = 100.0 * float64(used) / float64(s.Capacity)
				}

				// Track consumption deltas across ticks. Negative delta means
				// a token's hourly budget reset between ticks; approximate the
				// pre-reset consumption as lastUsed so the cumulative counter
				// captures spend that would otherwise be erased.
				var recentDelta int64
				if hasLastUsed {
					d := used - lastUsed
					if d < 0 {
						recentDelta = lastUsed + used
					} else {
						recentDelta = d
					}
					cumUsed += recentDelta
				} else {
					cumUsed = used
				}
				lastUsed = used
				hasLastUsed = true

				attrs = append(attrs,
					"tokens_available", fmt.Sprintf("%d/%d", s.Available, s.Total),
					"api_budget_used_pct", fmt.Sprintf("%.1f%%", pct),
					"api_budget_remaining", s.Remaining,
					"api_budget_capacity", s.Capacity,
					"api_consume_rate_recent", fmt.Sprintf("%.0f/h", float64(recentDelta)/windowSec*3600),
					"api_consume_rate_total", fmt.Sprintf("%.0f/h", float64(cumUsed)/elapsedSec*3600),
					"in_flight", s.InFlight,
					"secondary_rl_events", s.SecondaryRateLimitEvents,
					"primary_rl_events", s.PrimaryRateLimitEvents,
					"token_reassigns", s.TokenReassigns,
				)
			}
		}
		msg := "telemetry"
		if final {
			msg = "telemetry_final"
		}
		p.logger.Info(msg, attrs...)

		// Per-endpoint API-call breakdown: a separate log line so the short
		// "telemetry" summary stays readable. Shows the recent-window delta
		// for each endpoint plus cache/DB reuse — makes it easy to spot
		// which endpoint family (reviews? check runs? PR detail?) is
		// dominating API burn at any moment.
		if p.apiStats != nil {
			api := p.apiStats()
			if hasLastAPI || final {
				p.logger.Info("api_endpoint_breakdown",
					"elapsed", time.Duration(elapsedSec*float64(time.Second)).Round(time.Second),
					"total_api", api.Total(),
					"commit_detail", api.CommitDetail,
					"commit_prs", api.CommitPRs,
					"pr_detail", api.PRDetail,
					"reviews", api.Reviews,
					"check_runs", api.CheckRuns,
					"pr_commits", api.PRCommits,
					"revert_verify", api.RevertVerification,
					"cache_hits", api.CacheHits,
					"db_hits", api.DBHits,
					"delta_total", api.Total()-lastAPI.Total(),
					"delta_commit_detail", api.CommitDetail-lastAPI.CommitDetail,
					"delta_commit_prs", api.CommitPRs-lastAPI.CommitPRs,
					"delta_pr_detail", api.PRDetail-lastAPI.PRDetail,
					"delta_reviews", api.Reviews-lastAPI.Reviews,
					"delta_check_runs", api.CheckRuns-lastAPI.CheckRuns,
					"delta_pr_commits", api.PRCommits-lastAPI.PRCommits,
					"delta_cache_hits", api.CacheHits-lastAPI.CacheHits,
					"delta_db_hits", api.DBHits-lastAPI.DBHits,
					"recent_rate", fmt.Sprintf("%.1f/s", float64(api.Total()-lastAPI.Total())/windowSec),
				)
			}
			lastAPI = api
			hasLastAPI = true
		}

		// Structured JSONL sink: one self-describing line per tick. Gives
		// the operator a file they can post-process (jq, duckdb) to study
		// run dynamics without parsing slog text output.
		if p.telemetryOut != nil {
			record := buildTelemetryRecord(now, elapsedSec, windowSec, final, synced, audited, lastSynced, lastAudited, p)
			if blob, jsonErr := json.Marshal(record); jsonErr == nil {
				_, _ = p.telemetryOut.Write(append(blob, '\n'))
			}
		}

		lastSynced, lastAudited = synced, audited
		lastTick = now
	}

	for {
		select {
		case <-done:
			emit(time.Now(), true)
			return
		case <-ctx.Done():
			emit(time.Now(), true)
			return
		case now := <-t.C:
			emit(now, false)
		}
	}
}

func (p *Pipeline) reportProgress(prog RepoProgress) {
	if p.onProgress != nil {
		p.onProgress(prog)
	}
}

// telemetryRecord is the shape written to the JSONL sink each tick. Field
// names are flat and lowercase so jq / duckdb ingestion stays trivial.
type telemetryRecord struct {
	Timestamp          string  `json:"timestamp"`
	ElapsedSeconds     float64 `json:"elapsed_seconds"`
	Final              bool    `json:"final"`
	CommitsSynced      int64   `json:"commits_synced"`
	CommitsAudited     int64   `json:"commits_audited"`
	SyncRateRecent     float64 `json:"sync_rate_recent"`
	AuditRateRecent    float64 `json:"audit_rate_recent"`

	// Token pool (present only if tokenStats is wired)
	TokensTotal              *int   `json:"tokens_total,omitempty"`
	TokensAvailable          *int   `json:"tokens_available,omitempty"`
	BudgetRemaining          *int64 `json:"budget_remaining,omitempty"`
	BudgetCapacity           *int64 `json:"budget_capacity,omitempty"`
	InFlight                 *int   `json:"in_flight,omitempty"`
	SecondaryRateLimitEvents *int64 `json:"secondary_rl_events,omitempty"`
	PrimaryRateLimitEvents   *int64 `json:"primary_rl_events,omitempty"`
	TokenReassigns           *int64 `json:"token_reassigns,omitempty"`

	// API endpoint breakdown (present only if apiStats is wired)
	TotalAPI           *int64 `json:"total_api,omitempty"`
	CommitDetail       *int64 `json:"commit_detail,omitempty"`
	CommitPRs          *int64 `json:"commit_prs,omitempty"`
	PRDetail           *int64 `json:"pr_detail,omitempty"`
	Reviews            *int64 `json:"reviews,omitempty"`
	CheckRuns          *int64 `json:"check_runs,omitempty"`
	PRCommits          *int64 `json:"pr_commits,omitempty"`
	RevertVerification *int64 `json:"revert_verify,omitempty"`
	CacheHits          *int64 `json:"cache_hits,omitempty"`
	DBHits             *int64 `json:"db_hits,omitempty"`
}

func buildTelemetryRecord(now time.Time, elapsedSec, windowSec float64, final bool, synced, audited, lastSynced, lastAudited int64, p *Pipeline) telemetryRecord {
	r := telemetryRecord{
		Timestamp:       now.UTC().Format(time.RFC3339),
		ElapsedSeconds:  elapsedSec,
		Final:           final,
		CommitsSynced:   synced,
		CommitsAudited:  audited,
		SyncRateRecent:  float64(synced-lastSynced) / windowSec,
		AuditRateRecent: float64(audited-lastAudited) / windowSec,
	}
	if p.tokenStats != nil {
		s := p.tokenStats()
		if s.Total > 0 {
			total := s.Total
			avail := s.Available
			rem := s.Remaining
			cap := s.Capacity
			inf := s.InFlight
			sec := s.SecondaryRateLimitEvents
			pri := s.PrimaryRateLimitEvents
			rea := s.TokenReassigns
			r.TokensTotal = &total
			r.TokensAvailable = &avail
			r.BudgetRemaining = &rem
			r.BudgetCapacity = &cap
			r.InFlight = &inf
			r.SecondaryRateLimitEvents = &sec
			r.PrimaryRateLimitEvents = &pri
			r.TokenReassigns = &rea
		}
	}
	if p.apiStats != nil {
		a := p.apiStats()
		tot := a.Total()
		r.TotalAPI = &tot
		r.CommitDetail = &a.CommitDetail
		r.CommitPRs = &a.CommitPRs
		r.PRDetail = &a.PRDetail
		r.Reviews = &a.Reviews
		r.CheckRuns = &a.CheckRuns
		r.PRCommits = &a.PRCommits
		r.RevertVerification = &a.RevertVerification
		r.CacheHits = &a.CacheHits
		r.DBHits = &a.DBHits
	}
	return r
}

// repoWithOrg pairs a repo with the OrgConfig it belongs to so that
// syncRepo has access to the branch list.
type repoWithOrg struct {
	repo   model.RepoInfo
	orgCfg OrgConfig
}

// Run executes the full sync pipeline across all configured orgs.
// resolveExplicitRepos fans out GetRepo across the explicit repo list so the
// bootstrap for a large sweep (hundreds of --repo flags) doesn't serialize
// hundreds of API calls. First error short-circuits via errgroup cancellation.
func (p *Pipeline) resolveExplicitRepos(ctx context.Context, orgCfg OrgConfig, concurrency int) ([]repoWithOrg, error) {
	resolved := make([]repoWithOrg, len(orgCfg.Repos))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, repoName := range orgCfg.Repos {
		g.Go(func() error {
			info, err := p.source.GetRepo(gctx, orgCfg.Name, repoName)
			if err != nil {
				return fmt.Errorf("fetching repo %s/%s: %w", orgCfg.Name, repoName, err)
			}
			resolved[i] = repoWithOrg{repo: info, orgCfg: orgCfg}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return resolved, nil
}

func (p *Pipeline) Run(ctx context.Context) error {
	concurrency := p.config.Concurrency
	if concurrency <= 0 {
		concurrency = 32
	}

	var allRepos []repoWithOrg

	for _, orgCfg := range p.config.Orgs {
		if len(orgCfg.Repos) > 0 {
			// Explicit repos: fan out GetRepo in parallel so bootstrap on
			// large repo lists (hundreds+) isn't a serial API stall.
			resolveStart := time.Now()
			resolved, err := p.resolveExplicitRepos(ctx, orgCfg, concurrency)
			if err != nil {
				return err
			}
			allRepos = append(allRepos, resolved...)
			p.logger.Info("using explicit repos",
				"org", orgCfg.Name,
				"count", len(orgCfg.Repos),
				"resolve_duration", time.Since(resolveStart).Round(time.Millisecond),
			)
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

	writer := NewDBWriter(concurrency * 2)
	defer writer.Close()

	// Fail fast on real per-repo errors. Transient retries (rate-limit,
	// network) are handled at the transport layer; anything that bubbles
	// out to this point is a sustained or unexpected failure that deserves
	// operator attention — we abort the sweep, surface the error, and let
	// the sync cursor resume the remaining work on the next run.
	//
	// The one exception is `context.Canceled`: that's our own errgroup
	// cancelling sibling goroutines after a real failure. Logging those as
	// errors would obscure the root cause; we demote them to DEBUG.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	// Start periodic telemetry (throughput + token-pool headroom). Stops when
	// the sync finishes.
	telemDone := make(chan struct{})
	go p.runTelemetry(gctx, telemDone)

	for _, rwo := range allRepos {
		g.Go(func() error {
			if err := p.syncRepo(gctx, rwo.repo, rwo.orgCfg, writer); err != nil {
				if errors.Is(err, context.Canceled) && gctx.Err() != nil {
					p.logger.Debug("sync repo aborted (cascade)", "org", rwo.repo.Org, "repo", rwo.repo.Name)
					return err
				}
				p.logger.Error("sync repo failed", "org", rwo.repo.Org, "repo", rwo.repo.Name, "error", err)
				return err
			}
			return nil
		})
	}

	err := g.Wait()
	close(telemDone)
	return err
}

func (p *Pipeline) syncRepo(ctx context.Context, repo model.RepoInfo, orgCfg OrgConfig, writer *DBWriter) error {
	branches := orgCfg.Branches
	if len(branches) == 0 {
		branches = []string{repo.DefaultBranch}
	}

	var errs []error
	for _, branch := range branches {
		if err := p.syncRepoBranch(ctx, repo, branch, writer); err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				p.logger.Debug("sync branch aborted (cascade)", "org", repo.Org, "repo", repo.Name, "branch", branch)
			} else {
				p.logger.Error("sync branch failed", "org", repo.Org, "repo", repo.Name, "branch", branch, "error", err)
			}
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
	p.commitsSynced.Add(int64(len(commits)))

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

	// Enrich batches in parallel, writing each batch's data to DB immediately
	// so partial progress survives failures and populates the DB cache for retry.
	allEnrichments, err := p.enrichInParallel(ctx, repo, branch, unaudited, writer)
	if err != nil {
		prog.Phase = PhaseFailed
		prog.Error = err
		p.reportProgress(prog)
		return fmt.Errorf("enriching commits: %w", err)
	}

	prog.Enriched = len(allEnrichments)
	prog.Phase = PhaseAuditing
	p.reportProgress(prog)

	// Build enrichment map for audit evaluation
	enrichmentMap := make(map[string]model.EnrichmentResult)
	for _, e := range allEnrichments {
		enrichmentMap[e.Commit.SHA] = e
	}

	var auditResults []model.AuditResult
	for _, c := range unaudited {
		enrichment := enrichmentMap[c.SHA]
		// Carry any additions/deletions we happened to get during enrichment
		// (e.g. from a future GraphQL path). When absent, EvaluateCommit
		// resolves them lazily via p.statsFetcher only if the audit would
		// otherwise flag the commit non-compliant.
		if e, ok := enrichmentMap[c.SHA]; ok {
			c.Additions = e.Commit.Additions
			c.Deletions = e.Commit.Deletions
		}
		result := EvaluateCommit(c, enrichment, p.config.ExemptAuthors, p.config.RequiredChecks, p.statsFetcher)
		result.AuditedAt = time.Now()
		auditResults = append(auditResults, result)
	}

	if err := writer.Write(ctx, func() error {
		return p.store.UpsertAuditResults(ctx, auditResults)
	}); err != nil {
		return fmt.Errorf("upserting audit results: %w", err)
	}
	p.commitsAudited.Add(int64(len(auditResults)))

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

	doneAttrs := []any{
		"org", repo.Org, "repo", repo.Name, "branch", branch,
		"commits", len(commits), "audited", len(auditResults),
	}
	if p.tokenStats != nil {
		s := p.tokenStats()
		if s.Total > 0 {
			doneAttrs = append(doneAttrs,
				"tokens_available", fmt.Sprintf("%d/%d", s.Available, s.Total),
				"api_budget_remaining", s.Remaining,
			)
		}
	}
	p.logger.Info("sync repo branch done", doneAttrs...)
	return nil
}

// enrichInParallel runs enrichment batches concurrently using an errgroup.
// Each batch's enrichment data is written to DB immediately after enrichment,
// so partial progress survives failures and populates the DB cache for retry.
func (p *Pipeline) enrichInParallel(ctx context.Context, repo model.RepoInfo, branch string, unaudited []model.Commit, writer *DBWriter) ([]model.EnrichmentResult, error) {
	if len(unaudited) == 0 {
		return nil, nil
	}

	numBatches := (len(unaudited) + enrichBatchSize - 1) / enrichBatchSize
	batchResults := make([][]model.EnrichmentResult, numBatches)

	enrichConc := p.config.EnrichConcurrency
	if enrichConc <= 0 {
		enrichConc = 16
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

			if err := p.writeEnrichmentBatch(ectx, repo.Org, repo.Name, enrichments, writer); err != nil {
				return fmt.Errorf("writing enrichment batch %d: %w", batchIdx+1, err)
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

// writeEnrichmentBatch persists a batch of enrichment results to the database.
// All upserts are idempotent, so duplicate writes across batches are safe.
func (p *Pipeline) writeEnrichmentBatch(ctx context.Context, org, repo string, enrichments []model.EnrichmentResult, writer *DBWriter) error {
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

	seenPR := make(map[int]bool)
	seenReview := make(map[int64]bool)
	seenCheckRun := make(map[int64]bool)
	seenCommit := make(map[string]bool)

	for _, e := range enrichments {
		for _, pr := range e.PRs {
			if !seenPR[pr.Number] {
				seenPR[pr.Number] = true
				allPRs = append(allPRs, pr)
			}

			if commits, ok := e.PRBranchCommits[pr.Number]; ok {
				var branchSHAs []string
				for _, c := range commits {
					if !seenCommit[c.SHA] {
						seenCommit[c.SHA] = true
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
			if !seenReview[r.ReviewID] {
				seenReview[r.ReviewID] = true
				allReviews = append(allReviews, r)
			}
		}
		for _, cr := range e.CheckRuns {
			if !seenCheckRun[cr.CheckRunID] {
				seenCheckRun[cr.CheckRunID] = true
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

	return writer.Write(ctx, func() error {
		for _, e := range enrichments {
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
				if err := p.store.UpsertCommitBranches(ctx, org, repo, bl.shas, bl.branch); err != nil {
					return fmt.Errorf("upserting PR branch commit branches: %w", err)
				}
			}
			for _, sha := range bl.shas {
				if err := p.store.UpsertCommitPRs(ctx, org, repo, sha, []int{bl.prNumber}); err != nil {
					return fmt.Errorf("upserting PR branch commit-PR links: %w", err)
				}
			}
		}
		for _, link := range allLinks {
			if err := p.store.UpsertCommitPRs(ctx, org, repo, link.sha, link.prNumbers); err != nil {
				return fmt.Errorf("upserting commit-PR links: %w", err)
			}
		}
		return nil
	})
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
