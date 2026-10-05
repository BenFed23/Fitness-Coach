# העלאה לשרת בענן

הבוט רץ בקונטיינר Docker אחד: הבוט ב-Python + האג'נט ב-Go.
כל בוקר (DAILY_TIME) הוא שולח בטלגרם אם מותר להתאמן היום, ו-`/move` (או הכפתור בהודעה) מזיז את האימון של היום ביומן.

## 1. במחשב שלך - פעם אחת: טוקנים של גוגל

השרת לא יכול לפתוח דפדפן, אז יוצרים את הטוקנים במחשב ומעתיקים אותם.
מתיקיית הפרויקט, עם `CLIENT_ID` ו-`CLIENT_SECRET` מוגדרים:

```powershell
go build -o fitness-agent.exe ./cmd/agent
.\fitness-agent.exe                          # יוצר token_health.json (אם אין)
.\fitness-agent.exe -move-today -dry-run     # יוצר token_calendar.json - לא משנה כלום ביומן
```

## 2. ⚠️ חובה: להוציא את אפליקציית ה-OAuth ממצב Testing

כל עוד ב-Google Cloud Console ← **OAuth consent screen** ← **Audience** הסטטוס הוא **Testing**,
גוגל מבטלת את הטוקנים **אחרי 7 ימים** - והבוט בשרת יפסיק לקבל נתונים.
לוחצים **Publish app** (מעבר ל-In production). לשימוש אישי אין צורך באימות של גוגל;
במסך ההרשאה יופיע "Google hasn't verified this app" - ממשיכים דרך Advanced.
אחרי הפרסום צריך ליצור את הטוקנים מחדש (למחוק את שני קבצי ה-token ולחזור על שלב 1).

## 3. שרת

כל VPS עם Linux מתאים, למשל:
- **Google Cloud** - מכונת `e2-micro` ב-us-central1/us-west1/us-east1 (בחינם במסגרת Free Tier)
- **Hetzner** - CX22, כ-4€ לחודש

על השרת (Ubuntu):

```bash
curl -fsSL https://get.docker.com | sh
mkdir -p ~/fitness-agent/data
```

## 4. העתקת הפרויקט והטוקנים

מהמחשב שלך (PowerShell, מתיקיית הפרויקט; מחליפים `user@server`):

```powershell
scp -r Dockerfile docker-compose.yml .dockerignore go.mod go.sum cmd configs telegram_bot user@server:~/fitness-agent/
scp token_health.json token_calendar.json user@server:~/fitness-agent/data/
scp .env.example user@server:~/fitness-agent/.env
```

על השרת - ממלאים את `~/fitness-agent/.env` (טוקן טלגרם, ה-ID שלך, CLIENT_ID/SECRET, מיקום):

```bash
nano ~/fitness-agent/.env
chmod 600 ~/fitness-agent/.env ~/fitness-agent/data/*.json
```

## 5. הפעלה

```bash
cd ~/fitness-agent
docker compose up -d --build
docker compose logs -f          # אמור להופיע: Morning update at 07:00 for 1 chat(s)
```

שולחים לבוט `/start` ו-`/status` - המקור צריך להיות "Fitbit (Google Health)".

## עדכון גרסה

מעתיקים שוב את הקבצים שהשתנו ומריצים `docker compose up -d --build`.
הטוקנים ומצב הבוט נשמרים ב-`data/` ולא נמחקים.

## מאגר האימונים הקצרים

כשמבקשים להזיז אימון והשבוע מלא, הסוכן מחליף את האימון של היום (באותה שעה) בסרטון מ-`configs/short_workouts.json`.
את הקובץ ממלאים ידנית; בשרת עורכים את `~/fitness-agent/configs/short_workouts.json` והשינוי נכנס לתוקף מיד, בלי לבנות מחדש.
ההיסטוריה (כדי לא לחזור על סרטון תוך 7 ימים) נשמרת ב-`data/short_workout_history.json`.

## כשמשהו לא עובד

- **"אין גישה לנתוני הצמיד" בהודעות** - בדרך כלל טוקן שפג: חוזרים על שלב 1 במחשב ומעתיקים את הקובץ ל-`data/`.
- **`/move` אומר שאין אימון היום** - ביומן הראשי נחשבים רק אירועים שהכותרת שלהם מתחילה ב-`[A]`, `[B]` או `[RUN]`
  (או מגדירים `WORKOUT_CALENDAR_ID` ליומן אימונים נפרד).
- **מזג האוויר של עיר אחרת** - חסרים `AGENT_LATITUDE` / `AGENT_LONGITUDE` ב-`.env`.
