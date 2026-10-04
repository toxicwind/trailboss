package upstream

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetStatus(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: upstreamDir, UpstreamBranch: "main"}

	st, err := GetStatus(f)
	require.NoError(t, err)
	assert.Equal(t, "test", st.Fork)
	assert.Equal(t, 1, st.Behind)
	assert.Equal(t, 1, st.Ahead)
	assert.True(t, st.NeedsMerge)
	assert.NotEmpty(t, st.UpstreamSHA)
}

func TestGetStatusUpToDate(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: upstreamDir, UpstreamBranch: "main"}

	// Merge the upstream change so the fork is up to date (ahead still 1).
	res, err := Merge(f, false)
	require.NoError(t, err)
	require.True(t, res.Clean)

	st, err := GetStatus(f)
	require.NoError(t, err)
	assert.Equal(t, 0, st.Behind)
	assert.False(t, st.NeedsMerge)
}

func TestGetStatusBadDir(t *testing.T) {
	f := Fork{Name: "test", WorkDir: t.TempDir(), UpstreamURL: "https://example.invalid/x.git", UpstreamBranch: "main"}
	_, err := GetStatus(f)
	require.Error(t, err)
}

func TestStateDir(t *testing.T) {
	t.Setenv("TRAILBOSS_STATE_DIR", "/tmp/custom-state-dir")
	assert.Equal(t, "/tmp/custom-state-dir", StateDir())
}

func TestStateDirDefault(t *testing.T) {
	// Ensure the env var is unset for the default path.
	os.Unsetenv("TRAILBOSS_STATE_DIR")
	home, _ := os.UserHomeDir()
	assert.Equal(t, filepath.Join(home, ".trailboss", "upstream-state"), StateDir())
}

func TestSaveLoadStateRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRAILBOSS_STATE_DIR", dir)

	saved := &ForkState{
		LastMergedSHA: "abc123",
		LastResult:    "clean",
		LastBranch:    "upstream-merge/20260101-000000",
		Detail:        "all good",
	}
	require.NoError(t, SaveState("test", saved))

	loaded, err := LoadState("test")
	require.NoError(t, err)
	assert.Equal(t, "abc123", loaded.LastMergedSHA)
	assert.Equal(t, "clean", loaded.LastResult)
	assert.Equal(t, "upstream-merge/20260101-000000", loaded.LastBranch)
	assert.Equal(t, "all good", loaded.Detail)
	assert.False(t, loaded.LastChecked.IsZero(), "SaveState stamps LastChecked")
}

func TestLoadStateMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRAILBOSS_STATE_DIR", dir)
	s, err := LoadState("never-saved")
	require.NoError(t, err)
	assert.Equal(t, &ForkState{}, s)
}

func TestLoadStateCorrupt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRAILBOSS_STATE_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{nope"), 0o644))
	_, err := LoadState("bad")
	require.Error(t, err)
}

func TestWatchSkipsEmptyWorkDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRAILBOSS_STATE_DIR", dir)

	wr, err := Watch([]Fork{{Name: "ghost", Repo: "toxicwind/ghost"}}, true)
	require.NoError(t, err)
	assert.Empty(t, wr.Results)
	assert.Contains(t, wr.Summary, "ghost")
	assert.Contains(t, wr.Summary, "skipping")
	assert.False(t, wr.At.IsZero())
}

func TestWatchErrorPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRAILBOSS_STATE_DIR", dir)

	// A WorkDir that is not a git repo: Merge fails at checkout, Watch records
	// the error and persists blocked state.
	badDir := t.TempDir()
	wr, err := Watch([]Fork{{
		Name: "broken", Repo: "toxicwind/broken",
		WorkDir: badDir, UpstreamURL: "https://example.invalid/x.git", UpstreamBranch: "main",
	}}, true)
	require.NoError(t, err)
	assert.Empty(t, wr.Results)
	assert.Contains(t, wr.Summary, "broken")
	assert.Contains(t, wr.Summary, "error")

	st, err := LoadState("broken")
	require.NoError(t, err)
	assert.Equal(t, "error", st.LastResult)
	assert.NotEmpty(t, st.Detail)
}

func TestWatchDryRunMerge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRAILBOSS_STATE_DIR", dir)

	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: upstreamDir, UpstreamBranch: "main"}

	wr, err := Watch([]Fork{f}, true)
	require.NoError(t, err)
	require.Len(t, wr.Results, 1)
	res := wr.Results[0]
	assert.Equal(t, 1, res.UpstreamCommits)
	assert.Contains(t, res.Report, "would merge")

	st, err := LoadState("test")
	require.NoError(t, err)
	// Dry-run merge is not Ready (no branch created, no tests run) and has no
	// conflicts, so Watch records "no-changes".
	assert.Equal(t, "no-changes", st.LastResult)

	report := FormatFleetReport(wr)
	assert.Contains(t, report, "Trailboss upstream watch")
	assert.Contains(t, report, "Up to date: 1")
}

func TestFormatFleetReportCounts(t *testing.T) {
	wr := &WatchResult{
		At: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		Results: []MergeResult{
			{Fork: "a", Ready: true, UpstreamCommits: 3},
			{Fork: "b", Ready: true, UpstreamCommits: 0},
			{Fork: "c", Conflicts: []string{"x.go"}},
			{Fork: "d"},
		},
		Summary: "details here",
	}
	out := FormatFleetReport(wr)
	assert.Contains(t, out, "Ready for approval: 1")
	assert.Contains(t, out, "Needs human eyes: 1")
	assert.Contains(t, out, "Up to date: 2")
	assert.Contains(t, out, "details here")
	assert.True(t, strings.HasPrefix(out, "🐂 Trailboss upstream watch"))
}

func TestMergeDryRunNoChanges(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{
		Name: "test", WorkDir: forkDir,
		UpstreamURL: upstreamDir, UpstreamBranch: "main",
		TestCmd: []string{"true"},
	}
	// Do the real merge (lands on a new branch), then fast-forward main to it
	// so the fork is truly up to date with upstream.
	res, err := Merge(f, false)
	require.NoError(t, err)
	require.True(t, res.Clean)
	runGitTest(t, forkDir, "checkout", "main")
	runGitTest(t, forkDir, "merge", "--ff-only", res.Branch)

	// Now a dry run reports "already up to date".
	res, err = Merge(f, true)
	require.NoError(t, err)
	assert.True(t, res.Ready)
	assert.Equal(t, 0, res.UpstreamCommits)
	assert.Contains(t, res.Report, "already up to date")
}

// runGit is a test helper for direct git invocations.
func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestMergeDirtyTree(t *testing.T) {
	_, forkDir := setupTestRepo(t)
	// Dirty the working tree.
	require.NoError(t, os.WriteFile(filepath.Join(forkDir, "dirty.txt"), []byte("x"), 0o644))
	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: "x", UpstreamBranch: "main"}
	_, err := Merge(f, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dirty")
}
