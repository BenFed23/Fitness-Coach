package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func goReport() *AgentReport { return &AgentReport{Kind: VerdictGo} }

func TestTierForScoreBoundaries(t *testing.T) {
	tests := []struct {
		score int
		want  string
		pct   int
		max   string
	}{
		{29, "rest", -100, "low"},
		{30, "easy", -40, "low"},
		{49, "easy", -40, "low"},
		{50, "reduced", -20, "medium"},
		{64, "reduced", -20, "medium"},
		{65, "normal", 0, "high"},
		{84, "normal", 0, "high"},
		{85, "push", 0, "high"},
	}
	for _, tt := range tests {
		got, reason := computeAdjustment(&RecoveryData{ReadinessScore: tt.score}, goReport(), "A")
		if got == nil {
			t.Fatalf("score %d: no adjustment (%s)", tt.score, reason)
		}
		if got.Tier != tt.want || got.VolumeChangePct != tt.pct || got.MaxIntensity != tt.max {
			t.Errorf("score %d: got %s %d%% %s, want %s %d%% %s",
				tt.score, got.Tier, got.VolumeChangePct, got.MaxIntensity, tt.want, tt.pct, tt.max)
		}
		if got.Reason != "" {
			t.Errorf("score %d: nothing capped it, but reason is %q", tt.score, got.Reason)
		}
	}
}

func TestReducedFloorMatchesShortWorkoutThreshold(t *testing.T) {
	if got := tierForScore(lowReadinessThreshold); got != tierReduced {
		t.Errorf("at lowReadinessThreshold: got %s, want reduced", got)
	}
	if got := tierForScore(lowReadinessThreshold - 1); got != tierEasy {
		t.Errorf("below lowReadinessThreshold: got %s, want easy", got)
	}
}

func TestVerdictCapsTier(t *testing.T) {
	tests := []struct {
		name   string
		score  int
		report AgentReport
		want   string
	}{
		{"light caps a high score", 90, AgentReport{Kind: VerdictLight, RecoverySeverity: Caution}, "reduced"},
		{"light leaves a lower tier alone", 40, AgentReport{Kind: VerdictLight, RecoverySeverity: Caution}, "easy"},
		{"skip_recovery means rest", 90, AgentReport{Kind: VerdictSkipRecovery, RecoverySeverity: Hazardous}, "rest"},
		{"skip_weather doesn't change the tier", 90, AgentReport{Kind: VerdictSkipWeather, WeatherSeverity: Hazardous}, "push"},
		{"adjust_for_weather doesn't change the tier", 70, AgentReport{Kind: VerdictAdjustForWeather, WeatherSeverity: Caution}, "normal"},
		{"skip_weather hiding a recovery caution still caps", 90,
			AgentReport{Kind: VerdictSkipWeather, WeatherSeverity: Hazardous, RecoverySeverity: Caution}, "reduced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := computeAdjustment(&RecoveryData{ReadinessScore: tt.score}, &tt.report, "run")
			if got == nil || got.Tier != tt.want {
				t.Fatalf("got %+v, want tier %s", got, tt.want)
			}
		})
	}
}

// BuildAgentReport and the adjustment must agree, whatever the readings.
func TestAdjustmentNeverContradictsRealVerdict(t *testing.T) {
	sunny := &WeatherResponse{Current: CurrentWeather{Temperature2m: 20, RelativeHumidity2m: 50}}
	storm := &WeatherResponse{Current: CurrentWeather{Temperature2m: 20, RelativeHumidity2m: 50, Rain: 20}}
	for score := 1; score <= 100; score++ {
		r := &RecoveryData{ReadinessScore: score, SleepHours: 8, RestingHR: 50, RHRBaseline: 50, Source: "test"}
		report := BuildAgentReport(sunny, r)
		got, _ := computeAdjustment(r, report, "A")
		switch {
		case report.Kind == VerdictSkipRecovery && got.Tier != "rest":
			t.Errorf("score %d: skip_recovery with tier %s", score, got.Tier)
		case report.Kind == VerdictLight && (got.Tier == "normal" || got.Tier == "push"):
			t.Errorf("score %d: light with tier %s", score, got.Tier)
		}
	}

	// 5 hours of sleep is a recovery caution, which a stormy verdict hides.
	r := &RecoveryData{ReadinessScore: 90, SleepHours: 5, RestingHR: 50, RHRBaseline: 50}
	report := BuildAgentReport(storm, r)
	if report.Kind != VerdictSkipWeather {
		t.Fatalf("verdict = %s, want skip_weather", report.Kind)
	}
	if got, _ := computeAdjustment(r, report, "A"); got.Tier != "reduced" {
		t.Errorf("tier = %s, want reduced", got.Tier)
	}
}

