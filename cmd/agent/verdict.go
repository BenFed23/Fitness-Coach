package main

import (
	"fmt"
	"math"
)

// ---------------------------------------------------------------------
// Evaluation logic: turns raw weather/recovery readings into Factors,
// and Factors into an overall workout verdict.
// ---------------------------------------------------------------------

// Severity is ordered so the highest severity is the most restrictive.
type Severity int

const (
	Good Severity = iota
	Caution
	Hazardous
)

func (s Severity) String() string {
	switch s {
	case Good:
		return "GOOD"
	case Caution:
		return "CAUTION"
	case Hazardous:
		return "HAZARDOUS"
	default:
		return "UNKNOWN"
	}
}

// Factor is one evaluated measurement in the weather or recovery report.
type Factor struct {
	Name     string   `json:"name"`
	Value    float64  `json:"value"`
	Unit     string   `json:"unit"`
	Severity Severity `json:"severity"`
	Note     string   `json:"note"`
	// IsPlaceholder marks a Factor whose Value is not a real reading (e.g. a
	// mock default or a value Google Health never provides). The report
	// prints "-" for these instead of the number, and they're excluded from
	// severity aggregation so a fake "GOOD" can't mask missing data.
	IsPlaceholder bool `json:"is_placeholder"`
	// Unscored marks a Factor whose Value IS a real reading but can't be
	// judged - e.g. a resting HR with no personal baseline to compare it to.
	// The value is printed, but it's excluded from severity aggregation.
	Unscored bool `json:"unscored"`
	// SelfReported marks a Factor that comes from the user's own answer (the
	// -exhausted flag or recovery.json) rather than a device measurement.
	SelfReported bool `json:"self_reported"`
}

// VerdictKind is the machine-readable decision behind Verdict/Advice. Code
// must branch on this, never on the human-readable Verdict text.
type VerdictKind int

const (
	VerdictGo VerdictKind = iota
	VerdictAdjustForWeather
	VerdictLight
	VerdictSkipWeather  // unsafe weather: only outdoor workouts are affected
	VerdictSkipRecovery // the body needs rest: every workout is affected
)

// AgentReport combines recovery and weather into one workout decision.
type AgentReport struct {
	WeatherFactors   []Factor `json:"weather_factors"`
	RecoveryFactors  []Factor `json:"recovery_factors"`
	WeatherSeverity  Severity `json:"weather_severity"`
	RecoverySeverity Severity `json:"recovery_severity"`
	// RecoveryDataAvailable is false when no measured recovery signal (sleep,
	// readiness, scored resting HR) exists and the decision fell back to the
	// weather plus any self-report.
	RecoveryDataAvailable bool        `json:"recovery_data_available"`
	Kind                  VerdictKind `json:"verdict_kind"`
	Verdict               string      `json:"verdict"`
	Advice                string      `json:"advice"`
}

// evaluateRecovery scores each recovery reading independently.
func evaluateRecovery(r *RecoveryData) []Factor {
	return []Factor{
		evaluateReadiness(r.ReadinessScore, r.ReadinessIsPlaceholder, r.ReadinessBreakdown),
		evaluateSleep(r.SleepHours, r.TrackerNotWorn, r.SleepIsPlaceholder, r.sleepDetail()),
		evaluateRestingHR(r.RestingHR, r.RHRBaseline, r.RestingHRIsPlaceholder, r.RHRBaselineIsPlaceholder),
		evaluateHRV(r.HRV),
		evaluateRespiratoryRate(r.RespiratoryRate),
		evaluateSkinTemp(r.SkinTempDelta),
		evaluateExhaustion(r.FeelingExhausted, r.fatigueCounts()),
	}
}

// fatigueCounts reports whether self-reported tiredness should affect today's
// decision: it's ignored when the readiness score says you're fine (only
// sickness overrides a good score), but counts when the score is low or
// there's no score to go by.
func (r *RecoveryData) fatigueCounts() bool {
	if !r.FeelingExhausted {
		return false
	}
	return r.TrackerNotWorn || r.ReadinessIsPlaceholder || r.ReadinessScore < normalReadinessThreshold
}

// evaluateReadiness uses Fitbit's published bands: 0-29 low, 30-64
// moderate, 65-100 high. breakdown is non-empty when the score is our own
// estimate rather than Fitbit's.
func evaluateReadiness(score int, isPlaceholder bool, breakdown string) Factor {
	if isPlaceholder {
		return Factor{Name: "Readiness Score", Unit: "pts", IsPlaceholder: true,
			Note: "Not enough recent nights with the tracker to estimate it."}
	}
	f := Factor{Name: "Readiness Score", Value: float64(score), Unit: "pts"}
	switch {
	case score < 30:
		f.Severity = Hazardous
		f.Note = "Low readiness; body signals it needs rest."
	case score < 65:
		f.Severity = Caution
		f.Note = "Moderate readiness; consider an easier session."
	default:
		f.Severity = Good
		f.Note = "High readiness; supports a normal training load."
	}
	if breakdown != "" {
		f.Note += " Estimated from " + breakdown + "."
	}
	return f
}

