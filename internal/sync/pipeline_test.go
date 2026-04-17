package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// --- Mock implementations (thread-safe for DBWriter goroutine) ---

type mockSource struct {
	repos   map[string][]model.RepoInfo
	commits map[string][]model.Commit
	err     error
}

func (m *mockSource) ListOrgRepos(_ context.Context, org string) ([]model.RepoInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.repos[org], nil
}

func (m *mockSource) ListCommits(_ context.Context, org, repo, branch string, since, until time.Time) ([]model.Commit, error) {
	if m.err != nil {
		return nil, m.err
	}
	key := org + "/" + repo + "/" + branch
	if commits, ok := m.commits[key]; ok {
		return commits, nil
	}
	key = org + "/" + repo
	return m.commits[key], nil
}

type mockEnricher struct {
	mu      sync.Mutex
	results map[string][]model.EnrichmentResult
	calls   atomic.Int32
}

func (m *mockEnricher) EnrichCommits(_ context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	m.calls.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	key := org + "/" + repo
	if results, ok := m.results[key]; ok {
		shaSet := make(map[string]bool)
		for _, s := range shas {
			shaSet[s] = true
		}
		var filtered []model.EnrichmentResult
		for _, r := range results {
			if shaSet[r.Commit.SHA] {
				filtered = append(filtered, r)
			}
		}
		return filtered, nil
	}
	var out []model.EnrichmentResult
	for _, sha := range shas {
		out = append(out, model.EnrichmentResult{
			Commit: model.Commit{Org: org, Repo: repo, SHA: sha},
		})
	}
	return out, nil
}

type mockStore struct {
	mu             sync.Mutex
	cursors        map[string]*model.SyncCursor
	commits        []model.Commit
	commitBranches map[string][]string
	prs            []model.PullRequest
	reviews        []model.Review
	checkRuns      []model.CheckRun
	auditResults   []model.AuditResult
	unaudited      map[string][]model.Commit
	err            error
}

func newMockStore() *mockStore {
	return &mockStore{
		cursors:        make(map[string]*model.SyncCursor),
		commitBranches: make(map[string][]string),
		unaudited:      make(map[string][]model.Commit),
	}
}

func (m *mockStore) GetSyncCursor(_ context.Context, org, repo, branch string) (*model.SyncCursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := org + "/" + repo + "/" + branch
	cursor, ok := m.cursors[key]
	if !ok {
		return nil, nil
	}
	return cursor, nil
}

func (m *mockStore) UpsertSyncCursor(_ context.Context, cursor model.SyncCursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := cursor.Org + "/" + cursor.Repo + "/" + cursor.Branch
	m.cursors[key] = &cursor
	return nil
}

func (m *mockStore) UpsertCommits(_ context.Context, commits []model.Commit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commits = append(m.commits, commits...)
	for _, c := range commits {
		key := c.Org + "/" + c.Repo
		m.unaudited[key] = append(m.unaudited[key], c)
	}
	return nil
}

func (m *mockStore) UpsertCommitBranches(_ context.Context, org, repo string, shas []string, branch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sha := range shas {
		key := org + "/" + repo + "/" + sha
		m.commitBranches[key] = append(m.commitBranches[key], branch)
	}
	return nil
}

func (m *mockStore) UpsertPullRequests(_ context.Context, prs []model.PullRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prs = append(m.prs, prs...)
	return nil
}

func (m *mockStore) UpsertReviews(_ context.Context, reviews []model.Review) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reviews = append(m.reviews, reviews...)
	return nil
}

func (m *mockStore) UpsertCheckRuns(_ context.Context, runs []model.CheckRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkRuns = append(m.checkRuns, runs...)
	return nil
}

func (m *mockStore) UpsertCommitPRs(_ context.Context, org, repo, sha string, prNumbers []int) error {
	return nil
}

func (m *mockStore) UpsertAuditResults(_ context.Context, results []model.AuditResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.auditResults = append(m.auditResults, results...)
	return nil
}

func (m *mockStore) GetUnauditedCommits(_ context.Context, org, repo string) ([]model.Commit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := org + "/" + repo
	return m.unaudited[key], nil
}

