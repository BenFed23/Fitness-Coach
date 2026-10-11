"""
The agent's service layer - every integration point lives here.

The bot (bot.py) only talks to the functions in this file and doesn't know where the data comes from:

    get_recovery_data()   <- the Go agent (Fitbit via the Google Health API)
    get_weather()         <- the Go agent (Open-Meteo)
    decide_workout()      <- the agent's verdict + what the user reported in the chat
    parse_user_message()  <- free-text understanding (TODO: could be replaced by an LLM)

Talking to the agent: run fitness-agent.exe -json as a subprocess and read its JSON.
If it isn't available (not built, no token, no internet) the bot falls back to mock
data and says so in its messages.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import time
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta
from pathlib import Path

log = logging.getLogger("fitness-bot.services")

# The project folder (where token_health.json and the exe live) - one above telegram_bot
AGENT_DIR = Path(os.getenv("FITNESS_AGENT_DIR", Path(__file__).resolve().parent.parent))
AGENT_BIN = Path(os.getenv("FITNESS_AGENT_BIN", AGENT_DIR / "fitness-agent.exe"))
AGENT_TIMEOUT_SECONDS = 90
# How long to keep a result before running the agent again - tracker data doesn't change every minute
AGENT_CACHE_SECONDS = 10 * 60


# ---------------------------------------------------------------------------
# Data models
# ---------------------------------------------------------------------------

@dataclass
class RecoveryData:
    """Last night's recovery metrics. None = no reading."""
    readiness_score: int | None      # 1-100, an estimate in the style of Fitbit's Readiness
    sleep_hours: float | None
    resting_hr: int | None
    rhr_baseline: int | None
    hrv_ms: float | None
    source: str = "mock"             # google_health / recovery.json / mock
    readiness_breakdown: str = ""
    sleep_start: datetime | None = None
    sleep_end: datetime | None = None
    tracker_not_worn: bool = False
    respiratory_rate: float | None = None
    skin_temp_delta_c: float | None = None
    # The Go agent's verdict: go / adjust_for_weather / light /
    # skip_weather / skip_recovery. None = no agent (mock data).
    agent_verdict: str | None = None
    agent_has_recovery_data: bool = False
    warnings: list[str] = field(default_factory=list)


@dataclass
class Weather:
    temperature_c: float
    humidity_pct: float
    rain_mm: float
    wind_kmh: float
    source: str = "mock"


@dataclass
class UserContext:
    """
    What the user told the bot today. Resets automatically on a new day.
    These values override the tracker (e.g. sleep hours reported by hand).
    """
    day: date = field(default_factory=date.today)
    reported_sleep_hours: float | None = None
    injuries: list[str] = field(default_factory=list)   # e.g. ["knee"]
    feeling_exhausted: bool = False
    feeling_sick: bool = False
    notes: list[str] = field(default_factory=list)      # messages that weren't understood

    def reset_if_new_day(self) -> None:
        if self.day != date.today():
            self.__init__()  # start the new day clean


@dataclass
class UserUpdate:
    """What was extracted from one free-text message."""
    sleep_hours: float | None = None
    injury: str | None = None
    exhausted: bool = False
    sick: bool = False
    feeling_good: bool = False

    def is_empty(self) -> bool:
        return (self.sleep_hours is None and self.injury is None
                and not self.exhausted and not self.sick and not self.feeling_good)


@dataclass
class WorkoutPlan:
    level: str            # "full" / "light" / "rest"
    title: str
    details: list[str]
    reasons: list[str]    # why this is the decision - shown to the user


# ---------------------------------------------------------------------------
# Talking to the Go agent
# ---------------------------------------------------------------------------

# Latest results per exhausted flag: {exhausted: (time, snapshot)}
_snapshot_cache: dict[bool, tuple[float, dict]] = {}
# Prevents several parallel runs when a few messages arrive together
_agent_lock = asyncio.Lock()
# Why the agent last failed (e.g. an expired token) - so the bot can show it
last_agent_error: str | None = None


