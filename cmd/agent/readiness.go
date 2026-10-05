package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------
// Estimated readiness score.
//
// Fitbit's Daily Readiness Score is not exposed by the Google Health API and
// its formula is proprietary. What Fitbit does publish about it:
//   - it combines last night's heart rate variability (HRV), resting heart
//     rate (RHR) and the past week's sleep;
//   - each is compared with your own recent baseline, not population norms;
//   - you need about 7 nights with the tracker before you get a score;
//   - 0-29 is low, 30-64 moderate, 65-100 high.
//
// estimateReadiness follows those rules. The weights and scaling below are
// our own choices, so the number will not match the Fitbit app exactly -
// treat it as the same kind of signal, not the same value.
// ---------------------------------------------------------------------

const (
	readinessHRVWeight   = 0.45
	readinessRHRWeight   = 0.25
	readinessSleepWeight = 0.30

	// Nights of history (besides last night) needed for a baseline.
	minReadinessBaselineNights = 7
	// Days of history the baselines are computed from.
	nightlyBaselineWindowDays = 30

	// A reading exactly at your baseline scores this, and every standard
	// deviation away from it moves the score by readinessPointsPerSD.
	readinessAtBaseline  = 75
	readinessPointsPerSD = 15
	// Floors for the baseline standard deviation, so a very steady baseline
	// doesn't turn normal day-to-day noise into a big swing: HRV as a share
	// of its mean, resting HR in bpm (a 1-2 bpm change is noise).
	minHRVSDShare = 0.05
	minRHRSDBPM   = 2

	// Sleep scores 100 at sleepNeedMinutes and loses a point for every
	// sleepMinutesPerPoint below it (7h -> 85, 6h -> 70, 5h -> 55).
	sleepNeedMinutes     = 8 * 60
	sleepMinutesPerPoint = 4
	// Last night counts more than the rest of the week.
	lastNightSleepShare = 0.6
)

// dailyValue is one day's value of a daily Google Health metric.
type dailyValue struct {
	date  time.Time
	value float64
}

// dailyMetric is a daily reading compared with the user's own history. The
// zero value means "not available".
type dailyMetric struct {
	Available    bool
	Date         time.Time
	Value        float64
	BaselineMean float64
	BaselineSD   float64
	BaselineDays int
}

// z returns how many standard deviations Value is above the baseline mean.
// minSD keeps an unusually steady baseline from turning tiny changes into
// huge swings.
func (m dailyMetric) z(minSD float64) float64 {
	return (m.Value - m.BaselineMean) / math.Max(m.BaselineSD, minSD)
}

// latestAgainstBaseline returns the newest value in series (newest first)
// if it is dated notBefore or later, with the mean and standard deviation of
// all the earlier values as its baseline.
func latestAgainstBaseline(series []dailyValue, notBefore time.Time) dailyMetric {
	if len(series) == 0 || series[0].date.Before(notBefore) {
		return dailyMetric{}
	}
	m := dailyMetric{Available: true, Date: series[0].date, Value: series[0].value}
	history := series[1:]
	m.BaselineDays = len(history)
	if len(history) == 0 {
		return m
	}
	for _, d := range history {
		m.BaselineMean += d.value
	}
	m.BaselineMean /= float64(len(history))
	for _, d := range history {
		m.BaselineSD += (d.value - m.BaselineMean) * (d.value - m.BaselineMean)
	}
	m.BaselineSD = math.Sqrt(m.BaselineSD / float64(len(history)))
	return m
}

type healthDate struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

// healthFloat is a JSON double from Google Health. Values that aren't finite
// arrive as strings ("NaN", "Infinity"), e.g. baselineTemperatureCelsius
// before Fitbit has a temperature baseline; they decode to NaN/±Inf.
type healthFloat float64

func (f *healthFloat) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("decode number %s: %w", data, err)
	}
	*f = healthFloat(v)
	return nil
}

// valid reports whether f is a real, positive reading.
func (f healthFloat) valid() bool {
	return f > 0 && !math.IsInf(float64(f), 0) && !math.IsNaN(float64(f))
}

// fetchDailySeries lists a daily Google Health data type from since onwards
// and returns one value per day, newest first. value extracts the day and
// the value from a data point; ok=false skips the point.
func fetchDailySeries[T any](client *http.Client, dataType string, since time.Time, value func(T) (day healthDate, v float64, ok bool)) ([]dailyValue, error) {
	filter := fmt.Sprintf(`%s.date >= %q`, strings.ReplaceAll(dataType, "-", "_"), since.Format("2006-01-02"))
	points, err := listHealthDataPoints[T](client, dataType, filter, "")
	if err != nil {
		return nil, err
	}
	seen := map[time.Time]bool{}
	var series []dailyValue
	for _, p := range points {
		d, v, ok := value(p)
		if !ok {
			continue
		}
		date := time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, since.Location())
		if seen[date] {
			continue // one value per day, even with several sources
		}
		seen[date] = true
		series = append(series, dailyValue{date: date, value: v})
	}
	sort.Slice(series, func(i, j int) bool { return series[i].date.After(series[j].date) })
	return series, nil
}

