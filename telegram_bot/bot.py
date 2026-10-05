"""
בוט טלגרם ל-Fitness Agent.

פקודות:
    /start    - פתיחת שיחה + סטטוס יומי
    /status   - מדדי Fitbit ומזג אוויר
    /workout  - תוכנית האימון להיום
    /update   - עדכון נתונים מודרך (שינה / כאב / עייפות)
    /move     - הזזת האימון של היום ביומן גוגל (עם תצוגה מקדימה ואישור)
    /daily    - כיבוי/הפעלה של עדכון הבוקר (פועל אוטומטית למשתמשים מורשים)
    /refresh  - נתונים טריים מהצמיד (אחרי סנכרון)
    /tokens   - מצב הגישה לגוגל ואיך לחדש (תזכורת נשלחת גם אוטומטית)
    /reset    - מחיקת מה שדיווחת היום
    /cancel   - יציאה מעדכון מודרך

כל הודעה חופשית ("ישנתי רק 4 שעות", "כואבת לי הברך") עוברת ל-parse_user_message
ומעדכנת את ההמלצה.

הנתונים מגיעים מהאג'נט ב-Go (ראו services.py). לפני ההרצה בונים אותו,
מתיקיית הפרויקט:  go build -o fitness-agent.exe ./cmd/agent
ומריצים אותו פעם אחת בלי -json כדי לאשר את הגישה לגוגל בדפדפן.

הרצה (מתוך התיקייה telegram_bot):
    python -m venv .venv
    .venv\\Scripts\\activate
    pip install -r requirements.txt
    set TELEGRAM_BOT_TOKEN=...              (מ-@BotFather)
    set TELEGRAM_ALLOWED_USER_IDS=123456    (ה-ID שלך, מ-@userinfobot)
    set CLIENT_ID=... / set CLIENT_SECRET=...  (כמו לאג'נט - הוא יורש אותם)
    python bot.py
"""

from __future__ import annotations

import datetime as dt
import html
import logging
import os
import warnings
from zoneinfo import ZoneInfo

from telegram import InlineKeyboardButton, InlineKeyboardMarkup, Update
from telegram.constants import ParseMode
from telegram.ext import (
    Application,
    CallbackQueryHandler,
    CommandHandler,
    ContextTypes,
    ConversationHandler,
    MessageHandler,
    PicklePersistence,
    filters,
)
from telegram.warnings import PTBUserWarning

# בשיחת /update יש גם כפתורים וגם הודעות טקסט, ולכן per_message=False הוא הנכון.
# PTB מזהיר על זה תמיד - ההתנהגות כאן מכוונת.
warnings.filterwarnings("ignore", message=r".*CallbackQueryHandler", category=PTBUserWarning)

import services
from services import UserContext, UserUpdate

logging.basicConfig(format="%(asctime)s %(levelname)s %(name)s: %(message)s", level=logging.INFO)
logging.getLogger("httpx").setLevel(logging.WARNING)  # בלי לוג על כל בקשת polling
log = logging.getLogger("fitness-bot")

TIMEZONE = ZoneInfo("Asia/Jerusalem")
# שעת עדכון הבוקר (DAILY_TIME=07:30 למשל). כדאי אחרי שהצמיד מסתנכרן עם הטלפון.
DAILY_TIME = dt.time.fromisoformat(os.getenv("DAILY_TIME", "07:00")).replace(tzinfo=TIMEZONE)

MORNING_HEADLINES = {
    "full": "✅ מותר להתאמן היום",
    "light": "⚠️ היום רק אימון קל",
    "rest": "⛔ היום לא מתאמנים - יום מנוחה",
}
MOVE_BUTTON = InlineKeyboardMarkup([[InlineKeyboardButton("📅 להזיז את האימון של היום", callback_data="move")]])
HEBREW_DAYS = ["שני", "שלישי", "רביעי", "חמישי", "שישי", "שבת", "ראשון"]  # datetime.weekday(): שני=0

# מצבים של השיחה המודרכת (/update) ושל חידוש טוקן מהטלפון
CHOOSING, ASK_SLEEP, ASK_PAIN, RENEW_WAIT = range(4)


# ---------------------------------------------------------------------------
# הרשאות - הבוט מציג מידע בריאותי, אז רק משתמשים מורשים
# ---------------------------------------------------------------------------