async def _run_agent(*flags: str) -> dict | None:
    """Runs the agent with -json and the extra flags and returns its JSON, or None on failure."""
    global last_agent_error
    if not AGENT_BIN.exists():
        last_agent_error = f"agent binary not found ({AGENT_BIN.name}) - it needs to be built"
        log.warning("Agent binary not found at %s - run: go build -o fitness-agent.exe ./cmd/agent", AGENT_BIN)
        return None
    try:
        proc = await asyncio.create_subprocess_exec(
            str(AGENT_BIN), "-json", *flags, cwd=AGENT_DIR,
            stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
        )
        stdout, stderr = await asyncio.wait_for(proc.communicate(), AGENT_TIMEOUT_SECONDS)
    except (OSError, asyncio.TimeoutError) as err:
        last_agent_error = f"running the agent failed ({err!r})"
        log.error("Running the agent failed: %r", err)
        return None
    if proc.returncode != 0:
        details = stderr.decode(errors="replace").strip()
        last_agent_error = details.splitlines()[-1] if details else f"exit code {proc.returncode}"
        log.error("Agent exited with %s: %s", proc.returncode, details[-1000:])
        return None
    try:
        result = json.loads(stdout)
    except json.JSONDecodeError as err:
        last_agent_error = "the agent returned invalid output"
        log.error("Agent returned invalid JSON: %s", err)
        return None
    last_agent_error = None
    return result


async def get_agent_snapshot(ctx: UserContext | None = None) -> dict | None:
    """The agent's result, with a short cache. Passes on fatigue reported in the chat."""
    exhausted = bool(ctx and ctx.feeling_exhausted)
    async with _agent_lock:
        cached = _snapshot_cache.get(exhausted)
        if cached and time.monotonic() - cached[0] < AGENT_CACHE_SECONDS:
            return cached[1]
        snapshot = await _run_agent(*(["-exhausted"] if exhausted else []))
        if snapshot is not None:
            _mark_token("health", bool(snapshot["recovery"].get("auth_error")))
            _snapshot_cache[exhausted] = (time.monotonic(), snapshot)
        return snapshot


def clear_agent_cache() -> None:
    """E.g. after the user synced the tracker and wants fresh data."""
    _snapshot_cache.clear()


# ---------------------------------------------------------------------------
# Google token expiry
# ---------------------------------------------------------------------------

# While the OAuth app is in Testing, Google revokes every token 7 days after it
# was approved in the browser. The agent stores the approval time in the token
# file (authorized_at). 0 = don't compute expiry (e.g. after moving to In production).
GOOGLE_TOKEN_MAX_AGE_DAYS = int(os.getenv("GOOGLE_TOKEN_MAX_AGE_DAYS", "7"))
TOKEN_WARN_BEFORE = timedelta(hours=24)


@dataclass
class TokenStatus:
    key: str             # health / calendar
    label: str           # for display
    filename: str
    renew_command: str   # the command that creates it again (on a computer, with a browser)
    state: str           # ok / expiring / expired / missing / unknown
    authorized_at: datetime | None = None
    expires_at: datetime | None = None


_TOKENS = [
    ("health", "tracker data (Google Health)", "token_health.json", r".\fitness-agent.exe"),
    ("calendar", "Google Calendar", "token_calendar.json", r".\fitness-agent.exe -move-today -dry-run"),
]
# Tokens Google rejected according to the agent (auth_error) - override the date-based estimate
_rejected_tokens: set[str] = set()


def _mark_token(key: str, rejected: bool) -> None:
    if rejected:
        _rejected_tokens.add(key)
    else:
        _rejected_tokens.discard(key)


def token_label(key: str) -> str:
    return next((label for k, label, _, _ in _TOKENS if k == key), key)


async def start_token_renewal(key: str) -> dict:
    """
    Step 1 of renewing from the phone: the agent returns a Google approval link ({"ok", "url"}).
    After approval the browser lands on a localhost address that doesn't load - the user
    pastes that address, and finish_token_renewal completes the renewal.
    """
    result = await _run_agent("-auth", key)
    if result is None:
        return {"ok": False, "error": last_agent_error or "the agent isn't available"}
    return result


