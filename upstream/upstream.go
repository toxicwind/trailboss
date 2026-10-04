// Package upstream implements trailboss's agentic upstream ownership.
//
// Trailboss watches our renamed forks' upstreams, autonomously attempts merges
// on branches, runs test suites, and reports. It never pushes to main — it
// prepares, verifies, and reports. The human gives the final go.
package upstream

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Fork describes one of our renamed forks and its upstream relationship.
type Fork struct {
	// Name is our short name: "herd", "tau", "roundup", "trailboss".
	Name string `yaml:"name" json:"name"`
	// Repo is our GitHub repo: "toxicwind/herd".
	Repo string `yaml:"repo" json:"repo"`
	// UpstreamURL is the upstream git URL: "https://github.com/mostlygeek/llama-swap.git".
	UpstreamURL string `yaml:"upstream_url" json:"upstream_url"`
	// UpstreamBranch is the upstream branch to merge from, usually "main" or "master".
	UpstreamBranch string `yaml:"upstream_branch" json:"upstream_branch"`
	// TestCmd is the test command to run after merging, e.g. ["go", "test", "./..."].
	TestCmd []string `yaml:"test_cmd" json:"test_cmd"`
	// WorkDir is the local checkout path. If empty, derived from a base dir.
	WorkDir string `yaml:"work_dir" json:"work_dir"`
	// PatchInventory points to docs describing our patches (for conflict guidance).
	PatchInventory string `yaml:"patch_inventory" json:"patch_inventory"`
}

// DefaultForks is the built-in registry of our renamed forks.
var DefaultForks = []Fork{
	{
		Name:           "herd",
		Repo:           "toxicwind/herd",
		UpstreamURL:    "https://github.com/mostlygeek/llama-swap.git",
		UpstreamBranch: "main",
		TestCmd:        []string{"go", "test", "./..."},
		PatchInventory: "docs/PATCHES.md",
	},
	{
		Name:           "tau",
		Repo:           "toxicwind/tau",
		UpstreamURL:    "https://github.com/can1357/oh-my-pi.git",
		UpstreamBranch: "main",
		TestCmd:        []string{"bun", "test"},
		PatchInventory: "MIRROR-DIFF-vs-upstream.md",
	},
	{
		Name:           "roundup",
		Repo:           "toxicwind/roundup",
		UpstreamURL:    "https://github.com/vllm-project/guidellm.git",
		UpstreamBranch: "main",
		TestCmd:        []string{"python3", "-m", "pytest", "-x", "-q"},
		PatchInventory: "docs/PATCHES.md",
	},
	{
		Name:           "trailboss",
		Repo:           "toxicwind/trailboss",
		UpstreamURL:    "https://github.com/redscaresu/goldfinger.git",
		UpstreamBranch: "main",
		TestCmd:        []string{"go", "test", "./..."},
		PatchInventory: "docs/PATCHES.md",
	},
}

// ForkState tracks the last merge attempt per fork.
type ForkState struct {
	LastMergedSHA string    `json:"last_merged_sha"`
	LastChecked   time.Time `json:"last_checked"`
	LastResult    string    `json:"last_result"` // clean, conflicts-resolved, blocked, no-changes, test-failed
	LastBranch    string    `json:"last_branch"`
	Detail        string    `json:"detail"`
}

// MergeResult is the outcome of one merge attempt.
type MergeResult struct {
	Fork            string   `json:"fork"`
	UpstreamNewSHA  string   `json:"upstream_new_sha"`
	UpstreamCommits int      `json:"upstream_commits"`
	Branch          string   `json:"branch"`
	Clean           bool     `json:"clean"`
	Conflicts       []string `json:"conflicts,omitempty"`
	TestsPassed     bool     `json:"tests_passed"`
	TestOutput      string   `json:"test_output,omitempty"`
	Ready           bool     `json:"ready"` // clean merge + tests green = ready for human approval
	Report          string   `json:"report"`
}

