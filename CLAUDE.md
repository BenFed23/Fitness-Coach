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
  `main.go` holds only flag parsing and `main()`. The rest of `cmd/agent`:
  - `weather.go` - location (env or IP), Open-Meteo fetch, weather factor evaluation.
  - `googlehealth.go` - Google Health fetching (sleep, tracker worn, steps, resting HR) and `RecoveryData`.
  - `verdict.go` - severity, factors, recovery evaluation, `BuildAgentReport`, verdicts, report printing.
  - `calendar.go` - calendar types and API calls, workout title categorization, today's workout lookup.
  - `reschedule.go` - `RescheduleMissedWorkout`: plan the cascade/supersede, then apply it.
  - `oauth.go` - core OAuth (config, token storage and refresh, browser flow), used by every command.
  - `util.go` - shared helpers (`getJSON`, `firstNonEmpty`).
  - `authflow.go` - only the phone renewal flow (`-auth` / `-auth-code`).
- `telegram_bot/` (Python, python-telegram-bot) - runs the agent as a subprocess (`services.py`).
- `configs/short_workouts.json` - hand-maintained YouTube library. Tokens/history live next to the binary
  (`data/` on the server). Docker + `DEPLOY.md` for a VPS.

**Done:** Google Health integration, readiness estimate, tracker-not-worn detection, morning message,
`/move` with preview, cascade/supersede/short-workout fallback (type + muscle group A/B/full + equipment),
token expiry reminders + renewal via Telegram, workout adjustment by readiness (`adjustment.go`:
tiers push/normal/reduced/easy/rest, in `-json` and the bot), tests for all of the above, repo on GitHub,
README, whole project in English (bot messages, docs, keywords), OAuth client secret rotated
(old one disabled; new one in GoLand - `.env` doesn't exist yet, create it from `.env.example` on deploy).

**Known issues**
- OAuth app is in Testing: Google tokens expire 7 days after approval (bot reminds; renew via `/tokens`).
- `configs/short_workouts.json` still has only 2 sample entries with empty `url` - user must fill it.
- Bot never run against real Telegram (no bot token yet); Docker image never built (Docker Desktop off).
- Readiness is our estimate - Fitbit's own score isn't in the API.
- Calendar workouts are recognized only by titles starting with `Workout` / `Run` or `[A]/[B]/[RUN]`
  (English only - Hebrew titles like `אימון` are no longer recognized).
- `.gitattributes` enforces LF only for tracked files; untracked or uncommitted files (e.g. a `.env` made
  on Windows) aren't covered - create `.env` on the server.

**Next tasks**
1. Create the Telegram bot, test locally, then deploy per `DEPLOY.md`.