func TestReportedSignalsLowerTier(t *testing.T) {
	hours := func(h float64) *float64 { return &h }
	tests := []struct {
		name       string
		recovery   RecoveryData
		want       string
		wantReason string
	}{
		{"exhausted lowers one step", RecoveryData{ReadinessScore: 90, FeelingExhausted: true}, "normal", "tired"},
		{"exhausted from reduced", RecoveryData{ReadinessScore: 55, FeelingExhausted: true}, "easy", "tired"},
		{"exhausted never goes below rest", RecoveryData{ReadinessScore: 10, FeelingExhausted: true}, "rest", ""},
		{"exhausted and pain is still one step", RecoveryData{ReadinessScore: 55, FeelingExhausted: true, InPain: true}, "easy", "tired; you reported being in pain"},
		{"pain lowers one step", RecoveryData{ReadinessScore: 55, InPain: true}, "easy", "pain"},
		{"pain caps a high score like the bot's light", RecoveryData{ReadinessScore: 90, InPain: true}, "reduced", "pain"},
		{"sick means rest", RecoveryData{ReadinessScore: 90, FeelingSick: true}, "rest", "sick"},
		{"sick stays rest", RecoveryData{ReadinessScore: 10, FeelingSick: true}, "rest", ""},
		{"under 4 hours means rest", RecoveryData{ReadinessScore: 90, ReportedSleepHours: hours(3.5)}, "rest", "under 4 hours"},
		{"under 6 hours caps", RecoveryData{ReadinessScore: 90, ReportedSleepHours: hours(5)}, "reduced", "under 6 hours"},
		{"exactly 6 hours is fine", RecoveryData{ReadinessScore: 90, ReportedSleepHours: hours(6)}, "push", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := computeAdjustment(&tt.recovery, goReport(), "gym")
			if got == nil || got.Tier != tt.want {
				t.Fatalf("got %+v, want tier %s", got, tt.want)
			}
			if !strings.Contains(got.Reason, tt.wantReason) || (tt.wantReason == "" && got.Reason != "") {
				t.Errorf("reason = %q, want it to mention %q", got.Reason, tt.wantReason)
			}
			if strings.Count(got.Reason, "in pain") > 1 {
				t.Errorf("reason repeats itself: %q", got.Reason)
			}
		})
	}
}

// With a real report, -exhausted also makes the verdict "light", so a high
// score ends up at "reduced" rather than one step lower.
func TestExhaustedOnHighScoreUsesLightCap(t *testing.T) {
	r := &RecoveryData{ReadinessScore: 90, SleepHours: 8, RestingHR: 50, RHRBaseline: 50, FeelingExhausted: true}
	report := BuildAgentReport(&WeatherResponse{Current: CurrentWeather{Temperature2m: 20, RelativeHumidity2m: 50}}, r)
	got, _ := computeAdjustment(r, report, "A")
	if report.Kind != VerdictLight || got.Tier != "reduced" {
		t.Errorf("verdict %s tier %s, want light and reduced", report.Kind, got.Tier)
	}
}

