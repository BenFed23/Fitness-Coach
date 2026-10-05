"""
שכבת השירותים של האג'נט - כל נקודות החיבור נמצאות כאן.

הבוט (bot.py) מדבר רק עם הפונקציות בקובץ הזה, ולא יודע מאיפה מגיעים הנתונים:

    get_recovery_data()   <- האג'נט ב-Go (Fitbit דרך Google Health API)
    get_weather()         <- האג'נט ב-Go (Open-Meteo)
    decide_workout()      <- ההחלטה של האג'נט + מה שהמשתמש דיווח בצ'אט
    parse_user_message()  <- הבנת שפה חופשית (TODO: אפשר להחליף ב-LLM)

החיבור לאג'נט: מריצים את fitness-agent.exe -json כתהליך וקוראים את ה-JSON.
אם הוא לא זמין (לא נבנה, אין טוקן, אין אינטרנט) - הבוט עובר לנתוני דמה
ומציין את זה בהודעות.
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

# תיקיית הפרויקט (שם נמצאים token_health.json וה-exe) - אחת מעל telegram_bot
AGENT_DIR = Path(os.getenv("FITNESS_AGENT_DIR", Path(__file__).resolve().parent.parent))
AGENT_BIN = Path(os.getenv("FITNESS_AGENT_BIN", AGENT_DIR / "fitness-agent.exe"))
AGENT_TIMEOUT_SECONDS = 90
# כמה זמן לשמור תוצאה לפני הרצה חוזרת - נתוני הצמיד לא משתנים כל דקה
AGENT_CACHE_SECONDS = 10 * 60


# ---------------------------------------------------------------------------
# מודלי נתונים
# ---------------------------------------------------------------------------

@dataclass
class RecoveryData:
    """מדדי ההתאוששות של הלילה האחרון. None = אין נתון."""
    readiness_score: int | None      # 1-100, הערכה בסגנון ה-Readiness של Fitbit
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
    # ההחלטה של האג'נט ב-Go: go / adjust_for_weather / light /
    # skip_weather / skip_recovery. None = אין אג'נט (נתוני דמה).
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
    מה שהמשתמש סיפר לבוט היום. מתאפס אוטומטית ביום חדש.
    הערכים כאן גוברים על נתוני הצמיד (למשל שעות שינה שדווחו ידנית).
    """
    day: date = field(default_factory=date.today)
    reported_sleep_hours: float | None = None
    injuries: list[str] = field(default_factory=list)   # למשל ["ברך"]
    feeling_exhausted: bool = False
    feeling_sick: bool = False
    notes: list[str] = field(default_factory=list)      # הודעות שלא זוהו

    def reset_if_new_day(self) -> None:
        if self.day != date.today():
            self.__init__()  # מתחילים יום חדש נקי


@dataclass
class UserUpdate:
    """מה שחולץ מהודעה חופשית אחת."""
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
    reasons: list[str]    # למה זו ההחלטה - מוצג למשתמש


# ---------------------------------------------------------------------------
# החיבור לאג'נט ב-Go
# ---------------------------------------------------------------------------

# תוצאות אחרונות לפי הדגל exhausted: {exhausted: (time, snapshot)}
_snapshot_cache: dict[bool, tuple[float, dict]] = {}
# מונע כמה הרצות במקביל כשכמה הודעות מגיעות יחד
_agent_lock = asyncio.Lock()
# הסיבה לכישלון האחרון של האג'נט (למשל טוקן שפג) - כדי שהבוט יוכל להציג אותה
last_agent_error: str | None = None


async def _run_agent(*flags: str) -> dict | None:
    """מריץ את האג'נט עם -json והדגלים הנוספים ומחזיר את ה-JSON, או None אם נכשל."""
    global last_agent_error
    if not AGENT_BIN.exists():
        last_agent_error = f"קובץ האג'נט לא נמצא ({AGENT_BIN.name}) - צריך לבנות אותו"
        log.warning("Agent binary not found at %s - run: go build -o fitness-agent.exe ./cmd/agent", AGENT_BIN)
        return None
    try:
        proc = await asyncio.create_subprocess_exec(
            str(AGENT_BIN), "-json", *flags, cwd=AGENT_DIR,
            stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
        )
        stdout, stderr = await asyncio.wait_for(proc.communicate(), AGENT_TIMEOUT_SECONDS)
    except (OSError, asyncio.TimeoutError) as err:
        last_agent_error = f"הרצת האג'נט נכשלה ({err!r})"
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
        last_agent_error = "האג'נט החזיר פלט לא תקין"
        log.error("Agent returned invalid JSON: %s", err)
        return None
    last_agent_error = None
    return result


