package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/toxicwind/trailboss/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryAddGetList(t *testing.T) {
	r := newRegistry()
	j1 := r.add("mirror")
	j2 := r.add("scan")
	require.NotEqual(t, j1.ID, j2.ID)
	assert.Equal(t, j1, r.get(j1.ID))
	assert.Nil(t, r.get("nope"))
	list := r.list()
	require.Len(t, list, 2)
	assert.Equal(t, j2.ID, list[0].ID, "newest first")
	assert.Equal(t, JobQueued, list[0].Status)
}

func TestJobLogSubscribeAndTerminal(t *testing.T) {
	r := newRegistry()
	j := r.add("scan")
	sub := j.subscribe()
	j.logLine("hello")
	select {
	case line := <-sub:
		assert.Equal(t, "hello", line)
	case <-time.After(2 * time.Second):
		t.Fatal("no log line delivered to subscriber")
	}
	j.setStatus(JobDone)
	// Terminal delivery: a control frame, then the channel closes.
	select {
	case msg, ok := <-sub:
		require.True(t, ok, "control frame must arrive before close")
		assert.True(t, len(msg) > 0 && msg[0] == 0, "terminal delivery is a control frame")
	case <-time.After(2 * time.Second):
		t.Fatal("no terminal frame delivered")
	}
	select {
	case _, ok := <-sub:
		assert.False(t, ok, "channel closes after terminal delivery")
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after terminal status")
	}
	assert.True(t, j.Status.terminal())
	assert.False(t, JobRunning.terminal())
}

func TestSubscribeLive(t *testing.T) {
	r := newRegistry()
	j := r.add("scan")
	j.setStatus(JobRunning)
	j.logLine("one")
	j.logLine("two")

	// Atomic capture: 2 lines so far, still running.
	sub, n, status := j.subscribeLive()
	require.NotNil(t, sub)
	assert.Equal(t, 2, n)
	assert.Equal(t, JobRunning, status)
	assert.Equal(t, []string{"one", "two"}, j.logPrefix(n))

	// Lines logged after the capture arrive only via the channel.
	j.logLine("three")
	select {
	case line := <-sub:
		assert.Equal(t, "three", line)
	case <-time.After(2 * time.Second):
		t.Fatal("live line not delivered")
	}

	// A terminal job yields no subscription but reports its status.
	j.setStatus(JobDone)
	sub, n, status = j.subscribeLive()
	assert.Nil(t, sub, "no subscription for a terminal job")
	assert.True(t, status.terminal())
	assert.Equal(t, 3, n)
}

func TestResolveSelectionRef(t *testing.T) {
	// Unknown bare names are refused: only known entries resolve.
	_, err := resolveSelectionRef("my-herd")
	require.Error(t, err)

	// A lockfile in the working directory resolves by its bare name, and the
	// returned path is absolute.
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("demo.selection", []byte("{}"), 0o644))
	p, err := resolveSelectionRef("demo")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "demo.selection"), p)

	// Explicit lockfile paths stay local to the working directory.
	p, err = resolveSelectionRef("trailboss.selection")
	require.NoError(t, err)
	assert.Equal(t, "trailboss.selection", p)

	// Escapes are refused.
	for _, bad := range []string{"", "/etc/passwd", "../x.selection", "a/../../b"} {
		_, err := resolveSelectionRef(bad)
		assert.Error(t, err, bad)
	}

	// Workspace entries resolve under the workspace root when present, and a
	// "workspace/" reference never falls through to a CWD-relative path.
	ws, err := workspaceRoot()
	require.NoError(t, err)
	p, err = resolveSelectionRef("workspace/smoke")
	if _, statErr := os.Stat(filepath.Join(ws, "smoke.selection")); statErr == nil {
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(ws, "smoke.selection"), p)
	} else {
		require.Error(t, err, "a missing workspace selection must error, not fall through to CWD")
	}
	for _, bad := range []string{"workspace/", "workspace/.", "workspace/..", "workspace/../evil", `workspace/a\b`} {
		_, err := resolveSelectionRef(bad)
		assert.Error(t, err, bad)
	}
}

func applyReq() jobRequest {
	return jobRequest{
		Kind:          "apply",
		Selection:     "x",
		Script:        []string{"true"},
		CommitMessage: "msg",
		Branch:        "bump",
		Sign:          models.SignNone,
	}
}

func nonEmptySelection() models.Selection {
	return models.Selection{Repos: []models.Repo{{Owner: "o", Name: "n"}}}
}

func TestValidateJobRequest(t *testing.T) {
	sel := nonEmptySelection()
	require.NoError(t, validateJobRequest(jobRequest{Kind: "mirror", Selection: "x"}, sel))
	require.NoError(t, validateJobRequest(jobRequest{Kind: "scan", Selection: "x", Pattern: "foo"}, sel))
	require.NoError(t, validateJobRequest(applyReq(), sel))

	bad := []struct {
		name string
		req  jobRequest
		want string
	}{
		{"scan needs pattern", jobRequest{Kind: "scan", Selection: "x"}, "pattern"},
		{"apply needs script", func() jobRequest { r := applyReq(); r.Script = nil; return r }(), "script"},
		{"apply needs commit message", func() jobRequest { r := applyReq(); r.CommitMessage = ""; return r }(), "commitMessage"},
		{"apply needs sign", func() jobRequest { r := applyReq(); r.Sign = "maybe"; return r }(), "sign"},
		{"pr needs branch", func() jobRequest { r := applyReq(); r.Branch = ""; return r }(), "branch"},
		{"direct needs owners", func() jobRequest { r := applyReq(); r.Mode = "direct"; r.Owners = nil; return r }(), "owners"},
		{"direct refuses github sign", func() jobRequest {
			r := applyReq(); r.Mode = "direct"; r.Owners = []string{"o"}; r.Sign = models.SignGitHub; return r
		}(), "PR-mode only"},
		{"bad mode", func() jobRequest { r := applyReq(); r.Mode = "carrier"; return r }(), "mode"},
		{"live needs confirm", func() jobRequest { r := applyReq(); d := false; r.DryRun = &d; return r }(), "confirm"},
		{"empty selection", jobRequest{Kind: "mirror", Selection: "x"}, "empty"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			s := sel
			if tc.name == "empty selection" {
				s = models.Selection{}
			}
			err := validateJobRequest(tc.req, s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestSplitShellWords(t *testing.T) {
	words, err := splitShellWords(`sed -i 's|a|b c|' "Dockerfile"`)
	require.NoError(t, err)
	assert.Equal(t, []string{"sed", "-i", "s|a|b c|", "Dockerfile"}, words)

	words, err = splitShellWords(`echo hello\ world`)
	require.NoError(t, err)
	assert.Equal(t, []string{"echo", "hello world"}, words)

	_, err = splitShellWords(`echo 'unterminated`)
	require.Error(t, err)
}

func TestJobExecLogsInvocationNotEnv(t *testing.T) {
	r := newRegistry()
	j := r.add("mirror")
	out, err := j.exec(context.Background(), "echo", []string{"hi"}, []string{"TRAILBOSS_PAT=secret-token", "PATH=/usr/bin"})
	require.NoError(t, err)
	assert.Contains(t, string(out), "hi")
	for _, line := range j.snapshot() {
		assert.NotContains(t, line, "secret-token", "env values must never reach the log")
	}
	assert.Contains(t, j.snapshot()[0], "$ echo hi")
}