func TestWordingFitsWorkoutCategory(t *testing.T) {
	banned := map[string][]string{
		"A":   {"distance", "zone", "pace", "run"},
		"B":   {"distance", "zone", "pace", "run"},
		"gym": {"distance", "zone", "pace", "run"},
		"run": {"sets", "weights", "lift"},
		"":    {"sets", "weights", "lift", "distance", "zone", "pace"},
	}
	wantJSON := map[string]string{"A": "A", "B": "B", "gym": "gym", "run": "RUN", "": "generic"}
	for category, words := range banned {
		for _, score := range []int{90, 70, 55, 40, 10} {
			got, _ := computeAdjustment(&RecoveryData{ReadinessScore: score}, goReport(), category)
			if got.WorkoutCategory != wantJSON[category] {
				t.Errorf("%q: workout_category = %s, want %s", category, got.WorkoutCategory, wantJSON[category])
			}
			text := strings.ToLower(got.Summary + " " + strings.Join(got.Tips, " "))
			for _, word := range words {
				if strings.Contains(text, word) {
					t.Errorf("%q at score %d mentions %q: %s", category, score, word, text)
				}
			}
		}
	}

	strength, _ := computeAdjustment(&RecoveryData{ReadinessScore: 55}, goReport(), "A")
	if text := strings.Join(strength.Tips, " "); !strings.Contains(text, "set") || !strings.Contains(text, "weights") {
		t.Errorf("strength tips should talk about sets and weights: %s", text)
	}
	run, _ := computeAdjustment(&RecoveryData{ReadinessScore: 40}, goReport(), "run")
	if text := strings.Join(run.Tips, " "); !strings.Contains(text, "distance") || !strings.Contains(text, "zone 2") {
		t.Errorf("run tips should talk about distance and zone 2: %s", text)
	}
}

func TestTableIsCompleteAndWithinTipLimit(t *testing.T) {
	for _, tier := range []adjustmentTier{tierRest, tierEasy, tierReduced, tierNormal, tierPush} {
		for _, kind := range []workoutKind{kindGeneric, kindStrength, kindRun} {
			w := adjustmentTable[tier].Wording[kind]
			if w.Summary == "" || len(w.Tips) == 0 || len(w.Tips) > maxTierTips {
				t.Errorf("%s/%d: bad wording %+v", tier, kind, w)
			}
		}
	}
}

func TestExtraAndWeatherTips(t *testing.T) {
	has := func(a *Adjustment, part string) bool { return strings.Contains(strings.Join(a.Tips, "|"), part) }

	got, _ := computeAdjustment(&RecoveryData{ReadinessScore: 90}, goReport(), "A")
	if !has(got, "Optional") {
		t.Errorf("push on a go day should offer the extra: %v", got.Tips)
	}
	report := &AgentReport{Kind: VerdictAdjustForWeather, WeatherSeverity: Caution}
	got, _ = computeAdjustment(&RecoveryData{ReadinessScore: 90}, report, "run")
	if has(got, "Optional") || !has(got, "Weather:") || got.Tier != "push" {
		t.Errorf("no extra but a weather tip when the weather isn't ideal: %+v", got)
	}
	report = &AgentReport{Kind: VerdictSkipWeather, WeatherSeverity: Hazardous}
	got, _ = computeAdjustment(&RecoveryData{ReadinessScore: 55}, report, "run")
	if !has(got, "indoors") || len(got.Tips) > 3 {
		t.Errorf("a run in bad weather should move indoors, within 3 tips: %v", got.Tips)
	}
	got, _ = computeAdjustment(&RecoveryData{ReadinessScore: 55}, report, "A")
	if has(got, "Weather:") {
		t.Errorf("a gym session gets no weather tip: %v", got.Tips)
	}
	got, _ = computeAdjustment(&RecoveryData{ReadinessScore: 55}, report, "")
	if !has(got, "indoors") {
		t.Errorf("an unknown workout may be outside: %v", got.Tips)
	}
	got, _ = computeAdjustment(&RecoveryData{ReadinessScore: 10}, report, "run")
	if has(got, "Weather:") {
		t.Errorf("a rest day gets no weather tip: %v", got.Tips)
	}
}

func TestNoAdjustmentWithoutReadiness(t *testing.T) {
	tests := []struct {
		name     string
		recovery RecoveryData
		want     string
	}{
		{"placeholder", RecoveryData{ReadinessIsPlaceholder: true}, "no readiness score"},
		{"estimate error", RecoveryData{ReadinessIsPlaceholder: true, ReadinessUnavailable: "no HRV from last night"}, "no HRV from last night"},
		{"tracker not worn", RecoveryData{ReadinessScore: 80, TrackerNotWorn: true}, "wasn't worn"},
		{"reports don't invent a tier", RecoveryData{ReadinessIsPlaceholder: true, FeelingSick: true}, "no readiness score"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := computeAdjustment(&tt.recovery, goReport(), "A")
			if got != nil {
				t.Fatalf("want no adjustment, got %+v", got)
			}
			if !strings.Contains(reason, tt.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, tt.want)
			}
		})
	}
}