async def get_agent_snapshot(ctx: UserContext | None = None) -> dict | None:
    """תוצאת האג'נט, עם מטמון קצר. מעביר לו את העייפות שדווחה בצ'אט."""
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
    """למשל אחרי שהמשתמש סנכרן את הצמיד ורוצה נתונים טריים."""
    _snapshot_cache.clear()


# ---------------------------------------------------------------------------
# תוקף הטוקנים של גוגל
# ---------------------------------------------------------------------------

# כל עוד אפליקציית ה-OAuth במצב Testing, גוגל מבטלת כל טוקן 7 ימים אחרי האישור
# בדפדפן. האג'נט שומר את זמן האישור בקובץ הטוקן (authorized_at).
# 0 = לא לחשב תפוגה (למשל אחרי מעבר ל-In production).
GOOGLE_TOKEN_MAX_AGE_DAYS = int(os.getenv("GOOGLE_TOKEN_MAX_AGE_DAYS", "7"))
TOKEN_WARN_BEFORE = timedelta(hours=24)


@dataclass
class TokenStatus:
    key: str             # health / calendar
    label: str           # לתצוגה
    filename: str
    renew_command: str   # הפקודה שיוצרת אותו מחדש (במחשב, עם דפדפן)
    state: str           # ok / expiring / expired / missing / unknown
    authorized_at: datetime | None = None
    expires_at: datetime | None = None


_TOKENS = [
    ("health", "נתוני הצמיד (Google Health)", "token_health.json", r".\fitness-agent.exe"),
    ("calendar", "יומן גוגל", "token_calendar.json", r".\fitness-agent.exe -move-today -dry-run"),
]
# טוקנים שגוגל דחתה לפי הדיווח של האג'נט (auth_error) - גוברים על החישוב לפי תאריך
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
    שלב 1 של חידוש מהטלפון: האג'נט מחזיר קישור לאישור בגוגל ({"ok", "url"}).
    אחרי האישור הדפדפן עובר לכתובת localhost שלא נטענת - את הכתובת הזו
    המשתמש מדביק, ו-finish_token_renewal משלים.
    """
    result = await _run_agent("-auth", key)
    if result is None:
        return {"ok": False, "error": last_agent_error or "האג'נט לא זמין"}
    return result


async def finish_token_renewal(key: str, pasted_address: str) -> dict:
    """שלב 2: מעביר לאג'נט את הכתובת שהודבקה; הוא בודק אותה ושומר טוקן חדש."""
    result = await _run_agent("-auth", key, "-auth-code", pasted_address.strip())
    if result is None:
        return {"ok": False, "error": last_agent_error or "האג'נט לא זמין"}
    if result.get("ok"):
        _mark_token(key, False)
        clear_agent_cache()  # הנתונים הבאים כבר עם הטוקן החדש
    return result


def token_statuses(now: datetime | None = None) -> list[TokenStatus]:
    """מצב כל טוקן לפי הקובץ שלו (authorized_at) ולפי מה שהאג'נט דיווח."""
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
    מזיז את האימון של היום ביומן גוגל (בלי קשר להמלצה).
    dry_run=True - רק מחזיר מה ישתנה, בלי לגעת ביומן.
    מחזיר את ה-JSON של האג'נט: ok, error, workout, changes[{summary, from, to, new_summary}].
    """
    flags = ["-move-today"] + (["-dry-run"] if dry_run else [])
    result = await _run_agent(*flags)
    if result is None:
        return {"ok": False, "error": last_agent_error or "האג'נט לא זמין", "changes": []}
    _mark_token("calendar", bool(result.get("auth_error")))
    return result


def _parse_time(value: str | None) -> datetime | None:
    return datetime.fromisoformat(value) if value else None


# ---------------------------------------------------------------------------
# נקודות חיבור לנתונים
# ---------------------------------------------------------------------------

async def get_recovery_data(ctx: UserContext | None = None) -> RecoveryData:
    """מדדי הצמיד מהאג'נט. אם הוא לא זמין - נתוני דמה (source="mock")."""
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
    """מזג האוויר מהאג'נט (Open-Meteo לפי המיקום). אם הוא לא זמין - נתוני דמה."""
    snapshot = await get_agent_snapshot(ctx)
    if snapshot is None:
        return Weather(temperature_c=26.5, humidity_pct=60, rain_mm=0, wind_kmh=10)
    w = snapshot["weather"]
    return Weather(w["temperature_c"], w["humidity_pct"], w["rain_mm"], w["wind_kmh"], source="open-meteo")


