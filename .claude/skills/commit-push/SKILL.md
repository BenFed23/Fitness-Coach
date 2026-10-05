---
name: commit-push
description: Safely commit and push changes the user has approved in this repo - checks status/diff, blocks secrets and token files, runs go vet, stages only relevant files, writes an English commit message and pushes to origin main. Use after every change the user approves, or when asked to commit/push.
---

# Commit and push approved changes

Commit **only work the user has approved**. If anything below fails or looks
wrong, stop and report - never work around a check.

The shell is Windows PowerShell 5.1: chain with `;`, no `&&`.

## 1. Review what changed

```powershell
git status --short
git diff --stat
git diff
```

Read the diff. Every changed file must belong to the approved change. Leave
unrelated or unfinished files out, and mention them in the report.

## 2. Never commit these

Stop if any of these appear among the files you're about to stage, even if
`.gitignore` should have caught them:

| Path | Why |
|---|---|
| `token*.json`, `oauth_pending.json` | Google OAuth tokens / pending auth state |
| `.env` (`.env.example` is fine) | Telegram token, `CLIENT_SECRET` |
| `.idea/` | GoLand run configuration holds `CLIENT_SECRET` |
| `*.exe`, `*.pickle`, `data/` | build output, bot state, server data |
| `recovery.json`, `short_workout_history.json` | personal health / usage data |
| `.venv/`, `__pycache__/` | local Python environment |

Then scan the diff for secrets. Any hit means: do not commit, report it.

```powershell
$patterns = @('GOCSPX-', 'ya29\.', '1//0', '[0-9]{8,10}:AA[0-9A-Za-z_-]{30,}', 'AIza[0-9A-Za-z_-]{30,}',
              'sk-[A-Za-z0-9]{20,}', '-----BEGIN [A-Z ]*PRIVATE KEY', 'client_secret\s*[:=]\s*"[^"]+"')
foreach ($p in $patterns) { git diff HEAD -U0 | Select-String -Pattern $p }
```

(Before the first commit there is no `HEAD`; scan `git diff --cached` after
staging instead.)

## 3. Check the code

Run from the repo root; all must pass before committing:

```powershell
go vet ./...
go build ./...
go test ./...
```

Every Go file you're committing must be gofmt-clean (`gofmt -l <files>`
prints nothing). Some older files under `internal/` aren't formatted; don't
reformat them as a side effect of an unrelated change.

If Python files in `telegram_bot/` changed:

```powershell
telegram_bot\.venv\Scripts\python.exe -m py_compile telegram_bot\bot.py telegram_bot\services.py
```

## 4. Stage only the relevant files

Stage by explicit path - never `git add -A` or `git add .`:

```powershell
git add -- cmd/agent/move.go cmd/agent/move_test.go
git diff --cached --stat     # re-check: only the intended files
```

## 5. Commit

Clear English message: a summary line in the imperative, at most ~72
characters, then a blank line and a short body saying what changed and why.
Pass it with a single-quoted here-string (`'@` must start the line):

```powershell
git commit -m @'
Fall back to a short workout when the week is full

When today's workout can't be moved, replace it in place with a video
from configs/short_workouts.json chosen by type, muscle group and
readiness.
'@
```

Use the identity already configured in git; don't change `user.name` or
`user.email`.

## 6. Push

```powershell
git push origin main
git status -sb               # should show: ## main...origin/main
```

Never force-push. If the push is rejected (the remote has new commits), stop
and ask the user instead of pulling, rebasing or overwriting.

## 7. Report

Tell the user the commit hash and summary, the files committed, anything
left uncommitted and why, and whether the push succeeded.
