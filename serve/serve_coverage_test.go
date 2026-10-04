package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/toxicwind/trailboss/models"
	"github.com/toxicwind/trailboss/selection"
)

// writeSel writes a minimal valid selection lockfile for handler tests.
func writeSel(t *testing.T, path string, repos ...models.Repo) {
	t.Helper()
	sel := models.Selection{
		Version:    models.SelectionVersion,
		Owner:      "acme",
		OwnerType:  "Organization",
		ResolvedAt: time.Now().UTC(),
		Tool:       "trailboss-test",
		Repos:      repos,
	}
	require.NoError(t, selection.Write(path, sel, selection.WriteOptions{Overwrite: true}))
}

func testRepo(owner, name string) models.Repo {
	return models.Repo{Owner: owner, Name: name, DefaultBranch: "main"}
}

func TestWriteJSONAndError(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusCreated, map[string]string{"hello": "world"})
	resp := w.Result()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "world", body["hello"])

	w = httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "boom")
	resp = w.Result()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body = nil
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "boom", body["error"])
}

func TestValidSelectionName(t *testing.T) {
	for _, ok := range []string{"my-herd", "abc123", "a.b", "x-y-z_1"} {
		assert.True(t, validSelectionName(ok), ok)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a..b", "../x", "a/b.selection"} {
		assert.False(t, validSelectionName(bad), bad)
	}
}

func TestTruncSuffix(t *testing.T) {
	assert.Equal(t, "", truncSuffix(false))
	assert.Contains(t, truncSuffix(true), "truncated")
}

func TestDryWord(t *testing.T) {
	assert.Equal(t, "dry-run", dryWord(true))
	assert.Equal(t, "live", dryWord(false))
}

func TestFirstLineOf(t *testing.T) {
	assert.Equal(t, "one", firstLineOf("one\ntwo\nthree"))
	assert.Equal(t, "solo", firstLineOf("solo"))
	assert.Equal(t, "", firstLineOf(""))
}

func TestTrimLine(t *testing.T) {
	assert.Equal(t, "abc", trimLine("abc\r"))
	assert.Equal(t, "abc", trimLine("abc"))
	long := strings.Repeat("x", 500)
	trimmed := trimLine(long)
	assert.Len(t, trimmed, 303) // 300 + "…"
	assert.True(t, strings.HasSuffix(trimmed, "…"))
}

func TestIsBinary(t *testing.T) {
	assert.False(t, isBinary([]byte("hello world\n")))
	assert.True(t, isBinary([]byte("hel\x00lo")))
	assert.False(t, isBinary(nil))
	// NUL past the 8000-byte sniff window is not detected (documented limit).
	big := append([]byte(strings.Repeat("a", 9000)), 0)
	assert.False(t, isBinary(big))
}

