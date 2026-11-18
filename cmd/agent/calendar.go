package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	calendarTimeZone = "Asia/Jerusalem"
	// How long -json waits for today's workout before using generic wording.
	calendarLookupTimeout = 15 * time.Second
)

// CalendarEvent is a minimal representation of a Google Calendar event -
// enough to identify, read, and reschedule a workout.
type CalendarEvent struct {
	ID      string
	Summary string
	Start   time.Time
	End     time.Time
}

type calendarEventTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type calendarEventJSON struct {
	ID      string            `json:"id"`
	Summary string            `json:"summary"`
	Start   calendarEventTime `json:"start"`
	End     calendarEventTime `json:"end"`
}

// workoutCalendarID returns the calendar that holds workouts. Set
// WORKOUT_CALENDAR_ID to a dedicated workouts calendar to make every event in
// it count as a workout; otherwise the primary calendar is used and only
// tagged events count (see workoutTagPattern).
func workoutCalendarID() string {
	return firstNonEmpty(os.Getenv("WORKOUT_CALENDAR_ID"), "primary")
}

func usingDedicatedWorkoutCalendar() bool {
	return os.Getenv("WORKOUT_CALENDAR_ID") != ""
}

// workoutTagPattern identifies workouts by an explicit tag at the very start
// of the event title, mirroring the weekly split:
//
//	[A]   - legs, back, biceps, abs          (Sun, and again Wed)
//	[B]   - chest, shoulders, triceps, abs   (Mon, and again Thu)
//	[RUN] - running                          (Tue, usually Sat)
//
// e.g. "[A] Legs, back, biceps & abs". Free-text keyword matching anywhere in
// the title was removed: substrings like "run" (Brunch) or " a " (Plan a
// trip) turned ordinary events into "workouts" that then got moved around
// the calendar.
var workoutTagPattern = regexp.MustCompile(`(?i)^\s*\[(A|B|RUN)\]`)

// namedWorkoutPattern also accepts untagged titles, but only when the title
// STARTS with the whole word "Workout" or "Run" ("Workout back", "Run"). The
// words after "Workout" pick the group (see groupWords); a bare "Workout" is
// a generic gym session.
var namedWorkoutPattern = regexp.MustCompile(`(?i)^\s*(workout|run)(?:[\s,.:\-]+(.*))?$`)

// groupWords maps whole words (lowercase) in a "Workout ..." title to the
// split.
var groupWords = map[string]string{
	"legs": "A", "leg": "A", "back": "A", "biceps": "A",
	"chest": "B", "shoulders": "B", "shoulder": "B", "triceps": "B",
}

// supersededPrefix is prepended to a workout's title when the reschedule
// fallback replaces it with the missed session; such events stop counting
// as workouts.
const supersededPrefix = "[SUPERSEDED] "

// categorizeWorkout returns "A", "B" or "run" for a tagged title or a
// "Workout ..."/"Run" title, "gym" for a bare "Workout", or "" when the
// title isn't a workout.
func categorizeWorkout(summary string) string {
	if m := workoutTagPattern.FindStringSubmatch(summary); m != nil {
		if strings.EqualFold(m[1], "RUN") {
			return "run"
		}
		return strings.ToUpper(m[1])
	}
	m := namedWorkoutPattern.FindStringSubmatch(summary)
	if m == nil {
		return ""
	}
	if strings.EqualFold(m[1], "run") {
		return "run"
	}
	words := strings.FieldsFunc(strings.ToLower(m[2]), func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '-' || r == '+' || r == '/' || r == '&'
	})
	for _, word := range words {
		if group, ok := groupWords[word]; ok {
			return group
		}
	}
	return "gym"
}

func isWorkoutEvent(summary string) bool {
	if strings.HasPrefix(strings.TrimSpace(summary), strings.TrimSpace(supersededPrefix)) {
		return false
	}
	if usingDedicatedWorkoutCalendar() {
		return true
	}
	return categorizeWorkout(summary) != ""
}

// isOutdoorWorkout reports whether a workout category is affected by the
// weather. Gym sessions (A/B) happen indoors.
func isOutdoorWorkout(category string) bool {
	return category == "run"
}

func parseCalendarTime(t calendarEventTime) (time.Time, error) {
	if t.DateTime == "" {
		return time.Time{}, fmt.Errorf("event has no dateTime (likely an all-day event, which this feature ignores)")
	}
	return time.Parse(time.RFC3339, t.DateTime)
}

