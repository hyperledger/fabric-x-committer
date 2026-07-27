---
name: doc-audit
description: >-
  Audit the repository's documentation, agent instructions, other skills, config
  samples, and config templates for staleness introduced by a code change — every
  *.md, *.yaml/*.yml, and *.tmpl file. Use AFTER finishing any development task that
  touched Go code, config fields, CLI flags, metrics, make targets, scripts, or
  file/package layout, and whenever the user asks to "check the docs", "audit the
  docs", or "did I make anything obsolete?". It auto-fixes the repo's generated docs
  (metrics reference, CLI help, loadgen sample tree) and reports likely-stale prose,
  agent instructions, skills, and config templates with file:line and a suggested
  edit. Runs well on a dispatched subagent to keep the parent context clean. It
  only audits EXISTING files for staleness a code change introduced — do not use
  it to author or edit documentation (writing a new doc, adding a section,
  rewriting prose). For writing new Go code use the `development` skill; for
  tests use `tests`; for PR review use `pr-review`.
---

# Auditing docs, config, and agent instructions after a change

A development task can quietly invalidate documentation, agent-instruction files,
other skills, config samples, or config templates: a renamed config field, a moved
package, a removed CLI flag, a changed make target. This skill audits the whole
`*.md` / `*.yaml` / `*.tmpl` corpus against the change that just landed.

**Half of this is already deterministic and gated by `make lint`.** Do not reinvent
it — orchestrate it (Tier A), then spend real effort on the semantic gap nothing
checks (Tier B).

## Run this on a subagent

Prefer dispatching this skill to a subagent: the corpus grep and the `make build`
noise would otherwise flood the parent context. The subagent's **only** deliverable
is the report in the [Report](#report) format below — grounded in the diff, not in
conversation history — so it hands back cleanly.

## Step 1 — Establish the change set

Everything the audit checks is keyed off the actual diff. Do not audit from memory.

```bash
base=$(git merge-base main HEAD)
git diff --stat "$base"   # files touched since branching from main, incl. uncommitted work
git diff "$base"          # the full diff (committed on this branch + working tree)
```

`git diff "$base"` covers "branch-vs-main **plus** working tree" in one command. When
you are on `main`, `base` equals `HEAD`, so it degrades to the uncommitted diff — the
correct fallback.

From the diff, extract the change vocabulary you will grep for in Step 3:

- **Config fields** — added/renamed/removed `mapstructure:"..."` tags in `*.go`.
- **CLI flags / commands** — changes under `cmd/` (cobra commands, flag names).
- **Symbols** — renamed/removed exported and unexported funcs, types, methods.
- **Paths** — moved/renamed/deleted files and packages.
- **Service names** — sidecar / coordinator / verifier / vc / query / loadgen / mock.
- **Metrics** — changes in any `service/*/metrics.go`.
- **`workload.Profile` fields** — changes under `loadgen/workload/`.
- **Make targets / scripts** — changes to `Makefile` or `scripts/`.

## Step 2 — Tier A: mechanical checks (auto-fix derived docs)

Always run the full set, regardless of what the diff touched.

```bash
make build                                                 # needed by the CLI & sample-tree checks
make check-metrics-doc check-cli-doc check-sample-tree     # read-only; each prints a diff on drift
go test ./cmd/config/...                                   # every samples/*.yaml must load
```

For any **check that fails**, run its generator to fix the derived doc in place, then
record it under **Auto-fixed**:

| Failing check | Fix command | Regenerated file |
|---|---|---|
| `check-metrics-doc` | `make generate-metrics-doc` | `docs/metrics_reference.md` |
| `check-cli-doc` | `make generate-cli-doc` | `docs/cli/committer.md` |
| `check-sample-tree` | `make generate-sample-tree` | `docs/loadgen-artifacts.md` |

`go test ./cmd/config/...` is **report-only**: a failure means a `cmd/config/samples/*.yaml`
no longer matches its config struct. That is a code/config bug for the developer — put
it under **Must-fix**, do not try to "regenerate" a sample.

## Step 3 — Tier B: semantic sweep (report, do not edit)

The judgment pass over the corpus that no tool checks: prose docs, agent-instruction
files, other skills, block diagrams, and the `.tmpl` config templates (nothing renders
templates in CI against the config structs, so a renamed field silently rots them).

For each changed artifact from Step 1, grep the corpus and judge each hit against this
reference map. **Ground every finding in the diff — no speculative findings.**

| Change in code | Files that can go stale |
|---|---|
| Config field (`mapstructure` tag) | `cmd/config/samples/*.yaml`, `cmd/config/templates/*.yaml.tmpl`, service docs in `docs/`, the `development` skill's config-pipeline note |
| CLI flag / command | `docs/cli/committer.md` (Tier A), `docs/setup.md`, `README.md`, `docs/deployment-guide.md`, `docker/README.md` |
| Service / package / file / symbol | `docs/*.md` prose + `docs/*-block-diagram.md`, `CLAUDE.md`, `AGENTS.md`, `.bob/rules/*.md`, `.bob/skills/*/SKILL.md` (skills cite exact paths/symbols — e.g. the `development` skill's reuse table and `references/service-construction.md`) |
| Metric | `docs/metrics_reference.md` (Tier A) |
| `workload.Profile` field | `cmd/config/samples/loadgen.yaml`, `cmd/config/templates/loadgen_shared.yaml.tmpl`, `docs/loadgen-artifacts.md` (Tier A), and the env-override test |
| Make target / script / dev workflow | `CLAUDE.md`, `AGENTS.md`, `docs/setup.md`, any skill that names the command |

Grep the whole corpus, not just `docs/`:

```bash
git grep -n "<old-name>" -- '*.md' '*.yaml' '*.yml' '*.tmpl'
```

For each hit ask: does the change make this reference **false, obsolete, or incomplete**
(e.g. a new required config field missing from a sample/template, a renamed flag, a
deleted file still cited)? If yes, it is a finding.

## Report

Return one Markdown report, these four sections, in this order. Omit a section only if
it is empty (but always emit **Clean** when there are no findings at all).

```markdown
## doc-audit report

### Auto-fixed
- `docs/metrics_reference.md` — regenerated via `make generate-metrics-doc` (metric `xxx_total` renamed).

### Must-fix
- `cmd/config/samples/vc.yaml` — fails to load: unknown field `old-name` (renamed to `new-name` in service/vc/config.go). Update the sample.

### Review
- `docs/validator-committer.md:88` — references `resource-limits.max-workers-for-x`, renamed to `...max-workers-for-y`. Suggested edit: rename the key in the config block.
- `cmd/config/templates/vc.yaml.tmpl:14` — templates the removed field `min-transaction-batch-size`. Suggested edit: drop the line.

### Clean
- No stale references found for: <list the changed artifacts you checked and cleared>.
```

## Guardrails

- **Never edit** prose, agent-instruction, skill, or `.tmpl` files. Tier A regenerates
  *derived* docs only; everything else is a suggested edit in the report.
- **Diff-grounded only.** Every finding must trace to a specific change in Step 1's
  diff. Do not report pre-existing doc issues unrelated to this change.
- **State uncertainty.** If you cannot confirm a reference is stale, mark it
  "⚠️ possible" in **Review** rather than asserting it.
- **No silent scope cuts.** If you skip part of the corpus (e.g. `make build` fails so
  Tier A could not run), say so explicitly in the report.
