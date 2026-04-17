package ui

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	syncpkg "github.com/stefanpenner/gh-audit/internal/sync"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("99")).
			MarginBottom(1)

	repoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39"))

	doneStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("82"))

	failStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	countStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214"))
)

type progressMsg syncpkg.RepoProgress

// DoneMsg signals that the sync pipeline has completed.
type DoneMsg struct{ Err error }
type tickMsg time.Time

// SyncModel is the bubbletea model for sync progress.
type SyncModel struct {
	mu       sync.Mutex
	repos    map[string]*syncpkg.RepoProgress // key: org/repo/branch
	order    []string                          // insertion order
	spinner  spinner.Model
	done     bool
	err      error
	total    int
	started  time.Time
	width    int
	quitting bool
}

// NewSyncModel creates a new sync UI model.
func NewSyncModel(totalRepos int) *SyncModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	return &SyncModel{
		repos:   make(map[string]*syncpkg.RepoProgress),
		spinner: s,
		total:   totalRepos,
		started: time.Now(),
	}
}

// ProgressCallback returns a callback suitable for Pipeline.SetProgressCallback.
// It sends progress updates to the bubbletea program.
func (m *SyncModel) ProgressCallback(p *tea.Program) syncpkg.ProgressCallback {
	return func(prog syncpkg.RepoProgress) {
		p.Send(progressMsg(prog))
	}
}

func (m *SyncModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *SyncModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width

	case progressMsg:
		prog := syncpkg.RepoProgress(msg)
		key := fmt.Sprintf("%s/%s/%s", prog.Org, prog.Repo, prog.Branch)
		m.mu.Lock()
		if _, exists := m.repos[key]; !exists {
			m.order = append(m.order, key)
		}
		cp := prog
		m.repos[key] = &cp
		m.mu.Unlock()

	case DoneMsg:
		m.done = true
		m.err = msg.Err
		return m, tea.Quit

	case tickMsg:
		return m, tickCmd()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *SyncModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("gh-audit sync"))
	b.WriteString("\n")

	m.mu.Lock()
	repos := make([]*syncpkg.RepoProgress, 0, len(m.order))
	for _, key := range m.order {
		repos = append(repos, m.repos[key])
	}
	m.mu.Unlock()

	// Sort: active first, then done, then failed
	sort.SliceStable(repos, func(i, j int) bool {
		pi, pj := phaseOrder(repos[i].Phase), phaseOrder(repos[j].Phase)
		return pi < pj
	})

	doneCount := 0
	failCount := 0
	totalCommits := 0
	totalAudited := 0
	for _, r := range repos {
		switch r.Phase {
		case syncpkg.PhaseDone:
			doneCount++
		case syncpkg.PhaseFailed:
			failCount++
		}
		totalCommits += r.Commits
		totalAudited += r.Audited
	}

	// Summary line
	elapsed := time.Since(m.started).Truncate(time.Second)
	summary := fmt.Sprintf("  %s/%s repos  %s commits  %s audited  %s",
		countStyle.Render(fmt.Sprintf("%d", doneCount)),
		countStyle.Render(fmt.Sprintf("%d", m.total)),
		countStyle.Render(fmt.Sprintf("%d", totalCommits)),
		countStyle.Render(fmt.Sprintf("%d", totalAudited)),
		dimStyle.Render(elapsed.String()),
	)
	if failCount > 0 {
		summary += "  " + failStyle.Render(fmt.Sprintf("%d failed", failCount))
	}
	b.WriteString(summary)
	b.WriteString("\n\n")

	// Show repos: limit display to avoid overwhelming the terminal
	maxShow := 20
	shown := 0
	for _, r := range repos {
		if shown >= maxShow {
			remaining := len(repos) - maxShow
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ... and %d more\n", remaining)))
			break
		}
		b.WriteString(m.renderRepo(r))
		b.WriteString("\n")
		shown++
	}

	if m.done {
		b.WriteString("\n")
		if m.err != nil {
			b.WriteString(failStyle.Render(fmt.Sprintf("  Sync failed: %v", m.err)))
		} else {
			b.WriteString(doneStyle.Render("  Sync complete!"))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (m *SyncModel) renderRepo(r *syncpkg.RepoProgress) string {
	name := repoStyle.Render(fmt.Sprintf("%s/%s", r.Org, r.Repo))
	if r.Branch != "" && r.Branch != "main" && r.Branch != "master" {
		name += dimStyle.Render(fmt.Sprintf("@%s", r.Branch))
	}

	switch r.Phase {
	case syncpkg.PhaseDone:
		dur := r.DoneAt.Sub(r.StartedAt).Truncate(time.Millisecond)
		detail := fmt.Sprintf("%d commits, %d audited", r.Commits, r.Audited)
		return fmt.Sprintf("  %s %s  %s  %s",
			doneStyle.Render("✓"),
			name,
			dimStyle.Render(detail),
			dimStyle.Render(dur.String()),
		)
	case syncpkg.PhaseFailed:
		errMsg := "unknown error"
		if r.Error != nil {
			errMsg = r.Error.Error()
			if len(errMsg) > 60 {
				errMsg = errMsg[:57] + "..."
			}
		}
		return fmt.Sprintf("  %s %s  %s",
			failStyle.Render("✗"),
			name,
			failStyle.Render(errMsg),
		)
	default:
		phase := phaseLabel(r.Phase)
		detail := ""
		if r.Commits > 0 {
			detail = fmt.Sprintf(" %d commits", r.Commits)
		}
		if r.Unaudited > 0 {
			detail += fmt.Sprintf(", %d unaudited", r.Unaudited)
		}
		return fmt.Sprintf("  %s %s  %s%s",
			m.spinner.View(),
			name,
			dimStyle.Render(phase),
			dimStyle.Render(detail),
		)
	}
}

func phaseLabel(p syncpkg.RepoPhase) string {
	switch p {
	case syncpkg.PhaseFetchingCommits:
		return "fetching commits"
	case syncpkg.PhaseEnriching:
		return "enriching"
	case syncpkg.PhaseAuditing:
		return "auditing"
	case syncpkg.PhaseWriting:
		return "writing"
	default:
		return p.String()
	}
}

// Quitting returns true if the user quit the UI with q/ctrl+c.
func (m *SyncModel) Quitting() bool {
	return m.quitting
}

func phaseOrder(p syncpkg.RepoPhase) int {
	switch p {
	case syncpkg.PhaseFetchingCommits, syncpkg.PhaseEnriching, syncpkg.PhaseAuditing, syncpkg.PhaseWriting:
		return 0
	case syncpkg.PhaseQueued:
		return 1
	case syncpkg.PhaseDone:
		return 2
	case syncpkg.PhaseFailed:
		return 3
	default:
		return 4
	}
}
