# ZUSCAR: Patch Workflow v2 (public repos, Claude pulls from GitHub)

**Part A** is pasted into Claude once per chat (fill in the three lines at the top first). **Part B** is your cheat sheet. **Part C** is the one-time repo setup.

---

# PART A: PASTE THIS INTO CLAUDE

## Repo registry (fill in once, then never paste URLs again)

```
BACKEND_REPO=https://github.com/MboyaDan/zuscar_backend
FRONTEND_REPO=https://github.com/MboyaDan/zuscar_frontend
DEFAULT_BRANCH=main
```

## Workflow rules (replace the zip-per-stage rules in the master prompt)

**Roles.** My GitHub repos are the source of truth. Your sandbox is a scratch copy that you rebuild from GitHub whenever needed. You can read GitHub but you cannot push. I apply your patches, commit, push and merge. Docker is for the backend only; the frontend has no Dockerfile.

**1. Pull, do not ask.** At the start of every stage, and on "resume" in a new chat, clone or fetch both repos from the registry URLs and work from `origin/<branch>`. Branch is `DEFAULT_BRANCH` unless I name another. Never ask me to paste a URL or hash. Prove access at the start of the first stage with `git ls-remote <url>` and tell me if it fails; only then ask me to upload a zip or `git bundle` instead.

**2. Confirm my last patch landed.** My report-back is "continue". Before editing, check that the commit subject you gave me for the previous patch appears in `git log origin/<branch> -5` of the right repo. If it does not, stop and say "I do not see <subject> on origin/<branch>; did you push?" Do not build on top of something that has not landed.

**3. Read project memory from the repo.** `docs/PROGRESS.md`, `docs/ASSUMPTIONS.md`, `docs/traceability.md`, the brief (`docs/system-requirements.docx`, read with `extract-text`) and the master prompt and this workflow (`docs/claude/`) live in the backend repo. Re-read what the stage needs from there instead of relying on chat memory. Update `docs/PROGRESS.md` in every stage.

**4. Patch format.** Work in your clone on a working branch. When the stage is done, run `git status --short`, make sure only intended files changed, then `git add -A && git diff --cached --binary > <name>.patch`. Name: `zuscar-<backend|frontend>-<stageNN>-<topic>.patch`, and for follow-ups `zuscar-<repo>-<topic>-fixN.patch`. Never include `go.sandbox.mod`, `.env`, `node_modules`, build output or other generated junk. Keep each patch small (roughly 1,500 changed lines or fewer; split a stage if larger). Use targeted edits, not whole-file rewrites. Binary files (logos, icons) go in via `--binary`. Lockfile changes are included and mentioned.

**5. Verify before handing over.** In a fresh clone of the base, run `git apply --check` then `git apply` for the patch, then run every check the sandbox allows (backend: `go build ./...`, `go vet ./...`, `go test ./...`; frontend: `npm ci`, `npm run lint`, `npx tsc --noEmit`, `npm run build`). Report real results only.

**6. Sandbox workarounds stay out of the patch.** If a Go proxy or `replace golang.org/x/...` trick is needed to build here, use a separate `go.sandbox.mod` with `-modfile` and keep it out of the patch. If `go.sum` may be incomplete, say so and tell me to run `go mod tidy`.

**7. Repo ownership.** Backend repo: migrations, queries, `openapi.yaml`, `docs/`, Dockerfile, Compose, backend CI. Frontend repo: the Next.js app and its README. When a stage touches both, give the backend patch first and state the `openapi.yaml` expectation for the frontend patch. Migrations are never run automatically; tell me when I must run one.

**8. CI.** Backend CI (GitHub Actions) uses a PostgreSQL service container and sets `TEST_DATABASE_URL`, so the overlap, idempotency, concurrency and ledger tests run after I push. After I push and say "continue", try to read the run status through the public GitHub API (`api.github.com/repos/<owner>/<repo>/actions/runs`) and tell me pass or fail and the failing step. If you cannot read the logs, ask me to paste the failing step's output.

**9. Every stage report** keeps Built / Ran (with results) / Not run / Need from you / Next, then adds one **Apply block per repo**, backend first, laid out exactly like this (fill in real names, branch and message):