async def finish_token_renewal(key: str, pasted_address: str) -> dict:
    """Step 2: passes the pasted address to the agent, which checks it and saves a new token."""
    result = await _run_agent("-auth", key, "-auth-code", pasted_address.strip())
    if result is None:
        return {"ok": False, "error": last_agent_error or "the agent isn't available"}
    if result.get("ok"):
        _mark_token(key, False)
        clear_agent_cache()  # the next data already uses the new token
    return result


def token_statuses(now: datetime | None = None) -> list[TokenStatus]:
    """The state of each token, from its file (authorized_at) and from what the agent reported."""
    now = now or datetime.now().astimezone()
    statuses = []
    for key, label, filename, renew in _TOKENS:
        status = TokenStatus(key, label, filename, renew, state="unknown")
        path = AGENT_DIR / filename
        if not path.exists():
            status.state = "missing"
            statuses.append(status)
            continue
        try:
            raw = json.loads(path.read_text(encoding="utf-8")).get("authorized_at")
            status.authorized_at = datetime.fromisoformat(raw.replace("Z", "+00:00")) if raw else None
        except (OSError, ValueError, AttributeError):
            status.authorized_at = None
        if status.authorized_at and GOOGLE_TOKEN_MAX_AGE_DAYS > 0:
            status.expires_at = status.authorized_at + timedelta(days=GOOGLE_TOKEN_MAX_AGE_DAYS)

        if key in _rejected_tokens or (status.expires_at and now >= status.expires_at):
            status.state = "expired"
        elif status.expires_at and now >= status.expires_at - TOKEN_WARN_BEFORE:
            status.state = "expiring"
        elif status.authorized_at or GOOGLE_TOKEN_MAX_AGE_DAYS == 0:
            status.state = "ok"
        statuses.append(status)
    return statuses


async def move_todays_workout(dry_run: bool) -> dict:
    """
    Moves today's workout on Google Calendar (regardless of the recommendation).
    dry_run=True - only returns what would change, without touching the calendar.
    Returns the agent's JSON: ok, error, workout, changes[{summary, from, to, new_summary}].
    """
    flags = ["-move-today"] + (["-dry-run"] if dry_run else [])
    result = await _run_agent(*flags)
    if result is None:
        return {"ok": False, "error": last_agent_error or "the agent isn't available", "changes": []}
    _mark_token("calendar", bool(result.get("auth_error")))
    return result


def _parse_time(value: str | None) -> datetime | None:
    return datetime.fromisoformat(value) if value else None


# ---------------------------------------------------------------------------
# Data integration points
# ---------------------------------------------------------------------------

async def get_recovery_data(ctx: UserContext | None = None) -> RecoveryData:
    """Tracker metrics from the agent. If it isn't available - mock data (source="mock")."""
    snapshot = await get_agent_snapshot(ctx)
    if snapshot is None:
        return RecoveryData(readiness_score=72, sleep_hours=6.8, resting_hr=47, rhr_baseline=45, hrv_ms=81.0)

    r, verdict = snapshot["recovery"], snapshot["verdict"]
    return RecoveryData(
        readiness_score=r["readiness_score"],
        sleep_hours=r["sleep_hours"],
        resting_hr=r["resting_hr"],
        rhr_baseline=r["rhr_baseline"],
        hrv_ms=r["hrv_ms"],
        source=r["source"],
        readiness_breakdown=r.get("readiness_breakdown", ""),
        sleep_start=_parse_time(r["sleep_start"]),
        sleep_end=_parse_time(r["sleep_end"]),
        tracker_not_worn=r["tracker_not_worn"],
        respiratory_rate=r["respiratory_rate"],
        skin_temp_delta_c=r["skin_temp_delta_c"],
        agent_verdict=verdict["kind"],
        agent_has_recovery_data=verdict["recovery_data_available"],
        warnings=snapshot["warnings"],
    )