func TestSplitShellWordsExtended(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"echo hello", []string{"echo", "hello"}, false},
		{"  echo   hello  ", []string{"echo", "hello"}, false},
		{`echo 'hello world'`, []string{"echo", "hello world"}, false},
		{`echo "hello world"`, []string{"echo", "hello world"}, false},
		{`echo "a\"b"`, []string{"echo", `a"b`}, false},
		{`echo a\ b`, []string{"echo", "a b"}, false},
		{`echo 'it'\''s'`, []string{"echo", "it's"}, false},
		{`'unterminated`, nil, true},
		{`"unterminated`, nil, true},
		{`trailing\`, nil, true},
		{`mix 'single' "double" plain`, []string{"mix", "single", "double", "plain"}, false},
	}
	for _, c := range cases {
		got, err := splitShellWords(c.in)
		if c.wantErr {
			assert.Error(t, err, c.in)
			continue
		}
		require.NoError(t, err, c.in)
		assert.Equal(t, c.want, got, c.in)
	}
}

func TestLogLineTruncation(t *testing.T) {
	j := &Job{}
	j.logLine(strings.Repeat("x", 5000))
	lines := j.snapshot()
	require.Len(t, lines, 1)
	assert.True(t, strings.HasSuffix(lines[0], "… (truncated)"))
	assert.LessOrEqual(t, len(lines[0]), 4096+len("… (truncated)"))
}

func TestLogLineCap(t *testing.T) {
	j := &Job{}
	for i := 0; i < 20005; i++ {
		j.logLine("line")
	}
	lines := j.snapshot()
	assert.Len(t, lines, 20001) // 20000 + the cap marker
	assert.Equal(t, "… (log truncated at 20000 lines)", lines[len(lines)-1])
}

func TestUnsubscribe(t *testing.T) {
	j := &Job{}
	sub1 := j.subscribe()
	sub2 := j.subscribe()
	j.unsubscribe(sub1)
	j.logLine("hello")
	select {
	case <-sub1:
		t.Fatal("unsubscribed channel should not receive")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case line := <-sub2:
		assert.Equal(t, "hello", line)
	case <-time.After(2 * time.Second):
		t.Fatal("subscribed channel should receive")
	}
	// Unsubscribing an unknown channel is a no-op.
	j.unsubscribe(sub1)
}

func TestSetGetSummaryResult(t *testing.T) {
	j := &Job{}
	assert.Equal(t, "", j.getSummary())
	j.setSummary("did things")
	assert.Equal(t, "did things", j.getSummary())
	j.setResult(map[string]int{"n": 3})
	dto := j.snapshotDTO(false)
	assert.Equal(t, map[string]int{"n": 3}, dto.Result.(map[string]int))
}

func TestSnapshotDTO(t *testing.T) {
	j := &Job{ID: "x", Kind: "scan", Status: JobRunning}
	j.logLine("a")
	j.logLine("b")
	withLog := j.snapshotDTO(true)
	require.Len(t, withLog.Log, 2)
	withoutLog := j.snapshotDTO(false)
	assert.Nil(t, withoutLog.Log)
	assert.Equal(t, "x", withLog.ID)
	// The DTO is a copy: mutating the job afterwards doesn't affect it.
	j.logLine("c")
	assert.Len(t, withLog.Log, 2)
}

func TestLineWriter(t *testing.T) {
	j := &Job{}
	w := &lineWriter{job: j}
	n, err := w.Write([]byte("one\ntwo\npar"))
	require.NoError(t, err)
	assert.Equal(t, 11, n)
	assert.Equal(t, []string{"one", "two"}, j.snapshot())
	w.flush()
	assert.Equal(t, []string{"one", "two", "par"}, j.snapshot())
	w.flush() // second flush is a no-op
	assert.Len(t, j.snapshot(), 3)
}

func TestRunners(t *testing.T) {
	j := &Job{}
	ar := j.applyRunner()
	require.NotNil(t, ar)
	mr := j.mirrorRunner()
	require.NotNil(t, mr)
	// mirrorRunner adapts exec's ([]byte, error) to error-only.
	err := mr(context.Background(), "true", nil, nil)
	assert.NoError(t, err)
	err = mr(context.Background(), "false", nil, nil)
	assert.Error(t, err)
}

func TestValidateJobRequestExtended(t *testing.T) {
	sel := models.Selection{Repos: []models.Repo{testRepo("acme", "r1")}}
	empty := models.Selection{}

	boolPtr := func(b bool) *bool { return &b }

	cases := []struct {
		name    string
		req     jobRequest
		sel     models.Selection
		wantErr string
	}{
		{"empty selection", jobRequest{Kind: "scan", Pattern: "x"}, empty, "empty"},
		{"scan ok", jobRequest{Kind: "scan", Pattern: "x"}, sel, ""},
		{"scan no pattern", jobRequest{Kind: "scan"}, sel, "pattern"},
		{"apply no script", jobRequest{Kind: "apply", CommitMessage: "m", Branch: "b", Sign: models.SignNone}, sel, "script"},
		{"apply no message", jobRequest{Kind: "apply", Script: []string{"true"}, Branch: "b", Sign: models.SignNone}, sel, "commitMessage"},
		{"apply bad sign", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Branch: "b", Sign: "bogus"}, sel, "invalid sign"},
		{"apply pr no branch", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Sign: models.SignNone}, sel, "branch"},
		{"apply bad mode", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Mode: "bogus", Sign: models.SignNone}, sel, "invalid mode"},
		{"apply direct no owners", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Mode: "direct", Sign: models.SignNone}, sel, "owners"},
		{"apply direct github sign", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Mode: "direct", Owners: []string{"acme"}, Sign: models.SignGitHub}, sel, "PR-mode only"},
		{"apply live no confirm", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Branch: "b", DryRun: boolPtr(false), Sign: models.SignNone}, sel, "confirm"},
		{"apply pr ok", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Branch: "b", Sign: models.SignNone}, sel, ""},
		{"apply direct ok", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Mode: "direct", Owners: []string{"acme"}, Sign: models.SignNone}, sel, ""},
		{"apply live confirmed", jobRequest{Kind: "apply", Script: []string{"true"}, CommitMessage: "m", Branch: "b", DryRun: boolPtr(false), Confirm: true, Sign: models.SignNone}, sel, ""},
	}
	for _, c := range cases {
		err := validateJobRequest(c.req, c.sel)
		if c.wantErr == "" {
			assert.NoError(t, err, c.name)
		} else {
			require.Error(t, err, c.name)
			assert.Contains(t, err.Error(), c.wantErr, c.name)
		}
	}
}

func TestRequireTool(t *testing.T) {
	assert.NoError(t, requireTool("sh"))
	err := requireTool("definitely-not-a-real-binary-xyz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found on PATH")
}

func TestWorkspaceRoot(t *testing.T) {
	ws, err := workspaceRoot()
	require.NoError(t, err)
	home, _ := os.UserHomeDir()
	assert.Equal(t, filepath.Join(home, "trailboss"), ws)
}

func TestSummariseSelection(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "demo.selection")
	writeSel(t, p, testRepo("acme", "r1"), testRepo("acme", "r2"))

	sum := summariseSelection("demo", p)
	assert.Equal(t, "demo", sum.Name)
	assert.Equal(t, "acme", sum.Owner)
	require.NotNil(t, sum.RepoCount)
	assert.Equal(t, 2, *sum.RepoCount)
	assert.NotEmpty(t, sum.Digest)
	assert.Empty(t, sum.Error)

	bad := summariseSelection("bad", filepath.Join(dir, "missing.selection"))
	assert.NotEmpty(t, bad.Error)
	assert.Nil(t, bad.RepoCount)
}

func TestHandleSelections(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeSel(t, "demo.selection", testRepo("acme", "r1"))
	require.NoError(t, os.WriteFile("broken.selection", []byte("{nope"), 0o644))

	s := &Server{reg: newRegistry()}
	w := httptest.NewRecorder()
	s.handleSelections(w, httptest.NewRequest(http.MethodGet, "/api/selections", nil))
	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Selections []selectionSummary `json:"selections"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	names := map[string]selectionSummary{}
	for _, sm := range body.Selections {
		names[sm.Name] = sm
	}
	require.Contains(t, names, "demo")
	assert.Equal(t, 1, *names["demo"].RepoCount)
	require.Contains(t, names, "broken")
	assert.NotEmpty(t, names["broken"].Error)
}

