package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/toxicwind/trailboss/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// directFake simulates git + sh without touching the network or a real repo.
// A "clone" materialises the target dir and records its origin URL so reuse
// paths behave; every other git subcommand returns canned output.
type directFake struct {
	mu        sync.Mutex
	calls     []directCall
	remotes   map[string]string // dir -> origin URL
	diffStats map[string]string // dir -> `git diff --cached --stat` output
	pushOut   string
	pushErr   error
	scriptErr error
	seenSign  []string // commit invocations' signing flags
}

type directCall struct {
	name string
	args []string
	env  []string
}

func newDirectFake() *directFake {
	return &directFake{remotes: map[string]string{}, diffStats: map[string]string{}}
}

func (f *directFake) run(_ context.Context, name string, args, env []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, directCall{name: name, args: append([]string(nil), args...), env: append([]string(nil), env...)})
	switch name {
	case "git":
		return f.git(args)
	case "sh":
		if f.scriptErr != nil {
			return nil, f.scriptErr
		}
		return []byte("script ok"), nil
	}
	return nil, errors.New("unexpected command " + name)
}

func (f *directFake) git(args []string) ([]byte, error) {
	// Strip leading git-level flags (-c ..., -C <dir>).
	rest := args
	var dir string
	for len(rest) > 0 && rest[0] == "-c" {
		rest = rest[2:]
	}
	if len(rest) > 0 && rest[0] == "-C" {
		dir, rest = rest[1], rest[2:]
	}
	if len(rest) == 0 {
		return nil, errors.New("git with no subcommand")
	}
	switch rest[0] {
	case "clone":
		target := rest[len(rest)-1]
		url := rest[len(rest)-2]
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, err
		}
		f.remotes[target] = url
		return []byte("cloned"), nil
	case "remote": // remote get-url origin
		u, ok := f.remotes[dir]
		if !ok {
			return nil, errors.New("no such remote")
		}
		return []byte(u + "\n"), nil
	case "fetch", "checkout", "reset", "add":
		return nil, nil
	case "symbolic-ref":
		return []byte("origin/main\n"), nil
	case "diff":
		return []byte(f.diffStats[dir]), nil
	case "commit":
		for _, a := range rest[1:] {
			if a == "-S" || a == "--no-gpg-sign" {
				f.seenSign = append(f.seenSign, a)
			}
		}
		return []byte("committed"), nil
	case "push":
		return []byte(f.pushOut), f.pushErr
	}
	return nil, errors.New("unexpected git subcommand " + rest[0])
}

func (f *directFake) argvJoined() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, c := range f.calls {
		b.WriteString(c.name)
		b.WriteByte(' ')
		b.WriteString(strings.Join(c.args, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func (f *directFake) envHas(name, val string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		for _, e := range c.env {
			if e == name+"="+val {
				return true
			}
		}
	}
	return false
}

func (f *directFake) envHasPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		for _, e := range c.env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
	}
	return false
}

func directSelection() models.Selection {
	return models.Selection{
		Owner:     "toxicwind",
		OwnerType: models.OwnerOrganization,
		Repos: []models.Repo{
			{Owner: "toxicwind", Name: "trailboss", CloneURL: "https://github.com/toxicwind/trailboss.git", DefaultBranch: "main"},
			{Owner: "toxicwind", Name: "roundup", CloneURL: "https://github.com/toxicwind/roundup.git", DefaultBranch: "main"},
		},
	}
}

func directSpec() models.ApplySpec {
	return models.ApplySpec{
		CommitMessage: "bump image",
		Script:        []string{"sed", "-i", "s|a|b|", "Dockerfile"},
		DryRun:        true,
		Sign:          models.SignNone,
	}
}

func directOpts(workDir string) DirectOpts {
	return DirectOpts{AllowedOwners: []string{"toxicwind"}, WorkDir: workDir, Concurrency: 2}
}

