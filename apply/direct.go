// Direct mode: run a change across a selection by driving the git binary
// itself — clone → script → commit → push straight to each repo's default
// branch. No multi-gitter, no PRs.
//
// This is the deliberate, gated exception to trailboss's "never runs git
// itself" rule: PR mode still delegates everything to multi-gitter, but
// --mode=direct exists for fleets the operator owns outright, where opening a
// PR per repo is ceremony with no reviewer on the other side. The safety gate
// is DirectOpts.AllowedOwners: EVERY repo's owner must be allow-listed or the
// whole run refuses before touching anything. The usual charter guards still
// hold at this boundary — a live run needs Confirm, every run names a signing
// mode (--sign github is refused here: GitHub-signing is PR-mode only, a
// direct push needs a local key or explicit none) — and a push is never
// forced: a non-fast-forward rejection is recorded per repo and the run moves
// on.
//
// Token hygiene mirrors the PR path: the PAT never appears in argv (it would
// be visible in ps). git authenticates through a temp GIT_ASKPASS helper that
// reads the token from a mapped environment variable; the TRAILBOSS_PAT source
// var is stripped from the child environment. The user's script runs with no
// token in its environment at all.
package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/toxicwind/trailboss/models"
)

// askpassTokenEnv is the environment variable the GIT_ASKPASS helper reads the
// token from. trailboss maps the operator's TRAILBOSS_PAT onto it for git
// child processes and strips TRAILBOSS_PAT itself, so the raw PAT never
// reaches a child under its well-known name.
const askpassTokenEnv = "TRAILBOSS_ASKPASS_TOKEN" //nolint:gosec // G101: env var name, not a credential.

// defaultDirectConcurrency bounds how many repos one batch works at once when
// the caller leaves DirectOpts.Concurrency unset.
const defaultDirectConcurrency = 4

// segmentPattern mirrors the lockfile's owner/repo grammar (see the selection
// package): a Direct workdir is laid out as <workdir>/<owner>/<name>, so a
// hostile or malformed lockfile must not be able to smuggle a path separator
// into the join.
var directSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// DirectOpts tunes a direct-mode run.
type DirectOpts struct {
	// AllowedOwners is REQUIRED and non-empty: the safety gate. Every repo's
	// owner must be in this list (compared case-insensitively — GitHub logins
	// are) or Direct refuses the whole run before cloning anything. Direct
	// push exists for orgs/users the operator owns outright; this list is what
	// makes that explicit.
	AllowedOwners []string
	// WorkDir holds the per-run clones at <workdir>/<owner>/<name>. Empty
	// means a fresh temp dir that Direct removes when the run ends. A
	// caller-supplied dir is reused across runs: a repo already cloned there
	// with the right origin remote is fetched and reset to the remote's
	// default-branch tip rather than re-cloned.
	WorkDir string
	// Concurrency bounds the worker pool inside one batch. <= 0 means
	// defaultDirectConcurrency.
	Concurrency int
}

// RepoOutcome is one repo's observable result from a direct run.
type RepoOutcome struct {
	FullName string `json:"fullName"`
	// Pushed is true only when the commit actually landed on the remote. A
	// dry-run, a no-change repo, and a failed repo all report false.
	Pushed bool `json:"pushed"`
	DryRun bool `json:"dryRun"`
	// DiffStat is the `git diff --stat` of what the script changed (staged, in
	// a dry-run; committed, in a live run). Empty when the script changed
	// nothing.
	DiffStat string `json:"diffStat,omitempty"`
	// Err carries a per-repo failure — clone, script, commit, or push — as a
	// human sentence. The run continues with the remaining repos; a non-empty
	// Err never aborts its batch.
	Err string `json:"err,omitempty"`
}

// DirectResult is the observable output of a direct run: one outcome per repo
// in selection order, across all batches.
type DirectResult struct {
	Outcomes []RepoOutcome `json:"outcomes"`
}

