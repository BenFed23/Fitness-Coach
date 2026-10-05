package scheduler

import (
	"testing"
	"time"

	"fitness-agent/internal/model"
)

func TestFindBestSlot_FullWorkoutFit(t *testing.T) {
	loc := time.UTC
	targetDate := time.Date(2026, 9, 16, 0, 0, 0, 0, loc)
	currentTime := time.Date(2026, 9, 15, 12, 0, 0, 0, loc) // Previous day

	profiles := model.DefaultWorkoutProfiles()
	gymWorkout := profiles[model.WorkoutTypeGymStrength]
	compressed := profiles[model.WorkoutTypeCompressed]

	// Busy from 09:00 to 11:00, and from 14:00 to 18:00
	busy := []model.TimeSlot{
		{
			Start: time.Date(2026, 9, 16, 9, 0, 0, 0, loc),
			End:   time.Date(2026, 9, 16, 11, 0, 0, 0, loc),
		},
		{
			Start: time.Date(2026, 9, 16, 14, 0, 0, 0, loc),
			End:   time.Date(2026, 9, 16, 18, 0, 0, 0, loc),
		},
	}

	// Active window: 07:00 to 22:00
	// Gap 1: 07:00 - 09:00 (120 mins) -> Fits 90 min + 15 min buffer!
	proposal := FindBestSlot(targetDate, currentTime, 7, 22, busy, gymWorkout, compressed, loc)

	if !proposal.Found {
		t.Fatalf("expected slot to be found, got not found: %s", proposal.Message)
	}
	if proposal.IsFallback {
		t.Fatalf("expected full workout, got fallback")
	}
	if proposal.WorkoutType != model.WorkoutTypeGymStrength {
		t.Errorf("expected gym_strength, got %s", proposal.WorkoutType)
	}
	if proposal.Duration != 90*time.Minute {
		t.Errorf("expected 90m duration, got %v", proposal.Duration)
	}
	expectedStart := time.Date(2026, 9, 16, 7, 0, 0, 0, loc)
	if !proposal.Slot.Start.Equal(expectedStart) {
		t.Errorf("expected start %v, got %v", expectedStart, proposal.Slot.Start)
	}
}

func TestFindBestSlot_CompressedFallback(t *testing.T) {
	loc := time.UTC
	targetDate := time.Date(2026, 9, 16, 0, 0, 0, 0, loc)
	currentTime := time.Date(2026, 9, 15, 12, 0, 0, 0, loc)

	profiles := model.DefaultWorkoutProfiles()
	gymWorkout := profiles[model.WorkoutTypeGymStrength] // Needs 90m + 15m = 105m
	compressed := profiles[model.WorkoutTypeCompressed]   // Needs 40m + 5m = 45m

	// Heavy schedule:
	// 07:00 - 12:00 (Busy)
	// 12:50 - 18:00 (Busy) -> Gap between 12:00 and 12:50 is 50 mins! Not enough for 105m, but fits 45m!
	// 18:00 - 22:00 (Busy)
	busy := []model.TimeSlot{
		{
			Start: time.Date(2026, 9, 16, 7, 0, 0, 0, loc),
			End:   time.Date(2026, 9, 16, 12, 0, 0, 0, loc),
		},
		{
			Start: time.Date(2026, 9, 16, 12, 50, 0, 0, loc),
			End:   time.Date(2026, 9, 16, 18, 0, 0, 0, loc),
		},
		{
			Start: time.Date(2026, 9, 16, 18, 0, 0, 0, loc),
			End:   time.Date(2026, 9, 16, 22, 0, 0, 0, loc),
		},
	}

	proposal := FindBestSlot(targetDate, currentTime, 7, 22, busy, gymWorkout, compressed, loc)

	if !proposal.Found {
		t.Fatalf("expected fallback compressed slot to be found, got none")
	}
	if !proposal.IsFallback {
		t.Errorf("expected proposal to be fallback")
	}
	if proposal.WorkoutType != model.WorkoutTypeCompressed {
		t.Errorf("expected workout type compressed, got %s", proposal.WorkoutType)
	}
	if proposal.Duration != 40*time.Minute {
		t.Errorf("expected 40 min duration, got %v", proposal.Duration)
	}
	expectedStart := time.Date(2026, 9, 16, 12, 0, 0, 0, loc)
	if !proposal.Slot.Start.Equal(expectedStart) {
		t.Errorf("expected start %v, got %v", expectedStart, proposal.Slot.Start)
	}
}

func TestFindBestSlot_CompletelyBooked(t *testing.T) {
	loc := time.UTC
	targetDate := time.Date(2026, 9, 16, 0, 0, 0, 0, loc)
	currentTime := time.Date(2026, 9, 15, 12, 0, 0, 0, loc)

	profiles := model.DefaultWorkoutProfiles()
	gymWorkout := profiles[model.WorkoutTypeGymStrength]
	compressed := profiles[model.WorkoutTypeCompressed]

	// 07:00 - 22:00 completely booked
	busy := []model.TimeSlot{
		{
			Start: time.Date(2026, 9, 16, 7, 0, 0, 0, loc),
			End:   time.Date(2026, 9, 16, 22, 0, 0, 0, loc),
		},
	}

	proposal := FindBestSlot(targetDate, currentTime, 7, 22, busy, gymWorkout, compressed, loc)

	if proposal.Found {
		t.Fatalf("expected slot to NOT be found when day is fully booked")
	}
}

func TestFindBestSlot_NeverScheduleInPastToday(t *testing.T) {
	loc := time.UTC
	targetDate := time.Date(2026, 9, 16, 0, 0, 0, 0, loc)
	currentTime := time.Date(2026, 9, 16, 15, 0, 0, 0, loc) // It's 15:00 today

	profiles := model.DefaultWorkoutProfiles()
	gymWorkout := profiles[model.WorkoutTypeGymStrength]
	compressed := profiles[model.WorkoutTypeCompressed]

	// Morning was completely free, but we are already at 15:00!
	busy := []model.TimeSlot{
		{
			Start: time.Date(2026, 9, 16, 16, 0, 0, 0, loc),
			End:   time.Date(2026, 9, 16, 17, 0, 0, 0, loc),
		},
	}

	proposal := FindBestSlot(targetDate, currentTime, 7, 22, busy, gymWorkout, compressed, loc)

	if !proposal.Found {
		t.Fatalf("expected evening slot to be found")
	}
	if proposal.Slot.Start.Before(currentTime) {
		t.Fatalf("scheduled start %v is in the past relative to %v", proposal.Slot.Start, currentTime)
	}
	// From 17:00 to 22:00 is 5 hours, so it should schedule after 17:00
	expectedStart := time.Date(2026, 9, 16, 17, 0, 0, 0, loc)
	if !proposal.Slot.Start.Equal(expectedStart) {
		t.Errorf("expected start %v, got %v", expectedStart, proposal.Slot.Start)
	}
}