// --- Tests ---

func TestPipelineDiscoverRepos(t *testing.T) {
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {
				{Org: "myorg", Name: "repo1", DefaultBranch: "main"},
				{Org: "myorg", Name: "repo2", DefaultBranch: "main"},
			},
		},
		commits: map[string][]model.Commit{},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPipelineRespectsExcludeList(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {
				{Org: "myorg", Name: "repo1", DefaultBranch: "main"},
				{Org: "myorg", Name: "excluded-repo", DefaultBranch: "main"},
			},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main": {{Org: "myorg", Repo: "repo1", SHA: "aaa", CommittedAt: now, Additions: 1}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg", ExcludeRepos: []string{"excluded-repo"}}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	for _, c := range store.commits {
		if c.Repo == "excluded-repo" {
			t.Error("excluded repo commits should not be synced")
		}
	}
}

func TestPipelineUsesStoredCursor(t *testing.T) {
	cursorDate := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{},
	}
	store := newMockStore()
	store.cursors["myorg/repo1/main"] = &model.SyncCursor{
		Org: "myorg", Repo: "repo1", Branch: "main", LastDate: cursorDate,
	}
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPipelineUsesInitialLookbackDays(t *testing.T) {
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:                []OrgConfig{{Name: "myorg"}},
		Concurrency:         1,
		InitialLookbackDays: 30,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPipelineEnrichesInBatches(t *testing.T) {
	now := time.Now()
	var commits []model.Commit
	for i := 0; i < 30; i++ {
		commits = append(commits, model.Commit{
			Org: "myorg", Repo: "repo1", SHA: fmt.Sprintf("sha%03d", i),
			CommittedAt: now, Additions: 1, AuthorLogin: "dev",
		})
	}

	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main": commits,
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 30 commits / 25 batch size = 2 enricher calls
	calls := enricher.calls.Load()
	if calls != 2 {
		t.Errorf("enricher called %d times, want 2", calls)
	}
}

func TestPipelineEnrichesInParallel(t *testing.T) {
	now := time.Now()
	// Create 100 commits to ensure multiple batches
	var commits []model.Commit
	for i := 0; i < 100; i++ {
		commits = append(commits, model.Commit{
			Org: "myorg", Repo: "repo1", SHA: fmt.Sprintf("sha%03d", i),
			CommittedAt: now, Additions: 1, AuthorLogin: "dev",
		})
	}

	var maxConcurrent atomic.Int32
	var currentConcurrent atomic.Int32

	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main": commits,
		},
	}
	store := newMockStore()

	// Track concurrent enricher calls
	enricher := &concurrencyTrackingEnricher{
		current: &currentConcurrent,
		max:     &maxConcurrent,
	}

	cfg := &SyncConfig{
		Orgs:              []OrgConfig{{Name: "myorg"}},
		Concurrency:       1,
		EnrichConcurrency: 4,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With 4 batches (100/25) and EnrichConcurrency=4, we should see >1 concurrent
	if got := maxConcurrent.Load(); got <= 1 {
		t.Errorf("max concurrent enrichment = %d, want >1 (parallel enrichment not working)", got)
	}
}

type concurrencyTrackingEnricher struct {
	current *atomic.Int32
	max     *atomic.Int32
}

func (e *concurrencyTrackingEnricher) EnrichCommits(_ context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	cur := e.current.Add(1)
	for {
		old := e.max.Load()
		if cur <= old || e.max.CompareAndSwap(old, cur) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond) // Simulate API latency
	e.current.Add(-1)

	var out []model.EnrichmentResult
	for _, sha := range shas {
		out = append(out, model.EnrichmentResult{
			Commit: model.Commit{Org: org, Repo: repo, SHA: sha},
		})
	}
	return out, nil
}

