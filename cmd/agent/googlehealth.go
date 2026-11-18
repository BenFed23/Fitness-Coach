package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	recoveryFile = "recovery.json"

	googleHealthDataTypesURL = "https://health.googleapis.com/v4/users/me/dataTypes/"
	// Data from Google/Fitbit trackers only (no phone, no manual entries).
	wearablesSourceFamily = "users/me/dataSourceFamilies/google-wearables"
	// Heart rate in fewer hours than this last night means the tracker
	// wasn't worn (or only briefly), so sleep can't be judged.
	minWornNightHours = 3
	// Resting HR is scored against the mean of the earlier days in this
	// window, once at least minRHRBaselineDays of them exist.
	rhrBaselineWindowDays = 30
	minRHRBaselineDays    = 7
)

// RecoveryData represents the recovery inputs normally supplied by Fitbit.
type RecoveryData struct {
	ReadinessScore int     `json:"readiness_score"`
	SleepHours     float64 `json:"sleep_hours"`
	SleepMinutes   int     `json:"-"`
	// When last night's sleep started and ended; zero unless it comes from
	// a recorded sleep session. SleepEstimated marks a step-based estimate.
	SleepStart       time.Time `json:"-"`
	SleepEnd         time.Time `json:"-"`
	SleepEstimated   bool      `json:"-"`
	RestingHR        int       `json:"resting_hr"`
	RHRBaseline      int       `json:"rhr_baseline"`
	FeelingExhausted bool      `json:"feeling_exhausted"`
	TrackerNotWorn   bool      `json:"-"`

	// What the user reported in the chat, passed on by the bot as flags.
	// Unlike FeelingExhausted, these only affect the workout adjustment (see
	// adjustedTier), not the verdict: the bot applies them to its decision.
	FeelingSick        bool     `json:"-"`
	InPain             bool     `json:"-"`
	ReportedSleepHours *float64 `json:"-"`

	// These flags mark which fields are NOT backed by a real reading, so the
	// report can show "-" instead of a number that looks real but isn't.
	// With Google Health as the source, Readiness Score is our own estimate
	// and is a placeholder whenever there's too little data for it.
	ReadinessIsPlaceholder   bool `json:"-"`
	SleepIsPlaceholder       bool `json:"-"`
	RestingHRIsPlaceholder   bool `json:"-"`
	RHRBaselineIsPlaceholder bool `json:"-"`

	// Nightly metrics from Google Health. Unlike the fields above, their zero
	// value means "not available", so recovery.json and the mock fallback
	// never need to set them.
	HRV             dailyMetric `json:"-"`
	RespiratoryRate dailyMetric `json:"-"`
	SkinTempDelta   dailyMetric `json:"-"` // °C vs Fitbit's own baseline
	// ReadinessBreakdown is set when ReadinessScore is our own estimate (see
	// estimateReadiness) rather than a value copied from the Fitbit app.
	ReadinessBreakdown string `json:"-"`
	// ReadinessUnavailable says why estimateReadiness couldn't give a score.
	ReadinessUnavailable string `json:"-"`
	// Source is where the data came from: "google_health", "recovery.json"
	// or "mock".
	Source string `json:"-"`
	// AuthError is set when live data needs the user to authorize Google
	// again (token missing, expired or revoked).
	AuthError string `json:"-"`

	// Warnings lists any fields that could not be loaded from a live source
	// and fell back to a mock/default value. Empty when every field reflects
	// real data. Not persisted to recovery.json.
	Warnings []string `json:"-"`
}

var errNoRecoveryData = errors.New("no recovery data available")