# ---------------------------------------------------------------------------
# הבנת הודעות חופשיות
# ---------------------------------------------------------------------------

# איברים שמזוהים בהודעות על כאב/פציעה (מילה בהודעה -> שם לתצוגה)
_BODY_PARTS = {
    "ברך": "ברך", "ברכיים": "ברך", "knee": "ברך",
    "גב": "גב", "back": "גב",
    "כתף": "כתף", "shoulder": "כתף",
    "קרסול": "קרסול", "ankle": "קרסול",
    "מרפק": "מרפק", "elbow": "מרפק",
    "שורש כף היד": "שורש כף היד", "wrist": "שורש כף היד",
    "צוואר": "צוואר", "neck": "צוואר",
    "ירך": "ירך", "hip": "ירך",
}
_PAIN_WORDS = ("כאב", "כואב", "כואבת", "נפצעתי", "פציעה", "נקע", "pain", "hurt", "injur")
_EXHAUSTED_WORDS = ("עייף", "עייפה", "מותש", "מותשת", "גמור", "גמורה", "tired", "exhausted")
_SICK_WORDS = ("חולה", "חום", "מצונן", "מצוננת", "שפעת", "sick", "fever", "flu")
_GOOD_WORDS = ("מרגיש מצוין", "מרגישה מצוין", "מרגיש טוב", "מרגישה טוב", "feel great", "feel good")

_SLEEP_RE = re.compile(
    r"(?:ישנתי|שינה|נרדמתי|slept|sleep)\D{0,15}?(\d+(?:[.,]\d+)?)\s*(?:שעות|שעה|h|hours?)?",
    re.IGNORECASE,
)


def _contains_word(text: str, word: str) -> bool:
    """
    מחפש מילה שלמה, כולל אות שימוש אחת לפניה ("בברך", "הגב").
    כך "גב" לא יתאים בטעות ל"גבוה".
    """
    pattern = rf"(?<![א-תa-z])[בהלמו]?{re.escape(word)}(?![א-תa-z])"
    return re.search(pattern, text) is not None


