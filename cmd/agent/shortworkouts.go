package main

// ---------------------------------------------------------------------
// Short-workout fallback for -move-today.
//
// When today's workout can't be moved (errWeekFull: the week is full and
// there's no shorter same-group workout to replace), today's calendar event
// is replaced in place by a short YouTube workout from a library the user
// maintains by hand in configs/short_workouts.json. The event keeps its
// original time slot; only its title and description change.
// ---------------------------------------------------------------------

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	defaultShortWorkoutsFile = "configs/short_workouts.json"
	// The history lives in the working directory, next to the tokens (on
	// the server that's the /data volume). It's in .gitignore.
	shortWorkoutHistoryFile = "short_workout_history.json"
	// A video picked within this window isn't picked again.
	shortWorkoutRepeatWindow = 7 * 24 * time.Hour
	// History older than this is dropped when the file is saved.
	shortWorkoutHistoryKeep = 30 * 24 * time.Hour
	// Below this readiness score (or with no score at all) only low and
	// medium intensity videos are picked.
	lowReadinessThreshold = 50
)

// shortWorkout is one video in configs/short_workouts.json.
type shortWorkout struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	DurationMin int    `json:"duration_min"`
	Type        string `json:"type"`      // run, strength, mobility, hiit...
	Intensity   string `json:"intensity"` // low, medium or high
	// MuscleGroup is for strength videos and follows the calendar's split:
	// "A" (legs, back, biceps), "B" (chest, shoulders, triceps) or "full"
	// (full body). Empty counts as full body.
	MuscleGroup string `json:"muscle_group,omitempty"`
}

// muscleGroup returns w's muscle group, "full" when unset.
func (w shortWorkout) muscleGroup() string {
	group := strings.ToUpper(strings.TrimSpace(w.MuscleGroup))
	switch group {
	case "A", "B":
		return group
	default:
		return "full"
	}
}

func (w shortWorkout) isStrength() bool {
	return strings.EqualFold(strings.TrimSpace(w.Type), "strength")
}

// shortWorkoutPick is one entry of the history file.
type shortWorkoutPick struct {
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	PickedAt time.Time `json:"picked_at"`
}

func shortWorkoutsPath() string {
	return firstNonEmpty(os.Getenv("SHORT_WORKOUTS_FILE"), defaultShortWorkoutsFile)
}

func loadShortWorkouts(path string) ([]shortWorkout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read short workout library: %w", err)
	}
	var library []shortWorkout
	if err := json.Unmarshal(data, &library); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return library, nil
}

// loadShortWorkoutHistory returns the picks saved in path; a missing file
// is an empty history.
func loadShortWorkoutHistory(path string) ([]shortWorkoutPick, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read short workout history: %w", err)
	}
	var history []shortWorkoutPick
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return history, nil
}

