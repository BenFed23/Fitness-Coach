package engine

import (
	"fitness-agent/internal/model"
)

// EvaluateWeather analyzes current weather conditions against predefined safety thresholds.
func EvaluateWeather(w *model.WeatherResponse, t EvaluationThresholds) []model.EvaluationFactor {
	if w == nil {
		return []model.EvaluationFactor{
			{Name: "Temperature", Severity: model.SeverityGood, Note: "Weather data unavailable", IsPlaceholder: true, SeverityExcluded: true},
			{Name: "Humidity", Severity: model.SeverityGood, Note: "Weather data unavailable", IsPlaceholder: true, SeverityExcluded: true},
			{Name: "Rain", Severity: model.SeverityGood, Note: "Weather data unavailable", IsPlaceholder: true, SeverityExcluded: true},
			{Name: "Wind", Severity: model.SeverityGood, Note: "Weather data unavailable", IsPlaceholder: true, SeverityExcluded: true},
		}
	}

	c := w.Current
	var factors []model.EvaluationFactor

	// Temperature
	tempSev := model.SeverityGood
	tempNote := "Temperature is comfortable for outdoor/indoor training."
	if c.Temperature2m >= t.TempHazardousHighC || c.Temperature2m <= t.TempHazardousLowC {
		tempSev = model.SeverityHazardous
		tempNote = "Temperature is at hazardous levels (risk of heat stroke or hypothermia)."
	} else if c.Temperature2m >= t.TempCautionHighC || c.Temperature2m <= t.TempCautionLowC {
		tempSev = model.SeverityCaution
		tempNote = "Temperature requires caution and extra hydration."
	}
	factors = append(factors, model.EvaluationFactor{
		Name: "Temperature", Value: c.Temperature2m, Unit: "°C", Severity: tempSev, Note: tempNote,
	})

	// Humidity
	humSev := model.SeverityGood
	humNote := "Humidity is normal."
	if c.RelativeHumidity2m >= t.HumidityHazardousPct {
		humSev = model.SeverityHazardous
		humNote = "Humidity is dangerously high; sweat evaporation is impaired."
	} else if c.RelativeHumidity2m >= t.HumidityCautionPct {
		humSev = model.SeverityCaution
		humNote = "Humidity is elevated; monitor exertion."
	}
	factors = append(factors, model.EvaluationFactor{
		Name: "Humidity", Value: c.RelativeHumidity2m, Unit: "%", Severity: humSev, Note: humNote,
	})

	// Rain
	rainSev := model.SeverityGood
	rainNote := "No significant rain."
	if c.Rain >= t.RainHazardousMM {
		rainSev = model.SeverityHazardous
		rainNote = "Heavy rain conditions; outdoor running is hazardous."
	} else if c.Rain >= t.RainCautionMM {
		rainSev = model.SeverityCaution
		rainNote = "Light rain / drizzle."
	}
	factors = append(factors, model.EvaluationFactor{
		Name: "Rain", Value: c.Rain, Unit: "mm", Severity: rainSev, Note: rainNote,
	})

	// Wind
	windSev := model.SeverityGood
	windNote := "Wind is calm."
	if c.WindSpeed10m >= t.WindHazardousKMH {
		windSev = model.SeverityHazardous
		windNote = "Hazardous wind speeds."
	} else if c.WindSpeed10m >= t.WindCautionKMH {
		windSev = model.SeverityCaution
		windNote = "Elevated wind speeds."
	}
	factors = append(factors, model.EvaluationFactor{
		Name: "Wind", Value: c.WindSpeed10m, Unit: "km/h", Severity: windSev, Note: windNote,
	})

	return factors
}