async def parse_user_message(text: str) -> UserUpdate:
    """
    הבנה בסיסית מבוססת מילות מפתח (עברית ואנגלית).

    TODO: להחליף במודל שפה (LLM) לתוצאות טובות יותר - לבקש ממנו להחזיר
    JSON באותו מבנה של UserUpdate. חתימת הפונקציה נשארת זהה.
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
            "לא צוין",
        )

    update.exhausted = any(word in lowered for word in _EXHAUSTED_WORDS)
    update.sick = any(word in lowered for word in _SICK_WORDS)
    update.feeling_good = any(phrase in lowered for phrase in _GOOD_WORDS)
    return update


def apply_update(ctx: UserContext, update: UserUpdate, original_text: str) -> None:
    """מעדכן את הקשר השיחה לפי מה שזוהה בהודעה."""
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
# החלטה על האימון
# ---------------------------------------------------------------------------

async def decide_workout(recovery: RecoveryData, weather: Weather, ctx: UserContext) -> WorkoutPlan:
    """
    ההחלטה על האימון:
      1. הבסיס הוא ההחלטה של האג'נט ב-Go (Readiness, HRV, שינה, דופק, מזג אוויר).
         בלי אג'נט (נתוני דמה) - כללים פשוטים עם אותם ספים.
      2. מעל זה - מה שהמשתמש דיווח בצ'אט (שינה, מחלה, עייפות, כאבים).
    ההחלטה יכולה רק להחמיר: הכי מגביל מבין כל הסיבות קובע.
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
        # --- 1. ההחלטה של האג'נט ---
        readiness = ""
        if recovery.readiness_score is not None:
            readiness = f" ציון התאוששות {recovery.readiness_score}"
            if recovery.readiness_breakdown:
                readiness += f" ({recovery.readiness_breakdown})"
            readiness += "."
        # בלי נתוני צמיד, "light" של האג'נט נובע רק מהעייפות שדווחה - והיא מטופלת בסעיף 2
        match recovery.agent_verdict:
            case "skip_recovery" if recovery.agent_has_recovery_data:
                lower_to("rest", "מדדי ההתאוששות מהצמיד באדום." + readiness)
            case "light" if recovery.agent_has_recovery_data:
                lower_to("light", "ההתאוששות לפי הצמיד לא מלאה." + readiness)
            case "skip_weather":
                outdoor_ok = False
            case "adjust_for_weather":
                reasons.append("מזג האוויר לא אידיאלי - להתאים קצב, ביגוד ושתייה.")
        if recovery.tracker_not_worn:
            reasons.append("הצמיד לא נענד בלילה - אין נתוני התאוששות, ההחלטה בלי מדדי שינה/דופק.")
        elif recovery.agent_verdict in ("go", "adjust_for_weather", "skip_weather") and recovery.agent_has_recovery_data:
            reasons.append("מדדי ההתאוששות מהצמיד תקינים." + readiness)
        device_sleep = None  # השינה מהצמיד כבר נכללה בהחלטת האג'נט
    else:
        # --- 1. בלי אג'נט: כללים פשוטים על נתוני הדמה ---
        if recovery.readiness_score is not None:
            if recovery.readiness_score < 30:
                lower_to("rest", f"ציון התאוששות נמוך ({recovery.readiness_score}).")
            elif recovery.readiness_score < 65:
                lower_to("light", f"ציון התאוששות בינוני ({recovery.readiness_score}).")
        if recovery.resting_hr and recovery.rhr_baseline and recovery.resting_hr - recovery.rhr_baseline >= 10:
            lower_to("rest", f"דופק המנוחה גבוה ב-{recovery.resting_hr - recovery.rhr_baseline} מהבסיס שלך.")
        device_sleep = recovery.sleep_hours

    # --- 2. מה שהמשתמש דיווח (גובר על הצמיד) ---
    sleep = ctx.reported_sleep_hours if ctx.reported_sleep_hours is not None else device_sleep
    if sleep is not None:
        if sleep < 4:
            lower_to("rest", f"ישנת רק {sleep:g} שעות - סיכון גבוה לפציעה.")
        elif sleep < 6:
            lower_to("light", f"ישנת {sleep:g} שעות - ההתאוששות חלקית.")
    if ctx.feeling_sick:
        lower_to("rest", "דיווחת שאתה חולה - הגוף צריך מנוחה.")
    if ctx.feeling_exhausted:
        lower_to("light", "דיווחת על עייפות.")
    for injury in ctx.injuries:
        lower_to("light", f"כאב ב{injury} - להימנע מעומס על האזור.")

    if not outdoor_ok:
        reasons.append("מזג האוויר לא מתאים לאימון בחוץ - עדיף בפנים.")

    if level == "rest":
        return WorkoutPlan("rest", "יום מנוחה 🛌",
                           ["הליכה קלה של 20-30 דקות (לא חובה)", "מתיחות ונשימות 10 דקות", "שתייה ושינה מוקדמת"],
                           reasons)
    if level == "light":
        details = ["חימום 10 דקות", "אימון קל 25-30 דקות בדופק נמוך (אזור 2)", "מתיחות 10 דקות"]
        if ctx.injuries:
            details.insert(1, "לדלג על תרגילים שמעמיסים על: " + ", ".join(ctx.injuries))
        return WorkoutPlan("light", "אימון קל 🚶", details, reasons)

    where = "בחוץ" if outdoor_ok else "בחדר כושר / בבית"
    return WorkoutPlan("full", f"אימון מלא {where} 💪",
                       ["חימום 10 דקות", "אימון עיקרי 40-45 דקות לפי התוכנית", "שחרור ומתיחות 10 דקות"],
                       reasons or ["ההתאוששות ומזג האוויר תקינים." if recovery.agent_verdict is None
                                   else "מזג האוויר תקין."])
