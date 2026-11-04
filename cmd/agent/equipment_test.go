package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoableWith(t *testing.T) {
	tests := []struct {
		name      string
		equipment []string
		available string
		want      bool
	}{
		{"not asked allows anything", []string{"gym"}, "", true},
		{"no list on no equipment", nil, equipmentNone, true},
		{"explicit none on no equipment", []string{"none"}, equipmentNone, true},
		{"dumbbells on no equipment", []string{"dumbbells"}, equipmentNone, false},
		{"gym on no equipment", []string{"gym"}, equipmentNone, false},
		{"no list with dumbbells", nil, equipmentDumbbells, true},
		{"dumbbells with dumbbells", []string{" Dumbbells "}, equipmentDumbbells, true},
		{"gym with dumbbells", []string{"gym"}, equipmentDumbbells, false},
		{"needs more than dumbbells", []string{"dumbbells", "bench"}, equipmentDumbbells, false},
		{"no list at the gym", nil, equipmentGym, true},
		{"dumbbells at the gym", []string{"dumbbells"}, equipmentGym, true},
		{"anything at the gym", []string{"bench", "cables"}, equipmentGym, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := shortWorkout{Equipment: tt.equipment}
			if got := w.doableWith(tt.available); got != tt.want {
				t.Errorf("doableWith(%q) = %v, want %v", tt.available, got, tt.want)
			}
		})
	}
}

// fallbackLibrary is written to a temp folder, which also becomes the working
// directory, so the fallback finds no token, recovery.json or history there.
func fallbackLibrary(t *testing.T, library []shortWorkout) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	data, err := json.Marshal(library)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "library.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHORT_WORKOUTS_FILE", path)
	t.Setenv("CLIENT_ID", "")
	t.Setenv("CLIENT_SECRET", "")
}

func TestFallBackRespectsEquipment(t *testing.T) {
	strength := func(title string, minutes int, equipment ...string) shortWorkout {
		return shortWorkout{Title: title, URL: "https://example.com/" + title, DurationMin: minutes,
			Type: "strength", Intensity: "medium", Equipment: equipment}
	}
	missed := workoutEvent("[A] Legs", 60)
	now := time.Now()

	fallbackLibrary(t, []shortWorkout{
		strength("machines", 40, "gym"),
		strength("weights", 30, "dumbbells"),
		strength("bodyweight", 20),
	})
	for _, tt := range []struct{ equipment, want string }{
		{"", "machines"},
		{equipmentGym, "machines"},
		{equipmentDumbbells, "weights"},
		{equipmentNone, "bodyweight"},
	} {
		pick, _, err := fallBackToShortWorkout(nil, missed, errWeekFull, now, true, tt.equipment)
		if err != nil {
			t.Fatalf("equipment %q: %v", tt.equipment, err)
		}
		if pick.Title != tt.want {
			t.Errorf("equipment %q: picked %q, want %q", tt.equipment, pick.Title, tt.want)
		}
	}

	// Nothing fits: says why and keeps the week-full error.
	fallbackLibrary(t, []shortWorkout{strength("machines", 40, "gym")})
	pick, _, err := fallBackToShortWorkout(nil, missed, errWeekFull, now, true, equipmentNone)
	if pick != nil || !errors.Is(err, errWeekFull) || !strings.Contains(err.Error(), "this equipment (none)") {
		t.Errorf("pick %v, err %v: want no pick and an equipment reason", pick, err)
	}

	// An empty library still reports that, not an equipment problem.
	fallbackLibrary(t, []shortWorkout{})
	_, _, err = fallBackToShortWorkout(nil, missed, errWeekFull, now, true, equipmentNone)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("err = %v, want the library to be reported as empty", err)
	}
}

func TestMoveRejectsUnknownEquipment(t *testing.T) {
	result := moveTodaysWorkout(time.Now(), true, "bands")
	if result.Err == nil || !strings.Contains(result.Err.Error(), "unknown equipment") {
		t.Errorf("err = %v, want an unknown equipment error", result.Err)
	}
}

func TestMoveJSONListsEquipment(t *testing.T) {
	plan := &reschedulePlan{Missed: workoutEvent("[A] Legs", 60)}
	decode := func(w shortWorkout) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		if err := writeMoveJSON(&buf, moveResult{DryRun: true, Plan: plan, Replacement: &w}); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Replacement map[string]any `json:"replacement"`
		}
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Replacement
	}

	got, ok := decode(shortWorkout{Equipment: []string{"Dumbbells", "none"}})["equipment"].([]any)
	if !ok || len(got) != 1 || got[0] != "dumbbells" {
		t.Errorf("equipment = %v, want [dumbbells]", got)
	}
	got, ok = decode(shortWorkout{})["equipment"].([]any)
	if !ok || len(got) != 0 {
		t.Errorf("equipment = %v, want an empty list", got)
	}
}

func TestShortWorkoutDescriptionNamesEquipment(t *testing.T) {
	original := workoutEvent("[A] Legs", 60)
	if got := shortWorkoutDescription(shortWorkout{Type: "strength", Equipment: []string{"dumbbells"}}, original); !strings.Contains(got, "Equipment: dumbbells") {
		t.Errorf("description doesn't name the equipment: %s", got)
	}
	if got := shortWorkoutDescription(shortWorkout{Type: "strength"}, original); strings.Contains(got, "Equipment") {
		t.Errorf("description of a bodyweight video mentions equipment: %s", got)
	}
}
