package main

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

// workoutEvent is a calendar event of the given length starting at testNow.
func workoutEvent(summary string, minutes int) CalendarEvent {
	return CalendarEvent{ID: "today", Summary: summary, Start: testNow, End: testNow.Add(time.Duration(minutes) * time.Minute)}
}

func video(title, typ, intensity string, minutes int) shortWorkout {
	return shortWorkout{Title: title, URL: "https://youtu.be/" + title, DurationMin: minutes, Type: typ, Intensity: intensity}
}

func TestSelectShortWorkout(t *testing.T) {
	good, low := 80, 49
	threshold := lowReadinessThreshold
	library := []shortWorkout{
		video("strength-20", "strength", "medium", 20),
		video("strength-30-high", "strength", "high", 30),
		video("run-25", "run", "medium", 25),
		video("hiit-40", "hiit", "high", 40),
		video("mobility-15", "mobility", "low", 15),
	}
	tests := []struct {
		name      string
		library   []shortWorkout
		history   []shortWorkoutPick
		original  CalendarEvent
		readiness *int
		want      string
	}{
		{"same type, longest that fits", library, nil, workoutEvent("אימון גב", 60), &good, "strength-30-high"},
		{"only shorter than the original", library, nil, workoutEvent("אימון גב", 25), &good, "strength-20"},
		// run-25 is as long as the run itself, so no run is left: any type.
		{"equal length doesn't count", library, nil, workoutEvent("ריצה", 25), &good, "strength-20"},
		{"run maps to run", library, nil, workoutEvent("ריצה", 45), &good, "run-25"},
		{"no same type: any type, longest", []shortWorkout{library[3], library[4]}, nil, workoutEvent("[A] רגליים", 60), &good, "hiit-40"},
		{"low readiness: no high intensity", library, nil, workoutEvent("אימון חזה", 60), &low, "strength-20"},
		{"no readiness: no high intensity", library, nil, workoutEvent("אימון חזה", 60), nil, "strength-20"},
		{"readiness exactly 50 allows high", library, nil, workoutEvent("אימון חזה", 60), &threshold, "strength-30-high"},
		{"picked 6 days ago is skipped", library,
			[]shortWorkoutPick{{URL: library[1].URL, PickedAt: testNow.Add(-6 * 24 * time.Hour)}},
			workoutEvent("אימון גב", 60), &good, "strength-20"},
		{"picked 8 days ago is allowed again", library,
			[]shortWorkoutPick{{URL: library[1].URL, PickedAt: testNow.Add(-8 * 24 * time.Hour)}},
			workoutEvent("אימון גב", 60), &good, "strength-30-high"},
		{"videos without a url are skipped",
			[]shortWorkout{{Title: "no-url", DurationMin: 20, Type: "strength", Intensity: "low"}, library[4]},
			nil, workoutEvent("אימון גב", 60), &good, "mobility-15"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := selectShortWorkout(tt.library, tt.history, tt.original, tt.readiness, testNow)
			if got == nil {
				t.Fatalf("got nothing (%s), want %q", reason, tt.want)
			}
			if got.Title != tt.want {
				t.Errorf("got %q, want %q", got.Title, tt.want)
			}
		})
	}
}

func groupVideo(title, group string, minutes int) shortWorkout {
	w := video(title, "strength", "medium", minutes)
	w.MuscleGroup = group
	return w
}

func TestSelectShortWorkoutMuscleGroups(t *testing.T) {
	good := 80
	backDay := workoutEvent("אימון גב", 60)   // A
	chestDay := workoutEvent("אימון חזה", 60) // B
	tests := []struct {
		name     string
		library  []shortWorkout
		original CalendarEvent
		want     string
	}{
		{"same group wins over a longer full-body video",
			[]shortWorkout{groupVideo("A-20", "A", 20), groupVideo("full-30", "full", 30), groupVideo("B-40", "B", 40)}, backDay, "A-20"},
		{"same group for B",
			[]shortWorkout{groupVideo("A-20", "A", 20), groupVideo("B-25", "b", 25)}, chestDay, "B-25"},
		{"no same group: full body",
			[]shortWorkout{groupVideo("B-40", "B", 40), groupVideo("full-15", "full", 15)}, backDay, "full-15"},
		{"no group set counts as full body",
			[]shortWorkout{groupVideo("B-40", "B", 40), groupVideo("unset-15", "", 15)}, backDay, "unset-15"},
		{"other types before the other muscle group",
			[]shortWorkout{groupVideo("B-40", "B", 40), video("mobility-15", "mobility", "low", 15)}, backDay, "mobility-15"},
		{"bare workout: full body first",
			[]shortWorkout{groupVideo("A-35", "A", 35), groupVideo("full-15", "full", 15), video("hiit-40", "hiit", "medium", 40)},
			workoutEvent("אימון", 60), "full-15"},
		{"bare workout without full body: any strength video, longest",
			[]shortWorkout{groupVideo("A-20", "A", 20), groupVideo("B-35", "B", 35), video("hiit-40", "hiit", "medium", 40)},
			workoutEvent("אימון", 60), "B-35"},
		{"a run may get any strength video",
			[]shortWorkout{groupVideo("B-30", "B", 30), video("mobility-15", "mobility", "low", 15)},
			workoutEvent("ריצה", 45), "B-30"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := selectShortWorkout(tt.library, nil, tt.original, &good, testNow)
			if got == nil {
				t.Fatalf("got nothing (%s), want %q", reason, tt.want)
			}
			if got.Title != tt.want {
				t.Errorf("got %q, want %q", got.Title, tt.want)
			}
		})
	}

	got, reason := selectShortWorkout([]shortWorkout{groupVideo("B-40", "B", 40)}, nil, backDay, &good, testNow)
	if got != nil || !strings.Contains(reason, "other muscle groups") {
		t.Errorf("only an other-group video: want nothing with a clear reason, got %v (%q)", got, reason)
	}
}