// nightlyMetrics holds last night's HRV, respiratory rate and skin
// temperature. Each is left unavailable if Fitbit has no value for today.
type nightlyMetrics struct {
	HRV             dailyMetric
	RespiratoryRate dailyMetric
	SkinTempDelta   dailyMetric
}

// fetchNightlyMetrics loads the metrics Fitbit derives from last night's
// sleep. Fitbit dates them by the day you woke up, so only today's values
// describe last night. A failed request leaves that metric unavailable and
// is reported in the returned error; the others are still returned.
func fetchNightlyMetrics(client *http.Client, now time.Time) (nightlyMetrics, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since := today.AddDate(0, 0, -nightlyBaselineWindowDays)
	var metrics nightlyMetrics
	var errs []error

	type hrvPoint struct {
		DailyHeartRateVariability struct {
			Date  healthDate  `json:"date"`
			RMSSD healthFloat `json:"averageHeartRateVariabilityMilliseconds"`
		} `json:"dailyHeartRateVariability"`
	}
	hrv, err := fetchDailySeries(client, "daily-heart-rate-variability", since, func(p hrvPoint) (healthDate, float64, bool) {
		d := p.DailyHeartRateVariability
		return d.Date, float64(d.RMSSD), d.RMSSD.valid()
	})
	if err != nil {
		errs = append(errs, err)
	} else {
		metrics.HRV = latestAgainstBaseline(hrv, today)
	}

	type respiratoryPoint struct {
		DailyRespiratoryRate struct {
			Date             healthDate  `json:"date"`
			BreathsPerMinute healthFloat `json:"breathsPerMinute"`
		} `json:"dailyRespiratoryRate"`
	}
	respiratory, err := fetchDailySeries(client, "daily-respiratory-rate", since, func(p respiratoryPoint) (healthDate, float64, bool) {
		d := p.DailyRespiratoryRate
		return d.Date, float64(d.BreathsPerMinute), d.BreathsPerMinute.valid()
	})
	if err != nil {
		errs = append(errs, err)
	} else {
		metrics.RespiratoryRate = latestAgainstBaseline(respiratory, today)
	}

	// Fitbit ships its own personal baseline with each night's temperature,
	// so the delta against it is used directly.
	type skinTempPoint struct {
		DailySleepTemperatureDerivations struct {
			Date     healthDate  `json:"date"`
			Nightly  healthFloat `json:"nightlyTemperatureCelsius"`
			Baseline healthFloat `json:"baselineTemperatureCelsius"`
		} `json:"dailySleepTemperatureDerivations"`
	}
	skinTemp, err := fetchDailySeries(client, "daily-sleep-temperature-derivations", since, func(p skinTempPoint) (healthDate, float64, bool) {
		d := p.DailySleepTemperatureDerivations
		return d.Date, float64(d.Nightly - d.Baseline), d.Nightly.valid() && d.Baseline.valid()
	})
	if err != nil {
		errs = append(errs, err)
	} else {
		metrics.SkinTempDelta = latestAgainstBaseline(skinTemp, today)
	}

	return metrics, errors.Join(errs...)
}

// readinessInputs are the readings estimateReadiness combines.
type readinessInputs struct {
	HRV dailyMetric
	RHR dailyMetric
	// Minutes asleep last night and averaged over the past week's nights.
	LastNightSleepMinutes float64
	WeekAvgSleepMinutes   float64
}

// readinessResult is the estimated score and its three components (0-100).
type readinessResult struct {
	Score, HRV, RHR, Sleep int
}

func (r readinessResult) breakdown() string {
	return fmt.Sprintf("HRV %d, resting HR %d and sleep %d", r.HRV, r.RHR, r.Sleep)
}

