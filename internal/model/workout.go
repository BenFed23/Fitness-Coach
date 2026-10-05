package model

import "time"

type WorkoutType string

const (
	WorkoutTypeGymStrength WorkoutType = "gym_strength"
	WorkoutTypeCardio      WorkoutType = "cardio"
	WorkoutTypeCompressed  WorkoutType = "compressed"
	WorkoutTypeHomeBackup  WorkoutType = "home_backup"
)

// WorkoutProfile defines the time requirements for a training session.
type WorkoutProfile struct {
	Type            WorkoutType   `json:"type"`
	Title           string        `json:"title"`
	TargetDuration  time.Duration `json:"target_duration"` // e.g., 90 minutes for gym
	BufferDuration  time.Duration `json:"buffer_duration"` // e.g., 15 minutes for travel/change/shower
	MinDuration     time.Duration `json:"min_duration"`    // e.g., 70 minutes minimum viable workout
	IsOutdoor       bool          `json:"is_outdoor"`
	FallbackAllowed bool          `json:"fallback_allowed"`
}

// DefaultWorkoutProfiles provides sensible, real-world defaults.
func DefaultWorkoutProfiles() map[WorkoutType]WorkoutProfile {
	return map[WorkoutType]WorkoutProfile{
		WorkoutTypeGymStrength: {
			Type:            WorkoutTypeGymStrength,
			Title:           "Full Gym Strength Session",
			TargetDuration:  90 * time.Minute,
			BufferDuration:  15 * time.Minute,
			MinDuration:     75 * time.Minute,
			IsOutdoor:       false,
			FallbackAllowed: true,
		},
		WorkoutTypeCardio: {
			Type:            WorkoutTypeCardio,
			Title:           "Outdoor Running Session",
			TargetDuration:  60 * time.Minute,
			BufferDuration:  10 * time.Minute,
			MinDuration:     45 * time.Minute,
			IsOutdoor:       true,
			FallbackAllowed: true,
		},
		WorkoutTypeCompressed: {
			Type:            WorkoutTypeCompressed,
			Title:           "Compressed Density Session (Supersets)",
			TargetDuration:  40 * time.Minute,
			BufferDuration:  5 * time.Minute,
			MinDuration:     30 * time.Minute,
			IsOutdoor:       false,
			FallbackAllowed: false,
		},
		WorkoutTypeHomeBackup: {
			Type:            WorkoutTypeHomeBackup,
			Title:           "Home Follow-Along Workout",
			TargetDuration:  30 * time.Minute,
			BufferDuration:  5 * time.Minute,
			MinDuration:     20 * time.Minute,
			IsOutdoor:       false,
			FallbackAllowed: false,
		},
	}
}

// TimeSlot represents a continuous period of time.
type TimeSlot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (s TimeSlot) Duration() time.Duration {
	return s.End.Sub(s.Start)
}
