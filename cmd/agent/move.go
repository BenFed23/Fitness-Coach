package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// moveResult is the outcome of -move-today: what was (or, with -dry-run,
// would be) changed on the calendar.
type moveResult struct {
	DryRun bool
	Plan   *reschedulePlan
	// Replacement is set when the week was full and today's workout was
	// (or would be) replaced by a short workout instead of moved.
	Replacement *shortWorkout
	Readiness   *int // the readiness score the replacement was chosen with
	Err         error
}

// moveTodaysWorkout moves today's workout forward on Google Calendar because
// the user asked to - unlike -reschedule, it doesn't depend on the verdict.
// If the week has no room for it, today's workout is replaced by a short one
// from the library instead.
func moveTodaysWorkout(now time.Time, dryRun bool) moveResult {
	result := moveResult{DryRun: dryRun}
	config, err := googleOAuthConfig(googleCalendarScopes...)
	if err != nil {
		result.Err = err
		return result
	}
	client, err := googleClient(context.Background(), config, calendarTokenFile)
	if err != nil {
		result.Err = fmt.Errorf("authorize Google Calendar: %w", err)
		return result
	}
	result.Plan, result.Err = planReschedule(client, now, nil)
	switch {
	case result.Err == nil:
		if !dryRun {
			result.Err = applyReschedulePlan(client, result.Plan)
		}
	case errors.Is(result.Err, errWeekFull) && result.Plan != nil:
		result.Replacement, result.Readiness, result.Err =
			fallBackToShortWorkout(client, result.Plan.Missed, result.Err, now, dryRun)
	}
	return result
}

// fallBackToShortWorkout replaces missed by a short workout from the library
// (see selectShortWorkout). With dryRun it only picks one. When nothing fits,
// the error keeps weekErr and says why.
func fallBackToShortWorkout(client *http.Client, missed CalendarEvent, weekErr error, now time.Time, dryRun bool) (*shortWorkout, *int, error) {
	library, err := loadShortWorkouts(shortWorkoutsPath())
	if err != nil {
		return nil, nil, fmt.Errorf("%w; no short workout fallback: %v", weekErr, err)
	}
	history, err := loadShortWorkoutHistory(shortWorkoutHistoryFile)
	if err != nil {
		fmt.Printf("Warning: %v; ignoring the history.\n", err)
	}
	// If nothing fits even with full readiness, skip the slow Google Health
	// lookup - the score can only rule out more videos.
	full := 100
	if pick, reason := selectShortWorkout(library, history, missed, &full, now); pick == nil {
		return nil, nil, fmt.Errorf("%w; no short workout to replace it with: %s", weekErr, reason)
	}

	readiness := currentReadiness()
	pick, reason := selectShortWorkout(library, history, missed, readiness, now)
	if pick == nil {
		return nil, readiness, fmt.Errorf("%w; no short workout to replace it with: %s", weekErr, reason)
	}
	if !dryRun {
		if err := applyShortWorkout(client, missed, *pick, shortWorkoutHistoryFile, now); err != nil {
			return nil, readiness, err
		}
	}
	return pick, readiness, nil
}

// currentReadiness returns today's readiness score, or nil if there is none
// (tracker not worn, no access to Google Health...).
func currentReadiness() *int {
	recovery, err := FetchFitbitRecovery()
	if err != nil || recovery == nil || recovery.ReadinessIsPlaceholder {
		return nil
	}
	return ptr(recovery.ReadinessScore)
}

func printMoveResult(r moveResult) {
	if r.Err != nil {
		fmt.Printf("Could not move today's workout: %v\n", r.Err)
		return
	}
	if r.Replacement != nil {
		readiness := "no readiness score"
		if r.Readiness != nil {
			readiness = fmt.Sprintf("readiness %d", *r.Readiness)
		}
		if r.DryRun {
			fmt.Printf("Dry run - the calendar was not changed. The week is full, so %q would be replaced (same time slot) by:\n", r.Plan.Missed.Summary)
		} else {
			fmt.Printf("The week is full, so %q was replaced (same time slot) by:\n", r.Plan.Missed.Summary)
		}
		w := r.Replacement
		fmt.Printf("  %q - %d min, %s, %s intensity (%s)\n  %s\n", w.Title, w.DurationMin, w.Type, w.Intensity, readiness, w.URL)
		return
	}
	if r.DryRun {
		fmt.Println("Dry run - the calendar was not changed. The plan:")
		for _, c := range r.Plan.Changes {
			if c.NewSummary != "" {
				fmt.Printf("  - rename %q to %q\n", c.Event.Summary, c.NewSummary)
			} else {
				fmt.Printf("  - move %q from %s to %s\n", c.Event.Summary,
					c.Event.Start.Format("Mon Jan 2 15:04"), c.NewStart.Format("Mon Jan 2 15:04"))
			}
		}
	}
}

type moveChangeJSON struct {
	Summary    string     `json:"summary"`
	From       time.Time  `json:"from"`
	To         *time.Time `json:"to,omitempty"`          // set for a move
	NewSummary string     `json:"new_summary,omitempty"` // set for a rename
}

type moveJSON struct {
	OK         bool             `json:"ok"`
	DryRun     bool             `json:"dry_run"`
	Error      string           `json:"error,omitempty"`
	AuthError  bool             `json:"auth_error"`        // the calendar needs re-authorization
	Workout    string           `json:"workout,omitempty"` // today's workout being moved
	Superseded bool             `json:"superseded"`
	Changes    []moveChangeJSON `json:"changes"`
	// Replacement is set when the week was full and today's workout was
	// replaced (same time slot) by a short workout from the library.
	Replacement *replacementJSON `json:"replacement,omitempty"`
}

type replacementJSON struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	DurationMin int    `json:"duration_min"`
	Type        string `json:"type"`
	Intensity   string `json:"intensity"`
	MuscleGroup string `json:"muscle_group,omitempty"` // A, B or full - strength videos only
	Replaces    string `json:"replaces"`               // the original workout's title
	Readiness   *int   `json:"readiness"`              // the score it was chosen with, or null
}

// writeMoveJSON writes the -move-today result. Expected failures (no
// workout today, a full week...) are reported in "error", not the exit code.
func writeMoveJSON(w io.Writer, r moveResult) error {
	out := moveJSON{OK: r.Err == nil, DryRun: r.DryRun, Changes: []moveChangeJSON{}}
	if r.Err != nil {
		out.Error = r.Err.Error()
		out.AuthError = errors.Is(r.Err, errNeedsAuthorization)
	}
	if r.Plan != nil {
		out.Workout = r.Plan.Missed.Summary
		out.Superseded = r.Plan.Superseded
		for _, c := range r.Plan.Changes {
			change := moveChangeJSON{Summary: c.Event.Summary, From: c.Event.Start, NewSummary: c.NewSummary}
			if !c.NewStart.IsZero() {
				change.To = ptr(c.NewStart)
			}
			out.Changes = append(out.Changes, change)
		}
	}
	if w := r.Replacement; w != nil && r.Plan != nil {
		out.Replacement = &replacementJSON{
			Title: w.Title, URL: w.URL, DurationMin: w.DurationMin, Type: w.Type, Intensity: w.Intensity,
			Replaces: r.Plan.Missed.Summary, Readiness: r.Readiness,
		}
		if w.isStrength() {
			out.Replacement.MuscleGroup = w.muscleGroup()
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
