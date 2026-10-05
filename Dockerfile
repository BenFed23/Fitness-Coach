# Fitness Agent: the Telegram bot (Python) plus the agent it runs (Go).
#
#   docker compose up -d --build
#
# Tokens, the bot's state and anything the agent writes live in /data, which
# docker-compose.yml maps to ./data on the server.

# --- 1. Build the Go agent as a static Linux binary ---
FROM golang:1.26 AS agent
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /out/fitness-agent ./cmd/agent

# --- 2. The bot ---
FROM python:3.13-slim
WORKDIR /app
COPY telegram_bot/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY telegram_bot/bot.py telegram_bot/services.py ./
COPY --from=agent /out/fitness-agent /app/fitness-agent
# The short workout library. docker-compose.yml also mounts ./configs over
# it, so edits on the server apply without a rebuild.
COPY configs/short_workouts.json /app/configs/short_workouts.json

ENV FITNESS_AGENT_BIN=/app/fitness-agent \
    SHORT_WORKOUTS_FILE=/app/configs/short_workouts.json \
    FITNESS_AGENT_DIR=/data \
    BOT_STATE_FILE=/data/bot_state.pickle \
    TZ=Asia/Jerusalem \
    PYTHONUNBUFFERED=1
VOLUME /data
CMD ["python", "bot.py"]
