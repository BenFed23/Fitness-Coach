package model

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
)

type RoutineConfig struct {
	Title           string      `json:"title"`
	URL             string      `json:"url"`
	DurationMinutes int         `json:"duration_minutes"`
	WorkoutType     WorkoutType `json:"workout_type,omitempty"`
}

type AppConfig struct {
	WorkoutKeywords   []string                 `json:"workout_keywords"`
	FallbackRoutines  map[string]RoutineConfig `json:"fallback_routines"`
	ActiveHoursStart  int                      `json:"active_hours_start"`
	ActiveHoursEnd    int                      `json:"active_hours_end"`
	CalendarTimeZone  string                   `json:"calendar_time_zone"`
	BufferCommuteMins int                      `json:"buffer_commute_mins"`
}

type OffsetState struct {
	LastTelegramOffset int `json:"last_telegram_offset"`
}

func DefaultConfig() AppConfig {
	return AppConfig{
		WorkoutKeywords:   []string{"אימון", "workout", "training", "ריצה", "run", "gym", "כושר", "cindy"},
		ActiveHoursStart:  7,
		ActiveHoursEnd:    22,
		CalendarTimeZone:  "Asia/Jerusalem",
		BufferCommuteMins: 15,
		FallbackRoutines: map[string]RoutineConfig{
			"default": {
				Title:           "Tom Holland Spider-Man Routine (Emergency Backup)",
				URL:             "https://www.youtube.com/watch?v=SOaALNxeCZA",
				DurationMinutes: 20,
				WorkoutType:     WorkoutTypeHomeBackup,
			},
			"compressed_gym": {
				Title:           "High-Density Gym Strength Session (Supersets)",
				URL:             "",
				DurationMinutes: 40,
				WorkoutType:     WorkoutTypeCompressed,
			},
		},
	}
}

func LoadConfig(filePath string) (AppConfig, error) {
	defaultCfg := DefaultConfig()

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			if saveErr := SaveConfig(filePath, defaultCfg); saveErr != nil {
				return defaultCfg, fmt.Errorf("failed to save default config: %w", saveErr)
			}
			return defaultCfg, nil
		}
		return defaultCfg, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultCfg, fmt.Errorf("corrupted config json: %w", err)
	}

	if cfg.ActiveHoursStart <= 0 {
		cfg.ActiveHoursStart = 7
	}
	if cfg.ActiveHoursEnd <= 0 || cfg.ActiveHoursEnd > 24 {
		cfg.ActiveHoursEnd = 22
	}
	if cfg.CalendarTimeZone == "" {
		cfg.CalendarTimeZone = "Asia/Jerusalem"
	}
	if cfg.FallbackRoutines == nil {
		cfg.FallbackRoutines = defaultCfg.FallbackRoutines
	}

	for name, routine := range cfg.FallbackRoutines {
		if routine.DurationMinutes <= 0 {
			routine.DurationMinutes = 20
		}
		if _, parseErr := url.ParseRequestURI(routine.URL); parseErr != nil && routine.URL != "" {
			return defaultCfg, fmt.Errorf("invalid URL in routine %s: %s", name, routine.URL)
		}
		cfg.FallbackRoutines[name] = routine
	}

	return cfg, nil
}

// SaveConfig writes the configuration atomically using a temporary file.
func SaveConfig(filePath string, cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	tmpFile := filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return fmt.Errorf("failed to write temp config file: %w", err)
	}

	if err := os.Rename(tmpFile, filePath); err != nil {
		return fmt.Errorf("failed to replace config file atomically: %w", err)
	}
	return nil
}

func LoadTelegramOffset(filePath string) int {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return 0
	}
	var state OffsetState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0
	}
	return state.LastTelegramOffset
}

func SaveTelegramOffset(filePath string, offset int) {
	state := OffsetState{LastTelegramOffset: offset}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmpFile := filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmpFile, filePath)
}
