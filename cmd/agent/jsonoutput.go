package main

import (
	"encoding/json"
	"io"
	"time"
)

// interactiveAuth allows googleClient to open a browser for authorization.
// It's off in -json mode, where a caller like the Telegram bot is waiting
// for output and nobody is there to click through the consent screen.
var interactiveAuth = true

// agentSnapshot is the -json output: everything the agent measured and
// decided, for other programs (the Telegram bot) to consume. Readings that
// aren't available are null rather than a placeholder number.
type agentSnapshot struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Location    snapshotLocation `json:"location"`
	Weather     snapshotWeather  `json:"weather"`
	Recovery    snapshotRecovery `json:"recovery"`
	Verdict     snapshotVerdict  `json:"verdict"`
	Factors     []snapshotFactor `json:"factors"`
	Warnings    []string         `json:"warnings"`
}

type snapshotLocation struct {
	City    string `json:"city"`
	Country string `json:"country"`
}

type snapshotWeather struct {
	TemperatureC float64 `json:"temperature_c"`
	HumidityPct  float64 `json:"humidity_pct"`
	RainMM       float64 `json:"rain_mm"`
	WindKmh      float64 `json:"wind_kmh"`
}

type snapshotRecovery struct {
	Source             string     `json:"source"`               // google_health, recovery.json or mock
	AuthError          string     `json:"auth_error,omitempty"` // set when Google needs re-authorization
	ReadinessScore     *int       `json:"readiness_score"`
	ReadinessBreakdown string     `json:"readiness_breakdown,omitempty"`
	SleepHours         *float64   `json:"sleep_hours"`
	SleepStart         *time.Time `json:"sleep_start"`
	SleepEnd           *time.Time `json:"sleep_end"`
	SleepEstimated     bool       `json:"sleep_estimated"`
	TrackerNotWorn     bool       `json:"tracker_not_worn"`
	RestingHR          *int       `json:"resting_hr"`
	RHRBaseline        *int       `json:"rhr_baseline"`
	HRVMs              *float64   `json:"hrv_ms"`
	HRVBaselineMs      *float64   `json:"hrv_baseline_ms"`
	RespiratoryRate    *float64   `json:"respiratory_rate"`
	SkinTempDeltaC     *float64   `json:"skin_temp_delta_c"`
	FeelingExhausted   bool       `json:"feeling_exhausted"`
}

type snapshotVerdict struct {
	// Kind is machine-readable: go, adjust_for_weather, light,
	// skip_weather (outdoor only) or skip_recovery (rest day).
	Kind                  string `json:"kind"`
	Title                 string `json:"title"`
	Advice                string `json:"advice"`
	WeatherSeverity       string `json:"weather_severity"`
	RecoverySeverity      string `json:"recovery_severity"`
	RecoveryDataAvailable bool   `json:"recovery_data_available"`
}

type snapshotFactor struct {
	Group    string   `json:"group"` // weather or recovery
	Name     string   `json:"name"`
	Value    *float64 `json:"value"`
	Unit     string   `json:"unit"`
	Severity string   `json:"severity"` // GOOD, CAUTION, HAZARDOUS or N/A
	Note     string   `json:"note"`
}

func (k VerdictKind) String() string {
	switch k {
	case VerdictGo:
		return "go"
	case VerdictAdjustForWeather:
		return "adjust_for_weather"
	case VerdictLight:
		return "light"
	case VerdictSkipWeather:
		return "skip_weather"
	case VerdictSkipRecovery:
		return "skip_recovery"
	default:
		return "unknown"
	}
}

func ptr[T any](v T) *T { return &v }

// ptrIf returns &v when ok, otherwise nil (JSON null).
func ptrIf[T any](ok bool, v T) *T {
	if !ok {
		return nil
	}
	return &v
}

func writeSnapshotJSON(w io.Writer, loc *IPLocation, weather *WeatherResponse, r *RecoveryData, report *AgentReport) error {
	snapshot := agentSnapshot{
		GeneratedAt: time.Now(),
		Location:    snapshotLocation{City: loc.City, Country: loc.Country},
		Weather: snapshotWeather{
			TemperatureC: weather.Current.Temperature2m,
			HumidityPct:  weather.Current.RelativeHumidity2m,
			RainMM:       weather.Current.Rain,
			WindKmh:      weather.Current.WindSpeed10m,
		},
		Recovery: snapshotRecovery{
			Source:             r.Source,
			AuthError:          r.AuthError,
			ReadinessScore:     ptrIf(!r.ReadinessIsPlaceholder, r.ReadinessScore),
			ReadinessBreakdown: r.ReadinessBreakdown,
			SleepHours:         ptrIf(!r.SleepIsPlaceholder && !r.TrackerNotWorn, r.SleepHours),
			SleepStart:         ptrIf(!r.SleepStart.IsZero(), r.SleepStart),
			SleepEnd:           ptrIf(!r.SleepEnd.IsZero(), r.SleepEnd),
			SleepEstimated:     r.SleepEstimated,
			TrackerNotWorn:     r.TrackerNotWorn,
			RestingHR:          ptrIf(!r.RestingHRIsPlaceholder, r.RestingHR),
			RHRBaseline:        ptrIf(!r.RHRBaselineIsPlaceholder && r.RHRBaseline > 0, r.RHRBaseline),
			HRVMs:              ptrIf(r.HRV.Available, r.HRV.Value),
			HRVBaselineMs:      ptrIf(r.HRV.Available && r.HRV.BaselineDays > 0, r.HRV.BaselineMean),
			RespiratoryRate:    ptrIf(r.RespiratoryRate.Available, r.RespiratoryRate.Value),
			SkinTempDeltaC:     ptrIf(r.SkinTempDelta.Available, r.SkinTempDelta.Value),
			FeelingExhausted:   r.FeelingExhausted,
		},
		Verdict: snapshotVerdict{
			Kind:                  report.Kind.String(),
			Title:                 report.Verdict,
			Advice:                report.Advice,
			WeatherSeverity:       report.WeatherSeverity.String(),
			RecoverySeverity:      report.RecoverySeverity.String(),
			RecoveryDataAvailable: report.RecoveryDataAvailable,
		},
		Warnings: r.Warnings,
	}
	if snapshot.Warnings == nil {
		snapshot.Warnings = []string{}
	}
	addFactors := func(group string, factors []Factor) {
		for _, f := range factors {
			sf := snapshotFactor{Group: group, Name: f.Name, Unit: f.Unit, Severity: f.Severity.String(), Note: f.Note}
			if !f.IsPlaceholder {
				sf.Value = ptr(f.Value)
			}
			if f.IsPlaceholder || f.Unscored {
				sf.Severity = "N/A"
			}
			snapshot.Factors = append(snapshot.Factors, sf)
		}
	}
	addFactors("weather", report.WeatherFactors)
	addFactors("recovery", report.RecoveryFactors)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(snapshot)
}