// saveShortWorkoutHistory writes history to path, dropping old entries.
func saveShortWorkoutHistory(path string, history []shortWorkoutPick, now time.Time) error {
	kept := []shortWorkoutPick{}
	for _, p := range history {
		if now.Sub(p.PickedAt) < shortWorkoutHistoryKeep {
			kept = append(kept, p)
		}
	}
	data, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return fmt.Errorf("encode short workout history: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	return nil
}

// libraryTypeForCategory maps a calendar workout category (see
// categorizeWorkout) to a library type: runs to "run", gym sessions to
// "strength".
func libraryTypeForCategory(category string) string {
	switch category {
	case "run":
		return "run"
	case "A", "B", "gym":
		return "strength"
	default:
		return ""
	}
}

// selectShortWorkout picks a video to replace original:
//  1. only videos with a link that are shorter than the original workout;
//  2. with a readiness score below lowReadinessThreshold or no score at all,
//     only low or medium intensity;
//  3. nothing picked within shortWorkoutRepeatWindow;
//  4. the same type as the original workout if any is left, else any type.
//     For a strength workout with a known muscle group (A or B) the order
//     is: the same group, then full body, then other types - but never a
//     strength video for the other group, which would train the same
//     muscles as a neighbouring day. A bare "אימון" (no muscle group) gets
//     a full-body video first, then any strength video, then other types;
//  5. of those, the longest - the most training that fits the slot (ties
//     go to the one listed first).
//
// When nothing qualifies it returns nil and a reason for the user.
func selectShortWorkout(library []shortWorkout, history []shortWorkoutPick, original CalendarEvent, readiness *int, now time.Time) (*shortWorkout, string) {
	if len(library) == 0 {
		return nil, fmt.Sprintf("the short workout library (%s) is empty", shortWorkoutsPath())
	}

	var candidates []shortWorkout
	for _, w := range library {
		if strings.TrimSpace(w.URL) != "" {
			candidates = append(candidates, w)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Sprintf("no video in the short workout library (%s) has a url yet - fill them in", shortWorkoutsPath())
	}

	originalMinutes := int(original.End.Sub(original.Start) / time.Minute)
	candidates = slices.DeleteFunc(candidates, func(w shortWorkout) bool {
		return w.DurationMin <= 0 || w.DurationMin >= originalMinutes
	})
	if len(candidates) == 0 {
		return nil, fmt.Sprintf("no video in the library is shorter than %q (%d min)", original.Summary, originalMinutes)
	}

	if readiness == nil || *readiness < lowReadinessThreshold {
		candidates = slices.DeleteFunc(candidates, func(w shortWorkout) bool {
			intensity := strings.ToLower(strings.TrimSpace(w.Intensity))
			return intensity != "low" && intensity != "medium"
		})
		if len(candidates) == 0 {
			why := "no readiness score is available"
			if readiness != nil {
				why = fmt.Sprintf("readiness is low (%d)", *readiness)
			}
			return nil, fmt.Sprintf("%s, and every short enough video is high intensity", why)
		}
	}

	candidates = slices.DeleteFunc(candidates, func(w shortWorkout) bool {
		for _, p := range history {
			if p.URL == w.URL && now.Sub(p.PickedAt) < shortWorkoutRepeatWindow {
				return true
			}
		}
		return false
	})
	if len(candidates) == 0 {
		return nil, "every suitable video was already used in the last 7 days"
	}

	category := categorizeWorkout(original.Summary)
	if category == "A" || category == "B" {
		candidates = slices.DeleteFunc(candidates, func(w shortWorkout) bool {
			return w.isStrength() && w.muscleGroup() != category && w.muscleGroup() != "full"
		})
		if len(candidates) == 0 {
			return nil, fmt.Sprintf("the only suitable strength videos are for other muscle groups than %q (%s)", original.Summary, category)
		}
	}
	candidates = preferredShortWorkouts(candidates, category)

	best := candidates[0]
	for _, w := range candidates[1:] {
		if w.DurationMin > best.DurationMin {
			best = w
		}
	}
	return &best, ""
}

// preferredShortWorkouts narrows candidates (already filtered) to the best
// matching tier for a workout of the given calendar category, or returns
// them all when no tier matches.
func preferredShortWorkouts(candidates []shortWorkout, category string) []shortWorkout {
	wantType := libraryTypeForCategory(category)
	if wantType == "" {
		return candidates
	}
	tiers := []func(shortWorkout) bool{
		func(w shortWorkout) bool { return strings.EqualFold(strings.TrimSpace(w.Type), wantType) },
	}
	switch category {
	case "A", "B":
		tiers = []func(shortWorkout) bool{
			func(w shortWorkout) bool { return w.isStrength() && w.muscleGroup() == category },
			func(w shortWorkout) bool { return w.isStrength() && w.muscleGroup() == "full" },
		}
	case "gym":
		// A bare "אימון" doesn't say which muscles: full body first.
		tiers = []func(shortWorkout) bool{
			func(w shortWorkout) bool { return w.isStrength() && w.muscleGroup() == "full" },
			func(w shortWorkout) bool { return w.isStrength() },
		}
	}
	for _, matches := range tiers {
		var tier []shortWorkout
		for _, w := range candidates {
			if matches(w) {
				tier = append(tier, w)
			}
		}
		if len(tier) > 0 {
			return tier
		}
	}
	return candidates
}

// shortWorkoutDescription is the description written to the replaced event.
func shortWorkoutDescription(w shortWorkout, original CalendarEvent) string {
	kind := w.Type
	if w.isStrength() {
		kind += " (" + muscleGroupLabels[w.muscleGroup()] + ")"
	}
	return fmt.Sprintf("אימון קצר במקום \"%s\" - השבוע מלא ולא היה לאן להזיז אותו.\n\n%s\n\nמשך: %d דקות · סוג: %s · עצימות: %s",
		original.Summary, w.URL, w.DurationMin, kind, w.Intensity)
}

// muscleGroupLabels describes each muscle group in Hebrew, for the calendar.
var muscleGroupLabels = map[string]string{
	"A":    "רגליים, גב, יד קדמית",
	"B":    "חזה, כתפיים, יד אחורית",
	"full": "כל הגוף",
}

// applyShortWorkout replaces original in the calendar by w - new title and
// description, same time slot - and records the pick in the history file.
func applyShortWorkout(client *http.Client, original CalendarEvent, w shortWorkout, historyPath string, now time.Time) error {
	if err := patchCalendarEvent(client, original.ID, map[string]any{
		"summary":     w.Title,
		"description": shortWorkoutDescription(w, original),
	}); err != nil {
		return fmt.Errorf("replace %q with a short workout: %w", original.Summary, err)
	}
	fmt.Printf("Replaced %q with the short workout %q (%d min): %s\n", original.Summary, w.Title, w.DurationMin, w.URL)

	history, err := loadShortWorkoutHistory(historyPath)
	if err != nil {
		// The calendar is already updated; a broken history file only
		// means the 7-day rule may repeat a video, so don't fail for it.
		fmt.Printf("Warning: %v; starting a new history.\n", err)
		history = nil
	}
	history = append(history, shortWorkoutPick{URL: w.URL, Title: w.Title, PickedAt: now})
	if err := saveShortWorkoutHistory(historyPath, history, now); err != nil {
		fmt.Printf("Warning: could not record the pick: %v\n", err)
	}
	return nil
}
