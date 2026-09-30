package serve

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/toxicwind/trailboss/apply"
	"github.com/toxicwind/trailboss/client"
	"github.com/toxicwind/trailboss/discovery"
	"github.com/toxicwind/trailboss/mirror"
	"github.com/toxicwind/trailboss/models"
	"github.com/toxicwind/trailboss/selection"
)

// workspaceRoot is the default mirror workspace root: ~/trailboss, matching
// the mirror command's default.
func workspaceRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for workspace root: %w", err)
	}
	return filepath.Join(home, "trailboss"), nil
}

// runJob executes a registered job to a terminal state, recording the summary
// and any machine-readable result. Panics are caught so one bad job can never
// take the registry (or the server) down with it.
func (s *Server) runJob(ctx context.Context, j *Job, req jobRequest, sel models.Selection, selPath string) {
	j.setStatus(JobRunning)
	defer func() {
		if r := recover(); r != nil {
			j.logf("job panicked: %v", r)
			j.setSummary(fmt.Sprintf("panicked: %v", r))
			j.setStatus(JobError)
		}
	}()
	var err error
	switch req.Kind {
	case "mirror":
		err = s.jobMirror(ctx, j, sel)
	case "scan":
		err = s.jobScan(ctx, j, sel, req.Pattern)
	case "apply":
		err = s.jobApply(ctx, j, sel, req)
	default:
		err = fmt.Errorf("unknown kind %q", req.Kind)
	}
	if err != nil {
		j.logf("job failed: %s", err)
		if j.getSummary() == "" {
			j.setSummary("failed: " + firstLineOf(err.Error()))
		}
		j.setStatus(JobError)
		return
	}
	// Log completion before the terminal status: setStatus delivers the
	// terminal frame and closes subscriber channels, so anything logged
	// after it would be invisible to live SSE subscribers.
	j.logf("job done: %s", j.getSummary())
	j.setStatus(JobDone)
}

// requireTool fails when the named binary is not on PATH.
func requireTool(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is required but not found on PATH", name)
	}
	return nil
}

// runSelectOp resolves org repos via the read-only GitHub client and freezes a
// selection lockfile — the same resolve→filter→write flow as `trailboss
// select`, minus the CLI's branch-presence and explicit-repo knobs.
func runSelectOp(ctx context.Context, token string, req selectRequest) (models.Selection, string, error) {
	c, err := client.New(token)
	if err != nil {
		return models.Selection{}, "", fmt.Errorf("create GitHub client: %w", err)
	}
	login, err := c.Verify(ctx)
	if err != nil {
		return models.Selection{}, "", fmt.Errorf("verifying token: %w", err)
	}
	repos, ownerType, err := c.ListRepos(ctx, req.Org)
	if err != nil {
		return models.Selection{}, "", fmt.Errorf("listing repos for %s: %w", req.Org, err)
	}
	selected := discovery.Select(repos, discovery.Filter{AllRepos: req.AllRepos, Topics: req.Topics})
	if len(selected) == 0 {
		return models.Selection{}, "", fmt.Errorf("no repos matched for %s (topics %v) as %s — check the org name, the token's identity, and the topics", req.Org, req.Topics, login)
	}
	sel := models.Selection{
		Version:     models.SelectionVersion,
		Owner:       req.Org,
		OwnerType:   ownerType,
		Filter:      models.SelectionFilter{AllRepos: req.AllRepos, Topics: req.Topics},
		ResolvedAt:  time.Now().UTC(),
		Tool:        "trailboss-serve",
		Repos:       selected,
	}
	var path string
	if req.Name != "" {
		if !validSelectionName(req.Name) {
			return models.Selection{}, "", fmt.Errorf("invalid selection name %q", req.Name)
		}
		path, err = selection.PathForName(req.Name)
		if err != nil {
			return models.Selection{}, "", err
		}
	} else {
		path = req.Org + ".selection"
	}
	if err := selection.Write(path, sel, selection.WriteOptions{Overwrite: true}); err != nil {
		return models.Selection{}, "", fmt.Errorf("writing selection: %w", err)
	}
	return sel, path, nil
}