// runGit runs a git command in dir and returns stdout.
func runGit(dir string, args ...string) (string, error) {
	// #nosec G204 -- args are constructed internally from validated git subcommands, not user input
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// EnsureUpstreamRemote adds the "upstream" remote if missing.
func EnsureUpstreamRemote(f Fork) error {
	dir := f.WorkDir
	remotes, err := runGit(dir, "remote")
	if err != nil {
		return err
	}
	for _, r := range strings.Split(remotes, "\n") {
		if strings.TrimSpace(r) == "upstream" {
			return nil // already configured
		}
	}
	_, err = runGit(dir, "remote", "add", "upstream", f.UpstreamURL)
	return err
}

// Divergence returns (behind, ahead) commit counts: how many upstream commits
// we're behind, and how many of our commits upstream doesn't have.
func Divergence(f Fork) (behind, ahead int, err error) {
	dir := f.WorkDir
	if err := EnsureUpstreamRemote(f); err != nil {
		return 0, 0, err
	}
	if _, err := runGit(dir, "fetch", "upstream", f.UpstreamBranch); err != nil {
		return 0, 0, fmt.Errorf("fetch upstream: %w", err)
	}
	upstreamRef := "upstream/" + f.UpstreamBranch
	behindStr, err := runGit(dir, "rev-list", "--count", "HEAD.."+upstreamRef)
	if err != nil {
		return 0, 0, err
	}
	aheadStr, err := runGit(dir, "rev-list", "--count", upstreamRef+"..HEAD")
	if err != nil {
		return 0, 0, err
	}
	if _, err := fmt.Sscanf(behindStr, "%d", &behind); err != nil {
		return 0, 0, fmt.Errorf("parse behind %q: %w", behindStr, err)
	}
	if _, err := fmt.Sscanf(aheadStr, "%d", &ahead); err != nil {
		return 0, 0, fmt.Errorf("parse ahead %q: %w", aheadStr, err)
	}
	return behind, ahead, nil
}

// Status is a human/machine summary of one fork's upstream relationship.
type Status struct {
	Fork        string `json:"fork"`
	Repo        string `json:"repo"`
	Behind      int    `json:"behind"`
	Ahead       int    `json:"ahead"`
	UpstreamSHA string `json:"upstream_sha"`
	NeedsMerge  bool   `json:"needs_merge"`
}

// GetStatus returns the current status for a fork.
func GetStatus(f Fork) (*Status, error) {
	behind, ahead, err := Divergence(f)
	if err != nil {
		return nil, err
	}
	sha, err := runGit(f.WorkDir, "rev-parse", "upstream/"+f.UpstreamBranch)
	if err != nil {
		return nil, err
	}
	return &Status{
		Fork:        f.Name,
		Repo:        f.Repo,
		Behind:      behind,
		Ahead:       ahead,
		UpstreamSHA: sha,
		NeedsMerge:  behind > 0,
	}, nil
}

// Merge attempts to merge upstream into a fresh branch. If dryRun is true,
// it reports what would happen without creating the branch.
// It never touches main.
func Merge(f Fork, dryRun bool) (*MergeResult, error) {
	result := &MergeResult{Fork: f.Name}
	dir := f.WorkDir

	// Ensure clean working tree on main
	if _, err := runGit(dir, "checkout", "main"); err != nil {
		return nil, fmt.Errorf("checkout main: %w", err)
	}
	status, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(status) != "" {
		return nil, fmt.Errorf("working tree dirty in %s — commit or stash first", dir)
	}

	behind, _, err := Divergence(f)
	if err != nil {
		return nil, err
	}
	result.UpstreamCommits = behind
	if behind == 0 {
		result.Report = fmt.Sprintf("%s: already up to date with upstream", f.Name)
		result.Ready = true // nothing to do = trivially ready
		return result, nil
	}

	upstreamSHA, err := runGit(dir, "rev-parse", "upstream/"+f.UpstreamBranch)
	if err != nil {
		return nil, err
	}
	result.UpstreamNewSHA = upstreamSHA

	if dryRun {
		// Show what would be merged without doing it
		log, _ := runGit(dir, "log", "--oneline", fmt.Sprintf("HEAD..upstream/%s", f.UpstreamBranch), "--max-count=10")
		result.Report = fmt.Sprintf("%s: would merge %d upstream commits onto a new branch.\nRecent:\n%s", f.Name, behind, log)
		return result, nil
	}

	branch := fmt.Sprintf("upstream-merge/%s", time.Now().Format("20060102-150405"))
	result.Branch = branch

	if _, err := runGit(dir, "checkout", "-b", branch); err != nil {
		return nil, fmt.Errorf("create branch: %w", err)
	}

	// Attempt merge
	mergeOut, mergeErr := runGit(dir, "merge", "--no-ff", "-m",
		fmt.Sprintf("merge upstream %s (%d commits)", f.UpstreamBranch, behind),
		"upstream/"+f.UpstreamBranch)

	if mergeErr != nil {
		// Collect conflict list
		conflictOut, _ := runGit(dir, "diff", "--name-only", "--diff-filter=U")
		for _, c := range strings.Split(conflictOut, "\n") {
			if strings.TrimSpace(c) != "" {
				result.Conflicts = append(result.Conflicts, strings.TrimSpace(c))
			}
		}
		result.Report = fmt.Sprintf(
			"%s: merge has %d conflicted files — needs human eyes.\nMerge output:\n%s\nConflicts:\n%s\n\n"+
				"Resolve manually, then run tests. See %s for our patch inventory.",
			f.Name, len(result.Conflicts), mergeOut,
			strings.Join(result.Conflicts, "\n"), f.PatchInventory)
		// Leave the branch with conflicts for human resolution; abort option documented
		return result, nil
	}

	result.Clean = true

	// Run tests
	if len(f.TestCmd) > 0 {
		// #nosec G204 -- TestCmd is an explicit user-configured command from the trailboss config file
		testCmd := exec.Command(f.TestCmd[0], f.TestCmd[1:]...)
		testCmd.Dir = dir
		testOut, testErr := testCmd.CombinedOutput()
		result.TestOutput = string(testOut)
		if testErr != nil {
			result.TestsPassed = false
			result.Report = fmt.Sprintf(
				"%s: merged clean (%d upstream commits) but TESTS FAILED on branch %s.\n%s",
				f.Name, behind, branch, result.TestOutput)
			return result, nil
		}
		result.TestsPassed = true
	}

	result.Ready = result.Clean && result.TestsPassed
	if result.Ready {
		result.Report = fmt.Sprintf(
			"✅ %s: merged %d upstream commits clean, all tests pass.\nBranch: %s\nReady for human approval — merge to main when ready.",
			f.Name, behind, branch)
	}
	return result, nil
}

// StateDir returns the directory for per-fork state files.
func StateDir() string {
	if d := os.Getenv("TRAILBOSS_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".trailboss", "upstream-state")
}

// LoadState loads the persisted state for a fork.
func LoadState(forkName string) (*ForkState, error) {
	path := filepath.Join(StateDir(), forkName+".json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is StateDir()+sanitized fork name, not user input
	if os.IsNotExist(err) {
		return &ForkState{}, nil
	}
	if err != nil {
		return nil, err
	}
	// minimal JSON parse without extra deps
	var s ForkState
	// Use encoding/json via a helper to avoid import cycle concerns
	if err := jsonUnmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveState persists the state for a fork.
func SaveState(forkName string, s *ForkState) error {
	dir := StateDir()
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	s.LastChecked = time.Now()
	data, err := jsonMarshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, forkName+".json"), data, 0600)
}