async def get_weather(ctx: UserContext | None = None) -> Weather:
    """The weather from the agent (Open-Meteo for the location). If it isn't available - mock data."""
    snapshot = await get_agent_snapshot(ctx)
    if snapshot is None:
        return Weather(temperature_c=26.5, humidity_pct=60, rain_mm=0, wind_kmh=10)
    w = snapshot["weather"]
    return Weather(w["temperature_c"], w["humidity_pct"], w["rain_mm"], w["wind_kmh"], source="open-meteo")


# ---------------------------------------------------------------------------
# Understanding free-text messages
# ---------------------------------------------------------------------------

# Body parts recognized in messages about pain/injury (word in the message -> display name)
_BODY_PARTS = {
    "knee": "knee", "knees": "knee",
    "back": "back",
    "shoulder": "shoulder", "shoulders": "shoulder",
    "ankle": "ankle", "ankles": "ankle",
    "elbow": "elbow",
    "wrist": "wrist",
    "neck": "neck",
    "hip": "hip", "hips": "hip",
}
NO_BODY_PART = "unspecified"
_PAIN_WORDS = ("pain", "hurt", "injur", "sore", "sprain", "ache")
_EXHAUSTED_WORDS = ("tired", "exhausted", "wiped out", "worn out", "drained")
_SICK_WORDS = ("sick", "fever", "flu", "have a cold", "caught a cold")
_GOOD_WORDS = ("feel great", "feel good", "feeling great", "feeling good")

_SLEEP_RE = re.compile(
    r"(?:slept|sleep|asleep)\D{0,15}?(\d+(?:[.,]\d+)?)\s*(?:h|hours?|hrs?)?",
    re.IGNORECASE,
)


def _contains_word(text: str, word: str) -> bool:
    """Looks for a whole word, so "back" doesn't match "backpack"."""
    return re.search(rf"\b{re.escape(word)}\b", text) is not None


async def parse_user_message(text: str) -> UserUpdate:
    """
    Basic keyword-based understanding.

    TODO: replace with a language model (LLM) for better results - ask it to return
    JSON in the same shape as UserUpdate. The function signature stays the same.
    """
    lowered = text.lower()
    update = UserUpdate()

    match = _SLEEP_RE.search(lowered)
    if match:
        hours = float(match.group(1).replace(",", "."))
        if 0 <= hours <= 16:
            update.sleep_hours = hours

    if any(word in lowered for word in _PAIN_WORDS):
        update.injury = next(
            (name for word, name in _BODY_PARTS.items() if _contains_word(lowered, word)),
            NO_BODY_PART,
        )

    update.exhausted = any(word in lowered for word in _EXHAUSTED_WORDS)
    update.sick = any(word in lowered for word in _SICK_WORDS)
    update.feeling_good = any(phrase in lowered for phrase in _GOOD_WORDS)
    return update


def apply_update(ctx: UserContext, update: UserUpdate, original_text: str) -> None:
    """Updates the conversation context with what was recognized in the message."""
    if update.sleep_hours is not None:
        ctx.reported_sleep_hours = update.sleep_hours
    if update.injury and update.injury not in ctx.injuries:
        ctx.injuries.append(update.injury)
    if update.exhausted:
        ctx.feeling_exhausted = True
    if update.sick:
        ctx.feeling_sick = True
    if update.feeling_good:
        ctx.feeling_exhausted = False
    if update.is_empty():
        ctx.notes.append(original_text)


# ---------------------------------------------------------------------------
# Deciding on the workout
# ---------------------------------------------------------------------------