func snapshotJSON(t *testing.T, r *RecoveryData, adjustment *Adjustment, reason string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	err := writeSnapshotJSON(&buf, &IPLocation{City: "Test"}, &WeatherResponse{}, r, &AgentReport{}, adjustment, reason)
	if err != nil {
		t.Fatalf("writeSnapshotJSON: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestSnapshotJSONAdjustment(t *testing.T) {
	missing := &RecoveryData{ReadinessIsPlaceholder: true}
	adjustment, reason := computeAdjustment(missing, goReport(), "A")
	out := snapshotJSON(t, missing, adjustment, reason)
	value, present := out["adjustment"]
	if !present || value != nil {
		t.Errorf("adjustment = %v (present %v), want an explicit null", value, present)
	}
	if out["adjustment_reason"] != reason || reason == "" {
		t.Errorf("adjustment_reason = %v, want %q", out["adjustment_reason"], reason)
	}

	full := &RecoveryData{ReadinessScore: 55}
	adjustment, reason = computeAdjustment(full, goReport(), "run")
	out = snapshotJSON(t, full, adjustment, reason)
	object, ok := out["adjustment"].(map[string]any)
	if !ok {
		t.Fatalf("adjustment = %v, want an object", out["adjustment"])
	}
	for key, want := range map[string]any{
		"tier": "reduced", "volume_change_pct": float64(-20), "max_intensity": "medium",
		"workout_category": "RUN", "reason": "",
	} {
		if object[key] != want {
			t.Errorf("%s = %v, want %v", key, object[key], want)
		}
	}
	if object["summary"] == "" {
		t.Error("summary is empty")
	}
	if tips, ok := object["tips"].([]any); !ok || len(tips) == 0 || len(tips) > 3 {
		t.Errorf("tips = %v, want 1-3 items", object["tips"])
	}
	if _, present := out["adjustment_reason"]; present {
		t.Error("adjustment_reason should be left out when there is an adjustment")
	}
	for _, key := range []string{"generated_at", "location", "weather", "recovery", "verdict", "factors", "warnings"} {
		if _, present := out[key]; !present {
			t.Errorf("existing field %q is gone", key)
		}
	}
}

func TestTodaysWorkoutCategory(t *testing.T) {
	t.Setenv("WORKOUT_CALENDAR_ID", "")
	loc, _ := time.LoadLocation(calendarTimeZone)
	now := time.Date(2026, 9, 29, 7, 0, 0, 0, loc)
	at := func(h int) time.Time { return time.Date(2026, 9, 29, h, 0, 0, 0, loc) }
	tests := []struct {
		name   string
		events []calendarEventJSON
		want   string
	}{
		{"no events", nil, ""},
		{"not a workout", []calendarEventJSON{calendarEvent("x", "Brunch", at(10), 60)}, ""},
		{"a superseded workout doesn't count", []calendarEventJSON{calendarEvent("x", supersededPrefix+"[A] Legs", at(10), 60)}, ""},
		{"first workout of the day", []calendarEventJSON{
			calendarEvent("x", "Brunch", at(9), 60),
			calendarEvent("r", "[RUN] Easy", at(10), 60),
			calendarEvent("a", "[A] Legs", at(18), 60),
		}, "run"},
		{"bare workout", []calendarEventJSON{calendarEvent("g", "Workout", at(18), 60)}, "gym"},
		{"tomorrow doesn't count", []calendarEventJSON{calendarEvent("a", "[A] Legs", at(18).AddDate(0, 0, 1), 60)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: &fakeCalendar{events: tt.events}}
			got, err := todaysWorkoutCategory(client, now)
			if err != nil {
				t.Fatalf("todaysWorkoutCategory: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