// FetchFitbitRecovery prefers live Fitbit data from the Google Health API and
// gracefully falls back to recovery.json or mock values if authorization or
// an API call fails.
func FetchFitbitRecovery() (*RecoveryData, error) {
	config, err := googleOAuthConfig(googleHealthScopes...)
	var authErr error
	if err == nil {
		client, clientErr := googleClient(context.Background(), config, healthTokenFile)
		if errors.Is(clientErr, errNeedsAuthorization) {
			authErr = clientErr
		}
		if clientErr == nil {
			recovery, fetchErr := fetchGoogleRecovery(client)
			if fetchErr == nil {
				fmt.Println("Recovery data loaded from Google Health.")
				recovery.Source = "google_health"
				return recovery, nil
			}
			err = fmt.Errorf("fetch Google Health recovery data: %w", fetchErr)
		} else {
			err = fmt.Errorf("authorize Google: %w", clientErr)
		}
	}
	fmt.Printf("Live recovery sync unavailable (%v); using local fallback.\n", err)
	recovery, fallbackErr := loadLocalRecoveryFallback()
	if recovery != nil && authErr != nil {
		recovery.AuthError = authErr.Error()
	}
	return recovery, fallbackErr
}

func fetchGoogleRecovery(client *http.Client) (*RecoveryData, error) {
	recovery := defaultRecoveryData()
	now := time.Now()
	windowStart := now.Add(-48 * time.Hour)
	apiResponded := false

	// A 48-hour window includes the most recent overnight sleep even when its
	// start time was on the previous calendar day.
	sleepMinutes, sleepStart, sleepEnd, weekAvgSleep, sleepErr := fetchSleepMinutes(client, windowStart)
	switch {
	case sleepErr == nil:
		apiResponded = true
		recovery.SleepMinutes = sleepMinutes
		recovery.SleepHours = float64(recovery.SleepMinutes) / 60
		recovery.SleepStart, recovery.SleepEnd = sleepStart, sleepEnd
		fmt.Printf("Sleep session found: %s -> %s (%.1fh asleep)\n",
			sleepStart.Format("Jan 2 15:04"), sleepEnd.Format("Jan 2 15:04"), recovery.SleepHours)
	case errors.Is(sleepErr, errNoRecoveryData):
		apiResponded = true // The request worked; this account simply has no session.
		// Without a session, only step inactivity is left to estimate sleep
		// from - and a phone lying still overnight looks exactly like sleep.
		// So first make sure the tracker was actually worn last night.
		worn, nightStart, nightEnd, wornErr := trackerWornOvernight(client, now)
		if wornErr != nil || !worn {
			recovery.TrackerNotWorn = true
			recovery.SleepIsPlaceholder = true
			if wornErr != nil {
				recovery.Warnings = append(recovery.Warnings,
					fmt.Sprintf("No sleep session found, and checking whether the tracker was worn overnight failed (%v); sleep hours unavailable.", wornErr))
			} else {
				recovery.Warnings = append(recovery.Warnings,
					fmt.Sprintf("No sleep session found and your tracker recorded no heart rate between %s and %s. Looks like you didn't wear it overnight.",
						nightStart.Format("Jan 2 15:04"), nightEnd.Format("Jan 2 15:04")))
			}
			break
		}
		estimatedHours, estimateErr := estimateRestHoursFromSteps(client, windowStart, now)
		switch {
		case estimateErr == nil:
			if estimatedHours >= 12 {
				recovery.TrackerNotWorn = true
				recovery.SleepIsPlaceholder = true
				recovery.SleepHours = 0
				recovery.SleepMinutes = 0
				recovery.Warnings = append(recovery.Warnings,
					"No formal sleep session found and max inactivity reached (12h). Looks like you didn't wear the tracker.")
			} else {
				recovery.SleepHours = estimatedHours
				recovery.SleepMinutes = int(estimatedHours * 60)
				recovery.SleepEstimated = true
				recovery.Warnings = append(recovery.Warnings,
					fmt.Sprintf("No formal sleep session found; sleep duration estimated at %.1fh from step activity.", estimatedHours))
			}
		case errors.Is(estimateErr, errNoRecoveryData):
			// No real signal at all for sleep - don't show the mock number.
			recovery.SleepIsPlaceholder = true
			recovery.Warnings = append(recovery.Warnings,
				"No sleep or step data found in Google Health for the last 48h; sleep hours unavailable.")
		default:
			// The step-activity fallback itself failed unexpectedly (network,
			// decode, non-200 status...). Surface it instead of staying silent.
			recovery.SleepIsPlaceholder = true
			recovery.Warnings = append(recovery.Warnings,
				fmt.Sprintf("Sleep estimate from steps failed unexpectedly (%v); sleep hours unavailable.", estimateErr))
		}
	default:
		// fetchSleepMinutes failed for a reason other than "no session found"
		// - e.g. a network error, timeout, or bad HTTP response from Google.
		recovery.SleepIsPlaceholder = true
		recovery.Warnings = append(recovery.Warnings,
			fmt.Sprintf("Sleep session lookup failed unexpectedly (%v); sleep hours unavailable.", sleepErr))
	}

	// Resting heart rate is independent from sleep: always try it. Fitbit
	// computes a daily resting HR itself, and the previous days' values give
	// a personal baseline to score today's reading against.
	rhr, heartRateErr := fetchRestingHeartRate(client, now)
	switch {
	case heartRateErr == nil:
		apiResponded = true
		recovery.RestingHR = int(rhr.Value)
		if rhr.BaselineDays >= minRHRBaselineDays {
			recovery.RHRBaseline = int(math.Round(rhr.BaselineMean))
		} else {
			recovery.RHRBaselineIsPlaceholder = true
			recovery.Warnings = append(recovery.Warnings,
				fmt.Sprintf("Only %d earlier day(s) of resting HR found (need %d) for a personal baseline; resting HR is shown for reference only (not scored).",
					rhr.BaselineDays, minRHRBaselineDays))
		}
	case errors.Is(heartRateErr, errNoRecoveryData):
		apiResponded = true
		recovery.RestingHRIsPlaceholder = true
		recovery.RHRBaselineIsPlaceholder = true
		recovery.Warnings = append(recovery.Warnings,
			fmt.Sprintf("No daily resting heart rate found in Google Health (%v); resting HR unavailable.", heartRateErr))
	default:
		recovery.RestingHRIsPlaceholder = true
		recovery.RHRBaselineIsPlaceholder = true
		recovery.Warnings = append(recovery.Warnings,
			fmt.Sprintf("Resting heart rate lookup failed unexpectedly (%v); resting HR unavailable.", heartRateErr))
	}
	if !apiResponded {
		return nil, fmt.Errorf("all Google Health recovery requests failed (sleep: %v; heart rate: %v)", sleepErr, heartRateErr)
	}

	nightly, nightlyErr := fetchNightlyMetrics(client, now)
	if nightlyErr != nil {
		recovery.Warnings = append(recovery.Warnings,
			fmt.Sprintf("Some nightly metrics (HRV, breathing rate, skin temperature) could not be loaded: %v", nightlyErr))
	}
	recovery.HRV = nightly.HRV
	recovery.RespiratoryRate = nightly.RespiratoryRate
	recovery.SkinTempDelta = nightly.SkinTempDelta

	// Fitbit's own Readiness Score isn't in the API, so estimate one from the
	// same inputs. Only a real sleep session counts - not a step estimate.
	var lastNightSleep float64
	if sleepErr == nil {
		lastNightSleep = float64(recovery.SleepMinutes)
	}
	readiness, readinessErr := estimateReadiness(readinessInputs{
		HRV:                   nightly.HRV,
		RHR:                   rhr,
		LastNightSleepMinutes: lastNightSleep,
		WeekAvgSleepMinutes:   weekAvgSleep,
	})
	if readinessErr == nil {
		recovery.ReadinessScore = readiness.Score
		recovery.ReadinessBreakdown = readiness.breakdown()
	} else {
		recovery.ReadinessIsPlaceholder = true
		recovery.ReadinessUnavailable = readinessErr.Error()
		recovery.Warnings = append(recovery.Warnings,
			fmt.Sprintf("Readiness score can't be estimated today (%v).", readinessErr))
	}
	return recovery, nil
}

