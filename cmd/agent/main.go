package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	// Embedded time zone database, so Asia/Jerusalem also loads on a server
	// or container without one installed.
	_ "time/tzdata"

	"golang.org/x/oauth2"
)

const (
	weatherAPIBase    = "https://api.open-meteo.com/v1/forecast?latitude=%f&longitude=%f&current=temperature_2m,relative_humidity_2m,rain,wind_speed_10m"
	ipLocationAPIURL  = "http://ip-api.com/json/"
	googleAuthURL     = "https://accounts.google.com/o/oauth2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	healthTokenFile   = "token_health.json"
	calendarTokenFile = "token_calendar.json"
	recoveryFile      = "recovery.json"
	callbackURL       = "http://localhost:8080/callback"
	calendarTimeZone  = "Asia/Jerusalem"

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

	// Optional development fallbacks. Prefer CLIENT_ID and CLIENT_SECRET.
	defaultClientID     = ""
	defaultClientSecret = ""
)

// IPLocation is the approximate location inferred from the public IP address.
type IPLocation struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	City      string  `json:"city"`
	Country   string  `json:"country"`
}

// CurrentWeather contains Open-Meteo's current weather measurements.
type CurrentWeather struct {
	Time               string  `json:"time"`
	Interval           int     `json:"interval"`
	Temperature2m      float64 `json:"temperature_2m"`
	RelativeHumidity2m float64 `json:"relative_humidity_2m"`
	Rain               float64 `json:"rain"`
	WindSpeed10m       float64 `json:"wind_speed_10m"`
}

type CurrentUnits struct {
	Temperature2m      string `json:"temperature_2m"`
	RelativeHumidity2m string `json:"relative_humidity_2m"`
	Rain               string `json:"rain"`
	WindSpeed10m       string `json:"wind_speed_10m"`
}

type WeatherResponse struct {
	Latitude     float64        `json:"latitude"`
	Longitude    float64        `json:"longitude"`
	Timezone     string         `json:"timezone"`
	Current      CurrentWeather `json:"current"`
	CurrentUnits CurrentUnits   `json:"current_units"`
}

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

// Severity is ordered so the highest severity is the most restrictive.
type Severity int

var errNoRecoveryData = errors.New("no recovery data available")

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

// GetCurrentLocation returns AGENT_LATITUDE/AGENT_LONGITUDE (and optional
// AGENT_CITY) when set, otherwise the approximate location of the public IP.
// On a cloud server the IP points at the data center, so set them there.
func GetCurrentLocation() (*IPLocation, error) {
	if lat, lon := os.Getenv("AGENT_LATITUDE"), os.Getenv("AGENT_LONGITUDE"); lat != "" || lon != "" {
		var loc IPLocation
		if _, err := fmt.Sscan(lat, &loc.Latitude); err != nil {
			return nil, fmt.Errorf("invalid AGENT_LATITUDE %q: %w", lat, err)
		}
		if _, err := fmt.Sscan(lon, &loc.Longitude); err != nil {
			return nil, fmt.Errorf("invalid AGENT_LONGITUDE %q: %w", lon, err)
		}
		loc.City = firstNonEmpty(os.Getenv("AGENT_CITY"), "configured location")
		return &loc, nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(ipLocationAPIURL)
	if err != nil {
		return nil, fmt.Errorf("fetch IP location: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("location service returned status %d: %s", resp.StatusCode, body)
	}

	var loc IPLocation
	if err := json.NewDecoder(resp.Body).Decode(&loc); err != nil {
		return nil, fmt.Errorf("decode location response: %w", err)
	}
	return &loc, nil
}

func FetchWeatherData(url string) (*WeatherResponse, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch weather data: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("weather service returned status %d: %s", resp.StatusCode, body)
	}

	var weather WeatherResponse
	if err := json.NewDecoder(resp.Body).Decode(&weather); err != nil {
		return nil, fmt.Errorf("decode weather response: %w", err)
	}
	return &weather, nil
}

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

// The Google Health API rejects any token that also carries scopes of other
// APIs (403 DISALLOWED_OAUTH_SCOPES), so Health and Calendar each get their
// own consent and their own token file.
var (
	googleHealthScopes = []string{
		"https://www.googleapis.com/auth/googlehealth.sleep.readonly",
		"https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly",
		"https://www.googleapis.com/auth/googlehealth.activity_and_fitness.readonly",
	}
	googleCalendarScopes = []string{"https://www.googleapis.com/auth/calendar"}
)

func googleOAuthConfig(scopes ...string) (*oauth2.Config, error) {
	clientID := firstNonEmpty(os.Getenv("CLIENT_ID"), defaultClientID)
	clientSecret := firstNonEmpty(os.Getenv("CLIENT_SECRET"), defaultClientSecret)
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("CLIENT_ID and CLIENT_SECRET must be configured for Google sync")
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  callbackURL,
		Scopes:       scopes,
		Endpoint:     oauth2.Endpoint{AuthURL: googleAuthURL, TokenURL: googleTokenURL},
	}, nil
}

