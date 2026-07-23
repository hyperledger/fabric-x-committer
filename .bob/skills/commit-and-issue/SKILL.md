---
name: commit-and-issue
description: Write a commit message (which becomes the GitHub PR description verbatim) or open a GitHub issue for the Fabric-X Committer repo, following the repo's PR template and conventions. Use whenever the user wants to commit staged changes, write or fix a commit/PR message, prepare a PR description, or open/file/create a GitHub issue for this project — even for terse asks like "commit this", "write the PR description", or "file an issue for this bug".
---

# Commit messages and GitHub issues

This repo squash-merges PRs, and **the commit message body is used verbatim as the GitHub
PR description**. So a commit message here is not a throwaway line — it is the PR
description, reviewed as-is. Write it to the PR template and keep the exact wording the
user approves.

The upstream repo for all `gh` operations (issues, and PRs via `--repo`) is
**`hyperledger/fabric-x-committer`**. PRs, however, are pushed from and opened off **your
fork** (`<your-user>/fabric-x-committer`), not upstream — see
[Push to your fork, then open the PR against upstream](#5-push-to-your-fork-then-open-the-pr-against-upstream).

Pick the workflow that matches the request:

- **Committing changes / writing a commit or PR message** → [Writing a commit message](#writing-a-commit-message)
- **Filing / opening / creating a GitHub issue** → [Opening a GitHub issue](#opening-a-github-issue)

The two compose: a commit's `#### Related issues` often points at an issue you just opened,
and an issue often names the PR that will resolve it.

## Shared conventions

**Component tag `[component]`** — Titles (both commit subjects and most issue titles) start
with a bracketed area tag. It is the affected service/package or a topical area, not always
a directory. Multiple are allowed: `[coordinator, sidecar]`. Pick the primary affected area
and match precedent — skim `git log --oneline -20` and recent issues for the tag that area
usually gets. Common tags seen in this repo:

`verifier`, `coordinator`, `sidecar`, `vc`, `query`, `loadgen`, `mock`, `utils`, `retry`,
`dep` / `dependencies`, `proto`, `docs`, `test` / `integration-test` / `coordinator-test` /
`orderer-test`, `deliverorderer`.

**Formal headers in the commit/PR body** — A commit message body (which is the PR
description) uses **only** the headers from `.github/pull_request_template.md`:
`#### Type of change`, `#### Description`, `#### Additional details (Optional)`,
`#### Related issues`. Never invent others in a commit/PR — no `#### Context`, `#### The gap`,
`#### Problem`, `#### Proposal`, `#### Motivation`, `#### Changes`, `#### Success criteria`,
etc. If something needs saying, it goes inside a formal section (usually `#### Description`),
not under a new heading. (Issues are *not* bound by this — they're less formal; see
[Opening a GitHub issue](#opening-a-github-issue).)

**Issue references** — When referring to an issue or PR, use `#NNN`. The keyword in front of
it decides whether merging a PR **auto-closes** the issue, and the keyword is **always
lowercase** (`resolves`, not `Resolves`):

- **Closing** (auto-closes on merge to the default branch): `resolves` / `fixes` / `closes`.
  Default to lowercase `resolves`, e.g. `- resolves #656`.
- **Non-closing** (references without closing): `- resolves partly #625`, `- address #622`,
  `- related to #625`, `- follows up on #642` — all lowercase too. Use these when the change
  advances an issue but does not fully close it, or is merely related.

## Writing a commit message

### 1. Inspect what is being committed

Run these to ground the message in the actual change and current style:

```bash
git status
git diff --staged        # what will be committed; add `git diff` if nothing is staged yet
git log --oneline -20    # match the current [component] tags and phrasing
```

If nothing is staged and the user asked to commit, confirm what to stage (or stage per their
instruction) before writing the message. Don't guess at scope from unstaged noise.

### 2. Compose the subject line

Format: `[component] Short description`

- Keep it concise (roughly ≤ 70 chars) and specific about what changed.
- **Do not append `(#NNN)`** — GitHub adds the PR number on squash-merge automatically.
- Sentence case after the tag is typical; don't stress capitalization — match recent commits.

### 3. Compose the body from the PR template

The body **is** the PR description. Follow `.github/pull_request_template.md`, and **use only
its headers — do not invent new ones** (no `#### Changes`, `#### Motivation`, `#### The gap`,
`#### Success criteria`, etc.). Strip every HTML comment (`<!-- ... -->`) — the template says
to delete them before submitting, so they must not appear in the final message.

The formal headers, in order (all but Type of change and Description are optional):

```
#### Type of change

- <one or more of: Bug fix / New feature / Improvement (improvement to code, performance, etc) / Test update / Documentation update / Breaking change>

#### Description

- <Concise — see below.>

#### Additional details (Optional)

<Optional: implementation notes, how it was tested, deferred follow-ups, notes to reviewers.>

#### Related issues

- resolves #NNN
```

**Description — keep it concise; it is not a pitch.** The linked issue is the pitch: it
carries the motivation, the problem, and the gap. The Description's only job is to tell a
reviewer/developer *what this change does* — what they'll find in the diff. So:

- Lead with what changed, as a bullet list (`-`) when there's more than one point.
- Include only what is **not already in the linked issue** — e.g. a concrete implementation
  choice or a decision worth flagging.
- Describe only what *this* diff changes. Do **not** restate the motivation, the problem, or
  the gap; do **not** propose future work; do **not** list success criteria — all of that is
  the issue's job. The PR describes the change; the issue makes the case. Repeating it just
  makes the reviewer read the same thing twice.
- No selling, no background essay. A few tight bullets usually suffice; a one-line change can
  be one line.

**Type of change**: pick from the template list. A close variant is fine when it fits better
and matches recent commits (e.g. `Dependency update`, or `Design` for a design/discussion).
List several as bullets when the change spans categories (e.g. a bug fix that is also a test
update).

**Related issues — expected, not an afterthought.** Nearly every change here tracks an issue,
and keeping the Description lean depends on that issue holding the context. Ask which issue
this resolves. If there isn't one, treat that as a gap: offer to open one first (see
[Opening a GitHub issue](#opening-a-github-issue)) and link it, rather than silently shipping
without. Use the keyword semantics from [Shared conventions](#shared-conventions) — lowercase
`resolves` to close, `resolves partly` / `related to` when it doesn't fully close. Omit the
section only for a genuinely untracked, trivial change (e.g. a routine dependency bump).

**Never put in the body:** a hand-written `Signed-off-by:` trailer (added by `git commit -s`,
next step), a `---------` separator (inserted by GitHub's squash when it concatenates
commits), or **labels** — labels live on the PR/issue and are applied via `gh`
(e.g. `gh pr edit --add-label ...`), never written into the message text.

### 4. Show the message, then commit with sign-off

Because the wording ships verbatim as the PR description, **show the full drafted message and
get the user's approval before committing.** Adjust until it's exactly what they want.

Then commit. Write the message to a temp file and use `-F` so the multi-line body and blank
lines are preserved, and `-s` to add the DCO `Signed-off-by:` trailer from the user's git
config:

```bash
git commit -s -F /tmp/commit-msg.txt
```

Amend with `git commit -s --amend -F /tmp/commit-msg.txt` if refining an existing commit.

### 5. Push to your fork, then open the PR against upstream

Contributors don't have push access to `hyperledger/fabric-x-committer`, so the branch must
live on **your fork**, and the PR is opened from `your-fork:branch` into
`hyperledger:main`. Never push feature branches to the upstream remote.

Inspect the remotes and pick the fork (the one that is *not* `hyperledger/...`):

```bash
git remote -v
```

Conventionally `origin` is the upstream (`hyperledger/fabric-x-committer`) and a second
remote (often named `fork`, or `origin` when the fork was cloned directly) points at
`<your-user>/fabric-x-committer`. Confirm your fork's owner rather than hardcoding a name:

```bash
FORK_OWNER=$(gh api user -q .login)          # your GitHub login
FORK_REMOTE=fork                             # the remote that points at $FORK_OWNER/fabric-x-committer
```

Push the branch to the fork and open the PR with an explicit cross-repo `--head`:

```bash
git push -u "$FORK_REMOTE" <branch>
gh pr create --repo hyperledger/fabric-x-committer \
  --base main --head "$FORK_OWNER:<branch>" \
  --title "[component] Short description" \
  --body-file /tmp/pr-body.md
```

The `--head owner:branch` form is what makes it a fork PR — omitting the `owner:` prefix
makes `gh` look for the branch on upstream and fail (or, if you accidentally pushed there,
open an upstream-branch PR). If a branch was mistakenly pushed to upstream, delete it with
`git push origin --delete <branch>` and re-point local tracking at the fork
(`git branch --set-upstream-to=$FORK_REMOTE/<branch>`).

The PR body is the commit message body verbatim (subject line and the `Signed-off-by:`
trailer removed). If a PR already exists or you're updating one, reuse the same body with
`gh pr edit --repo hyperledger/fabric-x-committer <num> --body-file /tmp/pr-body.md`. Apply
any labels to the PR through `gh` when applicable (e.g.
`gh pr edit --repo hyperledger/fabric-x-committer <num> --add-label vc,bug`) — never in the
message body.

## Opening a GitHub issue

### 1. Understand the issue

Clarify what the issue is: a bug, a feature/improvement, a design discussion, a task. If it
references code, read the relevant files so the body can cite `file.go:line` and be concrete.

### 2. Title

Format: `[component] Short description`, matching the tag conventions in
[Shared conventions](#shared-conventions). Cross-cutting issues sometimes omit the prefix —
that's acceptable when no single component fits. (Issues that are part of a tracked epic use
that epic's own prefix, e.g. `I-6: [verifier] ...`; only follow that pattern if the user is
adding to such an epic.)

### 3. Body — the issue is where the case is made

Unlike the commit/PR, **issues are less formal** — there's no fixed template, so use whatever
structure fits the issue and suits the author. The one job is to make the case clearly and
concretely: what's broken or missing, and what to do about it, grounded in the code
(`file.go:line`) where relevant.

- A bug or small task can be a couple of sentences: what's wrong, where, and the impact.
- A feature or design proposal can use whatever sections make it readable (context, the gap,
  a proposal, alternatives, open questions, …) — pick what serves the reader.

This is where the motivation and background live (the commit/PR later just points back here),
so the depth the commit avoids is welcome here. Don't force the issue into the PR template.

### 4. Suggest labels, then create

Propose labels from the repo's label set and apply them once the user approves. Typically one
or more **component** labels plus a **type** label:

- **Component**: `coordinator`, `sidecar`, `vc`, `verifier`, `query`, `proto`, `loadgen`,
  `dependency`, `snapshot`, `docs`.
- **Type / kind**: `bug`, `enhancement`, `documentation`, `testing`, `test-coverage`,
  `performance`, `task`, `flaky-test`, `race-condition`, `security`, `breaking`,
  `code-hygiene`, `good-first-issue`, `help-wanted`.

Labels evolve — run `gh label list --repo hyperledger/fabric-x-committer` to confirm current
names before applying, and don't invent labels that don't exist. Labels are applied through
the `gh` command (`--label` below), never written into the issue body.

Show the proposed `gh issue create` command (title, body, labels) for approval, then run it
with `--body-file` so the multi-line body is preserved:

```bash
gh issue create --repo hyperledger/fabric-x-committer \
  --title "[component] Short description" \
  --body-file /tmp/issue-body.md \
  --label enhancement --label coordinator
```

Report the created issue URL back to the user. If it should be referenced by an upcoming
commit/PR, add it to that commit's `#### Related issues`.
