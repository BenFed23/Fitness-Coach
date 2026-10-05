# Fitness Agent

After every change I approve, run the commit-push skill.

## Project status

**What it does:** decides each morning whether to train (go / light / rest) from Fitbit recovery data
and the weather, tells you on Telegram, and moves today's workout on Google Calendar on request.

**Architecture**
- `cmd/agent` (Go) - the agent. Fitbit data via the Google Health API (sleep, resting HR, HRV, breathing,
  skin temp), estimated readiness score (`readiness.go`), Open-Meteo weather, Calendar moves (`move.go`),
  short-workout fallback (`shortworkouts.go`), OAuth incl. renewal from a phone (`authflow.go`).
  Every operation has `-json` output; the bot only talks to the agent through it.
- `telegram_bot/` (Python, python-telegram-bot) - runs the agent as a subprocess (`services.py`).
- `configs/short_workouts.json` - hand-maintained YouTube library. Tokens/history live next to the binary
  (`data/` on the server). Docker + `DEPLOY.md` for a VPS.
- `internal/` - older unused packages (not wired into `cmd/agent`, not gofmt-clean). Leave alone.

**Done:** Google Health integration, readiness estimate, tracker-not-worn detection, morning message,
`/move` with preview, cascade/supersede/short-workout fallback (type + muscle group A/B/full),
token expiry reminders + renewal via Telegram, tests for all of the above, repo on GitHub.

**Known issues**
- OAuth app is in Testing: Google tokens expire 7 days after approval (bot reminds; renew via `/tokens`).
- `configs/short_workouts.json` still has only 2 sample entries with empty `url` - user must fill it.
- Bot never run against real Telegram (no bot token yet); Docker image never built (Docker Desktop off).
- Readiness is our estimate - Fitbit's own score isn't in the API.
- Calendar workouts are recognized only by titles starting with `אימון` / `ריצה` or `[A]/[B]/[RUN]`.
- git `core.autocrlf` is on (CRLF in the working copy) - matters when copying files to a Linux server.

**Next tasks**
1. README for the public repo (what it is, setup, Google/Telegram config, no secrets).
2. Rotate the OAuth client secret (it was exposed in a chat) and update GoLand + `.env`.
3. Create the Telegram bot, test locally, then deploy per `DEPLOY.md`.