// googleClient loads the token stored in tokenPath, or completes the local
// callback OAuth flow and saves it there. oauth2.Config.Client automatically
// refreshes an expired token when its refresh token is available.
//
// An expired token is refreshed up front, so a token Google will never accept
// again (revoked access, deleted OAuth client...) is discarded and replaced
// by a fresh authorization instead of failing every API call.
func googleClient(ctx context.Context, config *oauth2.Config, tokenPath string) (*http.Client, error) {
	token, authorizedAt, err := loadToken(tokenPath)
	if err == nil && !token.Valid() {
		refreshed, refreshErr := config.TokenSource(ctx, token).Token()
		switch {
		case refreshErr == nil:
			token = refreshed
			if err := saveToken(tokenPath, token, authorizedAt); err != nil {
				return nil, err
			}
		case isPermanentTokenError(refreshErr):
			fmt.Printf("Stored Google token in %s is no longer valid (%v); authorizing again.\n", tokenPath, refreshErr)
			if err := os.Remove(tokenPath); err != nil {
				return nil, fmt.Errorf("remove invalid token %s: %w", tokenPath, err)
			}
			token, err = nil, os.ErrNotExist
		default:
			// Likely a network problem - keep the token for the next run.
			return nil, fmt.Errorf("refresh Google token: %w", refreshErr)
		}
	}
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if !interactiveAuth {
			return nil, fmt.Errorf("%w: no valid Google token in %s; run the agent once without -json to authorize in the browser",
				errNeedsAuthorization, tokenPath)
		}
		token, err = authorizeGoogle(ctx, config)
		if err != nil {
			return nil, err
		}
		if err := saveToken(tokenPath, token, time.Now()); err != nil {
			return nil, err
		}
	}
	return config.Client(ctx, token), nil
}

// errNeedsAuthorization means the user must approve access in the browser
// again - the token is missing, expired or revoked.
var errNeedsAuthorization = errors.New("Google authorization needed")

// isPermanentTokenError reports whether Google refused a token refresh in a
// way that retrying with the same token can never fix.
func isPermanentTokenError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		return false
	}
	switch retrieveErr.ErrorCode {
	case "invalid_grant", "invalid_client", "deleted_client", "unauthorized_client":
		return true
	}
	return false
}

