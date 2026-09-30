# Changelog

## 2026-09-29 — Standalone trailboss

- **Defork**: `redscaresu/goldfinger` → `toxicwind/trailboss` — renamed, re-themed, adopted (`25bf14c`)
- **Direct-to-main apply**: `trailboss apply --mode=direct --direct-owners=toxicwind` pushes straight to each repo's default branch — owner allow-list gate, fetch-first, never force-push (`b4126d6`)
- **The ranch office**: `trailboss serve` — embedded web UI + JSON API on `127.0.0.1:25250`, vanilla HTML/JS/CSS in the binary (`b4126d6`)
- **Agentic upstream ownership** (primary feature): `trailboss upstream status|merge|watch|init` — watches renamed forks' upstreams, merges on fresh branches, runs test suites, reports; never pushes to main (`fcb629a`)
- `UPSTREAM-MERGE.md`: dogfooding the upstream feature against its own fork relationship (`64bd7ac`)

## 2026-08-18 and earlier (upstream goldfinger)

- README slimmed to an overview with a real VHS demo GIF
- SLSA-3, release, and license badges; zizmor + OpenSSF Scorecard in CI
- Multi-gitter version-floor advisory surfaced at runtime
- Dependency bumps via Dependabot/Renovate
