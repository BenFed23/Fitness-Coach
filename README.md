# Fitness Coach

A personal training assistant that decides each morning whether you should train today -
**go**, **light** or **rest** - from your Fitbit recovery data and the weather, tells you on
Telegram, and moves today's workout on Google Calendar when you ask it to.

## How it works

- **Recovery** - last night's sleep, resting heart rate (against your own 30-day baseline), HRV,
  breathing rate and skin temperature, read from Fitbit through the
  [Google Health API](https://developers.google.com/health). Fitbit's own Readiness Score isn't
  available in the API, so the agent estimates one from the same inputs. If the tracker wasn't
  worn overnight, it says so instead of guessing.
- **Weather** - current temperature, humidity, rain and wind from [Open-Meteo](https://open-meteo.com)
  (no API key needed).
- **Verdict** - every reading is scored GOOD / CAUTION / HAZARDOUS and the most restrictive one wins:
  bad recovery means rest, bad weather means move indoors.
- **Calendar** - `/move` shifts today's workout to the next free slot this week, pushing later
  workouts along if needed. If the week is full, it replaces today's workout with a short
  YouTube session from `configs/short_workouts.json`. You always see a preview before anything changes.

## Architecture

```
telegram_bot/     Python bot (python-telegram-bot). Morning message, commands, free-text updates.
      │  runs as a subprocess, reads JSON
      ▼
cmd/agent/        Go agent. Google Health, Open-Meteo, Calendar, OAuth. Every operation has -json output.
configs/          short_workouts.json - the hand-maintained short-workout library.
```

`internal/` holds older packages that are not used by `cmd/agent`.

## Requirements

- Go 1.26+
- Python 3.11+
- A Fitbit (or Pixel Watch) synced to a Google account
- A Google Cloud project and a Telegram bot (see below)

## Setup

### 1. Google Cloud

1. Create a project in the [Google Cloud Console](https://console.cloud.google.com).
2. Enable the **Google Health API** and the **Google Calendar API**.
3. Configure the **OAuth consent screen** and add your Google account as a test user.
4. Create an **OAuth client ID** of type **Desktop app**. Its client ID and secret go into
   `CLIENT_ID` and `CLIENT_SECRET`.

While the app is in **Testing**, Google revokes tokens 7 days after you approve them. The bot
reminds you a day before and lets you renew from your phone (`/tokens`). To avoid this entirely,
publish the app (**Publish app** → *In production*); for personal use no verification is needed.

### 2. Telegram

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
2. Get your user ID from [@userinfobot](https://t.me/userinfobot). Only the IDs you list can use the bot.

### 3. Configuration

Copy `.env.example` to `.env` and fill it in:

| Variable | Description |
|---|---|
| `TELEGRAM_BOT_TOKEN` | Token from @BotFather |
| `TELEGRAM_ALLOWED_USER_IDS` | Comma-separated Telegram user IDs allowed to use the bot |
| `CLIENT_ID`, `CLIENT_SECRET` | Google OAuth client (Desktop app) |
| `AGENT_LATITUDE`, `AGENT_LONGITUDE`, `AGENT_CITY` | Location for the weather. Required on a server, otherwise the data center's location is used |
| `DAILY_TIME` | Time of the morning message (Israel time), default `07:00` |
| `GOOGLE_TOKEN_MAX_AGE_DAYS` | Token lifetime to remind about: `7` in Testing, `0` to disable |
| `WORKOUT_CALENDAR_ID` | Optional dedicated workouts calendar (see below) |

Never commit `.env` or the `token_*.json` files - they are in `.gitignore`.

### 4. Authorize Google

The first run opens a browser to approve access. Health and Calendar need separate approvals:

```powershell
go build -o fitness-agent.exe ./cmd/agent
.\fitness-agent.exe                         # creates token_health.json
.\fitness-agent.exe -move-today -dry-run    # creates token_calendar.json, changes nothing
```

### 5. Run the bot locally

```powershell
cd telegram_bot
python -m venv .venv
.venv\Scripts\activate
pip install -r requirements.txt
python bot.py
```

The bot expects `fitness-agent.exe` in the project root (override with `FITNESS_AGENT_BIN` /
`FITNESS_AGENT_DIR`) and reads the same environment variables as the agent.

### 6. Deploy

A `Dockerfile` and `docker-compose.yml` run the bot and the agent in one container on any Linux VPS.
Tokens and bot state live in `./data`. Step-by-step instructions are in [DEPLOY.md](DEPLOY.md).

## Calendar workouts

On the primary calendar, an event counts as a workout only if its title starts with:

- `[A]`, `[B]` or `[RUN]` - e.g. `[A] Legs, back, biceps`
- the word `Workout` or `Run` - e.g. `Workout chest`, `Run - 5k`. Muscle words after `Workout`
  (legs/back/biceps → A, chest/shoulders/triceps → B) pick the split; a bare `Workout` is a generic gym session.

Set `WORKOUT_CALENDAR_ID` to a dedicated calendar to treat every event in it as a workout.

## Short-workout library

`configs/short_workouts.json` is a list of fallback videos, filled in by hand:

```json
{
  "title": "20-minute full-body strength",
  "url": "https://www.youtube.com/watch?v=...",
  "duration_min": 20,
  "type": "strength",
  "muscle_group": "full",
  "intensity": "medium"
}
```

`muscle_group` is `A`, `B` or `full` (optional). The agent picks a video matching the missed
workout and avoids repeating one within 7 days.

## Bot commands

| Command | |
|---|---|
| `/start` | Intro and today's status |
| `/status` | Recovery metrics and weather |
| `/workout` | Today's recommendation |
| `/move` | Move today's workout on the calendar (with preview) |
| `/update` | Guided report: sleep, pain, fatigue, illness |
| `/refresh` | Fetch fresh data after the tracker syncs |
| `/daily` | Turn the morning message on/off |
| `/tokens` | Google access status and renewal |
| `/reset` | Clear what you reported today |

You can also just write, e.g. "I only slept 5 hours" or "my knee hurts", and the recommendation updates.
## Agent CLI

```
fitness-agent [flags]
  -json                print data and verdict as JSON (used by the bot)
  -exhausted           self-report fatigue (counts as a CAUTION signal)
  -reschedule          if the verdict is to skip, reschedule today's workout
  -move-today          move today's workout regardless of the verdict
  -dry-run             with -move-today: show the changes without applying them
  -auth health|calendar  re-authorize without a local browser; prints a link
  -auth-code URL       with -auth: the address Google redirected to
```

## Tests

```powershell
go test ./cmd/agent/...
```

## Disclaimer

The readiness score is an estimate, not Fitbit's. This is a personal project, not medical advice.
