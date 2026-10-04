package upstream

import (
	"fmt"
	"strings"
	"time"
)

// WatchResult is the outcome of one watch cycle across all forks.
type WatchResult struct {
	At      time.Time      `json:"at"`
	Results []MergeResult  `json:"results"`
	Summary string         `json:"summary"`
}

// Watch checks every fork for upstream changes and attempts merges.
// It never pushes to main. Each fork gets its own branch on success.
// Returns results for reporting (e.g. to squawk).
func Watch(forks []Fork, dryRun bool) (*WatchResult, error) {
	wr := &WatchResult{At: time.Now()}
	var lines []string

	for _, f := range forks {
		if f.WorkDir == "" {
			lines = append(lines, fmt.Sprintf("⏭️  %s: no local checkout configured (work_dir empty) — skipping", f.Name))
			continue
		}

		result, err := Merge(f, dryRun)
		if err != nil {
			lines = append(lines, fmt.Sprintf("🛑 %s: error: %v", f.Name, err))
			// Persist blocked state
			_ = SaveState(f.Name, &ForkState{LastResult: "error", Detail: err.Error()})
			continue
		}
		wr.Results = append(wr.Results, *result)

		// Persist state
		stateResult := "no-changes"
		switch {
		case result.Ready && result.UpstreamCommits > 0:
			stateResult = "clean"
		case result.Ready:
			stateResult = "no-changes"
		case len(result.Conflicts) > 0:
			stateResult = "conflicts"
		case !result.TestsPassed && result.Clean:
			stateResult = "test-failed"
		}
		_ = SaveState(f.Name, &ForkState{
			LastMergedSHA: result.UpstreamNewSHA,
			LastResult:    stateResult,
			LastBranch:    result.Branch,
			Detail:        result.Report,
		})

		lines = append(lines, result.Report)
	}

	wr.Summary = strings.Join(lines, "\n\n")
	return wr, nil
}

// FormatFleetReport renders a watch result as a squawk-friendly message.
func FormatFleetReport(wr *WatchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🐂 Trailboss upstream watch — %s\n\n", wr.At.Format("2006-01-02 15:04 MST"))

	ready := 0
	conflicted := 0
	uptodate := 0
	for _, r := range wr.Results {
		switch {
		case r.Ready && r.UpstreamCommits > 0:
			ready++
		case len(r.Conflicts) > 0:
			conflicted++
		default:
			uptodate++
		}
	}

	fmt.Fprintf(&b, "Ready for approval: %d | Needs human eyes: %d | Up to date: %d\n\n", ready, conflicted, uptodate)
	b.WriteString(wr.Summary)
	return b.String()
}