async def decide_workout(recovery: RecoveryData, weather: Weather, ctx: UserContext) -> WorkoutPlan:
    """
    The workout decision:
      1. The base is the Go agent's verdict (readiness, HRV, sleep, heart rate, weather).
         Without the agent (mock data) - simple rules with the same thresholds.
      2. On top of that - what the user reported in the chat (sleep, illness, fatigue, pain).
    The decision can only get stricter: the most restrictive reason wins.
    """
    reasons: list[str] = []
    level = "full"

    def lower_to(new_level: str, reason: str) -> None:
        nonlocal level
        order = {"full": 0, "light": 1, "rest": 2}
        if order[new_level] > order[level]:
            level = new_level
        reasons.append(reason)

    outdoor_ok = weather.rain_mm < 2 and 5 <= weather.temperature_c <= 30 and weather.wind_kmh < 30

    if recovery.agent_verdict is not None:
        # --- 1. The agent's verdict ---
        readiness = ""
        if recovery.readiness_score is not None:
            readiness = f" Readiness score {recovery.readiness_score}"
            if recovery.readiness_breakdown:
                readiness += f" ({recovery.readiness_breakdown})"
            readiness += "."
        # Without tracker data, the agent's "light" comes only from reported fatigue - handled in step 2
        match recovery.agent_verdict:
            case "skip_recovery" if recovery.agent_has_recovery_data:
                lower_to("rest", "Your tracker's recovery metrics are in the red." + readiness)
            case "light" if recovery.agent_has_recovery_data:
                lower_to("light", "Your tracker says you haven't fully recovered." + readiness)
            case "skip_weather":
                outdoor_ok = False
            case "adjust_for_weather":
                reasons.append("The weather isn't ideal - adjust pace, clothing and hydration.")
        if recovery.tracker_not_worn:
            reasons.append("The tracker wasn't worn overnight - no recovery data, so this ignores sleep and heart rate.")
        elif recovery.agent_verdict in ("go", "adjust_for_weather", "skip_weather") and recovery.agent_has_recovery_data:
            reasons.append("Your tracker's recovery metrics look fine." + readiness)
        device_sleep = None  # the tracker's sleep is already part of the agent's verdict
    else:
        # --- 1. No agent: simple rules on the mock data ---
        if recovery.readiness_score is not None:
            if recovery.readiness_score < 30:
                lower_to("rest", f"Low readiness score ({recovery.readiness_score}).")
            elif recovery.readiness_score < 65:
                lower_to("light", f"Moderate readiness score ({recovery.readiness_score}).")
        if recovery.resting_hr and recovery.rhr_baseline and recovery.resting_hr - recovery.rhr_baseline >= 10:
            lower_to("rest", f"Resting heart rate is {recovery.resting_hr - recovery.rhr_baseline} above your baseline.")
        device_sleep = recovery.sleep_hours

    # --- 2. What the user reported (overrides the tracker) ---
    sleep = ctx.reported_sleep_hours if ctx.reported_sleep_hours is not None else device_sleep
    if sleep is not None:
        if sleep < 4:
            lower_to("rest", f"You only slept {sleep:g} hours - high injury risk.")
        elif sleep < 6:
            lower_to("light", f"You slept {sleep:g} hours - recovery is partial.")
    if ctx.feeling_sick:
        lower_to("rest", "You said you're sick - your body needs rest.")
    if ctx.feeling_exhausted:
        lower_to("light", "You reported feeling tired.")
    for injury in ctx.injuries:
        lower_to("light", f"Pain in your {injury} - avoid loading that area.")

    if not outdoor_ok:
        reasons.append("The weather isn't right for training outside - better indoors.")

    if level == "rest":
        return WorkoutPlan("rest", "Rest day 🛌",
                           ["Easy 20-30 minute walk (optional)", "10 minutes of stretching and breathing",
                            "Hydrate and get to bed early"],
                           reasons)
    if level == "light":
        details = ["10-minute warm-up", "Easy 25-30 minute session at a low heart rate (zone 2)",
                   "10 minutes of stretching"]
        if ctx.injuries:
            details.insert(1, "Skip exercises that load: " + ", ".join(ctx.injuries))
        return WorkoutPlan("light", "Light workout 🚶", details, reasons)

    where = "outside" if outdoor_ok else "at the gym / at home"
    return WorkoutPlan("full", f"Full workout {where} 💪",
                       ["10-minute warm-up", "40-45 minute main session as planned", "10-minute cool-down and stretching"],
                       reasons or ["Recovery and weather both look good." if recovery.agent_verdict is None
                                   else "The weather is fine."])