// Direct clones each repo in s, runs spec's script in the checkout, and pushes
// the result straight to the repo's default branch. See the package doc above
// for the safety model. The token travels via the child environment only.
func Direct(ctx context.Context, run Runner, s models.Selection, spec models.ApplySpec, token string, opts DirectOpts) (DirectResult, error) {
	// (1) The owner allow-list gate — the whole point of direct mode's safety
	// model. Refuse before any clone, script write, or token mapping.
	if len(opts.AllowedOwners) == 0 {
		return DirectResult{}, errors.New("refusing a direct apply: no --direct-owners allow-list — direct push only targets explicitly allow-listed owners")
	}
	allowed := make(map[string]bool, len(opts.AllowedOwners))
	for _, o := range opts.AllowedOwners {
		allowed[strings.ToLower(o)] = true
	}
	var refused []string
	for _, r := range s.Repos {
		if !allowed[strings.ToLower(r.Owner)] {
			refused = append(refused, r.FullName())
		}
	}
	if len(refused) > 0 {
		return DirectResult{}, fmt.Errorf("refusing a direct apply: %d repo(s) owned outside the --direct-owners allow-list (%s): %s",
			len(refused), strings.Join(opts.AllowedOwners, ", "), strings.Join(refused, ", "))
	}

	// (2) Charter guards, same as Apply: a live run (pushes commits) must be
	// explicitly confirmed, and every run must name a recognised signing mode.
	if !spec.DryRun && !spec.Confirm {
		return DirectResult{}, errors.New("refusing a live direct apply: DryRun is false but Confirm is false — a real run that pushes commits must be explicitly confirmed")
	}
	if !models.IsValidSignMode(spec.Sign) {
		return DirectResult{}, fmt.Errorf("invalid signing mode %q: must be one of %s (your GPG key), %s (unsigned) — %q is PR-mode only and refused for direct push",
			spec.Sign, models.SignLocal, models.SignNone, models.SignGitHub)
	}
	if spec.Sign == models.SignGitHub {
		return DirectResult{}, fmt.Errorf("refusing a direct apply: --sign %q signs via multi-gitter's GitHub-API push, which is PR-mode only — direct push needs --sign %q (your GPG key) or --sign %q (explicitly unsigned)",
			models.SignGitHub, models.SignLocal, models.SignNone)
	}

	if len(s.Repos) == 0 {
		return DirectResult{}, errors.New("selection is empty — nothing to apply")
	}
	if len(s.Repos) > maxRepos {
		return DirectResult{}, fmt.Errorf("selection has %d repos, above the %d-repo limit for a single apply; narrow the selection", len(s.Repos), maxRepos)
	}
	if token == "" {
		return DirectResult{}, errors.New("refusing a direct apply: empty token — git authentication would fail on every clone")
	}

	workDir := opts.WorkDir
	cleanupWorkDir := func() {}
	if workDir == "" {
		tmp, err := os.MkdirTemp("", "trailboss-direct-*")
		if err != nil {
			return DirectResult{}, fmt.Errorf("create direct workdir: %w", err)
		}
		workDir = tmp
		cleanupWorkDir = func() { _ = os.RemoveAll(tmp) }
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		return DirectResult{}, fmt.Errorf("create direct workdir %s: %w", workDir, err)
	}
	defer cleanupWorkDir()

	scriptPath, scriptCleanup, err := writeScript(spec.Script)
	if err != nil {
		return DirectResult{}, err
	}
	defer scriptCleanup()

	askPath, askCleanup, err := writeAskpass()
	if err != nil {
		return DirectResult{}, err
	}
	defer askCleanup()

	// Map the PAT onto the askpass helper's own var and strip the source var
	// (plus any stale askpass mapping) so the raw PAT never reaches a child
	// under its well-known name. GIT_TERMINAL_PROMPT=0 keeps git from ever
	// falling back to an interactive prompt mid-run.
	gitEnv := overrideEnv(os.Environ(), "GIT_ASKPASS", askPath, models.TokenEnvVar, askpassTokenEnv)
	gitEnv = append(gitEnv, askpassTokenEnv+"="+token, "GIT_TERMINAL_PROMPT=0")
	// The user's script gets no token at all — unlike the PR path, nothing in
	// direct mode needs the script to talk to GitHub.
	scriptEnv := stripEnv(os.Environ(), models.TokenEnvVar, askpassTokenEnv)

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultDirectConcurrency
	}
	d := &directRun{
		run:         run,
		spec:        spec,
		workDir:     workDir,
		scriptPath:  scriptPath,
		gitEnv:      gitEnv,
		scriptEnv:   scriptEnv,
		concurrency: concurrency,
	}

	// Batch like the PR path so a large fleet's pushes spread out under
	// GitHub's secondary rate limits; a pause between batches, same semantics.
	var result DirectResult
	batches := chunk(s.Repos, spec.BatchSize)
	for i, repos := range batches {
		if i > 0 && spec.BatchPause > 0 {
			sleep(spec.BatchPause)
		}
		result.Outcomes = append(result.Outcomes, d.runBatch(ctx, repos)...)
	}
	return result, nil
}

// directRun carries the per-run state for Direct's worker pool.
type directRun struct {
	run         Runner
	spec        models.ApplySpec
	workDir     string
	scriptPath  string
	gitEnv      []string
	scriptEnv   []string
	concurrency int
}

