package sync

import "time"

// RepoPhase describes the current phase of a repo's sync.
type RepoPhase int

const (
	PhaseQueued RepoPhase = iota
	PhaseFetchingCommits
	PhaseEnriching
	PhaseAuditing
	PhaseWriting
	PhaseDone
	PhaseFailed
)

func (p RepoPhase) String() string {
	switch p {
	case PhaseQueued:
		return "queued"
	case PhaseFetchingCommits:
		return "fetching"
	case PhaseEnriching:
		return "enriching"
	case PhaseAuditing:
		return "auditing"
	case PhaseWriting:
		return "writing"
	case PhaseDone:
		return "done"
	case PhaseFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// RepoProgress holds the current state of a single repo sync.
type RepoProgress struct {
	Org       string
	Repo      string
	Branch    string
	Phase     RepoPhase
	Commits   int
	Unaudited int
	Enriched  int
	Audited   int
	Error     error
	StartedAt time.Time
	DoneAt    time.Time
}

// ProgressCallback receives sync progress updates.
type ProgressCallback func(RepoProgress)
