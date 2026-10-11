"""
Telegram bot for the Fitness Agent.

Commands:
    /start    - start a conversation + today's status
    /status   - Fitbit metrics and weather
    /workout  - today's workout plan
    /update   - guided update (sleep / pain / fatigue)
    /move     - move today's workout on Google Calendar (with a preview and confirmation)
    /daily    - turn the morning update off/on (on automatically for allowed users)
    /refresh  - fresh data from the tracker (after a sync)
    /tokens   - Google access status and how to renew (reminders are also sent automatically)
    /reset    - clear what you reported today
    /cancel   - leave a guided update

Any free-text message ("I only slept 4 hours", "my knee hurts") goes to parse_user_message
and updates the recommendation.

The data comes from the Go agent (see services.py). Before running, build it from the
project folder:  go build -o fitness-agent.exe ./cmd/agent
and run it once without -json to approve Google access in the browser.

Running (from the telegram_bot folder):
    python -m venv .venv
    .venv\\Scripts\\activate
    pip install -r requirements.txt
    set TELEGRAM_BOT_TOKEN=...              (from @BotFather)
    set TELEGRAM_ALLOWED_USER_IDS=123456    (your ID, from @userinfobot)
    set CLIENT_ID=... / set CLIENT_SECRET=...  (same as the agent - it inherits them)
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

# The /update conversation has both buttons and text messages, so per_message=False is right.
# PTB always warns about it - the behavior here is intentional.
warnings.filterwarnings("ignore", message=r".*CallbackQueryHandler", category=PTBUserWarning)

import services
from services import UserContext, UserUpdate

logging.basicConfig(format="%(asctime)s %(levelname)s %(name)s: %(message)s", level=logging.INFO)
logging.getLogger("httpx").setLevel(logging.WARNING)  # no log line for every polling request
log = logging.getLogger("fitness-bot")

TIMEZONE = ZoneInfo("Asia/Jerusalem")
# Time of the morning update (e.g. DAILY_TIME=07:30). Best after the tracker syncs with the phone.
DAILY_TIME = dt.time.fromisoformat(os.getenv("DAILY_TIME", "07:00")).replace(tzinfo=TIMEZONE)

MORNING_HEADLINES = {
    "full": "✅ You're good to train today",
    "light": "⚠️ Light workout only today",
    "rest": "⛔ No training today - rest day",
}
MOVE_BUTTON = InlineKeyboardMarkup([[InlineKeyboardButton("📅 Move today's workout", callback_data="move")]])

# States of the guided conversation (/update) and of token renewal from the phone
CHOOSING, ASK_SLEEP, ASK_PAIN, RENEW_WAIT = range(4)


# ---------------------------------------------------------------------------
# Access control - the bot shows health data, so allowed users only
# ---------------------------------------------------------------------------

def _allowed_user_ids() -> set[int]:
    raw = os.getenv("TELEGRAM_ALLOWED_USER_IDS", "")
    return {int(x) for x in raw.replace(" ", "").split(",") if x}


ALLOWED_USERS = _allowed_user_ids()
# Without a list the bot is open to everyone (only suitable for development)
AUTH_FILTER = filters.User(user_id=ALLOWED_USERS) if ALLOWED_USERS else filters.ALL


# ---------------------------------------------------------------------------
# Context (state) - kept in user_data and survives restarts (PicklePersistence)
# ---------------------------------------------------------------------------

def get_user_context(context: ContextTypes.DEFAULT_TYPE) -> UserContext:
    """Returns the user's context for today, creating/resetting it as needed."""
    user_ctx = context.user_data.get("ctx")
    if user_ctx is None:
        user_ctx = context.user_data["ctx"] = UserContext()
    user_ctx.reset_if_new_day()
    return user_ctx


# ---------------------------------------------------------------------------
# Building messages
# ---------------------------------------------------------------------------

def _fmt(value, unit: str = "", digits: int = 0) -> str:
    if value is None:
        return "—"
    return f"{value:.{digits}f}{unit}"


