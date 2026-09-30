# Upstream Merge Workflow — trailboss

**Fork:** `toxicwind/trailboss`
**Upstream:** `redscaresu/goldfinger`
**Renamed:** 2026-09-30 (goldfinger → trailboss, defork + western theme)

This repo **is** the agentic upstream owner. It uses its own `upstream` command to manage its upstream relationship — dogfooding the primary feature.

## Our Patches (what to protect)

1. **Defork:** rename goldfinger → trailboss, TRAILBOSS_PAT, western-themed README
2. **Ranch patches:** `apply/direct.go` (direct-to-main), `cmd/serve.go` + `cmd/webui/` + `serve/` (web UI)
3. **Upstream feature:** `upstream/` package + `upstream` command (status/merge/watch/init) — the agentic upstream ownership

## Merge Procedure (dogfood)

```sh
# trailboss manages its own upstream via itself
TRAILBOSS_FORKS_DIR=/home/toxic/forks trailboss upstream merge trailboss --dry-run
TRAILBOSS_FORKS_DIR=/home/toxic/forks trailboss upstream merge trailboss

# Or manually:
git remote add upstream https://github.com/redscaresu/goldfinger.git
git fetch upstream
git checkout -b upstream-merge/$(date +%Y%m%d-%H%M%S)
git merge --no-ff upstream/main
go test ./...   # must be green
# NEVER push to main directly — human approves
```

## Last Verified

- 2026-09-30: 0 behind, 3 ahead. Up to date.
