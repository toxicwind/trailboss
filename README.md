# trailboss

[![CI](https://github.com/toxicwind/trailboss/actions/workflows/ci.yml/badge.svg)](https://github.com/toxicwind/trailboss/actions/workflows/ci.yml)
[![zizmor](https://github.com/toxicwind/trailboss/actions/workflows/zizmor.yml/badge.svg)](https://github.com/toxicwind/trailboss/actions/workflows/zizmor.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/toxicwind/trailboss/badge)](https://scorecard.dev/viewer/?uri=github.com/toxicwind/trailboss)
[![SLSA 3](https://img.shields.io/badge/SLSA-Level_3-green)](https://slsa.dev/spec/v1.0/levels#build-l3)
[![Release](https://img.shields.io/github/v/release/toxicwind/trailboss)](https://github.com/toxicwind/trailboss/releases/latest)
[![License](https://img.shields.io/github/license/toxicwind/trailboss)](LICENSE)

**The trailboss runs the herd.** One command rides out, cuts every repo in the
org matching your mark, and freezes the list in a tally book. Then it drives
the herd into the local corral, lets you look the whole bunch over, and runs
your change through every head — dry-run first, PRs only when you say so.

trailboss resolves a set of repos once — by org/user and topic — freezes it as
a reviewable **selection** lockfile, then drives two mature tools against that
exact set: **[ghorg](https://github.com/gabrie30/ghorg)** to mirror the repos
locally, **[multi-gitter](https://github.com/lindell/multi-gitter)** to apply a
change and open the PRs. It clones nothing and opens no PRs itself. It's **built
to be driven by AI agents as much as by people**.

## The drive, to copy

```sh
# 1. the roundup — cut the herd, freeze the tally → ./trailboss.selection
trailboss select --org mycompany --topic platform
# 2. drive 'em into the corral — local clones, no API reads
trailboss mirror
# 3. look the herd over — regex across the local clones, zero GitHub rate limit
trailboss scan "golang:1.22"
# 4. DRY-RUN the change through every head (opens nothing);
#    add --dry-run=false --confirm to open the PRs
trailboss apply --branch bump-go --commit-message "Bump Go" --pr-title "Bump Go" \
  --sign local -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile
```

> The repos you mirror, scan, and change are **provably the same herd** —
> frozen in one tally book, so no filter can drift between phases.

## Why

An agent doing herd work ("which repos still pin `golang:1.22`? patch this CVE
everywhere") that reaches for the GitHub API hits two walls: **rate limits**
(5,000 REST req/hr, plus a stricter ~80 content-writes/min secondary limit) and
**latency** (every read is a paginated round-trip). trailboss spends the API
budget only where it must:

1. **Round up once, cheaply** — one read-only API pass turns `--org`/`--topic`
   into a concrete repo set, frozen in the tally book.
2. **Read by driving, not by API** — `mirror` + `scan` work the local corral;
   `git` isn't governed by REST limits, so the high-volume "what do I change?"
   work is free and fast.
3. **Write under the limit** — `apply` batches PR creation with pauses to stay
   under the secondary limit.

## Install

```sh
go install github.com/toxicwind/trailboss@latest

# or the one-line installer (grabs the right prebuilt binary, verifies its checksum):
curl -sSfL https://raw.githubusercontent.com/toxicwind/trailboss/main/install.sh | sh
```

trailboss needs **[ghorg](https://github.com/gabrie30/ghorg)** and
**[multi-gitter](https://github.com/lindell/multi-gitter)** on your PATH — it
drives them, it doesn't replace them.

Releases carry [SLSA Level 3](https://slsa.dev/spec/v1.0/levels#build-l3)
provenance and per-asset SHA-256 sidecars, and the build is reproducible
(`make repro VERSION=<tag>` rebuilds the tag and prints a bit-for-bit-matching
hash). Prebuilt binaries, `go install`, and source-verification steps are all
in [`trailboss guide`](#docs).

**Auth:** if you use the GitHub CLI there's nothing to set up — trailboss picks
up your `gh auth login` session automatically. In CI (no interactive login),
set `TRAILBOSS_PAT` to a PAT with Contents + Pull requests read/write.
trailboss maps the one token to the env vars ghorg and multi-gitter each
expect. You also need a **git identity** (`git config user.name`/`user.email`)
— multi-gitter authors the `apply` commit from it.

## Commands

| Command | What it does |
|---|---|
| `select` | the roundup — resolve repos by org/user + topic, freeze the tally book |
| `mirror` | drive the frozen herd into the local corral via ghorg (into `~/trailboss`) |
| `scan <pattern>` | look the herd over — read-only regex across the local mirror, no API |
| `apply … -- <cmd>` | run a change through every head and open PRs (via multi-gitter) |
| `check` | count the herd — diff the frozen tally against live discovery (drift) |
| `doctor` | preflight — token source, principal, child tools on PATH, signing |
| `selections` / `workspaces` | manage named tallies and separate corrals |
| `guide` / `schema` | the operator playbook and the JSON-Schema output contract |

Every read command takes `--json` (machine data on stdout, human banners on
stderr) and `--quiet` for compact, token-cheap output. Exit codes are a stable
contract: `0` success, `1` a domain outcome (drift / failed check), `2` error.

For a one-off campaign, `mirror --purpose <name>` drives the herd into a
fresh, timestamped `~/trailboss/<name>-<stamp>` corral instead of the shared
one, so parallel efforts don't mix; `trailboss workspaces` lists and prunes
them.

## Safety model

- **`apply` is dry-run by default.** A real run additionally needs
  `--dry-run=false --confirm`, and an agent may only do it with explicit human
  authorization — otherwise the real run is the human's to execute.
- **`--sign` is required on every `apply`** — `local` (your GPG key via git),
  `github` (GitHub's web-flow key), or `none` (unsigned). There is no default;
  trailboss states which mode it used.
- **Provable-same-herd.** `mirror`, `scan`, and `apply` all read the one tally
  book and never re-discover, so the repos you inspect and the repos you change
  are the same list, in one artifact you review before anything runs.
- trailboss **never runs `git` itself and never writes to GitHub directly** —
  discovery is read-only REST, mirroring is ghorg, PRs are multi-gitter.

## For AI agents

trailboss is mostly operated by AI agents. `trailboss guide` prints a compact
operator playbook that travels with the binary; the tally book is JSON and
every error names the next action, so an agent can self-orient without this
README.

`trailboss mcp` serves trailboss's **read-and-plan** surface over the
[Model Context Protocol](https://modelcontextprotocol.io) on stdio — the CLI's
machine surface as typed tools. Register it with Claude Code:

```sh
claude mcp add trailboss trailboss mcp
```

`apply` is deliberately **not** an MCP tool: opening PRs stays a human action.
`apply_plan` instead returns the exact, digest-bound `trailboss apply` command
for a human to review and run.

Tell your own agents to reach for it — drop this into your repo's `AGENTS.md`:

```md
For any change spanning many repos (bump a dependency, patch a CI config, rotate a
setting), use the `trailboss` CLI rather than hand-rolling clone/PR loops. Run
`trailboss guide` for the playbook. A real `trailboss apply --dry-run=false
--confirm` opens PRs and needs explicit human authorization — always dry-run first.
```

## The ranch

trailboss is worked and ridden from the **ranch** ([toxicwind/ranch](https://github.com/toxicwind/ranch)),
where it holds a top-level spread as `trailboss/` — real tracked files, no
nested repo — alongside `roundup/` (the benchmark roundup) and `stockyard/`
(the model herd). This repo is trailboss's standalone home; the ranch vendors
the same source in-tree.

## Defork note

trailboss is a defork of [redscaresu/goldfinger](https://github.com/redscaresu/goldfinger)
— renamed, re-themed, and adopted into the ranch. Upstream history is preserved
in the git log. Changes from upstream:

- module, binary, and docs renamed `goldfinger` → `trailboss`
- `GOLD_FINGER_PAT` → `TRAILBOSS_PAT`
- western-themed docs and README

## Docs

- **`trailboss guide`** — the full operator playbook (every command, flag, and
  auth/install detail), printed from the binary. `guide --json` is the
  machine-readable input catalogue.
- **`trailboss schema`** — the JSON Schema (draft 2020-12) for the tally book
  and every payload, so a consumer can validate trailboss's output.
- **`AGENTS.md`** / **`CLAUDE.md`** — contributor-agent rules for changing
  trailboss's own code.

## Development

```sh
make check   # go build + vet + race tests + lint (mirrors CI)
make e2e     # full-pipeline test against a sandbox repo (needs TRAILBOSS_PAT + gh)
make hooks   # install the gitleaks pre-commit hook
```

CI runs the unit tests, gitleaks, govulncheck, and an end-to-end job that opens
and tears down a real PR on a sandbox repo.