// runBatch works one batch with a bounded worker pool, collecting outcomes in
// selection order: outcomes[i] always describes repos[i], whatever the finish
// order.
func (d *directRun) runBatch(ctx context.Context, repos []models.Repo) []RepoOutcome {
	outcomes := make([]RepoOutcome, len(repos))
	sem := make(chan struct{}, d.concurrency)
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outcomes[i] = RepoOutcome{FullName: r.FullName(), DryRun: d.spec.DryRun, Err: "cancelled before start: " + ctx.Err().Error()}
				return
			}
			outcomes[i] = d.applyOne(ctx, r)
		}()
	}
	wg.Wait()
	return outcomes
}

// applyOne drives a single repo through clone/fetch → script → stage →
// (dry-run: report | live: commit → push). A per-repo failure is recorded in
// the outcome, never returned: one bad repo must not sink its batch.
func (d *directRun) applyOne(ctx context.Context, r models.Repo) RepoOutcome {
	out := RepoOutcome{FullName: r.FullName(), DryRun: d.spec.DryRun}
	if !directSegmentPattern.MatchString(r.Owner) || !directSegmentPattern.MatchString(r.Name) {
		out.Err = fmt.Sprintf("refusing repo with unsafe owner/name %q — expected letters, digits, '.', '_' or '-'", r.FullName())
		return out
	}
	dir := filepath.Join(d.workDir, r.Owner, r.Name)
	if err := d.ensureCheckout(ctx, r, dir); err != nil {
		out.Err = err.Error()
		return out
	}
	branch, err := d.defaultBranch(ctx, r, dir)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	// Run the user script with the checkout as its working directory,
	// mirroring multi-gitter's "script runs in the repo" contract.
	if _, err := d.run(ctx, "sh", []string{"-c", "cd " + shellQuote(dir) + " && exec " + shellQuote(d.scriptPath)}, d.scriptEnv); err != nil {
		out.Err = "script failed: " + runErrText(err)
		return out
	}
	if _, err := d.git(ctx, dir, "add", "-A"); err != nil {
		out.Err = "git add failed: " + runErrText(err)
		return out
	}
	// `git diff --cached --stat` exits 0 either way; empty output means the
	// script changed nothing — no exit-code inspection needed, which keeps
	// the Runner seam honest.
	stat, err := d.git(ctx, dir, "diff", "--cached", "--stat")
	if err != nil {
		out.Err = "git diff failed: " + runErrText(err)
		return out
	}
	out.DiffStat = strings.TrimSpace(string(stat))
	if out.DiffStat == "" {
		return out // no changes: nothing to commit or push.
	}
	if d.spec.DryRun {
		return out // Pushed stays false; DiffStat is the dry-run's result.
	}
	commitArgs := []string{"commit", "-m", d.spec.CommitMessage}
	switch d.spec.Sign {
	case models.SignLocal:
		// -S signs with the operator's own GPG key via their git config,
		// the same trust model as the PR path's --git-type=cmd.
		commitArgs = append(commitArgs, "-S")
	case models.SignNone:
		// Explicit unsigned: --no-gpg-sign wins even if the operator's
		// gitconfig auto-signs (commit.gpgsign=true).
		commitArgs = append(commitArgs, "--no-gpg-sign")
	}
	if _, err := d.git(ctx, dir, commitArgs...); err != nil {
		out.Err = "git commit failed: " + runErrText(err)
		return out
	}
	pushOut, err := d.git(ctx, dir, "push", "origin", branch)
	if err != nil {
		// Fetch-first discipline: we fetched before checkout, so a rejection
		// here means the remote moved under us. Record it and continue —
		// trailboss never force-pushes, ever.
		if isNonFastForward(pushOut) {
			out.Err = "push rejected: remote branch moved (non-fast-forward) — re-run to retry; trailboss never force-pushes"
		} else {
			out.Err = "git push failed: " + firstLine(pushOut, runErrText(err))
		}
		return out
	}
	out.Pushed = true
	return out
}

