package engine

import (
	"testing"

	"fitness-agent/internal/model"
)

func TestEvaluateRecovery_HazardousSleep(t *testing.T) {
	thresholds := DefaultThresholds()
	rec := &model.RecoveryData{
		ReadinessScore: 80,
		SleepHours:     3.5, // Below 4.5 hrs -> Hazardous
		RestingHR:      55,
		RHRBaseline:    55,
	}

	report := BuildAgentReport(nil, rec, thresholds)
	if report.RecoverySeverity != model.SeverityHazardous {
		t.Fatalf("expected RecoverySeverity to be Hazardous, got %v", report.RecoverySeverity)
	}
	if report.Verdict != "Rest Day / Active Mobility Only" {
		t.Errorf("expected Rest Day verdict, got %s", report.Verdict)
	}
}

func TestEvaluateWeather_HazardousRain(t *testing.T) {
	thresholds := DefaultThresholds()
	weather := &model.WeatherResponse{
		Current: model.CurrentWeather{
			Temperature2m:      22,
			RelativeHumidity2m: 60,
			Rain:               15.0, // > 10 mm -> Hazardous
			WindSpeed10m:       10,
		},
	}

	report := BuildAgentReport(weather, nil, thresholds)
	if report.WeatherSeverity != model.SeverityHazardous {
		t.Fatalf("expected WeatherSeverity to be Hazardous, got %v", report.WeatherSeverity)
	}
	if report.Verdict != "Shift Indoors / Gym Session" {
		t.Errorf("expected indoor verdict, got %s", report.Verdict)
	}
}
