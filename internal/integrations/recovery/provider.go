package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"fitness-agent/internal/model"
)

var ErrNoRecoveryData = errors.New("no recovery data available")

type Provider interface {
	Name() string
	FetchRecovery(ctx context.Context, now time.Time) (*model.RecoveryData, error)
}

type LocalFileProvider struct {
	Path string
}

func (p LocalFileProvider) Name() string { return "Local JSON File" }
func (p LocalFileProvider) FetchRecovery(ctx context.Context, now time.Time) (*model.RecoveryData, error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return nil, err
	}
	var recovery model.RecoveryData
	if err := json.Unmarshal(data, &recovery); err != nil {
		return nil, fmt.Errorf("decode recovery json: %w", err)
	}
	return &recovery, nil
}

type MockProvider struct {
	Scenario string
}

func (p MockProvider) Name() string { return fmt.Sprintf("Mock Provider (%s)", p.Scenario) }
func (p MockProvider) FetchRecovery(ctx context.Context, now time.Time) (*model.RecoveryData, error) {
	switch p.Scenario {
	case "poor":
		return &model.RecoveryData{
			ReadinessScore: 42,
			SleepHours:     3.5, // acute sleep deprivation
			RestingHR:      76,
			RHRBaseline:    55, // Delta = +21 bpm
		}, nil
	case "caution":
		return &model.RecoveryData{
			ReadinessScore: 64,
			SleepHours:     5.5,
			RestingHR:      62,
			RHRBaseline:    55, // Delta = +7 bpm
		}, nil
	default: // "good"
		return &model.RecoveryData{
			ReadinessScore: 88,
			SleepHours:     8.0,
			RestingHR:      54,
			RHRBaseline:    55, // Delta = -1 bpm
		}, nil
	}
}

// FetchRecoveryData queries available providers (Mock or Local file).
func FetchRecoveryData(ctx context.Context, now time.Time, useMock bool, mockScenario, localPath string) (*model.RecoveryData, error) {
	var providers []Provider
	if useMock {
		providers = []Provider{MockProvider{Scenario: mockScenario}}
	} else {
		providers = []Provider{LocalFileProvider{Path: localPath}}
	}

	var lastErr error
	for _, p := range providers {
		data, err := p.FetchRecovery(ctx, now)
		if err == nil && data != nil {
			return data, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoRecoveryData
}