func TestPipelineStoresEnrichmentData(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main": {{Org: "myorg", Repo: "repo1", SHA: "aaa", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{
		results: map[string][]model.EnrichmentResult{
			"myorg/repo1": {
				{
					Commit:    model.Commit{Org: "myorg", Repo: "repo1", SHA: "aaa"},
					PRs:       []model.PullRequest{{Org: "myorg", Repo: "repo1", Number: 1}},
					Reviews:   []model.Review{{Org: "myorg", Repo: "repo1", PRNumber: 1, ReviewID: 1}},
					CheckRuns: []model.CheckRun{{Org: "myorg", Repo: "repo1", CommitSHA: "aaa", CheckRunID: 1}},
				},
			},
		},
	}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.prs) == 0 {
		t.Error("expected PRs to be stored")
	}
	if len(store.reviews) == 0 {
		t.Error("expected reviews to be stored")
	}
	if len(store.checkRuns) == 0 {
		t.Error("expected check runs to be stored")
	}
}

func TestPipelineEvaluatesAuditRules(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main": {{Org: "myorg", Repo: "repo1", SHA: "aaa", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.auditResults) != 1 {
		t.Fatalf("expected 1 audit result, got %d", len(store.auditResults))
	}
}

func TestPipelineUpdatesCursorAfterSync(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main": {{Org: "myorg", Repo: "repo1", SHA: "aaa", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	cursor := store.cursors["myorg/repo1/main"]
	if cursor == nil {
		t.Fatal("expected cursor to be set")
	}
	if !cursor.LastDate.Equal(now) {
		t.Errorf("cursor.LastDate = %v, want %v", cursor.LastDate, now)
	}
	if cursor.Branch != "main" {
		t.Errorf("cursor.Branch = %q, want %q", cursor.Branch, "main")
	}
}

func TestPipelineHandlesEmptyRepo(t *testing.T) {
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "empty-repo", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.commits) != 0 {
		t.Error("expected no commits for empty repo")
	}
}

func TestPipelineRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	_ = p.Run(ctx)
}

func TestPipelineContinuesWhenOneRepoFails(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {
				{Org: "myorg", Name: "repo1", DefaultBranch: "main"},
				{Org: "myorg", Name: "repo2", DefaultBranch: "main"},
			},
		},
		commits: map[string][]model.Commit{
			"myorg/repo2/main": {{Org: "myorg", Repo: "repo2", SHA: "bbb", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
		},
	}

	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	found := false
	for _, c := range store.commits {
		if c.Repo == "repo2" {
			found = true
		}
	}
	if !found {
		t.Error("repo2 commits should still be synced even if repo1 had issues")
	}
}

func TestFilterRepos(t *testing.T) {
	repos := []model.RepoInfo{
		{Org: "o", Name: "a"},
		{Org: "o", Name: "b"},
		{Org: "o", Name: "archived", Archived: true},
		{Org: "o", Name: "excluded"},
	}

	tests := []struct {
		name string
		cfg  OrgConfig
		want []string
	}{
		{
			name: "exclude archived",
			cfg:  OrgConfig{Name: "o"},
			want: []string{"a", "b", "excluded"},
		},
		{
			name: "exclude list",
			cfg:  OrgConfig{Name: "o", ExcludeRepos: []string{"excluded"}},
			want: []string{"a", "b"},
		},
		{
			name: "include list",
			cfg:  OrgConfig{Name: "o", Repos: []string{"a"}},
			want: []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterRepos(repos, tt.cfg)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d repos, want %d", len(got), len(tt.want))
			}
			for i, r := range got {
				if r.Name != tt.want[i] {
					t.Errorf("repo[%d] = %s, want %s", i, r.Name, tt.want[i])
				}
			}
		})
	}
}

func TestPipelineMultiBranchSync(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main":      {{Org: "myorg", Repo: "repo1", SHA: "aaa", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
			"myorg/repo1/release/1": {{Org: "myorg", Repo: "repo1", SHA: "bbb", CommittedAt: now, Additions: 2, AuthorLogin: "dev"}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg", Branches: []string{"main", "release/1"}}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(store.commits))
	}

	mainCursor := store.cursors["myorg/repo1/main"]
	if mainCursor == nil {
		t.Fatal("expected cursor for main branch")
	}
	relCursor := store.cursors["myorg/repo1/release/1"]
	if relCursor == nil {
		t.Fatal("expected cursor for release/1 branch")
	}

	mainBranches := store.commitBranches["myorg/repo1/aaa"]
	if len(mainBranches) != 1 || mainBranches[0] != "main" {
		t.Errorf("expected commit aaa to be on main branch, got %v", mainBranches)
	}
	relBranches := store.commitBranches["myorg/repo1/bbb"]
	if len(relBranches) != 1 || relBranches[0] != "release/1" {
		t.Errorf("expected commit bbb to be on release/1 branch, got %v", relBranches)
	}
}

