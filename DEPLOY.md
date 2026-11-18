# Deploying to a cloud server

The bot runs in a single Docker container: the Python bot + the Go agent.
Every morning (`DAILY_TIME`) it tells you on Telegram whether you should train today, and `/move`
(or the button in the message) moves today's workout on your calendar.

## 1. On your computer - once: Google tokens

The server can't open a browser, so you create the tokens on your computer and copy them over.
From the project folder, with `CLIENT_ID` and `CLIENT_SECRET` set:

```powershell
go build -o fitness-agent.exe ./cmd/agent
.\fitness-agent.exe                          # creates token_health.json (if missing)
.\fitness-agent.exe -move-today -dry-run     # creates token_calendar.json - changes nothing on the calendar
```

## 2. ⚠️ Required: take the OAuth app out of Testing

As long as Google Cloud Console → **OAuth consent screen** → **Audience** shows the status **Testing**,
Google revokes the tokens **after 7 days** - and the bot on the server stops getting data.
Click **Publish app** (moves it to *In production*). For personal use, Google verification isn't needed;
the consent screen will show "Google hasn't verified this app" - continue via **Advanced**.
After publishing, create the tokens again (delete both token files and repeat step 1).

## 3. Server

Any Linux VPS works, for example:
- **Google Cloud** - an `e2-micro` machine in us-central1/us-west1/us-east1 (free under the Free Tier)
- **Hetzner** - CX22, about €4 a month

On the server (Ubuntu):

```bash
curl -fsSL https://get.docker.com | sh
sudo apt-get install -y git
```

## 4. Get the project and copy the tokens

On the server, clone the repo (`data/` is in `.gitignore`, so create it yourself):

```bash
git clone https://github.com/BenFed23/Fitness-Coach.git ~/fitness-agent
mkdir -p ~/fitness-agent/data
```

The repo enforces LF line endings for tracked files through `.gitattributes`.

Only the secrets come from your computer. Copy the tokens (PowerShell, in the project folder; replace
`user@server`):

```powershell
scp token_health.json token_calendar.json user@server:~/fitness-agent/data/
```

On the server - create `~/fitness-agent/.env` from `.env.example` and fill it in (Telegram token, your ID,
CLIENT_ID/SECRET, location). Create it here rather than copying one made on Windows, so it has no CRLF:

```bash
cp ~/fitness-agent/.env.example ~/fitness-agent/.env
nano ~/fitness-agent/.env
chmod 600 ~/fitness-agent/.env ~/fitness-agent/data/*.json
```

## 5. Start it

```bash
cd ~/fitness-agent
docker compose up -d --build
docker compose logs -f          # should show: Morning update at 07:00 for 1 chat(s)
```

Send the bot `/start` and `/status` - the source should be "Fitbit (Google Health)".

## Updating

```bash
cd ~/fitness-agent
git pull
docker compose up -d --build
```

The tokens and the bot's state are kept in `data/` and aren't deleted. `configs/short_workouts.json` is
tracked by git, so if you edited it on the server, `git pull` may refuse to overwrite it - commit the
library from your computer instead, or run `git stash` before pulling and `git stash pop` after.

## The short workout library

When you ask to move a workout and the week is full, the agent replaces today's workout (same time)
with a video from `configs/short_workouts.json`.
You fill this file in by hand; on the server, edit `~/fitness-agent/configs/short_workouts.json` and the
change takes effect immediately, without a rebuild.
The history (so a video isn't repeated within 7 days) is kept in `data/short_workout_history.json`.

## Troubleshooting

- **"No access to tracker data" in messages** - usually an expired token: renew it with `/tokens`, or
  repeat step 1 on your computer and copy the file to `data/`.
- **`/move` says there's no workout today** - on the primary calendar, only events whose title starts with
  `[A]`, `[B]`, `[RUN]`, `Workout` or `Run` count
  (or set `WORKOUT_CALENDAR_ID` to a separate workouts calendar).
- **Weather for a different city** - `AGENT_LATITUDE` / `AGENT_LONGITUDE` are missing from `.env`.
- **A file has Windows line endings (CRLF) on the server** - convert it with `dos2unix <file>`, or run
  `git add --renormalize .` locally, commit, and `git pull` on the server.