// jobMirror clones the selection into the default workspace via ghorg, through
// mirror.Mirror with the job's log-capturing runner.
func (s *Server) jobMirror(ctx context.Context, j *Job, sel models.Selection) error {
	if err := requireTool("ghorg"); err != nil {
		return err
	}
	ws, err := workspaceRoot()
	if err != nil {
		return err
	}
	j.logf("mirroring %d repos into %s", len(sel.Repos), ws)
	if err := mirror.Mirror(ctx, j.mirrorRunner(), sel, s.token, mirror.Options{Workspace: ws}); err != nil {
		return err
	}
	j.setSummary(fmt.Sprintf("mirrored %d repos into %s", len(sel.Repos), ws))
	return nil
}

// scan caps: the UI scan is a quick look, not an audit — bounded per repo and
// overall, with truncation flagged honestly like the CLI scan.
const (
	scanMaxFileBytes  = 1 << 20 // files bigger than 1 MiB are skipped
	scanMaxRepoMatch  = 100     // per-repo match cap
	scanMaxTotalMatch = 5000    // overall match cap
)

type scanMatch struct {
	Repo string `json:"repo"`
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type scanRepoOutcome struct {
	Repo       string `json:"repo"`
	Scanned    bool   `json:"scanned"`
	SkipReason string `json:"skipReason,omitempty"`
	Matches    int    `json:"matches"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type scanResult struct {
	Pattern          string            `json:"pattern"`
	Workspace        string            `json:"workspace"`
	ReposInSelection int               `json:"reposInSelection"`
	ReposScanned     int               `json:"reposScanned"`
	ReposNotScanned  int               `json:"reposNotScanned"`
	TotalMatches     int               `json:"totalMatches"`
	Truncated        bool              `json:"truncated"`
	Repos            []scanRepoOutcome `json:"repos"`
	Matches          []scanMatch       `json:"matches"`
}

// jobScan searches the local mirror for the pattern — pure local reads, no
// GitHub, no token. Only repos in the selection are searched; a rogue clone
// on disk outside the selection is ignored, mirroring the CLI's guarantee.
func (s *Server) jobScan(ctx context.Context, j *Job, sel models.Selection, pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid pattern: %w", err)
	}
	ws, err := workspaceRoot()
	if err != nil {
		return err
	}
	res := scanResult{Pattern: pattern, Workspace: ws, ReposInSelection: len(sel.Repos)}
	total := 0
	for _, r := range sel.Repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ro := scanRepoOutcome{Repo: r.FullName()}
		dir := filepath.Join(ws, r.Owner, r.Name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			ro.SkipReason = "not mirrored under workspace"
			res.ReposNotScanned++
			res.Repos = append(res.Repos, ro)
			continue
		}
		matches := s.scanRepo(ctx, dir, r, re, &ro, &total, &res)
		ro.Scanned = true
		res.Matches = append(res.Matches, matches...)
		res.ReposScanned++
		res.TotalMatches += ro.Matches
		if ro.Truncated {
			res.Truncated = true
		}
		res.Repos = append(res.Repos, ro)
	}
	j.setResult(res)
	j.setSummary(fmt.Sprintf("%d/%d repos scanned, %d matches%s", res.ReposScanned, res.ReposInSelection, res.TotalMatches, truncSuffix(res.Truncated)))
	j.logf("scan: %d repos scanned, %d not on disk, %d matches", res.ReposScanned, res.ReposNotScanned, res.TotalMatches)
	return nil
}

func truncSuffix(t bool) string {
	if t {
		return " (truncated — caps hit)"
	}
	return ""
}

// scanRepo walks one repo checkout collecting matches, honouring the caps.
func (s *Server) scanRepo(ctx context.Context, dir string, r models.Repo, re *regexp.Regexp, ro *scanRepoOutcome, total *int, res *scanResult) []scanMatch {
	var matches []scanMatch
	stop := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if stop || *total >= scanMaxTotalMatch {
			stop = true
			ro.Truncated, res.Truncated = true, true
			return filepath.SkipAll
		}
		if err != nil {
			return nil // unreadable entry: skip, don't fail the repo
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // symlinks skipped by design, like the CLI scan
		}
		info, err := d.Info()
		if err != nil || info.Size() > scanMaxFileBytes {
			if err == nil {
				ro.Truncated, res.Truncated = true, true
			}
			return nil
		}
		if ro.Matches >= scanMaxRepoMatch {
			ro.Truncated, res.Truncated = true, true
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path) //nolint:gosec // G304: path comes from walking the repo dir just stat'ed; the read is the scan itself.
		if err != nil {
			return nil
		}
		if isBinary(data) {
			return nil // binary files skipped by design
		}
		for i, line := range strings.Split(string(data), "\n") {
			if loc := re.FindStringIndex(line); loc != nil {
				ro.Matches++
				*total++
				matches = append(matches, scanMatch{Repo: r.FullName(), File: rel, Line: i + 1, Text: trimLine(line)})
				if ro.Matches >= scanMaxRepoMatch || *total >= scanMaxTotalMatch {
					ro.Truncated, res.Truncated = true, true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return matches
}

// isBinary sniffs for a NUL byte in the head of the file.
func isBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

func trimLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// jobApply runs the change across the selection: PR mode via apply.Apply
// (multi-gitter), direct mode via apply.Direct (git itself), both with the
// job's log-capturing runner. Live runs are throttled in batches to stay under
// GitHub's secondary rate limits.
func (s *Server) jobApply(ctx context.Context, j *Job, sel models.Selection, req jobRequest) error {
	script := req.Script
	if len(script) == 0 {
		var err error
		script, err = splitShellWords(req.ScriptLine)
		if err != nil {
			return fmt.Errorf("parsing scriptLine: %w", err)
		}
	}
	dry := req.DryRun == nil || *req.DryRun
	spec := models.ApplySpec{
		Branch:        req.Branch,
		CommitMessage: req.CommitMessage,
		Script:        script,
		Sign:          req.Sign,
		DryRun:        dry,
		Confirm:       req.Confirm,
	}
	if !dry {
		// Unattended server-side writes stay polite: batches of 20 with a
		// minute between them (GitHub's ~80 content-writes/min secondary
		// limit, with headroom).
		spec.BatchSize = 20
		spec.BatchPause = time.Minute
	}
	mode := req.Mode
	if mode == "" {
		mode = "pr"
	}
	if mode == "pr" {
		if err := requireTool("multi-gitter"); err != nil {
			return err
		}
		j.logf("apply (pr mode): %d repos, dry-run=%v", len(sel.Repos), dry)
		res, err := apply.Apply(ctx, j.applyRunner(), sel, spec, s.token)
		if err != nil {
			return err
		}
		// The raw multi-gitter output is already in the job log via the
		// runner; store a small structured result for the UI instead of
		// duplicating it.
		j.setResult(map[string]any{"mode": "pr", "dryRun": dry, "repos": len(sel.Repos)})
		_ = res
		j.setSummary(fmt.Sprintf("pr-mode apply %s over %d repos", dryWord(dry), len(sel.Repos)))
		return nil
	}
	if err := requireTool("git"); err != nil {
		return err
	}
	j.logf("apply (direct mode): %d repos, dry-run=%v, owners=%s", len(sel.Repos), dry, strings.Join(req.Owners, ","))
	res, err := apply.Direct(ctx, j.applyRunner(), sel, spec, s.token, apply.DirectOpts{
		AllowedOwners: req.Owners,
		Concurrency:   4,
	})
	j.setResult(res)
	if err != nil {
		return err
	}
	var pushed, changed, failed int
	for _, o := range res.Outcomes {
		switch {
		case o.Err != "":
			failed++
		case o.Pushed:
			pushed++
		case o.DiffStat != "":
			changed++
		}
	}
	if dry {
		j.setSummary(fmt.Sprintf("direct dry-run: %d would change, %d no-change, %d failed", changed, len(res.Outcomes)-changed-failed, failed))
	} else {
		j.setSummary(fmt.Sprintf("direct apply: %d pushed, %d no-change, %d failed", pushed, len(res.Outcomes)-pushed-failed, failed))
	}
	return nil
}

func dryWord(dry bool) string {
	if dry {
		return "dry-run"
	}
	return "live"
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// splitShellWords splits a command line into words with POSIX-ish quoting:
// single quotes (no escapes inside), double quotes (backslash escapes),
// backslash escapes outside quotes. It is the UI's convenience parser for the
// one-line script box; the API's script array bypasses it entirely.
func splitShellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inSingle, inDouble, escaped := false, false, false
	flush := func() {
		words = append(words, cur.String())
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case unicode.IsSpace(r) && !inSingle && !inDouble:
			if cur.Len() > 0 {
				flush()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if escaped || inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote or escape in %q", s)
	}
	if cur.Len() > 0 {
		flush()
	}
	return words, nil
}