func TestPipelineNoBranchesUsesDefault(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "develop"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/develop": {{Org: "myorg", Repo: "repo1", SHA: "aaa", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	cursor := store.cursors["myorg/repo1/develop"]
	if cursor == nil {
		t.Fatal("expected cursor for develop branch (the default branch)")
	}
	if cursor.Branch != "develop" {
		t.Errorf("cursor.Branch = %q, want %q", cursor.Branch, "develop")
	}
}

func TestPipelineDifferentCursorsPerBranch(t *testing.T) {
	now := time.Now()
	mainCursorDate := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	releaseCursorDate := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {{Org: "myorg", Name: "repo1", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main":      {{Org: "myorg", Repo: "repo1", SHA: "aaa", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
			"myorg/repo1/release/1": {{Org: "myorg", Repo: "repo1", SHA: "bbb", CommittedAt: now, Additions: 2, AuthorLogin: "dev"}},
		},
	}
	store := newMockStore()
	store.cursors["myorg/repo1/main"] = &model.SyncCursor{
		Org: "myorg", Repo: "repo1", Branch: "main", LastDate: mainCursorDate,
	}
	store.cursors["myorg/repo1/release/1"] = &model.SyncCursor{
		Org: "myorg", Repo: "repo1", Branch: "release/1", LastDate: releaseCursorDate,
	}
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg", Branches: []string{"main", "release/1"}}},
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	mainCursor := store.cursors["myorg/repo1/main"]
	if mainCursor == nil {
		t.Fatal("expected cursor for main branch")
	}
	if !mainCursor.LastDate.Equal(now) {
		t.Errorf("main cursor LastDate = %v, want %v", mainCursor.LastDate, now)
	}

	relCursor := store.cursors["myorg/repo1/release/1"]
	if relCursor == nil {
		t.Fatal("expected cursor for release/1 branch")
	}
	if !relCursor.LastDate.Equal(now) {
		t.Errorf("release/1 cursor LastDate = %v, want %v", relCursor.LastDate, now)
	}
}

