package model

// RecoveryData contains biometric metrics for training readiness.
type RecoveryData struct {
	ReadinessScore         int  `json:"readiness_score"`
	ReadinessIsPlaceholder bool `json:"readiness_is_placeholder,omitempty"`

	SleepHours         float64 `json:"sleep_hours"`
	SleepIsPlaceholder bool    `json:"sleep_is_placeholder,omitempty"`

	RestingHR              int  `json:"resting_hr"`
	RestingHRIsPlaceholder bool `json:"resting_hr_is_placeholder,omitempty"`

	RHRBaseline              int  `json:"rhr_baseline"`
	RHRBaselineIsPlaceholder bool `json:"rhr_baseline_is_placeholder,omitempty"`

	FeelingExhausted *bool `json:"feeling_exhausted,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

type Severity int

const (
	SeverityGood Severity = iota
	SeverityCaution
	SeverityHazardous
)

func (s Severity) String() string {
	switch s {
	case SeverityGood:
		return "GOOD"
	case SeverityCaution:
		return "CAUTION"
	case SeverityHazardous:
		return "HAZARDOUS"
	default:
		return "UNKNOWN"
	}
}

type EvaluationFactor struct {
	Name             string   `json:"name"`
	Value            float64  `json:"value"`
	Unit             string   `json:"unit"`
	Severity         Severity `json:"severity"`
	Note             string   `json:"note"`
	IsPlaceholder    bool     `json:"is_placeholder"`
	SeverityExcluded bool     `json:"severity_excluded"`
}

type AgentReport struct {
	WeatherFactors   []EvaluationFactor `json:"weather_factors"`
	RecoveryFactors  []EvaluationFactor `json:"recovery_factors"`
	WeatherSeverity  Severity           `json:"weather_severity"`
	RecoverySeverity Severity           `json:"recovery_severity"`
	OverallSeverity  Severity           `json:"overall_severity"`
	Verdict          string             `json:"verdict"`
	Advice           string             `json:"advice"`
}