func TestHandleSelectionDetail(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeSel(t, "demo.selection", testRepo("acme", "r1"))

	s := &Server{reg: newRegistry()}

	// Missing name.
	w := httptest.NewRecorder()
	s.handleSelectionDetail(w, httptest.NewRequest(http.MethodGet, "/api/selection", nil))
	assert.Equal(t, http.StatusBadRequest, w.Result().StatusCode)

	// Unknown bare name.
	w = httptest.NewRecorder()
	s.handleSelectionDetail(w, httptest.NewRequest(http.MethodGet, "/api/selection?name=nope", nil))
	assert.Equal(t, http.StatusBadRequest, w.Result().StatusCode)

	// Known selection.
	w = httptest.NewRecorder()
	s.handleSelectionDetail(w, httptest.NewRequest(http.MethodGet, "/api/selection?name=demo", nil))
	resp := w.Result()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Digest string `json:"digest"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotEmpty(t, body.Digest)
}

func TestHandleSelect(t *testing.T) {
	// No token: 503 before any parsing.
	s := &Server{reg: newRegistry()}
	w := httptest.NewRecorder()
	s.handleSelect(w, httptest.NewRequest(http.MethodPost, "/api/select", strings.NewReader(`{}`)))
	assert.Equal(t, http.StatusServiceUnavailable, w.Result().StatusCode)

	s = &Server{token: "bogus", reg: newRegistry()}
	// Invalid JSON.
	w = httptest.NewRecorder()
	s.handleSelect(w, httptest.NewRequest(http.MethodPost, "/api/select", strings.NewReader(`{`)))
	assert.Equal(t, http.StatusBadRequest, w.Result().StatusCode)
	// Empty org.
	w = httptest.NewRecorder()
	s.handleSelect(w, httptest.NewRequest(http.MethodPost, "/api/select", strings.NewReader(`{"org":""}`)))
	assert.Equal(t, http.StatusBadRequest, w.Result().StatusCode)
	// Invalid org name.
	w = httptest.NewRecorder()
	s.handleSelect(w, httptest.NewRequest(http.MethodPost, "/api/select", strings.NewReader(`{"org":"a/b"}`)))
	assert.Equal(t, http.StatusBadRequest, w.Result().StatusCode)
}

func postJob(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleJobCreate(w, httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(body)))
	return w
}

func TestHandleJobCreate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeSel(t, "demo.selection", testRepo("acme", "r1"))
	writeSel(t, "empty.selection")

	newServer := func(token string) *Server { return &Server{token: token, reg: newRegistry()} }

	// Invalid JSON.
	assert.Equal(t, http.StatusBadRequest, postJob(t, newServer(""), `{`).Result().StatusCode)
	// Bad kind.
	assert.Equal(t, http.StatusBadRequest, postJob(t, newServer(""), `{"kind":"nope","selection":"demo"}`).Result().StatusCode)
	// Unknown selection.
	assert.Equal(t, http.StatusBadRequest, postJob(t, newServer(""), `{"kind":"scan","selection":"nope"}`).Result().StatusCode)
	// Empty selection.
	assert.Equal(t, http.StatusBadRequest, postJob(t, newServer(""), `{"kind":"scan","pattern":"x","selection":"empty"}`).Result().StatusCode)
	// Scan without pattern.
	assert.Equal(t, http.StatusBadRequest, postJob(t, newServer(""), `{"kind":"scan","selection":"demo"}`).Result().StatusCode)
	// Mirror without token.
	assert.Equal(t, http.StatusServiceUnavailable, postJob(t, newServer(""), `{"kind":"mirror","selection":"demo"}`).Result().StatusCode)
	// Apply without token.
	assert.Equal(t, http.StatusServiceUnavailable, postJob(t, newServer(""), `{"kind":"apply","selection":"demo","script":["true"],"commitMessage":"m","branch":"b","sign":"none"}`).Result().StatusCode)

	// Scan succeeds without a token and returns a job id. The background job
	// scans a missing workspace and finishes on its own.
	w := postJob(t, newServer(""), `{"kind":"scan","pattern":"x","selection":"demo"}`)
	resp := w.Result()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.NotEmpty(t, created.ID)
}

func TestHandleJobsListAndDetail(t *testing.T) {
	s := &Server{reg: newRegistry()}

	w := httptest.NewRecorder()
	s.handleJobsList(w, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	require.Equal(t, http.StatusOK, w.Result().StatusCode)

	j := s.reg.add("scan")
	j.logLine("hello")

	w = httptest.NewRecorder()
	s.handleJobsList(w, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	var list struct {
		Jobs []jobDTO `json:"jobs"`
	}
	require.NoError(t, json.NewDecoder(w.Result().Body).Decode(&list))
	require.Len(t, list.Jobs, 1)
	assert.Equal(t, j.ID, list.Jobs[0].ID)
	assert.Nil(t, list.Jobs[0].Log, "list view omits the log")

	// Detail includes the log.
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/jobs/"+j.ID, nil)
	r.SetPathValue("id", j.ID)
	s.handleJobDetail(w, r)
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	var dto jobDTO
	require.NoError(t, json.NewDecoder(w.Result().Body).Decode(&dto))
	assert.Equal(t, []string{"hello"}, dto.Log)

	// Unknown id.
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/jobs/nope", nil)
	r.SetPathValue("id", "nope")
	s.handleJobDetail(w, r)
	assert.Equal(t, http.StatusNotFound, w.Result().StatusCode)
}

func TestHandleJobEventsTerminal(t *testing.T) {
	s := &Server{reg: newRegistry()}
	j := s.reg.add("scan")
	j.logLine("one")
	j.logLine("two")
	j.setStatus(JobDone)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/jobs/"+j.ID+"/events", nil)
	r.SetPathValue("id", j.ID)
	s.handleJobEvents(w, r)

	body := w.Body.String()
	assert.Contains(t, body, `"line":"one"`)
	assert.Contains(t, body, `"line":"two"`)
	assert.Contains(t, body, `"status":"done"`)
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
}

func TestHandleJobEventsLiveCancel(t *testing.T) {
	s := &Server{reg: newRegistry()}
	j := s.reg.add("scan")
	j.setStatus(JobRunning)

	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/jobs/"+j.ID+"/events", nil).WithContext(ctx)
	r.SetPathValue("id", j.ID)
	done := make(chan struct{})
	go func() { s.handleJobEvents(w, r); close(done) }()
	// Let the handler stream the (empty) replay, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
	assert.Contains(t, w.Body.String(), `"status":"running"`)
}

func TestHandleJobEventsMissing(t *testing.T) {
	s := &Server{reg: newRegistry()}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/jobs/nope/events", nil)
	r.SetPathValue("id", "nope")
	s.handleJobEvents(w, r)
	assert.Equal(t, http.StatusNotFound, w.Result().StatusCode)
}

func TestRunJobUnknownKind(t *testing.T) {
	s := &Server{reg: newRegistry()}
	j := s.reg.add("bogus")
	s.runJob(context.Background(), j, jobRequest{Kind: "bogus"}, models.Selection{}, "")
	assert.Equal(t, JobError, j.Status)
	assert.Contains(t, j.getSummary(), "unknown kind")
	assert.NotZero(t, j.EndedAt)
}

func TestRunJobScanInvalidPattern(t *testing.T) {
	s := &Server{reg: newRegistry()}
	j := s.reg.add("scan")
	s.runJob(context.Background(), j, jobRequest{Kind: "scan", Pattern: "["}, models.Selection{
		Repos: []models.Repo{testRepo("acme", "r1")},
	}, "")
	assert.Equal(t, JobError, j.Status)
	assert.Contains(t, j.getSummary(), "invalid pattern")
}

func TestJobScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Fake mirror workspace: ~/trailboss/acme/repo1 with searchable files.
	repoDir := filepath.Join(home, "trailboss", "acme", "repo1")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n// TODO: fix me\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "skip.bin"), []byte{0x1, 0x2, 0x0, 0x3}, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".git", "config"), []byte("TODO in git dir must be skipped\n"), 0o644))

	s := &Server{reg: newRegistry()}
	j := s.reg.add("scan")
	sel := models.Selection{Repos: []models.Repo{
		testRepo("acme", "repo1"),
		testRepo("acme", "missing"),
	}}
	require.NoError(t, s.jobScan(context.Background(), j, sel, "TODO"))

	res, ok := j.Result.(scanResult)
	require.True(t, ok, "job result should be a scanResult, got %T", j.Result)
	assert.Equal(t, 2, res.ReposInSelection)
	assert.Equal(t, 1, res.ReposScanned)
	assert.Equal(t, 1, res.ReposNotScanned)
	assert.Equal(t, 1, res.TotalMatches, "only main.go matches; .git and the binary are skipped")
	assert.False(t, res.Truncated)
	require.Len(t, res.Matches, 1)
	assert.Equal(t, "main.go", res.Matches[0].File)
	assert.Equal(t, 2, res.Matches[0].Line)
	assert.Contains(t, j.getSummary(), "1/2 repos scanned")
}

func TestJobScanBadPattern(t *testing.T) {
	s := &Server{reg: newRegistry()}
	j := s.reg.add("scan")
	err := s.jobScan(context.Background(), j, models.Selection{}, "[")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pattern")
}

func TestScanRepoCaps(t *testing.T) {
	dir := t.TempDir()
	// A file bigger than 1MiB is skipped and flags truncation.
	big := filepath.Join(dir, "big.txt")
	f, err := os.Create(big)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(scanMaxFileBytes+1))
	require.NoError(t, f.Close())

	s := &Server{}
	var ro scanRepoOutcome
	var total int
	res := &scanResult{}
	matches := s.scanRepo(context.Background(), dir, testRepo("acme", "r"), regexp.MustCompile("x"), &ro, &total, res)
	assert.Empty(t, matches)
	assert.True(t, ro.Truncated)
	assert.True(t, res.Truncated)
}

func regexpMust(p string) *regexp.Regexp {
	re, err := regexp.Compile(p)
	if err != nil {
		panic(err)
	}
	return re
}

func TestJobMirrorNoGhorg(t *testing.T) {
	if _, err := exec.LookPath("ghorg"); err == nil {
		t.Skip("ghorg is installed; cannot test the missing-tool path")
	}
	s := &Server{token: "t", reg: newRegistry()}
	j := s.reg.add("mirror")
	err := s.jobMirror(context.Background(), j, models.Selection{Repos: []models.Repo{testRepo("acme", "r1")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghorg")
}

func TestJobApplyBadScriptLine(t *testing.T) {
	s := &Server{token: "t", reg: newRegistry()}
	j := s.reg.add("apply")
	err := s.jobApply(context.Background(), j, models.Selection{}, jobRequest{ScriptLine: `"unterminated`})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing scriptLine")
}

func TestJobApplyPrModeNoTool(t *testing.T) {
	if _, err := exec.LookPath("multi-gitter"); err == nil {
		t.Skip("multi-gitter is installed; cannot test the missing-tool path")
	}
	s := &Server{token: "t", reg: newRegistry()}
	j := s.reg.add("apply")
	err := s.jobApply(context.Background(), j, models.Selection{Repos: []models.Repo{testRepo("acme", "r1")}},
		jobRequest{Mode: "pr", Script: []string{"true"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-gitter")
}

func TestRunStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Addr: "127.0.0.1:0", WebFS: fstest.MapFS{}})
	}()
	// Give the server a moment to bind, then cancel.
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestKnownSelections(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeSel(t, "alpha.selection", testRepo("acme", "r1"))
	known := knownSelections()
	abs, err := filepath.Abs("alpha.selection")
	require.NoError(t, err)
	assert.Equal(t, abs, known["alpha"])
}

func TestRunSelectOpBadToken(t *testing.T) {
	// A bogus token fails at Verify — exercises the error path without any
	// real credentials. Bounded by a timeout so CI without network can't hang.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := runSelectOp(ctx, "bogus-token-xyz", selectRequest{Org: "acme"})
	require.Error(t, err)
}

func TestJobApplyDirectModeEmpty(t *testing.T) {
	// Direct mode with an empty selection: git is present, apply.Direct short-
	// circuits on zero repos, and the summary counts zeros. Covers the
	// direct-mode success path without touching any real repo.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	s := &Server{token: "t", reg: newRegistry()}
	j := s.reg.add("apply")
	// Empty selection: apply.Direct rejects it before any git work. Exercises
	// the direct-mode dispatch and error propagation without touching repos.
	err := s.jobApply(context.Background(), j, models.Selection{},
		jobRequest{Mode: "direct", Script: []string{"true"}, Owners: []string{"acme"}, Sign: models.SignNone})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selection is empty")
}
