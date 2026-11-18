package main

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------
// Calendar rescheduling: when a workout is skipped, try to move it to the
// next day, cascading later workouts forward as needed - without spilling
// into the next week. If a full cascade won't fit, fall back to swapping
// the missed workout with the shortest other workout later in the week.
//
// This talks to the Google Calendar REST API directly (same style as the
// Fitness calls above) rather than the official client library, so it needs
// no new dependency - just the "calendar" scope already requested during
// OAuth.
// ---------------------------------------------------------------------

// calendarChange is one edit a reschedule makes to the calendar: a move to
// NewStart/NewEnd, or (for a superseded workout) a new title.
type calendarChange struct {
	Event      CalendarEvent
	NewStart   time.Time // zero when the time doesn't change
	NewEnd     time.Time
	NewSummary string // non-empty when the title changes
}

// reschedulePlan is everything a reschedule will change, computed before
// touching the calendar so it can be previewed (-dry-run) or applied.
type reschedulePlan struct {
	Missed  CalendarEvent
	Changes []calendarChange
	// Superseded is true for the fallback: the missed workout takes over a
	// shorter same-group workout's slot, and that one is renamed.
	Superseded bool
}

// errNothingToReschedule means no workout on the day is affected by the
// verdict (e.g. only indoor sessions on a bad-weather day).
var errNothingToReschedule = errors.New("no workout on that day is affected")

// errWeekFull means the workout can't be moved: the cascade would spill past
// the end of the week and no shorter same-group workout can be replaced.
// planReschedule still returns a plan with Missed set, so -move-today can
// fall back to a short workout instead (see shortworkouts.go).
var errWeekFull = errors.New("no room to move the workout this week")

// RescheduleMissedWorkout moves the workout scheduled on missedDate forward
// one day at a time, cascading later workouts as needed - but never past the
// end of the current week. See planReschedule for the rules.
func RescheduleMissedWorkout(client *http.Client, missedDate time.Time, eligible func(category string) bool) error {
	plan, err := planReschedule(client, missedDate, eligible)
	if errors.Is(err, errNothingToReschedule) {
		fmt.Printf("No workout on %s is affected by today's verdict (e.g. only indoor sessions on a bad-weather day) - nothing to reschedule.\n",
			missedDate.Format("2006-01-02"))
		return nil
	}
	if err != nil {
		return err
	}
	return applyReschedulePlan(client, plan)
}