// ensureCheckout gets dir into a clean checkout of the repo: a fresh clone
// when absent, or — when the dir already holds a clone whose origin remote
// matches the lockfile URL — a fetch plus a hard reset to the remote's
// default-branch tip. A dir whose origin points elsewhere is refused rather
// than clobbered.
func (d *directRun) ensureCheckout(ctx context.Context, r models.Repo, dir string) error {
	url := r.CloneURL
	if url == "" {
		url = "https://github.com/" + r.Owner + "/" + r.Name + ".git"
	}
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory — clear it or pick another --direct-workdir", dir)
		}
		got, err := d.git(ctx, dir, "remote", "get-url", "origin")
		if err != nil {
			return fmt.Errorf("%s exists but has no usable origin remote — clear it or pick another --direct-workdir: %s", dir, runErrText(err))
		}
		if strings.TrimSpace(string(got)) != url {
			return fmt.Errorf("refusing to touch %s: its origin remote is %q, expected %q", dir, strings.TrimSpace(string(got)), url)
		}
	} else if _, err := d.run(ctx, "git", []string{"-c", "credential.helper=", "clone", url, dir}, d.gitEnv); err != nil {
		return fmt.Errorf("git clone failed for %s: %s", r.FullName(), runErrText(err))
	}
	// One discipline for fresh and reused clones alike: fetch, check out the
	// lockfile's default branch, and hard-reset to the remote tip — so the
	// script always starts pristine on the recorded branch, even when the
	// clone's own origin/HEAD disagrees with the lockfile.
	branch, err := d.defaultBranch(ctx, r, dir)
	if err != nil {
		return err
	}
	if _, err := d.git(ctx, dir, "fetch", "origin"); err != nil {
		return fmt.Errorf("git fetch failed for %s: %s", r.FullName(), runErrText(err))
	}
	if _, err := d.git(ctx, dir, "checkout", branch); err != nil {
		return fmt.Errorf("git checkout %s failed for %s: %s", branch, r.FullName(), runErrText(err))
	}
	// The workdir is trailboss's scratch: reset to the remote tip so a
	// re-run (or a previous interrupted run) always starts pristine.
	if _, err := d.git(ctx, dir, "reset", "--hard", "origin/"+branch); err != nil {
		return fmt.Errorf("git reset failed for %s: %s", r.FullName(), runErrText(err))
	}
	return nil
}

// defaultBranch resolves the branch to push: the lockfile's recorded default,
// or — for a hand-built selection without one — the live origin/HEAD. It
// never guesses.
func (d *directRun) defaultBranch(ctx context.Context, r models.Repo, dir string) (string, error) {
	if r.DefaultBranch != "" {
		return r.DefaultBranch, nil
	}
	out, err := d.git(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("cannot determine the default branch for %s: the lockfile records none and origin/HEAD is unset", r.FullName())
	}
	if branch, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "origin/"); ok && branch != "" {
		return branch, nil
	}
	return "", fmt.Errorf("cannot determine the default branch for %s: unexpected origin/HEAD %q", r.FullName(), strings.TrimSpace(string(out)))
}

// git runs the git binary with a deterministic, non-interactive config:
// credential helpers are disabled (the askpass env is the single auth path),
// -C pins the working directory (the Runner seam carries no Dir), and the
// mapped-token environment keeps auth out of argv.
func (d *directRun) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-c", "credential.helper=", "-C", dir}, args...)
	return d.run(ctx, "git", full, d.gitEnv)
}

// writeAskpass writes the temp GIT_ASKPASS helper: a tiny POSIX script that
// prints the token from its environment. The file carries no secret — only
// the variable name — so 0700 is plenty; the token itself never touches disk.
func writeAskpass() (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "trailboss-askpass-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("create askpass helper: %w", err)
	}
	// printf '%s' (not echo): safe for tokens with leading dashes or
	// backslashes. :? fails loudly if the mapping is ever missing.
	content := "#!/bin/sh\n# trailboss direct-mode GIT_ASKPASS helper — prints the token from the\n# environment. git invokes this instead of prompting; the token never appears\n# in argv, so it cannot leak into process listings.\nexec printf '%s' \"${" + askpassTokenEnv + ":?trailboss: askpass token not in environment}\"\n"
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("write askpass helper: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("close askpass helper: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil { //nolint:gosec // G302: the helper must be executable (git runs it); 0700 keeps it owner-only.
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("chmod askpass helper: %w", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// stripEnv returns base with every var named in drop removed.
func stripEnv(base []string, drop ...string) []string {
	strip := make(map[string]bool, len(drop))
	for _, d := range drop {
		strip[d] = true
	}
	out := make([]string, 0, len(base))
	for _, e := range base {
		name := e[:strings.IndexByte(e+"=", '=')]
		if !strip[name] {
			out = append(out, e)
		}
	}
	return out
}

// isNonFastForward reports whether git push's output describes a
// non-fast-forward rejection — the case the fetch-first discipline handles by
// recording and continuing, never by forcing.
func isNonFastForward(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "non-fast-forward") || strings.Contains(s, "[rejected]")
}

// runErrText renders a Runner error without the token: Runner errors from the
// real exec path never contain it (it travels via env), and the message is
// what the operator debugs with.
func runErrText(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

// firstLine prefers the tool's own output for a failure sentence, falling back
// to the Go error when the tool said nothing.
func firstLine(out []byte, fallback string) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return fallback
}