SOURCE_NAMES = {"google_health": "Fitbit (Google Health)", "recovery.json": "recovery.json file", "mock": "mock data"}


def _sleep_line(recovery: services.RecoveryData) -> str:
    if recovery.tracker_not_worn:
        return "• Sleep: — (tracker not worn overnight)"
    if recovery.sleep_hours is None:
        return "• Sleep: —"
    minutes = round(recovery.sleep_hours * 60)
    line = f"• Sleep: {minutes // 60}h {minutes % 60:02d}m"
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
        "<b>📊 Recovery</b>",
        f"• Readiness score: {readiness}",
        _sleep_line(recovery),
        f"• Resting heart rate: {_fmt(recovery.resting_hr, ' bpm')} (baseline {_fmt(recovery.rhr_baseline)})",
        f"• HRV: {_fmt(recovery.hrv_ms, ' ms')}",
        f"• Breathing rate: {_fmt(recovery.respiratory_rate, ' /min', 1)}",
        f"• Skin temp vs baseline: {_fmt(recovery.skin_temp_delta_c, '°C', 1)}",
        f"<i>Source: {SOURCE_NAMES.get(recovery.source, recovery.source)}</i>",
        "",
        "<b>🌤 Weather</b>",
        f"• {weather.temperature_c:.0f}°C, humidity {weather.humidity_pct:.0f}%, "
        f"rain {weather.rain_mm:g} mm, wind {weather.wind_kmh:.0f} km/h",
    ]
    reported = _reported_summary(user_ctx)
    if reported:
        lines += ["", "<b>📝 What you reported today</b>", *reported]
    if recovery.source == "mock":
        lines += ["", "<i>⚠️ The agent isn't available - showing mock data (details in the bot's log).</i>"]
    return "\n".join(lines)


def _reported_summary(user_ctx: UserContext) -> list[str]:
    items = []
    if user_ctx.reported_sleep_hours is not None:
        items.append(f"• Sleep: {user_ctx.reported_sleep_hours:g} hours")
    if user_ctx.injuries:
        items.append("• Pain: " + ", ".join(user_ctx.injuries))
    if user_ctx.feeling_exhausted:
        items.append("• Tired")
    if user_ctx.feeling_sick:
        items.append("• Feeling sick")
    return items


async def build_workout_text(user_ctx: UserContext, morning: bool = False) -> str:
    recovery = await services.get_recovery_data(user_ctx)
    weather = await services.get_weather(user_ctx)
    plan = await services.decide_workout(recovery, weather, user_ctx)

    lines = []
    if morning:
        lines += ["☀️ <b>Good morning!</b>", f"<b>{MORNING_HEADLINES[plan.level]}</b>", ""]
    lines += [f"<b>🏋️ {plan.title}</b>", ""]
    lines += [f"{i}. {step}" for i, step in enumerate(plan.details, 1)]
    lines += ["", "<b>Why?</b>", *[f"• {r}" for r in plan.reasons]]
    if recovery.source == "mock":
        reason = services.last_agent_error or "the agent isn't available"
        lines += ["", f"<i>⚠️ No access to tracker data ({html.escape(reason)}) - this recommendation uses mock data.</i>"]
    return "\n".join(lines)


def _describe_update(update: UserUpdate) -> list[str]:
    """What the bot understood from the message - so the user can correct it if it's wrong."""
    understood = []
    if update.sleep_hours is not None:
        understood.append(f"you slept {update.sleep_hours:g} hours")
    if update.injury:
        understood.append(f"your {update.injury} hurts" if update.injury != services.NO_BODY_PART
                          else "something hurts (not sure where)")
    if update.exhausted:
        understood.append("you're tired")
    if update.sick:
        understood.append("you feel sick")
    if update.feeling_good:
        understood.append("you feel good")
    return understood


# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------

