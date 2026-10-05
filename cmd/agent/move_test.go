package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeCalendar serves a fixed list of events and records PATCH requests.
type fakeCalendar struct {
	events  []calendarEventJSON
	patches []string
}

func (f *fakeCalendar) RoundTrip(r *http.Request) (*http.Response, error) {
	body := "{}"
	switch r.Method {
	case http.MethodGet:
		b, _ := json.Marshal(map[string]any{"items": f.events})
		body = string(b)
	case http.MethodPatch:
		b, _ := io.ReadAll(r.Body)
		f.patches = append(f.patches, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]+" "+string(b))
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func calendarEvent(id, summary string, start time.Time, minutes int) calendarEventJSON {
	return calendarEventJSON{
		ID:      id,
		Summary: summary,
		Start:   calendarEventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendarEventTime{DateTime: start.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)},
	}
}

func TestPlanRescheduleCascade(t *testing.T) {
	loc, _ := time.LoadLocation(calendarTimeZone)
	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, loc) } // Sep 27 2026 is a Sunday
	cal := &fakeCalendar{events: []calendarEventJSON{
		calendarEvent("run", "[RUN] ריצה", day(29, 7), 60),   // Tue - today
		calendarEvent("a", "[A] רגליים", day(30, 18), 90),    // Wed - in the way
		calendarEvent("x", "פגישה עם רופא", day(30, 12), 30), // not a workout
	}}
	client := &http.Client{Transport: cal}

	plan, err := planReschedule(client, day(29, 9), nil)
	if err != nil {
		t.Fatalf("planReschedule: %v", err)
	}
	if plan.Superseded || len(plan.Changes) != 2 {
		t.Fatalf("want a 2-step cascade, got %+v", plan)
	}
	if got := plan.Changes[0]; got.Event.ID != "run" || !got.NewStart.Equal(day(30, 7)) {
		t.Errorf("run should move to Wed 07:00, got %s -> %s", got.Event.ID, got.NewStart)
	}
	if got := plan.Changes[1]; got.Event.ID != "a" || !got.NewStart.Equal(time.Date(2026, 10, 1, 18, 0, 0, 0, loc)) {
		t.Errorf("[A] should move to Thu 18:00, got %s -> %s", got.Event.ID, got.NewStart)
	}
	if len(cal.patches) != 0 {
		t.Fatalf("planning must not touch the calendar, got %v", cal.patches)
	}

	if err := applyReschedulePlan(client, plan); err != nil {
		t.Fatalf("applyReschedulePlan: %v", err)
	}
	if len(cal.patches) != 2 {
		t.Errorf("want 2 calendar updates, got %v", cal.patches)
	}
}

func TestPlanRescheduleSupersedesWhenWeekIsFull(t *testing.T) {
	loc, _ := time.LoadLocation(calendarTimeZone)
	at := func(month, d, h int) time.Time { return time.Date(2026, time.Month(month), d, h, 0, 0, 0, loc) }
	cal := &fakeCalendar{events: []calendarEventJSON{
		calendarEvent("a-long", "[A] גב ורגליים", at(9, 29, 18), 90), // Tue - today
		calendarEvent("b1", "[B] חזה", at(9, 30, 18), 60),
		calendarEvent("run", "[RUN] ריצה", at(10, 1, 7), 45),
		calendarEvent("a-short", "[A] רגליים קצר", at(10, 2, 10), 45), // Fri - shorter [A]
		calendarEvent("b2", "[B] כתפיים", at(10, 3, 18), 60),          // Sat - week is full
	}}
	plan, err := planReschedule(&http.Client{Transport: cal}, at(9, 29, 9), nil)
	if err != nil {
		t.Fatalf("planReschedule: %v", err)
	}
	if !plan.Superseded || len(plan.Changes) != 2 {
		t.Fatalf("want the supersede fallback, got %+v", plan)
	}
	move, rename := plan.Changes[0], plan.Changes[1]
	if move.Event.ID != "a-long" || !move.NewStart.Equal(at(10, 2, 10)) || move.NewEnd.Sub(move.NewStart) != 90*time.Minute {
		t.Errorf("long [A] should take Fri 10:00 and keep 90 minutes, got %+v", move)
	}
	if rename.Event.ID != "a-short" || rename.NewSummary != supersededPrefix+"[A] רגליים קצר" {
		t.Errorf("short [A] should be marked superseded, got %+v", rename)
	}
}

func TestCategorizeWorkout(t *testing.T) {
	tests := map[string]string{
		"[A] רגליים גב":        "A",
		"[run] easy":           "run",
		"אימון חזה":            "B",
		"אימון גב":             "A",
		"אימון רגליים וכתפיים": "A", // the first group word wins
		"אימון יד אחורית":      "B",
		"אימון":                "gym",
		"אימון כוח":            "gym",
		"ריצה":                 "run",
		"  ריצה - 5 ק\"מ":      "run",
		// Not workouts: the word must open the title, as a whole word.
		"גבינה ויין":                "",
		"פגישה על אימון":            "",
		"אימונים של הילדים":         "",
		"ריצות בוקר עם חברים":       "",
		"Brunch":                    "",
		supersededPrefix + "[A] גב": "",
	}
	for title, want := range tests {
		got := categorizeWorkout(title)
		if title == supersededPrefix+"[A] גב" {
			if isWorkoutEvent(title) {
				t.Errorf("a superseded workout must not count as a workout")
			}
			continue
		}
		if got != want {
			t.Errorf("categorizeWorkout(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestPlanRescheduleNoWorkoutToday(t *testing.T) {
	loc, _ := time.LoadLocation(calendarTimeZone)
	cal := &fakeCalendar{}
	_, err := planReschedule(&http.Client{Transport: cal}, time.Date(2026, 9, 29, 9, 0, 0, 0, loc), nil)
	if err == nil || !strings.Contains(err.Error(), "no workout found") {
		t.Errorf("want a 'no workout found' error, got %v", err)
	}
}