// sleepDetail describes last night's sleep for the report, e.g.
// "6h 22m asleep, 01:44-09:09".
func (r *RecoveryData) sleepDetail() string {
	minutes := r.SleepMinutes
	if minutes == 0 {
		minutes = int(math.Round(r.SleepHours * 60)) // e.g. from recovery.json
	}
	detail := fmt.Sprintf("%dh %02dm asleep", minutes/60, minutes%60)
	switch {
	case !r.SleepStart.IsZero():
		detail += fmt.Sprintf(", %s-%s", r.SleepStart.Format("15:04"), r.SleepEnd.Format("15:04"))
	case r.SleepEstimated:
		detail += " (estimated from steps)"
	}
	return detail
}

// evaluateSleep scores last night's sleep; detail (see sleepDetail) opens
// the note so the report shows the exact time asleep.
func evaluateSleep(hours float64, trackerNotWorn bool, isPlaceholder bool, detail string) Factor {
	if trackerNotWorn {
		return Factor{Name: "Sleep", Unit: "hrs", IsPlaceholder: true,
			Note: "Tracker not worn; sleep data unavailable."}
	}
	if isPlaceholder {
		return Factor{Name: "Sleep", Unit: "hrs", IsPlaceholder: true,
			Note: "No sleep data available."}
	}

	f := Factor{Name: "Sleep", Value: hours, Unit: "hrs"}
	switch {
	case hours < 4:
		f.Severity = Hazardous
		f.Note = "Severely under-slept; high injury and illness risk."
	case hours < 6:
		f.Severity = Caution
		f.Note = "Short on sleep; recovery is likely incomplete."
	default:
		f.Severity = Good
		f.Note = "Sleep duration supports normal training."
	}
	f.Note = detail + ". " + f.Note
	return f
}

func evaluateRestingHR(restingHR, baseline int, isPlaceholder, baselineIsPlaceholder bool) Factor {
	if isPlaceholder {
		return Factor{Name: "Resting Heart Rate", Unit: "bpm", IsPlaceholder: true,
			Note: "No heart-rate data available."}
	}
	f := Factor{Name: "Resting Heart Rate", Value: float64(restingHR), Unit: "bpm"}
	if baselineIsPlaceholder || baseline <= 0 {
		// A real reading with nothing real to compare it to: show it, but
		// never let it drive the verdict.
		f.Unscored = true
		f.Note = "No personal RHR baseline available; shown for reference only."
		return f
	}
	delta := restingHR - baseline
	switch {
	case delta >= 10:
		f.Severity = Hazardous
		f.Note = fmt.Sprintf("Resting HR is %d bpm above baseline; possible illness or overtraining.", delta)
	case delta >= 5:
		f.Severity = Caution
		f.Note = fmt.Sprintf("Resting HR is %d bpm above baseline; keep an eye on how you feel.", delta)
	default:
		f.Severity = Good
		f.Note = "Resting heart rate is close to baseline."
	}
	return f
}

func evaluateExhaustion(feelingExhausted, counts bool) Factor {
	// This is a direct self-report (the -exhausted flag or feeling_exhausted
	// in recovery.json), not a synced reading. It never counts as "measured"
	// recovery data on its own - see hasMeasuredRecoverySignal.
	f := Factor{Name: "Subjective Fatigue", Unit: "bool", SelfReported: true}
	switch {
	case feelingExhausted && counts:
		f.Value = 1
		f.Severity = Caution
		f.Note = "Self-reported exhaustion; listen to your body."
	case feelingExhausted:
		f.Value = 1
		f.Severity = Good
		f.Note = "Self-reported exhaustion; your readiness score is fine, so it doesn't change today's plan."
	default:
		f.Value = 0
		f.Severity = Good
		f.Note = "No self-reported exhaustion."
	}
	return f
}