func TestShortWorkoutDescriptionNamesMuscleGroup(t *testing.T) {
	desc := shortWorkoutDescription(groupVideo("A-20", "A", 20), workoutEvent("אימון גב", 60))
	if !strings.Contains(desc, "רגליים, גב, יד קדמית") {
		t.Errorf("description should name the muscle group: %s", desc)
	}
}

func TestSelectShortWorkoutNothingFits(t *testing.T) {
	low := 30
	tests := []struct {
		name       string
		library    []shortWorkout
		history    []shortWorkoutPick
		readiness  *int
		wantReason string
	}{
		{"empty library", nil, nil, nil, "empty"},
		{"no urls", []shortWorkout{{Title: "x", DurationMin: 10, Type: "strength", Intensity: "low"}}, nil, nil, "has a url"},
		{"all too long", []shortWorkout{video("long", "strength", "low", 45)}, nil, nil, "shorter than"},
		{"all high intensity on a low day", []shortWorkout{video("hiit", "hiit", "high", 20)}, nil, &low, "readiness is low (30)"},
		{"all high intensity with no score", []shortWorkout{video("hiit", "hiit", "high", 20)}, nil, nil, "no readiness score"},
		{"all used this week", []shortWorkout{video("used", "strength", "low", 20)},
			[]shortWorkoutPick{{URL: "https://youtu.be/used", PickedAt: testNow.Add(-24 * time.Hour)}}, nil, "last 7 days"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := selectShortWorkout(tt.library, tt.history, workoutEvent("אימון", 45), tt.readiness, testNow)
			if got != nil {
				t.Fatalf("want nothing, got %q", got.Title)
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason %q should mention %q", reason, tt.wantReason)
			}
		})
	}
}

func TestApplyShortWorkoutKeepsTimeSlotAndRecordsHistory(t *testing.T) {
	cal := &fakeCalendar{}
	historyPath := filepath.Join(t.TempDir(), "history.json")
	original := workoutEvent("אימון גב", 60)
	w := video("strength-20", "strength", "medium", 20)

	if err := applyShortWorkout(&http.Client{Transport: cal}, original, w, historyPath, testNow); err != nil {
		t.Fatalf("applyShortWorkout: %v", err)
	}
	if len(cal.patches) != 1 {
		t.Fatalf("want one calendar update, got %v", cal.patches)
	}
	patch := cal.patches[0]
	for _, want := range []string{`"summary":"strength-20"`, w.URL, "אימון גב"} {
		if !strings.Contains(patch, want) {
			t.Errorf("calendar update should contain %q: %s", want, patch)
		}
	}
	if strings.Contains(patch, `"start"`) || strings.Contains(patch, `"end"`) {
		t.Errorf("the time slot must not change: %s", patch)
	}

	history, err := loadShortWorkoutHistory(historyPath)
	if err != nil || len(history) != 1 || history[0].URL != w.URL || !history[0].PickedAt.Equal(testNow) {
		t.Errorf("history = %+v, err %v", history, err)
	}
}

func TestSaveShortWorkoutHistoryDropsOldPicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	history := []shortWorkoutPick{
		{URL: "old", PickedAt: testNow.Add(-40 * 24 * time.Hour)},
		{URL: "recent", PickedAt: testNow.Add(-2 * 24 * time.Hour)},
	}
	if err := saveShortWorkoutHistory(path, history, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := loadShortWorkoutHistory(path)
	if err != nil || len(got) != 1 || got[0].URL != "recent" {
		t.Errorf("got %+v, err %v", got, err)
	}
	if missing, err := loadShortWorkoutHistory(filepath.Join(t.TempDir(), "none.json")); err != nil || missing != nil {
		t.Errorf("a missing history file should be empty, got %+v, err %v", missing, err)
	}
}

func TestPlanRescheduleReportsWeekFull(t *testing.T) {
	loc, _ := time.LoadLocation(calendarTimeZone)
	at := func(month, d, h int) time.Time { return time.Date(2026, time.Month(month), d, h, 0, 0, 0, loc) }
	// Every day from today (Tue) to Saturday has a workout, and no later
	// workout is a shorter [A] - nothing can be moved or superseded.
	cal := &fakeCalendar{events: []calendarEventJSON{
		calendarEvent("today", "אימון גב", at(9, 29, 18), 60),
		calendarEvent("b1", "אימון חזה", at(9, 30, 18), 60),
		calendarEvent("run", "ריצה", at(10, 1, 7), 45),
		calendarEvent("a2", "אימון רגליים", at(10, 2, 18), 90),
		calendarEvent("b2", "אימון כתפיים", at(10, 3, 18), 60),
	}}
	plan, err := planReschedule(&http.Client{Transport: cal}, at(9, 29, 9), nil)
	if !errors.Is(err, errWeekFull) {
		t.Fatalf("want errWeekFull, got %v", err)
	}
	if plan == nil || plan.Missed.ID != "today" {
		t.Fatalf("the plan should still name today's workout, got %+v", plan)
	}
}