def _allowed_user_ids() -> set[int]:
    raw = os.getenv("TELEGRAM_ALLOWED_USER_IDS", "")
    return {int(x) for x in raw.replace(" ", "").split(",") if x}


ALLOWED_USERS = _allowed_user_ids()
# אם לא הוגדרה רשימה - הבוט פתוח לכולם (מתאים רק לפיתוח)
AUTH_FILTER = filters.User(user_id=ALLOWED_USERS) if ALLOWED_USERS else filters.ALL


# ---------------------------------------------------------------------------
# ניהול הקשר (State) - נשמר ב-user_data ושורד הפעלה מחדש (PicklePersistence)
# ---------------------------------------------------------------------------

def get_user_context(context: ContextTypes.DEFAULT_TYPE) -> UserContext:
    """מחזיר את ההקשר של המשתמש להיום, ויוצר/מאפס אותו לפי הצורך."""
    user_ctx = context.user_data.get("ctx")
    if user_ctx is None:
        user_ctx = context.user_data["ctx"] = UserContext()
    user_ctx.reset_if_new_day()
    return user_ctx


# ---------------------------------------------------------------------------
# בניית הודעות
# ---------------------------------------------------------------------------

def _fmt(value, unit: str = "", digits: int = 0) -> str:
    if value is None:
        return "—"
    return f"{value:.{digits}f}{unit}"


SOURCE_NAMES = {"google_health": "Fitbit (Google Health)", "recovery.json": "קובץ recovery.json", "mock": "נתוני דמה"}


def _sleep_line(recovery: services.RecoveryData) -> str:
    if recovery.tracker_not_worn:
        return "• שינה: — (הצמיד לא נענד בלילה)"
    if recovery.sleep_hours is None:
        return "• שינה: —"
    minutes = round(recovery.sleep_hours * 60)
    line = f"• שינה: {minutes // 60}:{minutes % 60:02d} שעות"
    if recovery.sleep_start and recovery.sleep_end:
        line += f" ({recovery.sleep_start:%H:%M}–{recovery.sleep_end:%H:%M})"
    return line


async def build_status_text(user_ctx: UserContext) -> str:
    recovery = await services.get_recovery_data(user_ctx)
    weather = await services.get_weather(user_ctx)

    readiness = _fmt(recovery.readiness_score)
    if recovery.readiness_breakdown:
        readiness += f" <i>({recovery.readiness_breakdown})</i>"
    lines = [
        "<b>📊 מדדי התאוששות</b>",
        f"• ציון התאוששות: {readiness}",
        _sleep_line(recovery),
        f"• דופק מנוחה: {_fmt(recovery.resting_hr, ' bpm')} (בסיס {_fmt(recovery.rhr_baseline)})",
        f"• HRV: {_fmt(recovery.hrv_ms, ' ms')}",
        f"• קצב נשימה: {_fmt(recovery.respiratory_rate, ' לדקה', 1)}",
        f"• טמפ' עור מול הבסיס: {_fmt(recovery.skin_temp_delta_c, '°C', 1)}",
        f"<i>מקור: {SOURCE_NAMES.get(recovery.source, recovery.source)}</i>",
        "",
        "<b>🌤 מזג אוויר</b>",
        f"• {weather.temperature_c:.0f}°C, לחות {weather.humidity_pct:.0f}%, "
        f"גשם {weather.rain_mm:g} מ\"מ, רוח {weather.wind_kmh:.0f} קמ\"ש",
    ]
    reported = _reported_summary(user_ctx)
    if reported:
        lines += ["", "<b>📝 מה שדיווחת היום</b>", *reported]
    if recovery.source == "mock":
        lines += ["", "<i>⚠️ האג'נט לא זמין - מוצגים נתוני דמה (פרטים בלוג של הבוט).</i>"]
    return "\n".join(lines)


def _reported_summary(user_ctx: UserContext) -> list[str]:
    items = []
    if user_ctx.reported_sleep_hours is not None:
        items.append(f"• שינה: {user_ctx.reported_sleep_hours:g} שעות")
    if user_ctx.injuries:
        items.append("• כאב: " + ", ".join(user_ctx.injuries))
    if user_ctx.feeling_exhausted:
        items.append("• עייפות")
    if user_ctx.feeling_sick:
        items.append("• מרגיש חולה")
    return items


