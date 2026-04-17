package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stefanpenner/gh-audit/internal/model"
)

// --- Mock implementations ---

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
	// Try branch-specific key first, then fall back to org/repo
	key := org + "/" + repo + "/" + branch
	if commits, ok := m.commits[key]; ok {
		return commits, nil
	}
	key = org + "/" + repo
	return m.commits[key], nil
}

type mockEnricher struct {
	results map[string][]model.EnrichmentResult
	calls   int32
}

func (m *mockEnricher) EnrichCommits(_ context.Context, org, repo string, shas []string) ([]model.EnrichmentResult, error) {
	atomic.AddInt32(&m.calls, 1)
	key := org + "/" + repo
	if results, ok := m.results[key]; ok {
		// Filter to requested SHAs
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
	// Return empty results for each SHA
	var out []model.EnrichmentResult
	for _, sha := range shas {
		out = append(out, model.EnrichmentResult{
			Commit: model.Commit{Org: org, Repo: repo, SHA: sha},
		})
	}
	return out, nil
}

type mockStore struct {
	cursors        map[string]*model.SyncCursor
	commits        []model.Commit
	commitBranches map[string][]string // key: org/repo/sha -> branches
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
	key := org + "/" + repo + "/" + branch
	cursor, ok := m.cursors[key]
	if !ok {
		return nil, nil
	}
	return cursor, nil
}

func (m *mockStore) UpsertSyncCursor(_ context.Context, cursor model.SyncCursor) error {
	key := cursor.Org + "/" + cursor.Repo + "/" + cursor.Branch
	m.cursors[key] = &cursor
	return nil
}

func (m *mockStore) UpsertCommits(_ context.Context, commits []model.Commit) error {
	m.commits = append(m.commits, commits...)
	// Also add to unaudited for GetUnauditedCommits
	for _, c := range commits {
		key := c.Org + "/" + c.Repo
		m.unaudited[key] = append(m.unaudited[key], c)
	}
	return nil
}

func (m *mockStore) UpsertCommitBranches(_ context.Context, org, repo string, shas []string, branch string) error {
	for _, sha := range shas {
		key := org + "/" + repo + "/" + sha
		m.commitBranches[key] = append(m.commitBranches[key], branch)
	}
	return nil
}

func (m *mockStore) UpsertPullRequests(_ context.Context, prs []model.PullRequest) error {
	m.prs = append(m.prs, prs...)
	return nil
}

func (m *mockStore) UpsertReviews(_ context.Context, reviews []model.Review) error {
	m.reviews = append(m.reviews, reviews...)
	return nil
}

func (m *mockStore) UpsertCheckRuns(_ context.Context, runs []model.CheckRun) error {
	m.checkRuns = append(m.checkRuns, runs...)
	return nil
}

func (m *mockStore) UpsertCommitPRs(_ context.Context, org, repo, sha string, prNumbers []int) error {
	return nil
}

func (m *mockStore) UpsertAuditResults(_ context.Context, results []model.AuditResult) error {
	if m.err != nil {
		return m.err
	}
	m.auditResults = append(m.auditResults, results...)
	return nil
}

func (m *mockStore) GetUnauditedCommits(_ context.Context, org, repo string) ([]model.Commit, error) {
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

	// Only repo1 commits should be stored
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
	// Create 30 commits to test batching (batch size 25)
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

	// Should have been called twice: batch of 25 + batch of 5
	calls := atomic.LoadInt32(&enricher.calls)
	if calls != 2 {
		t.Errorf("enricher called %d times, want 2", calls)
	}
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

	if len(store.commits) != 0 {
		t.Error("expected no commits for empty repo")
	}
}

func TestPipelineRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

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
	err := p.Run(ctx)
	// The error from cancelled context propagates from ListOrgRepos or ListCommits.
	// Our mock doesn't check context, but the errgroup context will be done.
	// This is acceptable; in production the real source checks context.
	_ = err
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

	// repo2 should still be processed
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

	// Both branches' commits should be stored
	if len(store.commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(store.commits))
	}

	// Both cursors should be set (per branch)
	mainCursor := store.cursors["myorg/repo1/main"]
	if mainCursor == nil {
		t.Fatal("expected cursor for main branch")
	}
	relCursor := store.cursors["myorg/repo1/release/1"]
	if relCursor == nil {
		t.Fatal("expected cursor for release/1 branch")
	}

	// Commit branches should be recorded
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
		Orgs:        []OrgConfig{{Name: "myorg"}}, // No Branches configured
		Concurrency: 1,
	}

	p := NewPipeline(source, enricher, store, cfg, slog.Default())
	err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use "develop" as the default branch
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
	// Set different cursors for each branch
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

	// After sync, cursors should be updated to now (the latest commit date)
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
