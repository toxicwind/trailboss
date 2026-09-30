package upstream

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// setupTestRepo creates a temp git repo pair: "upstream" with commits and a
// "fork" cloned from it, then adds divergent commits to each.
func setupTestRepo(t *testing.T) (upstreamDir, forkDir string) {
	t.Helper()
	base := t.TempDir()

	run := func(dir string, args ...string) {
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

	// Upstream repo
	upstreamDir = filepath.Join(base, "upstream")
	if err := os.MkdirAll(upstreamDir, 0755); err != nil {
		t.Fatal(err)
	}
	run(upstreamDir, "init", "-b", "main")
	os.WriteFile(filepath.Join(upstreamDir, "base.txt"), []byte("base\n"), 0644)
	run(upstreamDir, "add", ".")
	run(upstreamDir, "commit", "-m", "initial")

	// Fork = clone of upstream at this point
	forkDir = filepath.Join(base, "fork")
	cmd := exec.Command("git", "clone", upstreamDir, forkDir)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	// Divergent upstream commit (touches a file the fork doesn't)
	os.WriteFile(filepath.Join(upstreamDir, "upstream-new.txt"), []byte("new\n"), 0644)
	run(upstreamDir, "add", ".")
	run(upstreamDir, "commit", "-m", "upstream adds file")

	// Divergent fork commit (our "patch")
	os.WriteFile(filepath.Join(forkDir, "our-patch.txt"), []byte("patch\n"), 0644)
	run(forkDir, "add", ".")
	run(forkDir, "commit", "-m", "our patch")
	run(forkDir, "config", "user.email", "test@test")
	run(forkDir, "config", "user.name", "test")

	return upstreamDir, forkDir
}

func TestEnsureUpstreamRemote(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: upstreamDir, UpstreamBranch: "main"}

	if err := EnsureUpstreamRemote(f); err != nil {
		t.Fatalf("EnsureUpstreamRemote: %v", err)
	}
	// Idempotent: second call should be a no-op
	if err := EnsureUpstreamRemote(f); err != nil {
		t.Fatalf("EnsureUpstreamRemote (2nd): %v", err)
	}
}

func TestDivergence(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: upstreamDir, UpstreamBranch: "main"}

	behind, ahead, err := Divergence(f)
	if err != nil {
		t.Fatalf("Divergence: %v", err)
	}
	if behind != 1 {
		t.Errorf("behind = %d, want 1", behind)
	}
	if ahead != 1 {
		t.Errorf("ahead = %d, want 1", ahead)
	}
}

func TestMergeClean(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{
		Name: "test", WorkDir: forkDir,
		UpstreamURL: upstreamDir, UpstreamBranch: "main",
		TestCmd: []string{"true"}, // no-op test
	}

	result, err := Merge(f, false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !result.Clean {
		t.Errorf("expected clean merge, got conflicts: %v", result.Conflicts)
	}
	if !result.Ready {
		t.Errorf("expected ready, report: %s", result.Report)
	}
	if result.Branch == "" {
		t.Error("expected branch name to be set")
	}
	// Our patch file must survive the merge
	if _, err := os.Stat(filepath.Join(forkDir, "our-patch.txt")); err != nil {
		t.Errorf("our patch file lost in merge: %v", err)
	}
	// Upstream's new file must be present
	if _, err := os.Stat(filepath.Join(forkDir, "upstream-new.txt")); err != nil {
		t.Errorf("upstream file missing after merge: %v", err)
	}
}

func TestMergeDryRun(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)
	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: upstreamDir, UpstreamBranch: "main"}

	result, err := Merge(f, true)
	if err != nil {
		t.Fatalf("Merge dry-run: %v", err)
	}
	if result.Branch != "" {
		t.Errorf("dry-run should not create a branch, got %s", result.Branch)
	}
	if result.UpstreamCommits != 1 {
		t.Errorf("UpstreamCommits = %d, want 1", result.UpstreamCommits)
	}
}

func TestMergeConflict(t *testing.T) {
	upstreamDir, forkDir := setupTestRepo(t)

	// Create a genuine conflict: both modify base.txt differently
	run := func(dir string, args ...string) {
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
	os.WriteFile(filepath.Join(upstreamDir, "base.txt"), []byte("upstream version\n"), 0644)
	run(upstreamDir, "add", ".")
	run(upstreamDir, "commit", "-m", "upstream edits base")
	os.WriteFile(filepath.Join(forkDir, "base.txt"), []byte("our version\n"), 0644)
	run(forkDir, "add", ".")
	run(forkDir, "commit", "-m", "we edit base")

	f := Fork{Name: "test", WorkDir: forkDir, UpstreamURL: upstreamDir, UpstreamBranch: "main"}

	result, err := Merge(f, false)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.Clean {
		t.Error("expected conflicts, got clean merge")
	}
	if len(result.Conflicts) == 0 {
		t.Error("expected conflict list to be non-empty")
	}
	if result.Ready {
		t.Error("conflicted merge should not be ready")
	}
}

func TestStateRoundtrip(t *testing.T) {
	t.Setenv("TRAILBOSS_STATE_DIR", t.TempDir())
	s := &ForkState{LastMergedSHA: "abc123", LastResult: "clean", LastBranch: "upstream-merge/x"}
	if err := SaveState("testfork", s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := LoadState("testfork")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.LastMergedSHA != "abc123" || loaded.LastResult != "clean" {
		t.Errorf("state mismatch: %+v", loaded)
	}
}