func authorizeGoogle(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	state, err := oauthState()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback server: %w", err)
	}
	defer listener.Close()

	codeCh := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != state || r.URL.Query().Get("error") != "" {
			http.Error(w, "Authorization was denied or did not match this request.", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Authorization code missing.", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Authorization complete. You can close this browser window and return to the Fitness Agent.")
		select {
		case codeCh <- code:
		default:
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())

	// Request a refresh token and force Google's consent screen on every new
	// authorization flow, so newly added Health/Calendar scopes are granted.
	authURL := config.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
	fmt.Printf("Open this URL to authorize Google (%s):\n%s\n", strings.Join(config.Scopes, ", "), authURL)
	if err := openBrowser(authURL); err != nil {
		fmt.Printf("Could not open a browser automatically: %v\n", err)
	}

	select {
	case code := <-codeCh:
		token, err := config.Exchange(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("exchange OAuth authorization code: %w", err)
		}
		return token, nil
	case <-time.After(5 * time.Minute):
		return nil, errors.New("timed out waiting for Google authorization callback")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

func getJSON(client *http.Client, endpoint string, target any) error {
	resp, err := client.Get(endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return err
	}
	return nil
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

// storedToken is the token file: the OAuth token plus when the user approved
// it in the browser. While the OAuth app is in "Testing", Google revokes the
// refresh token 7 days after that, so the Telegram bot reads authorized_at to
// remind the user to re-authorize in time. Refreshing keeps authorized_at.
type storedToken struct {
	*oauth2.Token
	AuthorizedAt *time.Time `json:"authorized_at,omitempty"`
}

// loadToken returns the token in filename and when it was authorized (zero
// if the file predates that field).
func loadToken(filename string) (*oauth2.Token, time.Time, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer file.Close()
	stored := storedToken{Token: &oauth2.Token{}}
	if err := json.NewDecoder(file).Decode(&stored); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode %s: %w", filename, err)
	}
	var authorizedAt time.Time
	if stored.AuthorizedAt != nil {
		authorizedAt = *stored.AuthorizedAt
	}
	return stored.Token, authorizedAt, nil
}

func saveToken(filename string, token *oauth2.Token, authorizedAt time.Time) error {
	stored := storedToken{Token: token}
	if !authorizedAt.IsZero() {
		stored.AuthorizedAt = &authorizedAt
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		return fmt.Errorf("save %s: %w", filename, err)
	}
	return nil
}

func oauthState() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// Evaluation logic: turns raw weather/recovery readings into Factors,
// and Factors into an overall workout verdict.
// ---------------------------------------------------------------------

// evaluateWeather scores each weather reading independently.
func evaluateWeather(w *WeatherResponse) []Factor {
	c := w.Current
	factors := []Factor{
		evaluateTemperature(c.Temperature2m),
		evaluateHumidity(c.RelativeHumidity2m),
		evaluateRain(c.Rain),
		evaluateWind(c.WindSpeed10m),
	}
	return factors
}

func evaluateTemperature(tempC float64) Factor {
	f := Factor{Name: "Temperature", Value: tempC, Unit: "°C"}
	switch {
	case tempC >= 35 || tempC <= 0:
		f.Severity = Hazardous
		f.Note = "Extreme temperature; heat stroke or cold-injury risk."
	case tempC >= 30 || tempC <= 5:
		f.Severity = Caution
		f.Note = "Uncomfortable temperature; pace yourself and hydrate."
	default:
		f.Severity = Good
		f.Note = "Comfortable range for outdoor training."
	}
	return f
}

func evaluateHumidity(humidityPct float64) Factor {
	f := Factor{Name: "Humidity", Value: humidityPct, Unit: "%"}
	switch {
	case humidityPct >= 90:
		f.Severity = Hazardous
		f.Note = "Very high humidity severely limits sweat evaporation."
	case humidityPct >= 75:
		f.Severity = Caution
		f.Note = "High humidity; cooling is less effective than usual."
	default:
		f.Severity = Good
		f.Note = "Humidity within a manageable range."
	}
	return f
}

func evaluateRain(rainMM float64) Factor {
	f := Factor{Name: "Rain", Value: rainMM, Unit: "mm"}
	switch {
	case rainMM >= 10:
		f.Severity = Hazardous
		f.Note = "Heavy rain; poor visibility and slick surfaces."
	case rainMM > 0:
		f.Severity = Caution
		f.Note = "Light rain; wear appropriate gear."
	default:
		f.Severity = Good
		f.Note = "No rain expected."
	}
	return f
}

func evaluateWind(windKMH float64) Factor {
	f := Factor{Name: "Wind", Value: windKMH, Unit: "km/h"}
	switch {
	case windKMH >= 40:
		f.Severity = Hazardous
		f.Note = "Strong wind; risk of being blown off balance, falling debris."
	case windKMH >= 25:
		f.Severity = Caution
		f.Note = "Noticeable wind; expect a slower pace outdoors."
	default:
		f.Severity = Good
		f.Note = "Calm conditions."
	}
	return f
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
		evaluateExhaustion(r.FeelingExhausted),
	}
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

func evaluateExhaustion(feelingExhausted bool) Factor {
	// This is a direct self-report (the -exhausted flag or feeling_exhausted
	// in recovery.json), not a synced reading. It never counts as "measured"
	// recovery data on its own - see hasMeasuredRecoverySignal.
	f := Factor{Name: "Subjective Fatigue", Unit: "bool", SelfReported: true}
	if feelingExhausted {
		f.Value = 1
		f.Severity = Caution
		f.Note = "Self-reported exhaustion; listen to your body."
	} else {
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

// ---------------------------------------------------------------------
// Calendar rescheduling: when a workout is skipped, try to move it to the
// next day, cascading later workouts forward as needed - without spilling
// into the next week. If a full cascade won't fit, fall back to swapping
// the missed workout with the shortest other workout later in the week.
//
// This talks to the Google Calendar REST API directly (same style as the
// Fitness calls above) rather than the official client library, so it needs
// no new dependency - just the "calendar" scope already requested during
// OAuth.
// ---------------------------------------------------------------------

// CalendarEvent is a minimal representation of a Google Calendar event -
// enough to identify, read, and reschedule a workout.
type CalendarEvent struct {
	ID      string
	Summary string
	Start   time.Time
	End     time.Time
}

type calendarEventTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type calendarEventJSON struct {
	ID      string            `json:"id"`
	Summary string            `json:"summary"`
	Start   calendarEventTime `json:"start"`
	End     calendarEventTime `json:"end"`
}

// workoutCalendarID returns the calendar that holds workouts. Set
// WORKOUT_CALENDAR_ID to a dedicated workouts calendar to make every event in
// it count as a workout; otherwise the primary calendar is used and only
// tagged events count (see workoutTagPattern).
func workoutCalendarID() string {
	return firstNonEmpty(os.Getenv("WORKOUT_CALENDAR_ID"), "primary")
}

func usingDedicatedWorkoutCalendar() bool {
	return os.Getenv("WORKOUT_CALENDAR_ID") != ""
}

// workoutTagPattern identifies workouts by an explicit tag at the very start
// of the event title, mirroring the weekly split:
//
//	[A]   - legs, back, biceps, abs          (Sun, and again Wed)
//	[B]   - chest, shoulders, triceps, abs   (Mon, and again Thu)
//	[RUN] - running                          (Tue, usually Sat)
//
// e.g. "[A] רגליים גב יד קדמית ובטן". Free-text keyword matching anywhere in
// the title was removed: substrings like "run" (Brunch), " a " (Plan a trip)
// or "גב" (גבינה) turned ordinary events into "workouts" that then got moved
// around the calendar.
var workoutTagPattern = regexp.MustCompile(`(?i)^\s*\[(A|B|RUN)\]`)

// hebrewWorkoutPattern also accepts untagged titles, but only when the title
// STARTS with the whole word "אימון" or "ריצה" ("אימון גב", "ריצה"). The
// words after "אימון" pick the group (see hebrewGroupWords); a bare "אימון"
// is a generic gym session.
var hebrewWorkoutPattern = regexp.MustCompile(`^\s*(אימון|ריצה)(?:[\s,.:\-]+(.*))?$`)

// hebrewGroupWords maps whole words in an "אימון ..." title to the split.
// A leading "ו" ("and") is ignored, so "וגב" counts as "גב".
var hebrewGroupWords = map[string]string{
	"רגליים": "A", "רגל": "A", "גב": "A", "קדמית": "A", "ביספס": "A",
	"חזה": "B", "כתפיים": "B", "כתף": "B", "אחורית": "B", "טרייספס": "B",
}

// supersededPrefix is prepended to a workout's title when the reschedule
// fallback replaces it with the missed session; such events stop counting
// as workouts.
const supersededPrefix = "[SUPERSEDED] "

// categorizeWorkout returns "A", "B" or "run" for a tagged title or a
// Hebrew "אימון ..."/"ריצה" title, "gym" for a bare "אימון", or "" when the
// title isn't a workout.
func categorizeWorkout(summary string) string {
	if m := workoutTagPattern.FindStringSubmatch(summary); m != nil {
		if strings.EqualFold(m[1], "RUN") {
			return "run"
		}
		return strings.ToUpper(m[1])
	}
	m := hebrewWorkoutPattern.FindStringSubmatch(summary)
	if m == nil {
		return ""
	}
	if m[1] == "ריצה" {
		return "run"
	}
	words := strings.FieldsFunc(m[2], func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '-' || r == '+' || r == '/'
	})
	for _, word := range words {
		if group, ok := hebrewGroupWords[word]; ok {
			return group
		}
		if group, ok := hebrewGroupWords[strings.TrimPrefix(word, "ו")]; ok {
			return group
		}
	}
	return "gym"
}

func isWorkoutEvent(summary string) bool {
	if strings.HasPrefix(strings.TrimSpace(summary), strings.TrimSpace(supersededPrefix)) {
		return false
	}
	if usingDedicatedWorkoutCalendar() {
		return true
	}
	return categorizeWorkout(summary) != ""
}

// isOutdoorWorkout reports whether a workout category is affected by the
// weather. Gym sessions (A/B) happen indoors.
func isOutdoorWorkout(category string) bool {
	return category == "run"
}

func parseCalendarTime(t calendarEventTime) (time.Time, error) {
	if t.DateTime == "" {
		return time.Time{}, fmt.Errorf("event has no dateTime (likely an all-day event, which this feature ignores)")
	}
	return time.Parse(time.RFC3339, t.DateTime)
}

// listCalendarEvents fetches events (already expanded from any recurring
// series, via singleEvents) in [timeMin, timeMax] from the user's primary
// calendar, ordered by start time.
func listCalendarEvents(client *http.Client, timeMin, timeMax time.Time) ([]CalendarEvent, error) {
	params := url.Values{}
	params.Set("timeMin", timeMin.Format(time.RFC3339))
	params.Set("timeMax", timeMax.Format(time.RFC3339))
	params.Set("singleEvents", "true")
	params.Set("orderBy", "startTime")
	endpoint := "https://www.googleapis.com/calendar/v3/calendars/" + url.PathEscape(workoutCalendarID()) + "/events?" + params.Encode()

	var response struct {
		Items []calendarEventJSON `json:"items"`
	}
	if err := getJSON(client, endpoint, &response); err != nil {
		return nil, fmt.Errorf("list calendar events: %w", err)
	}

	events := make([]CalendarEvent, 0, len(response.Items))
	for _, item := range response.Items {
		start, err := parseCalendarTime(item.Start)
		if err != nil {
			continue // skip all-day / malformed events
		}
		end, err := parseCalendarTime(item.End)
		if err != nil {
			continue
		}
		events = append(events, CalendarEvent{ID: item.ID, Summary: item.Summary, Start: start, End: end})
	}
	return events, nil
}

// patchCalendarEvent applies a partial update to an event in the workout
// calendar; fields not in payload are left untouched.
func patchCalendarEvent(client *http.Client, eventID string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event update: %w", err)
	}
	endpoint := "https://www.googleapis.com/calendar/v3/calendars/" + url.PathEscape(workoutCalendarID()) +
		"/events/" + url.PathEscape(eventID)
	req, err := http.NewRequest(http.MethodPatch, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update calendar event returned status %d: %s", resp.StatusCode, responseBody)
	}
	return nil
}

// updateCalendarEventTime moves an existing event to a new start/end time,
// keeping all other fields untouched.
func updateCalendarEventTime(client *http.Client, eventID string, newStart, newEnd time.Time, loc *time.Location) error {
	return patchCalendarEvent(client, eventID, map[string]any{
		"start": map[string]string{"dateTime": newStart.In(loc).Format(time.RFC3339), "timeZone": loc.String()},
		"end":   map[string]string{"dateTime": newEnd.In(loc).Format(time.RFC3339), "timeZone": loc.String()},
	})
}

// weekBounds returns the Sunday 00:00:00 -> Saturday 23:59:59 window (in loc)
// that contains day. Matches the Israeli calendar week, consistent with
// calendarTimeZone.
func weekBounds(loc *time.Location, day time.Time) (time.Time, time.Time) {
	d := day.In(loc)
	dayStart := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	offset := int(dayStart.Weekday()) // Sunday = 0 ... Saturday = 6
	weekStart := dayStart.AddDate(0, 0, -offset)
	weekEndDay := weekStart.AddDate(0, 0, 6)
	weekEnd := time.Date(weekEndDay.Year(), weekEndDay.Month(), weekEndDay.Day(), 23, 59, 59, 0, loc)
	return weekStart, weekEnd
}

func sameDay(a, b time.Time, loc *time.Location) bool {
	a, b = a.In(loc), b.In(loc)
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

// calendarChange is one edit a reschedule makes to the calendar: a move to
// NewStart/NewEnd, or (for a superseded workout) a new title.
type calendarChange struct {
	Event      CalendarEvent
	NewStart   time.Time // zero when the time doesn't change
	NewEnd     time.Time
	NewSummary string // non-empty when the title changes
}

// reschedulePlan is everything a reschedule will change, computed before
// touching the calendar so it can be previewed (-dry-run) or applied.
type reschedulePlan struct {
	Missed  CalendarEvent
	Changes []calendarChange
	// Superseded is true for the fallback: the missed workout takes over a
	// shorter same-group workout's slot, and that one is renamed.
	Superseded bool
}

// errNothingToReschedule means no workout on the day is affected by the
// verdict (e.g. only indoor sessions on a bad-weather day).
var errNothingToReschedule = errors.New("no workout on that day is affected")

// errWeekFull means the workout can't be moved: the cascade would spill past
// the end of the week and no shorter same-group workout can be replaced.
// planReschedule still returns a plan with Missed set, so -move-today can
// fall back to a short workout instead (see shortworkouts.go).
var errWeekFull = errors.New("no room to move the workout this week")

// RescheduleMissedWorkout moves the workout scheduled on missedDate forward
// one day at a time, cascading later workouts as needed - but never past the
// end of the current week. See planReschedule for the rules.
func RescheduleMissedWorkout(client *http.Client, missedDate time.Time, eligible func(category string) bool) error {
	plan, err := planReschedule(client, missedDate, eligible)
	if errors.Is(err, errNothingToReschedule) {
		fmt.Printf("No workout on %s is affected by today's verdict (e.g. only indoor sessions on a bad-weather day) - nothing to reschedule.\n",
			missedDate.Format("2006-01-02"))
		return nil
	}
	if err != nil {
		return err
	}
	return applyReschedulePlan(client, plan)
}

// planReschedule works out how to move the workout scheduled on missedDate,
// without changing anything.
//
// eligible optionally restricts which of the day's workouts may be treated as
// the missed one, by category; nil means any workout. A weather-based skip
// passes isOutdoorWorkout so indoor gym sessions are left alone.
//
// The workout moves forward one day at a time, cascading later workouts as
// needed, but never past the end of the current week. If a full cascade would
// spill past the week boundary, it falls back to letting the missed workout
// take over the slot of a shorter workout of the same muscle group on a LATER
// day this week, marking that shorter one as superseded. Nothing is ever
// moved INTO the skipped day.
func planReschedule(client *http.Client, missedDate time.Time, eligible func(category string) bool) (*reschedulePlan, error) {
	loc, err := time.LoadLocation(calendarTimeZone)
	if err != nil {
		return nil, fmt.Errorf("load calendar timezone: %w", err)
	}
	weekStart, weekEnd := weekBounds(loc, missedDate)

	events, err := listCalendarEvents(client, weekStart, weekEnd)
	if err != nil {
		return nil, err
	}

	var workouts []CalendarEvent
	for _, e := range events {
		if isWorkoutEvent(e.Summary) {
			workouts = append(workouts, e)
		}
	}
	sort.Slice(workouts, func(i, j int) bool { return workouts[i].Start.Before(workouts[j].Start) })

	missedDay := missedDate.In(loc)
	missedIdx := -1
	for i, w := range workouts {
		if !sameDay(w.Start, missedDay, loc) {
			continue
		}
		if eligible != nil && !eligible(categorizeWorkout(w.Summary)) {
			continue
		}
		missedIdx = i
		break
	}
	if missedIdx == -1 {
		if eligible != nil {
			return nil, errNothingToReschedule
		}
		return nil, fmt.Errorf("no workout found on %s to reschedule", missedDay.Format("2006-01-02"))
	}
	missed := workouts[missedIdx]
	plan := &reschedulePlan{Missed: missed}

	// Map each remaining day in the week to whichever workout (if any)
	// already occupies it, so we can detect collisions while cascading.
	byDay := map[string]CalendarEvent{}
	for i, w := range workouts {
		if i == missedIdx {
			continue
		}
		byDay[w.Start.In(loc).Format("2006-01-02")] = w
	}

	type move struct {
		event    CalendarEvent
		newStart time.Time
	}
	var moves []move
	current := missed
	target := missedDay.AddDate(0, 0, 1)
	cascadeSucceeded := false
	for !target.After(weekEnd) {
		key := target.Format("2006-01-02")
		if occupant, ok := byDay[key]; ok {
			// target day is taken - bump whoever's there and try to place
			// the current event on the day after that.
			moves = append(moves, move{event: current, newStart: target})
			current = occupant
			target = target.AddDate(0, 0, 1)
			continue
		}
		moves = append(moves, move{event: current, newStart: target})
		cascadeSucceeded = true
		break
	}

	if cascadeSucceeded {
		for _, m := range moves {
			duration := m.event.End.Sub(m.event.Start)
			origStart := m.event.Start.In(loc)
			newStart := time.Date(m.newStart.Year(), m.newStart.Month(), m.newStart.Day(),
				origStart.Hour(), origStart.Minute(), 0, 0, loc)
			plan.Changes = append(plan.Changes, calendarChange{Event: m.event, NewStart: newStart, NewEnd: newStart.Add(duration)})
		}
		return plan, nil
	}

	// Cascade would spill past the end of the week. Fall back to letting the
	// missed workout take over the slot of a shorter workout that targets the
	// same muscle group on a LATER day this week: the week still gets the
	// full session for that group, and the shorter duplicate is marked as
	// superseded. Nothing is pulled into the skipped day - doing that would
	// defeat the reason for skipping.
	missedCategory := categorizeWorkout(missed.Summary)
	if missedCategory == "" {
		return plan, fmt.Errorf(
			"%w: cascade would spill past the end of the week, and %q has no workout group ([A]/[B]/[RUN] tag or an \"אימון ...\"/\"ריצה\" title) - can't find a same-muscle-group workout to replace",
			errWeekFull, missed.Summary)
	}
	missedDur := missed.End.Sub(missed.Start)

	var replaced *CalendarEvent
	var replacedDur time.Duration
	for i := range workouts {
		if i == missedIdx {
			continue
		}
		w := workouts[i]
		if !w.Start.After(missedDay) || sameDay(w.Start, missedDay, loc) {
			continue // only workouts on a later day - never anything on the skipped day
		}
		if categorizeWorkout(w.Summary) != missedCategory {
			continue // must target the same muscle group as the missed workout
		}
		dur := w.End.Sub(w.Start)
		if dur >= missedDur {
			continue // must be shorter than the full session replacing it
		}
		if replaced == nil || dur < replacedDur {
			replaced = &workouts[i]
			replacedDur = dur
		}
	}
	if replaced == nil {
		return plan, fmt.Errorf(
			"%w: cascade would spill past the end of the week, and no shorter %q workout was found on a later day to replace with %q",
			errWeekFull, missedCategory, missed.Summary)
	}

	// The missed workout takes over the replaced workout's slot (its day and
	// start time) and keeps its own full duration.
	missedNewStart := replaced.Start.In(loc)
	if sameDay(missedNewStart, missedDay, loc) {
		// Defensive: the filter above should make this impossible.
		return nil, errors.New("refusing to schedule a workout on the skipped day")
	}
	plan.Superseded = true
	plan.Changes = []calendarChange{
		{Event: missed, NewStart: missedNewStart, NewEnd: missedNewStart.Add(missedDur)},
		{Event: *replaced, NewSummary: supersededPrefix + replaced.Summary},
	}
	return plan, nil
}

// applyReschedulePlan makes the calendar changes of a plan.
func applyReschedulePlan(client *http.Client, plan *reschedulePlan) error {
	loc, err := time.LoadLocation(calendarTimeZone)
	if err != nil {
		return fmt.Errorf("load calendar timezone: %w", err)
	}
	if !plan.Superseded {
		for _, c := range plan.Changes {
			if err := updateCalendarEventTime(client, c.Event.ID, c.NewStart, c.NewEnd, loc); err != nil {
				return fmt.Errorf("cascade-move %q to %s: %w", c.Event.Summary, c.NewStart.Format("2006-01-02"), err)
			}
			fmt.Printf("Moved %q to %s\n", c.Event.Summary, c.NewStart.Format("Mon Jan 2"))
		}
		return nil
	}

	moved, renamed := plan.Changes[0], plan.Changes[1]
	if err := updateCalendarEventTime(client, moved.Event.ID, moved.NewStart, moved.NewEnd, loc); err != nil {
		return fmt.Errorf("move missed workout %q: %w", moved.Event.Summary, err)
	}
	if err := patchCalendarEvent(client, renamed.Event.ID, map[string]any{"summary": renamed.NewSummary}); err != nil {
		// Undo the move so the calendar isn't left with two sessions on one day.
		if undoErr := updateCalendarEventTime(client, moved.Event.ID, moved.Event.Start, moved.Event.End, loc); undoErr != nil {
			return fmt.Errorf("mark %q as superseded: %w; undoing the move of %q ALSO failed (%v) - fix the calendar manually",
				renamed.Event.Summary, err, moved.Event.Summary, undoErr)
		}
		return fmt.Errorf("mark %q as superseded: %w (the move of %q was undone)", renamed.Event.Summary, err, moved.Event.Summary)
	}
	fmt.Printf("Could not cascade within the week - moved %q to %s (the slot of the shorter %q, now marked %s).\n",
		moved.Event.Summary, moved.NewStart.Format("Mon Jan 2 15:04"), renamed.Event.Summary, strings.TrimSpace(supersededPrefix))
	return nil
}

func main() {
	reschedule := flag.Bool("reschedule", false,
		"if today's verdict is to skip the workout, try to reschedule it on Google Calendar")
	exhausted := flag.Bool("exhausted", false,
		"self-report: you feel exhausted today (counts as a CAUTION recovery signal)")
	jsonOutput := flag.Bool("json", false,
		"print the data and verdict as JSON on stdout (for the Telegram bot); progress messages go to stderr")
	moveToday := flag.Bool("move-today", false,
		"move today's workout on Google Calendar regardless of the verdict (e.g. you're busy), then exit")
	dryRun := flag.Bool("dry-run", false,
		"with -move-today: only show what would change, without touching the calendar")
	authFor := flag.String("auth", "",
		"re-authorize Google without a local browser: health or calendar. Prints a link; then pass the address you land on with -auth-code")
	authCode := flag.String("auth-code", "",
		"with -auth: the full address Google redirected to after approving (http://localhost:8080/callback?...)")
	flag.Parse()

	// In JSON mode stdout must carry nothing but the JSON document, so every
	// progress message printed along the way is sent to stderr instead. There
	// is also nobody to complete a browser authorization.
	jsonOut := os.Stdout
	if *jsonOutput {
		os.Stdout = os.Stderr
		interactiveAuth = false
	}

	if *authFor != "" {
		result := runAuthFlow(*authFor, *authCode, time.Now())
		if *jsonOutput {
			if err := writeAuthJSON(jsonOut, result); err != nil {
				fmt.Printf("Could not write JSON output: %v\n", err)
				os.Exit(1)
			}
		} else {
			printAuthResult(result)
		}
		if result.Err != nil && !*jsonOutput {
			os.Exit(1)
		}
		return
	}

	if *moveToday {
		result := moveTodaysWorkout(time.Now(), *dryRun)
		if *jsonOutput {
			if err := writeMoveJSON(jsonOut, result); err != nil {
				fmt.Printf("Could not write JSON output: %v\n", err)
				os.Exit(1)
			}
		} else {
			printMoveResult(result)
		}
		if result.Err != nil && !*jsonOutput {
			os.Exit(1)
		}
		return
	}

	loc, err := GetCurrentLocation()
	if err != nil {
		fmt.Printf("Could not determine location automatically: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Location: %s, %s (%.4f, %.4f)\n", loc.City, loc.Country, loc.Latitude, loc.Longitude)

	weatherURL := fmt.Sprintf(weatherAPIBase, loc.Latitude, loc.Longitude)
	weather, err := FetchWeatherData(weatherURL)
	if err != nil {
		fmt.Printf("Could not fetch weather data: %v\n", err)
		os.Exit(1)
	}

	recovery, err := FetchFitbitRecovery()
	if err != nil {
		// FetchFitbitRecovery already falls back internally, so this should
		// only trigger on an unexpected local I/O failure.
		fmt.Printf("Could not obtain recovery data: %v\n", err)
		os.Exit(1)
	}
	// The flag adds to (never clears) feeling_exhausted from recovery.json.
	recovery.FeelingExhausted = recovery.FeelingExhausted || *exhausted

	report := BuildAgentReport(weather, recovery)
	if *jsonOutput {
		if err := writeSnapshotJSON(jsonOut, loc, weather, recovery, report); err != nil {
			fmt.Printf("Could not write JSON output: %v\n", err)
			os.Exit(1)
		}
	} else {
		printReport(report, recovery.Warnings)
	}

	if !*reschedule {
		return
	}
	var eligible func(category string) bool
	switch report.Kind {
	case VerdictSkipRecovery:
		eligible = nil // the body needs rest: any workout today is affected
	case VerdictSkipWeather:
		eligible = isOutdoorWorkout // unsafe weather: indoor gym sessions stay put
	default:
		return // not a skip verdict - nothing to reschedule
	}

	config, cfgErr := googleOAuthConfig(googleCalendarScopes...)
	if cfgErr != nil {
		fmt.Printf("Could not reschedule workout: %v\n", cfgErr)
		return
	}
	calClient, clientErr := googleClient(context.Background(), config, calendarTokenFile)
	if clientErr != nil {
		fmt.Printf("Could not reschedule workout: %v\n", clientErr)
		return
	}
	if err := RescheduleMissedWorkout(calClient, time.Now(), eligible); err != nil {
		fmt.Printf("Could not reschedule workout: %v\n", err)
	}
}