// overallSeverity returns the single most restrictive severity in a set of
// factors, ignoring placeholders and unscored readings - a fake "GOOD" must
// never mask missing data.
func overallSeverity(factors []Factor) Severity {
	worst := Good
	for _, f := range factors {
		if f.IsPlaceholder || f.Unscored {
			continue
		}
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// hasMeasuredRecoverySignal reports whether at least one recovery factor is a
// real, scored measurement. Placeholders, unscored readings and self-reports
// don't count.
func hasMeasuredRecoverySignal(factors []Factor) bool {
	for _, f := range factors {
		if !f.IsPlaceholder && !f.Unscored && !f.SelfReported {
			return true
		}
	}
	return false
}

// buildVerdict turns weather severity into a verdict and advice (used when
// there is no measured recovery data, e.g. the tracker wasn't worn).
func buildVerdict(weatherSev Severity) (VerdictKind, string, string) {
	switch weatherSev {
	case Hazardous:
		return VerdictSkipWeather, "Skip or move indoors", "Weather conditions are unsafe for outdoor training. Consider an indoor session instead, or rest entirely."
	case Caution:
		return VerdictAdjustForWeather, "Adjust for the weather", "Conditions are workable but not ideal. Adjust pace, hydration, or clothing accordingly."
	default:
		return VerdictGo, "Go for it", "Weather conditions look good. This is a solid day for your planned workout."
	}
}

// BuildAgentReport combines weather and recovery data into a full report.
func BuildAgentReport(weather *WeatherResponse, recovery *RecoveryData) *AgentReport {
	weatherFactors := evaluateWeather(weather)
	recoveryFactors := evaluateRecovery(recovery)

	weatherSev := overallSeverity(weatherFactors)
	recoverySev := overallSeverity(recoveryFactors)
	measured := !recovery.TrackerNotWorn && hasMeasuredRecoverySignal(recoveryFactors)

	const noDataNote = " Note: no measured recovery data (sleep, readiness, resting HR) is available"
	var kind VerdictKind
	var verdict, advice string
	switch {
	case measured:
		kind, verdict, advice = buildVerdictCombined(weatherSev, recoverySev)
	case recovery.FeelingExhausted:
		// No measurements, but the user said they're exhausted - that's a
		// real signal and must not be dropped.
		kind, verdict, advice = buildVerdictCombined(weatherSev, Caution)
		advice += noDataNote + "; this is based on the weather and your self-reported fatigue only."
	default:
		// Nothing real to judge recovery by - decide on weather alone and
		// say so, instead of claiming recovery "looks good".
		kind, verdict, advice = buildVerdict(weatherSev)
		advice += noDataNote + "; this is based on the weather only."
	}

	return &AgentReport{
		WeatherFactors:        weatherFactors,
		RecoveryFactors:       recoveryFactors,
		WeatherSeverity:       weatherSev,
		RecoverySeverity:      recoverySev,
		RecoveryDataAvailable: measured,
		Kind:                  kind,
		Verdict:               verdict,
		Advice:                advice,
	}
}

// buildVerdictCombined weighs weather and recovery together when both carry
// at least some real signal.
func buildVerdictCombined(weatherSev, recoverySev Severity) (VerdictKind, string, string) {
	overall := weatherSev
	if recoverySev > overall {
		overall = recoverySev
	}

	switch overall {
	case Hazardous:
		if recoverySev == Hazardous {
			return VerdictSkipRecovery, "Skip today's workout", "Your recovery signals are in the red zone. Take a full rest day, hydrate, and reassess tomorrow."
		}
		return VerdictSkipWeather, "Skip or move indoors", "Weather conditions are unsafe for outdoor training. Consider an indoor session instead, or rest entirely."
	case Caution:
		if recoverySev == Caution && weatherSev == Caution {
			return VerdictLight, "Light workout only", "Both your recovery and the weather suggest easing off. Keep today's session short and low intensity."
		}
		if recoverySev == Caution {
			return VerdictLight, "Light workout only", "Your body hasn't fully recovered. Scale back intensity and volume today."
		}
		return VerdictAdjustForWeather, "Adjust for the weather", "Conditions are workable but not ideal. Adjust pace, hydration, or clothing accordingly."
	default:
		return VerdictGo, "Go for it", "Weather and recovery both look good. This is a solid day for your planned workout."
	}
}

func printFactors(title string, factors []Factor) {
	fmt.Printf("\n%s:\n", title)
	for _, f := range factors {
		switch {
		case f.IsPlaceholder:
			fmt.Printf("  - %-20s %8s %-6s [%s] %s\n", f.Name, "-", "-", "N/A", f.Note)
		case f.Unscored:
			fmt.Printf("  - %-20s %8.1f %-6s [%s] %s\n", f.Name, f.Value, f.Unit, "N/A", f.Note)
		default:
			fmt.Printf("  - %-20s %8.1f %-6s [%s] %s\n", f.Name, f.Value, f.Unit, f.Severity, f.Note)
		}
	}
}

func printReport(report *AgentReport, recoveryWarnings []string) {
	if len(recoveryWarnings) > 0 {
		fmt.Println("\n⚠ Recovery data caveats:")
		for _, w := range recoveryWarnings {
			fmt.Printf("  - %s\n", w)
		}
	}

	printFactors("Weather", report.WeatherFactors)
	printFactors("Recovery", report.RecoveryFactors)

	fmt.Printf("\nOverall weather severity:  %s\n", report.WeatherSeverity)
	switch {
	case report.RecoveryDataAvailable:
		fmt.Printf("Overall recovery severity: %s\n", report.RecoverySeverity)
	case report.RecoverySeverity > Good:
		fmt.Printf("Overall recovery severity: %s (self-reported fatigue only; no measured recovery data)\n", report.RecoverySeverity)
	default:
		fmt.Println("Overall recovery severity: UNKNOWN (no measured recovery data)")
	}
	fmt.Printf("\n=== Verdict: %s ===\n%s\n", report.Verdict, report.Advice)
}