async def start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    user_ctx = get_user_context(context)
    name = update.effective_user.first_name if update.effective_user else ""
    await update.message.reply_text(
        f"Hi {html.escape(name)}! 👋 I'm your Fitness Agent.\n"
        "I adapt your daily workout to your recovery and the weather.\n\n"
        "You can just write to me, e.g. <i>\"I only slept 5 hours\"</i> or <i>\"my knee hurts\"</i>.\n"
        f"Every morning at {DAILY_TIME:%H:%M} I'll tell you whether you should train today.\n\n"
        "/workout - today's workout · /status - your metrics · /move - move today's workout · "
        "/update - guided update · /daily - turn the morning message off/on",
        parse_mode=ParseMode.HTML,
    )
    await update.message.reply_text(await build_status_text(user_ctx), parse_mode=ParseMode.HTML)


async def status(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.message.reply_text(await build_status_text(get_user_context(context)), parse_mode=ParseMode.HTML)


async def workout(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.message.reply_text(await build_workout_text(get_user_context(context)), parse_mode=ParseMode.HTML)


async def refresh(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    """After a tracker sync - fetch fresh data instead of the cached result."""
    services.clear_agent_cache()
    await update.message.reply_text("🔄 Fetching fresh data from the tracker...")
    await update.message.reply_text(await build_status_text(get_user_context(context)), parse_mode=ParseMode.HTML)


async def reset(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    context.user_data["ctx"] = UserContext()
    await update.message.reply_text("🧹 Cleared everything you reported today.")


# ---------------------------------------------------------------------------
# Free-text messages
# ---------------------------------------------------------------------------

async def free_text(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    text = update.message.text
    user_ctx = get_user_context(context)
    parsed = await services.parse_user_message(text)
    services.apply_update(user_ctx, parsed, text)

    if parsed.is_empty():
        await update.message.reply_text(
            "Not sure I got that 🤔 I saved it as a note.\n"
            "Try e.g. \"I slept 6 hours\", \"my back hurts\", \"I'm exhausted\" - or /update."
        )
        return

    understood = "Got it: " + ", ".join(_describe_update(parsed)) + ". Updating the recommendation 👇"
    await update.message.reply_text(understood)
    await update.message.reply_text(await build_workout_text(user_ctx), parse_mode=ParseMode.HTML)


# ---------------------------------------------------------------------------
# Guided update (ConversationHandler)
# ---------------------------------------------------------------------------

UPDATE_MENU = InlineKeyboardMarkup([
    [InlineKeyboardButton("😴 Hours of sleep", callback_data="sleep")],
    [InlineKeyboardButton("🤕 Pain / injury", callback_data="pain")],
    [InlineKeyboardButton("🥱 I'm tired", callback_data="tired"),
     InlineKeyboardButton("🤒 I'm sick", callback_data="sick")],
    [InlineKeyboardButton("✅ Done", callback_data="done")],
])


async def update_start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    await update.message.reply_text("What would you like to update?", reply_markup=UPDATE_MENU)
    return CHOOSING


async def update_choice(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    query = update.callback_query
    await query.answer()
    user_ctx = get_user_context(context)

    match query.data:
        case "sleep":
            await query.edit_message_text("How many hours did you sleep? (a number, e.g. 5.5)")
            return ASK_SLEEP
        case "pain":
            await query.edit_message_text("Where does it hurt? (e.g. knee, back, shoulder)")
            return ASK_PAIN
        case "tired":
            user_ctx.feeling_exhausted = True
            await query.edit_message_text("Noted that you're tired. Anything else?", reply_markup=UPDATE_MENU)
            return CHOOSING
        case "sick":
            user_ctx.feeling_sick = True
            await query.edit_message_text("Noted that you're sick - get well soon 🙏 Anything else?",
                                          reply_markup=UPDATE_MENU)
            return CHOOSING
        case _:  # done
            await query.edit_message_text("Great, updating the recommendation 👇")
            await query.message.reply_text(await build_workout_text(user_ctx), parse_mode=ParseMode.HTML)
            return ConversationHandler.END


async def update_sleep(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    try:
        hours = float(update.message.text.strip().replace(",", "."))
        if not 0 <= hours <= 16:
            raise ValueError
    except ValueError:
        await update.message.reply_text("I need a number between 0 and 16, e.g. 6.5. Try again:")
        return ASK_SLEEP
    get_user_context(context).reported_sleep_hours = hours
    await update.message.reply_text(f"Noted {hours:g} hours of sleep. Anything else?", reply_markup=UPDATE_MENU)
    return CHOOSING


async def update_pain(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    area = update.message.text.strip()
    user_ctx = get_user_context(context)
    if area and area not in user_ctx.injuries:
        user_ctx.injuries.append(area)
    await update.message.reply_text(f"Noted pain in your {area}. Anything else?", reply_markup=UPDATE_MENU)
    return CHOOSING


async def update_cancel(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    await update.message.reply_text("Cancelled. What I already noted is kept.")
    return ConversationHandler.END


# ---------------------------------------------------------------------------
# Automatic morning update (JobQueue)
# ---------------------------------------------------------------------------

async def send_daily_update(context: ContextTypes.DEFAULT_TYPE) -> None:
    """The morning message: train / light / rest, with a button to move the workout."""
    services.clear_agent_cache()  # in the morning, always fresh data from last night
    # The job is created with user_id, so context.user_data belongs to that user
    text = await build_workout_text(get_user_context(context), morning=True)
    problems = [s for s in services.token_statuses() if s.state in ("expiring", "expired", "missing")]
    if problems:
        text += "\n\n" + "\n".join(f"🔑 {TOKEN_STATE_SHORT[s.state]}: {s.label}" for s in problems) + \
                "\n<i>Details and how to renew: /tokens</i>"
    await context.bot.send_message(context.job.chat_id, text, parse_mode=ParseMode.HTML, reply_markup=MOVE_BUTTON)


def _daily_jobs(application: Application, chat_id: int):
    return application.job_queue.get_jobs_by_name(f"daily-{chat_id}")


def _schedule_daily(application: Application, chat_id: int, user_id: int) -> None:
    for job in _daily_jobs(application, chat_id):
        job.schedule_removal()
    application.job_queue.run_daily(send_daily_update, DAILY_TIME, chat_id=chat_id,
                                    user_id=user_id, name=f"daily-{chat_id}")


async def daily(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    """/daily turns the morning update on or off. The choice is kept in chat_data and survives restarts."""
    chat_id, user_id = update.effective_chat.id, update.effective_user.id
    if _daily_jobs(context.application, chat_id):
        context.chat_data["daily"] = False
        for job in _daily_jobs(context.application, chat_id):
            job.schedule_removal()
        await update.message.reply_text("🔕 Morning update turned off. /daily turns it back on.")
    else:
        context.chat_data["daily"] = True
        context.chat_data["daily_user"] = user_id
        _schedule_daily(context.application, chat_id, user_id)
        await update.message.reply_text(f"🔔 Every morning at {DAILY_TIME:%H:%M} I'll tell you whether to train today.")


async def restore_daily_jobs(application: Application) -> None:
    """
    On startup: the morning update is on automatically for every allowed user (in a private
    chat chat_id equals user_id), unless they turned it off with /daily. Also for anyone
    who turned it on manually.
    """
    for user_id in ALLOWED_USERS:
        if application.chat_data.get(user_id, {}).get("daily", True):
            _schedule_daily(application, user_id, user_id)
    for chat_id, data in application.chat_data.items():
        if data.get("daily") and data.get("daily_user") and not _daily_jobs(application, chat_id):
            _schedule_daily(application, chat_id, data["daily_user"])
    log.info("Morning update at %s for %d chat(s)", DAILY_TIME.strftime("%H:%M"),
             sum(1 for job in application.job_queue.jobs() if job.name.startswith("daily-")))
    # Check the tokens every hour (reads files only, never calls Google)
    application.job_queue.run_repeating(check_tokens, interval=3600, first=60, name="token-check")


# ---------------------------------------------------------------------------
# Reminders to renew the Google tokens (in Testing they expire after 7 days)
# ---------------------------------------------------------------------------

TOKEN_STATE_SHORT = {
    "ok": "✅ OK",
    "expiring": "⏰ Expiring soon",
    "expired": "⚠️ Expired",
    "missing": "⚠️ Missing",
    "unknown": "❔ Expiry unknown",
}


def _format_dt(when: dt.datetime) -> str:
    return _format_when(when.isoformat())


def token_alert_text(status: services.TokenStatus) -> str:
    """A reminder for one token, with renewal instructions."""
    if status.state == "expiring":
        title = f"⏰ <b>Access to {status.label} expires {_format_dt(status.expires_at)}</b>"
        why = "Google revokes it 7 days after approval because the app is in Testing. Best to renew now."
    elif status.state == "missing":
        title = f"⚠️ <b>No access to {status.label}</b>"
        why = f"The file {status.filename} wasn't found."
    else:
        title = f"⚠️ <b>Access to {status.label} has expired</b>"
        why = "Google no longer accepts the token - until it's renewed, this data isn't available."
    return "\n".join([
        title, why, "",
        "👇 Tap <b>Renew now</b> - it takes a minute, from your phone.",
        "",
        "<i>Or on your computer, in the project folder (with CLIENT_ID and CLIENT_SECRET set):</i>",
        f"<pre>Remove-Item {status.filename} -ErrorAction SilentlyContinue\n{status.renew_command}</pre>",
        f"<i>If the bot runs on a server, then copy {status.filename} to it (into the data folder).</i>",
    ])


def renew_button(key: str) -> InlineKeyboardButton:
    return InlineKeyboardButton(f"🔗 Renew now: {services.token_label(key)}", callback_data=f"renew:{key}")


def _alert_recipients(application: Application) -> set[int]:
    if ALLOWED_USERS:
        return set(ALLOWED_USERS)
    return {chat_id for chat_id, data in application.chat_data.items() if data.get("daily")}


async def check_tokens(context: ContextTypes.DEFAULT_TYPE) -> None:
    """Sends a reminder when a token is about to expire, expired or is missing - at most once a day per state."""
    sent = context.bot_data.setdefault("token_alerts", {})  # "health:expiring" -> date
    today = dt.date.today().isoformat()
    for status in services.token_statuses():
        if status.state not in ("expiring", "expired", "missing"):
            # OK - reset, so the next expiry alerts again
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
    """/tokens - Google access status and when it needs renewing."""
    lines = ["🔑 <b>Google access</b>", ""]
    statuses = services.token_statuses()
    for status in statuses:
        line = f"{TOKEN_STATE_SHORT[status.state]} - {status.label}"
        if status.expires_at and status.state in ("ok", "expiring"):
            line += f"\n    valid until {_format_dt(status.expires_at)}"
        lines.append(line)
    lines += ["", "<i>You can renew any time (e.g. both together, so they expire on the same day):</i>"]
    buttons = InlineKeyboardMarkup([[renew_button(s.key)] for s in statuses])
    await update.message.reply_text("\n".join(lines), parse_mode=ParseMode.HTML, reply_markup=buttons)


# ---------------------------------------------------------------------------
# Renewing a token from the phone: approval link -> the user pastes the address they landed on
# ---------------------------------------------------------------------------

async def renew_start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    query = update.callback_query
    if not _is_allowed(update):
        await query.answer("You're not allowed to do that", show_alert=True)
        return ConversationHandler.END
    await query.answer()
    key = query.data.split(":", 1)[1]
    result = await services.start_token_renewal(key)
    if not result.get("ok"):
        await query.message.reply_text(f"Couldn't create a link 😕\n<i>{html.escape(result.get('error', ''))}</i>",
                                       parse_mode=ParseMode.HTML)
        return ConversationHandler.END

    context.user_data["renew_key"] = key
    await query.message.reply_text(
        "\n".join([
            f"🔗 <b>Renewing access to {services.token_label(key)}</b>",
            "",
            "1. Tap the button below and approve access in Google.",
            "   (If you see \"Google hasn't verified this app\" - Advanced → Go to... )",
            "2. At the end the browser goes to a page that <b>doesn't load</b> (localhost) - that's fine.",
            "3. Copy the <b>full address</b> from the address bar and send it to me here.",
            "",
            "<i>The link is valid for 15 minutes. /cancel to cancel.</i>",
        ]),
        parse_mode=ParseMode.HTML,
        reply_markup=InlineKeyboardMarkup([[InlineKeyboardButton("🔐 Approve in Google", url=result["url"])]]),
    )
    return RENEW_WAIT


async def renew_code(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    key = context.user_data.get("renew_key")
    if not key:
        return ConversationHandler.END
    pasted = update.message.text.strip()
    if "code=" not in pasted and "error=" not in pasted:
        await update.message.reply_text(
            "That doesn't look like the right address. I need the full address of the page that didn't load - "
            "it starts with http://localhost:8080/callback and contains code=. Try again, or /cancel.")
        return RENEW_WAIT

    await update.message.reply_text("⏳ Saving the new access...")
    result = await services.finish_token_renewal(key, pasted)
    if not result.get("ok"):
        await update.message.reply_text(
            f"Renewal failed 😕\n<i>{html.escape(result.get('error', ''))}</i>\n\n"
            "You can paste it again, or get a new link with /tokens.", parse_mode=ParseMode.HTML)
        return RENEW_WAIT

    context.user_data.pop("renew_key", None)
    status = next(s for s in services.token_statuses() if s.key == key)
    until = f" - valid until {_format_dt(status.expires_at)}" if status.expires_at else ""
    await update.message.reply_text(f"✅ Access to {status.label} renewed{until}.")
    return ConversationHandler.END


async def renew_cancel(update: Update, context: ContextTypes.DEFAULT_TYPE) -> int:
    context.user_data.pop("renew_key", None)
    await update.message.reply_text("Cancelled. You can renew whenever you like with /tokens.")
    return ConversationHandler.END


# ---------------------------------------------------------------------------
# Moving today's workout on Google Calendar (/move or the button in the morning message)
# ---------------------------------------------------------------------------

def _is_allowed(update: Update) -> bool:
    return not ALLOWED_USERS or (update.effective_user is not None and update.effective_user.id in ALLOWED_USERS)


def _format_when(iso: str) -> str:
    when = dt.datetime.fromisoformat(iso).astimezone(TIMEZONE)
    return f"{when:%a %d/%m} at {when:%H:%M}"


def _describe_changes(result: dict) -> list[str]:
    lines = []
    for change in result.get("changes", []):
        summary = html.escape(change["summary"])
        if change.get("new_summary"):
            lines.append(f"• \"{summary}\" will be marked as superseded (your workout takes its place)")
        else:
            lines.append(f"• \"{summary}\": {_format_when(change['from'])} → {_format_when(change['to'])}")
    return lines


MUSCLE_GROUPS = {"A": "legs, back, biceps", "B": "chest, shoulders, triceps", "full": "full body"}


def _describe_replacement(replacement: dict) -> list[str]:
    """The short workout replacing today's workout (when the week is full): name, duration and link."""
    kind = html.escape(replacement["type"])
    if replacement.get("muscle_group"):
        kind += f" ({MUSCLE_GROUPS.get(replacement['muscle_group'], replacement['muscle_group'])})"
    return [
        f"🎬 <b>{html.escape(replacement['title'])}</b>",
        f"⏱ {replacement['duration_min']} min · {kind} · {html.escape(replacement['intensity'])} intensity",
        html.escape(replacement["url"]),
    ]


async def _preview_move(reply) -> None:
    """Shows what will change on the calendar and asks for confirmation. reply = a function that sends a message."""
    result = await services.move_todays_workout(dry_run=True)
    if not result.get("ok"):
        await reply(f"Couldn't move the workout 😕\n<i>{html.escape(result.get('error', ''))}</i>",
                    parse_mode=ParseMode.HTML)
        return

    replacement = result.get("replacement")
    if replacement:
        # The week is full - instead of moving, today's workout is replaced by a short one from the library
        confirm = "✅ Yes, replace it"
        text = "\n".join([
            f"📅 <b>Nowhere to move \"{html.escape(result['workout'])}\" - the week is full</b>",
            "", "Instead, I'll replace it on the calendar (same time) with a short workout:", "",
            *_describe_replacement(replacement),
        ])
    else:
        confirm = "✅ Yes, move it"
        text = "\n".join([f"📅 <b>Moving \"{html.escape(result['workout'])}\"</b>", "", "What will change on the calendar:",
                          *_describe_changes(result)])
    buttons = InlineKeyboardMarkup([[
        InlineKeyboardButton(confirm, callback_data="move_confirm"),
        InlineKeyboardButton("❌ Cancel", callback_data="move_cancel"),
    ]])
    await reply(text, parse_mode=ParseMode.HTML, reply_markup=buttons)


async def move(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    await update.message.reply_text("🔎 Checking the calendar...")
    await _preview_move(update.message.reply_text)


async def move_callback(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    query = update.callback_query
    if not _is_allowed(update):
        await query.answer("You're not allowed to do that", show_alert=True)
        return
    await query.answer()

    if query.data == "move":  # the button in the morning message
        await _preview_move(query.message.reply_text)
    elif query.data == "move_cancel":
        await query.edit_message_text("Cancelled - the calendar wasn't changed.")
    elif query.data == "move_confirm":
        await query.edit_message_text("⏳ Moving on the calendar...")
        # Recompute before applying, in case the calendar changed since the preview
        result = await services.move_todays_workout(dry_run=False)
        if result.get("ok") and result.get("replacement"):
            await query.edit_message_text("\n".join([
                f"✅ <b>\"{html.escape(result['workout'])}\" was replaced on the calendar with a short workout:</b>", "",
                *_describe_replacement(result["replacement"]),
            ]), parse_mode=ParseMode.HTML)
        elif result.get("ok"):
            await query.edit_message_text("\n".join(["✅ <b>Workout moved on the calendar</b>", "", *_describe_changes(result)]),
                                          parse_mode=ParseMode.HTML)
        else:
            await query.edit_message_text(f"Moving failed 😕\n<i>{html.escape(result.get('error', ''))}</i>",
                                          parse_mode=ParseMode.HTML)


async def on_error(update: object, context: ContextTypes.DEFAULT_TYPE) -> None:
    log.exception("Unhandled error", exc_info=context.error)
    if isinstance(update, Update) and update.effective_message:
        await update.effective_message.reply_text("Something went wrong on my end 😕 Try again in a moment.")


# ---------------------------------------------------------------------------
# Assembling the bot
# ---------------------------------------------------------------------------

def build_application(token: str) -> Application:
    persistence = PicklePersistence(filepath=os.getenv("BOT_STATE_FILE", "bot_state.pickle"))
    app = (Application.builder().token(token).persistence(persistence)
           .post_init(restore_daily_jobs).build())

    # Must be registered before the free-text handler, so replies inside /update aren't caught there
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
    # Token renewal: also before free text, so the pasted address reaches it
    app.add_handler(ConversationHandler(
        entry_points=[CallbackQueryHandler(renew_start, pattern="^renew:(health|calendar)$")],
        states={RENEW_WAIT: [MessageHandler(AUTH_FILTER & filters.TEXT & ~filters.COMMAND, renew_code)]},
        fallbacks=[CommandHandler("cancel", renew_cancel)],
        name="renew_conversation",
        persistent=True,
        conversation_timeout=15 * 60,  # same as the link's validity
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
        raise SystemExit("TELEGRAM_BOT_TOKEN is missing - see the setup instructions.")
    if not ALLOWED_USERS:
        log.warning("TELEGRAM_ALLOWED_USER_IDS is not set - the bot is open to everyone!")
    build_application(token).run_polling(allowed_updates=Update.ALL_TYPES)


if __name__ == "__main__":
    main()