// EvaluateRecovery analyzes biometric indicators (sleep, readiness score, resting heart rate delta).
func EvaluateRecovery(r *model.RecoveryData, t EvaluationThresholds) []model.EvaluationFactor {
	if r == nil {
		return []model.EvaluationFactor{
			{Name: "Readiness Score", Severity: model.SeverityGood, Note: "Recovery data unavailable", IsPlaceholder: true, SeverityExcluded: true},
			{Name: "Sleep", Severity: model.SeverityGood, Note: "Recovery data unavailable", IsPlaceholder: true, SeverityExcluded: true},
			{Name: "Resting HR Delta", Severity: model.SeverityGood, Note: "Recovery data unavailable", IsPlaceholder: true, SeverityExcluded: true},
		}
	}

	var factors []model.EvaluationFactor

	// Readiness Score
	readinessSev := model.SeverityGood
	readinessNote := "Readiness score is solid."
	if r.ReadinessIsPlaceholder {
		factors = append(factors, model.EvaluationFactor{
			Name: "Readiness Score", Value: float64(r.ReadinessScore), Unit: "pts", Severity: model.SeverityGood,
			Note: "Readiness score is placeholder", IsPlaceholder: true, SeverityExcluded: true,
		})
	} else {
		if r.ReadinessScore < t.ReadinessHazardousBelow {
			readinessSev = model.SeverityHazardous
			readinessNote = "Readiness is critically low; high systemic fatigue."
		} else if r.ReadinessScore < t.ReadinessCautionBelow {
			readinessSev = model.SeverityCaution
			readinessNote = "Readiness is below optimal."
		}
		factors = append(factors, model.EvaluationFactor{
			Name: "Readiness Score", Value: float64(r.ReadinessScore), Unit: "pts", Severity: readinessSev, Note: readinessNote,
		})
	}

	// Sleep Duration
	sleepSev := model.SeverityGood
	sleepNote := "Sleep duration is adequate for recovery and muscle protein synthesis."
	if r.SleepIsPlaceholder {
		factors = append(factors, model.EvaluationFactor{
			Name: "Sleep", Value: r.SleepHours, Unit: "hrs", Severity: model.SeverityGood,
			Note: "Sleep duration is placeholder", IsPlaceholder: true, SeverityExcluded: true,
		})
	} else {
		if r.SleepHours < t.SleepHazardousBelowHrs {
			sleepSev = model.SeverityHazardous
			sleepNote = "Critically insufficient sleep; central nervous system is fatigued."
		} else if r.SleepHours < t.SleepCautionBelowHrs {
			sleepSev = model.SeverityCaution
			sleepNote = "Suboptimal sleep duration."
		}
		factors = append(factors, model.EvaluationFactor{
			Name: "Sleep", Value: r.SleepHours, Unit: "hrs", Severity: sleepSev, Note: sleepNote,
		})
	}

	// Resting Heart Rate (Delta from Baseline)
	rhrDelta := r.RestingHR - r.RHRBaseline
	rhrSev := model.SeverityGood
	rhrNote := "Resting HR is close to personal baseline."
	if r.RestingHRIsPlaceholder || r.RHRBaselineIsPlaceholder {
		factors = append(factors, model.EvaluationFactor{
			Name: "Resting HR Delta", Value: float64(rhrDelta), Unit: "bpm", Severity: model.SeverityGood,
			Note: "RHR baseline/delta is placeholder", IsPlaceholder: true, SeverityExcluded: true,
		})
	} else {
		if rhrDelta >= t.RHRDeltaHazardousBPM {
			rhrSev = model.SeverityHazardous
			rhrNote = "Resting HR is significantly elevated; signs of acute stress or early infection."
		} else if rhrDelta >= t.RHRDeltaCautionBPM {
			rhrSev = model.SeverityCaution
			rhrNote = "Resting HR is slightly elevated."
		}
		factors = append(factors, model.EvaluationFactor{
			Name: "Resting HR Delta", Value: float64(rhrDelta), Unit: "bpm", Severity: rhrSev, Note: rhrNote,
		})
	}

	return factors
}

func overallFactorSeverity(factors []model.EvaluationFactor) model.Severity {
	worst := model.SeverityGood
	for _, f := range factors {
		if f.SeverityExcluded {
			continue
		}
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// BuildAgentReport synthesizes weather and biometric recovery data into actionable coaching advice.
func BuildAgentReport(weather *model.WeatherResponse, recovery *model.RecoveryData, t EvaluationThresholds) *model.AgentReport {
	weatherFactors := EvaluateWeather(weather, t)
	recoveryFactors := EvaluateRecovery(recovery, t)

	weatherSev := overallFactorSeverity(weatherFactors)
	recoverySev := overallFactorSeverity(recoveryFactors)

	overallSev := weatherSev
	if recoverySev > overallSev {
		overallSev = recoverySev
	}

	verdict := "Go for it"
	advice := "Weather and recovery conditions are optimal. Full 90-minute session recommended."

	switch {
	case recoverySev == model.SeverityHazardous:
		verdict = "Rest Day / Active Mobility Only"
		advice = "Recovery indicators (sleep/RHR) show acute physical exhaustion. Skip heavy lifting to prevent injury; do 15 minutes of foam rolling or walking."
	case weatherSev == model.SeverityHazardous:
		verdict = "Shift Indoors / Gym Session"
		advice = "Outdoor weather is hazardous. If you had a run planned, switch to gym strength or an indoor fallback routine."
	case overallSev == model.SeverityCaution:
		verdict = "Modify / Reduce Intensity"
		advice = "Some caution factors detected. Consider reducing working set loads by 10-15% or shortening the workout to avoid overreaching."
	case overallSev == model.SeverityGood:
		verdict = "Go for it (Green Light)"
		advice = "All systems green. Great conditions for a high-intensity 90-minute workout."
	}

	return &model.AgentReport{
		WeatherFactors:   weatherFactors,
		RecoveryFactors:  recoveryFactors,
		WeatherSeverity:  weatherSev,
		RecoverySeverity: recoverySev,
		OverallSeverity:  overallSev,
		Verdict:          verdict,
		Advice:           advice,
	}
}