func TestDirectRequiresOwners(t *testing.T) {
	f := newDirectFake()
	_, err := Direct(context.Background(), f.run, directSelection(), directSpec(), "tok", DirectOpts{WorkDir: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--direct-owners")
	assert.Empty(t, f.calls, "no command may run before the allow-list gate")
}

func TestDirectRefusesForeignOwner(t *testing.T) {
	f := newDirectFake()
	sel := directSelection()
	sel.Repos = append(sel.Repos, models.Repo{Owner: "someone-else", Name: "x", DefaultBranch: "main"})
	_, err := Direct(context.Background(), f.run, sel, directSpec(), "tok", directOpts(t.TempDir()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "someone-else/x")
	assert.Contains(t, err.Error(), "allow-list")
	assert.Empty(t, f.calls, "a refused run clones nothing")
}

func TestDirectOwnerMatchIsCaseInsensitive(t *testing.T) {
	f := newDirectFake()
	// GitHub logins are case-insensitive; the gate must be too.
	_, err := Direct(context.Background(), f.run, directSelection(), directSpec(), "tok",
		DirectOpts{AllowedOwners: []string{"ToxicWind"}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	assert.NotEmpty(t, f.calls)
}

func TestDirectGuards(t *testing.T) {
	workDir := func() string { return t.TempDir() }
	cases := []struct {
		name string
		spec models.ApplySpec
		sel  models.Selection
		want string
	}{
		{"live needs confirm", func() models.ApplySpec { s := directSpec(); s.DryRun = false; return s }(), directSelection(), "Confirm"},
		{"bad sign mode", func() models.ApplySpec { s := directSpec(); s.Sign = "carrier-pigeon"; return s }(), directSelection(), "invalid signing mode"},
		{"github sign refused", func() models.ApplySpec { s := directSpec(); s.Sign = models.SignGitHub; return s }(), directSelection(), "PR-mode only"},
		{"empty selection", directSpec(), models.Selection{}, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDirectFake()
			_, err := Direct(context.Background(), f.run, tc.sel, tc.spec, "tok", directOpts(workDir()))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, f.calls)
		})
	}
}

func TestDirectTooManyRepos(t *testing.T) {
	f := newDirectFake()
	sel := directSelection()
	for i := 0; i < maxRepos; i++ {
		sel.Repos = append(sel.Repos, models.Repo{Owner: "toxicwind", Name: "r", DefaultBranch: "main"})
	}
	_, err := Direct(context.Background(), f.run, sel, directSpec(), "tok", directOpts(t.TempDir()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "above the")
	assert.Empty(t, f.calls)
}

func TestDirectTokenHygiene(t *testing.T) {
	f := newDirectFake()
	_, err := Direct(context.Background(), f.run, directSelection(), directSpec(), "secret-token", directOpts(t.TempDir()))
	require.NoError(t, err)

	// The token never appears in any argv (visible via ps).
	assert.NotContains(t, f.argvJoined(), "secret-token")
	// git children get the askpass helper + the mapped token var...
	assert.True(t, f.envHasPrefix("GIT_ASKPASS="), "git must authenticate via GIT_ASKPASS")
	assert.True(t, f.envHas(askpassTokenEnv, "secret-token"), "token mapped onto the askpass var")
	// ...and the TRAILBOSS_PAT source var is stripped everywhere.
	assert.False(t, f.envHasPrefix(models.TokenEnvVar+"="), "TRAILBOSS_PAT must not reach any child")
	// The user script runs with no token at all.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.name == "sh" {
			for _, e := range c.env {
				assert.NotContains(t, e, "secret-token", "user script env must carry no token")
				assert.False(t, strings.HasPrefix(e, models.TokenEnvVar+"="))
			}
		}
	}
}

func TestDirectDryRunReportsDiffStat(t *testing.T) {
	f := newDirectFake()
	workDir := t.TempDir()
	// Pre-seed one repo's diff stat; the other changes nothing.
	f.diffStats[filepath.Join(workDir, "toxicwind", "trailboss")] = " Dockerfile | 2 +-\n 1 file changed, 1 insertion(+), 1 deletion(-)\n"
	res, err := Direct(context.Background(), f.run, directSelection(), directSpec(), "tok", directOpts(workDir))
	require.NoError(t, err)
	require.Len(t, res.Outcomes, 2)
	// Outcomes stay in selection order regardless of worker finish order.
	assert.Equal(t, "toxicwind/trailboss", res.Outcomes[0].FullName)
	assert.Equal(t, "toxicwind/roundup", res.Outcomes[1].FullName)
	assert.True(t, res.Outcomes[0].DryRun)
	assert.False(t, res.Outcomes[0].Pushed, "dry-run pushes nothing")
	assert.Contains(t, res.Outcomes[0].DiffStat, "Dockerfile")
	assert.Empty(t, res.Outcomes[0].Err)
	assert.Empty(t, res.Outcomes[1].DiffStat, "no-change repo reports empty")
	assert.Empty(t, res.Outcomes[1].Err)
	// No commit or push ran in a dry-run.
	assert.NotContains(t, f.argvJoined(), " commit ")
	assert.NotContains(t, f.argvJoined(), " push ")
}

func TestDirectLivePushesAndSigns(t *testing.T) {
	f := newDirectFake()
	workDir := t.TempDir()
	for _, n := range []string{"trailboss", "roundup"} {
		f.diffStats[filepath.Join(workDir, "toxicwind", n)] = " README.md | 2 +-\n"
	}
	spec := directSpec()
	spec.DryRun = false
	spec.Confirm = true
	spec.Sign = models.SignLocal
	res, err := Direct(context.Background(), f.run, sel2(), spec, "tok", directOpts(workDir))
	require.NoError(t, err)
	for _, o := range res.Outcomes {
		assert.True(t, o.Pushed, o.FullName)
		assert.False(t, o.DryRun)
		assert.Empty(t, o.Err)
	}
	assert.Contains(t, f.seenSign, "-S", "SignLocal must pass -S to git commit")
	assert.NotContains(t, f.argvJoined(), "--force", "never force-push")
}

func TestDirectSignNonePassesNoGpgSign(t *testing.T) {
	f := newDirectFake()
	workDir := t.TempDir()
	f.diffStats[filepath.Join(workDir, "toxicwind", "trailboss")] = " x | 1 +\n"
	spec := directSpec()
	spec.DryRun = false
	spec.Confirm = true // SignNone already the default in directSpec
	sel := directSelection()
	sel.Repos = sel.Repos[:1]
	_, err := Direct(context.Background(), f.run, sel, spec, "tok", directOpts(workDir))
	require.NoError(t, err)
	assert.Contains(t, f.seenSign, "--no-gpg-sign")
}

func TestDirectNonFastForwardRecordedNotForced(t *testing.T) {
	f := newDirectFake()
	f.pushErr = errors.New("exit status 1")
	f.pushOut = "To https://github.com/toxicwind/trailboss.git\n ! [rejected]  main -> main (non-fast-forward)\n"
	workDir := t.TempDir()
	f.diffStats[filepath.Join(workDir, "toxicwind", "trailboss")] = " x | 1 +\n"
	spec := directSpec()
	spec.DryRun = false
	spec.Confirm = true
	sel := directSelection()
	sel.Repos = sel.Repos[:1]
	res, err := Direct(context.Background(), f.run, sel, spec, "tok", directOpts(workDir))
	// The run itself succeeds — the rejection is a per-repo outcome, and the
	// run continues (a second repo would still be attempted).
	require.NoError(t, err)
	require.Len(t, res.Outcomes, 1)
	assert.False(t, res.Outcomes[0].Pushed)
	assert.Contains(t, res.Outcomes[0].Err, "non-fast-forward")
	assert.Contains(t, res.Outcomes[0].Err, "never force-push")
	assert.NotContains(t, f.argvJoined(), "--force")
}

func TestDirectCloneURLFallback(t *testing.T) {
	f := newDirectFake()
	sel := models.Selection{
		Owner: "toxicwind",
		Repos: []models.Repo{{Owner: "toxicwind", Name: "bare", DefaultBranch: "main"}}, // no CloneURL
	}
	_, err := Direct(context.Background(), f.run, sel, directSpec(), "tok", directOpts(t.TempDir()))
	require.NoError(t, err)
	assert.Contains(t, f.argvJoined(), "https://github.com/toxicwind/bare.git")
}

func TestDirectRejectsUnsafeName(t *testing.T) {
	f := newDirectFake()
	sel := models.Selection{
		Owner: "toxicwind",
		Repos: []models.Repo{{Owner: "toxicwind", Name: "../evil", DefaultBranch: "main"}},
	}
	res, err := Direct(context.Background(), f.run, sel, directSpec(), "tok", directOpts(t.TempDir()))
	require.NoError(t, err, "a bad repo is a per-repo refusal, not a run failure")
	require.Len(t, res.Outcomes, 1)
	assert.Contains(t, res.Outcomes[0].Err, "unsafe owner/name")
	assert.Empty(t, f.calls)
}

func TestDirectScriptFailureRecorded(t *testing.T) {
	f := newDirectFake()
	f.scriptErr = errors.New("exit status 1")
	res, err := Direct(context.Background(), f.run, directSelection(), directSpec(), "tok", directOpts(t.TempDir()))
	require.NoError(t, err)
	require.Len(t, res.Outcomes, 2)
	for _, o := range res.Outcomes {
		assert.Contains(t, o.Err, "script failed")
		assert.False(t, o.Pushed)
	}
}

// sel2 is directSelection under a shorter name for the table above; kept
// separate so the shared fixture stays obvious.
func sel2() models.Selection { return directSelection() }

func TestDirectReuseExistingCheckout(t *testing.T) {	f := newDirectFake()
	workDir := t.TempDir()
	dir := filepath.Join(workDir, "toxicwind", "trailboss")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	f.remotes[dir] = "https://github.com/toxicwind/trailboss.git"
	sel := directSelection()
	sel.Repos = sel.Repos[:1]
	_, err := Direct(context.Background(), f.run, sel, directSpec(), "tok", directOpts(workDir))
	require.NoError(t, err)
	joined := f.argvJoined()
	assert.NotContains(t, joined, " clone ", "matching remote means reuse, not re-clone")
	assert.Contains(t, joined, " fetch ", "reuse still fetches")
	assert.Contains(t, joined, "reset --hard", "reuse resets to the remote tip")
}

// TestDirectFreshCloneChecksOutDefaultBranch asserts the unified discipline:
// a fresh clone goes through the same fetch → checkout → reset to the
// lockfile's default branch as a reused checkout.
func TestDirectFreshCloneChecksOutDefaultBranch(t *testing.T) {
	f := newDirectFake()
	workDir := t.TempDir()
	res, err := Direct(context.Background(), f.run, directSelection(), directSpec(), "tok", directOpts(workDir))
	require.NoError(t, err)
	require.Len(t, res.Outcomes, 2)
	joined := f.argvJoined()
	assert.Contains(t, joined, " clone ", "a fresh workdir is cloned")
	assert.Contains(t, joined, " checkout main\n", "a fresh clone checks out the recorded default branch")
	assert.Contains(t, joined, "reset --hard origin/main", "a fresh clone resets to the remote tip")
}

func TestDirectRefusesForeignRemote(t *testing.T) {
	f := newDirectFake()
	workDir := t.TempDir()
	dir := filepath.Join(workDir, "toxicwind", "trailboss")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	f.remotes[dir] = "https://github.com/someone-else/trailboss.git"
	sel := directSelection()
	sel.Repos = sel.Repos[:1]
	res, err := Direct(context.Background(), f.run, sel, directSpec(), "tok", directOpts(workDir))
	require.NoError(t, err)
	require.Len(t, res.Outcomes, 1)
	assert.Contains(t, res.Outcomes[0].Err, "origin remote")
	assert.NotContains(t, f.argvJoined(), "reset --hard", "a foreign checkout is never reset")
}
