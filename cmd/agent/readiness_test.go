package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// metric builds a dailyMetric with a full baseline.
func metric(value, mean, sd float64) dailyMetric {
	return dailyMetric{Available: true, Value: value, BaselineMean: mean, BaselineSD: sd, BaselineDays: 14}
}

func TestEstimateReadiness(t *testing.T) {
	tests := []struct {
		name     string
		in       readinessInputs
		wantBand Severity
	}{
		{
			name:     "at baseline with a full night",
			in:       readinessInputs{HRV: metric(60, 60, 8), RHR: metric(50, 50, 2), LastNightSleepMinutes: 480, WeekAvgSleepMinutes: 470},
			wantBand: Good,
		},
		{
			name:     "HRV and resting HR one SD worse, 6h sleep",
			in:       readinessInputs{HRV: metric(52, 60, 8), RHR: metric(52, 50, 2), LastNightSleepMinutes: 360, WeekAvgSleepMinutes: 360},
			wantBand: Caution,
		},
		{
			name:     "HRV crashed, resting HR way up, 3h sleep",
			in:       readinessInputs{HRV: metric(30, 60, 8), RHR: metric(58, 50, 2), LastNightSleepMinutes: 180, WeekAvgSleepMinutes: 300},
			wantBand: Hazardous,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := estimateReadiness(tt.in)
			if err != nil {
				t.Fatalf("estimateReadiness: %v", err)
			}
			if got.Score < 1 || got.Score > 100 {
				t.Fatalf("score %d out of range", got.Score)
			}
			if band := evaluateReadiness(got.Score, false, got.breakdown()).Severity; band != tt.wantBand {
				t.Errorf("score %d (%s) is %s, want %s", got.Score, got.breakdown(), band, tt.wantBand)
			}
		})
	}
}

func TestEstimateReadinessNeedsData(t *testing.T) {
	full := readinessInputs{HRV: metric(60, 60, 8), RHR: metric(50, 50, 2), LastNightSleepMinutes: 480}
	cases := map[string]func(*readinessInputs){
		"no HRV":            func(in *readinessInputs) { in.HRV = dailyMetric{} },
		"short HRV history": func(in *readinessInputs) { in.HRV.BaselineDays = minReadinessBaselineNights - 1 },
		"no resting HR":     func(in *readinessInputs) { in.RHR = dailyMetric{} },
		"no sleep":          func(in *readinessInputs) { in.LastNightSleepMinutes = 0 },
	}
	for name, mutate := range cases {
		in := full
		mutate(&in)
		if _, err := estimateReadiness(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestHealthFloatDecodesNaNStrings(t *testing.T) {
	var got struct {
		Nightly  healthFloat `json:"nightly"`
		Baseline healthFloat `json:"baseline"`
	}
	if err := json.Unmarshal([]byte(`{"nightly": 33.5, "baseline": "NaN"}`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Nightly != 33.5 || !got.Nightly.valid() {
		t.Errorf("nightly = %v, want a valid 33.5", got.Nightly)
	}
	if got.Baseline.valid() {
		t.Errorf(`"NaN" must decode to an invalid value, got %v`, got.Baseline)
	}
}

func TestLatestAgainstBaseline(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	series := []dailyValue{{day(30), 70}, {day(29), 60}, {day(28), 50}}

	m := latestAgainstBaseline(series, day(30))
	if !m.Available || m.Value != 70 || m.BaselineDays != 2 || m.BaselineMean != 55 || math.Abs(m.BaselineSD-5) > 1e-9 {
		t.Errorf("got %+v, want value 70 against mean 55, SD 5 over 2 days", m)
	}
	if m := latestAgainstBaseline(series, day(31)); m.Available {
		t.Errorf("a value older than notBefore must not be available, got %+v", m)
	}
	if m := latestAgainstBaseline(nil, day(30)); m.Available {
		t.Errorf("an empty series must not be available, got %+v", m)
	}
}