// estimateReadiness combines HRV, resting HR and sleep into a 1-100 score
// the way Fitbit describes its Daily Readiness Score (see the top of this
// file). It returns an error explaining what's missing when a score can't
// be given.
func estimateReadiness(in readinessInputs) (readinessResult, error) {
	switch {
	case !in.HRV.Available:
		return readinessResult{}, errors.New("no HRV from last night")
	case in.HRV.BaselineDays < minReadinessBaselineNights:
		return readinessResult{}, fmt.Errorf("only %d earlier night(s) of HRV (need %d)", in.HRV.BaselineDays, minReadinessBaselineNights)
	case !in.RHR.Available:
		return readinessResult{}, errors.New("no recent resting heart rate")
	case in.RHR.BaselineDays < minReadinessBaselineNights:
		return readinessResult{}, fmt.Errorf("only %d earlier day(s) of resting HR (need %d)", in.RHR.BaselineDays, minReadinessBaselineNights)
	case in.LastNightSleepMinutes <= 0:
		return readinessResult{}, errors.New("no sleep recorded last night")
	}

	// Higher HRV than usual is good; higher resting HR than usual is bad.
	hrv := baselineComponent(in.HRV.z(minHRVSDShare * in.HRV.BaselineMean))
	rhr := baselineComponent(-in.RHR.z(minRHRSDBPM))
	week := in.WeekAvgSleepMinutes
	if week <= 0 {
		week = in.LastNightSleepMinutes
	}
	sleep := lastNightSleepShare*sleepComponent(in.LastNightSleepMinutes) +
		(1-lastNightSleepShare)*sleepComponent(week)

	score := readinessHRVWeight*hrv + readinessRHRWeight*rhr + readinessSleepWeight*sleep
	return readinessResult{
		Score: int(math.Round(clamp(score, 1, 100))),
		HRV:   int(math.Round(hrv)),
		RHR:   int(math.Round(rhr)),
		Sleep: int(math.Round(sleep)),
	}, nil
}

func baselineComponent(z float64) float64 {
	return clamp(readinessAtBaseline+readinessPointsPerSD*z, 0, 100)
}

func sleepComponent(minutes float64) float64 {
	return clamp(100-math.Max(0, sleepNeedMinutes-minutes)/sleepMinutesPerPoint, 0, 100)
}

func clamp(v, lo, hi float64) float64 {
	return math.Min(hi, math.Max(lo, v))
}

func evaluateHRV(m dailyMetric) Factor {
	if !m.Available {
		return Factor{Name: "HRV", Unit: "ms", IsPlaceholder: true,
			Note: "No HRV from last night (needs a night's sleep with the tracker)."}
	}
	f := Factor{Name: "HRV", Value: m.Value, Unit: "ms"}
	if m.BaselineDays < minReadinessBaselineNights {
		f.Unscored = true
		f.Note = fmt.Sprintf("Only %d earlier night(s) for a personal baseline (need %d); shown for reference only.",
			m.BaselineDays, minReadinessBaselineNights)
		return f
	}
	z := m.z(minHRVSDShare * m.BaselineMean)
	pct := (m.Value - m.BaselineMean) / m.BaselineMean * 100
	switch {
	case z <= -2:
		f.Severity = Hazardous
		f.Note = fmt.Sprintf("HRV is %.0f%% below your baseline (%.0f ms); your body is under strain.", -pct, m.BaselineMean)
	case z <= -1:
		f.Severity = Caution
		f.Note = fmt.Sprintf("HRV is %.0f%% below your baseline (%.0f ms); recovery may be incomplete.", -pct, m.BaselineMean)
	default:
		f.Severity = Good
		f.Note = fmt.Sprintf("HRV is within your normal range (baseline %.0f ms).", m.BaselineMean)
	}
	return f
}

func evaluateRespiratoryRate(m dailyMetric) Factor {
	if !m.Available {
		return Factor{Name: "Respiratory Rate", Unit: "br/min", IsPlaceholder: true,
			Note: "No breathing rate from last night."}
	}
	f := Factor{Name: "Respiratory Rate", Value: m.Value, Unit: "br/min"}
	if m.BaselineDays < minReadinessBaselineNights {
		f.Unscored = true
		f.Note = "Not enough earlier nights for a personal baseline; shown for reference only."
		return f
	}
	delta := m.Value - m.BaselineMean
	switch {
	case delta >= 3:
		f.Severity = Hazardous
		f.Note = fmt.Sprintf("Breathing rate is %.1f above your baseline; a common early sign of illness.", delta)
	case delta >= 2:
		f.Severity = Caution
		f.Note = fmt.Sprintf("Breathing rate is %.1f above your baseline; watch for signs of illness.", delta)
	default:
		f.Severity = Good
		f.Note = fmt.Sprintf("Breathing rate is normal for you (baseline %.1f).", m.BaselineMean)
	}
	return f
}

func evaluateSkinTemp(m dailyMetric) Factor {
	if !m.Available {
		return Factor{Name: "Skin Temp vs Base", Unit: "°C", IsPlaceholder: true,
			Note: "No skin temperature from last night."}
	}
	f := Factor{Name: "Skin Temp vs Base", Value: m.Value, Unit: "°C"}
	switch {
	case m.Value >= 1.0:
		f.Severity = Hazardous
		f.Note = "Skin temperature is well above your baseline; possible fever or illness."
	case m.Value >= 0.5:
		f.Severity = Caution
		f.Note = "Skin temperature is above your baseline; watch for signs of illness."
	default:
		f.Severity = Good
		f.Note = "Skin temperature is normal for you."
	}
	return f
}