// healthDataSource identifies where a Google Health data point came from
// (e.g. FITBIT for data synced from a Fitbit tracker).
type healthDataSource struct {
	Platform        string `json:"platform"`
	RecordingMethod string `json:"recordingMethod"`
}

func (s healthDataSource) String() string {
	return firstNonEmpty(strings.TrimSpace(s.Platform+" "+s.RecordingMethod), "unknown source")
}

// listHealthDataPoints fetches every page of a Google Health dataPoints.list
// query. filter follows https://google.aip.dev/160, e.g.
// `steps.interval.start_time >= "2024-08-14T00:00:00Z"`. sourceFamily
// optionally restricts the data sources (e.g. wearablesSourceFamily); it is
// not supported for sleep.
func listHealthDataPoints[T any](client *http.Client, dataType, filter, sourceFamily string) ([]T, error) {
	var points []T
	pageToken := ""
	for {
		query := url.Values{"filter": {filter}, "pageSize": {"10000"}}
		if sourceFamily != "" {
			query.Set("dataSourceFamily", sourceFamily)
		}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var page struct {
			DataPoints    []T    `json:"dataPoints"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := getJSON(client, googleHealthDataTypesURL+dataType+"/dataPoints?"+query.Encode(), &page); err != nil {
			return nil, fmt.Errorf("list %s data points: %w", dataType, err)
		}
		points = append(points, page.DataPoints...)
		if page.NextPageToken == "" {
			return points, nil
		}
		pageToken = page.NextPageToken
	}
}

// fetchSleepMinutes returns the minutes actually asleep (awake stages
// excluded) in the most recent main sleep that ended after windowStart,
// together with the session's start/end time, and the average minutes asleep
// per night over the past week (0 if there's no sleep that week). Naps are
// ignored; if no session of a night is flagged as main sleep, the longest one
// is used.
func fetchSleepMinutes(client *http.Client, windowStart time.Time) (int, time.Time, time.Time, float64, error) {
	type sleepDataPoint struct {
		Sleep struct {
			Interval struct {
				StartTime time.Time `json:"startTime"`
				EndTime   time.Time `json:"endTime"`
			} `json:"interval"`
			Summary struct {
				MinutesAsleep int64 `json:"minutesAsleep,string"`
			} `json:"summary"`
			Metadata struct {
				MainSleep bool `json:"mainSleep"`
				Nap       bool `json:"nap"`
			} `json:"metadata"`
		} `json:"sleep"`
		DataSource healthDataSource `json:"dataSource"`
	}
	weekStart := windowStart.Add(-5 * 24 * time.Hour)
	filter := fmt.Sprintf(`sleep.interval.end_time >= %q`, weekStart.UTC().Format(time.RFC3339))
	points, err := listHealthDataPoints[sleepDataPoint](client, "sleep", filter, "")
	if err != nil {
		return 0, time.Time{}, time.Time{}, 0, fmt.Errorf("fetch sleep sessions: %w", err)
	}

	// Pick one session per night, keyed by the day you woke up.
	nights := map[time.Time]*sleepDataPoint{}
	var wakeDays []time.Time
	for i := range points {
		p := &points[i]
		if p.Sleep.Metadata.Nap || !p.Sleep.Interval.EndTime.After(p.Sleep.Interval.StartTime) {
			continue
		}
		end := p.Sleep.Interval.EndTime.Local()
		day := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.Local)
		current, ok := nights[day]
		switch {
		case !ok:
			wakeDays = append(wakeDays, day)
			nights[day] = p
		case current.Sleep.Metadata.MainSleep:
		case p.Sleep.Metadata.MainSleep || p.Sleep.Interval.EndTime.Sub(p.Sleep.Interval.StartTime) >
			current.Sleep.Interval.EndTime.Sub(current.Sleep.Interval.StartTime):
			nights[day] = p
		}
	}
	minutesAsleep := func(p *sleepDataPoint) int {
		if p.Sleep.Summary.MinutesAsleep > 0 {
			return int(p.Sleep.Summary.MinutesAsleep)
		}
		// Sleep stages not processed yet - fall back to the time in bed.
		return int(p.Sleep.Interval.EndTime.Sub(p.Sleep.Interval.StartTime) / time.Minute)
	}

	var weekTotal int
	for _, p := range nights {
		weekTotal += minutesAsleep(p)
	}
	var weekAvg float64
	if len(nights) > 0 {
		weekAvg = float64(weekTotal) / float64(len(nights))
	}

	sort.Slice(wakeDays, func(i, j int) bool { return wakeDays[i].After(wakeDays[j]) })
	if len(wakeDays) == 0 || nights[wakeDays[0]].Sleep.Interval.EndTime.Before(windowStart) {
		return 0, time.Time{}, time.Time{}, weekAvg, fmt.Errorf("%w: no sleep session found in the last 48 hours", errNoRecoveryData)
	}
	chosen := nights[wakeDays[0]]
	start, end := chosen.Sleep.Interval.StartTime.Local(), chosen.Sleep.Interval.EndTime.Local()
	if chosen.Sleep.Summary.MinutesAsleep <= 0 {
		fmt.Println("Note: sleep stages are not processed yet; using total time in bed instead of time asleep.")
	}
	fmt.Printf("Sleep session source: %s\n", chosen.DataSource)
	return minutesAsleep(chosen), start, end, weekAvg, nil
}

// trackerWornOvernight reports whether a Google or Fitbit wearable recorded
// heart rate in at least minWornNightHours distinct hours of last night's
// window (22:00-07:00 local, cut off at now). It also returns that window.
func trackerWornOvernight(client *http.Client, now time.Time) (bool, time.Time, time.Time, error) {
	type heartRateDataPoint struct {
		HeartRate struct {
			SampleTime struct {
				PhysicalTime time.Time `json:"physicalTime"`
			} `json:"sampleTime"`
		} `json:"heartRate"`
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start, end := today.Add(-2*time.Hour), today.Add(7*time.Hour)
	if end.After(now) {
		end = now // Run in the small hours: "last night" is still in progress.
	}
	filter := fmt.Sprintf(`heart_rate.sample_time.physical_time >= %q AND heart_rate.sample_time.physical_time < %q`,
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	points, err := listHealthDataPoints[heartRateDataPoint](client, "heart-rate", filter, wearablesSourceFamily)
	if err != nil {
		return false, start, end, fmt.Errorf("fetch overnight heart rate: %w", err)
	}

	hours := map[int]bool{}
	for _, p := range points {
		hours[int(p.HeartRate.SampleTime.PhysicalTime.Sub(start)/time.Hour)] = true
	}
	required := min(minWornNightHours, max(1, int(end.Sub(start).Hours())))
	return len(hours) >= required, start, end, nil
}

// estimateRestHoursFromSteps identifies the longest low-activity period in
// hourly step buckets when Google Health has no explicit sleep session.
func estimateRestHoursFromSteps(client *http.Client, start, end time.Time) (float64, error) {
	type stepsDataPoint struct {
		Steps struct {
			Interval struct {
				StartTime time.Time `json:"startTime"`
			} `json:"interval"`
			Count int64 `json:"count,string"`
		} `json:"steps"`
	}
	filter := fmt.Sprintf(`steps.interval.start_time >= %q AND steps.interval.start_time < %q`,
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	// Only the tracker's own steps: a phone left on the nightstand says
	// nothing about whether you were asleep.
	points, err := listHealthDataPoints[stepsDataPoint](client, "steps", filter, wearablesSourceFamily)
	if err != nil {
		return 0, fmt.Errorf("fetch step activity: %w", err)
	}
	if len(points) == 0 {
		return 0, fmt.Errorf("%w: no step data in the last 48 hours", errNoRecoveryData)
	}

	// Hours with no data points count as zero steps.
	hourlySteps := make([]int64, int(math.Ceil(end.Sub(start).Hours())))
	for _, p := range points {
		hour := int(p.Steps.Interval.StartTime.Sub(start) / time.Hour)
		if hour >= 0 && hour < len(hourlySteps) {
			hourlySteps[hour] += p.Steps.Count
		}
	}

	longest, current := 0, 0
	for _, steps := range hourlySteps {
		if steps <= 20 {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	if longest < 3 {
		return 0, fmt.Errorf("%w: no sustained low-activity period found", errNoRecoveryData)
	}
	if longest > 12 {
		longest = 12
	}
	return float64(longest), nil
}

// fetchRestingHeartRate returns the most recent daily resting heart rate
// computed by Fitbit, against a personal baseline: the earlier days in the
// last rhrBaselineWindowDays.
func fetchRestingHeartRate(client *http.Client, now time.Time) (dailyMetric, error) {
	type restingHRDataPoint struct {
		DailyRestingHeartRate struct {
			Date           healthDate `json:"date"`
			BeatsPerMinute int64      `json:"beatsPerMinute,string"`
		} `json:"dailyRestingHeartRate"`
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	series, err := fetchDailySeries(client, "daily-resting-heart-rate", today.AddDate(0, 0, -rhrBaselineWindowDays),
		func(p restingHRDataPoint) (healthDate, float64, bool) {
			d := p.DailyRestingHeartRate
			return d.Date, float64(d.BeatsPerMinute), d.BeatsPerMinute > 0
		})
	if err != nil {
		return dailyMetric{}, fmt.Errorf("fetch resting heart rate: %w", err)
	}

	// Only today's or yesterday's value describes how recovered you are now.
	m := latestAgainstBaseline(series, today.AddDate(0, 0, -1))
	if !m.Available {
		return dailyMetric{}, fmt.Errorf("%w: no resting heart rate for today or yesterday", errNoRecoveryData)
	}
	fmt.Printf("Resting heart rate: %.0f bpm on %s\n", m.Value, m.Date.Format("Jan 2"))
	return m, nil
}

func loadLocalRecoveryFallback() (*RecoveryData, error) {
	data, err := os.ReadFile(recoveryFile)
	if err == nil {
		var recovery RecoveryData
		if err := json.Unmarshal(data, &recovery); err == nil {
			fmt.Printf("Recovery data loaded from local %s.\n", recoveryFile)
			recovery.Source = recoveryFile
			return &recovery, nil
		}
		fmt.Printf("Local %s is invalid; using default mock recovery values.\n", recoveryFile)
	} else if os.IsNotExist(err) {
		fmt.Printf("Local %s not found; using default mock recovery values.\n", recoveryFile)
	} else {
		fmt.Printf("Could not open local %s; using default mock recovery values.\n", recoveryFile)
	}
	mock := defaultRecoveryData()
	mock.Source = "mock"
	mock.ReadinessIsPlaceholder = true
	mock.SleepIsPlaceholder = true
	mock.RestingHRIsPlaceholder = true
	mock.RHRBaselineIsPlaceholder = true
	mock.Warnings = append(mock.Warnings,
		fmt.Sprintf("No live or local recovery data available; every field below is unavailable, not today's reading (see %s).", recoveryFile))
	return mock, nil
}

func defaultRecoveryData() *RecoveryData {
	return &RecoveryData{
		ReadinessScore:   78,
		SleepHours:       6.2,
		RestingHR:        57,
		RHRBaseline:      56,
		FeelingExhausted: false,
	}
}
