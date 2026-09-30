package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/toxicwind/trailboss/models"
	"github.com/toxicwind/trailboss/selection"
)

// Options configures the web server.
type Options struct {
	// Addr is the listen address, e.g. "127.0.0.1:25250".
	Addr string
	// WebFS serves the embedded UI with index.html at its root.
	WebFS fs.FS
}

// Server holds the web server's dependencies.
type Server struct {
	token string // TRAILBOSS_PAT from the server environment; never exposed via API
	reg   *registry
	webFS fs.FS
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	s := &Server{token: os.Getenv(models.TokenEnvVar), reg: newRegistry(), webFS: opts.WebFS}
	mux := http.NewServeMux()
	mux.Handle("GET /api/selections", http.HandlerFunc(s.handleSelections))
	mux.Handle("POST /api/select", http.HandlerFunc(s.handleSelect))
	mux.Handle("GET /api/selection", http.HandlerFunc(s.handleSelectionDetail))
	mux.Handle("POST /api/jobs", http.HandlerFunc(s.handleJobCreate))
	mux.Handle("GET /api/jobs", http.HandlerFunc(s.handleJobsList))
	mux.Handle("GET /api/jobs/{id}", http.HandlerFunc(s.handleJobDetail))
	mux.Handle("GET /api/jobs/{id}/events", http.HandlerFunc(s.handleJobEvents))
	// The embedded UI owns every other path (index.html serves /).
	mux.Handle("/", http.FileServer(http.FS(s.webFS)))
	srv := &http.Server{Addr: opts.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	fmt.Printf("trailboss serve: listening on http://%s\n", opts.Addr)
	if s.token == "" {
		fmt.Printf("trailboss serve: warning: %s is not set — the dashboard works, but select/mirror/apply jobs will fail until it is\n", models.TokenEnvVar)
	}
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// writeJSON encodes v as compact JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// selectionSummary is one lockfile found by the selections scan.
type selectionSummary struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Owner      string `json:"owner,omitempty"`
	RepoCount  *int   `json:"repoCount"`
	ResolvedAt string `json:"resolvedAt,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Error      string `json:"error,omitempty"`
}

// knownSelections scans the three selection sources and returns a
// name→lockfile-path map: the named registry (~/.config/trailboss/selections),
// *.selection files in the server's working directory, and the mirror
// workspace root (~/trailboss, under the "workspace/" prefix). On a bare-name
// collision the registry wins, then the working directory; workspace entries
// never collide. Paths are absolute. Both the list endpoint and the
// reference resolver read this one map, so every advertised name resolves.
func knownSelections() map[string]string {
	out := map[string]string{}
	put := func(name, path string) {
		if _, exists := out[name]; exists {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		out[name] = abs
	}
	if names, err := selection.Names(); err == nil {
		for _, n := range names {
			if p, err := selection.PathForName(n); err == nil {
				put(n, p)
			}
		}
	}
	if matches, _ := filepath.Glob("*.selection"); matches != nil {
		for _, m := range matches {
			put(strings.TrimSuffix(filepath.Base(m), ".selection"), m)
		}
	}
	if ws, err := workspaceRoot(); err == nil {
		if matches, _ := filepath.Glob(filepath.Join(ws, "*.selection")); matches != nil {
			for _, m := range matches {
				put("workspace/"+strings.TrimSuffix(filepath.Base(m), ".selection"), m)
			}
		}
	}
	return out
}

// handleSelections lists every known selection lockfile: the named registry,
// *.selection files in the server's working directory, and the mirror
// workspace root (~/trailboss). An unreadable lockfile is listed with an
// error, not dropped.
func (s *Server) handleSelections(w http.ResponseWriter, r *http.Request) {
	known := knownSelections()
	names := make([]string, 0, len(known))
	for n := range known {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]selectionSummary, 0, len(names))
	for _, n := range names {
		out = append(out, summariseSelection(n, known[n]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"selections": out})
}

func summariseSelection(name, path string) selectionSummary {
	sum := selectionSummary{Name: name, Path: path}
	sel, err := selection.Read(path)
	if err != nil {
		sum.Error = err.Error()
		return sum
	}
	n := len(sel.Repos)
	sum.RepoCount = &n
	sum.Owner = sel.Owner
	sum.ResolvedAt = sel.ResolvedAt.Format(time.RFC3339)
	_, sum.Digest = selection.Digest(sel)
	return sum
}

// resolveSelectionRef turns a UI selection reference into a lockfile path. It
// resolves only known entries — the names knownSelections advertises — plus
// explicit lockfile paths inside the server's working directory (e.g.
// "roundup.selection"). Anything else (absolute paths, ".." escapes, unknown
// bare names) is refused.
func resolveSelectionRef(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("selection is required")
	}
	if p, ok := knownSelections()[ref]; ok {
		return p, nil
	}
	if isBareName(ref) {
		return "", fmt.Errorf("no selection named %q — run `trailboss select` first", ref)
	}
	// The "workspace/" prefix is reserved for workspace entries: a reference
	// with that prefix never falls through to a CWD-relative path.
	if strings.HasPrefix(ref, "workspace/") {
		return "", fmt.Errorf("no selection at %q — run `trailboss select` first", ref)
	}
	clean := filepath.Clean(ref)
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("invalid selection path %q: must be a known selection name or a path inside the server's working directory", ref)
	}
	return clean, nil
}

// isBareName reports whether ref looks like a registry name rather than a
// path: no separators, no extension.
func isBareName(ref string) bool {
	return !strings.ContainsAny(ref, `/\`) && filepath.Ext(ref) == "" && ref != "." && ref != ".." && !strings.Contains(ref, "..")
}

// handleSelectionDetail returns the full lockfile for ?name=.
func (s *Server) handleSelectionDetail(w http.ResponseWriter, r *http.Request) {
	path, err := resolveSelectionRef(r.URL.Query().Get("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sel, err := selection.Read(path)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	_, digest := selection.Digest(sel)
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      path,
		"digest":    digest,
		"selection": sel,
	})
}

// selectRequest is the POST /api/select body.
type selectRequest struct {
	Org      string   `json:"org"`
	Topics   []string `json:"topics"`
	AllRepos bool     `json:"allRepos"`
	Name     string   `json:"name"` // optional: save into the named registry
}

// handleSelect resolves an org's repos and freezes a new selection lockfile.
// Without Name it writes ./<org>.selection in the server's working directory;
// with Name it writes into the named registry. Re-running select refreshes
// the lockfile in place (the CLI's select does the same). The response
// carries a stable "name" reference that round-trips through
// /api/selection?name= and POST /api/jobs.
func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	if s.token == "" {
		writeError(w, http.StatusServiceUnavailable, models.TokenEnvVar+" is not set in the server environment")
		return
	}
	var req selectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Org == "" {
		writeError(w, http.StatusBadRequest, "org is required")
		return
	}
	if !validSelectionName(req.Org) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid org %q: use a plain GitHub login", req.Org))
		return
	}
	sel, path, err := runSelectOp(r.Context(), s.token, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	_, digest := selection.Digest(sel)
	// A stable reference for the UI: the registry name when one was given
	// (it resolves through knownSelections), else the CWD lockfile name.
	name := req.Name
	if name == "" {
		name = req.Org + ".selection"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":          name,
		"selectionPath": path,
		"digest":        digest,
		"selection":     sel,
	})
}

// jobRequest is the POST /api/jobs body.
type jobRequest struct {
	Kind          string   `json:"kind"` // mirror | scan | apply
	Selection     string   `json:"selection"`
	Pattern       string   `json:"pattern"`       // scan
	DryRun        *bool    `json:"dryRun"`        // apply; default true
	Mode          string   `json:"mode"`          // apply: pr | direct; default pr
	Script        []string `json:"script"`        // apply
	ScriptLine    string   `json:"scriptLine"`    // apply: shell-style single line, split server-side
	Branch        string   `json:"branch"`        // apply pr-mode
	CommitMessage string   `json:"commitMessage"` // apply
	Sign          string   `json:"sign"`          // apply
	Confirm       bool     `json:"confirm"`       // apply live
	Owners        []string `json:"owners"`        // apply direct-mode allow-list
}

// handleJobCreate validates the request, resolves the selection lockfile,
// registers the job, and starts it in the background.
func (s *Server) handleJobCreate(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch req.Kind {
	case "mirror", "scan", "apply":
	default:
		writeError(w, http.StatusBadRequest, "kind must be mirror, scan, or apply")
		return
	}
	path, err := resolveSelectionRef(req.Selection)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sel, err := selection.Read(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "cannot read selection: "+err.Error())
		return
	}
	if err := validateJobRequest(req, sel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if (req.Kind == "mirror" || req.Kind == "apply") && s.token == "" {
		writeError(w, http.StatusServiceUnavailable, models.TokenEnvVar+" is not set in the server environment")
		return
	}
	j := s.reg.add(req.Kind)
	j.logf("job %s: %s over selection %s (%d repos)", j.ID, req.Kind, path, len(sel.Repos))
	go s.runJob(context.Background(), j, req, sel, path)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": j.ID})
}

// validateJobRequest checks kind-specific fields before a job is registered.
func validateJobRequest(req jobRequest, sel models.Selection) error {
	if len(sel.Repos) == 0 {
		return fmt.Errorf("selection is empty — nothing to run")
	}
	switch req.Kind {
	case "scan":
		if req.Pattern == "" {
			return fmt.Errorf("scan needs a pattern")
		}
	case "apply":
		if len(req.Script) == 0 && strings.TrimSpace(req.ScriptLine) == "" {
			return fmt.Errorf("apply needs a script (script array or scriptLine)")
		}
		if req.CommitMessage == "" {
			return fmt.Errorf("apply needs a commitMessage")
		}
		if !models.IsValidSignMode(req.Sign) {
			return fmt.Errorf("invalid sign %q: must be one of %s", req.Sign, strings.Join(models.SignModes(), ", "))
		}
		mode := req.Mode
		if mode == "" {
			mode = "pr"
		}
		switch mode {
		case "pr":
			if req.Branch == "" {
				return fmt.Errorf("pr-mode apply needs a branch")
			}
		case "direct":
			if len(req.Owners) == 0 {
				return fmt.Errorf("direct-mode apply needs owners: the owner allow-list gate")
			}
			if req.Sign == models.SignGitHub {
				return fmt.Errorf("sign %q is PR-mode only; direct push needs %q or %q", models.SignGitHub, models.SignLocal, models.SignNone)
			}
		default:
			return fmt.Errorf("invalid mode %q: pr or direct", req.Mode)
		}
		dry := req.DryRun == nil || *req.DryRun
		if !dry && !req.Confirm {
			return fmt.Errorf("a live apply needs confirm: true")
		}
	}
	return nil
}

// handleJobsList returns job summaries, newest first. Each job is encoded
// from an immutable snapshot so a running job's mutations can't race the
// JSON encoder.
func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	jobs := s.reg.list()
	dtos := make([]jobDTO, 0, len(jobs))
	for _, j := range jobs {
		dtos = append(dtos, j.snapshotDTO(false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": dtos})
}

// handleJobDetail returns one job with its full log, from an immutable
// snapshot (see handleJobsList).
func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	j := s.reg.get(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, http.StatusOK, j.snapshotDTO(true))
}

// handleJobEvents streams the job log over server-sent events: first a replay
// of the log so far, then live lines, then a final status frame. The
// subscribe and the log-length capture are one atomic step (subscribeLive),
// so a job that goes terminal mid-replay still delivers its terminal frame
// and the stream always closes — and no line is missed or duplicated.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	j := s.reg.get(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	send := func(ev sseEvent) bool {
		payload, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	sub, n, status := j.subscribeLive()
	if sub != nil {
		defer j.unsubscribe(sub)
	}
	for _, line := range j.logPrefix(n) {
		if !send(sseEvent{Type: "log", Line: line}) {
			return
		}
	}
	if !send(sseEvent{Type: "status", Status: string(status)}) {
		return
	}
	if status.terminal() {
		return
	}
	for {
		select {
		case msg, ok := <-sub:
			if !ok {
				return // terminal status was delivered as a control frame
			}
			if strings.HasPrefix(msg, "\x00") {
				var ev sseEvent
				_ = json.Unmarshal([]byte(msg[1:]), &ev)
				send(ev)
				return
			}
			if !send(sseEvent{Type: "log", Line: msg}) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// validSelectionName rejects names that are not safe single-segment filenames.
func validSelectionName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}