func TestPipelineConcurrentRepoSync(t *testing.T) {
	now := time.Now()
	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"myorg": {
				{Org: "myorg", Name: "repo1", DefaultBranch: "main"},
				{Org: "myorg", Name: "repo2", DefaultBranch: "main"},
				{Org: "myorg", Name: "repo3", DefaultBranch: "main"},
			},
		},
		commits: map[string][]model.Commit{
			"myorg/repo1/main": {{Org: "myorg", Repo: "repo1", SHA: "a1", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
			"myorg/repo2/main": {{Org: "myorg", Repo: "repo2", SHA: "b1", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
			"myorg/repo3/main": {{Org: "myorg", Repo: "repo3", SHA: "c1", CommittedAt: now, Additions: 1, AuthorLogin: "dev"}},
		},
	}
	store := newMockStore()
	enricher := &mockEnricher{}

	cfg := &SyncConfig{
		Orgs:        []OrgConfig{{Name: "myorg"}},
		Concurrency: 3, // All repos sync concurrently
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.commits) != 3 {
		t.Fatalf("expected 3 commits, got %d", len(store.commits))
	}
	if len(store.auditResults) != 3 {
		t.Fatalf("expected 3 audit results, got %d", len(store.auditResults))
	}
}

func TestPipelineRealisticMix(t *testing.T) {
	now := time.Now()

	commits := []model.Commit{
		{Org: "acme", Repo: "app", SHA: "aaa1", AuthorLogin: "dependabot[bot]", CommittedAt: now, Additions: 5, Deletions: 2},
		{Org: "acme", Repo: "app", SHA: "aaa2", AuthorLogin: "some-ci[bot]", CommittedAt: now, Additions: 3, Deletions: 1},
		{Org: "acme", Repo: "app", SHA: "aaa3", AuthorLogin: "alice", CommittedAt: now, Additions: 10, Deletions: 4},
		{Org: "acme", Repo: "app", SHA: "aaa4", AuthorLogin: "bob", CommittedAt: now, Additions: 7, Deletions: 3},
		{Org: "acme", Repo: "app", SHA: "aaa5", AuthorLogin: "charlie", CommittedAt: now, Additions: 1, Deletions: 0},
		{Org: "acme", Repo: "app", SHA: "aaa6", AuthorLogin: "dave", CommittedAt: now, Additions: 0, Deletions: 0},
		{Org: "acme", Repo: "app", SHA: "aaa7", AuthorLogin: "eve", CommittedAt: now, Additions: 4, Deletions: 1},
		{Org: "acme", Repo: "app", SHA: "aaa8", AuthorLogin: "frank", CommittedAt: now, Additions: 2, Deletions: 1},
	}

	source := &mockSource{
		repos: map[string][]model.RepoInfo{
			"acme": {{Org: "acme", Name: "app", FullName: "acme/app", DefaultBranch: "main"}},
		},
		commits: map[string][]model.Commit{
			"acme/app/main": commits,
		},
	}

	enricher := &mockEnricher{
		results: map[string][]model.EnrichmentResult{
			"acme/app": {
				// aaa1: dependabot — exempt, enrichment doesn't matter
				{Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa1", Additions: 5, Deletions: 2}},
				// aaa2: non-exempt bot, no PR → non-compliant
				{Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa2", Additions: 3, Deletions: 1}},
				// aaa3: alice's commit, proper approval on final commit → compliant
				{
					Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa3", Additions: 10, Deletions: 4},
					PRs:    []model.PullRequest{{Org: "acme", Repo: "app", Number: 10, Merged: true, HeadSHA: "aaa3", AuthorLogin: "alice"}},
					Reviews: []model.Review{
						{Org: "acme", Repo: "app", PRNumber: 10, ReviewID: 100, ReviewerLogin: "bob", State: "APPROVED", CommitID: "aaa3"},
					},
				},
				// aaa4: bob's commit, approval on OLD commit (stale) → non-compliant
				{
					Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa4", Additions: 7, Deletions: 3},
					PRs:    []model.PullRequest{{Org: "acme", Repo: "app", Number: 20, Merged: true, HeadSHA: "aaa4", AuthorLogin: "bob"}},
					Reviews: []model.Review{
						{Org: "acme", Repo: "app", PRNumber: 20, ReviewID: 200, ReviewerLogin: "alice", State: "APPROVED", CommitID: "old-sha-xyz"},
					},
				},
				// aaa5: charlie, direct push no PR → non-compliant
				{Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa5", Additions: 1, Deletions: 0}},
				// aaa6: dave, empty commit → compliant
				{Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa6", Additions: 0, Deletions: 0}},
				// aaa7: eve self-approves her own PR → non-compliant
				{
					Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa7", Additions: 4, Deletions: 1},
					PRs:    []model.PullRequest{{Org: "acme", Repo: "app", Number: 30, Merged: true, HeadSHA: "aaa7", AuthorLogin: "eve"}},
					Reviews: []model.Review{
						{Org: "acme", Repo: "app", PRNumber: 30, ReviewID: 300, ReviewerLogin: "eve", State: "APPROVED", CommitID: "aaa7"},
					},
				},
				// aaa8: frank self-approves BUT independent approval also exists → compliant
				{
					Commit: model.Commit{Org: "acme", Repo: "app", SHA: "aaa8", Additions: 2, Deletions: 1},
					PRs:    []model.PullRequest{{Org: "acme", Repo: "app", Number: 40, Merged: true, HeadSHA: "aaa8", AuthorLogin: "frank"}},
					Reviews: []model.Review{
						{Org: "acme", Repo: "app", PRNumber: 40, ReviewID: 400, ReviewerLogin: "frank", State: "APPROVED", CommitID: "aaa8"},
						{Org: "acme", Repo: "app", PRNumber: 40, ReviewID: 401, ReviewerLogin: "alice", State: "APPROVED", CommitID: "aaa8"},
					},
				},
			},
		},
	}

	store := newMockStore()
	cfg := &SyncConfig{
		Orgs:          []OrgConfig{{Name: "acme"}},
		Concurrency:   1,
		ExemptAuthors: []string{"dependabot[bot]"},
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("pipeline run: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.auditResults) != 8 {
		t.Fatalf("expected 8 audit results, got %d", len(store.auditResults))
	}

	resultMap := make(map[string]model.AuditResult)
	for _, r := range store.auditResults {
		resultMap[r.SHA] = r
	}

	type expectation struct {
		sha            string
		compliant      bool
		isBot          bool
		isExempt       bool
		isEmpty        bool
		isSelfApproved bool
		hasPR          bool
		desc           string
	}

	expectations := []expectation{
		{sha: "aaa1", compliant: true, isBot: true, isExempt: true, desc: "exempt bot (dependabot)"},
		{sha: "aaa2", compliant: false, isBot: true, isExempt: false, desc: "non-exempt bot, no PR"},
		{sha: "aaa3", compliant: true, hasPR: true, desc: "human with proper approval"},
		{sha: "aaa4", compliant: false, hasPR: true, desc: "human with stale approval"},
		{sha: "aaa5", compliant: false, desc: "direct push, no PR"},
		{sha: "aaa6", compliant: true, isEmpty: true, desc: "empty commit"},
		{sha: "aaa7", compliant: false, isSelfApproved: true, hasPR: true, desc: "self-approved only"},
		{sha: "aaa8", compliant: true, hasPR: true, desc: "self-approval + independent approval"},
	}

	for _, exp := range expectations {
		t.Run(exp.desc, func(t *testing.T) {
			r, ok := resultMap[exp.sha]
			if !ok {
				t.Fatalf("missing audit result for %s", exp.sha)
			}
			if r.IsCompliant != exp.compliant {
				t.Errorf("IsCompliant: got %v, want %v (reasons: %v)", r.IsCompliant, exp.compliant, r.Reasons)
			}
			if r.IsBot != exp.isBot {
				t.Errorf("IsBot: got %v, want %v", r.IsBot, exp.isBot)
			}
			if r.IsExemptAuthor != exp.isExempt {
				t.Errorf("IsExemptAuthor: got %v, want %v", r.IsExemptAuthor, exp.isExempt)
			}
			if r.IsEmptyCommit != exp.isEmpty {
				t.Errorf("IsEmptyCommit: got %v, want %v", r.IsEmptyCommit, exp.isEmpty)
			}
			if r.IsSelfApproved != exp.isSelfApproved {
				t.Errorf("IsSelfApproved: got %v, want %v", r.IsSelfApproved, exp.isSelfApproved)
			}
			if r.HasPR != exp.hasPR {
				t.Errorf("HasPR: got %v, want %v", r.HasPR, exp.hasPR)
			}
		})
	}

	// Verify aggregate counts
	var compliant, nonCompliant, bots, exempt, empty, selfApproved int
	for _, r := range store.auditResults {
		if r.IsCompliant {
			compliant++
		} else {
			nonCompliant++
		}
		if r.IsBot {
			bots++
		}
		if r.IsExemptAuthor {
			exempt++
		}
		if r.IsEmptyCommit {
			empty++
		}
		if r.IsSelfApproved {
			selfApproved++
		}
	}

	if compliant != 4 {
		t.Errorf("compliant: got %d, want 4", compliant)
	}
	if nonCompliant != 4 {
		t.Errorf("non-compliant: got %d, want 4", nonCompliant)
	}
	if bots != 2 {
		t.Errorf("bots: got %d, want 2", bots)
	}
	if exempt != 1 {
		t.Errorf("exempt: got %d, want 1", exempt)
	}
	if empty != 1 {
		t.Errorf("empty: got %d, want 1", empty)
	}
	if selfApproved != 1 {
		t.Errorf("self-approved: got %d, want 1", selfApproved)
	}
}