// planReschedule works out how to move the workout scheduled on missedDate,
// without changing anything.
//
// eligible optionally restricts which of the day's workouts may be treated as
// the missed one, by category; nil means any workout. A weather-based skip
// passes isOutdoorWorkout so indoor gym sessions are left alone.
//
// The workout moves forward one day at a time, cascading later workouts as
// needed, but never past the end of the current week. If a full cascade would
// spill past the week boundary, it falls back to letting the missed workout
// take over the slot of a shorter workout of the same muscle group on a LATER
// day this week, marking that shorter one as superseded. Nothing is ever
// moved INTO the skipped day.
func planReschedule(client *http.Client, missedDate time.Time, eligible func(category string) bool) (*reschedulePlan, error) {
	loc, err := time.LoadLocation(calendarTimeZone)
	if err != nil {
		return nil, fmt.Errorf("load calendar timezone: %w", err)
	}
	weekStart, weekEnd := weekBounds(loc, missedDate)

	events, err := listCalendarEvents(client, weekStart, weekEnd)
	if err != nil {
		return nil, err
	}

	var workouts []CalendarEvent
	for _, e := range events {
		if isWorkoutEvent(e.Summary) {
			workouts = append(workouts, e)
		}
	}
	sort.Slice(workouts, func(i, j int) bool { return workouts[i].Start.Before(workouts[j].Start) })

	missedDay := missedDate.In(loc)
	missedIdx := -1
	for i, w := range workouts {
		if !sameDay(w.Start, missedDay, loc) {
			continue
		}
		if eligible != nil && !eligible(categorizeWorkout(w.Summary)) {
			continue
		}
		missedIdx = i
		break
	}
	if missedIdx == -1 {
		if eligible != nil {
			return nil, errNothingToReschedule
		}
		return nil, fmt.Errorf("no workout found on %s to reschedule", missedDay.Format("2006-01-02"))
	}
	missed := workouts[missedIdx]
	plan := &reschedulePlan{Missed: missed}

	// Map each remaining day in the week to whichever workout (if any)
	// already occupies it, so we can detect collisions while cascading.
	byDay := map[string]CalendarEvent{}
	for i, w := range workouts {
		if i == missedIdx {
			continue
		}
		byDay[w.Start.In(loc).Format("2006-01-02")] = w
	}

	type move struct {
		event    CalendarEvent
		newStart time.Time
	}
	var moves []move
	current := missed
	target := missedDay.AddDate(0, 0, 1)
	cascadeSucceeded := false
	for !target.After(weekEnd) {
		key := target.Format("2006-01-02")
		if occupant, ok := byDay[key]; ok {
			// target day is taken - bump whoever's there and try to place
			// the current event on the day after that.
			moves = append(moves, move{event: current, newStart: target})
			current = occupant
			target = target.AddDate(0, 0, 1)
			continue
		}
		moves = append(moves, move{event: current, newStart: target})
		cascadeSucceeded = true
		break
	}

	if cascadeSucceeded {
		for _, m := range moves {
			duration := m.event.End.Sub(m.event.Start)
			origStart := m.event.Start.In(loc)
			newStart := time.Date(m.newStart.Year(), m.newStart.Month(), m.newStart.Day(),
				origStart.Hour(), origStart.Minute(), 0, 0, loc)
			plan.Changes = append(plan.Changes, calendarChange{Event: m.event, NewStart: newStart, NewEnd: newStart.Add(duration)})
		}
		return plan, nil
	}

	// Cascade would spill past the end of the week. Fall back to letting the
	// missed workout take over the slot of a shorter workout that targets the
	// same muscle group on a LATER day this week: the week still gets the
	// full session for that group, and the shorter duplicate is marked as
	// superseded. Nothing is pulled into the skipped day - doing that would
	// defeat the reason for skipping.
	missedCategory := categorizeWorkout(missed.Summary)
	if missedCategory == "" {
		return plan, fmt.Errorf(
			"%w: cascade would spill past the end of the week, and %q has no workout group ([A]/[B]/[RUN] tag or a \"Workout ...\"/\"Run\" title) - can't find a same-muscle-group workout to replace",
			errWeekFull, missed.Summary)
	}
	missedDur := missed.End.Sub(missed.Start)

	var replaced *CalendarEvent
	var replacedDur time.Duration
	for i := range workouts {
		if i == missedIdx {
			continue
		}
		w := workouts[i]
		if !w.Start.After(missedDay) || sameDay(w.Start, missedDay, loc) {
			continue // only workouts on a later day - never anything on the skipped day
		}
		if categorizeWorkout(w.Summary) != missedCategory {
			continue // must target the same muscle group as the missed workout
		}
		dur := w.End.Sub(w.Start)
		if dur >= missedDur {
			continue // must be shorter than the full session replacing it
		}
		if replaced == nil || dur < replacedDur {
			replaced = &workouts[i]
			replacedDur = dur
		}
	}
	if replaced == nil {
		return plan, fmt.Errorf(
			"%w: cascade would spill past the end of the week, and no shorter %q workout was found on a later day to replace with %q",
			errWeekFull, missedCategory, missed.Summary)
	}

	// The missed workout takes over the replaced workout's slot (its day and
	// start time) and keeps its own full duration.
	missedNewStart := replaced.Start.In(loc)
	if sameDay(missedNewStart, missedDay, loc) {
		// Defensive: the filter above should make this impossible.
		return nil, errors.New("refusing to schedule a workout on the skipped day")
	}
	plan.Superseded = true
	plan.Changes = []calendarChange{
		{Event: missed, NewStart: missedNewStart, NewEnd: missedNewStart.Add(missedDur)},
		{Event: *replaced, NewSummary: supersededPrefix + replaced.Summary},
	}
	return plan, nil
}

// applyReschedulePlan makes the calendar changes of a plan.
func applyReschedulePlan(client *http.Client, plan *reschedulePlan) error {
	loc, err := time.LoadLocation(calendarTimeZone)
	if err != nil {
		return fmt.Errorf("load calendar timezone: %w", err)
	}
	if !plan.Superseded {
		for _, c := range plan.Changes {
			if err := updateCalendarEventTime(client, c.Event.ID, c.NewStart, c.NewEnd, loc); err != nil {
				return fmt.Errorf("cascade-move %q to %s: %w", c.Event.Summary, c.NewStart.Format("2006-01-02"), err)
			}
			fmt.Printf("Moved %q to %s\n", c.Event.Summary, c.NewStart.Format("Mon Jan 2"))
		}
		return nil
	}

	moved, renamed := plan.Changes[0], plan.Changes[1]
	if err := updateCalendarEventTime(client, moved.Event.ID, moved.NewStart, moved.NewEnd, loc); err != nil {
		return fmt.Errorf("move missed workout %q: %w", moved.Event.Summary, err)
	}
	if err := patchCalendarEvent(client, renamed.Event.ID, map[string]any{"summary": renamed.NewSummary}); err != nil {
		// Undo the move so the calendar isn't left with two sessions on one day.
		if undoErr := updateCalendarEventTime(client, moved.Event.ID, moved.Event.Start, moved.Event.End, loc); undoErr != nil {
			return fmt.Errorf("mark %q as superseded: %w; undoing the move of %q ALSO failed (%v) - fix the calendar manually",
				renamed.Event.Summary, err, moved.Event.Summary, undoErr)
		}
		return fmt.Errorf("mark %q as superseded: %w (the move of %q was undone)", renamed.Event.Summary, err, moved.Event.Summary)
	}
	fmt.Printf("Could not cascade within the week - moved %q to %s (the slot of the shorter %q, now marked %s).\n",
		moved.Event.Summary, moved.NewStart.Format("Mon Jan 2 15:04"), renamed.Event.Summary, strings.TrimSpace(supersededPrefix))
	return nil
}
