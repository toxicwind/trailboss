package main

import (
	"bytes"
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

func TestValidateDirectApply(t *testing.T) {
	ok := directApplyValidation{
		commitMessage: "bump image",
		sign:          "none",
		script:        []string{"true"},
		owners:        []string{"toxicwind"},
	}
	require.NoError(t, validateDirectApply(ok))

	tests := []struct {
		name    string
		mutate  func(*directApplyValidation)
		wantErr string
	}{
		{"missing commit message", func(d *directApplyValidation) { d.commitMessage = "" }, "--commit-message is required"},
		{"missing sign", func(d *directApplyValidation) { d.sign = "" }, "--sign is required"},
		{"invalid sign", func(d *directApplyValidation) { d.sign = "gpg" }, "--sign \"gpg\" is invalid"},
		{"github sign refused", func(d *directApplyValidation) { d.sign = "github" }, "PR-mode only"},
		{"missing script", func(d *directApplyValidation) { d.script = nil }, "after --"},
		{"missing owners", func(d *directApplyValidation) { d.owners = nil }, "--direct-owners is required"},
		// PR-mode fields are deliberately NOT required: direct mode targets
		// each repo's default branch, so there is no branch or PR title.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dv := ok
			tt.mutate(&dv)
			err := validateDirectApply(dv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// cmdDirectFake emulates git + sh for runDirectApply: clones materialise the
// target dir, the "script" is any sh invocation, diff --cached --stat is
// canned per dir.
type cmdDirectFake struct {
	mu        sync.Mutex
	calls     int
	diffStats map[string]string
}

func (f *cmdDirectFake) run(_ context.Context, name string, args, env []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	// The token must never ride along in argv.
	for _, a := range args {
		if strings.Contains(a, "sekrit") {
			return nil, errors.New("token leaked into argv")
		}
	}
	if name == "sh" {
		return []byte("script ok"), nil
	}
	rest := args
	for len(rest) > 1 && rest[0] == "-c" {
		rest = rest[2:]
	}
	var dir string
	if len(rest) > 1 && rest[0] == "-C" {
		dir, rest = rest[1], rest[2:]
	}
	switch rest[0] {
	case "clone":
		if err := os.MkdirAll(rest[len(rest)-1], 0o755); err != nil {
			return nil, err
		}
		return []byte("cloned"), nil
	case "diff":
		return []byte(f.diffStats[dir]), nil
	case "fetch", "checkout", "reset", "add":
		return nil, nil
	case "commit", "push":
		return []byte("ok"), nil
	}
	return nil, errors.New("unexpected git subcommand " + rest[0])
}

func cmdDirectSelection() models.Selection {
	return models.Selection{
		Owner: "toxicwind",
		Repos: []models.Repo{
			{Owner: "toxicwind", Name: "trailboss", CloneURL: "https://github.com/toxicwind/trailboss.git", DefaultBranch: "main"},
		},
	}
}

func TestRunDirectApplyDryRun(t *testing.T) {
	f := &cmdDirectFake{diffStats: map[string]string{}}
	workDir := t.TempDir()
	f.diffStats[filepath.Join(workDir, "toxicwind", "trailboss")] = " Dockerfile | 2 +-\n"
	var errOut bytes.Buffer
	spec := models.ApplySpec{
		CommitMessage: "bump image",
		Script:        []string{"sed", "-i", "s|a|b|", "Dockerfile"},
		DryRun:        true,
		Sign:          models.SignNone,
	}
	err := runDirectApply(context.Background(), f.run, cmdDirectSelection(), spec,
		"sekrit", []string{"toxicwind"}, workDir, &errOut)
	require.NoError(t, err)
	out := errOut.String()
	assert.Contains(t, out, "Applying to 1 repo(s)")
	assert.Contains(t, out, "direct mode")
	assert.Contains(t, out, "dry-run")
	assert.Contains(t, out, "owners allow-list: toxicwind")
	assert.Contains(t, out, "toxicwind/trailboss   would change")
	assert.Contains(t, out, "direct apply complete")
	// A dry-run must not commit or push.
	assert.NotContains(t, out, "pushed")
}

func TestRunDirectApplyOwnerRefusal(t *testing.T) {
	f := &cmdDirectFake{diffStats: map[string]string{}}
	var errOut bytes.Buffer
	spec := models.ApplySpec{
		CommitMessage: "bump",
		Script:        []string{"true"},
		DryRun:        true,
		Sign:          models.SignNone,
	}
	sel := cmdDirectSelection()
	sel.Repos = append(sel.Repos, models.Repo{Owner: "someone-else", Name: "x", DefaultBranch: "main"})
	err := runDirectApply(context.Background(), f.run, sel, spec,
		"sekrit", []string{"toxicwind"}, t.TempDir(), &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow-list")
	assert.Zero(t, f.calls, "a refused run executes nothing")
}

func TestDirectRunnerStreamsAndCaptures(t *testing.T) {
	var errOut bytes.Buffer
	run := directRunner(&errOut)
	out, err := run(context.Background(), "echo", []string{"hello"}, []string{"PATH=" + os.Getenv("PATH")})
	require.NoError(t, err)
	assert.Contains(t, string(out), "hello", "captured output is returned")
	assert.Contains(t, errOut.String(), "hello", "output streams live to errOut")
}