async def build_workout_text(user_ctx: UserContext, morning: bool = False) -> str:
    recovery = await services.get_recovery_data(user_ctx)
    weather = await services.get_weather(user_ctx)
    plan = await services.decide_workout(recovery, weather, user_ctx)

    lines = []
    if morning:
        lines += ["☀️ <b>בוקר טוב!</b>", f"<b>{MORNING_HEADLINES[plan.level]}</b>", ""]
    lines += [f"<b>🏋️ {plan.title}</b>", ""]
    lines += [f"{i}. {step}" for i, step in enumerate(plan.details, 1)]
    lines += ["", "<b>למה?</b>", *[f"• {r}" for r in plan.reasons]]
    if recovery.source == "mock":
        reason = services.last_agent_error or "האג'נט לא זמין"
        lines += ["", f"<i>⚠️ אין גישה לנתוני הצמיד ({reason}) - ההמלצה על נתוני דמה.</i>"]
    return "\n".join(lines)


def _describe_update(update: UserUpdate) -> list[str]:
    """מה הבוט הבין מההודעה - כדי שהמשתמש יוכל לתקן אם טעה."""
    understood = []
    if update.sleep_hours is not None:
        understood.append(f"ישנת {update.sleep_hours:g} שעות")
    if update.injury:
        understood.append(f"כאב ב{update.injury}" if update.injury != "לא צוין" else "יש כאב (לא צוין איפה)")
    if update.exhausted:
        understood.append("אתה עייף")
    if update.sick:
        understood.append("אתה מרגיש חולה")
    if update.feeling_good:
        understood.append("אתה מרגיש טוב")
    return understood


# ---------------------------------------------------------------------------
# פקודות
# ---------------------------------------------------------------------------

async def start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    user_ctx = get_user_context(context)
    name = update.effective_user.first_name if update.effective_user else ""
    await update.message.reply_text(
        f"היי {name}! 👋 אני ה-Fitness Agent שלך.\n"
        "אני מתאים את האימון היומי לפי ההתאוששות שלך ומזג האוויר.\n\n"
        "אפשר פשוט לכתוב לי, למשל: <i>\"ישנתי רק 5 שעות\"</i> או <i>\"כואבת לי הברך\"</i>.\n"
        f"כל בוקר ב-{DAILY_TIME:%H:%M} אשלח לך אם מותר להתאמן היום.\n\n"
        "/workout - האימון להיום · /status - המדדים · /move - להזיז את האימון של היום · "
        "/update - עדכון מודרך · /daily - כיבוי/הפעלה של הודעת הבוקר",
        parse_mode=ParseMode.HTML,
    )
    await update.message.reply_text(await build_status_text(user_ctx), parse_mode=ParseMode.HTML)