// listCalendarEvents fetches events (already expanded from any recurring
// series, via singleEvents) in [timeMin, timeMax] from the user's primary
// calendar, ordered by start time.
func listCalendarEvents(client *http.Client, timeMin, timeMax time.Time) ([]CalendarEvent, error) {
	params := url.Values{}
	params.Set("timeMin", timeMin.Format(time.RFC3339))
	params.Set("timeMax", timeMax.Format(time.RFC3339))
	params.Set("singleEvents", "true")
	params.Set("orderBy", "startTime")
	endpoint := "https://www.googleapis.com/calendar/v3/calendars/" + url.PathEscape(workoutCalendarID()) + "/events?" + params.Encode()

	var response struct {
		Items []calendarEventJSON `json:"items"`
	}
	if err := getJSON(client, endpoint, &response); err != nil {
		return nil, fmt.Errorf("list calendar events: %w", err)
	}

	events := make([]CalendarEvent, 0, len(response.Items))
	for _, item := range response.Items {
		start, err := parseCalendarTime(item.Start)
		if err != nil {
			continue // skip all-day / malformed events
		}
		end, err := parseCalendarTime(item.End)
		if err != nil {
			continue
		}
		events = append(events, CalendarEvent{ID: item.ID, Summary: item.Summary, Start: start, End: end})
	}
	return events, nil
}

// patchCalendarEvent applies a partial update to an event in the workout
// calendar; fields not in payload are left untouched.
func patchCalendarEvent(client *http.Client, eventID string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event update: %w", err)
	}
	endpoint := "https://www.googleapis.com/calendar/v3/calendars/" + url.PathEscape(workoutCalendarID()) +
		"/events/" + url.PathEscape(eventID)
	req, err := http.NewRequest(http.MethodPatch, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update calendar event returned status %d: %s", resp.StatusCode, responseBody)
	}
	return nil
}

// updateCalendarEventTime moves an existing event to a new start/end time,
// keeping all other fields untouched.
func updateCalendarEventTime(client *http.Client, eventID string, newStart, newEnd time.Time, loc *time.Location) error {
	return patchCalendarEvent(client, eventID, map[string]any{
		"start": map[string]string{"dateTime": newStart.In(loc).Format(time.RFC3339), "timeZone": loc.String()},
		"end":   map[string]string{"dateTime": newEnd.In(loc).Format(time.RFC3339), "timeZone": loc.String()},
	})
}

// weekBounds returns the Sunday 00:00:00 -> Saturday 23:59:59 window (in loc)
// that contains day. Matches the Israeli calendar week, consistent with
// calendarTimeZone.
func weekBounds(loc *time.Location, day time.Time) (time.Time, time.Time) {
	d := day.In(loc)
	dayStart := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	offset := int(dayStart.Weekday()) // Sunday = 0 ... Saturday = 6
	weekStart := dayStart.AddDate(0, 0, -offset)
	weekEndDay := weekStart.AddDate(0, 0, 6)
	weekEnd := time.Date(weekEndDay.Year(), weekEndDay.Month(), weekEndDay.Day(), 23, 59, 59, 0, loc)
	return weekStart, weekEnd
}

func sameDay(a, b time.Time, loc *time.Location) bool {
	a, b = a.In(loc), b.In(loc)
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

// todaysWorkoutCategory returns the categorizeWorkout result of the first
// workout on now's day, or "" if the day has none (the same workout
// -move-today picks).
func todaysWorkoutCategory(client *http.Client, now time.Time) (string, error) {
	loc, err := time.LoadLocation(calendarTimeZone)
	if err != nil {
		return "", fmt.Errorf("load calendar timezone: %w", err)
	}
	day := now.In(loc)
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	events, err := listCalendarEvents(client, dayStart, dayStart.AddDate(0, 0, 1))
	if err != nil {
		return "", err
	}
	for _, e := range events {
		if isWorkoutEvent(e.Summary) && sameDay(e.Start, day, loc) {
			return categorizeWorkout(e.Summary), nil
		}
	}
	return "", nil
}

// lookupTodaysWorkoutCategory is todaysWorkoutCategory with the real calendar.
// It's only an input to the wording of the adjustment, so any failure is
// logged and treated as an unknown workout. Never opens a browser: it only
// runs in -json mode, where interactiveAuth is off.
func lookupTodaysWorkoutCategory(now time.Time) string {
	config, err := googleOAuthConfig(googleCalendarScopes...)
	if err != nil {
		fmt.Printf("Could not read today's workout: %v\n", err)
		return ""
	}
	client, err := googleClient(context.Background(), config, calendarTokenFile)
	if err != nil {
		fmt.Printf("Could not read today's workout: %v\n", err)
		return ""
	}
	// A slow calendar must not cost the bot its recovery data and verdict.
	client.Timeout = calendarLookupTimeout
	category, err := todaysWorkoutCategory(client, now)
	if err != nil {
		fmt.Printf("Could not read today's workout: %v\n", err)
		return ""
	}
	return category
}