```
cd ~/path/to/zuscar-backend
git checkout main && git pull
git checkout -b <branch>
git status
cp ~/Downloads/<patch-name>.patch .
git apply --check <patch-name>.patch
git apply <patch-name>.patch
rm <patch-name>.patch
go build ./...
go vet ./...
go test ./...
git status --short
git add -A
git commit -m "<type>(<scope>): <subject, 72 characters or fewer>"
git push -u origin HEAD
```
(Frontend block uses `npm ci` when the lockfile changed, then `npm run lint`, `npx tsc --noEmit`, `npm run build`.) For follow-up fixes on a branch I am already on, use `git checkout <branch>` and omit the `-b` line.

Under the block give: the commit message (Conventional Commits: feat, fix, docs, test, chore, refactor; add `[AC-xx]` when relevant), the PR title and body, the squash-merge message, and the merge and deploy order, including whether a migration must run before the frontend merges.

**10. If something fails.** `git apply` is all-or-nothing, so a failed apply changes nothing. I will paste the error and you fix it with a small new patch against my current branch, never a cumulative one. If I edit code by hand, I will tell you and push first; you will see it when you pull.

---

# PART B: YOUR CHEAT SHEET

## Each stage

1. Claude gives you an Apply block per repo. Run it in a terminal (Git Bash on Windows works).
2. If `go build` complains about missing go.sum entries, run `go mod tidy`, then include go.mod and go.sum in the same commit.
3. Look at `git status --short` before `git add -A`. Anything you do not recognise (a `.env`, a database dump, a binary) must not be added; fix `.gitignore` instead.
4. Push, open a pull request on GitHub, wait for CI, merge, then `git checkout main && git pull`.
5. Reply "continue" in the chat.

## Pull request template

**Title:** same as the commit message.

```
## What
One or two lines.

## Why
Stage and acceptance criteria (AC-xx, NN-xx).

## Tested
Commands run and real results (local and CI).

## Not verified
What was not run, and who runs it.

## Migration and deploy notes
Migration added: yes/no. Must run before deploy: yes/no.
New environment variables: none / list.
Merge order: backend first, then frontend.

## Rollback
Revert commit; migration: forward-fix or documented down step.
```

**Squash-merge message:** `<type>(<scope>): <subject> (#NN)`.

## If a patch will not apply

Do not edit by hand. Paste the full `git apply --check` error to Claude. Common causes: you changed the same files since Claude pulled (push first, say "continue", Claude pulls again), or you applied it to the wrong branch.

## Starting a new chat

Paste Part A (with the three repo lines filled in) and write "resume". No zip, no hashes, no URLs again. Claude clones both repos, reads `docs/PROGRESS.md` and carries on. The master prompt and brief are already in the repo, so you do not need to attach them, but attach the logo files if Claude has not committed them yet.

---

# PART C: ONE-TIME SETUP

1. Commit what exists now to `main` in both repos and push (public).
2. Add to the **backend repo** `docs/`: `system-requirements.docx` (the v2 brief), `claude/MASTER_PROMPT.md` (the master prompt), `claude/WORKFLOW.md` (this file), and, if Claude has not already, `PROGRESS.md`, `ASSUMPTIONS.md`, `traceability.md`.
3. Add `.gitattributes` (`* text=auto eol=lf`) and a correct `.gitignore` (`.env`, `node_modules`, `.next`, build output, `go.sandbox.mod`) to both repos. On Windows run `git config core.autocrlf true`; on Mac or Linux `git config core.autocrlf input`.
4. **Public repo warning.** Anyone can read public code. Never commit secrets, real credentials, real staff names, real M-Pesa data or real venue data. `.env.example` holds placeholders only. Before production data or the pilot, make both repos **private**. After that Claude cannot pull, so upload a zip or `git bundle` at the start of each chat instead.
5. Keep `main` as production. Vercel and Render deploy from it, so do all work on branches and merge through pull requests with CI passing (turn on branch protection).
6. Make sure the backend CI runs with a Postgres service and `TEST_DATABASE_URL`, and the frontend CI runs lint, `tsc --noEmit` and build.