async def status(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.message.reply_text(await build_status_text(get_user_context(context)), parse_mode=ParseMode.HTML)


async def workout(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.message.reply_text(await build_workout_text(get_user_context(context)), parse_mode=ParseMode.HTML)


async def refresh(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    """אחרי סנכרון הצמיד - מביא נתונים טריים במקום התוצאה השמורה."""
    services.clear_agent_cache()
    await update.message.reply_text("🔄 מביא נתונים טריים מהצמיד...")
    await update.message.reply_text(await build_status_text(get_user_context(context)), parse_mode=ParseMode.HTML)


async def reset(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    context.user_data["ctx"] = UserContext()
    await update.message.reply_text("🧹 מחקתי את כל מה שדיווחת היום.")


# ---------------------------------------------------------------------------
# הודעות חופשיות
# ---------------------------------------------------------------------------

async def free_text(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    text = update.message.text
    user_ctx = get_user_context(context)
    parsed = await services.parse_user_message(text)
    services.apply_update(user_ctx, parsed, text)

    if parsed.is_empty():
        await update.message.reply_text(
            "לא בטוח שהבנתי 🤔 שמרתי את זה כהערה.\n"
            "נסה למשל: \"ישנתי 6 שעות\", \"כואב לי הגב\", \"אני מותש\" - או /update."
        )
        return

    understood = "הבנתי ש" + ", ".join(_describe_update(parsed)) + ". מעדכן את ההמלצה 👇"
    await update.message.reply_text(understood)
    await update.message.reply_text(await build_workout_text(user_ctx), parse_mode=ParseMode.HTML)


# ---------------------------------------------------------------------------
# עדכון מודרך (ConversationHandler)
# ---------------------------------------------------------------------------

UPDATE_MENU = InlineKeyboardMarkup([
    [InlineKeyboardButton("😴 שעות שינה", callback_data="sleep")],
    [InlineKeyboardButton("🤕 כאב / פציעה", callback_data="pain")],
    [InlineKeyboardButton("🥱 אני עייף", callback_data="tired"),
     InlineKeyboardButton("🤒 אני חולה", callback_data="sick")],
    [InlineKeyboardButton("✅ סיימתי", callback_data="done")],
])


async def update_start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    await update.message.reply_text("מה תרצה לעדכן?", reply_markup=UPDATE_MENU)
    return CHOOSING


async def update_choice(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    query = update.callback_query
    await query.answer()
    user_ctx = get_user_context(context)

    match query.data:
        case "sleep":
            await query.edit_message_text("כמה שעות ישנת? (מספר, למשל 5.5)")
            return ASK_SLEEP
        case "pain":
            await query.edit_message_text("איפה כואב? (למשל: ברך, גב, כתף)")
            return ASK_PAIN
        case "tired":
            user_ctx.feeling_exhausted = True
            await query.edit_message_text("רשמתי שאתה עייף. עוד משהו?", reply_markup=UPDATE_MENU)
            return CHOOSING
        case "sick":
            user_ctx.feeling_sick = True
            await query.edit_message_text("רשמתי שאתה חולה - רפואה שלמה 🙏 עוד משהו?", reply_markup=UPDATE_MENU)
            return CHOOSING
        case _:  # done
            await query.edit_message_text("מעולה, מעדכן את ההמלצה 👇")
            await query.message.reply_text(await build_workout_text(user_ctx), parse_mode=ParseMode.HTML)
            return ConversationHandler.END


async def update_sleep(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    try:
        hours = float(update.message.text.strip().replace(",", "."))
        if not 0 <= hours <= 16:
            raise ValueError
    except ValueError:
        await update.message.reply_text("צריך מספר בין 0 ל-16, למשל 6.5. נסה שוב:")
        return ASK_SLEEP
    get_user_context(context).reported_sleep_hours = hours
    await update.message.reply_text(f"רשמתי {hours:g} שעות שינה. עוד משהו?", reply_markup=UPDATE_MENU)
    return CHOOSING


async def update_pain(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    area = update.message.text.strip()
    user_ctx = get_user_context(context)
    if area and area not in user_ctx.injuries:
        user_ctx.injuries.append(area)
    await update.message.reply_text(f"רשמתי כאב ב{area}. עוד משהו?", reply_markup=UPDATE_MENU)
    return CHOOSING


async def update_cancel(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    await update.message.reply_text("בוטל. מה שכבר רשמתי נשמר.")
    return ConversationHandler.END


# ---------------------------------------------------------------------------
# עדכון בוקר אוטומטי (JobQueue)
# ---------------------------------------------------------------------------

async def send_daily_update(context: ContextTypes.DEFAULT_TYPE) -> None:
    """ההודעה של כל בוקר: מותר / קל / אסור להתאמן, עם כפתור להזזת האימון."""
    services.clear_agent_cache()  # בבוקר תמיד נתונים טריים מהלילה
    # ה-job נוצר עם user_id, ולכן context.user_data הוא של אותו משתמש
    text = await build_workout_text(get_user_context(context), morning=True)
    problems = [s for s in services.token_statuses() if s.state in ("expiring", "expired", "missing")]
    if problems:
        text += "\n\n" + "\n".join(f"🔑 {TOKEN_STATE_SHORT[s.state]}: {s.label}" for s in problems) + \
                "\n<i>פרטים ואיך לחדש: /tokens</i>"
    await context.bot.send_message(context.job.chat_id, text, parse_mode=ParseMode.HTML, reply_markup=MOVE_BUTTON)


def _daily_jobs(application: Application, chat_id: int):
    return application.job_queue.get_jobs_by_name(f"daily-{chat_id}")


def _schedule_daily(application: Application, chat_id: int, user_id: int) -> None:
    for job in _daily_jobs(application, chat_id):
        job.schedule_removal()
    application.job_queue.run_daily(send_daily_update, DAILY_TIME, chat_id=chat_id,
                                    user_id=user_id, name=f"daily-{chat_id}")


async def daily(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    """/daily מפעיל או מכבה עדכון בוקר. הבחירה נשמרת ב-chat_data ושורדת הפעלה מחדש."""
    chat_id, user_id = update.effective_chat.id, update.effective_user.id
    if _daily_jobs(context.application, chat_id):
        context.chat_data["daily"] = False
        for job in _daily_jobs(context.application, chat_id):
            job.schedule_removal()
        await update.message.reply_text("🔕 עדכון הבוקר כובה. /daily מפעיל אותו שוב.")
    else:
        context.chat_data["daily"] = True
        context.chat_data["daily_user"] = user_id
        _schedule_daily(context.application, chat_id, user_id)
        await update.message.reply_text(f"🔔 אשלח לך כל בוקר ב-{DAILY_TIME:%H:%M} אם מותר להתאמן היום.")


async def restore_daily_jobs(application: Application) -> None:
    """
    בהפעלה: עדכון הבוקר פועל אוטומטית לכל משתמש מורשה (בצ'אט פרטי ה-chat_id
    שווה ל-user_id), אלא אם כיבה אותו ב-/daily. וגם לכל מי שהפעיל אותו ידנית.
    """
    for user_id in ALLOWED_USERS:
        if application.chat_data.get(user_id, {}).get("daily", True):
            _schedule_daily(application, user_id, user_id)
    for chat_id, data in application.chat_data.items():
        if data.get("daily") and data.get("daily_user") and not _daily_jobs(application, chat_id):
            _schedule_daily(application, chat_id, data["daily_user"])
    log.info("Morning update at %s for %d chat(s)", DAILY_TIME.strftime("%H:%M"),
             sum(1 for job in application.job_queue.jobs() if job.name.startswith("daily-")))
    # בדיקת הטוקנים - כל שעה (קריאת קבצים בלבד, בלי פנייה לגוגל)
    application.job_queue.run_repeating(check_tokens, interval=3600, first=60, name="token-check")


# ---------------------------------------------------------------------------
# תזכורות לחידוש הטוקנים של גוגל (במצב Testing הם פגים אחרי 7 ימים)
# ---------------------------------------------------------------------------

TOKEN_STATE_SHORT = {
    "ok": "✅ תקין",
    "expiring": "⏰ יפוג בקרוב",
    "expired": "⚠️ פג",
    "missing": "⚠️ חסר",
    "unknown": "❔ לא ידוע מתי יפוג",
}


def _format_dt(when: dt.datetime) -> str:
    return _format_when(when.isoformat())


def token_alert_text(status: services.TokenStatus) -> str:
    """הודעת תזכורת לטוקן אחד, עם הוראות חידוש."""
    if status.state == "expiring":
        title = f"⏰ <b>הגישה ל{status.label} תפוג {_format_dt(status.expires_at)}</b>"
        why = "גוגל מבטלת אותה 7 ימים אחרי האישור, כי האפליקציה במצב Testing. כדאי לחדש כבר עכשיו."
    elif status.state == "missing":
        title = f"⚠️ <b>אין גישה ל{status.label}</b>"
        why = f"הקובץ {status.filename} לא נמצא."
    else:
        title = f"⚠️ <b>הגישה ל{status.label} פגה</b>"
        why = "גוגל לא מקבלת יותר את הטוקן - עד החידוש, הנתונים האלה לא זמינים."
    return "\n".join([
        title, why, "",
        "👇 לחץ על <b>לחדש עכשיו</b> - זה לוקח דקה, מהטלפון.",
        "",
        "<i>או מהמחשב, בתיקיית הפרויקט (עם CLIENT_ID ו-CLIENT_SECRET):</i>",
        f"<pre>Remove-Item {status.filename} -ErrorAction SilentlyContinue\n{status.renew_command}</pre>",
        f"<i>ואם הבוט על שרת - להעתיק אליו את {status.filename} (לתיקייה data).</i>",
    ])


def renew_button(key: str) -> InlineKeyboardButton:
    return InlineKeyboardButton(f"🔗 לחדש עכשיו: {services.token_label(key)}", callback_data=f"renew:{key}")


def _alert_recipients(application: Application) -> set[int]:
    if ALLOWED_USERS:
        return set(ALLOWED_USERS)
    return {chat_id for chat_id, data in application.chat_data.items() if data.get("daily")}


async def check_tokens(context: ContextTypes.DEFAULT_TYPE) -> None:
    """שולח תזכורת כשטוקן עומד לפוג, פג או חסר - לכל היותר פעם ביום לכל מצב."""
    sent = context.bot_data.setdefault("token_alerts", {})  # "health:expiring" -> תאריך
    today = dt.date.today().isoformat()
    for status in services.token_statuses():
        if status.state not in ("expiring", "expired", "missing"):
            # תקין - מאפס, כדי שהתפוגה הבאה תתריע שוב
            for key in [k for k in sent if k.startswith(status.key + ":")]:
                del sent[key]
            continue
        alert_key = f"{status.key}:{status.state}"
        if sent.get(alert_key) == today:
            continue
        for chat_id in _alert_recipients(context.application):
            await context.bot.send_message(chat_id, token_alert_text(status), parse_mode=ParseMode.HTML,
                                           reply_markup=InlineKeyboardMarkup([[renew_button(status.key)]]))
        sent[alert_key] = today
        log.info("Token alert sent: %s", alert_key)


async def tokens(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    """/tokens - מצב הגישה לגוגל ומתי צריך לחדש."""
    lines = ["🔑 <b>הגישה לגוגל</b>", ""]
    statuses = services.token_statuses()
    for status in statuses:
        line = f"{TOKEN_STATE_SHORT[status.state]} - {status.label}"
        if status.expires_at and status.state in ("ok", "expiring"):
            line += f"\n    בתוקף עד {_format_dt(status.expires_at)}"
        lines.append(line)
    lines += ["", "<i>אפשר לחדש בכל רגע (למשל את שניהם יחד, כדי שיפגו באותו יום):</i>"]
    buttons = InlineKeyboardMarkup([[renew_button(s.key)] for s in statuses])
    await update.message.reply_text("\n".join(lines), parse_mode=ParseMode.HTML, reply_markup=buttons)


# ---------------------------------------------------------------------------
# חידוש טוקן מהטלפון: קישור לאישור -> המשתמש מדביק את הכתובת שאליה הגיע
# ---------------------------------------------------------------------------

async def renew_start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    query = update.callback_query
    if not _is_allowed(update):
        await query.answer("אין לך הרשאה", show_alert=True)
        return ConversationHandler.END
    await query.answer()
    key = query.data.split(":", 1)[1]
    result = await services.start_token_renewal(key)
    if not result.get("ok"):
        await query.message.reply_text(f"לא הצלחתי ליצור קישור 😕\n<i>{html.escape(result.get('error', ''))}</i>",
                                       parse_mode=ParseMode.HTML)
        return ConversationHandler.END

    context.user_data["renew_key"] = key
    await query.message.reply_text(
        "\n".join([
            f"🔗 <b>חידוש הגישה ל{services.token_label(key)}</b>",
            "",
            "1. לחץ על הכפתור למטה ואשר את הגישה בגוגל.",
            "   (אם מופיע \"Google hasn't verified this app\" - Advanced ← Go to... )",
            "2. בסוף הדפדפן יעבור לדף <b>שלא נטען</b> (localhost) - זה בסדר.",
            "3. העתק את <b>הכתובת המלאה</b> משורת הכתובת ושלח לי אותה כאן.",
            "",
            "<i>הקישור תקף ל-15 דקות. /cancel לביטול.</i>",
        ]),
        parse_mode=ParseMode.HTML,
        reply_markup=InlineKeyboardMarkup([[InlineKeyboardButton("🔐 לאישור בגוגל", url=result["url"])]]),
    )
    return RENEW_WAIT


async def renew_code(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    key = context.user_data.get("renew_key")
    if not key:
        return ConversationHandler.END
    pasted = update.message.text.strip()
    if "code=" not in pasted and "error=" not in pasted:
        await update.message.reply_text(
            "זו לא נראית הכתובת הנכונה. צריך את הכתובת המלאה של הדף שלא נטען - "
            "היא מתחילה ב-http://localhost:8080/callback ויש בה code=. נסה שוב, או /cancel.")
        return RENEW_WAIT

    await update.message.reply_text("⏳ שומר את הגישה החדשה...")
    result = await services.finish_token_renewal(key, pasted)
    if not result.get("ok"):
        await update.message.reply_text(
            f"החידוש נכשל 😕\n<i>{html.escape(result.get('error', ''))}</i>\n\n"
            "אפשר להדביק שוב, או לבקש קישור חדש ב-/tokens.", parse_mode=ParseMode.HTML)
        return RENEW_WAIT

    context.user_data.pop("renew_key", None)
    status = next(s for s in services.token_statuses() if s.key == key)
    until = f" - בתוקף עד {_format_dt(status.expires_at)}" if status.expires_at else ""
    await update.message.reply_text(f"✅ הגישה ל{status.label} חודשה{until}.")
    return ConversationHandler.END


async def renew_cancel(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    context.user_data.pop("renew_key", None)
    await update.message.reply_text("בוטל. אפשר לחדש מתי שתרצה דרך /tokens.")
    return ConversationHandler.END


# ---------------------------------------------------------------------------
# הזזת האימון של היום ביומן גוגל (/move או הכפתור בהודעת הבוקר)
# ---------------------------------------------------------------------------

def _is_allowed(update: Update) -> bool:
    return not ALLOWED_USERS or (update.effective_user is not None and update.effective_user.id in ALLOWED_USERS)


def _format_when(iso: str) -> str:
    when = dt.datetime.fromisoformat(iso).astimezone(TIMEZONE)
    return f"יום {HEBREW_DAYS[when.weekday()]} {when:%d/%m} בשעה {when:%H:%M}"


def _describe_changes(result: dict) -> list[str]:
    lines = []
    for change in result.get("changes", []):
        if change.get("new_summary"):
            lines.append(f"• \"{change['summary']}\" יסומן כמוחלף (האימון שלך ייכנס במקומו)")
        else:
            lines.append(f"• \"{change['summary']}\": {_format_when(change['from'])} ← {_format_when(change['to'])}")
    return lines


MUSCLE_GROUPS = {"A": "רגליים, גב, יד קדמית", "B": "חזה, כתפיים, יד אחורית", "full": "כל הגוף"}


def _describe_replacement(replacement: dict) -> list[str]:
    """האימון הקצר שמחליף את האימון של היום (כשהשבוע מלא): שם, משך וקישור."""
    kind = html.escape(replacement["type"])
    if replacement.get("muscle_group"):
        kind += f" ({MUSCLE_GROUPS.get(replacement['muscle_group'], replacement['muscle_group'])})"
    return [
        f"🎬 <b>{html.escape(replacement['title'])}</b>",
        f"⏱ {replacement['duration_min']} דקות · {kind} · עצימות {html.escape(replacement['intensity'])}",
        html.escape(replacement["url"]),
    ]


async def _preview_move(reply) -> None:
    """מציג מה ישתנה ביומן ומבקש אישור. reply = פונקציה לשליחת הודעה."""
    result = await services.move_todays_workout(dry_run=True)
    if not result.get("ok"):
        await reply(f"לא הצלחתי להזיז את האימון 😕\n<i>{html.escape(result.get('error', ''))}</i>",
                    parse_mode=ParseMode.HTML)
        return

    replacement = result.get("replacement")
    if replacement:
        # השבוע מלא - במקום להזיז, האימון של היום יוחלף באימון קצר מהמאגר
        confirm = "✅ כן, להחליף"
        text = "\n".join([
            f"📅 <b>אין לאן להזיז את \"{html.escape(result['workout'])}\" - השבוע מלא</b>",
            "", "במקום זה אחליף אותו ביומן (באותה שעה) באימון קצר:", "", *_describe_replacement(replacement),
        ])
    else:
        confirm = "✅ כן, להזיז"
        text = "\n".join([f"📅 <b>הזזת \"{html.escape(result['workout'])}\"</b>", "", "מה ישתנה ביומן:",
                          *_describe_changes(result)])
    buttons = InlineKeyboardMarkup([[
        InlineKeyboardButton(confirm, callback_data="move_confirm"),
        InlineKeyboardButton("❌ ביטול", callback_data="move_cancel"),
    ]])
    await reply(text, parse_mode=ParseMode.HTML, reply_markup=buttons)


async def move(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.message.reply_text("🔎 בודק את היומן...")
    await _preview_move(update.message.reply_text)


async def move_callback(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    query = update.callback_query
    if not _is_allowed(update):
        await query.answer("אין לך הרשאה", show_alert=True)
        return
    await query.answer()

    if query.data == "move":  # הכפתור בהודעת הבוקר
        await _preview_move(query.message.reply_text)
    elif query.data == "move_cancel":
        await query.edit_message_text("בוטל - היומן לא השתנה.")
    elif query.data == "move_confirm":
        await query.edit_message_text("⏳ מזיז ביומן...")
        # מחשב שוב לפני הביצוע, למקרה שהיומן השתנה מאז התצוגה המקדימה
        result = await services.move_todays_workout(dry_run=False)
        if result.get("ok") and result.get("replacement"):
            await query.edit_message_text("\n".join([
                f"✅ <b>\"{html.escape(result['workout'])}\" הוחלף ביומן באימון קצר:</b>", "",
                *_describe_replacement(result["replacement"]),
            ]), parse_mode=ParseMode.HTML)
        elif result.get("ok"):
            await query.edit_message_text("\n".join(["✅ <b>האימון הוזז ביומן</b>", "", *_describe_changes(result)]),
                                          parse_mode=ParseMode.HTML)
        else:
            await query.edit_message_text(f"ההזזה נכשלה 😕\n<i>{html.escape(result.get('error', ''))}</i>",
                                          parse_mode=ParseMode.HTML)


async def on_error(update: object, context: ContextTypes.DEFAULT_TYPE) -> None:
    log.exception("Unhandled error", exc_info=context.error)
    if isinstance(update, Update) and update.effective_message:
        await update.effective_message.reply_text("משהו השתבש אצלי 😕 נסה שוב בעוד רגע.")


# ---------------------------------------------------------------------------
# הרכבת הבוט
# ---------------------------------------------------------------------------

def build_application(token: str) -> Application:
    persistence = PicklePersistence(filepath=os.getenv("BOT_STATE_FILE", "bot_state.pickle"))
    app = (Application.builder().token(token).persistence(persistence)
           .post_init(restore_daily_jobs).build())

    # חייב להירשם לפני ה-handler של הטקסט החופשי, כדי שתשובות בתוך /update לא ייתפסו שם
    app.add_handler(ConversationHandler(
        entry_points=[CommandHandler("update", update_start, filters=AUTH_FILTER)],
        states={
            CHOOSING: [CallbackQueryHandler(update_choice, pattern="^(sleep|pain|tired|sick|done)$")],
            ASK_SLEEP: [MessageHandler(filters.TEXT & ~filters.COMMAND, update_sleep)],
            ASK_PAIN: [MessageHandler(filters.TEXT & ~filters.COMMAND, update_pain)],
        },
        fallbacks=[CommandHandler("cancel", update_cancel)],
        name="update_conversation",
        persistent=True,
    ))
    # חידוש טוקן: גם הוא לפני הטקסט החופשי, כדי שהכתובת המודבקת תגיע אליו
    app.add_handler(ConversationHandler(
        entry_points=[CallbackQueryHandler(renew_start, pattern="^renew:(health|calendar)$")],
        states={RENEW_WAIT: [MessageHandler(AUTH_FILTER & filters.TEXT & ~filters.COMMAND, renew_code)]},
        fallbacks=[CommandHandler("cancel", renew_cancel)],
        name="renew_conversation",
        persistent=True,
        conversation_timeout=15 * 60,  # כמו תוקף הקישור
    ))
    app.add_handler(CommandHandler("start", start, filters=AUTH_FILTER))
    app.add_handler(CommandHandler("status", status, filters=AUTH_FILTER))
    app.add_handler(CommandHandler("workout", workout, filters=AUTH_FILTER))
    app.add_handler(CommandHandler("daily", daily, filters=AUTH_FILTER))
    app.add_handler(CommandHandler("reset", reset, filters=AUTH_FILTER))
    app.add_handler(CommandHandler("refresh", refresh, filters=AUTH_FILTER))
    app.add_handler(CommandHandler("move", move, filters=AUTH_FILTER))
    app.add_handler(CommandHandler("tokens", tokens, filters=AUTH_FILTER))
    app.add_handler(CallbackQueryHandler(move_callback, pattern="^move"))
    app.add_handler(MessageHandler(AUTH_FILTER & filters.TEXT & ~filters.COMMAND, free_text))
    app.add_error_handler(on_error)
    return app


def main() -> None:
    token = os.getenv("TELEGRAM_BOT_TOKEN")
    if not token:
        raise SystemExit("חסר TELEGRAM_BOT_TOKEN - ראו הוראות הגדרה.")
    if not ALLOWED_USERS:
        log.warning("TELEGRAM_ALLOWED_USER_IDS לא הוגדר - הבוט פתוח לכל משתמש!")
    build_application(token).run_polling(allowed_updates=Update.ALL_TYPES)


if __name__ == "__main__":
    main()
