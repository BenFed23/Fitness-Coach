package scheduler

import (
	"sort"
	"time"

	"fitness-agent/internal/model"
)

type ScheduleProposal struct {
	Found       bool                 `json:"found"`
	Slot        model.TimeSlot       `json:"slot"`
	WorkoutType model.WorkoutType    `json:"workout_type"`
	Duration    time.Duration        `json:"duration"`
	IsFallback  bool                 `json:"is_fallback"`
	Message     string               `json:"message"`
}

// FindBestSlot searches for the optimal available time window in a given day.
func FindBestSlot(
	targetDate time.Time,
	currentTime time.Time,
	activeStartHour int,
	activeEndHour int,
	busySlots []model.TimeSlot,
	workout model.WorkoutProfile,
	compressedWorkout model.WorkoutProfile,
	loc *time.Location,
) ScheduleProposal {
	if loc == nil {
		loc = targetDate.Location()
	}

	windowStart := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), activeStartHour, 0, 0, 0, loc)
	windowEnd := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), activeEndHour, 0, 0, 0, loc)

	// If scheduling for today, we cannot schedule in the past.
	// Give at least a 15-minute buffer from currentTime.
	currInLoc := currentTime.In(loc)
	targetInLoc := targetDate.In(loc)
	earliestAllowed := currInLoc.Add(15 * time.Minute)
	if targetInLoc.Year() == currInLoc.Year() &&
		targetInLoc.Month() == currInLoc.Month() &&
		targetInLoc.Day() == currInLoc.Day() {
		if earliestAllowed.After(windowStart) {
			windowStart = earliestAllowed
		}
	}

	if windowStart.After(windowEnd) || windowStart.Equal(windowEnd) {
		return ScheduleProposal{
			Found:   false,
			Message: "Active training window for this day has already passed.",
		}
	}

	// 1. Filter, clamp and merge busy slots
	mergedBusy := mergeAndClampBusySlots(busySlots, windowStart, windowEnd)

	// 2. Find free gaps
	freeGaps := computeFreeGaps(windowStart, windowEnd, mergedBusy)

	// 3. Try to schedule full target workout (e.g. 90m + 15m buffer)
	neededFull := workout.TargetDuration + workout.BufferDuration
	for _, gap := range freeGaps {
		if gap.Duration() >= neededFull {
			workoutSlot := model.TimeSlot{
				Start: gap.Start,
				End:   gap.Start.Add(workout.TargetDuration),
			}
			return ScheduleProposal{
				Found:       true,
				Slot:        workoutSlot,
				WorkoutType: workout.Type,
				Duration:    workout.TargetDuration,
				IsFallback:  false,
				Message:     "Found optimal window for full workout session.",
			}
		}
	}

	// 4. If full workout doesn't fit, try fallback / compressed workout (e.g. 40m + 5m buffer)
	if workout.FallbackAllowed {
		neededCompressed := compressedWorkout.TargetDuration + compressedWorkout.BufferDuration
		for _, gap := range freeGaps {
			if gap.Duration() >= neededCompressed {
				workoutSlot := model.TimeSlot{
					Start: gap.Start,
					End:   gap.Start.Add(compressedWorkout.TargetDuration),
				}
				return ScheduleProposal{
					Found:       true,
					Slot:        workoutSlot,
					WorkoutType: compressedWorkout.Type,
					Duration:    compressedWorkout.TargetDuration,
					IsFallback:  true,
					Message:     "No 90-minute window available today; scheduled a compressed high-density workout to maintain momentum.",
				}
			}
		}
	}

	return ScheduleProposal{
		Found:   false,
		Message: "Schedule is too congested today; consider taking an active recovery day or scheduling for tomorrow.",
	}
}

// mergeAndClampBusySlots sorts intervals, clamps them to [windowStart, windowEnd], and merges overlapping intervals.
func mergeAndClampBusySlots(busy []model.TimeSlot, windowStart, windowEnd time.Time) []model.TimeSlot {
	var valid []model.TimeSlot
	for _, b := range busy {
		start := b.Start
		end := b.End

		if end.Before(windowStart) || start.After(windowEnd) {
			continue
		}
		if start.Before(windowStart) {
			start = windowStart
		}
		if end.After(windowEnd) {
			end = windowEnd
		}
		if start.Before(end) {
			valid = append(valid, model.TimeSlot{Start: start, End: end})
		}
	}

	if len(valid) == 0 {
		return nil
	}

	sort.Slice(valid, func(i, j int) bool {
		return valid[i].Start.Before(valid[j].Start)
	})

	merged := []model.TimeSlot{valid[0]}
	for i := 1; i < len(valid); i++ {
		last := &merged[len(merged)-1]
		curr := valid[i]

		if curr.Start.Before(last.End) || curr.Start.Equal(last.End) {
			if curr.End.After(last.End) {
				last.End = curr.End
			}
		} else {
			merged = append(merged, curr)
		}
	}

	return merged
}

// computeFreeGaps derives unbooked intervals between windowStart and windowEnd.
func computeFreeGaps(windowStart, windowEnd time.Time, mergedBusy []model.TimeSlot) []model.TimeSlot {
	var gaps []model.TimeSlot
	curr := windowStart

	for _, b := range mergedBusy {
		if b.Start.After(curr) {
			gaps = append(gaps, model.TimeSlot{Start: curr, End: b.Start})
		}
		if b.End.After(curr) {
			curr = b.End
		}
	}

	if curr.Before(windowEnd) {
		gaps = append(gaps, model.TimeSlot{Start: curr, End: windowEnd})
	}

	return gaps
}
